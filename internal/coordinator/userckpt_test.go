package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Custok/sofmat/internal/gateway"
)

type ckptEngine struct {
	mu    sync.Mutex
	calls []string
	pass  map[string]any
}

func newCkptEngine(t *testing.T) (*ckptEngine, *httptest.Server) {
	e := &ckptEngine{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		e.mu.Lock()
		e.calls = append(e.calls, r.URL.Path)
		e.mu.Unlock()
		switch r.URL.Path {
		case "/apply-template":
			writeTestJSON(w, 200, map[string]any{"prompt": "<|im_start|>system\nS<|im_end|>\n<|im_start|>user\nhola<|im_end|>\n" +
				"<|im_start|>assistant\ntexto<|im_end|>\n<|im_start|>user\notra<|im_end|>\n<|im_start|>assistant\n<think>\n\n</think>\n\n"})
		case "/completion":
			e.mu.Lock()
			e.pass = b
			e.mu.Unlock()
			writeTestJSON(w, 200, map[string]any{"id_slot": 2, "timings": map[string]any{"cache_n": 20186.0, "prompt_n": 300.0}})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return e, srv
}

func ckptBody(lastRole string) gateway.Body {
	return gateway.Body{"stream": true, "messages": []any{
		map[string]any{"role": "system", "content": "S"},
		map[string]any{"role": "user", "content": "hola"},
		map[string]any{"role": "assistant", "content": "texto"},
		map[string]any{"role": lastRole, "content": "otra"},
	}}
}

// RED before the fix: no first pass, so the engine's span started where the
// previous turn diverged and no checkpoint ever sat before the reply.
func TestUserTurnProcessesUpToTheEndOfTheUserMessageFirst(t *testing.T) {
	e, srv := newCkptEngine(t)
	s := &Server{}
	plan := &gateway.Plan{Body: ckptBody("user"), Headers: gateway.Headers{}}
	s.checkpointUserTurn(context.Background(), srv.URL, plan)
	if len(e.calls) != 2 || e.calls[0] != "/apply-template" || e.calls[1] != "/completion" {
		t.Fatalf("calls = %v", e.calls)
	}
	p, _ := e.pass["prompt"].(string)
	if !strings.HasSuffix(p, "otra<|im_end|>\n") || strings.Contains(p[strings.LastIndex(p, "otra"):], "assistant") {
		t.Fatalf("first pass must stop right before the reply header, got ...%q", p[max(0, len(p)-40):])
	}
	if e.pass["n_predict"] != 0.0 || e.pass["cache_prompt"] != true {
		t.Fatalf("first pass must only process: %v", e.pass)
	}
	if plan.Body["id_slot"] != 2 {
		t.Fatalf("the request must be pinned to the slot of the first pass, got %v", plan.Body["id_slot"])
	}
}

// Tool rounds inside a turn only append: no extra pass (it would cost a round
// trip per round for nothing).
func TestToolRoundsSkipTheExtraPass(t *testing.T) {
	e, srv := newCkptEngine(t)
	s := &Server{}
	plan := &gateway.Plan{Body: ckptBody("tool"), Headers: gateway.Headers{}}
	s.checkpointUserTurn(context.Background(), srv.URL, plan)
	if len(e.calls) != 0 {
		t.Fatalf("a tool round must not trigger the pass, calls = %v", e.calls)
	}
	if _, ok := plan.Body["id_slot"]; ok {
		t.Fatal("no pin without the pass")
	}
}

// An engine that fails the pass leaves the request untouched.
func TestUserTurnPassFailureIsHarmless(t *testing.T) {
	s := &Server{}
	plan := &gateway.Plan{Body: ckptBody("user"), Headers: gateway.Headers{}}
	s.checkpointUserTurn(context.Background(), "http://127.0.0.1:1", plan)
	if _, ok := plan.Body["id_slot"]; ok {
		t.Fatal("a failed pass must not pin")
	}
}
