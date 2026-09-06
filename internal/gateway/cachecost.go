package gateway

// Cache-aware admission + cost model for the disaggregation decision.
//
// lastPrompts remembers, per prefix key, the token ids of the last prompt the
// gateway routed: the decode slot that served it holds at least those tokens
// (plus what it generated), so the NEW work of the next turn is the part after
// the common prefix — not the whole prompt. Observed without it (2026-09-06,
// HUD agent chaining tool calls): every turn re-prefilled the full 34k prompt
// on the prefill node (18 s) while the decode held 22k of it and would have
// continued in ~1 s.
//
// costModel keeps EMAs of the engines' measured prompt-processing rates by
// prompt size and of the handoff cost per token, so ModeAuto can pick the
// faster path when the decode is idle: the decode's rate degrades with context
// (measured 27B Q6_K: ~2 200 tok/s at 11k, ~1 400 at 46k) while the prefill
// node holds ~1 800-2 000, so from ~30k the prefill path wins end-to-end even
// for a lone user (46k: 28 s vs 36 s direct).

import "sync"

// ── last prompts (cache lower bound) ────────────────────────────────────────

type lastPrompts struct {
	mu       sync.Mutex
	capacity int
	ids      map[string][]int
	order    []string // LRU, oldest first
}

func newLastPrompts(capacity int) *lastPrompts {
	if capacity <= 0 {
		capacity = 4
	}
	return &lastPrompts{capacity: capacity, ids: map[string][]int{}}
}

// commonPrefix returns how many leading tokens of ids match the last prompt
// routed under key (0 when unknown).
func (l *lastPrompts) commonPrefix(key string, ids []int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	prev, ok := l.ids[key]
	if !ok {
		return 0
	}
	n := len(prev)
	if len(ids) < n {
		n = len(ids)
	}
	i := 0
	for i < n && prev[i] == ids[i] {
		i++
	}
	return i
}

func (l *lastPrompts) remember(key string, ids []int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.ids[key]; !ok && len(l.ids) >= l.capacity {
		oldest := l.order[0]
		l.order = l.order[1:]
		delete(l.ids, oldest)
	}
	cp := make([]int, len(ids))
	copy(cp, ids)
	l.ids[key] = cp
	for i, o := range l.order {
		if o == key {
			l.order = append(l.order[:i], l.order[i+1:]...)
			break
		}
	}
	l.order = append(l.order, key)
}

func (l *lastPrompts) forget(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.ids[key]; !ok {
		return
	}
	delete(l.ids, key)
	for i, o := range l.order {
		if o == key {
			l.order = append(l.order[:i], l.order[i+1:]...)
			break
		}
	}
}

// ── cost model ──────────────────────────────────────────────────────────────

// size buckets for the prompt-processing rate (tokens): the rate falls with
// context length, so one EMA per bucket.
var costBuckets = []int{16384, 49152, 1 << 30}

type costModel struct {
	mu           sync.Mutex
	ppDecode     [3]float64 // tok/s by bucket (direct requests)
	ppPrefill    [3]float64 // tok/s by bucket (prefill node)
	handoffPerTk float64    // ms per token of save + fetch + restore
	handoffFixed float64    // ms
	seenDec      [3]bool
	seenPre      [3]bool
}

// defaults = the 2026-09-06 measurements (27B Q6_K, RTX 5080 pairs, 10GbE);
// overwritten by live observations as they come.
func newCostModel() *costModel {
	return &costModel{
		ppDecode:     [3]float64{2200, 1400, 1200},
		ppPrefill:    [3]float64{2050, 1830, 1440},
		handoffPerTk: 0.045, // ≈ 0.5 s at 11k, 2 s at 46k
		handoffFixed: 200,
	}
}

func bucketOf(tokens int) int {
	for i, b := range costBuckets {
		if tokens <= b {
			return i
		}
	}
	return len(costBuckets) - 1
}

// prefillCheaper compares the two paths for an idle decode: the decode only
// processes the NEW tokens (its slot holds the rest), the prefill node
// processes the whole prompt (its slot was erased) and the state has to be
// saved, shipped and restored.
func (c *costModel) prefillCheaper(total, newTokens int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if total <= 0 {
		return false
	}
	b := bucketOf(total)
	direct := float64(newTokens) / c.ppDecode[b] * 1000
	pre := float64(total)/c.ppPrefill[b]*1000 + c.handoffFixed + float64(total)*c.handoffPerTk
	return pre < direct
}

const costAlpha = 0.3

func ema(cur, obs float64, seen bool) float64 {
	if !seen || cur <= 0 {
		return obs
	}
	return cur + costAlpha*(obs-cur)
}

// observe feeds one finished request: a direct one measures the decode's
// prompt rate (only when it really processed a sizeable prompt), a handed-off
// one measures the prefill rate and the handoff cost per token.
func (c *costModel) observe(fields Record, resp Body, viaPrefill bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if viaPrefill {
		toks, _ := fields["tokens"].(int)
		if pp, ok := fields["prefill_pp"].(float64); ok && pp > 0 && toks > 0 {
			b := bucketOf(toks)
			c.ppPrefill[b] = ema(c.ppPrefill[b], pp, c.seenPre[b])
			c.seenPre[b] = true
		}
		var ms float64
		for _, k := range []string{"save_ms", "fetch_ms", "restore_ms"} {
			if v, ok := fields[k].(float64); ok {
				ms += v
			}
		}
		if ms > 0 && toks > 0 {
			per := (ms - c.handoffFixed) / float64(toks)
			if per > 0 {
				c.handoffPerTk = ema(c.handoffPerTk, per, true)
			}
		}
		return
	}
	pn, ok1 := timingInt(resp, "prompt_n")
	pms, ok2 := timingFloat(resp, "prompt_ms")
	if ok1 && ok2 && pn >= 2048 && pms > 0 {
		b := bucketOf(pn)
		c.ppDecode[b] = ema(c.ppDecode[b], float64(pn)/pms*1000, c.seenDec[b])
		c.seenDec[b] = true
	}
}
