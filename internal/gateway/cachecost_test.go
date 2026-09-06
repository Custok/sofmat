package gateway

import (
	"errors"
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
