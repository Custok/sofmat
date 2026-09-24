package gateway

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// With a tokenizer wired, the prefix (system + tools) is counted EXACTLY, once
// per prefix key, on a body that carries only the prefix — never re-counted on
// the next turn of the same conversation. RED before fix#5b: prefix_toks was
// the chars/4 estimate and no count ran.
func TestPrefixCountedExactlyOncePerPrefix(t *testing.T) {
	calls := 0 // prefix-only counts (the same counter also serves the stage-2 recount)
	gw, _ := newTestGW(t, func(o *Options) {
		o.CountTokens = func(body Body) (int, error) {
			msgs, _ := body["messages"].([]any)
			if len(msgs) != 1 {
				return 12345, nil // stage-2 exact recount of the whole prompt: not the prefix count
			}
			calls++
			if body["tools"] == nil {
				t.Fatal("the prefix count must carry the tool catalogue")
			}
			return 12000, nil
		}
	})
	body := toolsBody("sys", 20, "hola")
	gw.Chat(Headers{}, body)
	rec := lastRecord(t, gw)
	if got, _ := rec["prefix_toks"].(int); got != 12000 {
		t.Fatalf("prefix_toks must be the tokenizer's count (12000), got %v", rec["prefix_toks"])
	}
	if rec["prefix_exact"] != true {
		t.Fatalf("the record must say the prefix was counted exactly: %v", rec["prefix_exact"])
	}
	gw.Chat(Headers{}, toolsBody("sys", 20, "hola", "respuesta", "otra"))
	if calls != 1 {
		t.Fatalf("the same prefix must be counted once, counted %d times", calls)
	}
	if got, _ := lastRecord(t, gw)["prefix_toks"].(int); got != 12000 {
		t.Fatalf("second turn must reuse the cached exact count, got %d", got)
	}
}

// fix#10: what the slot verifiably holds of THIS conversation is credited
// against the estimate — but only for a request with the SAME SHAPE as the
// turn that built it (same estimated prefix, tail that only grew). An agentic
// turn whose history is mostly resident is then decode-direct instead of a
// prefill that rebuilds it; a closing call without tools[] (another prompt from
// the start) gets no credit. RED before: est_new counted the whole tail.
func TestResidentCreditOnlySameShape(t *testing.T) {
	gw, c := newTestGW(t, func(o *Options) {
		o.BackendCall = func(body Body, extra Headers) (Body, error) {
			r := okResp()
			// the engine reports it now holds the whole prompt (+ a short reply)
			pt := EstimateTokens(prefixTextOf(body), nil) + EstimateTokens(tailTextOf(body), nil)
			r["timings"] = map[string]any{"cache_n": 0.0, "prompt_n": float64(pt), "predicted_n": 20.0}
			return r, nil
		}
	})
	long := strings.Repeat("resultado de la herramienta ", 1100) // ~8k tokens of history
	turn1 := toolsBody("sys", 40, long)                          // ~9.4k prefix + 8k tail: cold -> prefill
	gw.Chat(Headers{}, turn1)
	if len(c.prefill) != 1 {
		t.Fatalf("test premise: turn 1 is a cold large prompt (prefill), got %d prefills", len(c.prefill))
	}
	// turn 2: same shape, the history grew by a short exchange (~250 tokens)
	turn2 := toolsBody("sys", 40, long, "respuesta", strings.Repeat("sigue ", 160))
	gw.Chat(Headers{}, turn2)
	rec := lastRecord(t, gw)
	if res, _ := rec["resident_toks"].(int); res <= 0 {
		t.Fatalf("turn 2 must be credited with the resident conversation: %v", rec)
	}
	if got, _ := rec["est_new_tokens"].(int); got >= 6144 {
		t.Fatalf("turn 2 est_new must be the growth only (< 6144), got %d", got)
	}
	if len(c.prefill) != 1 {
		t.Fatalf("turn 2 must stay decode-direct, got %d prefills", len(c.prefill))
	}
	// turn 3: same conversation but WITHOUT the catalogue: another prompt shape -> no credit
	turn3 := map[string]any{"messages": turn2["messages"]}
	gw.Chat(Headers{}, Body(turn3))
	rec = lastRecord(t, gw)
	if res, _ := rec["resident_toks"].(int); res != 0 {
		t.Fatalf("a call without tools[] renders another prompt: no credit, got %v", res)
	}
}

// kv_source says where the cached part of a turn's prompt came from, derived
// from what the coordinator already measures (the engine only reports cache_n):
// slot = the whole conversation came back and the pool held it at admit;
// ram = the whole conversation came back although the pool did NOT hold it
// (the engine's RAM prompt cache); lcp = only the shared catalogue prefix;
// none = nothing. Asked for on 2026-09-23 (debian): 3 of David's 17 turns came
// back cold and nobody could say whether the RAM cache ever rescues anything.
func TestFinishClassifiesKVSource(t *testing.T) {
	var cacheN float64
	poolHeld := 0
	engSlot := "2" // the engine slot that served (what /slots would show)
	engine := func(o *Options) {
		o.PoolHeld = func() (int, bool) { return poolHeld, true }
		o.SlotHolding = func(n int) (string, bool) { return engSlot, true }
		o.BackendCall = func(body Body, extra Headers) (Body, error) {
			r := okResp()
			pt := EstimateTokens(prefixTextOf(body), nil) + EstimateTokens(tailTextOf(body), nil)
			r["timings"] = map[string]any{"cache_n": cacheN, "prompt_n": float64(pt) - cacheN, "predicted_n": 20.0}
			return r, nil
		}
	}
	// decode-direct throughout (the HUD's threshold), so the source is the engine's
	gw, _ := newTestGW(t, func(o *Options) { engine(o); o.Threshold = 40000 })
	// a short first message (the conversation's identity) and a history that
	// grows ~6k tokens per turn, so a re-sliced history is a real shrink
	long := strings.Repeat("resultado de la herramienta ", 300)
	turn := func(n int) Body { return toolsBody("sys", 40, long, "respuesta", strings.Repeat("sigue ", 4000*n)) }
	total := func(b Body) int { // what the slot holds after serving b: the prompt + the 20-token reply
		return EstimateTokens(prefixTextOf(b), nil) + EstimateTokens(tailTextOf(b), nil) + 20
	}
	want := func(g *Gateway, step string, src string) {
		t.Helper()
		if got := lastRecord(t, g)["kv_source"]; got != src {
			t.Fatalf("%s: kv_source must be %q, got %v (record %v)", step, src, got, lastRecord(t, g))
		}
	}
	// turn 1: cold, nothing cached anywhere
	cacheN, poolHeld = 0, 0
	gw.Chat(Headers{}, turn(1))
	want(gw, "turn 1 cold", "none")
	// turn 2: the engine hands back the whole conversation and the pool held it
	cacheN, poolHeld = float64(total(turn(1))), total(turn(1))+5000
	gw.Chat(Headers{}, turn(2))
	want(gw, "turn 2 resident", "slot")
	// turn 3: the whole conversation comes back although the pool held almost nothing
	cacheN, poolHeld = float64(total(turn(2))), 100
	gw.Chat(Headers{}, turn(3))
	want(gw, "turn 3 restored from RAM", "ram")
	// turn 4: only the catalogue prefix was reused, by the SAME slot that holds
	// the conversation: the prompt diverged inside its own conversation (the HUD
	// re-sliced the history: id 80 of 2026-09-23), not a copy found elsewhere
	cacheN, poolHeld = float64(EstimateTokens(prefixTextOf(turn(4)), nil)), 60000
	gw.Chat(Headers{}, turn(4))
	want(gw, "turn 4 prefix only, own slot", "lcp-self")
	// turn 4b: the same prefix-only reuse served by ANOTHER slot (a copy)
	engSlot = "3"
	gw.Chat(Headers{}, turn(4))
	want(gw, "turn 4b prefix only, another slot", "lcp")
	// turn 5: the HUD re-sliced the history (id 12 -> 19 of 2026-09-23: the tail
	// shrank from 47k to 1.4k) yet the slot served everything the shorter prompt
	// shares with it: that is the slot, not a prefix-only hit
	short := toolsBody("sys", 40, long, "respuesta", "sigue")
	cacheN, poolHeld = float64(total(short)-40), 60000 // all but the last few tokens of the shorter prompt
	gw.Chat(Headers{}, short)
	want(gw, "turn 5 shrunken prompt served from its slot", "slot")
	// a handoff (prefill route): the COORDINATOR restored the state from a file
	// (id 18 of 2026-09-23: n_restored 51 817). Not the engine's RAM cache, and
	// the pool at admit did not include it either — it must not count as "ram".
	gw2, c2 := newTestGW(t, engine)
	cacheN, poolHeld = 0, 0
	gw2.Chat(Headers{}, turn(1))
	if len(c2.prefill) != 1 {
		t.Fatalf("test premise: a cold large prompt hands off, got %d prefills", len(c2.prefill))
	}
	cacheN, poolHeld = float64(total(turn(1))), 100 // the restore lands after admit
	gw2.Chat(Headers{}, turn(2))
	if lastRecord(t, gw2)["admitted_via"] == "prefill" {
		want(gw2, "handoff turn", "handoff")
	} else {
		want(gw2, "decode turn after a handoff, whole conversation back with an empty pool", "ram")
	}
}

// The resident total of the conversation (when verifiably still in its slot)
// is credited against the whole prompt: a 15k prefix + 47k tail of which the
// slot holds 43.6k is ~19k of new work, not 47k (measured 2026-09-23 23:05:
// est 47 291 vs a threshold of 40 000). Without a resident figure the old rule
// holds; the credit never goes negative.
func TestAdmissionCreditsResidentConversation(t *testing.T) {
	d := ClassifyAdmission(AdmissionInput{PrefixTokens: 15000, TailTokens: 47000,
		HotPrefixTokens: 15000, ResidentTokens: 43600, Threshold: 40000, PrefillAvailable: true})
	if d.Route != "decode" || d.EstNewTokens != 18400 {
		t.Fatalf("resident conversation must be new-work only: %+v", d)
	}
	d = ClassifyAdmission(AdmissionInput{PrefixTokens: 15000, TailTokens: 47000,
		HotPrefixTokens: 15000, Threshold: 40000, PrefillAvailable: true})
	if d.Route != "prefill" || d.EstNewTokens != 47000 {
		t.Fatalf("without a resident figure the tail counts as new (old rule): %+v", d)
	}
	d = ClassifyAdmission(AdmissionInput{PrefixTokens: 15000, TailTokens: 100,
		HotPrefixTokens: 15000, ResidentTokens: 20000, Threshold: 40000, PrefillAvailable: true})
	if d.EstNewTokens != 0 || d.Route != "decode" {
		t.Fatalf("a shrunken prompt is not negative work: %+v", d)
	}
}

func itoa(i int) string {
	return strings.TrimSpace(strings.Repeat(" ", 0) + string(rune('0'+i)))
}

// fix#16: a declared conversation id (X-Sofmat-Conversation) keys the
// conversation instead of the first message. A client that sends a sliding
// window of history changes its first message every time the window's anchor
// jumps, and the same conversation is born again for the gateway: no residency
// credit, no learned slot, and the fix#15 gate never sees the overlap it exists
// for (David's HUD, 2026-09-24 09:31: window of 16 advancing by 8; the real
// overlap arrived under two keys and reprocessed 18k tokens, 19.6 s). With the
// header, two bodies whose first messages differ share the key, the row says
// where the key came from (ckey_src), and the gate catches the overlap.
func TestConversationHeaderKeysTheConversation(t *testing.T) {
	gw, _ := newTestGW(t, nil)
	a := chatBody("sys", "primer mensaje de la ventana")
	b := chatBody("sys", "otro primer mensaje: la ventana ha saltado")
	// without the header: two keys, keyed on the first message
	gw.Chat(Headers{}, a)
	ra := lastRecord(t, gw)
	gw.Chat(Headers{}, b)
	rb := lastRecord(t, gw)
	if ra["ckey"] == rb["ckey"] {
		t.Fatal("test premise: different first messages must key differently without the header")
	}
	if ra["ckey_src"] != "first-message" || rb["ckey_src"] != "first-message" {
		t.Fatalf("ckey_src must say the first message keyed it: %v / %v", ra["ckey_src"], rb["ckey_src"])
	}
	// with the header (canonical spelling, as the HTTP layer hands it over): one key
	gw.Chat(Headers{"X-Sofmat-Conversation": "hud-conv-42"}, a)
	ha := lastRecord(t, gw)
	gw.Chat(Headers{"x-sofmat-conversation": "hud-conv-42"}, b)
	hb := lastRecord(t, gw)
	if ha["ckey"] != hb["ckey"] {
		t.Fatalf("the declared conversation id must key both bodies alike: %v vs %v", ha["ckey"], hb["ckey"])
	}
	if ha["ckey_src"] != "header" || hb["ckey_src"] != "header" {
		t.Fatalf("ckey_src must say the header keyed it: %v / %v", ha["ckey_src"], hb["ckey_src"])
	}
	if ha["ckey"] == ra["ckey"] {
		t.Fatal("a declared id must not collide with the first-message key")
	}
	// and a different declared id is a different conversation, same first message
	gw.Chat(Headers{"X-Sofmat-Conversation": "hud-conv-43"}, a)
	if lastRecord(t, gw)["ckey"] == ha["ckey"] {
		t.Fatal("different declared ids must key differently")
	}
}

// With the header, the fix#15 gate catches an overlap whose bodies have
// different first messages (the sliding-window case): the second waits.
func TestConversationHeaderReachesTheGate(t *testing.T) {
	var calls, inflight, overlaps int32
	gw, _ := newTestGW(t, func(o *Options) {
		o.ConvWait = 2 * time.Second
		o.BackendCall = func(body Body, extra Headers) (Body, error) {
			if atomic.AddInt32(&inflight, 1) > 1 {
				atomic.AddInt32(&overlaps, 1)
			}
			defer atomic.AddInt32(&inflight, -1)
			if atomic.AddInt32(&calls, 1) == 1 {
				time.Sleep(300 * time.Millisecond)
			}
			r := okResp()
			r["timings"] = map[string]any{"cache_n": 0.0, "prompt_n": 10.0, "predicted_n": 5.0}
			return r, nil
		}
	})
	h := Headers{"X-Sofmat-Conversation": "hud-conv-7"}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = gw.Chat(h, chatBody("sys", "ventana en un sitio")) }()
	time.Sleep(50 * time.Millisecond)
	if _, err := gw.Chat(h, chatBody("sys", "ventana en OTRO sitio: primer mensaje distinto")); err != nil {
		t.Fatal(err)
	}
	rec := lastRecord(t, gw)
	wg.Wait()
	if w, _ := rec["wait_conv_ms"].(float64); w < 200 {
		t.Fatalf("with the header the overlap must be gated although the first messages differ, wait_conv_ms=%v", rec["wait_conv_ms"])
	}
	if atomic.LoadInt32(&overlaps) != 0 {
		t.Fatal("the two requests overlapped at the backend")
	}
}

// seq_admit is the ARRIVAL order. The row id is assigned when the row is
// written (at the end), so a slow request in flight makes the ids cross the
// arrival order — pairings "by the previous id" then go wrong (a fleet
// reviewer's finding of 2026-09-24 09:33: ids 49, 51, 50 by arrival).
func TestSeqAdmitFollowsArrival(t *testing.T) {
	// the FIRST backend call sleeps; a sync.Once would make the second caller
	// wait for it too and turn the finishing order into a coin flip
	var calls int32
	gw, _ := newTestGW(t, func(o *Options) {
		o.BackendCall = func(body Body, extra Headers) (Body, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				time.Sleep(300 * time.Millisecond)
			}
			return okResp(), nil
		}
	})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = gw.Chat(Headers{}, chatBody("sys", "lenta")) }()
	time.Sleep(50 * time.Millisecond)
	if _, err := gw.Chat(Headers{}, chatBody("sys", "rápida")); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	rows, _ := gw.Requests(Headers{}, 2)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	fast, slow := rows[0], rows[1] // the fast one finished first: lower id
	if fast["id"].(int) > slow["id"].(int) {
		t.Fatalf("test premise: the fast request must have the lower id: %v %v", fast["id"], slow["id"])
	}
	fs, ok1 := fast["seq_admit"].(uint64)
	ss, ok2 := slow["seq_admit"].(uint64)
	if !ok1 || !ok2 {
		t.Fatalf("seq_admit must be on every row: %v / %v", fast["seq_admit"], slow["seq_admit"])
	}
	if !(ss < fs) {
		t.Fatalf("the slow request arrived first: its seq_admit must be lower (slow %d, fast %d)", ss, fs)
	}
}

// fix#17: every row says what the client asked of the tools (tool_choice: the
// forced function's name, or the mode) and how many it offered, so "forced and
// ignored" (tool_choice = a name, tool_calls_n = 0) is measurable from outside
// the client (2026-09-24 10:00: a read was forced twice, the model called
// nothing and copied its own old reply; nobody could count that from the rows).
func TestToolChoiceRecorded(t *testing.T) {
	gw, _ := newTestGW(t, nil)
	cases := []struct {
		name   string
		choice any
		want   string
	}{
		{"forced function", map[string]any{"type": "function", "function": map[string]any{"name": "mis_pendientes"}}, "mis_pendientes"},
		{"required", "required", "required"},
		{"none", "none", "none"},
		{"absent", nil, ""},
	}
	for _, c := range cases {
		body := toolsBody("sys", 3, "hola "+c.name)
		if c.choice != nil {
			body["tool_choice"] = c.choice
		} else {
			delete(body, "tool_choice")
		}
		gw.Chat(Headers{}, body)
		rec := lastRecord(t, gw)
		if got, _ := rec["tool_choice"].(string); got != c.want {
			t.Fatalf("%s: tool_choice must be %q, got %v", c.name, c.want, rec["tool_choice"])
		}
		if n, _ := rec["tools_n"].(int); n != 3 {
			t.Fatalf("%s: tools_n must be 3, got %v", c.name, rec["tools_n"])
		}
	}
	// no catalogue at all: tools_n 0 and an explicit empty tool_choice
	gw.Chat(Headers{}, chatBody("sys", "sin herramientas"))
	rec := lastRecord(t, gw)
	if n, _ := rec["tools_n"].(int); n != 0 {
		t.Fatalf("tools_n must be 0 without a catalogue, got %v", rec["tools_n"])
	}
	if got, ok := rec["tool_choice"].(string); !ok || got != "" {
		t.Fatalf("tool_choice must be an explicit empty string when none was sent, got %v", rec["tool_choice"])
	}
}

// fix#18: the residency probe only sees the SIZE of the learned slot, so a
// conversation of the same catalogue sitting there passes for this one and
// the credit is wrong (2026-09-24 10:20, seq 131: 15 703 credited, 0 reused,
// after a bench pushed 47 conversations through the decode). Two guards: the
// pool as a bound at admission (if the whole pool holds fewer tokens than the
// conversation had, it cannot be there), and resident_wrong on the row when
// the engine reused less than half of what was credited, dropping the learned
// slot so the next turn is not credited on it again.
func TestResidentCreditBoundedByPoolAndMarkedWhenWrong(t *testing.T) {
	var cacheN float64
	poolHeld, poolKnown := 0, false
	gw, _ := newTestGW(t, func(o *Options) {
		o.Threshold = 40000
		o.PoolHeld = func() (int, bool) { return poolHeld, poolKnown }
		o.BackendCall = func(body Body, extra Headers) (Body, error) {
			r := okResp()
			pt := EstimateTokens(prefixTextOf(body), nil) + EstimateTokens(tailTextOf(body), nil)
			r["timings"] = map[string]any{"cache_n": cacheN, "prompt_n": float64(pt) - cacheN, "predicted_n": 20.0}
			return r, nil
		}
	})
	long := strings.Repeat("resultado de la herramienta ", 300)
	turn := func(n int) Body { return toolsBody("sys", 40, long, "respuesta", strings.Repeat("sigue ", 400*n)) }
	// turn 1: cold; every row carries resident_wrong, false explicit
	cacheN = 0
	gw.Chat(Headers{}, turn(1))
	if rec := lastRecord(t, gw); rec["resident_wrong"] != false {
		t.Fatalf("resident_wrong must be an explicit false on a turn that was not credited: %v", rec["resident_wrong"])
	}
	// turn 2: same shape, probe says resident, but the pool holds almost nothing -> no credit
	poolHeld, poolKnown = 100, true
	cacheN = 0
	gw.Chat(Headers{}, turn(2))
	if res, _ := lastRecord(t, gw)["resident_toks"].(int); res != 0 {
		t.Fatalf("a pool that cannot hold the conversation must void the credit, got resident_toks=%d", res)
	}
	// turn 3: pool large enough, credit given, but the engine reuses nothing -> wrong, marked
	poolHeld = 1_000_000
	cacheN = 0
	gw.Chat(Headers{}, turn(3))
	rec := lastRecord(t, gw)
	if res, _ := rec["resident_toks"].(int); res <= 0 {
		t.Fatalf("test premise: turn 3 must be credited (pool large, same shape), got %v", rec["resident_toks"])
	}
	if rec["resident_wrong"] != true {
		t.Fatalf("credited and not reused must be marked resident_wrong: %v (record %v)", rec["resident_wrong"], rec)
	}
	// turn 4: the engine reuses the whole conversation -> credited and right
	prev := EstimateTokens(prefixTextOf(turn(3)), nil) + EstimateTokens(tailTextOf(turn(3)), nil) + 20
	cacheN = float64(prev)
	gw.Chat(Headers{}, turn(4))
	if rec := lastRecord(t, gw); rec["resident_wrong"] != false {
		t.Fatalf("a credit the engine honoured must not be marked wrong: %v", rec["resident_wrong"])
	}
}

func TestConvSlotsForget(t *testing.T) {
	cs := newConvSlots(4)
	cs.set("a", "1")
	cs.set("b", "2")
	cs.forget("a")
	if cs.get("a") != "" || cs.get("b") != "2" {
		t.Fatalf("forget must drop only that conversation: a=%q b=%q", cs.get("a"), cs.get("b"))
	}
	cs.forget("missing") // no-op
	cs.set("a", "3")
	if cs.get("a") != "3" {
		t.Fatal("a forgotten conversation can be learned again")
	}
}

// fix#15: the requests of ONE conversation go to the decode one at a time.
// With kv_unified an overlapping request of the same conversation lands in
// another slot and reprocesses everything although its KV is in VRAM
// (reproduced 2026-09-24 01:29 by a fleet reviewer: overlap -> other slot -> cold,
// 8 of 8; David's id 185, 15.9k reprocessed, 10 s). The wait is recorded on
// every row (wait_conv_ms, 0 = did not wait), the cap has its own mark
// (wait_conv_capped), other conversations never wait, and OFF (the default)
// is the behaviour before the fix: overlap allowed, nothing recorded but 0.
func TestConversationRequestsAreSerialised(t *testing.T) {
	// a backend that blocks its FIRST call for 300 ms and counts overlapping calls
	build := func(wait time.Duration) (*Gateway, *int32) {
		var inflight, overlaps int32
		var first sync.Once
		gw, _ := newTestGW(t, func(o *Options) {
			o.ConvWait = wait
			o.BackendCall = func(body Body, extra Headers) (Body, error) {
				if atomic.AddInt32(&inflight, 1) > 1 {
					atomic.AddInt32(&overlaps, 1)
				}
				defer atomic.AddInt32(&inflight, -1)
				first.Do(func() { time.Sleep(300 * time.Millisecond) })
				r := okResp()
				r["timings"] = map[string]any{"cache_n": 0.0, "prompt_n": 10.0, "predicted_n": 5.0}
				return r, nil
			}
		})
		return gw, &overlaps
	}
	// two requests of the same conversation, the second 50 ms after the first
	pair := func(gw *Gateway, second Body) Record {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = gw.Chat(Headers{}, chatBody("sys", "hola")) }()
		time.Sleep(50 * time.Millisecond)
		if _, err := gw.Chat(Headers{}, second); err != nil {
			t.Fatal(err)
		}
		rec := lastRecord(t, gw) // the second's row: the first is still blocked, or finished before it
		wg.Wait()
		return rec
	}

	// ON: the second waits for the first, nothing overlaps
	gw, overlaps := build(2 * time.Second)
	rec := pair(gw, chatBody("sys", "hola"))
	if w, _ := rec["wait_conv_ms"].(float64); w < 200 {
		t.Fatalf("the second request of a conversation must wait for the first (~250 ms), got %v (record %v)", rec["wait_conv_ms"], rec)
	}
	if rec["wait_conv_capped"] != false {
		t.Fatalf("wait_conv_capped must be an explicit false when the wait completed: %v", rec["wait_conv_capped"])
	}
	if atomic.LoadInt32(overlaps) != 0 {
		t.Fatal("requests of the same conversation overlapped at the backend")
	}
	// ON: ANOTHER conversation (another first message) does not wait
	gw, _ = build(2 * time.Second)
	rec = pair(gw, chatBody("sys", "otra conversación"))
	if w, _ := rec["wait_conv_ms"].(float64); w != 0 {
		t.Fatalf("another conversation must not wait, got %v", rec["wait_conv_ms"])
	}
	// CAP: 100 ms cap, the first blocks 300 ms: the second goes on, capped, overlapping
	gw, overlaps = build(100 * time.Millisecond)
	rec = pair(gw, chatBody("sys", "hola"))
	if w, _ := rec["wait_conv_ms"].(float64); w < 80 || w >= 250 {
		t.Fatalf("a capped wait must be about the cap (100 ms), got %v", rec["wait_conv_ms"])
	}
	if rec["wait_conv_capped"] != true {
		t.Fatalf("the cap must be marked on the row: %v", rec["wait_conv_capped"])
	}
	if atomic.LoadInt32(overlaps) != 1 {
		t.Fatalf("after the cap the request must proceed (overlapping once), overlaps=%d", atomic.LoadInt32(overlaps))
	}
	// OFF (default): no wait, overlap happens — the behaviour before fix#15
	gw, overlaps = build(0)
	rec = pair(gw, chatBody("sys", "hola"))
	if w, _ := rec["wait_conv_ms"].(float64); w != 0 || rec["wait_conv_capped"] != false {
		t.Fatalf("off: wait_conv_ms must be 0 and capped false, got %v / %v", rec["wait_conv_ms"], rec["wait_conv_capped"])
	}
	if atomic.LoadInt32(overlaps) != 1 {
		t.Fatalf("off must not serialise (control), overlaps=%d", atomic.LoadInt32(overlaps))
	}
}
