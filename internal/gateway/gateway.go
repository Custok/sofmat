package gateway

// Gateway — composes policy + admission + status + request-log behind one
// authenticated surface. Every dependency (auth verify, backend call, prefill
// call, handoff driver, token counter, status provider) is injected, so the
// routing/policy logic is fully testable without a network or a live engine.
//
// The engine's real speedup lever (speculative n_max), the prefix-cache reuse
// (slot affinity) and the disaggregated admission (prefill node + KV handoff)
// are applied HERE as policy, so they persist across engine restarts.
//
// A request is handled in two halves so the streaming path can share them:
// Prepare (auth, admission, prefill + handoff, body/headers to send) and
// Finish (engine timings → request record). Chat is Prepare + backend + Finish.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrUnauthorized = errors.New("unauthorized")

// ErrSkipHandoff says the handoff was not attempted for a reason that is NOT a
// prefill failure (typically: the decode has no room for the state right now).
// The request is served decode-direct without opening the prefill breaker, so
// the next request tries the prefill again straight away.
var ErrSkipHandoff = errors.New("handoff skipped")

// ExactTokensHeader lleva el conteo EXACTO de tokens del prompt desde la
// admisión hasta quien elige motor, para que la decisión de capacidad no
// dependa de una estimación por bytes cuyo error no se puede acotar.
const ExactTokensHeader = "x-sofmat-exact-tokens"

type Headers map[string]string
type Body map[string]any

// Verify authenticates a request from its headers.
type Verify func(h Headers) bool

// BackendCall proxies a request to the decode engine.
type BackendCall func(body Body, extra Headers) (Body, error)

// PrefillCall runs the prompt on the dedicated prefill node and saves its KV
// state; it must return a body containing "handoff_id" (the state's name) once
// the state is ready to ship. Any numeric field it returns (prefill_ms,
// save_ms, tokens, state_bytes, ...) is copied into the request record.
type PrefillCall func(body Body, extra Headers) (Body, error)

// Handoff moves the saved state to the decode node and restores it into a
// decode slot — the requested one when idle, else an idle one (a restore into
// a slot that is generating waits for that stream to end); it returns its
// metrics (fetch_ms, restore_ms, n_restored) plus "slot" = the slot actually
// used, or an error. Owned by the transport module; the gateway sequences it.
type Handoff func(handoffID string, slot string) (Body, error)

// CountTokens returns the EXACT prompt token count of a chat body as the
// engine will see it (chat template applied). Optional: when set, a request
// the estimate admitted to prefill is re-checked against the exact floor.
type CountTokens func(body Body) (int, error)

// Tokens returns the exact prompt token ids (chat template applied). When
// set it supersedes CountTokens and enables the cache-aware estimate: the
// tokens the decode already holds for this prefix (the common prefix with the
// previous prompt routed under the same key) are not "new" work, so a multi-
// turn agent conversation is not re-prefilled from scratch on every turn.
type Tokens func(body Body) ([]int, error)

// Modes for the disaggregation decision once a prompt is admitted (exact new
// tokens ≥ the floor):
//
//	ModeBusy:   offload only while the decode is generating for others.
//	ModeAlways: offload every admitted prompt.
//	ModeAuto:   offload while busy (protect the streams); when idle, offload
//	            only if the cost model says the prefill path is faster
//	            (total/pp_prefill + handoff < new/pp_decode, EMAs per size).
const (
	ModeBusy   = "busy"
	ModeAlways = "always"
	ModeAuto   = "auto"
)

// DecodeBusy reports whether the decode engine is generating for OTHER
// requests right now. Optional: when set, the prefill route is taken only
// while the decode is busy — the handoff exists to keep a long prefill from
// stalling live token streams (measured ×5.8 interference); on an idle decode
// the direct path is faster (the restored state lacks the engine's speculative
// draft context, so post-handoff generation runs slower).
type DecodeBusy func() bool

// StatusProvider returns the /api/status document.
type StatusProvider func() Body

type Gateway struct {
	verify      Verify
	backend     BackendCall
	status      StatusProvider
	prefill     PrefillCall                  // optional; nil = no disaggregation
	handoff     Handoff                      // optional; nil = no disaggregation
	count       CountTokens                  // optional; nil = estimate only
	tokens      Tokens                       // optional; supersedes count, enables the cache-aware estimate
	busy        DecodeBusy                   // optional; nil = handoff whenever admitted
	allowed     func(Body) bool              // optional; false = this request must not be handed off
	resident    func(Body, int, string) bool // optional; false = the engine no longer holds that prefix in the given slot
	poolHeld    func() (int, bool)           // optional; what the decode's slots hold at admission (request log only)
	slotOf      func(int) (string, bool)     // optional; which decode slot holds a prompt of exactly n tokens (learns the real slot)
	noThink     bool                         // ask the template to skip the chain-of-thought
	replyRoom   func(int) int                // how big a reply the engine can still host
	mode        string                       // ModeBusy / ModeAlways / ModeAuto
	threshold   int                          // admission threshold on the ESTIMATE
	exactMin    int                          // floor on the EXACT count (when count != nil)
	ring        *Ring
	alpha       *AlphaEma
	log         *RequestLog
	known       *KnownPrefixes
	prefixExact *KnownPrefixes // per prefix key: the EXACT prefix token count (tokenizer), cached
	convLast    *convTurns     // per conversation: shape + tokens the decode slot held after its last turn
	last        *lastPrompts   // per prefix key: ids of the last prompt routed (cache lower bound)
	convSlot    *convSlots     // per conversation: the decode slot its KV actually lives in
	cost        *costModel     // measured pp / handoff EMAs for ModeAuto
	convWait    time.Duration  // fix#15: how long a request waits for its conversation's in-flight request (0 = off)
	convGate    *convGate

	// circuit breaker: after a prefill-side failure (tokenize, prefill, handoff)
	// the prefill route is skipped without contacting the node for breakerFor,
	// so a dead prefill host costs one dial timeout, not one per long request.
	breakerMu  sync.Mutex
	downUntil  time.Time
	breakerFor time.Duration
}

// Options for New. NSlots defaults to 4; KeepContent defaults to false;
// Threshold defaults to PrefillThresholdTokens (estimated tokens) and
// ExactMinTokens to PrefillExactMinTokens (exact tokens, only with CountTokens).
type Options struct {
	Verify         Verify
	BackendCall    BackendCall
	StatusProvider StatusProvider
	PrefillCall    PrefillCall
	Handoff        Handoff
	CountTokens    CountTokens
	Tokens         Tokens
	DecodeBusy     DecodeBusy
	// ReplyRoom answers how many tokens of reply the engine can still host for a
	// prompt of that many tokens. The client declares a max_tokens as if it were
	// alone, and it has no way to know how much of the engine its own prompt just
	// took; asking for more than what is left makes llama-server abort the
	// connection outright — 0 bytes, "unexpected EOF", and the client reports an
	// answer with no choices. Returning 0 disables the clamp.
	ReplyRoom func(promptTokens int) int

	// NoThink asks the chat template to skip the model's chain-of-thought. Those
	// tokens cost decode time AND occupy KV while they are produced, so they
	// push a long conversation towards the engine's ceiling twice over. A client
	// that sets chat_template_kwargs itself is left alone.
	NoThink bool

	// CacheResident asks whether the engine that will serve this request still
	// holds about that many tokens of its prefix. The bookkeeping here can only
	// say what the gateway ROUTED; it cannot see the engine evicting a slot to
	// make room for somebody else. Without this check a conversation whose cache
	// was evicted is admitted as cache-hot and the decode silently reprocesses
	// the whole prompt (measured 2026-09-07: 78 329 tokens, 184.5 s).
	// nil = assume resident (previous behaviour).
	CacheResident func(Body, int, string) bool

	// PoolHeld reports how many prompt tokens the decode's slots hold, all slots
	// together, at the moment a request is admitted. Evidence only: it is written
	// to the request log as pool_held_at_admit and never changes a decision. It
	// answers "what occupied the pool when this turn arrived?" without anyone
	// sampling /slots at the right second (a restored KV that is gone by the next
	// turn shows up as a drop between two consecutive rows). nil = not logged.
	PoolHeld func() (int, bool)

	// SlotHolding answers which decode slot holds a prompt of exactly that many
	// tokens right now ("" / false when none or more than one does). Finish asks
	// it after every decode turn with cache_n + prompt_n — the engine's own
	// n_prompt_tokens for the slot that served — to LEARN where the conversation
	// really lives. Since cache-hot no longer pins id_slot, the engine picks the
	// slot by content and the ring's pick is a guess: the residency probe then
	// read a slot the conversation was never in, called it evicted, counted the
	// whole resident prompt as new and only an idle decode saved the turn from a
	// handoff (measured 2026-09-23 22:16, five turns: known_hot 13 616,
	// hot_prefix 0, cache_n 15k, engine slot 1 vs recorded slot 3).
	SlotHolding func(promptTokens int) (slot string, ok bool)

	// HandoffAllowed vetoes the handoff for a request the caller knows must not
	// take it. With more than one decode engine the restored state lands in the
	// primary, so a conversation whose cache lives in ANOTHER engine must not be
	// handed off: it would be dragged across engines and lose its whole prefix.
	HandoffAllowed func(Body) bool
	Mode           string // ModeBusy (default when DecodeBusy is set), ModeAlways, ModeAuto
	Threshold      int
	// ConvWait serialises the requests of ONE conversation (fix#15): a request
	// whose conversation already has a request in flight waits up to ConvWait
	// for it to finish before the decode is dialled. 0 = off (default). Why: with
	// kv_unified the engine puts an overlapping request of the same conversation
	// in ANOTHER slot and cannot reuse its own KV although it is in VRAM
	// (reproduced 2026-09-24 01:29 by debian-dev: overlap -> other slot -> cold,
	// 8 of 8; David's id 185, 15.9k reprocessed, 10 s). Waiting removes the
	// overlap, the identified cause; it does not promise every cold turn away.
	ConvWait       time.Duration
	ExactMinTokens int
	NSlots         int
	KeepContent    bool
	// PrefillBreaker is how long the prefill route stays closed after a
	// prefill-side failure (0 = PrefillBreakerDefault; negative = never close).
	PrefillBreaker time.Duration
}

// PrefillBreakerDefault: a dead or ejected prefill node is retried every 30 s.
const PrefillBreakerDefault = 30 * time.Second

func New(o Options) (*Gateway, error) {
	if o.Verify == nil || o.BackendCall == nil || o.StatusProvider == nil {
		return nil, fmt.Errorf("verify, backend_call and status_provider are required")
	}
	n := o.NSlots
	if n <= 0 {
		n = 4
	}
	members := make([]string, n)
	for i := range members {
		members[i] = strconv.Itoa(i)
	}
	alpha, err := NewAlphaEma(0.3, 3)
	if err != nil {
		return nil, err
	}
	// The engine holds at most one sequence per slot, so at most NSlots prefixes
	// can be hot: a larger registry claims reuse the engine evicted long ago and
	// then routes a cold 16k prompt to the decode as "small-new-prefill"
	// (observed 2026-09-06: est 3.7k new, the engine processed 16.8k).
	// NOTE: keyed on the shared system prefix, NOT per conversation. Per
	// conversation is only right once the registry also knows WHICH engine holds
	// the prefix — hot in one engine is cold in the other — and the engine is
	// picked after admission today. Until then this stays shared and the
	// Forget-on-miss below corrects it after one bad guess.
	known, err := NewKnownPrefixes(n)
	if err != nil {
		return nil, err
	}
	th := o.Threshold
	if th <= 0 {
		th = PrefillThresholdTokens
	}
	em := o.ExactMinTokens
	if em <= 0 {
		em = PrefillExactMinTokens
	}
	mode := o.Mode
	switch mode {
	case ModeBusy, ModeAlways, ModeAuto:
	case "":
		mode = ModeAlways
		if o.DecodeBusy != nil {
			mode = ModeBusy
		}
	default:
		return nil, fmt.Errorf("unknown kv handoff mode %q (busy|always|auto)", mode)
	}
	if (mode == ModeBusy || mode == ModeAuto) && o.DecodeBusy == nil {
		return nil, fmt.Errorf("mode %s needs a DecodeBusy probe", mode)
	}
	breaker := o.PrefillBreaker
	if breaker == 0 {
		breaker = PrefillBreakerDefault
	}
	return &Gateway{
		breakerFor:  breaker,
		verify:      o.Verify,
		backend:     o.BackendCall,
		status:      o.StatusProvider,
		prefill:     o.PrefillCall,
		handoff:     o.Handoff,
		count:       o.CountTokens,
		tokens:      o.Tokens,
		busy:        o.DecodeBusy,
		allowed:     o.HandoffAllowed,
		resident:    o.CacheResident,
		poolHeld:    o.PoolHeld,
		slotOf:      o.SlotHolding,
		noThink:     o.NoThink,
		replyRoom:   o.ReplyRoom,
		mode:        mode,
		threshold:   th,
		exactMin:    em,
		ring:        NewRing(members, 64),
		alpha:       alpha,
		log:         NewRequestLog(500, o.KeepContent),
		known:       known,
		prefixExact: mustKnownPrefixes(64),
		convWait:    o.ConvWait,
		convGate:    newConvGate(),
		convLast:    newConvTurns(8 * n),
		last:        newLastPrompts(8 * n),
		convSlot:    newConvSlots(8 * n),
		cost:        newCostModel(),
	}, nil
}

// convSlots remembers which decode slot each conversation's KV lives in, so a
// continuing turn is checked and pinned to THAT slot instead of a freshly hashed
// one. Without it the ring re-derives a slot from the prefix key while the KV
// actually landed wherever the handoff restored it, so every turn checked the
// wrong slot, missed, and reprocessed the whole prompt. Bounded LRU: a stale
// entry is only a guess the residency probe corrects (a cold slot reads cold).
type convSlots struct {
	mu  sync.Mutex
	m   map[string]string
	ord []string
	cap int
}

func newConvSlots(capacity int) *convSlots {
	if capacity < 1 {
		capacity = 1
	}
	return &convSlots{m: map[string]string{}, cap: capacity}
}

func (c *convSlots) get(key string) string {
	if c == nil || key == "" {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[key]
}

func (c *convSlots) set(key, slot string) {
	if c == nil || key == "" || slot == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[key]; !ok {
		if len(c.ord) >= c.cap {
			delete(c.m, c.ord[0])
			c.ord = c.ord[1:]
		}
		c.ord = append(c.ord, key)
	}
	c.m[key] = slot
}

// Mode reports the disaggregation mode in force.
func (g *Gateway) Mode() string { return g.mode }

// Disaggregated reports whether a prefill node + handoff driver are wired.
func (g *Gateway) Disaggregated() bool { return g.prefill != nil && g.handoff != nil }

func (g *Gateway) auth(h Headers) error {
	if !g.verify(h) {
		return ErrUnauthorized
	}
	return nil
}

// Status handles GET /api/status.
func (g *Gateway) Status(h Headers) (Body, error) {
	if err := g.auth(h); err != nil {
		return nil, err
	}
	return g.status(), nil
}

// Requests handles GET /api/requests.
func (g *Gateway) Requests(h Headers, n int) ([]Record, error) {
	if err := g.auth(h); err != nil {
		return nil, err
	}
	return g.log.Tail(n), nil
}

// Plan is a prepared request: the body and headers to send to the decode
// engine (policy applied: n_max, slot, and — after a successful handoff — the
// engine-visible id_slot + cache_prompt so the restored KV is reused), plus
// the record fields Finish completes with the engine's timings.
type Plan struct {
	Body    Body
	Headers Headers

	fields     Record
	pkey       string
	ckey       string
	prefixToks int
	tailToks   int // estimated tail: with prefixToks, the SHAPE of this prompt (fix#10 credit)
	alphaKey   string
	viaPrefill bool
	tokenized  bool // Prepare already ran the tokenizer this turn (success or fail)
	gated      bool // holds its conversation's gate until the record is written (fix#15)
	start      time.Time
}

// Chat handles POST /api/chat: apply policy, proxy, log. Returns the backend
// response verbatim.
func (g *Gateway) Chat(h Headers, body Body) (Body, error) {
	p, err := g.Prepare(h, body)
	if err != nil {
		return nil, err
	}
	resp, err := g.backend(p.Body, p.Headers)
	if err != nil {
		p.fields["error"] = err.Error()
		g.finishRecord(p)
		return nil, err
	}
	g.Finish(p, resp)
	return resp, nil
}

// residentFor reports whether the engine that will serve this request still
// holds about expect tokens of its prefix. Unknown (no probe) = assume yes.
func (g *Gateway) residentFor(body Body, expect int, slot string) bool {
	if g.resident == nil || expect <= 0 {
		return true
	}
	ok := true
	func() {
		defer func() { _ = recover() }() // a probe must never take the request down
		ok = g.resident(body, expect, slot)
	}()
	return ok
}

// prefixTokens is the token count of the stable prefix (system prompt + tool
// catalogue, as the template renders them). With a tokenizer wired it is the
// EXACT count, asked once per prefix key and cached: the prefix is identical
// across a conversation's turns, so a new count only happens when the system
// prompt or the catalogue changes. Without one (or with the prefill side down)
// it is the chars/4 estimate, which under-counts the tool JSON by ~11 %
// (measured 2026-09-23: 13 616 estimated vs 15 350 rendered — 3.54 chars per
// token in Spanish JSON plus ~200 tokens of template scaffolding). The second
// result says which one it was.
func (g *Gateway) prefixTokens(pkey string, body Body) (int, bool) {
	est := EstimateTokens(prefixTextOf(body), nil)
	// Only a tool catalogue is worth a tokenizer round trip: on prose chars/4 is
	// within a percent, on tool JSON it is ~11 % short. Text-only prefixes keep
	// the estimate and never dial the tokenizer on the request path.
	if g.count == nil || pkey == "" || est == 0 || body["tools"] == nil || g.prefillDown() {
		return est, false
	}
	if n := g.prefixExact.HotTokens(pkey); n > 0 {
		return n, true
	}
	pb := Body{"messages": []any{}}
	if sp := systemPromptOf(body); sp != "" {
		pb["messages"] = []any{map[string]any{"role": "system", "content": sp}}
	}
	for _, k := range []string{"tools", "tool_choice", "chat_template_kwargs", "reasoning_format", "parallel_tool_calls"} {
		if v, ok := body[k]; ok {
			pb[k] = v
		}
	}
	n, err := g.countSafe(pb)
	if err != nil || n <= 0 {
		// a tokenizer hiccup is not a prefill failure: keep the estimate, do not
		// trip the breaker, try again next request.
		return est, false
	}
	g.prefixExact.Record(pkey, n)
	return n, true
}

// convTurn is what a conversation's last turn left in its decode slot, plus the
// shape of the prompt that built it (estimated prefix and tail), so the next
// turn is credited only when it continues THAT prompt.
type convTurn struct {
	prefix int // estimated prefix tokens of the prompt that built the slot
	tail   int // estimated tail tokens of that prompt
	total  int // tokens the slot held afterwards (cache_n + prompt_n + predicted_n)
}

// convTurns is a bounded map of ckey → convTurn (cleared wholesale when full:
// a lost entry only costs one conservative estimate).
type convTurns struct {
	mu  sync.Mutex
	cap int
	m   map[string]convTurn
}

func newConvTurns(capacity int) *convTurns {
	if capacity <= 0 {
		capacity = 32
	}
	return &convTurns{cap: capacity, m: map[string]convTurn{}}
}

func (c *convTurns) get(key string) (convTurn, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.m[key]
	return t, ok
}

func (c *convTurns) set(key string, t convTurn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.cap {
		c.m = map[string]convTurn{}
	}
	c.m[key] = t
}

// mustKnownPrefixes builds a registry with a capacity that is a constant here,
// so the only failure mode (capacity <= 0) cannot happen at runtime.
func mustKnownPrefixes(capacity int) *KnownPrefixes {
	k, err := NewKnownPrefixes(capacity)
	if err != nil {
		panic(err)
	}
	return k
}

// slotOfSafe: a probe error or panic just leaves the slot as it was.
func (g *Gateway) slotOfSafe(n int) (slot string, ok bool) {
	if g.slotOf == nil || n <= 0 {
		return "", false
	}
	defer func() {
		if r := recover(); r != nil {
			slot, ok = "", false
		}
	}()
	return g.slotOf(n)
}

// poolHeldSafe: a probe error or panic just leaves the field out of the record.
func (g *Gateway) poolHeldSafe() (held int, ok bool) {
	if g.poolHeld == nil {
		return 0, false
	}
	defer func() {
		if r := recover(); r != nil {
			held, ok = 0, false
		}
	}()
	return g.poolHeld()
}

// residentHot zeroes a "hot prefix" the engine no longer holds.
func (g *Gateway) residentHot(body Body, hot int, slot string) int {
	if hot > 0 && !g.residentFor(body, hot, slot) {
		return 0
	}
	return hot
}

// intField reads a numeric body field however the client encoded it.
func intField(b Body, key string) int {
	switch v := b[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

// conversationSeed is the first non-system turn of a conversation: what tells
// two clients apart when they share a system prompt, and what stays put as
// later turns are appended.
func conversationSeed(body Body) string {
	msgs, _ := body["messages"].([]any)
	first, turns := "", 0
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		switch role, _ := mm["role"].(string); role {
		case "system", "developer":
			continue
		}
		turns++
		if first != "" {
			continue
		}
		b, err := json.Marshal(mm["content"])
		if err != nil || len(b) == 0 {
			continue
		}
		if len(b) > conversationSeedChars {
			b = b[:conversationSeedChars]
		}
		first = string(b)
	}
	_ = turns
	return first
}

// conversationSeedChars is how much of that first turn identifies it: enough to
// tell two conversations apart, bounded so the key stays cheap.
const conversationSeedChars = 2048

// Prepare runs everything BEFORE the decode call: auth, slot affinity,
// admission, and — for a large new prompt with a prefill node wired — the
// prefill + KV handoff. Any prefill/handoff problem degrades to decode-direct
// (fail-soft): the request never fails because of the disaggregation.
func (g *Gateway) Prepare(h Headers, body Body) (*Plan, error) {
	if err := g.auth(h); err != nil {
		return nil, err
	}
	start := time.Now()

	tenant := h["x-sofmat-tenant"]
	if tenant == "" {
		tenant = h["X-Sofmat-Tenant"]
	}
	systemPrompt := systemPromptOf(body)
	// slot affinity: the same CONVERSATION to the same slot, so its KV survives
	// to the next turn. The key must include the first real turn: several
	// clients of the same product send byte-identical system prompts, so keying
	// on the system prompt alone gave all of them one key — one slot on the
	// engine, each turn evicting the previous client's cache — and one shared
	// last-prompt record, so a request was judged "cache-hot" against ANOTHER
	// conversation's prompt and the decode then reprocessed everything
	// (measured 2026-09-07: admission cache-hot with prompt_n = 73 536, 67.6 s).
	pkey := PrefixKey(systemPrompt, tenant)
	// ckey identifies the CONVERSATION, not just the shared system prompt.
	// Several clients of the same product send byte-identical system prompts, so
	// under pkey alone they overwrite each other's last-prompt record and every
	// turn is then measured against ANOTHER conversation's prompt: the cache the
	// engine really holds becomes invisible and a 6 s turn is sent off to a 60 s
	// prefill (measured 2026-09-07 with three VS Code clients, median 29.5 s).
	ckey := pkey
	if seed := conversationSeed(body); seed != "" {
		ckey = PrefixKey(systemPrompt+"\x00"+seed, tenant)
	}
	// slot affinity: reuse the slot this conversation's KV already lives in, so it
	// does not rotate between turns (the ring only seeds it on the first turn).
	slot := g.convSlot.get(ckey)
	if slot == "" {
		s, err := g.ring.Route(pkey)
		if err != nil {
			return nil, err
		}
		slot = s
	}
	// fix#15: one request of a conversation at a time. Taken BEFORE the
	// residency probes so that, once through, the slot is idle and holds the
	// conversation and the admission sees it. Always recorded: wait_conv_ms (0 =
	// did not wait) and wait_conv_capped (the cap ran out and the request went
	// on anyway, overlapping — its own value, never confused with "did not
	// wait"). Released when the record is written, on every path.
	waitConv, capped := g.convGate.acquire(ckey, g.convWait)
	planned := false
	defer func() {
		if !planned {
			g.convGate.release(ckey)
		}
	}()

	// admission: decode-direct, or dedicated prefill + KV handoff first.
	// The prefix is what the ENGINE sees ahead of the conversation: system prompt
	// + tool catalogue (prefixTextOf), not the system prompt alone — counted with
	// the engine's tokenizer once per prefix when one is wired (fix#5b), else
	// estimated.
	prefixToks, prefixExact := g.prefixTokens(pkey, body)
	tailToks := EstimateTokens(tailTextOf(body), nil)
	// hotPrefix hoisted to a var so /api/requests can log it alongside pkey/ckey:
	// the estimate that decides prefill-vs-decode is opaque without these, and the
	// 2026-09-21 diagnosis (resident conversations routed to a 21 s handoff) turns
	// on whether HotTokens(pkey) reflects THIS conversation or another one that
	// shares the system prompt. Behaviour is unchanged: same call, same value.
	knownHot := g.known.HotTokens(pkey)
	hotPrefix := g.residentHot(body, knownHot, slot)
	// fix#10: tokens of THIS conversation the slot verifiably still holds from
	// its last turn (prefix + history + reply). Without it the whole tail counted
	// as new and an agentic turn whose tool results pile up in the history was
	// shipped to a prefill that rebuilt 51 817 tokens when the decode lacked
	// 8 226 (2026-09-23 23:05, 41 s). Unknown or not resident = 0 (as before).
	// Credited ONLY when this request has the SAME SHAPE as the turn that left
	// those tokens — same estimated prefix (a closing call without tools[]
	// renders another prompt from the start: 2 415 vs 13 616, ids 18/25) and a
	// tail that only grew (append-only) — and the learned slot still holds
	// ≥ 0.8× of them. Occupancy alone is NOT reuse (id 25: 52 724 held, 2 473
	// matched); a mismatch credits nothing and the estimate is as before.
	resident := 0
	if ct, ok := g.convLast.get(ckey); ok && ct.total > 0 && ct.prefix == prefixToks && tailToks >= ct.tail &&
		g.residentFor(body, ct.total, slot) {
		resident = ct.total
	}
	decision := ClassifyAdmission(AdmissionInput{
		PrefixTokens:     prefixToks,
		TailTokens:       tailToks,
		HotPrefixTokens:  hotPrefix,
		ResidentTokens:   resident,
		Threshold:        g.threshold,
		PrefillAvailable: g.Disaggregated(),
	})

	// next_wave: speculative depth from live alpha (per tenant) or domain.
	alphaKey := tenant
	if alphaKey == "" {
		alphaKey = pkey
	}
	alphaEma := g.alpha.Get(alphaKey)
	promptText := lastUserTextOf(body)
	nMax := NextWaveNMax(alphaEma, &promptText, 0)

	merged := Body{}
	for k, v := range body {
		merged[k] = v
	}
	// Skip the chain-of-thought unless the caller has an opinion. Injected here,
	// before the prompt is tokenized, so the prefill and the decode build the
	// SAME prompt — a template flag applied to only one of them would make the
	// handoff restore a state the decode does not recognise.
	if g.noThink {
		if _, ok := merged["chat_template_kwargs"]; !ok {
			merged["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
		}
	}
	// do not override a caller who set it explicitly.
	if _, ok := merged[SpeculativeNMaxKey]; !ok {
		merged[SpeculativeNMaxKey] = nMax
	}

	fields := Record{
		"route":          "/api/chat",
		"tenant":         tenant,
		"slot":           slot,
		"n_max":          merged[SpeculativeNMaxKey],
		"admission":      decision.Reason,
		"est_new_tokens": decision.EstNewTokens,
		// diagnosis columns (2026-09-21): group turns by conversation and see the
		// estimate's inputs. pkey collision across conversations that share a
		// system prompt is the suspected cause of resident turns hitting prefill.
		"pkey":              pkey,
		"ckey":              ckey,
		"known_hot_tokens":  knownHot,
		"hot_prefix_tokens": hotPrefix,
		"resident_toks":     resident, // this conversation's tokens still in its slot (credited against est_new)
		"prefix_toks":       prefixToks,
		"prefix_exact":      prefixExact, // true = tokenizer count (cached per pkey); false = chars/4
		"tail_toks":         tailToks,
		// t1: wall-clock at admission, so decode occupancy can be crossed against the
		// instant a turn ARRIVES (not when it finishes appearing in the log).
		"t1_admit": time.Now().UTC().Format(time.RFC3339Nano),
	}
	// what the decode's slots held when this turn ARRIVED (evidence for "who
	// emptied the slot between two turns": the drop shows between two rows).
	if held, ok := g.poolHeldSafe(); ok {
		fields["pool_held_at_admit"] = held
	}
	decodeHeaders := Headers{"x-sofmat-slot": slot}
	admittedVia := decision.Route
	viaPrefill := false
	var ids []int      // exact prompt ids when tokenized this request (cache bookkeeping)
	tokenized := false // whether the prefill tokenizer was dialled this turn (success or fail)
	if decision.Route == "prefill" {
		// fail-soft: any prefill/handoff problem degrades to decode-direct.
		admittedVia = "decode-fallback"
		goPrefill := true
		if g.prefillDown() {
			// breaker open: the prefill side failed moments ago, don't dial it again.
			fields["admission"] = "prefill-down"
			admittedVia = "decode"
			goPrefill = false
		}
		if goPrefill && g.allowed != nil && !g.allowed(merged) {
			// its cache lives in another decode engine: serving it there is free,
			// dragging it to the primary would cost a full reprocess.
			fields["admission"] = "other-engine"
			admittedVia = "decode"
			goPrefill = false
		}
		busy := false
		if goPrefill && g.mode != ModeAlways {
			busy = g.busySafe()
		}
		if goPrefill && g.mode == ModeBusy && !busy {
			// idle decode: nothing to protect from interference, direct is faster
			// (and no tokenizer round-trip needed to know it).
			fields["admission"] = "decode-idle"
			admittedVia = "decode"
			goPrefill = false
		}
		// exact recount (chat template applied) — the estimate only opened the door.
		n, newExact := 0, 0
		switch {
		case !goPrefill:
		case g.tokens != nil:
			tokenized = true
			var err error
			ids, err = g.tokensSafe(merged)
			if err != nil {
				fields["prefill_error"] = "tokenize: " + err.Error()
				g.tripBreaker()
				goPrefill = false
			} else {
				n = len(ids)
				// tokens the decode already holds for this prefix (lower bound: the
				// common prefix with the previous prompt routed under the same key)
				newExact = n - g.last.bestPrefix(ckey, pkey, ids)
			}
		case g.count != nil:
			var err error
			n, err = g.countSafe(merged)
			if err != nil {
				fields["prefill_error"] = "count: " + err.Error()
				g.tripBreaker()
				goPrefill = false
			} else {
				newExact = n
			}
		}
		if goPrefill && n > 0 {
			fields["tokens"] = n
			// El conteo EXACTO ya está hecho aquí (costó una vuelta al tokenizador
			// y se paga igual). Viaja al que elige motor para que decida con él en
			// vez de con la estimación por bytes: estBodyTokens supone 3 bytes por
			// token y el error real medido va de +0,01 % (prosa) a -36 % (relleno
			// sintético o JSON), o sea que no se puede acotar. Cuando no hay conteo
			// —peticiones pequeñas, que no rozan el techo— se sigue estimando.
			decodeHeaders[ExactTokensHeader] = strconv.Itoa(n)
			if newExact < g.exactMin && !g.residentFor(merged, n-newExact, slot) {
				// the engine no longer holds the prefix we were counting on: this
				// prompt is cold, whatever the bookkeeping says. Say so out loud
				// and let the cost model route it (the prefill node can chew it
				// without stalling whoever is generating on the decode).
				fields["cache_evicted"] = true
				newExact = n
			}
			// recorded AFTER the eviction check: the log must show the number the
			// routing decision was actually made on
			fields["new_tokens"] = newExact
			// The prompt is measured: cap the reply to what the engine can still
			// host. The client sized max_tokens as if it were alone and cannot
			// know how much of the engine its own prompt just took.
			if g.replyRoom != nil {
				if room := g.replyRoom(n); room > 0 {
					if want := intField(merged, "max_tokens"); want > room {
						merged["max_tokens"] = room
						fields["max_tokens_clamped"] = room
						fields["max_tokens_asked"] = want
					}
				}
			}
			if newExact < g.exactMin {
				if newExact < n {
					fields["admission"] = "cache-hot"
					// cache_prompt lets the engine reuse its slot cache; the engine
					// selects the slot by its OWN content match (LCP), which is
					// identity-correct. We deliberately do NOT pin id_slot to convSlot:
					// convSlot goes stale between turns on a shared decode, and pinning
					// it forced the decode onto a slot holding ANOTHER conversation's
					// similar-sized KV — the count-based residency check was fooled and
					// the engine reprocessed the whole prompt (measured 2026-09-21, ids
					// 37/44: cache-hot with cache_n=0, prompt_n ~23-29k, 20-23 s). The
					// no-pin decode-direct route was measured fast at 16k tokens (0.3 s);
					// slots are stable now (per-slot residency fix), so the engine's own
					// slot selection no longer needs the pin that once corrected rotation.
					merged["cache_prompt"] = true
				} else {
					fields["admission"] = "exact-below-floor"
				}
				admittedVia = "decode"
				goPrefill = false
			}
		}
		if goPrefill && g.mode == ModeAuto && !busy && n > 0 && !g.cost.prefillCheaper(n, newExact) {
			// idle decode and the direct path is faster by the measured rates.
			fields["admission"] = "decode-cheaper"
			admittedVia = "decode"
			goPrefill = false
		}
		if goPrefill {
			t0 := time.Now()
			pre, err := g.callPrefillSafe(merged, Headers{"x-sofmat-slot": slot})
			if errors.Is(err, ErrSkipHandoff) {
				// not a prefill problem: serve direct now, retry the handoff next time
				fields["admission"] = "handoff-skipped"
				fields["handoff_skipped"] = strings.TrimPrefix(err.Error(), ErrSkipHandoff.Error()+": ")
				admittedVia = "decode"
			} else if err != nil {
				fields["prefill_error"] = err.Error()
				g.tripBreaker()
			} else {
				copyMetrics(fields, pre)
				hid, _ := pre["handoff_id"].(string)
				if hid == "" {
					fields["prefill_error"] = "no handoff_id"
				} else if hm, err := g.driveHandoffSafe(hid, slot); errors.Is(err, ErrSkipHandoff) {
					// no room in the decode at restore time: serve direct. The prefill
					// work is lost but the request is not, and the breaker stays closed.
					fields["admission"] = "handoff-skipped"
					fields["handoff_skipped"] = strings.TrimPrefix(err.Error(), ErrSkipHandoff.Error()+": ")
					admittedVia = "decode"
				} else if err != nil {
					fields["handoff_error"] = err.Error()
					g.tripBreaker()
				} else {
					copyMetrics(fields, hm)
					fields["handoff_id"] = hid
					fields["handoff_ms"] = msSince(t0)
					decodeHeaders["x-sofmat-kv-handoff"] = hid
					// the driver may have restored into a different (idle) slot than the
					// affinity pick — a restore into a busy slot waits for that stream to end.
					if s, _ := hm["slot"].(string); s != "" && s != slot {
						slot = s
						decodeHeaders["x-sofmat-slot"] = s
						fields["slot"] = s
					}
					// engine-visible: continue in the slot that now holds the restored KV.
					if si, err := strconv.Atoi(slot); err == nil {
						merged["id_slot"] = si
					}
					merged["cache_prompt"] = true
					admittedVia = "prefill"
					viaPrefill = true
				}
			}
		}
	}
	fields["admitted_via"] = admittedVia
	// admit_ms: the coordinator's own time BEFORE the decode is dialled (probes,
	// tokenizer, prefill + handoff when taken). With first_byte_ms, the reply's
	// generation and finish_ms, total_ms decomposes: an unexplained residual was
	// being read as engine queueing (2026-09-23 23:23, ids 10/11: 0.7-2.5 s).
	// admit_ms excludes the conversation wait: it is the coordinator's own work.
	fields["admit_ms"] = msSince(start) - float64(waitConv)/float64(time.Millisecond)
	fields["wait_conv_ms"] = float64(waitConv) / float64(time.Millisecond)
	fields["wait_conv_capped"] = capped
	if ids != nil {
		// whichever path ran, the decode slot now holds this prompt (either it
		// processed it or the restored state carries it).
		g.last.remember(ckey, ids)
		if ckey != pkey {
			// a stateless client sends system + the current question every time:
			// what it reuses is the system prefix, and this record is what sees it
			g.last.remember(pkey, ids)
		}
	}

	// The conversation's KV now lives in `slot` (processed there, or the handoff
	// restored it there and updated `slot` to where it landed). Remember it so the
	// next turn checks and pins THIS slot instead of a freshly hashed one.
	g.convSlot.set(ckey, slot)

	planned = true // the gate is now the Plan's: released by finishRecord
	return &Plan{
		Body:       merged,
		Headers:    decodeHeaders,
		fields:     fields,
		pkey:       pkey,
		ckey:       ckey,
		gated:      true,
		prefixToks: prefixToks,
		tailToks:   tailToks,
		alphaKey:   alphaKey,
		viaPrefill: viaPrefill,
		tokenized:  tokenized,
		start:      start,
	}, nil
}

// Note attaches a diagnostic to this request's record. The coordinator uses it
// to say what the engine actually answered: without it, a stream that produced
// nothing is recorded as a request with no timings and no cause, which is
// exactly the shape of the failures that took longest to diagnose.
func (p *Plan) Note(key string, v any) {
	if p == nil || p.fields == nil || key == "" {
		return
	}
	p.fields[key] = v
}

// Finish runs everything AFTER the decode call: prefix bookkeeping, the alpha
// EMA feed and the request record (engine timings included). resp may carry
// only {"timings": ...} — the streaming path reconstructs that from the last
// SSE chunk.
func (g *Gateway) Finish(p *Plan, resp Body) {
	// finish_ms: bookkeeping AFTER the reply (slot probe, the fix#4 tokenize of
	// the whole prompt on decode-direct turns). It is inside total_ms but the
	// streaming client has already got its answer by then. Written right before
	// the record is stored — a deferred write landed after finishRecord and the
	// field never appeared (live rows 1-6 of 2026-09-24 00:51: finish_ms null).
	finishStart := time.Now()
	// the slot now holds this prefix's KV — record it for later admissions.
	g.known.Record(p.pkey, p.prefixToks)
	// Learn where the conversation REALLY lives: the engine picks the slot by
	// content, the coordinator only guessed. cache_n + prompt_n is the engine's
	// n_prompt_tokens for the slot that just served, so the slot reporting exactly
	// that many tokens is it (unique, or we keep the guess). Written to the record
	// as slot_engine (+ slot_mismatch) and used by the next turn's residency probe.
	if cn, okc := timingInt(resp, "cache_n"); okc {
		if pn, okp := timingInt(resp, "prompt_n"); okp {
			// the slot keeps the reply too: /slots reports prompt + generated (+1
			// for the stop token), measured 2026-09-23 22:31 (16 481 + 123 -> 16 605).
			gen, _ := timingInt(resp, "predicted_n")
			// kv_source: where the cached part of this prompt came from, derived
			// from what was measured (the engine only says cache_n). Read BEFORE
			// convLast is overwritten with this turn. "slot": the whole conversation
			// (>= 80 % of what its last turn left) came back and the pool held it at
			// admit. "ram": the whole conversation came back although the pool held
			// LESS than that at admit — it cannot have been in VRAM, so the engine's
			// RAM prompt cache restored it. "lcp": only the shared catalogue prefix
			// (another conversation's slot, or a copy of the prefix). "none": cold.
			// Asked for 2026-09-23: three of David's turns came back cold and nobody
			// could say whether the RAM cache ever rescues anything (fix#12).
			// Timing that makes "ram" valid: pool_held_at_admit is read in Prepare,
			// before the decode is dialled, so before the engine assigns the slot
			// and restores from its RAM cache (its snapshot is at most 1 s OLDER,
			// never newer). "handoff": the coordinator itself restored the state
			// from a file (prefill route) — that is not the engine's cache, and it
			// must not inflate the "ram" count (id 18 of 2026-09-23 restored 51 817
			// tokens this way). A conversation the HUD SHRANK (the tail re-sliced)
			// legitimately reuses less than its last total: "whole" is measured
			// against the smaller of the last total and this prompt, and only when
			// the reuse clearly exceeds the catalogue prefix — otherwise it is "lcp".
			src := "none"
			total := cn + pn
			prev, hadPrev := g.convLast.get(p.ckey)
			whole := hadPrev && prev.total > 0 && cn > p.prefixToks*11/10 &&
				cn >= min(prev.total, total)*8/10
			switch {
			case p.viaPrefill:
				src = "handoff"
			case whole:
				src = "slot"
				if held, ok := p.fields["pool_held_at_admit"].(int); ok && held < prev.total*8/10 {
					src = "ram"
				}
			case p.prefixToks > 0 && cn >= p.prefixToks/2:
				src = "lcp"
			}
			// the slot this conversation was known to live in BEFORE this turn: an
			// "lcp" served by that same slot is the prompt diverging inside its own
			// conversation (the HUD re-slicing the history: id 80 of 2026-09-23,
			// cache_n = the catalogue with the whole conversation still in slot 2),
			// not a copy found elsewhere — "lcp-self".
			prevSlot := g.convSlot.get(p.ckey)
			// what the slot holds of this conversation now, with the shape of the
			// prompt that built it: the next turn's admission credits it (fix#10)
			// only if it keeps that shape and the slot still has it.
			if p.ckey != "" && cn+pn+gen > 0 {
				g.convLast.set(p.ckey, convTurn{prefix: p.prefixToks, tail: p.tailToks, total: cn + pn + gen})
			}
			if s, ok := g.slotOfSafe(cn + pn + gen); ok && s != "" {
				if src == "lcp" && prevSlot != "" && s == prevSlot {
					src = "lcp-self"
				}
				p.fields["slot_engine"] = s
				if recorded, _ := p.fields["slot"].(string); recorded != "" && recorded != s {
					p.fields["slot_mismatch"] = true
				}
				if p.ckey != "" {
					g.convSlot.set(p.ckey, s)
				}
			}
			p.fields["kv_source"] = src
		}
	}
	// ...unless the engine just told us it did NOT have it: a reply whose cache_n
	// is well below the prefix means the registry was stale (evicted slot, other
	// route). Forget it so the next admission counts the whole prompt again.
	if cn, ok := timingInt(resp, "cache_n"); ok && !p.viaPrefill && p.prefixToks > 0 && cn < p.prefixToks/2 {
		g.known.Forget(p.pkey)
		g.last.forget(p.pkey)
		// Do NOT forget g.last[ckey]: prefixToks is the SYSTEM prefix, so this miss
		// only says the system prefix was cold — it does not mean the CONVERSATION's
		// last-prompt record is invalid. Forgetting ckey poisoned the next turn:
		// bestPrefix then returned ~0, the whole (still-resident) conversation counted
		// as new, and it was shipped to a full prefill+handoff of tens of thousands of
		// tokens (measured 2026-09-21, ckey 0cc88487: new_tokens 40065, cache_n 40067,
		// handoff 26.7 s restoring a KV the decode already held). Keeping the ckey
		// record lets the next turn recognise the continuation and serve decode-direct;
		// if the KV really is gone the engine content-matches and reprocesses honestly.
		p.fields["prefix_cold"] = true
	}
	// Keep g.last fresh across DECODE-DIRECT turns too. Prepare only records g.last
	// when it tokenized (the prefill route); a run of decode-direct turns
	// (small-new-prefill / prefix-hot) leaves g.last stale, so the next turn that
	// crosses the est_new threshold finds bestPrefix ~0, counts the whole resident
	// conversation as new, and is shipped to a wasteful prefill+handoff with
	// prompt_n=1 — the engine had the conversation ENTIRE (measured 2026-09-22 on
	// David's live turns: 33 s wasted over 3 turns, each reprocessing ~20-30k it
	// already held). Tokenize here, AFTER the response, so it never adds latency to
	// the turn; the next turn's bestPrefix then recognises the prefix and stays
	// decode-direct. Best-effort: a tokenizer hiccup just leaves g.last as it was.
	if !p.tokenized && !g.prefillDown() && g.tokens != nil && p.ckey != "" {
		if ids, err := g.tokensSafe(p.Body); err == nil && len(ids) > 0 {
			g.last.remember(p.ckey, ids)
		}
	}
	// feed the cost model with what the engines just measured.
	g.cost.observe(p.fields, resp, p.viaPrefill)

	// feed the alpha EMA from the engine's acceptance counters, if present.
	dn, dnOK := timingInt(resp, "draft_n")
	da, daOK := timingInt(resp, "draft_n_accepted")
	if dnOK && daOK {
		g.alpha.Update(p.alphaKey, dn, da)
	}
	if alphaEma := g.alpha.Get(p.alphaKey); alphaEma != nil {
		p.fields["alpha_ema"] = *alphaEma
	} else {
		p.fields["alpha_ema"] = nil
	}
	p.fields["draft_n"] = nil
	p.fields["draft_n_accepted"] = nil
	if dnOK {
		p.fields["draft_n"] = dn
	}
	if daOK {
		p.fields["draft_n_accepted"] = da
	}
	// engine timings: prompt_n is the proof of the handoff (1 = the restored KV
	// was reused; the whole prompt = the engine re-processed it → kv_miss).
	if pn, ok := timingInt(resp, "prompt_n"); ok {
		p.fields["prompt_n"] = pn
		if p.viaPrefill {
			p.fields["kv_miss"] = pn > kvMissPromptTokens
		}
	}
	if cn, ok := timingInt(resp, "cache_n"); ok {
		p.fields["cache_n"] = cn
	}
	if n, ok := timingInt(resp, "predicted_n"); ok {
		p.fields["predicted_n"] = n
	}
	// tool_calls_n: how many tool calls the model returned in this reply. The
	// streamed path counts them on the wire (distinct tool_calls indexes) and
	// leaves a note; a whole reply is counted here. Asked for 2026-09-24: the
	// HUD claimed actions ("acabo de publicar el post") in turns that called
	// nothing, and the guard for that needs a count taken outside the HUD.
	if _, ok := p.fields["tool_calls_n"]; !ok {
		p.fields["tool_calls_n"] = toolCallsIn(resp)
	}
	if v, ok := timingFloat(resp, "predicted_per_second"); ok {
		p.fields["tg_tokps"] = v
	}
	if v, ok := timingFloat(resp, "prompt_ms"); ok {
		p.fields["prompt_ms"] = v
	}
	// Waits, ALWAYS written (0 = did not wait; -1 = not measurable this way), in
	// two separate numbers because they are two different queues with two
	// different fixes: the balancer's budget wait (room in the engine's KV) and
	// the engine-side wait before the first byte (a busy slot / prompt queue).
	// The coordinator measures them and hands them over either as plan notes
	// (streaming path) or as x-sofmat-t-* entries in the decode headers.
	if _, ok := p.fields["wait_budget_ms"]; !ok {
		p.fields["wait_budget_ms"] = headerMs(p.Headers, "x-sofmat-t-wait-budget-ms", 0)
	}
	if _, ok := p.fields["first_byte_ms"]; !ok {
		p.fields["first_byte_ms"] = headerMs(p.Headers, "x-sofmat-t-first-byte-ms", -1)
	}
	if v, ok := timingFloat(resp, "predicted_ms"); ok {
		p.fields["predicted_ms"] = v
	}
	if _, ok := p.fields["first_token_ms"]; !ok {
		p.fields["first_token_ms"] = -1.0
	}
	// wait_slot_ms: the queue in front of the slot — what the engine took to
	// START on the prompt. llama-server sends a streamed reply's HTTP headers
	// BEFORE processing the prompt (live 2026-09-23 id 13: first_byte 27 ms for
	// a 13k-token prompt of 5.8 s), so first_byte - prompt_ms undercounted the
	// wait by exactly prompt_ms and read 0 under real contention. Streamed: the
	// first TOKEN minus prompt_ms. Non-streamed: the reply arrives whole, after
	// the generation as well, so that comes off too. Needs prompt_ms.
	// wait_slot_src says which derivation produced the number, so a streamed
	// row ("first-token") and a whole-reply row ("whole-reply": headers arrive
	// after prompt AND generation, both come off) are never compared blindly.
	// A whole reply without predicted_ms is NOT derivable: the headers-minus-
	// prompt figure is the undercount fix#11 removed, so it stays -1 (review
	// by debian-dev 2026-09-24 00:52: "prefiero un -1 honesto").
	p.fields["wait_slot_ms"] = -1.0
	p.fields["wait_slot_src"] = ""
	if pm, ok := p.fields["prompt_ms"].(float64); ok {
		ws, src := 0.0, ""
		if ft, _ := p.fields["first_token_ms"].(float64); ft >= 0 {
			ws, src = ft-pm, "first-token"
		} else if fb, _ := p.fields["first_byte_ms"].(float64); fb >= 0 {
			if gm, ok := p.fields["predicted_ms"].(float64); ok {
				ws, src = fb-pm-gm, "whole-reply"
			}
		}
		if src != "" {
			if ws < 0 {
				ws = 0
			}
			p.fields["wait_slot_ms"] = ws
			p.fields["wait_slot_src"] = src
		}
	}
	p.fields["finish_ms"] = msSince(finishStart)
	g.finishRecord(p)
}

// headerMs reads a millisecond figure the coordinator left in the decode headers
// (never forwarded to the engine), or def when absent/unparseable.
func headerMs(h Headers, key string, def float64) float64 {
	if h == nil {
		return def
	}
	v, ok := h[key]
	if !ok || v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// kvMissPromptTokens: after a handoff the decode should process ~1 token (the
// held-back last one); anything past this many means the restored KV was not
// reused and the prompt was re-processed.
const kvMissPromptTokens = 64

func (g *Gateway) finishRecord(p *Plan) {
	if p.gated {
		p.gated = false
		g.convGate.release(p.ckey)
	}
	p.fields["total_ms"] = msSince(p.start)
	g.log.RecordEntry(p.fields, nil)
}

// convGate serialises the requests of one conversation (fix#15). An entry
// counts the requests in flight for a key; acquire waits while the count is
// positive, up to the cap, then joins anyway (capped) so a stuck request can
// never block its conversation for good. Zero cap = no waiting at all.
type convGate struct {
	mu      sync.Mutex
	entries map[string]*convGateEntry
}

type convGateEntry struct {
	n    int
	done chan struct{} // closed when n drops to 0
}

func newConvGate() *convGate { return &convGate{entries: map[string]*convGateEntry{}} }

func (c *convGate) acquire(key string, limit time.Duration) (waited time.Duration, capped bool) {
	if key == "" {
		return 0, false
	}
	t0 := time.Now()
	deadline := t0.Add(limit)
	blocked := false // 0 means "did not wait", not "waited a few microseconds for the lock"
	for {
		c.mu.Lock()
		e := c.entries[key]
		if e == nil || e.n == 0 || limit <= 0 {
			if e == nil {
				e = &convGateEntry{done: make(chan struct{})}
				c.entries[key] = e
			}
			e.n++
			c.mu.Unlock()
			if !blocked {
				return 0, false
			}
			return time.Since(t0), capped
		}
		blocked = true
		done := e.done
		c.mu.Unlock()
		remaining := time.Until(deadline)
		if remaining <= 0 {
			// the cap ran out: go on overlapping, and say so
			c.mu.Lock()
			if e2 := c.entries[key]; e2 != nil {
				e2.n++
			} else {
				c.entries[key] = &convGateEntry{n: 1, done: make(chan struct{})}
			}
			c.mu.Unlock()
			return time.Since(t0), true
		}
		select {
		case <-done:
		case <-time.After(remaining):
		}
	}
}

func (c *convGate) release(key string) {
	if key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil {
		return
	}
	e.n--
	if e.n <= 0 {
		close(e.done)
		delete(c.entries, key)
	}
}

// callPrefillSafe isolates the prefill call: an error OR a panic in the
// injected callable both read as "prefill unavailable right now".
func (g *Gateway) callPrefillSafe(body Body, extra Headers) (out Body, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("prefill panic: %v", r)
		}
	}()
	out, err = g.prefill(body, extra)
	if err == nil && out == nil {
		err = fmt.Errorf("prefill returned nil body")
	}
	return out, err
}

func (g *Gateway) driveHandoffSafe(hid, slot string) (out Body, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("handoff panic: %v", r)
		}
	}()
	return g.handoff(hid, slot)
}

// prefillDown reports whether the breaker is open (recent prefill-side failure).
func (g *Gateway) prefillDown() bool {
	g.breakerMu.Lock()
	defer g.breakerMu.Unlock()
	return time.Now().Before(g.downUntil)
}

// tripBreaker closes the prefill route for breakerFor (no-op when negative).
func (g *Gateway) tripBreaker() {
	if g.breakerFor < 0 {
		return
	}
	g.breakerMu.Lock()
	g.downUntil = time.Now().Add(g.breakerFor)
	g.breakerMu.Unlock()
}

// busySafe: a probe error or panic reads as "idle" (the faster direct path).
func (g *Gateway) busySafe() (busy bool) {
	defer func() {
		if r := recover(); r != nil {
			busy = false
		}
	}()
	return g.busy()
}

func (g *Gateway) tokensSafe(body Body) (ids []int, err error) {
	defer func() {
		if r := recover(); r != nil {
			ids, err = nil, fmt.Errorf("tokenize panic: %v", r)
		}
	}()
	return g.tokens(body)
}

func (g *Gateway) countSafe(body Body) (n int, err error) {
	defer func() {
		if r := recover(); r != nil {
			n, err = 0, fmt.Errorf("count panic: %v", r)
		}
	}()
	return g.count(body)
}

// copyMetrics folds the numeric/bool fields a driver returned into the record
// (strings other than handoff_id are dropped: metrics only, never content).
func copyMetrics(dst Record, src Body) {
	for k, v := range src {
		switch v.(type) {
		case int, int64, float64, bool:
			dst[k] = v
		}
	}
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000
}

// ── body helpers ────────────────────────────────────────────────────────────

func messagesOf(body Body) []map[string]any {
	raw, _ := body["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		if mm, ok := m.(map[string]any); ok {
			out = append(out, mm)
		}
	}
	// also accept a pre-typed slice (tests, in-process callers)
	if typed, ok := body["messages"].([]map[string]any); ok {
		out = typed
	}
	return out
}

func systemPromptOf(body Body) string {
	for _, m := range messagesOf(body) {
		if m["role"] == "system" {
			s, _ := m["content"].(string)
			return s
		}
	}
	return ""
}

func lastUserTextOf(body Body) string {
	msgs := messagesOf(body)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i]["role"] == "user" {
			s, _ := msgs[i]["content"].(string)
			return s
		}
	}
	return ""
}

// prefixTextOf is the stable prefix the engine really sees: the system prompt
// PLUS the tool catalogue. The chat template renders `tools[]` ahead of the
// conversation, next to the system prompt, so the engine caches them with it
// and they belong to the prefix — the same across a conversation's turns, cold
// only once. Estimating the prefix from the system prompt alone under-counted
// the HUD's prompt ~4x (measured 2026-09-22 on David's live turns: prefix 2415
// + tail 1766 = 4181 estimated against prompt_n 17412 received): the admission
// threshold was deciding on a number the engine never saw, and a turn crossing
// it by a large tool result was shipped to a handoff its cache did not need.
func prefixTextOf(body Body) string {
	return systemPromptOf(body) + toolsTextOf(body)
}

// toolsTextOf serialises the tool catalogue the way the template will: JSON.
// Absent or malformed tools estimate as nothing (never fail admission on them).
func toolsTextOf(body Body) string {
	tools, ok := body["tools"]
	if !ok || tools == nil {
		return ""
	}
	b, err := json.Marshal(tools)
	if err != nil {
		return ""
	}
	return string(b)
}

// tailTextOf is everything after the stable prefix: the non-system turns,
// including the assistant's tool_calls (name + arguments), which the template
// renders into the prompt like any other content.
func tailTextOf(body Body) string {
	out := ""
	for _, m := range messagesOf(body) {
		if m["role"] == "system" {
			continue
		}
		s, _ := m["content"].(string)
		if tc, ok := m["tool_calls"]; ok && tc != nil {
			if b, err := json.Marshal(tc); err == nil {
				s += string(b)
			}
		}
		if out != "" {
			out += "\n"
		}
		out += s
	}
	return out
}

// timingInt reads an int out of resp["timings"][key], tolerating JSON's
// float64 decoding.
func timingInt(resp Body, key string) (int, bool) {
	tm, _ := resp["timings"].(map[string]any)
	if tm == nil {
		return 0, false
	}
	switch v := tm[key].(type) {
	case int:
		return v, true
	case float64:
		return int(v), true
	default:
		return 0, false
	}
}

// toolCallsIn counts the tool calls of a whole (non-streamed) chat reply.
func toolCallsIn(resp Body) int {
	n := 0
	choices, _ := resp["choices"].([]any)
	for _, c := range choices {
		cm, _ := c.(map[string]any)
		msg, _ := cm["message"].(map[string]any)
		if tc, ok := msg["tool_calls"].([]any); ok {
			n += len(tc)
		}
	}
	return n
}

func timingFloat(resp Body, key string) (float64, bool) {
	tm, _ := resp["timings"].(map[string]any)
	if tm == nil {
		return 0, false
	}
	switch v := tm[key].(type) {
	case int:
		return float64(v), true
	case float64:
		return v, true
	default:
		return 0, false
	}
}
