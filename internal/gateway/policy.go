// Package gateway is the durable home of sofmat's runtime serving policy —
// the Go production port of the validated Python prototype (gateway-v0).
// Behavior is 1:1 with the prototype; its test suite is the contract.
package gateway

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// ── 1. next_wave: speculative depth from acceptance ─────────────────────────
//
// Values from the canonical spec docs/design/next-wave-spec.md: table v1 =
// code 3 / technical 3 / prose 2, guardrail {2,3,4}. With alpha ~0.96 in code,
// n_max 3/4/5 are flat (verification-bound) so the smallest saturating depth
// wins; in prose (~0.39) a deep wave drafts condemned tokens that still pay
// verification, so shallow is strictly better.

const (
	NMaxFloor = 2 // spec guardrail {2,3,4}
	NMaxCap   = 4
)

// NMaxForAlpha returns the speculative n_max for a measured acceptance rate
// alpha in [0,1]. Monotone non-decreasing, clamped to [NMaxFloor, NMaxCap].
func NMaxForAlpha(alpha float64) (int, error) {
	if alpha < 0.0 || alpha > 1.0 {
		return 0, fmt.Errorf("alpha out of range: %v", alpha)
	}
	n := 2
	if alpha >= 0.55 {
		n = 3
	}
	if n < NMaxFloor {
		n = NMaxFloor
	}
	if n > NMaxCap {
		n = NMaxCap
	}
	return n, nil
}

// Domain fallback for cold start, before an alpha EMA exists for a caller.
// Nominal acceptance per domain from the measured sweep.
var domainAlpha = map[string]float64{
	"code":      0.96,
	"technical": 0.59,
	"prose":     0.39,
}

var codeMarkers = regexp.MustCompile(
	"(?m)```|def\\s+\\w+\\s*\\(|class\\s+\\w+|import\\s+\\w+|function\\s+\\w+|=>|;\\s*$|</?\\w+>")

var technicalWords = []string{
	"protocol", "protocolo", "algorithm", "algoritmo", "latency",
	"throughput", "cache", "kernel", "tcp", "http", "gpu", "config",
}

// ClassifyDomain is the cheap cold-start domain heuristic. Superseded by the
// live alpha EMA as soon as one is warmed up.
func ClassifyDomain(prompt string) string {
	if codeMarkers.MatchString(prompt) {
		return "code"
	}
	low := strings.ToLower(prompt)
	for _, w := range technicalWords {
		if strings.Contains(low, w) {
			return "technical"
		}
	}
	return "prose"
}

// SpeculativeNMaxKey is the engine request field next_wave overrides.
const SpeculativeNMaxKey = "speculative.n_max"

// NextWaveNMax decides the per-request speculative depth. alphaEma nil falls
// back to a domain guess from prompt; prompt nil falls back to the neutral
// technical prior. cap <= 0 means the module cap.
func NextWaveNMax(alphaEma *float64, prompt *string, cap int) int {
	if cap <= 0 {
		cap = NMaxCap
	}
	a := 0.0
	switch {
	case alphaEma != nil:
		a = *alphaEma
	case prompt != nil:
		a = domainAlpha[ClassifyDomain(*prompt)]
	default:
		a = domainAlpha["technical"]
	}
	n, err := NMaxForAlpha(a)
	if err != nil { // out-of-range EMA cannot happen from AlphaEma; be safe
		n = NMaxFloor
	}
	if n > cap {
		n = cap
	}
	return n
}

// AlphaEma is an exponential moving average of speculative acceptance per key
// (tenant or session), fed from the engine's (drafted, accepted) counters.
// Safe for concurrent use.
type AlphaEma struct {
	mu        sync.Mutex
	smoothing float64
	warmup    int
	ema       map[string]float64
	n         map[string]int
}

// NewAlphaEma mirrors the prototype defaults: smoothing 0.3, warmup 3.
func NewAlphaEma(smoothing float64, warmup int) (*AlphaEma, error) {
	if smoothing <= 0.0 || smoothing > 1.0 {
		return nil, fmt.Errorf("smoothing must be in (0,1]: %v", smoothing)
	}
	return &AlphaEma{smoothing: smoothing, warmup: warmup,
		ema: map[string]float64{}, n: map[string]int{}}, nil
}

func (e *AlphaEma) Update(key string, drafted, accepted int) {
	if drafted <= 0 {
		return
	}
	rate := float64(accepted) / float64(drafted)
	if rate < 0 {
		rate = 0
	}
	if rate > 1 {
		rate = 1
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if prev, ok := e.ema[key]; ok {
		e.ema[key] = e.smoothing*rate + (1-e.smoothing)*prev
	} else {
		e.ema[key] = rate
	}
	e.n[key]++
}

// Get returns the smoothed alpha for key, or nil until warmed up.
func (e *AlphaEma) Get(key string) *float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.n[key] < e.warmup {
		return nil
	}
	if v, ok := e.ema[key]; ok {
		return &v
	}
	return nil
}

// ── 2. prefix routing: slot / replica affinity via consistent hashing ───────

// PrefixKey is the stable cache key for the shared part of a request.
// Canonicalizes first so incidental whitespace differences between tenants'
// "same" system prompt do not split the key.
func PrefixKey(systemPrompt, tenant string) string {
	canon := CanonicalizePrompt(systemPrompt)
	material := tenant + "\x00" + canon
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])
}

// Ring is a minimal consistent-hash ring mapping a key to one of its members
// (slots or replicas). vnodes virtual points per member smooth the
// distribution. Deterministic across processes and restarts, and hash-value
// compatible with the Python prototype (same sha256-prefix construction) so a
// rolling port does not reshuffle the prefix cache.
type Ring struct {
	mu      sync.Mutex
	vnodes  int
	ring    []ringPoint
	members []string
}

type ringPoint struct {
	h uint64
	m string
}

func NewRing(members []string, vnodes int) *Ring {
	if vnodes < 1 {
		vnodes = 1
	}
	r := &Ring{vnodes: vnodes}
	for _, m := range members {
		r.Add(m)
	}
	return r
}

func ringHash(s string) uint64 {
	sum := sha256.Sum256([]byte(s))
	return binary.BigEndian.Uint64(sum[:8]) // == int(hexdigest()[:16], 16)
}

func (r *Ring) Add(member string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.members {
		if m == member {
			return
		}
	}
	r.members = append(r.members, member)
	for v := 0; v < r.vnodes; v++ {
		r.ring = append(r.ring, ringPoint{ringHash(fmt.Sprintf("%s#%d", member, v)), member})
	}
	sort.Slice(r.ring, func(i, j int) bool { return r.ring[i].h < r.ring[j].h })
}

func (r *Ring) Remove(member string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.members[:0]
	for _, m := range r.members {
		if m != member {
			out = append(out, m)
		}
	}
	r.members = out
	pts := r.ring[:0]
	for _, p := range r.ring {
		if p.m != member {
			pts = append(pts, p)
		}
	}
	r.ring = pts
}

// Route returns the member owning key (first ring point clockwise).
func (r *Ring) Route(key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ring) == 0 {
		return "", fmt.Errorf("ring has no members")
	}
	h := ringHash(key)
	idx := sort.Search(len(r.ring), func(i int) bool { return r.ring[i].h >= h })
	if idx == len(r.ring) {
		idx = 0
	}
	return r.ring[idx].m, nil
}

// ── 3. canonical prompt assembly ────────────────────────────────────────────

// CanonicalizePrompt normalizes a prompt for stable prefix matching: unify
// newlines, strip trailing whitespace per line, collapse blank-line runs,
// strip leading/trailing blank lines. Content-preserving.
func CanonicalizePrompt(text string) string {
	t := strings.ReplaceAll(text, "\r\n", "\n")
	t = strings.ReplaceAll(t, "\r", "\n")
	lines := strings.Split(t, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t\v\f")
		if ln == "" {
			blank++
			if blank <= 1 {
				out = append(out, ln)
			}
		} else {
			blank = 0
			out = append(out, ln)
		}
	}
	return strings.Trim(strings.Join(out, "\n"), "\n")
}
