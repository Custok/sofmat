package coordinator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Custok/sofmat/internal/config"
)

// panelEngine answers like llama-server for the panel's probes (/props,
// /v1/models, /health) and counts the chat requests it receives.
func panelEngine(t *testing.T, model, ftype string, nctx, slots int, chats *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/props":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_slots": slots, "model_path": "/m/" + model + ".gguf"})
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{
				"id": model + ".gguf", "meta": map[string]any{"n_ctx": nctx, "ftype": ftype, "size": 1e10},
			}}})
		case "/v1/chat/completions":
			atomic.AddInt32(chats, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}],\"timings\":{\"predicted_per_second\":50,\"predicted_n\":1}}\n\ndata: [DONE]\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A role "solo" instance selected on the panel must drive the SERVED MODEL card,
// the panel chat and "Medir" with ITS OWN endpoint — not fall back to decode.
// Regression: with the router (Q4, 2 slots) selected the card showed the decode's
// Q6, 4 slots and 100k, and chat/measure went to the decode.
func TestSelectedSoloInstanceUsesItsOwnEndpoint(t *testing.T) {
	var decChats, rtChats int32
	dec := panelEngine(t, "Model-Q6_K", "Q6_K", 100096, 4, &decChats)
	rt := panelEngine(t, "Model-Q4_K_M", "Q4_K - Medium", 53248, 2, &rtChats)
	s, err := NewServer(&config.Config{Instances: []config.Instance{
		{Key: "decode", Role: "decode", Endpoint: dec.URL},
		{Key: "router", Role: "solo", Endpoint: rt.URL, ModelName: "Model-Q4_K_M"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	pstate.mu.Lock()
	prev := pstate.selected
	pstate.mu.Unlock()
	t.Cleanup(func() { pstate.mu.Lock(); pstate.selected = prev; pstate.mu.Unlock() })

	h := s.Handler()
	if code, _ := postJSON(t, h, "/api/selectinstance", map[string]any{"instance": "router"}); code != http.StatusOK {
		t.Fatalf("selectinstance: %d", code)
	}

	// 1. the card
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var st map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("status json: %v", err)
	}
	if st["model"] != "Model-Q4_K_M" || st["quant"] != "Q4_K - Medium" || pint(st["slots"]) != 2 || pint(st["n_ctx"]) != 53248 {
		t.Errorf("card shows model=%v quant=%v slots=%v n_ctx=%v; want the router's Q4, 2 slots, 53248",
			st["model"], st["quant"], st["slots"], st["n_ctx"])
	}

	// 2. "Medir"
	decBefore, rtBefore := atomic.LoadInt32(&decChats), atomic.LoadInt32(&rtChats)
	postJSON(t, h, "/api/measure", map[string]any{"mode": "individual"})
	if atomic.LoadInt32(&rtChats) == rtBefore || atomic.LoadInt32(&decChats) != decBefore {
		t.Errorf("measure hit router %d times, decode %d times; want router only",
			atomic.LoadInt32(&rtChats)-rtBefore, atomic.LoadInt32(&decChats)-decBefore)
	}

	// 3. panel chat
	decBefore, rtBefore = atomic.LoadInt32(&decChats), atomic.LoadInt32(&rtChats)
	postJSON(t, h, "/api/chat/stream", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hola"}}})
	if atomic.LoadInt32(&rtChats) == rtBefore || atomic.LoadInt32(&decChats) != decBefore {
		t.Errorf("chat hit router %d times, decode %d times; want router only",
			atomic.LoadInt32(&rtChats)-rtBefore, atomic.LoadInt32(&decChats)-decBefore)
	}

	// decode stays decode, unknown keys fall back to decode
	if got := s.instanceEndpoint("decode"); got != s.bc.DecodeEntryURL {
		t.Errorf("instanceEndpoint(decode) = %q, want %q", got, s.bc.DecodeEntryURL)
	}
	if got := s.selectedBase("nope"); got != s.bc.DecodeEntryURL {
		t.Errorf("selectedBase(unknown) = %q, want decode %q", got, s.bc.DecodeEntryURL)
	}
}
