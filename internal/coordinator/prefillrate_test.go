package coordinator

import (
	"math"
	"testing"
)

// A prefill is three passes: the stable prefix, up to the end of the last
// message, and the few tokens left. The rate the cost model learns must be the
// rate of the whole thing. The numbers are the shape of ring row 10368
// (2026-11): ~21 000 tokens in ~8.9 s, with a last pass of 7 tokens.
func TestPrefillRateIsTheWholePrefillNotTheLastPass(t *testing.T) {
	pass := func(n, ms float64) map[string]any {
		return map[string]any{"timings": map[string]any{
			"prompt_n": n, "prompt_ms": ms, "prompt_per_second": n / ms * 1000}}
	}
	passes := []map[string]any{pass(3000, 1250), pass(18419, 7500), pass(7, 150.5)}

	last := passes[2]["timings"].(map[string]any)["prompt_per_second"].(float64)
	if last > 50 {
		t.Fatalf("control: the last pass alone must look slow (got %.1f tok/s), or this test proves nothing", last)
	}
	pp, n := prefillRate(passes)
	if n != 21426 {
		t.Fatalf("tokens: got %d, want 21426", n)
	}
	if want := 21426 / 8.9005; math.Abs(pp-want) > 1 {
		t.Fatalf("rate: got %.1f tok/s, want %.1f — and never the last pass's %.1f", pp, want, last)
	}
}

// An engine that reports only the rate still yields a time; a pass with no
// timings at all contributes nothing instead of zeroing the result.
func TestPrefillRateToleratesMissingFields(t *testing.T) {
	passes := []map[string]any{
		{"timings": map[string]any{"prompt_n": 4000.0, "prompt_per_second": 2000.0}}, // 2 s
		{"content": ""}, // a failed or silent pass
		{"timings": map[string]any{"prompt_n": 2000.0, "prompt_ms": 1000.0}},
	}
	pp, n := prefillRate(passes)
	if n != 6000 || math.Abs(pp-2000) > 0.5 {
		t.Fatalf("got %.1f tok/s over %d tokens, want 2000 over 6000", pp, n)
	}
	if pp, n := prefillRate(nil); pp != 0 || n != 0 {
		t.Fatalf("no passes must give zero, got %.1f / %d", pp, n)
	}
}
