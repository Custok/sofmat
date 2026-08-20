package gateway

// Admission policy — decides, per request, whether prompt work runs on the
// DECODE pipeline directly or through a dedicated PREFILL node first
// (disaggregated serving, docs/design/kv-handoff-desagregado.md). Measured
// basis: a long co-located prefill degrades a 40-token decode ×5.79 on the
// live 3-GPU pipeline (×8.5 on the earlier 4-GPU one); with the prefill on a
// dedicated node the same probe reads ×0.99 — interference eliminated
// (sofmat-bench/interference_results.md). The admission rule is a threshold
// on the request's NEW prefill work, per the spec rule
// interferencia_evitada > coste_handoff.

import (
	"fmt"
	"sync"
	"unicode/utf8"
)

// PrefillThresholdTokens is the default admission threshold in estimated NEW
// prompt tokens. Conservative v1 floor; the interference bench moves it onto
// a measured curve.
const PrefillThresholdTokens = 2048

// charsPerToken is the estimator fallback (mixed ES/EN measured ~3.6-4.2
// chars/token; 4 biases low, preferring decode-direct on ties — the cheaper
// wrong answer).
const charsPerToken = 4

// AdmissionDecision says where a request's prompt work should run.
type AdmissionDecision struct {
	Route        string // "decode" or "prefill"
	Reason       string // auditable tag for the request log
	EstNewTokens int
}

// Tokenizer optionally replaces the chars/4 heuristic (e.g. a cached
// /tokenize round-trip). Admission needs the order of magnitude, not the
// exact count.
type Tokenizer func(text string) int

// EstimateTokens estimates the token count of text.
func EstimateTokens(text string, tok Tokenizer) int {
	if text == "" {
		return 0
	}
	if tok != nil {
		n := tok(text)
		if n < 0 {
			return 0
		}
		return n
	}
	return utf8.RuneCountInString(text) / charsPerToken
}

// KnownPrefixes is a bounded LRU of prefix keys believed hot in a decode
// slot's prompt cache. Bounded because the engine's own cache is bounded — an
// unbounded registry would claim reuse the engine evicted long ago. Being
// wrong only costs one conservative reroute. Safe for concurrent use.
type KnownPrefixes struct {
	mu       sync.Mutex
	capacity int
	toks     map[string]int
	order    []string // LRU order, oldest first
}

func NewKnownPrefixes(capacity int) (*KnownPrefixes, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("capacity must be positive: %d", capacity)
	}
	return &KnownPrefixes{capacity: capacity, toks: map[string]int{}}, nil
}

func (k *KnownPrefixes) touch(key string) {
	for i, o := range k.order {
		if o == key {
			k.order = append(k.order[:i], k.order[i+1:]...)
			break
		}
	}
	k.order = append(k.order, key)
}

// Record marks key as hot with its prefilled token estimate.
func (k *KnownPrefixes) Record(key string, estTokens int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.toks[key]; !ok && len(k.toks) >= k.capacity {
		oldest := k.order[0]
		k.order = k.order[1:]
		delete(k.toks, oldest)
	}
	k.toks[key] = estTokens
	k.touch(key)
}

// HotTokens returns the tokens already prefilled for key (0 if unknown or
// evicted) and refreshes its LRU position.
func (k *KnownPrefixes) HotTokens(key string) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.toks[key]
	if !ok {
		return 0
	}
	k.touch(key)
	return v
}

// AdmissionInput carries the estimates ClassifyAdmission decides on.
type AdmissionInput struct {
	PrefixTokens     int  // stable shared prefix (system prompt / tenant header)
	TailTokens       int  // rest of the prompt (conversation + new turn)
	HotPrefixTokens  int  // tokens of this prefix already hot in a decode slot
	Threshold        int  // 0 means PrefillThresholdTokens
	PrefillAvailable bool // false degrades to decode-direct (fail-soft)
}

// ClassifyAdmission decides the route for a request. Admission must never
// fail a request: with no prefill role available it always answers decode.
func ClassifyAdmission(in AdmissionInput) AdmissionDecision {
	threshold := in.Threshold
	if threshold == 0 {
		threshold = PrefillThresholdTokens
	}
	newTokens := in.PrefixTokens - in.HotPrefixTokens
	if newTokens < 0 {
		newTokens = 0
	}
	newTokens += in.TailTokens
	if !in.PrefillAvailable {
		return AdmissionDecision{"decode", "prefill-unavailable", newTokens}
	}
	if newTokens >= threshold {
		return AdmissionDecision{"prefill", "large-new-prefill", newTokens}
	}
	if in.HotPrefixTokens > 0 && in.PrefixTokens <= in.HotPrefixTokens+threshold {
		return AdmissionDecision{"decode", "prefix-hot", newTokens}
	}
	return AdmissionDecision{"decode", "small-new-prefill", newTokens}
}
