package coordinator

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/Custok/sofmat/internal/config"
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
}

// NewServer wires the config's instances into a gateway.
func NewServer(cfg *config.Config) (*Server, error) {
	bc := backendConfigFrom(cfg)
	gw, err := gateway.New(gateway.Options{
		Verify:         func(gateway.Headers) bool { return true }, // TODO: real auth (internal/auth)
		BackendCall:    httpBackend(bc.DecodeEntryURL),
		StatusProvider: func() gateway.Body { return gateway.Body{"status": "ok"} },
		// PrefillCall + Handoff wire in when the engine KV binding
		// (state_seq_get/set) lands; until then decode-only (fail-soft).
		NSlots: 4,
	})
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, gw: gw, bc: bc, client: &http.Client{Timeout: 600 * time.Second}}, nil
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

// httpBackend proxies a request body to endpoint's OpenAI chat endpoint.
func httpBackend(endpoint string) gateway.BackendCall {
	client := &http.Client{Timeout: 600 * time.Second}
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
	mux.HandleFunc("/admin/eject", s.adminEject)
	mux.HandleFunc("/admin/load", s.adminLoad)
	return mux
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
	var body gateway.Body
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, gateway.Body{"error": err.Error()})
		return
	}
	h := gateway.Headers{}
	for k := range r.Header {
		h[k] = r.Header.Get(k)
	}
	out, err := s.gw.Chat(h, body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, gateway.Body{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
