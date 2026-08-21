package coordinator

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Custok/sofmat/internal/config"
	"github.com/Custok/sofmat/internal/gateway"
)

//go:embed panel.html
var panelHTML []byte

// measureMsgs is the fixed code-generation prompt used for throughput measures,
// so numbers are comparable across runs.
var measureMsgs = []map[string]any{{
	"role":    "user",
	"content": "Escribe en Python quicksort con docstring, type hints y 3 asserts. Solo el codigo completo.",
}}

// panelState holds the small mutable UI state the dashboard needs across polls:
// node renames, the selected instance, the operator-picked active config, and
// the last measured throughput per instance / for the disaggregated cluster.
type panelState struct {
	mu           sync.Mutex
	renames      map[string]string
	selected     string
	activeConfig string
	apiKey       string // runtime-generated key; overrides config when set
	tokps        map[string]float64
	clusterTokps map[string]float64
}

// effectiveAPIKey is the key currently enforced: a runtime-generated one wins,
// else the config's. Empty = auth disabled.
func (s *Server) effectiveAPIKey() string {
	pstate.mu.Lock()
	defer pstate.mu.Unlock()
	if pstate.apiKey != "" {
		return pstate.apiKey
	}
	return s.cfg.APIKey
}

// authOK passes when auth is off, or the request carries the key as a Bearer
// token or X-API-Key header.
func (s *Server) authOK(r *http.Request) bool {
	key := s.effectiveAPIKey()
	if key == "" {
		return true
	}
	return r.Header.Get("Authorization") == "Bearer "+key || r.Header.Get("X-API-Key") == key
}

// guard wraps a MUTATING handler so it requires the API key when one is set —
// load/eject/config never happen from an unauthenticated device on the LAN.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authOK(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "API key requerida para esta acción"})
			return
		}
		next(w, r)
	}
}

// panelGenKey mints a fresh random API key, activates it immediately, and
// returns it so the panel can show + copy it (Jupyter-token style).
func (s *Server) panelGenKey(w http.ResponseWriter, r *http.Request) {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	key := "sk-soflink-" + hex.EncodeToString(b)
	pstate.mu.Lock()
	pstate.apiKey = key
	pstate.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "api_key": key})
}

var pstate = &panelState{
	renames:      map[string]string{},
	selected:     "decode",
	tokps:        map[string]float64{},
	clusterTokps: map[string]float64{},
}

// panelPage serves the embedded live dashboard, so a node's binary carries its
// own UI — no separate web server, no Python interpreter to ship.
func (s *Server) panelPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/panel" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(panelHTML)
}

// probeClient has a SHORT timeout: panel status probes must fail fast when a
// backend is unreachable from this node (e.g. a remote node probing another
// host's prefill port through a closed firewall) — otherwise /api/status hangs
// on the 600s serving client and the whole dashboard shows blank.
var probeClient = &http.Client{Timeout: 1000 * time.Millisecond}

// getJSON GETs and decodes a path from a base URL (served-model info).
func (s *Server) getJSONFrom(base, pth string) map[string]any {
	if base == "" {
		return nil
	}
	resp, err := probeClient.Get(base + pth)
	if err != nil || resp == nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var m map[string]any
	if json.NewDecoder(resp.Body).Decode(&m) != nil {
		return nil
	}
	return m
}

func (s *Server) getJSON(pth string) map[string]any { return s.getJSONFrom(s.bc.DecodeEntryURL, pth) }

// instanceEndpoint returns the base URL of an instance role.
func (s *Server) instanceEndpoint(key string) string {
	switch key {
	case "prefill":
		return s.bc.PrefillURL
	default:
		return s.bc.DecodeEntryURL
	}
}

// ---- served-model + node assembly ----------------------------------------

func (s *Server) servedModel() (loaded bool, model, quant string, nctx, slots int, sizeGB float64, modelPath string) {
	props := s.getJSON("/props")
	models := s.getJSON("/v1/models")
	loaded = props != nil
	slots = 4
	if props != nil {
		modelPath = pstr(props["model_path"])
	}
	if data, ok := models["data"].([]any); ok && len(data) > 0 {
		if d0, ok := data[0].(map[string]any); ok {
			model = strings.TrimSuffix(path.Base(pstr(d0["id"])), ".gguf")
			if meta, ok := d0["meta"].(map[string]any); ok {
				nctx = pint(meta["n_ctx"])
				quant = pstr(meta["ftype"])
				sizeGB = round1(pnum(meta["size"]) / 1e9)
			}
		}
	}
	if props != nil {
		if v := pint(props["total_slots"]); v > 0 {
			slots = v
		}
		if quant == "" {
			quant = pstr(props["model_ftype"])
		}
	}
	return
}

func (s *Server) aggregateNodes() map[string]map[string]any {
	refs := make([]gateway.NodeRef, 0, len(s.cfg.Nodes))
	for _, n := range s.cfg.Nodes {
		refs = append(refs, gateway.NodeRef{Name: n.ID, URL: n.Agent + "/gpu"})
	}
	list := gateway.AggregateNodes(refs, gateway.HTTPNodeFetcher(2500*time.Millisecond))
	out := map[string]map[string]any{}
	for _, n := range list {
		out[pstr(n["name"])] = n
	}
	return out
}

// ---- presets / active config / pipeline ----------------------------------

// activePreset picks the loaded layout: the operator's pick if set and matching,
// else the preset whose model name matches the served model and whose topology
// nodes are all backed by live VRAM (disambiguates same-model configs by which
// nodes actually hold weights).
func (s *Server) activePreset(model string, cn map[string]map[string]any) *config.Preset {
	byKey := map[string]*config.Preset{}
	for i := range s.cfg.Presets {
		byKey[s.cfg.Presets[i].Key] = &s.cfg.Presets[i]
	}
	pstate.mu.Lock()
	pick := pstate.activeConfig
	pstate.mu.Unlock()
	if p, ok := byKey[pick]; ok {
		return p
	}
	loaded := strings.ToLower(model)
	hasVRAM := func(node string) bool {
		for _, g := range asMaps(cn[node]["gpus"]) {
			if pnum(g["used_mb"]) > 4000 {
				return true
			}
		}
		return false
	}
	var best *config.Preset
	bestScore := -1
	for i := range s.cfg.Presets {
		p := &s.cfg.Presets[i]
		if p.ModelName != "" && !strings.Contains(loaded, strings.ToLower(p.ModelName)) {
			continue
		}
		score := 0
		for _, st := range p.Topology {
			if hasVRAM(st.Node) {
				score++
			}
		}
		if score > bestScore {
			bestScore, best = score, p
		}
	}
	return best
}

// pipelineFrom builds the panel pipeline (stages + hop) from a preset topology.
func (s *Server) pipelineFrom(p *config.Preset) map[string]any {
	if p == nil || len(p.Topology) == 0 {
		return nil
	}
	stages := make([]map[string]any, 0, len(p.Topology))
	total := 0
	for _, st := range p.Topology {
		stages = append(stages, map[string]any{"node": st.Node, "layers": st.Layers, "gpus": st.GPUs})
		total += st.Layers
	}
	pl := map[string]any{"stages": stages, "total_layers": total}
	if hop := s.hopMS(p); hop > 0 {
		pl["hop_ms"] = hop
	}
	return pl
}

// hopMS is a quick median TCP RTT (ms) to the served endpoint's host — the
// per-hop latency that bounds single-stream decode.
func (s *Server) hopMS(p *config.Preset) float64 {
	hostport := strings.TrimPrefix(strings.TrimPrefix(p.Endpoint, "http://"), "https://")
	if i := strings.IndexByte(hostport, '/'); i >= 0 {
		hostport = hostport[:i]
	}
	if !strings.Contains(hostport, ":") {
		return 0
	}
	var samples []float64
	for i := 0; i < 3; i++ {
		t0 := time.Now()
		c, err := net.DialTimeout("tcp", hostport, 400*time.Millisecond)
		if err != nil {
			continue
		}
		samples = append(samples, float64(time.Since(t0).Microseconds())/1000.0)
		c.Close()
	}
	if len(samples) == 0 {
		return 0
	}
	sort.Float64s(samples)
	return round2(samples[len(samples)/2])
}

// ---- status ---------------------------------------------------------------

func (s *Server) panelStatus(w http.ResponseWriter, r *http.Request) {
	// Probe the served model, the nodes, and the prefill role CONCURRENTLY — a
	// remote node reaching another host's closed port must not chain into a
	// multi-second stall that blanks the whole dashboard.
	var (
		loaded              bool
		model, quant, modelPath string
		nctx, slots         int
		sizeGB              float64
		cn                  map[string]map[string]any
		prefillUp           bool
	)
	var pwg sync.WaitGroup
	pwg.Add(3)
	go func() { defer pwg.Done(); loaded, model, quant, nctx, slots, sizeGB, modelPath = s.servedModel() }()
	go func() { defer pwg.Done(); cn = s.aggregateNodes() }()
	go func() {
		defer pwg.Done()
		prefillUp = s.bc.PrefillURL != "" && s.getJSONFrom(s.bc.PrefillURL, "/health") != nil
	}()
	pwg.Wait()
	preset := s.activePreset(model, cn)
	pipe := s.pipelineFrom(preset)

	pstate.mu.Lock()
	renames := copyStr(pstate.renames)
	selected := pstate.selected
	tokps := copyNum(pstate.tokps)
	cluster := copyNum(pstate.clusterTokps)
	pstate.mu.Unlock()

	// per-node layer ranges from the active pipeline (cumulative).
	ranges := map[string][2]int{}
	if pipe != nil {
		off := 0
		for _, st := range asMaps(pipe["stages"]) {
			n := pstr(st["node"])
			l := pint(st["layers"])
			ranges[n] = [2]int{off, off + l - 1}
			off += l
		}
	}

	nodes := make([]map[string]any, 0, len(s.cfg.Nodes))
	order := []string{}
	for _, n := range s.cfg.Nodes {
		order = append(order, n.ID)
	}
	for _, id := range order {
		n := cn[id]
		up, _ := n["up"].(bool)
		gpus := n["gpus"]
		active := hasRange(ranges, id) && loaded
		role := "worker (rpc)"
		if id == "node-a" {
			role = "master + worker"
		}
		if active {
			// keep role
		} else if up {
			role = "libre · disponible"
		}
		nd := map[string]any{
			"node": id, "role": role, "up": up, "active": active,
			"busy": false, "gpus": gpus,
			"stats": map[string]any{
				"cpu": n["cpu"], "ram_used_mb": n["ram_used_mb"],
				"ram_total_mb": n["ram_total_mb"], "rx": n["rx"], "tx": n["tx"],
			},
		}
		if rg, ok := ranges[id]; ok {
			nd["range"] = []int{rg[0], rg[1]}
		}
		nodes = append(nodes, nd)
	}

	// context footprint = active-node VRAM used minus weights.
	var vram float64
	for _, nd := range nodes {
		if b, _ := nd["active"].(bool); b {
			for _, g := range asMaps(nd["gpus"]) {
				vram += pnum(g["used_mb"])
			}
		}
	}
	var ctxGB any
	if sizeGB > 0 && vram/1024.0 > sizeGB+0.3 {
		ctxGB = round1(vram/1024.0 - sizeGB)
	}

	// instances: decode (served model) + prefill (if a prefill backend answers).
	// Each carries its connection surface (endpoint, model path, API key); when
	// roles are split, they also carry the coordinator that fronts them.
	coordinator := s.cfg.PublicURL
	apiKey := s.effectiveAPIKey()
	coordShown := ""
	if loaded && prefillUp {
		coordShown = coordinator
	}
	instConn := func(key, fallback string) connInfo {
		ep := fallback
		if inst, ok := s.cfg.Instance(key); ok && inst.Endpoint != "" {
			ep = inst.Endpoint
		}
		return connInfo{endpoint: ep, modelPath: modelPath, apiKey: apiKey, apiKeyEnabled: apiKey != "", coordinator: coordShown}
	}
	instances := []map[string]any{}
	if loaded {
		instances = append(instances, s.instance("decode", "decode · genera la respuesta", model, selected, pipe, tokps, instConn("decode", s.bc.DecodeEntryURL)))
	}
	if prefillUp {
		instances = append(instances, s.instance("prefill", "prefill · procesa el prompt", model, selected, pipe, tokps, instConn("prefill", s.bc.PrefillURL)))
	}

	// configs for the picker + nodes modal.
	configs := make([]map[string]any, 0, len(s.cfg.Presets))
	for i := range s.cfg.Presets {
		p := &s.cfg.Presets[i]
		topo := make([]map[string]any, 0, len(p.Topology))
		for _, st := range p.Topology {
			topo = append(topo, map[string]any{"node": st.Node, "layers": st.Layers, "gpus": st.GPUs})
		}
		configs = append(configs, map[string]any{
			"key": p.Key, "label": p.Label, "main": p.Main, "remote": p.Remote,
			"note": p.Note, "quant": p.Quant, "ctx": p.Ctx, "size_gb": p.SizeGB,
			"topology": topo, "launchable": p.Main == "node-a", "active": p.Key == activeKey(preset),
		})
	}

	upn := 0
	for _, nd := range nodes {
		if b, _ := nd["up"].(bool); b {
			upn++
		}
	}
	var clusterOut any
	if len(cluster) > 0 {
		clusterOut = cluster
	}
	endpointLabel := "coordinator Go"
	if coordinator != "" {
		endpointLabel = coordinator
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"loaded": loaded, "loading": false, "model": model, "quant": quant,
		"size_gb": sizeGB, "n_ctx": nctx, "slots": slots, "context_gb": ctxGB,
		"endpoint": endpointLabel, "model_path": modelPath, "nodes": nodes,
		"instances": instances, "pipeline": pipe, "configs": configs, "names": renames,
		"tokps": nz(tokps[selected]), "cluster_tokps": clusterOut,
		"selected": selected, "active_config": activeKey(preset),
		"coordinator": coordinator, "api_key": apiKey, "api_key_enabled": apiKey != "",
		"_upn": upn,
	})
}

// connInfo is the per-instance connection surface the panel shows (like LM
// Studio's "Reachable at" + cURL): where the API answers, the served model
// path, the optional API key, and — when roles are split — the coordinator that
// fronts them all.
type connInfo struct {
	endpoint, modelPath, apiKey, coordinator string
	apiKeyEnabled                            bool
}

func (s *Server) instance(key, funcLabel, model, selected string, pipe map[string]any, tokps map[string]float64, conn connInfo) map[string]any {
	hosts, gpus := 0, 0
	if pipe != nil {
		seen := map[string]bool{}
		for _, st := range asMaps(pipe["stages"]) {
			seen[pstr(st["node"])] = true
			gpus += pint(st["gpus"])
		}
		hosts = len(seen)
	}
	return map[string]any{
		"key": key, "up": true, "selected": key == selected, "model": model,
		"name": model, "func": funcLabel, "role": funcLabel,
		"hosts": hosts, "gpus": gpus, "tokps": nz(tokps[key]), "pipeline": pipe,
		"endpoint": conn.endpoint, "model_path": conn.modelPath,
		"api_key": conn.apiKey, "api_key_enabled": conn.apiKeyEnabled,
		"coordinator": conn.coordinator,
	}
}

// ---- actions --------------------------------------------------------------

// chatCall posts a chat request and returns (text, completionTokens, secs,
// tok/s, prompt tok/s).
func (s *Server) chatCall(base string, messages any, maxTokens int) (text string, ct int, secs, tokps, promptTokps float64, err error) {
	body, _ := json.Marshal(map[string]any{
		"messages": messages, "max_tokens": maxTokens, "temperature": 0.4,
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	})
	t0 := time.Now()
	resp, err := s.client.Post(base+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", 0, 0, 0, 0, err
	}
	defer resp.Body.Close()
	var d map[string]any
	if err = json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return "", 0, 0, 0, 0, err
	}
	dt := time.Since(t0).Seconds()
	if u, ok := d["usage"].(map[string]any); ok {
		ct = pint(u["completion_tokens"])
	}
	if ch, ok := d["choices"].([]any); ok && len(ch) > 0 {
		if m, ok := ch[0].(map[string]any); ok {
			if msg, ok := m["message"].(map[string]any); ok {
				text = pstr(msg["content"])
			}
		}
	}
	if tim, ok := d["timings"].(map[string]any); ok {
		promptTokps = round1(pnum(tim["prompt_per_second"]))
	}
	tokps = 0
	if dt > 0 {
		tokps = round1(float64(ct) / dt)
	}
	return text, ct, round2(dt), tokps, promptTokps, nil
}

func (s *Server) panelChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages any    `json:"messages"`
		Mode     string `json:"mode"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	dec := s.bc.DecodeEntryURL
	if req.Mode == "cluster" && s.bc.PrefillURL != "" {
		pre, _, _, _, preTps, e1 := s.chatCall(s.bc.PrefillURL, req.Messages, 1)
		text, ct, secs, tps, _, e2 := s.chatCall(dec, req.Messages, 2048)
		_ = pre
		if e2 != nil {
			writeJSON(w, http.StatusOK, map[string]any{"text": "(error: " + errStr(e2) + ")"})
			return
		}
		pf := preTps
		if e1 != nil {
			pf = 0
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"text": textOr(text), "tokps": tps, "tokens": ct, "secs": secs,
			"mode": "cluster", "prefill_tokps": pf, "decode_tokps": tps,
		})
		return
	}
	text, ct, secs, tps, _, err := s.chatCall(dec, req.Messages, 2048)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"text": "(error: " + errStr(err) + ")"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"text": textOr(text), "tokps": tps, "tokens": ct, "secs": secs, "mode": "individual",
	})
}

func (s *Server) panelMeasure(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	pstate.mu.Lock()
	sel := pstate.selected
	pstate.mu.Unlock()

	if req.Mode == "cluster" && s.bc.PrefillURL != "" {
		_, _, _, _, preTps, e1 := s.chatCall(s.bc.PrefillURL, measureMsgs, 1)
		_, _, _, decTps, _, e2 := s.chatCall(s.bc.DecodeEntryURL, measureMsgs, 220)
		if e2 != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": errStr(e2)})
			return
		}
		pf := preTps
		if e1 != nil {
			pf = 0
		}
		pstate.mu.Lock()
		pstate.clusterTokps = map[string]float64{"prefill": pf, "decode": decTps}
		pstate.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": "cluster", "prefill": pf, "decode": decTps})
		return
	}
	if req.Mode == "concurrent" {
		agg, per := s.measureConcurrent(s.instanceEndpoint(sel), 4)
		pstate.mu.Lock()
		pstate.clusterTokps = map[string]float64{"concurrent": agg, "single": per}
		pstate.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": "concurrent", "aggregate": agg, "per_stream_avg": per})
		return
	}
	_, _, _, tps, _, err := s.chatCall(s.instanceEndpoint(sel), measureMsgs, 220)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": errStr(err)})
		return
	}
	pstate.mu.Lock()
	pstate.tokps[sel] = tps
	pstate.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": "individual", "tokps": tps})
}

// measureConcurrent fires n requests at once and reports aggregate tok/s
// (total tokens / wall) plus the mean per-stream rate.
func (s *Server) measureConcurrent(base string, n int) (aggregate, perStream float64) {
	type res struct {
		ct  int
		tps float64
	}
	out := make([]res, n)
	var wg sync.WaitGroup
	t0 := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, ct, _, tps, _, err := s.chatCall(base, measureMsgs, 200)
			if err == nil {
				out[i] = res{ct, tps}
			}
		}(i)
	}
	wg.Wait()
	wall := time.Since(t0).Seconds()
	total, sum, cnt := 0, 0.0, 0
	for _, rr := range out {
		total += rr.ct
		if rr.tps > 0 {
			sum += rr.tps
			cnt++
		}
	}
	if wall > 0 {
		aggregate = round1(float64(total) / wall)
	}
	if cnt > 0 {
		perStream = round1(sum / float64(cnt))
	}
	return
}

func (s *Server) panelSelectInstance(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Instance string `json:"instance"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Instance != "" {
		pstate.mu.Lock()
		pstate.selected = req.Instance
		pstate.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) panelSetConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Config string `json:"config"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	pstate.mu.Lock()
	pstate.activeConfig = req.Config
	pstate.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) panelRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Node string `json:"node"`
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	pstate.mu.Lock()
	if req.Name == "" {
		delete(pstate.renames, req.Node)
	} else {
		pstate.renames[req.Node] = req.Name
	}
	pstate.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- small helpers --------------------------------------------------------

func pstr(v any) string { s, _ := v.(string); return s }
func pnum(v any) float64 {
	switch n := v.(type) {
	case float64: // decoded from JSON
		return n
	case int: // native (config-derived pipeline values)
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}
func pint(v any) int { return int(pnum(v)) }
func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

func asMaps(v any) []map[string]any {
	switch a := v.(type) {
	case []map[string]any: // built in-process (pipeline stages)
		return a
	case []any: // decoded from JSON (node gpus)
		out := make([]map[string]any, 0, len(a))
		for _, e := range a {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}
func copyStr(m map[string]string) map[string]string {
	o := map[string]string{}
	for k, v := range m {
		o[k] = v
	}
	return o
}
func copyNum(m map[string]float64) map[string]float64 {
	o := map[string]float64{}
	for k, v := range m {
		o[k] = v
	}
	return o
}
func hasRange(m map[string][2]int, k string) bool { _, ok := m[k]; return ok }
func nz(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}
func activeKey(p *config.Preset) any {
	if p == nil {
		return nil
	}
	return p.Key
}
func errStr(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}
func textOr(t string) string {
	if t == "" {
		return "(sin contenido)"
	}
	return t
}
