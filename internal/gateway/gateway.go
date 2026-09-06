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
	"errors"
	"fmt"
	"strconv"
	"time"
)

var ErrUnauthorized = errors.New("unauthorized")

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

// Handoff moves the saved state to the decode node and restores it into the
// decode slot; it returns its metrics (fetch_ms, restore_ms, n_restored) or an
// error. Owned by the transport module; the gateway only sequences it.
type Handoff func(handoffID string, slot string) (Body, error)

// CountTokens returns the EXACT prompt token count of a chat body as the
// engine will see it (chat template applied). Optional: when set, a request
// the estimate admitted to prefill is re-checked against the exact floor.
type CountTokens func(body Body) (int, error)

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
	prefill   PrefillCall // optional; nil = no disaggregation
	handoff   Handoff     // optional; nil = no disaggregation
	count     CountTokens // optional; nil = estimate only
	busy      DecodeBusy  // optional; nil = handoff whenever admitted
	threshold int         // admission threshold on the ESTIMATE
	exactMin  int         // floor on the EXACT count (when count != nil)
	ring      *Ring
	alpha     *AlphaEma
	log       *RequestLog
	known     *KnownPrefixes
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
	DecodeBusy     DecodeBusy
	Threshold      int
	ExactMinTokens int
	NSlots         int
	KeepContent    bool
}

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
	known, err := NewKnownPrefixes(512)
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
	return &Gateway{
		verify:    o.Verify,
		backend:   o.BackendCall,
		status:    o.StatusProvider,
		prefill:   o.PrefillCall,
		handoff:   o.Handoff,
		count:     o.CountTokens,
		busy:      o.DecodeBusy,
		threshold: th,
		exactMin:  em,
		ring:      NewRing(members, 64),
		alpha:     alpha,
		log:       NewRequestLog(500, o.KeepContent),
		known:     known,
	}, nil
}

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
	// slot affinity: same-prefix requests to the same slot to reuse KV.
	pkey := PrefixKey(systemPrompt, tenant)
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
		HotPrefixTokens:  g.known.HotTokens(pkey),
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
	if decision.Route == "prefill" {
		// fail-soft: any prefill/handoff problem degrades to decode-direct.
		admittedVia = "decode-fallback"
		goPrefill := true
		if g.busy != nil && !g.busySafe() {
			// idle decode: nothing to protect from interference, direct is faster.
			fields["admission"] = "decode-idle"
			admittedVia = "decode"
			goPrefill = false
		}
		if goPrefill && g.count != nil {
			// exact recount (chat template applied) — the estimate only opened the door.
			n, err := g.countSafe(merged)
			switch {
			case err != nil:
				fields["prefill_error"] = "count: " + err.Error()
				goPrefill = false
			case n < g.exactMin:
				fields["tokens"] = n
				fields["admission"] = "exact-below-floor"
				admittedVia = "decode"
				goPrefill = false
			default:
				fields["tokens"] = n
			}
		}
		if goPrefill {
			t0 := time.Now()
			pre, err := g.callPrefillSafe(merged, Headers{"x-sofmat-slot": slot})
			if err != nil {
				fields["prefill_error"] = err.Error()
			} else {
				copyMetrics(fields, pre)
				hid, _ := pre["handoff_id"].(string)
				if hid == "" {
					fields["prefill_error"] = "no handoff_id"
				} else if hm, err := g.driveHandoffSafe(hid, slot); err != nil {
					fields["handoff_error"] = err.Error()
				} else {
					copyMetrics(fields, hm)
					fields["handoff_id"] = hid
					fields["handoff_ms"] = msSince(t0)
					decodeHeaders["x-sofmat-kv-handoff"] = hid
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

	return &Plan{
		Body:       merged,
		Headers:    decodeHeaders,
		fields:     fields,
		pkey:       pkey,
		prefixToks: prefixToks,
		alphaKey:   alphaKey,
		viaPrefill: viaPrefill,
		start:      start,
	}, nil
}

// Finish runs everything AFTER the decode call: prefix bookkeeping, the alpha
// EMA feed and the request record (engine timings included). resp may carry
// only {"timings": ...} — the streaming path reconstructs that from the last
// SSE chunk.
func (g *Gateway) Finish(p *Plan, resp Body) {
	// the slot now holds this prefix's KV — record it for later admissions.
	g.known.Record(p.pkey, p.prefixToks)

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

// busySafe: a probe error or panic reads as "idle" (the faster direct path).
func (g *Gateway) busySafe() (busy bool) {
	defer func() {
		if r := recover(); r != nil {
			busy = false
		}
	}()
	return g.busy()
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
