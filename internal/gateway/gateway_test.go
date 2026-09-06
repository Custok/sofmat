package gateway

import (
	"errors"
	"strings"
	"testing"
)

var bigPrompt = strings.Repeat("x", 40000) // ~10000 est. tokens > threshold (6144)

func okResp() Body {
	return Body{"choices": []any{map[string]any{"ok": true}}, "timings": map[string]any{}}
}

type calls struct {
	prefill []Headers
	decode  []Headers
	bodies  []Body
	handoff []string
	slots   []string
}

func newTestGW(t *testing.T, mutate func(*Options)) (*Gateway, *calls) {
	t.Helper()
	c := &calls{}
	o := Options{
		Verify: func(Headers) bool { return true },
		BackendCall: func(body Body, extra Headers) (Body, error) {
			c.decode = append(c.decode, extra)
			c.bodies = append(c.bodies, body)
			return okResp(), nil
		},
		StatusProvider: func() Body { return Body{} },
		PrefillCall: func(body Body, extra Headers) (Body, error) {
			c.prefill = append(c.prefill, extra)
			return Body{"handoff_id": "h-1"}, nil
		},
		Handoff: func(hid, slot string) (Body, error) {
			c.handoff = append(c.handoff, hid)
			c.slots = append(c.slots, slot)
			return Body{}, nil
		},
	}
	if mutate != nil {
		mutate(&o)
	}
	gw, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return gw, c
}

func chatBody(system, user string) Body {
	return Body{"messages": []any{
		map[string]any{"role": "system", "content": system},
		map[string]any{"role": "user", "content": user},
	}}
}

func lastRecord(t *testing.T, gw *Gateway) Record {
	t.Helper()
	rows, _ := gw.Requests(Headers{}, 1)
	if len(rows) == 0 {
		t.Fatal("no request recorded")
	}
	return rows[len(rows)-1]
}

func TestAuthRejected(t *testing.T) {
	gw, _ := newTestGW(t, func(o *Options) {
		o.Verify = func(Headers) bool { return false }
	})
	if _, err := gw.Chat(Headers{}, chatBody("s", "u")); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
	if _, err := gw.Status(Headers{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("status must auth")
	}
	if _, err := gw.Requests(Headers{}, 5); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("requests must auth")
	}
}

func TestLargePromptGoesThroughPrefillAndHandoff(t *testing.T) {
	gw, c := newTestGW(t, nil)
	if _, err := gw.Chat(Headers{}, chatBody(bigPrompt, "hola")); err != nil {
		t.Fatal(err)
	}
	if len(c.prefill) != 1 || len(c.handoff) != 1 || c.handoff[0] != "h-1" {
		t.Fatalf("prefill/handoff not driven: %+v", c)
	}
	if c.decode[0]["x-sofmat-kv-handoff"] != "h-1" {
		t.Fatal("decode must receive the handoff marker")
	}
	if c.decode[0]["x-sofmat-slot"] == "" {
		t.Fatal("decode must receive a slot")
	}
	if c.slots[0] != c.decode[0]["x-sofmat-slot"] {
		t.Fatal("handoff must restore into the slot the decode is pinned to")
	}
}

// After a handoff the decode body must carry the ENGINE-visible fields: the
// slot holding the restored KV (id_slot) and cache_prompt, else the engine
// would re-process the prompt and the handoff would be wasted.
func TestHandoffPinsEngineSlotInBody(t *testing.T) {
	gw, c := newTestGW(t, nil)
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	b := c.bodies[0]
	slot, ok := b["id_slot"].(int)
	if !ok {
		t.Fatalf("id_slot must be an int in the decode body: %v", b["id_slot"])
	}
	if want := c.decode[0]["x-sofmat-slot"]; want != itoa(slot) {
		t.Fatalf("id_slot %d != routed slot %s", slot, want)
	}
	if b["cache_prompt"] != true {
		t.Fatal("cache_prompt must be true after a handoff")
	}
	// the caller's body is never mutated
	if _, ok := chatBody(bigPrompt, "hola")["id_slot"]; ok {
		t.Fatal("caller body mutated")
	}
}

// The handoff driver may restore into a different idle slot: the decode call
// must follow it (id_slot, header, record), never the affinity pick.
func TestHandoffSlotOverrideFollowed(t *testing.T) {
	gw, c := newTestGW(t, func(o *Options) {
		o.Handoff = func(hid, slot string) (Body, error) { return Body{"slot": "3", "restore_ms": 50.0}, nil }
	})
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if c.bodies[0]["id_slot"] != 3 || c.decode[0]["x-sofmat-slot"] != "3" {
		t.Fatalf("decode must follow the driver's slot: %v %v", c.bodies[0]["id_slot"], c.decode[0])
	}
	if lastRecord(t, gw)["slot"] != "3" {
		t.Fatal("record must show the slot actually used")
	}
}

func TestSmallPromptBodyHasNoSlotPin(t *testing.T) {
	gw, c := newTestGW(t, nil)
	gw.Chat(Headers{}, chatBody("corto", "hola"))
	if _, ok := c.bodies[0]["id_slot"]; ok {
		t.Fatal("decode-direct must leave slot selection to the engine")
	}
}

func TestSecondRequestSamePrefixIsDecodeDirect(t *testing.T) {
	gw, c := newTestGW(t, nil)
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	gw.Chat(Headers{}, chatBody(bigPrompt, "otra pregunta corta"))
	if len(c.prefill) != 1 {
		t.Fatalf("prefix hot: second prefill must not happen (%d)", len(c.prefill))
	}
	if len(c.decode) != 2 {
		t.Fatalf("both requests must reach decode (%d)", len(c.decode))
	}
	if _, ok := c.decode[1]["x-sofmat-kv-handoff"]; ok {
		t.Fatal("second request must not carry a handoff marker")
	}
}

func TestSmallPromptNeverTouchesPrefill(t *testing.T) {
	gw, c := newTestGW(t, nil)
	gw.Chat(Headers{}, chatBody("corto", "hola"))
	if len(c.prefill) != 0 || len(c.decode) != 1 {
		t.Fatalf("small prompt routing wrong: %+v", c)
	}
}

func TestNoPrefillConfiguredDegradesSilently(t *testing.T) {
	gw, c := newTestGW(t, func(o *Options) {
		o.PrefillCall = nil
		o.Handoff = nil
	})
	resp, err := gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if err != nil || resp == nil {
		t.Fatalf("must degrade, not fail: %v", err)
	}
	if len(c.decode) != 1 {
		t.Fatal("decode must still be called")
	}
	if gw.Disaggregated() {
		t.Fatal("must report decode-only")
	}
}

func TestPrefillErrorFallsBackToDecode(t *testing.T) {
	gw, c := newTestGW(t, func(o *Options) {
		o.PrefillCall = func(Body, Headers) (Body, error) {
			return nil, errors.New("prefill node down")
		}
	})
	resp, err := gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if err != nil || resp == nil {
		t.Fatalf("request must survive: %v", err)
	}
	if _, ok := c.decode[0]["x-sofmat-kv-handoff"]; ok {
		t.Fatal("failed prefill must not mark a handoff")
	}
	if _, ok := c.bodies[0]["id_slot"]; ok {
		t.Fatal("failed prefill must not pin a slot")
	}
	if lastRecord(t, gw)["prefill_error"] != "prefill node down" {
		t.Fatal("the prefill error must be visible in the record")
	}
}

func TestPrefillPanicFallsBackToDecode(t *testing.T) {
	gw, c := newTestGW(t, func(o *Options) {
		o.PrefillCall = func(Body, Headers) (Body, error) { panic("boom") }
	})
	resp, err := gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if err != nil || resp == nil {
		t.Fatalf("request must survive a panic: %v", err)
	}
	if len(c.decode) != 1 {
		t.Fatal("decode must still run")
	}
}

func TestHandoffErrorFallsBackToDecode(t *testing.T) {
	gw, c := newTestGW(t, func(o *Options) {
		o.Handoff = func(string, string) (Body, error) { return nil, errors.New("fetch failed") }
	})
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if _, ok := c.decode[0]["x-sofmat-kv-handoff"]; ok {
		t.Fatal("failed handoff must not mark the decode call")
	}
	if _, ok := c.bodies[0]["id_slot"]; ok {
		t.Fatal("failed handoff must not pin a slot")
	}
	rec := lastRecord(t, gw)
	if rec["admitted_via"] != "decode-fallback" || rec["handoff_error"] != "fetch failed" {
		t.Fatalf("fallback + cause must be visible: %v", rec)
	}
}

func TestHandoffPanicFallsBackToDecode(t *testing.T) {
	gw, c := newTestGW(t, func(o *Options) {
		o.Handoff = func(string, string) (Body, error) { panic("boom") }
	})
	resp, err := gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if err != nil || resp == nil || len(c.decode) != 1 {
		t.Fatalf("request must survive a handoff panic: %v", err)
	}
}

func TestAdmissionMetricsLogged(t *testing.T) {
	gw, _ := newTestGW(t, nil)
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	last := lastRecord(t, gw)
	if last["admission"] != "large-new-prefill" || last["admitted_via"] != "prefill" {
		t.Fatalf("metrics wrong: %v", last)
	}
	if last["handoff_id"] != "h-1" {
		t.Fatalf("handoff id must be logged: %v", last)
	}
	if _, ok := last["handoff_ms"].(float64); !ok {
		t.Fatalf("handoff_ms must be logged: %v", last)
	}
}

// The drivers' numeric metrics (prefill/save/fetch/restore) land in the record.
func TestDriverMetricsLogged(t *testing.T) {
	gw, _ := newTestGW(t, func(o *Options) {
		o.PrefillCall = func(Body, Headers) (Body, error) {
			return Body{"handoff_id": "h-9", "prefill_ms": 4010.5, "tokens": 8001, "state_bytes": int64(304087040), "note": "dropped"}, nil
		}
		o.Handoff = func(string, string) (Body, error) {
			return Body{"fetch_ms": 270.0, "restore_ms": 62.0, "n_restored": 8000}, nil
		}
	})
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	last := lastRecord(t, gw)
	for _, k := range []string{"prefill_ms", "tokens", "state_bytes", "fetch_ms", "restore_ms", "n_restored"} {
		if _, ok := last[k]; !ok {
			t.Fatalf("metric %s missing: %v", k, last)
		}
	}
	if _, ok := last["note"]; ok {
		t.Fatal("string fields other than handoff_id must not be copied")
	}
}

// prompt_n from the engine is the proof: 1 = restored KV reused; the whole
// prompt = the engine re-processed it → kv_miss.
func TestKVMissFlaggedFromTimings(t *testing.T) {
	pn := 1.0
	gw, _ := newTestGW(t, func(o *Options) {
		o.BackendCall = func(Body, Headers) (Body, error) {
			return Body{"timings": map[string]any{"prompt_n": pn, "cache_n": 8000.0, "predicted_per_second": 43.2}}, nil
		}
	})
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	last := lastRecord(t, gw)
	if last["kv_miss"] != false || last["prompt_n"] != 1 || last["cache_n"] != 8000 {
		t.Fatalf("hit must be recorded: %v", last)
	}
	if last["tg_tokps"] != 43.2 {
		t.Fatalf("tg must be recorded: %v", last)
	}
	pn = 8001
	gw.Chat(Headers{"x-sofmat-tenant": "other"}, chatBody(bigPrompt+"y", "hola"))
	if last := lastRecord(t, gw); last["kv_miss"] != true {
		t.Fatalf("re-processed prompt must flag kv_miss: %v", last)
	}
}

func TestKVMissNotFlaggedWithoutHandoff(t *testing.T) {
	gw, _ := newTestGW(t, func(o *Options) {
		o.BackendCall = func(Body, Headers) (Body, error) {
			return Body{"timings": map[string]any{"prompt_n": 5000.0}}, nil
		}
	})
	gw.Chat(Headers{}, chatBody("corto", "hola"))
	if _, ok := lastRecord(t, gw)["kv_miss"]; ok {
		t.Fatal("kv_miss only applies to requests admitted via prefill")
	}
}

// The exact recount (chat template applied) makes the final call: below the
// floor the estimate over-admitted and decode-direct is cheaper.
func TestExactCountBelowFloorGoesDecode(t *testing.T) {
	gw, c := newTestGW(t, func(o *Options) {
		o.CountTokens = func(Body) (int, error) { return 5000, nil }
	})
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if len(c.prefill) != 0 || len(c.decode) != 1 {
		t.Fatalf("below the exact floor must be decode-direct: %+v", c)
	}
	last := lastRecord(t, gw)
	if last["admitted_via"] != "decode" || last["admission"] != "exact-below-floor" || last["tokens"] != 5000 {
		t.Fatalf("record wrong: %v", last)
	}
}

func TestExactCountAboveFloorGoesPrefill(t *testing.T) {
	gw, c := newTestGW(t, func(o *Options) {
		o.CountTokens = func(Body) (int, error) { return 9000, nil }
	})
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if len(c.prefill) != 1 {
		t.Fatalf("above the exact floor must prefill: %+v", c)
	}
	if last := lastRecord(t, gw); last["tokens"] != 9000 || last["admitted_via"] != "prefill" {
		t.Fatalf("record wrong: %v", last)
	}
}

func TestCustomFloors(t *testing.T) {
	gw, c := newTestGW(t, func(o *Options) {
		o.CountTokens = func(Body) (int, error) { return 3000, nil }
		o.Threshold = 1000
		o.ExactMinTokens = 2500
	})
	gw.Chat(Headers{}, chatBody(strings.Repeat("x", 6000), "hola")) // est 1500 ≥ 1000; exact 3000 ≥ 2500
	if len(c.prefill) != 1 {
		t.Fatalf("custom floors must apply: %+v", c)
	}
}

func TestCountErrorFallsBackToDecode(t *testing.T) {
	gw, c := newTestGW(t, func(o *Options) {
		o.CountTokens = func(Body) (int, error) { return 0, errors.New("tokenize: 503") }
	})
	resp, err := gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if err != nil || resp == nil || len(c.prefill) != 0 || len(c.decode) != 1 {
		t.Fatalf("a count error must degrade to decode-direct: %v %+v", err, c)
	}
	if last := lastRecord(t, gw); last["admitted_via"] != "decode-fallback" || last["prefill_error"] != "count: tokenize: 503" {
		t.Fatalf("record wrong: %v", last)
	}
}

// With a busy probe wired, the handoff only happens while the decode is busy
// (interference to avoid); an idle decode takes the faster direct path.
func TestIdleDecodeGoesDirect(t *testing.T) {
	gw, c := newTestGW(t, func(o *Options) {
		o.DecodeBusy = func() bool { return false }
		o.CountTokens = func(Body) (int, error) { t.Fatal("no count on an idle decode"); return 0, nil }
	})
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if len(c.prefill) != 0 || len(c.decode) != 1 {
		t.Fatalf("idle decode must be direct: %+v", c)
	}
	last := lastRecord(t, gw)
	if last["admitted_via"] != "decode" || last["admission"] != "decode-idle" {
		t.Fatalf("record wrong: %v", last)
	}
}

func TestBusyDecodeGoesPrefill(t *testing.T) {
	gw, c := newTestGW(t, func(o *Options) {
		o.DecodeBusy = func() bool { return true }
	})
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if len(c.prefill) != 1 {
		t.Fatalf("busy decode must offload the prefill: %+v", c)
	}
	if lastRecord(t, gw)["admitted_via"] != "prefill" {
		t.Fatal("record must say prefill")
	}
}

func TestBusyProbePanicReadsIdle(t *testing.T) {
	gw, c := newTestGW(t, func(o *Options) {
		o.DecodeBusy = func() bool { panic("probe down") }
	})
	resp, err := gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if err != nil || resp == nil || len(c.prefill) != 0 {
		t.Fatalf("a broken probe must not fail or offload: %v %+v", err, c)
	}
}

func TestFallbackVisibleInMetrics(t *testing.T) {
	gw, _ := newTestGW(t, func(o *Options) {
		o.Handoff = func(string, string) (Body, error) { return nil, errors.New("no") }
	})
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if lastRecord(t, gw)["admitted_via"] != "decode-fallback" {
		t.Fatalf("fallback must be visible: %v", lastRecord(t, gw))
	}
}

// Prepare/Finish is the streaming contract: the plan carries the body to send
// (stream flag intact, slot pinned) and Finish logs from a timings-only body.
func TestPrepareFinishForStreaming(t *testing.T) {
	gw, c := newTestGW(t, nil)
	body := chatBody(bigPrompt, "hola")
	body["stream"] = true
	p, err := gw.Prepare(Headers{}, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.decode) != 0 {
		t.Fatal("Prepare must not call the backend")
	}
	if p.Body["stream"] != true || p.Body["id_slot"] == nil || p.Headers["x-sofmat-kv-handoff"] != "h-1" {
		t.Fatalf("plan wrong: %v %v", p.Body, p.Headers)
	}
	if rows, _ := gw.Requests(Headers{}, 1); len(rows) != 0 {
		t.Fatal("nothing recorded before Finish")
	}
	gw.Finish(p, Body{"timings": map[string]any{"prompt_n": 1.0, "cache_n": 8000.0}})
	last := lastRecord(t, gw)
	if last["admitted_via"] != "prefill" || last["prompt_n"] != 1 || last["kv_miss"] != false {
		t.Fatalf("record wrong: %v", last)
	}
	if _, ok := last["total_ms"].(float64); !ok {
		t.Fatal("total_ms must be recorded")
	}
}

func TestPrepareAuth(t *testing.T) {
	gw, _ := newTestGW(t, func(o *Options) { o.Verify = func(Headers) bool { return false } })
	if _, err := gw.Prepare(Headers{}, chatBody("s", "u")); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("Prepare must auth")
	}
}

func TestNextWaveDefaultAndCallerOverride(t *testing.T) {
	var seen Body
	gw, _ := newTestGW(t, func(o *Options) {
		o.BackendCall = func(body Body, extra Headers) (Body, error) {
			seen = body
			return okResp(), nil
		}
	})
	gw.Chat(Headers{}, chatBody("s", "def f():\n  pass"))
	if seen[SpeculativeNMaxKey] != 3 {
		t.Fatalf("code prompt must default n_max 3: %v", seen[SpeculativeNMaxKey])
	}
	body := chatBody("s", "def f():\n  pass")
	body[SpeculativeNMaxKey] = 4
	gw.Chat(Headers{}, body)
	if seen[SpeculativeNMaxKey] != 4 {
		t.Fatal("caller-set n_max must not be overridden")
	}
}

func TestAlphaEmaFedFromTimings(t *testing.T) {
	n := 0
	gw, _ := newTestGW(t, func(o *Options) {
		o.BackendCall = func(body Body, extra Headers) (Body, error) {
			n++
			return Body{"timings": map[string]any{
				"draft_n": float64(10), "draft_n_accepted": float64(3)}}, nil
		}
	})
	h := Headers{"x-sofmat-tenant": "t1"}
	for i := 0; i < 4; i++ { // warmup 3, then the live alpha drives n_max
		gw.Chat(h, chatBody("s", "def f():\n  pass"))
	}
	last := lastRecord(t, gw)
	// alpha ~0.3 < 0.55 -> n_max 2 despite the code-domain prompt
	if last["n_max"] != 2 {
		t.Fatalf("live alpha must supersede domain guess: %v", last)
	}
	if last["alpha_ema"] == nil {
		t.Fatal("alpha_ema must be logged once warmed")
	}
}

func TestSlotAffinityDeterministic(t *testing.T) {
	gw, c := newTestGW(t, nil)
	gw.Chat(Headers{}, chatBody("mismo prefijo", "a"))
	gw.Chat(Headers{}, chatBody("mismo prefijo", "b"))
	if c.decode[0]["x-sofmat-slot"] != c.decode[1]["x-sofmat-slot"] {
		t.Fatal("same prefix must pin to the same slot")
	}
}

func TestBackendErrorPropagates(t *testing.T) {
	gw, _ := newTestGW(t, func(o *Options) {
		o.BackendCall = func(Body, Headers) (Body, error) {
			return nil, errors.New("engine down")
		}
	})
	if _, err := gw.Chat(Headers{}, chatBody("s", "u")); err == nil {
		t.Fatal("backend errors must propagate")
	}
	if lastRecord(t, gw)["error"] != "engine down" {
		t.Fatal("a failed decode must still leave a record")
	}
}

func itoa(i int) string {
	return strings.TrimSpace(strings.Repeat(" ", 0) + string(rune('0'+i)))
}
