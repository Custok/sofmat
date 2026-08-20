// Package gateway is the sofmat control-plane front door: it classifies each
// request (prefix-cache state, small vs large prefill) and routes it either
// straight to decode or via a prefill node + KV handoff, always with a hard
// fail-soft to decode-direct so a broken handoff never drops a request.
//
// Ports gateway-v0/ (admission.py + role-aware server, 63 tests). Lane: gateway.
package gateway

// Decision is the coarse admission outcome for a request. It is the skeleton
// façade over AdmissionDecision for callers that only need the fork; the full
// routing (slot affinity, next_wave, handoff sequencing, metrics) lives on
// Gateway.Chat.
type Decision int

const (
	// DecodeDirect: prefill marginal or the handoff would cost more than the
	// interference it avoids.
	DecodeDirect Decision = iota
	// PrefillThenHandoff: a large new prefill -> prefill node -> KV handoff -> decode.
	PrefillThenHandoff
)

func (d Decision) String() string {
	if d == PrefillThenHandoff {
		return "prefill-then-handoff"
	}
	return "decode-direct"
}

// Admit classifies a request by its estimated NEW prefill tokens (work the
// decode slot does not already hold) and whether the request's stable prefix
// is hot in a decode slot. Callers that track prefix heat with KnownPrefixes
// pass the discounted estimate; ClassifyAdmission is the full-fidelity form.
func Admit(estNewPrefillTokens int, prefixHot bool) Decision {
	in := AdmissionInput{TailTokens: estNewPrefillTokens, PrefillAvailable: true}
	if prefixHot {
		in.HotPrefixTokens = 1 // heat marker; the caller already discounted
	}
	if ClassifyAdmission(in).Route == "prefill" {
		return PrefillThenHandoff
	}
	return DecodeDirect
}
