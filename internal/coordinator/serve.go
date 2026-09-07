package coordinator

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Custok/sofmat/internal/agent"
	"github.com/Custok/sofmat/internal/config"
	"github.com/Custok/sofmat/internal/discovery"
	"github.com/Custok/sofmat/internal/gateway"
)

// Server fronts the gateway with the OpenAI-compatible HTTP API and proxies the
// read endpoints a dashboard needs (/health, /v1/models, /props) to the decode
// backend, so a panel can point at the coordinator instead of the raw engine.
type Server struct {
	cfg    *config.Config
	gw     *gateway.Gateway
	bc     gateway.BackendConfig
	client *http.Client

	peerMu sync.Mutex
	peers  []discovery.Peer // soflink nodes found on the LAN (background sweep)

	// bal spreads chat traffic across the configured decode engines, pinning a
	// conversation to the engine that holds its prompt cache (balancer.go).
	bal *decodeBalancer

	// kp drives the disaggregated prefill→decode handoff; nil when the fleet is
	// not disaggregated. Kept so a handed-off request can be sent to the engine
	// its state was actually restored into.
	kp *kvPipe
}

// NewServer wires the config's instances into a gateway.
func NewServer(cfg *config.Config) (*Server, error) {
	bc := backendConfigFrom(cfg)
	s := &Server{cfg: cfg, bc: bc, client: &http.Client{Timeout: 600 * time.Second}}
	s.bal = newDecodeBalancer(decodeEndpointsFrom(cfg))
	s.bal.setOccupancy(s.engineHeldTokens)
	if s.bal.Len() > 1 {
		names := []string{}
		for _, st := range s.bal.stats() {
			names = append(names, st["name"].(string)+"="+st["url"].(string))
		}
		log.Printf("gateway: %d motores de decode con reparto por conversación: %s",
			s.bal.Len(), strings.Join(names, ", "))
		go s.bal.probeBudgets(func(u string) (map[string]any, error) { return s.getJSONOr(u) })
	}
	opts := gateway.Options{
		Verify:         func(gateway.Headers) bool { return true }, // TODO: real auth (internal/auth)
		BackendCall:    s.backendCall,
		StatusProvider: func() gateway.Body { return gateway.Body{"status": "ok"} },
		NSlots:         4,
	}
	// Disaggregated prefill→decode (F1): wired only when both roles are configured
	// AND both main nodes expose a soflink agent (the KV state travels between the
	// two agents). Otherwise the gateway stays decode-only (fail-soft).
	if kp := kvPipeFrom(cfg, bc); kp != nil {
		s.kp = kp
		opts.PrefillCall = kp.Prefill
		opts.Handoff = kp.Handoff
		opts.CountTokens = kp.Count
		opts.Tokens = kp.Tokens
		opts.DecodeBusy = kp.DecodeBusy
		// symmetric routing: the prefill runs on the engine where the conversation
		// does NOT live, and the state is restored into the engine that will serve
		// it. So every conversation can take the handoff — no veto needed.
		ctlOf := ctlByEndpoint(cfg)
		kp.resolve = func(b gateway.Body) (kvRoute, bool) {
			if s.bal == nil || s.bal.Len() < 2 {
				return kvRoute{}, false
			}
			dec := s.bal.stickyNode(sessionKey(b))
			if dec == nil {
				return kvRoute{}, false
			}
			pre := s.bal.otherThan(dec)
			if pre == nil || ctlOf[pre.url] == "" || ctlOf[dec.url] == "" {
				return kvRoute{}, false
			}
			return kvRoute{
				prefillURL: pre.url, prefillCtl: ctlOf[pre.url],
				decodeURL: dec.url, decodeCtl: ctlOf[dec.url],
			}, true
		}
		mode := cfg.KVHandoff
		if mode == "" {
			mode = gateway.ModeAuto
		}
		opts.Mode = mode
		log.Printf("gateway: KV handoff prefill→decode ACTIVO modo %s (prefill %s via %s → decode %s via %s; umbral exacto %d tokens)",
			mode, kp.prefillURL, kp.prefillCtl, kp.decodeURL, kp.decodeCtl, gateway.PrefillExactMinTokens)
		// el prefill ingiere varios prompts a la vez hasta su presupuesto real de KV
		go func() {
			ctxOf := func(u string) int {
				d, err := s.getJSONOr(u + "/props")
				if err != nil {
					return 0
				}
				gs, _ := d["default_generation_settings"].(map[string]any)
				if gs == nil {
					return 0
				}
				v, _ := gs["n_ctx"].(float64)
				return int(v)
			}
			seen := map[string]bool{}
			for _, u := range append(engineURLs(cfg), kp.prefillURL, kp.decodeURL) {
				if u == "" || seen[u] {
					continue
				}
				seen[u] = true
				kp.setEngineBudget(u, ctxOf(u))
			}
		}()
	} else {
		log.Printf("gateway: decode-only (sin prefill configurado o sin agent soflink en los nodos main)")
	}
	gw, err := gateway.New(opts)
	if err != nil {
		return nil, err
	}
	s.gw = gw
	return s, nil
}

// ctlByEndpoint maps each engine endpoint to the soflink agent of its main node
// (the agent is what ships the KV state between engines).
func ctlByEndpoint(cfg *config.Config) map[string]string {
	out := map[string]string{}
	for _, in := range cfg.Instances {
		if in.Endpoint == "" {
			continue
		}
		if a := agentOfNode(cfg, in.Main); a != "" {
			out[strings.TrimRight(in.Endpoint, "/")] = a
		}
	}
	return out
}

// engineURLs lists every configured engine endpoint, once.
func engineURLs(cfg *config.Config) []string {
	var out []string
	seen := map[string]bool{}
	for _, in := range cfg.Instances {
		u := strings.TrimRight(in.Endpoint, "/")
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}

// decodeEndpointsFrom lists every configured decode engine, the first one being
// the primary (the engine a KV handoff restores into). An instance is a decode
// when its role says so, or when its key starts with "decode" (decode, decode2…).
func decodeEndpointsFrom(cfg *config.Config) []struct{ Name, URL string } {
	var out []struct{ Name, URL string }
	seen := map[string]bool{}
	for _, in := range cfg.Instances {
		isDecode := in.Role == "decode" || strings.HasPrefix(in.Key, "decode")
		if !isDecode || in.Endpoint == "" || seen[in.Endpoint] {
			continue
		}
		seen[in.Endpoint] = true
		name := in.Key
		if name == "" {
			name = in.Main
		}
		out = append(out, struct{ Name, URL string }{name, in.Endpoint})
	}
	return out
}

// engineHeldTokens is the KV an engine cannot give away for ONE more request. -1 when /slots cannot be read, so the caller keeps
// its last reading instead of assuming the engine is empty.
func (s *Server) engineHeldTokens(url string) int {
	req, err := http.NewRequest(http.MethodGet, url+"/slots", nil)
	if err != nil {
		return -1
	}
	c := &http.Client{Timeout: 1500 * time.Millisecond, Transport: gateway.PooledTransport()}
	resp, err := c.Do(req)
	if err != nil {
		return -1
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return -1
	}
	var slots []map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&slots); err != nil {
		return -1
	}
	return committedKV(slots)
}

// committedKV is the shared occupancy formula for a UNIFIED KV cache: the
// engine holds n_ctx cells in TOTAL, not per slot. A new request can displace
// the cache of the one slot it lands in, but not the others'. So what it cannot
// have is everything held minus the biggest reclaimable (idle) slot.
//
// Both extremes were tried on the live fleet on 2026-09-07 and both broke:
//   - counting every slot (nothing reclaimable): two engines holding ~55k of
//     FINISHED conversations, nothing generating, and every request queued the
//     full wait and was refused. The gateway stopped answering.
//   - counting only the slots that GENERATE (everything idle reclaimable): a
//     50 462-token prefill was admitted into an engine whose idle slots already
//     held the rest, and llama-server answered HTTP 500 "Context size has been
//     exceeded".
func committedKV(slots []map[string]any) int {
	held, reclaimable := 0, 0
	for _, sl := range slots {
		n := 0
		if v, ok := sl["n_prompt_tokens"].(float64); ok {
			n = int(v)
		}
		held += n
		if busy, _ := sl["is_processing"].(bool); !busy && n > reclaimable {
			reclaimable = n
		}
	}
	return held - reclaimable
}

// getJSONOr fetches a JSON document (used to probe each engine's context size).
func (s *Server) getJSONOr(url string) (map[string]any, error) {
	resp, err := s.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// backendCall is the gateway's BackendCall: it chooses the decode engine for
// this conversation (sticky + least loaded + budget guard) and proxies there.
// A request whose KV was just handed off must stay on the primary engine — the
// restored state lives in that engine's slot.
func (s *Server) backendCall(body gateway.Body, extra gateway.Headers) (gateway.Body, error) {
	node, done, err := s.pickDecode(body, extra)
	if err != nil {
		return nil, err
	}
	defer done()
	url := s.bc.DecodeEntryURL
	if node != nil {
		url = node.url
	}
	return httpBackend(url)(body, extra)
}

// pickDecode resolves the engine for a request plus its release function.
func (s *Server) pickDecode(body gateway.Body, extra gateway.Headers) (*decodeNode, func(), error) {
	if s.bal == nil || s.bal.Len() == 0 {
		return nil, func() {}, nil
	}
	if hid := extra["x-sofmat-kv-handoff"]; hid != "" {
		// its state was restored into a specific engine (symmetric routing picks
		// the conversation's own): it must go there, and STAY there.
		n := s.bal.Primary()
		if s.kp != nil {
			if m := s.bal.nodeByURL(s.kp.DecodeURLFor(hid)); m != nil {
				n = m
			}
		}
		est := estBodyTokens(body)
		n.claim(est)
		s.bal.remember(sessionKey(body), n)
		return n, func() { n.release(est); n.invalidateHeld() }, nil
	}
	return s.bal.pick(sessionKey(body), estBodyTokens(body))
}

// estBodyTokens is the cheap chars/4 estimate of what a request will hold in
// the engine's KV (prompt + the reply it is allowed to generate).
func estBodyTokens(body gateway.Body) int {
	n := 0
	if raw, err := json.Marshal(body["messages"]); err == nil {
		n = len(raw) / 4
	}
	reply := 0
	switch v := body["max_tokens"].(type) {
	case float64:
		reply = int(v)
	case int:
		reply = v
	}
	if reply > replyReserve {
		reply = replyReserve
	}
	return n + reply
}

// agentOfNode returns the soflink agent base of a configured node ("" if none).
func agentOfNode(cfg *config.Config, id string) string {
	for _, n := range cfg.Nodes {
		if n.ID == id {
			return strings.TrimRight(n.Agent, "/")
		}
	}
	return ""
}

// kvPipeFrom builds the handoff drivers from the config: the decode and prefill
// engines plus the soflink agents of their main nodes. nil = not disaggregated.
func kvPipeFrom(cfg *config.Config, bc gateway.BackendConfig) *kvPipe {
	if bc.PrefillURL == "" || bc.DecodeEntryURL == "" {
		return nil
	}
	var preCtl, decCtl string
	for _, in := range cfg.Instances {
		switch in.Key {
		case "decode":
			decCtl = agentOfNode(cfg, in.Main)
		case "prefill":
			preCtl = agentOfNode(cfg, in.Main)
		}
	}
	if preCtl == "" || decCtl == "" {
		return nil
	}
	return newKVPipe(bc.PrefillURL, bc.DecodeEntryURL, preCtl, decCtl)
}

// backendConfigFrom resolves decode/prefill endpoints from the config instances.
func backendConfigFrom(cfg *config.Config) gateway.BackendConfig {
	var bc gateway.BackendConfig
	for _, in := range cfg.Instances {
		switch in.Key {
		case "decode":
			bc.DecodeEntryURL = in.Endpoint
		case "prefill":
			bc.PrefillURL = in.Endpoint
		}
	}
	return bc
}

// backendClient is shared by every decode call so balancing across engines
// reuses keep-alive connections instead of opening one per request.
var backendClient = &http.Client{Timeout: 600 * time.Second, Transport: gateway.PooledTransport()}

// httpBackend proxies a request body to endpoint's OpenAI chat endpoint.
func httpBackend(endpoint string) gateway.BackendCall {
	client := backendClient
	return func(body gateway.Body, extra gateway.Headers) (gateway.Body, error) {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequest(http.MethodPost, endpoint+"/v1/chat/completions", bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range extra {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		var out gateway.Body
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, err
		}
		return out, nil
	}
}

// Handler returns the HTTP mux for the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Decode (root) — the answer-generating stage; chat runs through the gateway.
	mux.HandleFunc("/health", s.proxyGET("/health"))
	mux.HandleFunc("/v1/models", s.proxyGET("/v1/models"))
	mux.HandleFunc("/props", s.proxyGET("/props"))
	mux.HandleFunc("/v1/chat/completions", s.chat)
	// Prefill stage, addressable under /prefill so a dashboard reads the WHOLE
	// pipeline through the coordinator (plain proxy — not the gateway path; the
	// disaggregated prefill→handoff→decode flow wires in with the engine binding).
	if s.bc.PrefillURL != "" {
		mux.HandleFunc("/prefill/health", s.proxyReq(s.bc.PrefillURL, "/health"))
		mux.HandleFunc("/prefill/v1/models", s.proxyReq(s.bc.PrefillURL, "/v1/models"))
		mux.HandleFunc("/prefill/props", s.proxyReq(s.bc.PrefillURL, "/props"))
		mux.HandleFunc("/prefill/v1/chat/completions", s.proxyReq(s.bc.PrefillURL, "/v1/chat/completions"))
	}
	// Cluster telemetry + control, so a dashboard reads nodes and drives
	// load/eject through the coordinator (see nodes.go).
	mux.HandleFunc("/nodes", s.nodes)
	mux.HandleFunc("/hop", s.hop)
	mux.HandleFunc("/admin/eject", s.guard(s.adminEject))
	mux.HandleFunc("/admin/load", s.guard(s.adminLoad))
	mux.HandleFunc("/config/apply", s.guard(s.configApply)) // one-call model swap (eject+load)
	// soflink LAN discovery: answer the hello so a peer's subnet sweep recognizes
	// this node as soflink (not a random open port).
	mux.HandleFunc("/soflink/hello", discovery.HelloHandler(s.selfID(), "coordinator"))
	// Shared display labels: peers gossip pencil renames here (one hop, no
	// re-propagate), and expose their current labels for startup catch-up.
	mux.HandleFunc("/soflink/rename", s.peerRename)
	mux.HandleFunc("/soflink/renames", s.renamesList)
	// The daemon IS the node sensor: it serves its own GPU/host telemetry, so any
	// soflink found on the LAN self-reports its hardware (no separate agent).
	mux.HandleFunc("/gpu", agent.Handler(s.selfID()))
	// Embedded live panel + its API, so the binary ships its own dashboard.
	mux.HandleFunc("/panel", s.panelPage)
	mux.HandleFunc("/api/status", s.panelStatus)
	mux.HandleFunc("/api/version", s.panelVersion)
	mux.HandleFunc("/api/autoupdate", s.panelSetAutoUpdate)
	mux.HandleFunc("/api/update", s.panelUpdateNow)
	mux.HandleFunc("/api/update/fleet", s.panelUpdateFleet) // one click updates every node
	mux.HandleFunc("/api/eject", s.guard(s.adminEject))
	mux.HandleFunc("/api/load", s.guard(s.adminLoad))
	mux.HandleFunc("/api/apply", s.guard(s.configApply))
	mux.HandleFunc("/api/apply-union", s.guard(s.configApplyUnion)) // one click raises the whole decode+prefill group (idempotent)
	mux.HandleFunc("/api/genkey", s.panelGenKey)
	mux.HandleFunc("/api/chat", s.panelChat)
	mux.HandleFunc("/api/chat/stream", s.panelChatStream)
	mux.HandleFunc("/api/measure", s.panelMeasure)
	mux.HandleFunc("/api/selectinstance", s.panelSelectInstance)
	mux.HandleFunc("/api/setconfig", s.panelSetConfig)
	mux.HandleFunc("/api/rename", s.panelRename)
	// Model download from HuggingFace (unsloth only): browse → pick quant →
	// download to models dir → appears in Load. Additive; see models.go.
	mux.HandleFunc("/api/hf/models", s.hfModels)
	mux.HandleFunc("/api/hf/files", s.hfFiles)
	mux.HandleFunc("/api/hf/download", s.hfDownload)
	mux.HandleFunc("/api/hf/progress", s.hfProgress)
	mux.HandleFunc("/api/models/local", s.localModels)
	mux.HandleFunc("/api/models/load", s.guard(s.modelsLoad))
	mux.HandleFunc("/api/models/eject", s.guard(s.modelsEject))
	mux.HandleFunc("/api/models/delete", s.guard(s.modelsDelete))
	mux.HandleFunc("/api/models/probe", s.modelsProbe)
	mux.HandleFunc("/api/models/alive", s.controlAlive)
	// Control plane merged into the daemon (no separate node-agent): launch/stop
	// llama-server on this host. LAN-trust like the old node-agent (allowlisted).
	mux.HandleFunc("/control/load", s.controlLoad)
	mux.HandleFunc("/control/eject", s.controlEject)
	mux.HandleFunc("/control/kill", s.controlKill)
	// KV state exchange for the prefill→decode handoff (kvstate.go): a node serves
	// the slot states its engine saved and pulls a peer's into its own dir.
	mux.HandleFunc("/control/kv-fetch", s.controlKVFetch)
	mux.HandleFunc("/kv/", s.kvFile)
	// Live request log (routing decisions + engine timings per request).
	mux.HandleFunc("/api/requests", s.panelRequests)
	mux.HandleFunc("/", s.panelPage) // dashboard home (catch-all last)
	return mux
}

// selfID is this node's anonymous discovery id: the config's self_id if set,
// else a stable hash of the hostname — the raw machine hostname never goes on
// the wire.
func (s *Server) selfID() string {
	if s.cfg.SelfID != "" {
		return s.cfg.SelfID
	}
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "node-sofmat"
	}
	sum := sha256.Sum256([]byte(h))
	return "node-" + hex.EncodeToString(sum[:3])
}

// proxyReq forwards the incoming request (method+body+content-type) to base+path
// and streams the reply back, so a dashboard reaches a stage through the coordinator.
func (s *Server) proxyReq(base, path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		req, err := http.NewRequest(r.Method, base+path, bytes.NewReader(body))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}

// proxyGET forwards a GET to the decode backend and streams the reply, so a
// dashboard reads model/health/props through the coordinator.
func (s *Server) proxyGET(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.bc.DecodeEntryURL == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no decode backend"})
			return
		}
		resp, err := s.client.Get(s.bc.DecodeEntryURL + path)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, gateway.Body{"error": err.Error()})
		return
	}
	var body gateway.Body
	if err := json.Unmarshal(raw, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, gateway.Body{"error": err.Error()})
		return
	}
	log.Printf("chat: from=%s stream=%v tools=%v bytes=%d", r.RemoteAddr,
		streamRequested(body), body["tools"] != nil, len(raw))
	h := gateway.Headers{}
	for k := range r.Header {
		h[k] = r.Header.Get(k)
	}
	// Streaming (SSE) can't go through gateway.Chat — it json.Unmarshals the
	// whole reply, which chokes on the upstream's "data: {...}" event stream.
	// It shares the gateway's two halves instead: Prepare (admission, prefill +
	// KV handoff, slot pin) → stream the decode → Finish from the last chunk.
	if streamRequested(body) {
		plan, err := s.gw.Prepare(h, body)
		if err != nil {
			code := http.StatusBadGateway
			if errors.Is(err, gateway.ErrUnauthorized) {
				code = http.StatusUnauthorized
			}
			writeJSON(w, code, gateway.Body{"error": err.Error()})
			return
		}
		s.chatStream(w, r, plan)
		return
	}
	out, err := s.gw.Chat(h, body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, gateway.Body{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// panelRequests serves the gateway's request log (newest last): per request the
// admission decision, where it ran (decode / prefill / decode-fallback), the
// handoff metrics and the engine timings (prompt_n = 1 proves the handoff).
func (s *Server) panelRequests(w http.ResponseWriter, r *http.Request) {
	n := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && v > 0 && v <= 500 {
		n = v
	}
	rows, err := s.gw.Requests(gateway.Headers{}, n)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, gateway.Body{"error": err.Error()})
		return
	}
	out := gateway.Body{"requests": rows, "disaggregated": s.gw.Disaggregated()}
	if s.bal != nil && s.bal.Len() > 0 {
		out["decode_engines"] = s.bal.stats()
	}
	writeJSON(w, http.StatusOK, out)
}

// streamRequested reports whether the body asked for an SSE stream.
func streamRequested(body gateway.Body) bool {
	b, _ := body["stream"].(bool)
	return b
}

// chatStream proxies a PREPARED streaming chat request to the decode backend
// and copies the SSE response to the client chunk-by-chunk (flushed), so clients
// that want token streaming (e.g. a HUD) get it through the coordinator with the
// same policy as the JSON path. The tail of the stream is kept to Finish the
// request record from the engine's final-chunk timings.
func (s *Server) chatStream(w http.ResponseWriter, r *http.Request, plan *gateway.Plan) {
	if s.bc.DecodeEntryURL == "" {
		writeJSON(w, http.StatusServiceUnavailable, gateway.Body{"error": "no decode backend"})
		return
	}
	// same engine choice as the JSON path: sticky per conversation, least loaded
	// for a new one, and pinned to the primary when a handoff just restored there.
	node, doneNode, perr := s.pickDecode(plan.Body, plan.Headers)
	if perr != nil {
		writeJSON(w, http.StatusServiceUnavailable, gateway.Body{"error": perr.Error()})
		s.gw.Finish(plan, gateway.Body{})
		return
	}
	defer doneNode()
	decodeURL := s.bc.DecodeEntryURL
	if node != nil {
		decodeURL = node.url
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, gateway.Body{"error": "streaming unsupported by server"})
		return
	}
	raw, err := json.Marshal(plan.Body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, gateway.Body{"error": err.Error()})
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		decodeURL+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, gateway.Body{"error": err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range plan.Headers {
		req.Header.Set(k, v)
	}
	if a := r.Header.Get("Authorization"); a != "" {
		req.Header.Set("Authorization", a)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, gateway.Body{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/event-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	const tailKeep = 16 << 10
	var tail []byte
	defer func() {
		fin := gateway.Body{}
		if tm := sseTimings(tail); tm != nil {
			fin["timings"] = tm
		}
		s.gw.Finish(plan, fin)
	}()
	buf := make([]byte, 8192)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			flusher.Flush()
			tail = append(tail, buf[:n]...)
			if len(tail) > tailKeep {
				tail = tail[len(tail)-tailKeep:]
			}
		}
		if rerr != nil {
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
