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
// chosen engine's budget waits for room instead of being rejected, and after
// waitBudget it is sent anyway (fail-open — never worse than before).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/Custok/sofmat/internal/gateway"
)

const (
	// budgetUse is the share of an engine's context we let in-flight requests
	// claim. The rest absorbs what they generate (a reply is KV too).
	budgetUse = 0.80
	// waitBudget is how long a request waits for room before going anyway.
	waitBudget = 20 * time.Second
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

// fits reports whether tok more tokens stay inside the usable budget.
func (n *decodeNode) fits(tok int) bool {
	_, cur := n.load()
	return float64(cur+tok) <= float64(n.budget)*budgetUse
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

// sessionKey identifies a conversation from the head of its messages: the
// system prompt plus the start of the first user turn. Stable across the turns
// that append to it, different between two clients working on different things.
func sessionKey(body gateway.Body) string {
	raw, _ := body["messages"]
	b, err := json.Marshal(raw)
	if err != nil || len(b) == 0 {
		return "default"
	}
	if len(b) > sessionKeyChars {
		b = b[:sessionKeyChars]
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// pick returns the engine that serves this request plus the release function to
// call once the reply is done. Sticky by conversation; new conversations go to
// the emptiest engine; a request that does not fit waits for room (fail-open).
func (b *decodeBalancer) pick(key string, estTokens int) (*decodeNode, func()) {
	if len(b.nodes) == 0 {
		return nil, func() {}
	}
	if estTokens < 0 {
		estTokens = 0
	}
	n := b.chooseNode(key, estTokens)
	deadline := time.Now().Add(waitBudget)
	for !n.fits(estTokens) && time.Now().Before(deadline) {
		// another engine with room right now beats waiting for this one
		if alt := b.freeNode(estTokens); alt != nil && alt != n {
			n = alt
			b.remember(key, n)
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	n.claim(estTokens)
	return n, func() { n.release(estTokens) }
}

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

// stats is what the panel/request log reports about the spread.
func (b *decodeBalancer) stats() []map[string]any {
	out := make([]map[string]any, 0, len(b.nodes))
	for _, n := range b.nodes {
		i, t := n.load()
		out = append(out, map[string]any{
			"name": n.name, "url": n.url, "budget": n.budget,
			"inflight": i, "tokens_inflight": t,
		})
	}
	return out
}
