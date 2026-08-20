package gateway

import "testing"

func TestEstimateTokens(t *testing.T) {
	if EstimateTokens("", nil) != 0 {
		t.Fatal("empty -> 0")
	}
	long := make([]byte, 4000)
	for i := range long {
		long[i] = 'x'
	}
	if got := EstimateTokens(string(long), nil); got != 1000 {
		t.Fatalf("4000 chars -> %d; want 1000", got)
	}
	// runes, not bytes: 400 two-byte runes = 100 tokens
	nn := ""
	for i := 0; i < 400; i++ {
		nn += "ñ"
	}
	if got := EstimateTokens(nn, nil); got != 100 {
		t.Fatalf("400 runes -> %d; want 100", got)
	}
	if got := EstimateTokens("whatever", func(string) int { return 7 }); got != 7 {
		t.Fatalf("injected tokenizer -> %d; want 7", got)
	}
	if got := EstimateTokens("whatever", func(string) int { return -3 }); got != 0 {
		t.Fatalf("negative tokenizer clamped -> %d; want 0", got)
	}
}

func TestKnownPrefixes(t *testing.T) {
	if _, err := NewKnownPrefixes(0); err == nil {
		t.Fatal("capacity 0 must error")
	}
	kp, _ := NewKnownPrefixes(2)
	if kp.HotTokens("nope") != 0 {
		t.Fatal("unknown -> 0")
	}
	kp.Record("a", 1)
	kp.Record("b", 2)
	kp.Record("c", 3) // evicts a
	if kp.HotTokens("a") != 0 || kp.HotTokens("b") != 2 || kp.HotTokens("c") != 3 {
		t.Fatal("LRU eviction wrong")
	}
	// touch refreshes order: b becomes most-recent, d evicts c
	kp.HotTokens("b")
	kp.Record("d", 4)
	if kp.HotTokens("b") != 2 || kp.HotTokens("c") != 0 {
		t.Fatal("touch must refresh LRU order")
	}
	// re-record updates value without eviction
	kp.Record("b", 9)
	if kp.HotTokens("b") != 9 {
		t.Fatal("re-record must update tokens")
	}
}

func TestClassifyAdmission(t *testing.T) {
	d := ClassifyAdmission(AdmissionInput{PrefixTokens: 3000, TailTokens: 500,
		PrefillAvailable: true})
	if d.Route != "prefill" || d.Reason != "large-new-prefill" || d.EstNewTokens != 3500 {
		t.Fatalf("large: %+v", d)
	}
	d = ClassifyAdmission(AdmissionInput{PrefixTokens: 200, TailTokens: 100,
		PrefillAvailable: true})
	if d.Route != "decode" || d.Reason != "small-new-prefill" {
		t.Fatalf("small: %+v", d)
	}
	d = ClassifyAdmission(AdmissionInput{PrefixTokens: 5000, TailTokens: 300,
		HotPrefixTokens: 5000, PrefillAvailable: true})
	if d.Route != "decode" || d.Reason != "prefix-hot" || d.EstNewTokens != 300 {
		t.Fatalf("hot: %+v", d)
	}
	d = ClassifyAdmission(AdmissionInput{PrefixTokens: 5000, TailTokens: 4000,
		HotPrefixTokens: 5000, PrefillAvailable: true})
	if d.Route != "prefill" {
		t.Fatalf("hot prefix + huge tail must still reroute: %+v", d)
	}
	d = ClassifyAdmission(AdmissionInput{PrefixTokens: PrefillThresholdTokens,
		PrefillAvailable: true})
	if d.Route != "prefill" {
		t.Fatalf("exact threshold must reroute: %+v", d)
	}
	d = ClassifyAdmission(AdmissionInput{PrefixTokens: 90000, TailTokens: 90000,
		PrefillAvailable: false})
	if d.Route != "decode" || d.Reason != "prefill-unavailable" {
		t.Fatalf("unavailable must degrade: %+v", d)
	}
	// partial hot prefix counts only the delta: 1000 new + 500 tail < 2048
	d = ClassifyAdmission(AdmissionInput{PrefixTokens: 4000, TailTokens: 500,
		HotPrefixTokens: 3000, PrefillAvailable: true})
	if d.Route != "decode" || d.EstNewTokens != 1500 {
		t.Fatalf("partial hot: %+v", d)
	}
	// hot tokens exceeding prefix must not go negative
	d = ClassifyAdmission(AdmissionInput{PrefixTokens: 100, TailTokens: 50,
		HotPrefixTokens: 5000, PrefillAvailable: true})
	if d.EstNewTokens != 50 {
		t.Fatalf("negative delta clamped: %+v", d)
	}
}
