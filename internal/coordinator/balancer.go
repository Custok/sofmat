package coordinator

// decodeBalancer — spreads chat traffic across the decode engines and keeps a
// conversation pinned to the engine that already holds its prompt cache.
//
// Why both halves matter, measured on the live fleet (2026-09-07, 3 VS Code
// clients against the gateway):
//
//   - Affinity: a client's turn re-sends the whole conversation (median 44k
//     tokens) but only ~650 of them are new — the engine that served the
//     previous turn still holds the rest. Moving that turn to another engine
//     turns a 1 s reply into a 30 s reprocess, so the session must stick.
//   - Spread: each engine has its OWN unified KV budget (100 096 tokens across
//     its 4 slots). With every session on one engine the budget blows and
//     llama-server then fails EVERY concurrent request (observed: the three
//     clients erroring at once, working again ~10 s later). Two engines = two
//     budgets, and new sessions land on the emptier one.
//
// The budget guard closes the remaining gap: a request that does not fit the
// chosen engine's budget waits for room, and if none appears within waitBudget
// it is REFUSED. Refusing one request is the right trade: sending it blows the
// unified budget and llama-server then fails every concurrent request at once
// (measured: three clients erroring together, working again ~10 s later).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Custok/sofmat/internal/gateway"
)

const (
	// budgetUse is the share of an engine's context we let in-flight requests
	// claim. The rest absorbs what they generate (a reply is KV too).
	budgetUse = 0.80
	// waitBudget is how long a request waits for room in the engine. A 46k
	// request takes ~40 s end to end, so three clients need a generous queue;
	// when it expires the request is REFUSED rather than sent, because sending
	// it blows the unified budget and llama-server then fails every concurrent
	// request at once (measured: three clients erroring together).
	waitBudget = 75 * time.Second // ver waitBudgetForTest
	// replyReserve caps how much of max_tokens is reserved as KV. Copilot asks
	// for 16k it almost never uses; reserving all of it would admit one client.
	replyReserve = 4096
	// stickyCap is how many conversations keep an engine assignment.
	stickyCap = 256
	// sessionKeyChars is how much of the conversation head identifies it: enough
	// to tell two clients apart, short enough to survive the turns that append.
	sessionKeyChars = 2048
	// defaultCtx is the assumed engine context when /props cannot be read.
	defaultCtx = 100096
)

type decodeNode struct {
	name   string
	url    string
	budget int // context tokens

	mu       sync.Mutex
	inflight int
	tokens   int // tokens claimed by in-flight requests

	// held is the KV the engine cannot give away: the tokens of the slots that
	// are GENERATING (see Server.engineHeldTokens). Counting only this gateway's
	// in-flight requests under-counts when another client is streaming; counting
	// idle slots' caches over-counts and refuses work the engine could serve.
	heldMu    sync.Mutex
	held      int
	heldAt    time.Time
	occupancy func(url string) int
}

// heldTokens returns the engine's real occupancy, re-read at most every second.
func (n *decodeNode) heldTokens() int {
	n.heldMu.Lock()
	defer n.heldMu.Unlock()
	if n.occupancy == nil {
		return 0
	}
	if time.Since(n.heldAt) < time.Second {
		return n.held
	}
	if v := n.occupancy(n.url); v >= 0 {
		n.held = v
		n.heldAt = time.Now()
	}
	return n.held
}

// invalidateHeld forces the next heldTokens to re-read (after claiming or
// releasing, the picture changed).
func (n *decodeNode) invalidateHeld() {
	n.heldMu.Lock()
	n.heldAt = time.Time{}
	n.heldMu.Unlock()
}

func (n *decodeNode) claim(tok int) {
	n.mu.Lock()
	n.inflight++
	n.tokens += tok
	n.mu.Unlock()
}

func (n *decodeNode) release(tok int) {
	n.mu.Lock()
	n.inflight--
	if n.inflight < 0 {
		n.inflight = 0
	}
	n.tokens -= tok
	if n.tokens < 0 {
		n.tokens = 0
	}
	n.mu.Unlock()
}

func (n *decodeNode) load() (inflight, tokens int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.inflight, n.tokens
}

// fits reports whether tok more tokens stay inside the usable budget, counting
// BOTH what this gateway has in flight and what the engine's slots still hold
// from finished conversations (that cache occupies the unified budget too; a
// slot the engine can reuse is only freed when it actually reuses it).
func (n *decodeNode) fits(tok int) bool {
	_, mine := n.load()
	used := mine
	if held := n.heldTokens(); held > used {
		// held already includes the prompts of my in-flight requests
		used = held
	}
	return float64(used+tok) <= float64(n.budget)*budgetUse
}

type decodeBalancer struct {
	nodes []*decodeNode

	mu     sync.Mutex
	sticky map[string]int // session key -> node index
	order  []string       // LRU, oldest first
	rr     int            // round-robin cursor for ties
}

func newDecodeBalancer(endpoints []struct{ Name, URL string }) *decodeBalancer {
	b := &decodeBalancer{sticky: map[string]int{}}
	for _, e := range endpoints {
		b.nodes = append(b.nodes, &decodeNode{name: e.Name, url: strings.TrimRight(e.URL, "/"), budget: defaultCtx})
	}
	return b
}

// Primary is the first configured decode: the engine a KV handoff restores
// into, so a handed-off request must be served there.
func (b *decodeBalancer) Primary() *decodeNode {
	if len(b.nodes) == 0 {
		return nil
	}
	return b.nodes[0]
}

func (b *decodeBalancer) Len() int { return len(b.nodes) }

// stickyIsPrimary reports whether this conversation is already assigned to an
// engine OTHER than the primary. A KV handoff restores into the primary, so
// such a conversation must not take that route: it would leave its cache
// behind and reprocess the whole prompt on the other engine.
func (b *decodeBalancer) stickyIsPrimary(key string) bool {
	if len(b.nodes) < 2 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	i, ok := b.sticky[key]
	return !ok || i == 0
}

// setOccupancy gives every engine the probe that reads what its slots hold.
func (b *decodeBalancer) setOccupancy(f func(url string) int) {
	for _, n := range b.nodes {
		n.heldMu.Lock()
		n.occupancy = f
		n.heldMu.Unlock()
	}
}

// probeBudgets reads each engine's real context size once (fail-soft: keeps the
// default when /props does not answer).
func (b *decodeBalancer) probeBudgets(get func(url string) (map[string]any, error)) {
	for _, n := range b.nodes {
		d, err := get(n.url + "/props")
		if err != nil {
			continue
		}
		gs, _ := d["default_generation_settings"].(map[string]any)
		if gs == nil {
			continue
		}
		if v, ok := gs["n_ctx"].(float64); ok && v > 0 {
			n.budget = int(v)
		}
	}
}

// sessionKey identifies a conversation by its first NON-system turn.
//
// It deliberately skips the system prompt. Copilot-style clients send a system
// prompt of several thousand characters, so hashing the head of the messages
// array hashed only that — identical across every client — and the three VS
// Code windows collapsed into ONE key, pinned to ONE engine, leaving the second
// engine idle (observed 2026-09-07: 13 requests, all on the primary, decode2 at
// zero). The first user turn is what actually differs between conversations,
// and it stays put as later turns are appended.
func sessionKey(body gateway.Body) string {
	msgs, _ := body["messages"].([]any)
	var head []byte
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		switch role, _ := mm["role"].(string); role {
		case "system", "developer":
			continue
		}
		if b, err := json.Marshal(mm); err == nil && len(b) > 0 {
			head = b
			return hashKey(head)
		}
	}
	// no user turn yet (a bare system prompt): fall back to the whole head, which
	// at least keeps such a request stable across retries
	b, err := json.Marshal(body["messages"])
	if err != nil || len(b) == 0 {
		return "default"
	}
	return hashKey(b)
}

func hashKey(b []byte) string {
	if len(b) > sessionKeyChars {
		b = b[:sessionKeyChars]
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// pick returns the engine that serves this request plus the release function to
// call once the reply is done. Sticky by conversation; new conversations go to
// the emptiest engine; a request that does not fit waits for room (fail-open).
func (b *decodeBalancer) pick(key string, estTokens int) (*decodeNode, func(), error) {
	if len(b.nodes) == 0 {
		return nil, func() {}, nil
	}
	if estTokens < 0 {
		estTokens = 0
	}
	n := b.chooseNode(key, estTokens)
	deadline := time.Now().Add(waitBudgetForTest)
	for !n.fits(estTokens) && time.Now().Before(deadline) {
		// another engine with room right now beats waiting for this one
		if alt := b.freeNode(estTokens); alt != nil && alt != n {
			n = alt
			b.remember(key, n)
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !n.fits(estTokens) {
		// still no room: refuse THIS request instead of blowing the engine's
		// budget and taking every other client's request down with it.
		i, held := n.load()
		return nil, func() {}, fmt.Errorf("%w: %s lleno (%d tokens en vuelo en %d peticiones, presupuesto %d); esta pide %d",
			ErrEngineFull, n.name, held, i, n.budget, estTokens)
	}
	n.claim(estTokens)
	n.invalidateHeld()
	return n, func() { n.release(estTokens); n.invalidateHeld() }, nil
}

// ErrEngineFull says no decode engine had room for this request within
// waitBudget. Refusing one request keeps the engine serving everyone else.
var ErrEngineFull = errors.New("motor de decode sin contexto disponible")

// waitBudgetForTest is the live queue deadline (a var so the tests can shorten it).
var waitBudgetForTest = waitBudget

// chooseNode applies the sticky assignment, or picks the least loaded engine.
func (b *decodeBalancer) chooseNode(key string, estTokens int) *decodeNode {
	b.mu.Lock()
	defer b.mu.Unlock()
	if i, ok := b.sticky[key]; ok && i < len(b.nodes) {
		b.touchLocked(key)
		return b.nodes[i]
	}
	best, bestIdx := b.nodes[0], 0
	bi, bt := best.load()
	for i := 1; i < len(b.nodes); i++ {
		ni, nt := b.nodes[i].load()
		if nt < bt || (nt == bt && ni < bi) {
			best, bestIdx, bi, bt = b.nodes[i], i, ni, nt
		}
	}
	// perfect tie (both idle): alternate, so two new sessions do not stack up
	if bt == 0 && bi == 0 {
		bestIdx = b.rr % len(b.nodes)
		best = b.nodes[bestIdx]
		b.rr++
	}
	b.setLocked(key, bestIdx)
	return best
}

// freeNode returns an engine with room for tok right now, or nil.
func (b *decodeBalancer) freeNode(tok int) *decodeNode {
	for _, n := range b.nodes {
		if n.fits(tok) {
			return n
		}
	}
	return nil
}

func (b *decodeBalancer) remember(key string, n *decodeNode) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, x := range b.nodes {
		if x == n {
			b.setLocked(key, i)
			return
		}
	}
}

func (b *decodeBalancer) setLocked(key string, idx int) {
	if _, ok := b.sticky[key]; !ok && len(b.sticky) >= stickyCap {
		oldest := b.order[0]
		b.order = b.order[1:]
		delete(b.sticky, oldest)
	}
	b.sticky[key] = idx
	b.touchLocked(key)
}

func (b *decodeBalancer) touchLocked(key string) {
	for i, o := range b.order {
		if o == key {
			b.order = append(b.order[:i], b.order[i+1:]...)
			break
		}
	}
	b.order = append(b.order, key)
}

// sessionsOn counts the conversations currently assigned to an engine: the
// number that shows whether the spread is actually happening.
func (b *decodeBalancer) sessionsOn(idx int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, i := range b.sticky {
		if i == idx {
			n++
		}
	}
	return n
}

// stats is what the panel/request log reports about the spread.
func (b *decodeBalancer) stats() []map[string]any {
	out := make([]map[string]any, 0, len(b.nodes))
	for idx, n := range b.nodes {
		i, t := n.load()
		out = append(out, map[string]any{
			"name": n.name, "url": n.url, "budget": n.budget,
			"inflight": i, "tokens_inflight": t, "tokens_held": n.heldTokens(),
			"sessions": b.sessionsOn(idx),
		})
	}
	return out
}
