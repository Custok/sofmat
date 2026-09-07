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
	verify    Verify
	backend   BackendCall
	status    StatusProvider
	prefill   PrefillCall          // optional; nil = no disaggregation
	handoff   Handoff              // optional; nil = no disaggregation
	count     CountTokens          // optional; nil = estimate only
	tokens    Tokens               // optional; supersedes count, enables the cache-aware estimate
	busy      DecodeBusy           // optional; nil = handoff whenever admitted
	allowed   func(Body) bool      // optional; false = this request must not be handed off
	resident  func(Body, int) bool // optional; false = the engine no longer holds that prefix
	mode      string               // ModeBusy / ModeAlways / ModeAuto
	threshold int                  // admission threshold on the ESTIMATE
	exactMin  int                  // floor on the EXACT count (when count != nil)
	ring      *Ring
	alpha     *AlphaEma
	log       *RequestLog
	known     *KnownPrefixes
	last      *lastPrompts // per prefix key: ids of the last prompt routed (cache lower bound)
	cost      *costModel   // measured pp / handoff EMAs for ModeAuto

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
	// CacheResident asks whether the engine that will serve this request still
	// holds about that many tokens of its prefix. The bookkeeping here can only
	// say what the gateway ROUTED; it cannot see the engine evicting a slot to
	// make room for somebody else. Without this check a conversation whose cache
	// was evicted is admitted as cache-hot and the decode silently reprocesses
	// the whole prompt (measured 2026-09-07: 78 329 tokens, 184.5 s).
	// nil = assume resident (previous behaviour).
	CacheResident func(Body, int) bool

	// HandoffAllowed vetoes the handoff for a request the caller knows must not
	// take it. With more than one decode engine the restored state lands in the
	// primary, so a conversation whose cache lives in ANOTHER engine must not be
	// handed off: it would be dragged across engines and lose its whole prefix.
	HandoffAllowed func(Body) bool
	Mode           string // ModeBusy (default when DecodeBusy is set), ModeAlways, ModeAuto
	Threshold      int
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
		breakerFor: breaker,
		verify:     o.Verify,
		backend:    o.BackendCall,
		status:     o.StatusProvider,
		prefill:    o.PrefillCall,
		handoff:    o.Handoff,
		count:      o.CountTokens,
		tokens:     o.Tokens,
		busy:       o.DecodeBusy,
		allowed:    o.HandoffAllowed,
		resident:   o.CacheResident,
		mode:       mode,
		threshold:  th,
		exactMin:   em,
		ring:       NewRing(members, 64),
		alpha:      alpha,
		log:        NewRequestLog(500, o.KeepContent),
		known:      known,
		last:       newLastPrompts(8 * n),
		cost:       newCostModel(),
	}, nil
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
	alphaKey   string
	viaPrefill bool
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
func (g *Gateway) residentFor(body Body, expect int) bool {
	if g.resident == nil || expect <= 0 {
		return true
	}
	ok := true
	func() {
		defer func() { _ = recover() }() // a probe must never take the request down
		ok = g.resident(body, expect)
	}()
	return ok
}

// residentHot zeroes a "hot prefix" the engine no longer holds.
func (g *Gateway) residentHot(body Body, hot int) int {
	if hot > 0 && !g.residentFor(body, hot) {
		return 0
	}
	return hot
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
	slot, err := g.ring.Route(pkey)
	if err != nil {
		return nil, err
	}

	// admission: decode-direct, or dedicated prefill + KV handoff first.
	prefixToks := EstimateTokens(systemPrompt, nil)
	tailToks := EstimateTokens(tailTextOf(body), nil)
	decision := ClassifyAdmission(AdmissionInput{
		PrefixTokens:     prefixToks,
		TailTokens:       tailToks,
		HotPrefixTokens:  g.residentHot(body, g.known.HotTokens(pkey)),
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
	}
	decodeHeaders := Headers{"x-sofmat-slot": slot}
	admittedVia := decision.Route
	viaPrefill := false
	var ids []int // exact prompt ids when tokenized this request (cache bookkeeping)
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
			if newExact < g.exactMin && !g.residentFor(merged, n-newExact) {
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
			if newExact < g.exactMin {
				if newExact < n {
					fields["admission"] = "cache-hot"
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

	return &Plan{
		Body:       merged,
		Headers:    decodeHeaders,
		fields:     fields,
		pkey:       pkey,
		ckey:       ckey,
		prefixToks: prefixToks,
		alphaKey:   alphaKey,
		viaPrefill: viaPrefill,
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
	// the slot now holds this prefix's KV — record it for later admissions.
	g.known.Record(p.pkey, p.prefixToks)
	// ...unless the engine just told us it did NOT have it: a reply whose cache_n
	// is well below the prefix means the registry was stale (evicted slot, other
	// route). Forget it so the next admission counts the whole prompt again.
	if cn, ok := timingInt(resp, "cache_n"); ok && !p.viaPrefill && p.prefixToks > 0 && cn < p.prefixToks/2 {
		g.known.Forget(p.pkey)
		g.last.forget(p.pkey)
		if p.ckey != "" && p.ckey != p.pkey {
			g.last.forget(p.ckey)
		}
		p.fields["prefix_cold"] = true
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
	if v, ok := timingFloat(resp, "predicted_per_second"); ok {
		p.fields["tg_tokps"] = v
	}
	if v, ok := timingFloat(resp, "prompt_ms"); ok {
		p.fields["prompt_ms"] = v
	}
	g.finishRecord(p)
}

// kvMissPromptTokens: after a handoff the decode should process ~1 token (the
// held-back last one); anything past this many means the restored KV was not
// reused and the prompt was re-processed.
const kvMissPromptTokens = 64

func (g *Gateway) finishRecord(p *Plan) {
	p.fields["total_ms"] = msSince(p.start)
	g.log.RecordEntry(p.fields, nil)
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

// tailTextOf is everything after the stable prefix: the non-system turns.
func tailTextOf(body Body) string {
	out := ""
	for _, m := range messagesOf(body) {
		if m["role"] == "system" {
			continue
		}
		s, _ := m["content"].(string)
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
