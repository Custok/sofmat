package coordinator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Custok/sofmat/internal/config"
)

// engineWithSlots serves /slots with the given per-slot prompt-token counts.
func engineWithSlots(t *testing.T, held ...int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/slots"):
			out := make([]map[string]any, 0, len(held))
			for i, n := range held {
				out = append(out, map[string]any{
					"id": i, "n_prompt_tokens": float64(n), "is_processing": false,
				})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasPrefix(r.URL.Path, "/props"):
			writeTestJSON(w, 200, map[string]any{"default_generation_settings": map[string]any{"n_ctx": 100096.0}})
		default:
			writeTestJSON(w, 200, map[string]any{
				"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "ok"}}},
			})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// residentServer needs BOTH roles and both soflink agents: cacheResident only
// exists when the KV pipe is wired, and a decode-only config leaves it nil —
// which made the first version of these tests pass by never running the code
// they were written for.
func residentServer(t *testing.T, engine *httptest.Server) *Server {
	t.Helper()
	pre := newFakeEngine(t)
	preCtl := soflinkFor(t, pre.dir)
	decCtl := soflinkFor(t, t.TempDir())
	s, err := NewServer(&config.Config{
		Nodes: []config.Node{
			{ID: "node-c", Agent: preCtl.URL, GPUs: 2},
			{ID: "node-d", Agent: decCtl.URL, GPUs: 2},
		},
		Instances: []config.Instance{
			{Key: "decode", Role: "decode", Endpoint: engine.URL, Main: "node-d"},
			{Key: "prefill", Role: "prefill", Endpoint: pre.srv.URL, Main: "node-c"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.kp == nil {
		t.Fatal("sin kvPipe, cacheResident no se ejecuta: el test no probaría nada")
	}
	return s
}

func convo(text string) map[string]any {
	return map[string]any{"messages": []any{map[string]any{"role": "user", "content": text}}}
}

// TestBigSlotOfAnotherConversationIsNotMine reproduces what a HUD request paid
// for on 2026-09-09: one engine, several conversations, and a slot holding
// someone else's 13k tokens. The probe used to answer "yes, your prefix is
// resident" because SOME slot was big, the admission kept its optimistic
// estimate, and the decode prefilled all 17,189 tokens from scratch — 18 of the
// request's 21.7 seconds.
func TestBigSlotOfAnotherConversationIsNotMine(t *testing.T) {
	// one big slot and TWO more holding somebody else's smaller conversations:
	// the big one may or may not be mine, and "may" has to read as cold
	engine := engineWithSlots(t, 20000, 3000, 5000, 0)
	s := residentServer(t, engine)

	mine := convo("proyecto tres " + strings.Repeat("x", 400))
	if s.cacheResident(s.kp, mine, 15000) {
		t.Fatal("un slot grande de OTRA conversación se atribuyó a la mía: " +
			"eso es lo que costó 18 s de prefill en frío")
	}
}

// TestAloneOnTheEngineStillTrustsItsSlot is the control. Distrusting always
// would pass the test above and send every hot prompt through the handoff,
// paying a transfer that was not needed.
func TestAloneOnTheEngineStillTrustsItsSlot(t *testing.T) {
	engine := engineWithSlots(t, 20000, 0, 0, 0)
	s := residentServer(t, engine)

	only := convo("una sola conversación " + strings.Repeat("y", 400))
	if !s.cacheResident(s.kp, only, 15000) {
		t.Fatal("con UNA sola conversación en el motor, su slot grande es suyo: " +
			"desconfiar aquí manda al prefill algo que ya está caliente")
	}
}

// TestEnoughBigSlotsForEveryone: every conversation the engine is caching has a
// big slot, so mine is big whichever one it is. This refinement keeps the fix
// from taxing an engine that is busy AND warm.
func TestEnoughBigSlotsForEveryone(t *testing.T) {
	engine := engineWithSlots(t, 20000, 20000, 20000, 0)
	s := residentServer(t, engine)
	mine := convo("gamma " + strings.Repeat("z", 400))
	if !s.cacheResident(s.kp, mine, 15000) {
		t.Fatal("tres conversaciones y tres slots grandes: hay uno para cada una")
	}
}

// TestUnreadableEngineDoesNotCreateWork: a failed probe must not be turned into
// a cold prefill. Not knowing is not the same as knowing it is cold.
func TestUnreadableEngineDoesNotCreateWork(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/props") {
			writeTestJSON(w, 200, map[string]any{"default_generation_settings": map[string]any{"n_ctx": 100096.0}})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(dead.Close)
	s := residentServer(t, dead)
	c := convo("lo que sea " + strings.Repeat("w", 400))
	if !s.cacheResident(s.kp, c, 15000) {
		t.Fatal("una sonda que no se puede leer no debe convertirse en trabajo extra")
	}
}
