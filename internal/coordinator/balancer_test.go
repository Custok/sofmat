package coordinator

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Custok/sofmat/internal/config"
	"github.com/Custok/sofmat/internal/gateway"
)

func balTestNodes(n int) []struct{ Name, URL string } {
	out := make([]struct{ Name, URL string }, n)
	for i := range out {
		out[i] = struct{ Name, URL string }{fmt.Sprintf("decode%d", i), fmt.Sprintf("http://e%d", i)}
	}
	return out
}

func bodyWith(system, user string) gateway.Body {
	return gateway.Body{"messages": []any{
		map[string]any{"role": "system", "content": system},
		map[string]any{"role": "user", "content": user},
	}}
}

// A conversation must keep going to the same engine: the next turn re-sends the
// whole prompt and only the engine that served the previous turn still has it.
func TestSessionSticksToItsEngine(t *testing.T) {
	b := newDecodeBalancer(balTestNodes(2))
	body := bodyWith("eres un asistente", strings.Repeat("contexto del repo A. ", 200))
	k := sessionKey(body)

	first, done, _ := b.pick(k, 1000)
	done()
	for i := 0; i < 5; i++ {
		n, d, _ := b.pick(k, 1000)
		d()
		if n != first {
			t.Fatalf("turn %d moved to another engine (%s != %s)", i, n.name, first.name)
		}
	}
	// the key survives appending to the conversation (the head is what identifies it)
	grown := bodyWith("eres un asistente", strings.Repeat("contexto del repo A. ", 200)+"y ahora otra pregunta")
	if sessionKey(grown) != k {
		t.Fatal("appending to a conversation must not change its session key")
	}
}

// Two different clients must not stack on the same engine.
func TestNewSessionsSpreadAcrossEngines(t *testing.T) {
	b := newDecodeBalancer(balTestNodes(2))
	a, doneA, _ := b.pick(sessionKey(bodyWith("s", "proyecto A "+strings.Repeat("x", 500))), 40000)
	c, doneC, _ := b.pick(sessionKey(bodyWith("s", "proyecto B "+strings.Repeat("y", 500))), 40000)
	if a == c {
		t.Fatalf("two fresh conversations landed on the same engine (%s)", a.name)
	}
	doneA()
	doneC()
	for _, n := range b.nodes {
		if i, tk := n.load(); i != 0 || tk != 0 {
			t.Fatalf("engine %s not released: inflight=%d tokens=%d", n.name, i, tk)
		}
	}
}

// A request that does not fit the busy engine goes to the one with room instead
// of waiting (and never to an engine that would blow its budget).
func TestBudgetGuardPrefersTheEngineWithRoom(t *testing.T) {
	b := newDecodeBalancer(balTestNodes(2))
	// fill engine 0 to its usable budget
	b.nodes[0].claim(int(float64(b.nodes[0].budget) * budgetUse))
	key := sessionKey(bodyWith("s", "conversacion pegada al motor 0"))
	b.remember(key, b.nodes[0]) // sticky to the full engine
	n, done, _ := b.pick(key, 30000)
	defer done()
	if n != b.nodes[1] {
		t.Fatalf("a request that does not fit must move to the engine with room, got %s", n.name)
	}
}

func TestBudgetProbeReadsRealContext(t *testing.T) {
	b := newDecodeBalancer(balTestNodes(1))
	b.probeBudgets(func(url string) (map[string]any, error) {
		return map[string]any{"default_generation_settings": map[string]any{"n_ctx": float64(65536)}}, nil
	})
	if b.nodes[0].budget != 65536 {
		t.Fatalf("budget = %d, want 65536", b.nodes[0].budget)
	}
	// a failing probe keeps the default
	b2 := newDecodeBalancer(balTestNodes(1))
	b2.probeBudgets(func(string) (map[string]any, error) { return nil, fmt.Errorf("down") })
	if b2.nodes[0].budget != defaultCtx {
		t.Fatal("a failed probe must keep the default budget")
	}
}

func TestConcurrentPicksAccountCorrectly(t *testing.T) {
	b := newDecodeBalancer(balTestNodes(2))
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n, done, _ := b.pick(fmt.Sprintf("k%d", i), 500)
			_ = n
			done()
		}(i)
	}
	wg.Wait()
	for _, n := range b.nodes {
		if i, tk := n.load(); i != 0 || tk != 0 {
			t.Fatalf("engine %s leaked: inflight=%d tokens=%d", n.name, i, tk)
		}
	}
}

func TestDecodeEndpointsFromConfig(t *testing.T) {
	cfg := &config.Config{Instances: []config.Instance{
		{Key: "decode", Role: "decode", Endpoint: "http://a:8090"},
		{Key: "prefill", Role: "prefill", Endpoint: "http://b:8081"},
		{Key: "decode2", Role: "decode", Endpoint: "http://b:8081"},
		{Key: "decode3", Role: "decode", Endpoint: "http://c:8090"},
	}}
	got := decodeEndpointsFrom(cfg)
	if len(got) != 3 || got[0].URL != "http://a:8090" {
		t.Fatalf("decode list wrong: %+v", got)
	}
	if got[1].URL != "http://b:8081" || got[2].URL != "http://c:8090" {
		t.Fatalf("order/dedup wrong: %+v", got)
	}
	// prefill alone is not a decode target
	only := decodeEndpointsFrom(&config.Config{Instances: []config.Instance{
		{Key: "prefill", Role: "prefill", Endpoint: "http://b:8081"}}})
	if len(only) != 0 {
		t.Fatalf("prefill must not be a decode engine: %+v", only)
	}
}

// End to end through the real server: two engines, two conversations, each one
// served by a different engine and each staying on its own.
func TestServerBalancesTwoDecodeEngines(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	mk := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/props") {
				writeTestJSON(w, 200, map[string]any{"default_generation_settings": map[string]any{"n_ctx": 100096.0}})
				return
			}
			mu.Lock()
			hits[name]++
			mu.Unlock()
			writeTestJSON(w, 200, map[string]any{
				"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": name}}},
				"timings": map[string]any{"prompt_n": 10.0, "cache_n": 0.0},
			})
		}))
	}
	e1, e2 := mk("uno"), mk("dos")
	defer e1.Close()
	defer e2.Close()

	cfg := &config.Config{Instances: []config.Instance{
		{Key: "decode", Role: "decode", Endpoint: e1.URL},
		{Key: "decode2", Role: "decode", Endpoint: e2.URL},
	}}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(s.Handler())
	defer gw.Close()

	convA := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "proyecto A " + strings.Repeat("a", 400)}}}
	convB := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "proyecto B " + strings.Repeat("b", 400)}}}
	seen := map[string]string{}
	for _, c := range []struct {
		name string
		body map[string]any
	}{{"A", convA}, {"B", convB}, {"A", convA}, {"B", convB}, {"A", convA}} {
		code, data := postChat(t, gw.URL, c.body)
		if code != 200 {
			t.Fatalf("chat %s failed: %d %s", c.name, code, data)
		}
		var out struct {
			Choices []struct {
				Message struct{ Content string } `json:"message"`
			} `json:"choices"`
		}
		_ = json.Unmarshal(data, &out)
		served := out.Choices[0].Message.Content
		if prev, ok := seen[c.name]; ok && prev != served {
			t.Fatalf("conversation %s moved engine: %s -> %s", c.name, prev, served)
		}
		seen[c.name] = served
	}
	if seen["A"] == seen["B"] {
		t.Fatalf("both conversations landed on the same engine (%s); hits=%v", seen["A"], hits)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["uno"] == 0 || hits["dos"] == 0 {
		t.Fatalf("traffic not spread: %v", hits)
	}
}

// With a single decode configured nothing changes: no balancing, no stats.
func TestSingleEngineUnchanged(t *testing.T) {
	e := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, 200, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}}}})
	}))
	defer e.Close()
	s, err := NewServer(&config.Config{Instances: []config.Instance{{Key: "decode", Role: "decode", Endpoint: e.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	if s.bal.Len() != 1 {
		t.Fatalf("one engine expected, got %d", s.bal.Len())
	}
	gw := httptest.NewServer(s.Handler())
	defer gw.Close()
	if code, _ := postChat(t, gw.URL, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hola"}}}); code != 200 {
		t.Fatalf("single-engine chat failed: %d", code)
	}
}

// The prefill engine ingests several prompts at once, but never more than its
// KV budget allows: serializing everything queued cold prompts behind each
// other while 3 of its 4 slots idled.
func TestPrefillConcurrencyBoundedByBudget(t *testing.T) {
	k := newKVPipe("http://p", "http://d", "http://pc", "http://dc")
	k.setEngineBudget("http://p", 100096) // usable: 80 076

	s1, r1, ok1 := k.acquireSlot("http://p", 40000)
	s2, r2, ok2 := k.acquireSlot("http://p", 40000)
	if !ok1 || !ok2 {
		t.Fatal("two 40k prompts must fit together in a 100k engine")
	}
	if s1 == s2 {
		t.Fatalf("each concurrent prefill needs its own slot (%d == %d)", s1, s2)
	}
	// a third does not fit: it must wait, not pile on
	done := make(chan bool, 1)
	go func() {
		_, r3, ok3 := k.acquireSlot("http://p", 40000)
		done <- ok3
		r3()
	}()
	select {
	case <-done:
		t.Fatal("a third 40k prompt must wait for room, not be admitted")
	case <-time.After(400 * time.Millisecond):
	}
	r1() // frees 40k
	select {
	case ok3 := <-done:
		if !ok3 {
			t.Fatal("once there is room the waiting prompt must be admitted")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the waiting prompt was never woken up")
	}
	r2()
	if len(k.poolOf("http://p").free) != prefillSlots || k.poolOf("http://p").inflight != 0 {
		t.Fatalf("slots/tokens leaked: free=%d inflight=%d", len(k.poolOf("http://p").free), k.poolOf("http://p").inflight)
	}
}

// A prompt bigger than the whole usable budget still runs (alone), instead of
// being rejected for ever.
func TestOversizedPrefillRunsAlone(t *testing.T) {
	k := newKVPipe("http://p", "http://d", "http://pc", "http://dc")
	k.setEngineBudget("http://p", 100096)
	_, rel, ok := k.acquireSlot("http://p", 95000)
	if !ok {
		t.Fatal("a prompt larger than the usable budget must still run when the engine is free")
	}
	rel()
}

// The decode must be able to host the state BEFORE the prefill is spent: the
// live failure was 27 s of prefill thrown away by "No available space in KV
// cache", after which the decode re-processed the whole 50k prompt.
//
// What counts as "no room" is only the KV the engine cannot give away — the
// slots that are GENERATING. An idle slot's cache is erased right before the
// restore, so counting it refuses work the engine could serve (measured
// 2026-09-07: two engines holding ~55k each of FINISHED conversations, nothing
// generating, and every request queued the full wait and got refused).
func TestDecodeRoomCountsOnlyGeneratingSlots(t *testing.T) {
	var busy bool
	dec := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slots" {
			writeTestJSON(w, 200, []any{
				map[string]any{"id": 0, "is_processing": busy, "n_prompt_tokens": 90000.0},
				map[string]any{"id": 1, "is_processing": false, "n_prompt_tokens": 0.0},
			})
			return
		}
		writeTestJSON(w, 200, map[string]any{})
	}))
	defer dec.Close()

	k := newKVPipe("http://prefill", dec.URL, "http://pc", "http://dc")
	k.setEngineBudget(dec.URL, 100096)

	// 90k sitting in an IDLE slot is reclaimable: the whole budget is free
	free, evict, _, ok := k.decodeRoom(k.decodeURL)
	if !ok || free != 100096 {
		t.Fatalf("idle cache must not count: free = %d (ok=%v)", free, ok)
	}
	if evict == "" {
		t.Fatal("an idle slot must be offered for eviction")
	}

	// the same 90k while that slot GENERATES is committed KV: no room for 50k
	busy = true
	free, _, _, ok = k.decodeRoom(k.decodeURL)
	if !ok || free != 10096 {
		t.Fatalf("a generating slot holds its KV: free = %d (ok=%v), want 10096", free, ok)
	}
	if free >= 50000 {
		t.Fatal("a 50k state must not be considered to fit")
	}
}

// When no engine has room the request is REFUSED, not sent: sending it blows
// the unified budget and llama-server then fails every concurrent request.
func TestFullEngineRefusesInsteadOfOverloading(t *testing.T) {
	b := newDecodeBalancer(balTestNodes(1))
	b.nodes[0].claim(int(float64(b.nodes[0].budget) * budgetUse)) // engine full
	orig := waitBudgetForTest
	waitBudgetForTest = 300 * time.Millisecond
	defer func() { waitBudgetForTest = orig }()

	n, done, err := b.pick("k", 40000)
	done()
	if err == nil || n != nil {
		t.Fatalf("a full engine must refuse, got node=%v err=%v", n, err)
	}
	if !errors.Is(err, ErrEngineFull) {
		t.Fatalf("the refusal must be identifiable: %v", err)
	}
	// once there is room again it is admitted
	b.nodes[0].release(int(float64(b.nodes[0].budget) * budgetUse))
	if _, d, err := b.pick("k", 40000); err != nil {
		t.Fatalf("with room free it must be admitted: %v", err)
	} else {
		d()
	}
}

// Copilot asks for 16k of output it rarely uses; reserving all of it would let
// a single client fill the engine.
func TestReplyReserveIsCapped(t *testing.T) {
	big := gateway.Body{"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("x", 40000)}},
		"max_tokens": float64(16000)}
	got := estBodyTokens(big)
	if got > 10000+replyReserve+200 {
		t.Fatalf("reservation must cap the reply at %d tokens, got %d", replyReserve, got)
	}
	small := gateway.Body{"messages": []any{map[string]any{"role": "user", "content": "hola"}}, "max_tokens": float64(100)}
	if estBodyTokens(small) > 200 {
		t.Fatalf("a small request must reserve little: %d", estBodyTokens(small))
	}
}

// The admission guard must count what the engine's slots HOLD, not only what
// this gateway has in flight. Measured 2026-09-07: the decode held 84 021 of
// 100 096 tokens with a single 36k request in flight; counting only the request
// admitted a second 40k prompt, llama-server ran out of unified KV and failed
// every concurrent request at once.
func TestFitsCountsRealOccupancy(t *testing.T) {
	b := newDecodeBalancer([]struct{ Name, URL string }{{"decode", "http://x"}})
	held := 84021
	b.setOccupancy(func(string) int { return held })
	n := b.nodes[0]
	if n.fits(40000) {
		t.Fatalf("84k held + 40k must not fit in %d", n.budget)
	}
	held = 60000
	n.invalidateHeld()
	if !n.fits(10000) {
		t.Fatal("60k held + 10k stays under 80% of the budget")
	}
	held = 0
	n.invalidateHeld()
	if !n.fits(40000) {
		t.Fatal("an empty engine takes the 40k")
	}
	// a probe that cannot read /slots (-1) keeps the last reading instead of
	// declaring the engine empty
	held = 70000
	n.invalidateHeld()
	n.heldTokens()
	b.setOccupancy(func(string) int { return -1 })
	n.invalidateHeld()
	if n.fits(40000) {
		t.Fatal("an unreadable /slots must not reset the occupancy to zero")
	}
}

// pick must refuse when the engine is full by occupancy alone (nothing in
// flight): that is precisely the state that was admitting the failing requests.
func TestPickRefusesOnOccupancyAlone(t *testing.T) {
	old := waitBudgetForTest
	waitBudgetForTest = 200 * time.Millisecond
	defer func() { waitBudgetForTest = old }()
	b := newDecodeBalancer([]struct{ Name, URL string }{{"decode", "http://x"}})
	b.setOccupancy(func(string) int { return 84021 })
	if _, _, err := b.pick("s", 40000); !errors.Is(err, ErrEngineFull) {
		t.Fatalf("must refuse: %v", err)
	}
}

// Two clients with the SAME (long) system prompt but different questions are
// different conversations and must be able to land on different engines.
// Hashing the head of the messages array hashed only the system prompt, so the
// three VS Code windows collapsed into one key and one engine.
func TestSessionKeySkipsTheSystemPrompt(t *testing.T) {
	sys := map[string]any{"role": "system", "content": strings.Repeat("eres un asistente muy detallado. ", 200)}
	mk := func(q string) gateway.Body {
		return gateway.Body{"messages": []any{sys, map[string]any{"role": "user", "content": q}}}
	}
	a, b := sessionKey(mk("arregla el balanceador")), sessionKey(mk("revisa el frigate"))
	if a == b {
		t.Fatal("two conversations sharing a system prompt must not share a key")
	}
	// the same conversation keeps its key as turns are appended
	cont := gateway.Body{"messages": []any{sys,
		map[string]any{"role": "user", "content": "arregla el balanceador"},
		map[string]any{"role": "assistant", "content": "hecho"},
		map[string]any{"role": "user", "content": "y ahora publica"},
	}}
	if sessionKey(cont) != a {
		t.Fatal("appending turns must not move the conversation to another engine")
	}
	// a bare system prompt still yields a stable key
	only := gateway.Body{"messages": []any{sys}}
	if sessionKey(only) == "" || sessionKey(only) != sessionKey(only) {
		t.Fatal("a system-only request needs a stable key")
	}
}

// Waiting for prefill room must never cost more than the prefill path saves.
// Measured: a 34 409-token request waited the full 45 s queue for an engine
// that was busy generating, gave up, and the decode did the work in 22 s — 76 s
// total for 31 s of work, chasing a ~4 s saving.
func TestPrefillGivesUpFastWhenTheEngineIsBusy(t *testing.T) {
	if prefillWait > 8*time.Second {
		t.Fatalf("the prefill queue (%s) must stay below what the handoff saves", prefillWait)
	}
	k := newKVPipe("http://p", "http://d", "http://pc", "http://dc")
	k.setEngineBudget("http://p", 100096)
	_, rel, ok := k.acquireSlot("http://p", 80000) // engine effectively full
	if !ok {
		t.Fatal("the first prompt must be admitted")
	}
	defer rel()
	t0 := time.Now()
	if _, _, ok := k.acquireSlot("http://p", 40000); ok {
		t.Fatal("a prompt that does not fit must not be admitted")
	}
	if el := time.Since(t0); el > prefillWait+3*time.Second {
		t.Fatalf("giving up took %s, expected about %s", el, prefillWait)
	}
}

// Symmetric routing: the prefill runs on the engine where the conversation does
// NOT live, and the state is restored into the engine that will serve it — in
// both directions. With the roles nailed to the config, conversations living in
// the second engine could not take the handoff at all: they were served direct
// on the very engine that was prefilling for everyone else (3 of 4 such
// requests came back empty, 52-160 s — live, 2026-09-07).
func TestPrefillRunsOnTheEngineTheConversationDoesNotLiveIn(t *testing.T) {
	b := newDecodeBalancer([]struct{ Name, URL string }{
		{"decode", "http://a"}, {"decode2", "http://b"}})
	b.setOccupancy(func(string) int { return 0 })
	ctl := map[string]string{"http://a": "http://actl", "http://b": "http://bctl"}

	k := newKVPipe("http://b", "http://a", "http://bctl", "http://actl")
	k.resolve = func(body gateway.Body) (kvRoute, bool) {
		dec := b.stickyNode(sessionKey(body))
		pre := b.otherThan(dec)
		if dec == nil || pre == nil {
			return kvRoute{}, false
		}
		return kvRoute{prefillURL: pre.url, prefillCtl: ctl[pre.url],
			decodeURL: dec.url, decodeCtl: ctl[dec.url]}, true
	}

	mk := func(q string) gateway.Body {
		return gateway.Body{"messages": []any{map[string]any{"role": "user", "content": q}}}
	}
	seen := map[string]bool{}
	for _, q := range []string{"conversacion uno", "conversacion dos"} {
		body := mk(q)
		rt, err := k.routeFor(body)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if rt.prefillURL == rt.decodeURL {
			t.Fatalf("%s: prefill and decode must be different engines (%s)", q, rt.decodeURL)
		}
		if rt.prefillCtl != ctl[rt.prefillURL] || rt.decodeCtl != ctl[rt.decodeURL] {
			t.Fatalf("%s: each engine must carry its own agent: %+v", q, rt)
		}
		// the route must be stable for the same conversation
		if again, _ := k.routeFor(body); again != rt {
			t.Fatalf("%s: the route must not move between turns", q)
		}
		seen[rt.decodeURL] = true
	}
	if len(seen) != 2 {
		t.Fatalf("two new conversations must land in different engines, got %v", seen)
	}

	// and the handoff remembers where it restored, so the request is served there
	rt, _ := k.routeFor(mk("conversacion uno"))
	k.noteRoute("sf-x.bin", rt, 1000)
	if got := k.DecodeURLFor("sf-x.bin"); got != rt.decodeURL {
		t.Fatalf("a handed-off request must be served by the engine that restored it: %s vs %s", got, rt.decodeURL)
	}
	if got := k.DecodeURLFor("sf-desconocido.bin"); got != k.decodeURL {
		t.Fatal("an unknown state falls back to the configured decode")
	}
}

// A single engine has nobody to hand off to: saying so is better than shipping
// the state to the very engine that is going to serve the request.
func TestNoHandoffWhenBothEndsAreTheSameEngine(t *testing.T) {
	k := newKVPipe("http://same", "http://same", "http://c1", "http://c2")
	if _, err := k.routeFor(gateway.Body{}); !errors.Is(err, gateway.ErrSkipHandoff) {
		t.Fatalf("must skip, got %v", err)
	}
}
