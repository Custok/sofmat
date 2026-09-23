package gateway

import (
	"encoding/json"
	"errors"
	"strconv"
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

// A stale "hot prefix" must not send a cold long prompt to the decode as small:
// when the engine reports it did not have the prefix (cache_n ≈ 0), the
// registry forgets it and the next request is admitted on the whole prompt.
func TestColdPrefixForgottenAfterEngineMiss(t *testing.T) {
	cacheN := 0.0
	gw, c := newTestGW(t, func(o *Options) {
		o.BackendCall = func(body Body, extra Headers) (Body, error) {
			return Body{"timings": map[string]any{"prompt_n": 10000.0, "cache_n": cacheN}}, nil
		}
	})
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola")) // first: prefill + handoff, prefix recorded hot
	if len(c.prefill) != 1 {
		t.Fatalf("first request must prefill: %+v", c)
	}
	// engine says the prefix was NOT there on the direct second request
	gw.Chat(Headers{}, chatBody(bigPrompt, "segunda"))
	if len(c.prefill) != 1 {
		t.Fatal("second request rides the (believed) hot prefix")
	}
	if lastRecord(t, gw)["prefix_cold"] != true {
		t.Fatalf("engine miss must be recorded: %v", lastRecord(t, gw))
	}
	// third request: the registry forgot the prefix → whole prompt counts → prefill again
	gw.Chat(Headers{}, chatBody(bigPrompt, "tercera"))
	if len(c.prefill) != 2 {
		t.Fatalf("after an engine miss the prefix must be re-admitted on its full size: %+v", c)
	}
	// and a genuine hit keeps it hot
	cacheN = 9000
	gw.Chat(Headers{}, chatBody(bigPrompt, "cuarta"))
	gw.Chat(Headers{}, chatBody(bigPrompt, "quinta"))
	if len(c.prefill) != 2 {
		t.Fatal("a real cache hit must keep the prefix hot")
	}
}

// A run of DECODE-DIRECT turns must keep g.last fresh, so when the conversation
// finally crosses the est_new threshold the exact recount recognises the resident
// prefix and stays decode-direct instead of shipping the whole thing to a
// prefill+handoff. Before the fix, decode-direct turns never tokenized, g.last
// stayed empty, bestPrefix returned ~0, and the crossing turn went large-new-prefill
// with prompt_n=1 — the engine held the conversation ENTIRE (2026-09-22, David's
// live turns: 33 s wasted over 3 turns reprocessing ~20-30k it already had).
func TestDecodeDirectTurnsKeepPrefixWarm(t *testing.T) {
	gw, c := newTestGW(t, func(o *Options) {
		o.Tokens = func(b Body) ([]int, error) {
			raw, _ := json.Marshal(b["messages"]) // shared content -> shared leading ids
			ids := make([]int, len(raw)/4)
			for i := range ids {
				h := 0
				for _, ch := range raw[4*i : 4*i+4] {
					h = h*31 + int(ch)
				}
				ids[i] = 1000 + h%50000
			}
			return ids, nil
		}
	})
	base := strings.Repeat("y", 22000) // ~5.5k est. tokens: turn 1 stays UNDER the 6144 threshold
	conv := func(extra string) Body {
		msgs := []any{
			map[string]any{"role": "system", "content": "s"},
			map[string]any{"role": "user", "content": base},
		}
		if extra != "" {
			msgs = append(msgs,
				map[string]any{"role": "assistant", "content": "ok"},
				map[string]any{"role": "user", "content": extra})
		}
		return Body{"messages": msgs}
	}
	// turn 1: decode-direct (small-new-prefill), so Prepare does NOT tokenize.
	// Its Finish must tokenize + remember g.last so the next turn can recognise it.
	gw.Chat(Headers{}, conv(""))
	if a, _ := lastRecord(t, gw)["admission"].(string); a != "small-new-prefill" && a != "prefix-hot" {
		t.Fatalf("test premise: turn 1 must be decode-direct (got %q)", a)
	}
	pBefore := len(c.prefill)
	// turn 2: same conversation + a delta that pushes the whole prompt over exactMin
	// (8192). Warm g.last -> only the delta is new -> decode-direct. Cold -> the whole
	// ~9k counts as new -> large-new-prefill.
	gw.Chat(Headers{}, conv(strings.Repeat("z", 14000)))
	rec := lastRecord(t, gw)
	if len(c.prefill) != pBefore {
		t.Fatalf("crossing turn re-prefilled a resident conversation: prefill %d->%d, admission=%v new_tokens=%v",
			pBefore, len(c.prefill), rec["admission"], rec["new_tokens"])
	}
}

// An engine miss on the SHARED system prefix must not forget the CONVERSATION's
// own last-prompt record. It used to (g.last.forget(ckey) on any cache-cold
// reply), so the next turn's bestPrefix returned ~0, the whole still-resident
// conversation counted as new, and it was shipped to a full prefill+handoff of
// tens of thousands of tokens (2026-09-21, ckey 0cc88487: 40k recomputed, 27 s).
// The same conversation (stable ckey) must stay recognised across a miss.
func TestEngineMissKeepsConversationPrefix(t *testing.T) {
	cacheN := 20000.0
	gw, c := newTestGW(t, func(o *Options) {
		o.Tokens = func(b Body) ([]int, error) {
			raw, _ := json.Marshal(b["messages"]) // deterministic ids: shared content -> shared prefix
			ids := make([]int, len(raw)/4)
			for i := range ids {
				h := 0
				for _, ch := range raw[4*i : 4*i+4] {
					h = h*31 + int(ch)
				}
				ids[i] = 1000 + h%50000
			}
			return ids, nil
		}
		o.BackendCall = func(body Body, extra Headers) (Body, error) {
			return Body{"timings": map[string]any{"prompt_n": 1.0, "cache_n": cacheN}}, nil
		}
	})
	sysPrompt := strings.Repeat("s", 4000) // ~1000 tok: prefixToks>0 so the cold-prefix Forget can fire
	seed := strings.Repeat("y", 40000)     // stable first user turn -> stable ckey, big enough to route via prefill
	conv := func(tail string) Body {
		msgs := []any{
			map[string]any{"role": "system", "content": sysPrompt},
			map[string]any{"role": "user", "content": seed},
		}
		if tail != "" {
			msgs = append(msgs,
				map[string]any{"role": "assistant", "content": "ok"},
				map[string]any{"role": "user", "content": tail})
		}
		return Body{"messages": msgs}
	}
	gw.Chat(Headers{}, conv("")) // turn 1: establish + remember the conversation
	cacheN = 0                   // turn 2: engine reports a miss -> Finish forgets pkey (must KEEP ckey)
	gw.Chat(Headers{}, conv("a"))
	cacheN = 20000
	pBefore := len(c.prefill)
	gw.Chat(Headers{}, conv("ab")) // turn 3: continuation of the SAME conversation
	rec := lastRecord(t, gw)
	if nt, _ := rec["new_tokens"].(float64); nt > 8192 {
		t.Fatalf("miss forgot the conversation prefix: new_tokens=%v (bestPrefix went stale)", rec["new_tokens"])
	}
	if len(c.prefill) != pBefore {
		t.Fatalf("a continuation after a miss was re-prefilled whole: prefill %d -> %d", pBefore, len(c.prefill))
	}
}

// The registry cannot hold more hot prefixes than the engine has slots.
func TestHotPrefixRegistryBoundedBySlots(t *testing.T) {
	gw, c := newTestGW(t, func(o *Options) { o.NSlots = 2 })
	gw.Chat(Headers{}, chatBody(bigPrompt+"A", "x"))
	gw.Chat(Headers{}, chatBody(bigPrompt+"B", "x"))
	gw.Chat(Headers{}, chatBody(bigPrompt+"C", "x")) // evicts A
	gw.Chat(Headers{}, chatBody(bigPrompt+"A", "y")) // A must count as cold again → prefill
	if len(c.prefill) != 4 {
		t.Fatalf("with 2 slots the 4th request (evicted prefix) must prefill again: %d", len(c.prefill))
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

// toolsBody is the HUD's shape: a short system prompt, a large tool catalogue
// (what actually fills the prompt) and the conversation turns (user/assistant
// alternating).
func toolsBody(system string, ntools int, turns ...string) Body {
	tools := make([]any, 0, ntools)
	for i := 0; i < ntools; i++ {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "tool_" + strconv.Itoa(i),
				"description": strings.Repeat("d", 700),
				"parameters":  map[string]any{"type": "object"},
			},
		})
	}
	msgs := []any{map[string]any{"role": "system", "content": system}}
	for i, tt := range turns {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, map[string]any{"role": role, "content": tt})
	}
	return Body{"messages": msgs, "tools": tools}
}

// The HUD's prompt is mostly its tool catalogue: the chat template renders
// tools[] ahead of the conversation, so the engine caches them with the system
// prompt. Estimating the prefix from the system prompt alone made the admission
// threshold decide on ~1/4 of what the engine received (measured 2026-09-22 on
// David's live turns: est 4181 vs prompt_n 17412). RED before fix#5.
func TestPrefixEstimateCountsToolCatalogue(t *testing.T) {
	gw, c := newTestGW(t, nil)
	body := toolsBody("sys", 60, "hola")
	if _, err := gw.Chat(Headers{}, body); err != nil {
		t.Fatal(err)
	}
	rec := lastRecord(t, gw)
	// what the template renders: the catalogue as JSON (independent of the
	// gateway's own helper, so this test also compiles against the old code)
	toolsJSON, _ := json.Marshal(body["tools"])
	want := EstimateTokens(string(toolsJSON), nil)
	if got, _ := rec["prefix_toks"].(int); got < want {
		t.Fatalf("prefix_toks %d must count the tool catalogue (>= %d)", got, want)
	}
	// ~10k tokens of tools on a cold slot is a large new prompt: prefill +
	// handoff, exactly as the same tokens in a system prompt would be.
	if len(c.prefill) != 1 {
		t.Fatalf("cold tool-heavy prompt must go through prefill (%d)", len(c.prefill))
	}
}

// Once the catalogue is hot in the slot, the next turn is prefix-hot and stays
// decode-direct: the tools are prefix, not tail, so they are never counted as
// new again.
func TestToolCatalogueHotStaysDecodeDirect(t *testing.T) {
	gw, c := newTestGW(t, nil)
	gw.Chat(Headers{}, toolsBody("sys", 60, "hola"))
	gw.Chat(Headers{}, toolsBody("sys", 60, "hola", "respuesta", "otra corta"))
	if len(c.prefill) != 1 {
		t.Fatalf("second turn must not prefill again (%d)", len(c.prefill))
	}
	if adm := lastRecord(t, gw)["admission"]; adm != "prefix-hot" {
		t.Fatalf("second turn admission = %v, want prefix-hot", adm)
	}
	if _, ok := c.decode[1]["x-sofmat-kv-handoff"]; ok {
		t.Fatal("second turn must not carry a handoff marker")
	}
}

// The assistant's tool_calls (name + arguments) are rendered into the prompt
// like any content: the tail estimate must count them. RED before fix#5.
func TestTailCountsAssistantToolCalls(t *testing.T) {
	body := Body{"messages": []any{
		map[string]any{"role": "system", "content": "s"},
		map[string]any{"role": "user", "content": "u"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{
				"name": "f", "arguments": strings.Repeat("a", 400),
			}},
		}},
		map[string]any{"role": "tool", "content": "result"},
	}}
	if got := EstimateTokens(tailTextOf(body), nil); got < 100 {
		t.Fatalf("tail must count tool_calls arguments, got %d tokens", got)
	}
}

// The pool occupancy at admission is evidence, logged per request and never a
// decision input: a probe that answers is recorded, one that fails or panics
// just leaves the field out.
func TestPoolHeldAtAdmitLogged(t *testing.T) {
	gw, _ := newTestGW(t, func(o *Options) {
		o.PoolHeld = func() (int, bool) { return 12345, true }
	})
	gw.Chat(Headers{}, chatBody("sys", "hola"))
	if got := lastRecord(t, gw)["pool_held_at_admit"]; got != 12345 {
		t.Fatalf("pool_held_at_admit = %v, want 12345", got)
	}
	gw2, _ := newTestGW(t, func(o *Options) {
		o.PoolHeld = func() (int, bool) { panic("slots down") }
	})
	if _, err := gw2.Chat(Headers{}, chatBody("sys", "hola")); err != nil {
		t.Fatalf("a panicking probe must not fail the request: %v", err)
	}
	if _, ok := lastRecord(t, gw2)["pool_held_at_admit"]; ok {
		t.Fatal("a failed probe must leave the field out, not record a fake number")
	}
}

// After a decode turn the gateway LEARNS which slot served it (the engine picks
// by content; the ring only guessed) and checks the next turn's residency
// there. RED before fix#6: the record had no slot_engine and the second turn
// was still pinned/checked on the ring's pick.
func TestFinishLearnsEngineSlot(t *testing.T) {
	var seen []Headers // the mutate closure cannot see newTestGW's recorder
	gw, _ := newTestGW(t, func(o *Options) {
		o.BackendCall = func(body Body, extra Headers) (Body, error) {
			seen = append(seen, extra)
			c2 := okResp()
			c2["timings"] = map[string]any{"cache_n": 60.0, "prompt_n": 40.0}
			return c2, nil
		}
		o.SlotHolding = func(n int) (string, bool) {
			if n == 100 {
				return "1", true // the engine served this 100-token prompt in slot 1
			}
			return "", false
		}
	})
	sys := strings.Repeat("s", 4000)
	if _, err := gw.Chat(Headers{}, chatBody(sys, "hola")); err != nil {
		t.Fatal(err)
	}
	rec := lastRecord(t, gw)
	if rec["slot_engine"] != "1" {
		t.Fatalf("record must carry the engine's slot, got %v", rec["slot_engine"])
	}
	if rec["slot"] != "1" && rec["slot_mismatch"] != true {
		t.Fatalf("a slot different from the ring's pick must be flagged: %v", rec)
	}
	// the next turn of the SAME conversation is routed to the learned slot
	gw.Chat(Headers{}, Body{"messages": []any{
		map[string]any{"role": "system", "content": sys},
		map[string]any{"role": "user", "content": "hola"},
		map[string]any{"role": "assistant", "content": "qué tal"},
		map[string]any{"role": "user", "content": "sigue"},
	}})
	if len(seen) != 2 {
		t.Fatalf("both turns must reach the decode: %d", len(seen))
	}
	if got := seen[1]["x-sofmat-slot"]; got != "1" {
		t.Fatalf("next turn must go to the slot the engine used (1), got %q", got)
	}
}

func itoa(i int) string {
	return strings.TrimSpace(strings.Repeat(" ", 0) + string(rune('0'+i)))
}
