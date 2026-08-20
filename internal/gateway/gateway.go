package gateway

// Gateway — composes policy + admission + status + request-log behind one
// authenticated surface. Every dependency (auth verify, backend call, prefill
// call, handoff driver, status provider) is injected, so routing/policy logic
// is fully testable without a network or a live engine.
//
// The engine's real speedup lever (speculative n_max), the prefix-cache reuse
// (slot affinity) and the disaggregated admission (prefill node + KV handoff)
// are applied HERE as policy, so they persist across engine restarts.

import (
	"errors"
	"fmt"
	"strconv"
)

var ErrUnauthorized = errors.New("unauthorized")

type Headers map[string]string
type Body map[string]any

// Verify authenticates a request from its headers.
type Verify func(h Headers) bool

// BackendCall proxies a request to the decode engine.
type BackendCall func(body Body, extra Headers) (Body, error)

// PrefillCall sends the request to the dedicated prefill node; it must return
// a body containing "handoff_id" once the KV is ready to ship.
type PrefillCall func(body Body, extra Headers) (Body, error)

// Handoff drives the transport-lane KV scatter (prefill topology -> decode
// layer map) and returns true once the decode pipeline holds the sequence.
// Owned by the transport module; the gateway only sequences it.
type Handoff func(handoffID string, slot string) bool

// StatusProvider returns the /api/status document.
type StatusProvider func() Body

type Gateway struct {
	verify  Verify
	backend BackendCall
	status  StatusProvider
	prefill PrefillCall // optional; nil = no disaggregation
	handoff Handoff     // optional; nil = no disaggregation
	ring    *Ring
	alpha   *AlphaEma
	log     *RequestLog
	known   *KnownPrefixes
}

// Options for New. NSlots defaults to 4; KeepContent defaults to false.
type Options struct {
	Verify         Verify
	BackendCall    BackendCall
	StatusProvider StatusProvider
	PrefillCall    PrefillCall
	Handoff        Handoff
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
	return &Gateway{
		verify:  o.Verify,
		backend: o.BackendCall,
		status:  o.StatusProvider,
		prefill: o.PrefillCall,
		handoff: o.Handoff,
		ring:    NewRing(members, 64),
		alpha:   alpha,
		log:     NewRequestLog(500, o.KeepContent),
		known:   known,
	}, nil
}

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

// Chat handles POST /api/chat: apply policy, proxy, log. Returns the backend
// response verbatim.
func (g *Gateway) Chat(h Headers, body Body) (Body, error) {
	if err := g.auth(h); err != nil {
		return nil, err
	}

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
		PrefillAvailable: g.prefill != nil && g.handoff != nil,
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

	decodeHeaders := Headers{"x-sofmat-slot": slot}
	admittedVia := decision.Route
	if decision.Route == "prefill" {
		// fail-soft: any prefill/handoff problem degrades to decode-direct.
		admittedVia = "decode-fallback"
		if pre, err := g.callPrefillSafe(merged, Headers{"x-sofmat-slot": slot}); err == nil {
			if hid, _ := pre["handoff_id"].(string); hid != "" && g.driveHandoffSafe(hid, slot) {
				decodeHeaders["x-sofmat-kv-handoff"] = hid
				admittedVia = "prefill"
			}
		}
	}

	resp, err := g.backend(merged, decodeHeaders)
	if err != nil {
		return nil, err
	}
	// the slot now holds this prefix's KV — record it for later admissions.
	g.known.Record(pkey, prefixToks)

	// feed the alpha EMA from the engine's acceptance counters, if present.
	dn, dnOK := timingInt(resp, "draft_n")
	da, daOK := timingInt(resp, "draft_n_accepted")
	if dnOK && daOK {
		g.alpha.Update(alphaKey, dn, da)
	}

	fields := Record{
		"route":            "/api/chat",
		"tenant":           tenant,
		"slot":             slot,
		"n_max":            merged[SpeculativeNMaxKey],
		"draft_n":          nil,
		"draft_n_accepted": nil,
		"admission":        decision.Reason,
		"admitted_via":     admittedVia,
		"est_new_tokens":   decision.EstNewTokens,
	}
	if alphaEma != nil {
		fields["alpha_ema"] = *alphaEma
	} else {
		fields["alpha_ema"] = nil
	}
	if dnOK {
		fields["draft_n"] = dn
	}
	if daOK {
		fields["draft_n_accepted"] = da
	}
	g.log.RecordEntry(fields, nil)
	return resp, nil
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

func (g *Gateway) driveHandoffSafe(hid, slot string) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()
	return g.handoff(hid, slot)
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
