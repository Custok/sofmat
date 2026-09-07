package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func seq(n, from int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = from + i
	}
	return out
}

func TestLastPromptsCommonPrefix(t *testing.T) {
	l := newLastPrompts(2)
	if l.commonPrefix("a", seq(10, 0)) != 0 {
		t.Fatal("unknown key → 0")
	}
	l.remember("a", seq(100, 0))
	if got := l.commonPrefix("a", append(seq(100, 0), 7, 8, 9)); got != 100 {
		t.Fatalf("extended prompt shares the full old prompt: %d", got)
	}
	if got := l.commonPrefix("a", append(seq(40, 0), seq(60, 500)...)); got != 40 {
		t.Fatalf("divergence at 40: %d", got)
	}
	if got := l.commonPrefix("a", seq(20, 0)); got != 20 {
		t.Fatalf("shorter prompt bounded by its own length: %d", got)
	}
	l.remember("b", seq(5, 0))
	l.remember("c", seq(5, 0)) // evicts a (capacity 2)
	if l.commonPrefix("a", seq(10, 0)) != 0 {
		t.Fatal("evicted key must read as cold")
	}
	l.forget("b")
	if l.commonPrefix("b", seq(5, 0)) != 0 {
		t.Fatal("forgotten key must read as cold")
	}
}

func TestCostModelDefaults(t *testing.T) {
	c := newCostModel()
	if c.prefillCheaper(11500, 11500) {
		t.Fatal("cold 11.5k: direct is cheaper with the measured defaults")
	}
	if !c.prefillCheaper(46000, 46000) {
		t.Fatal("cold 46k: the prefill path is cheaper (decode rate falls with context)")
	}
	if c.prefillCheaper(34000, 12000) {
		t.Fatal("34k with 22k hot: the decode only does 12k, prefill would redo 34k")
	}
	if c.prefillCheaper(0, 0) {
		t.Fatal("no tokens → never")
	}
}

func TestCostModelLearnsFromObservations(t *testing.T) {
	c := newCostModel()
	// a direct 12k request that the decode processed slowly → decode rate drops
	for i := 0; i < 8; i++ {
		c.observe(Record{}, Body{"timings": map[string]any{"prompt_n": 12000.0, "prompt_ms": 20000.0}}, false)
	}
	if !c.prefillCheaper(12000, 12000) {
		t.Fatalf("after observing 600 tok/s on the decode the prefill must win: %+v", c.ppDecode)
	}
	// a handed-off request that measured a fast prefill and cheap handoff
	c2 := newCostModel()
	c2.observe(Record{"tokens": 46000, "prefill_pp": 3000.0, "save_ms": 200.0, "fetch_ms": 300.0, "restore_ms": 100.0}, Body{}, true)
	if c2.ppPrefill[1] != 3000 {
		t.Fatalf("first observation replaces the default: %v", c2.ppPrefill[1])
	}
	if c2.handoffPerTk <= 0 || c2.handoffPerTk > 0.045 {
		t.Fatalf("handoff per token must move toward the observed 0.009 ms: %v", c2.handoffPerTk)
	}
	// tiny direct prompts must not pollute the decode rate
	c3 := newCostModel()
	c3.observe(Record{}, Body{"timings": map[string]any{"prompt_n": 50.0, "prompt_ms": 5000.0}}, false)
	if c3.seenDec[0] {
		t.Fatal("prompts under 2k tokens are noise for the rate")
	}
}

func TestModeValidation(t *testing.T) {
	base := Options{
		Verify:         func(Headers) bool { return true },
		BackendCall:    func(Body, Headers) (Body, error) { return okResp(), nil },
		StatusProvider: func() Body { return Body{} },
	}
	if _, err := New(base); err != nil {
		t.Fatal(err)
	}
	o := base
	o.Mode = "sometimes"
	if _, err := New(o); err == nil {
		t.Fatal("unknown mode must be rejected")
	}
	o = base
	o.Mode = ModeAuto
	if _, err := New(o); err == nil {
		t.Fatal("auto without a busy probe must be rejected")
	}
	o.DecodeBusy = func() bool { return false }
	gw, err := New(o)
	if err != nil || gw.Mode() != ModeAuto {
		t.Fatalf("auto with probe: %v %v", err, gw)
	}
	base.DecodeBusy = func() bool { return true }
	if gw, _ := New(base); gw.Mode() != ModeBusy {
		t.Fatal("a probe without a mode defaults to busy")
	}
}

// Tokens (ids) supersedes CountTokens and makes the estimate cache-aware: the
// second turn of a conversation only counts the tokens after the common prefix.
func TestCacheAwareNewTokens(t *testing.T) {
	turn := 1
	gw, c := newTestGW(t, func(o *Options) {
		o.Tokens = func(Body) ([]int, error) {
			if turn == 1 {
				return seq(10000, 0), nil
			}
			return append(seq(10000, 0), seq(600, 90000)...), nil
		}
	})
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if len(c.prefill) != 1 {
		t.Fatal("cold first turn must prefill")
	}
	turn = 2
	// a long tail (tool results) so the estimate alone would send it to prefill again
	gw.Chat(Headers{}, chatBody(bigPrompt, strings.Repeat("resultado de tool ", 2000)))
	if len(c.prefill) != 1 {
		t.Fatal("second turn (600 new tokens) must not prefill again")
	}
	rec := lastRecord(t, gw)
	if rec["admission"] != "cache-hot" || rec["new_tokens"] != 600 || rec["tokens"] != 10600 {
		t.Fatalf("record must show the delta: %v", rec)
	}
	// a cold key (other tenant) counts everything
	gw.Chat(Headers{"x-sofmat-tenant": "otro"}, chatBody(bigPrompt, "hola"))
	if len(c.prefill) != 2 {
		t.Fatal("another prefix key is cold and must prefill")
	}
}

func TestTokenizeErrorFallsBackToDecode(t *testing.T) {
	gw, c := newTestGW(t, func(o *Options) {
		o.Tokens = func(Body) ([]int, error) { return nil, errors.New("prefill down") }
	})
	resp, err := gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if err != nil || resp == nil || len(c.prefill) != 0 || len(c.decode) != 1 {
		t.Fatalf("tokenize errors degrade to decode-direct: %v %+v", err, c)
	}
	if lastRecord(t, gw)["prefill_error"] != "tokenize: prefill down" {
		t.Fatalf("cause must be recorded: %v", lastRecord(t, gw))
	}
}

// After a prefill-side failure the route stays closed for the breaker window:
// no tokenizer/prefill call is attempted (a dead host would cost a dial
// timeout per long request), and it reopens once the window passes.
func TestPrefillBreaker(t *testing.T) {
	calls := 0
	gw, c := newTestGW(t, func(o *Options) {
		o.Tokens = func(Body) ([]int, error) { calls++; return nil, errors.New("dial tcp: refused") }
		o.PrefillBreaker = 60 * time.Millisecond
	})
	// distinct tenants: each request is a cold prefix (else the 2nd would ride prefix-hot)
	t1, t2, t3 := Headers{"x-sofmat-tenant": "a"}, Headers{"x-sofmat-tenant": "b"}, Headers{"x-sofmat-tenant": "c"}
	gw.Chat(t1, chatBody(bigPrompt, "uno"))
	gw.Chat(t2, chatBody(bigPrompt, "dos"))
	if calls != 1 || len(c.decode) != 2 {
		t.Fatalf("second request inside the window must not dial the prefill: calls=%d decode=%d", calls, len(c.decode))
	}
	rec := lastRecord(t, gw)
	if rec["admission"] != "prefill-down" || rec["admitted_via"] != "decode" {
		t.Fatalf("breaker must be visible: %v", rec)
	}
	time.Sleep(80 * time.Millisecond)
	gw.Chat(t3, chatBody(bigPrompt, "tres"))
	if calls != 2 {
		t.Fatal("after the window the prefill must be retried")
	}
	// handoff failures trip it too
	gw2, _ := newTestGW(t, func(o *Options) {
		o.Handoff = func(string, string) (Body, error) { return nil, errors.New("kv-fetch: HTTP 503") }
	})
	gw2.Chat(t1, chatBody(bigPrompt, "uno"))
	gw2.Chat(t2, chatBody(bigPrompt, "dos"))
	if lastRecord(t, gw2)["admission"] != "prefill-down" {
		t.Fatal("a handoff failure must open the breaker")
	}
	// negative = never close
	gw3, c3 := newTestGW(t, func(o *Options) {
		o.Tokens = func(Body) ([]int, error) { return nil, errors.New("down") }
		o.PrefillBreaker = -1
	})
	gw3.Chat(t1, chatBody(bigPrompt, "uno"))
	gw3.Chat(t2, chatBody(bigPrompt, "dos"))
	if lastRecord(t, gw3)["admission"] == "prefill-down" || len(c3.decode) != 2 {
		t.Fatal("breaker disabled must retry every time")
	}
}

func TestAutoModeDecisions(t *testing.T) {
	busy := false
	mk := func(ids []int) (*Gateway, *calls) {
		return newTestGW(t, func(o *Options) {
			o.Mode = ModeAuto
			o.DecodeBusy = func() bool { return busy }
			o.Tokens = func(Body) ([]int, error) { return ids, nil }
		})
	}
	// idle + cold 11.5k → direct (cheaper)
	gw, c := mk(seq(11500, 0))
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if len(c.prefill) != 0 || lastRecord(t, gw)["admission"] != "decode-cheaper" {
		t.Fatalf("idle cold 11.5k must be direct: %v", lastRecord(t, gw))
	}
	// busy + cold 11.5k → prefill (protect the streams)
	busy = true
	gw, c = mk(seq(11500, 0))
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if len(c.prefill) != 1 {
		t.Fatal("busy decode must offload an 11.5k cold prompt")
	}
	// idle + cold 46k → prefill (cheaper end-to-end)
	busy = false
	gw, c = mk(seq(46000, 0))
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if len(c.prefill) != 1 {
		t.Fatalf("idle cold 46k must go through the prefill: %v", lastRecord(t, gw))
	}
}

// "No room in the decode" is not a prefill failure: the request goes direct,
// the reason is recorded, and the breaker stays CLOSED so the very next request
// tries the prefill again.
func TestSkipHandoffDoesNotOpenTheBreaker(t *testing.T) {
	calls := 0
	gw, c := newTestGW(t, func(o *Options) {
		o.PrefillCall = func(Body, Headers) (Body, error) {
			calls++
			return nil, fmt.Errorf("%w: el decode no tiene sitio (10096 libres, hacen falta 50019)", ErrSkipHandoff)
		}
	})
	gw.Chat(Headers{}, chatBody(bigPrompt, "uno"))
	rec := lastRecord(t, gw)
	if rec["admitted_via"] != "decode" || rec["admission"] != "handoff-skipped" {
		t.Fatalf("a skip must read as a plain decode request: %v", rec)
	}
	if rec["prefill_error"] != nil {
		t.Fatalf("a skip is not a prefill error: %v", rec["prefill_error"])
	}
	if s, _ := rec["handoff_skipped"].(string); !strings.Contains(s, "no tiene sitio") {
		t.Fatalf("the reason must be recorded: %v", rec["handoff_skipped"])
	}
	// breaker closed: the next request tries the prefill again
	gw.Chat(Headers{"x-sofmat-tenant": "otro"}, chatBody(bigPrompt, "dos"))
	if calls != 2 {
		t.Fatalf("the prefill must be retried immediately after a skip (calls=%d)", calls)
	}
	if len(c.decode) != 2 {
		t.Fatalf("both requests must reach the decode: %d", len(c.decode))
	}
}

// With two decode engines the restored state lands in the primary, so a
// conversation whose cache lives in the OTHER engine must not be handed off:
// it would leave its prefix behind and reprocess the whole prompt.
func TestHandoffVetoForOtherEngine(t *testing.T) {
	allowed := false
	gw, c := newTestGW(t, func(o *Options) {
		o.HandoffAllowed = func(Body) bool { return allowed }
	})
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if len(c.prefill) != 0 || len(c.decode) != 1 {
		t.Fatalf("a vetoed request goes straight to its own engine: %+v", c)
	}
	if rec := lastRecord(t, gw); rec["admission"] != "other-engine" || rec["admitted_via"] != "decode" {
		t.Fatalf("the veto must be visible in the record: %v", rec)
	}
	allowed = true
	gw.Chat(Headers{"x-sofmat-tenant": "otro"}, chatBody(bigPrompt, "hola"))
	if len(c.prefill) != 1 {
		t.Fatal("without the veto the prefill runs as usual")
	}
}

// Several clients of the same product send byte-identical system prompts. If
// the prefix key ignores the first real turn they all collapse into ONE key:
// one slot on the engine (each turn evicting the previous client's cache) and
// one shared last-prompt record, so a request reads as "cache-hot" against
// ANOTHER conversation's prompt and the decode reprocesses the whole thing.
func TestPrefixKeySeparatesConversationsSharingASystemPrompt(t *testing.T) {
	sys := strings.Repeat("eres un asistente muy detallado. ", 200)
	mk := func(q string) Body {
		return Body{"messages": []any{
			map[string]any{"role": "system", "content": sys},
			map[string]any{"role": "user", "content": q},
		}}
	}
	a := PrefixKey(sys+"\x00"+conversationSeed(mk("arregla el balanceador")), "")
	b := PrefixKey(sys+"\x00"+conversationSeed(mk("revisa el frigate")), "")
	if a == b {
		t.Fatal("two conversations sharing a system prompt must not share a slot")
	}
	// appending turns keeps the conversation on its slot
	cont := Body{"messages": []any{
		map[string]any{"role": "system", "content": sys},
		map[string]any{"role": "user", "content": "arregla el balanceador"},
		map[string]any{"role": "assistant", "content": "hecho"},
		map[string]any{"role": "user", "content": "y ahora publica"},
	}}
	if PrefixKey(sys+"\x00"+conversationSeed(cont), "") != a {
		t.Fatal("a later turn must stay on the same slot")
	}
	// and the tenant still separates
	if PrefixKey(sys+"\x00"+conversationSeed(mk("x")), "t1") == PrefixKey(sys+"\x00"+conversationSeed(mk("x")), "t2") {
		t.Fatal("tenants must stay separate")
	}
}

// Two clients of the same product share a byte-identical system prompt. Keyed
// on that alone they overwrite each other's last-prompt record, so a turn that
// the engine could continue in 6 s is measured against the OTHER conversation
// and shipped off to a 60 s prefill. Each conversation must keep its own record.
func TestConversationsDoNotPolluteEachOthersCache(t *testing.T) {
	sys := bigPrompt + " system compartido por los tres clientes"
	conv := func(first string, turns int) Body {
		// every turn carries a long tail so the cheap estimate never short-circuits
		// and the exact, cache-aware recount is what routes it (that recount is
		// also what writes the last-prompt record we are testing)
		msgs := []any{map[string]any{"role": "system", "content": sys},
			map[string]any{"role": "user", "content": first + ": " + bigPrompt}}
		for i := 0; i < turns; i++ {
			msgs = append(msgs,
				map[string]any{"role": "assistant", "content": "ok"},
				// a long tail (a tool result) so the cheap estimate cannot decide and
				// the exact, cache-aware recount is what routes the turn
				map[string]any{"role": "user", "content": fmt.Sprintf("resultado %d: %s", i, bigPrompt)})
		}
		return Body{"messages": msgs}
	}
	idsA1, idsB1 := append(seq(10000, 0), seq(20000, 100000)...), append(seq(10000, 0), seq(20000, 500000)...)
	idsA2 := append(append([]int{}, idsA1...), seq(600, 900000)...)

	gw, c := newTestGW(t, func(o *Options) {
		o.Tokens = func(b Body) ([]int, error) {
			raw, _ := json.Marshal(b["messages"])
			switch {
			case strings.Contains(string(raw), "conversacion A") && strings.Contains(string(raw), "resultado 0"):
				return idsA2, nil
			case strings.Contains(string(raw), "conversacion A"):
				return idsA1, nil
			default:
				return idsB1, nil
			}
		}
	})
	gw.Chat(Headers{}, conv("conversacion A", 0))
	gw.Chat(Headers{}, conv("conversacion B", 0)) // B's turn overwrites the shared record
	before := len(c.prefill)

	// A continues: 600 new tokens on top of ITS OWN previous prompt. Measured
	// against B's it would look like 20 000 new and be shipped to the prefill.
	gw.Chat(Headers{}, conv("conversacion A", 1))
	rec := lastRecord(t, gw)
	if rec["admission"] != "cache-hot" || rec["new_tokens"] != 600 {
		t.Fatalf("A's turn must ride ITS OWN cache, not be measured against B: %v", rec)
	}
	if len(c.prefill) != before {
		t.Fatalf("a 600-token continuation must not prefill again: %d → %d", before, len(c.prefill))
	}
}

// The bookkeeping records what the gateway ROUTED; it cannot see the engine
// evicting a slot to make room for somebody else. With 80k prompts only ONE
// conversation fits per engine, so eviction is the normal case — and believing
// the cache then costs a FULL reprocess on the decode (measured 2026-09-07:
// admission cache-hot, prompt_n 78 329, 184.5 s). Asking the engine turns that
// into a prefill on the other card instead.
func TestEvictedCacheIsNotBelieved(t *testing.T) {
	resident := true
	ids := append(seq(40000, 0), seq(300, 90000)...)
	gw, c := newTestGW(t, func(o *Options) {
		o.Tokens = func(Body) ([]int, error) { return ids, nil }
		o.CacheResident = func(_ Body, expect int) bool { return resident }
	})
	// first turn: cold, goes through the prefill and is recorded
	gw.Chat(Headers{}, chatBody(bigPrompt, "primera "+bigPrompt))
	before := len(c.prefill)

	// second turn while the engine still holds it: rides the cache
	// long tails throughout, so the cheap estimate never short-circuits and the
	// exact, cache-aware recount is what routes the turn
	gw.Chat(Headers{}, chatBody(bigPrompt, "segunda "+bigPrompt))
	if rec := lastRecord(t, gw); rec["admission"] != "cache-hot" {
		t.Fatalf("with the prefix resident the turn must ride the cache: %v", rec)
	}
	if len(c.prefill) != before {
		t.Fatal("a resident prefix must not be prefilled again")
	}

	// now the engine evicted it: the same turn must NOT be called cache-hot
	resident = false
	gw.Chat(Headers{}, chatBody(bigPrompt, "tercera "+bigPrompt))
	rec := lastRecord(t, gw)
	if rec["cache_evicted"] != true {
		t.Fatalf("an evicted prefix must be recorded as such: %v", rec)
	}
	if rec["admission"] == "cache-hot" {
		t.Fatalf("an evicted prefix must not read as cache-hot: %v", rec)
	}
	if rec["new_tokens"] != len(ids) {
		t.Fatalf("an evicted prefix makes the WHOLE prompt new: %v", rec["new_tokens"])
	}
	if len(c.prefill) == before {
		t.Fatal("the reprocess must go through the prefill node, not the decode")
	}
}

// The chain-of-thought is not free twice over: those tokens are generated at
// decode speed AND occupy KV while they are produced, so a long deliberation
// both costs minutes and pushes the conversation towards the engine's ceiling.
// Measured on the fleet: the same answer took 52 generated tokens with
// reasoning and 4 without; live, one turn spent 124 s of its 126 s generating
// 9 557 tokens.
func TestNoThinkIsInjectedBeforeTokenizing(t *testing.T) {
	var seenByTokenizer, seenByDecode Body
	gw, c := newTestGW(t, func(o *Options) {
		o.NoThink = true
		o.Tokens = func(b Body) ([]int, error) { seenByTokenizer = b; return seq(20000, 0), nil }
	})
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	seenByDecode = c.bodies[len(c.bodies)-1]

	want := map[string]any{"enable_thinking": false}
	for name, b := range map[string]Body{"tokenizer": seenByTokenizer, "decode": seenByDecode} {
		got, _ := b["chat_template_kwargs"].(map[string]any)
		if got == nil || got["enable_thinking"] != want["enable_thinking"] {
			t.Fatalf("%s must see the template flag: %v", name, b["chat_template_kwargs"])
		}
	}

	// a client with an opinion keeps it
	gw2, c2 := newTestGW(t, func(o *Options) { o.NoThink = true })
	body := chatBody(bigPrompt, "hola")
	body["chat_template_kwargs"] = map[string]any{"enable_thinking": true}
	gw2.Chat(Headers{}, body)
	got, _ := c2.bodies[0]["chat_template_kwargs"].(map[string]any)
	if got["enable_thinking"] != true {
		t.Fatalf("an explicit request must not be overridden: %v", got)
	}

	// and off by default, nothing is injected
	gw3, c3 := newTestGW(t, nil)
	gw3.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if _, ok := c3.bodies[0]["chat_template_kwargs"]; ok {
		t.Fatal("without the option the body must be left alone")
	}
}

// The client sizes max_tokens as if it were alone and cannot know how much of
// the engine its own prompt just took. Asking for more than what is left makes
// llama-server abort the connection outright: 0 bytes, "unexpected EOF", and
// the client reports an answer with no choices. Live: a prompt of 82 296
// tokens plus a declared reply of 16 384 = 98 680 of a 100 096 engine.
func TestReplyIsClampedToWhatTheEngineHasLeft(t *testing.T) {
	const budget = 100096
	gw, c := newTestGW(t, func(o *Options) {
		o.Tokens = func(Body) ([]int, error) { return seq(82296, 0), nil }
		o.ReplyRoom = func(n int) int { return budget - n - 3072 }
	})
	body := chatBody(bigPrompt, "hola")
	body["max_tokens"] = float64(16384)
	gw.Chat(Headers{}, body)

	sent := c.bodies[len(c.bodies)-1]
	got := intField(sent, "max_tokens")
	want := budget - 82296 - 3072 // 14 728
	if got != want {
		t.Fatalf("the reply must be clamped to the room left: got %d, want %d", got, want)
	}
	if got+82296+3072 > budget {
		t.Fatalf("prompt + reply + template must fit the engine: %d", got+82296+3072)
	}
	rec := lastRecord(t, gw)
	if rec["max_tokens_clamped"] != want || rec["max_tokens_asked"] != 16384 {
		t.Fatalf("the clamp must be visible in the record: %v", rec)
	}

	// a reply that already fits is left alone
	gw2, c2 := newTestGW(t, func(o *Options) {
		o.Tokens = func(Body) ([]int, error) { return seq(20000, 0), nil }
		o.ReplyRoom = func(n int) int { return budget - n - 3072 }
	})
	small := chatBody(bigPrompt, "hola")
	small["max_tokens"] = float64(4096)
	gw2.Chat(Headers{}, small)
	if intField(c2.bodies[len(c2.bodies)-1], "max_tokens") != 4096 {
		t.Fatal("a reply that fits must not be touched")
	}
}
