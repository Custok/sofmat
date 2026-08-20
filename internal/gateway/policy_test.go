package gateway

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func TestNMaxForAlphaBoundsAndSteps(t *testing.T) {
	if _, err := NMaxForAlpha(-0.1); err == nil {
		t.Fatal("negative alpha must error")
	}
	if _, err := NMaxForAlpha(1.1); err == nil {
		t.Fatal("alpha > 1 must error")
	}
	cases := map[float64]int{0.0: 2, 0.39: 2, 0.54: 2, 0.55: 3, 0.96: 3, 1.0: 3}
	for a, want := range cases {
		got, err := NMaxForAlpha(a)
		if err != nil || got != want {
			t.Fatalf("NMaxForAlpha(%v) = %d, %v; want %d", a, got, err, want)
		}
	}
}

func TestNMaxMonotone(t *testing.T) {
	prev := 0
	for a := 0.0; a <= 1.0; a += 0.05 {
		n, _ := NMaxForAlpha(a)
		if n < prev {
			t.Fatalf("not monotone at alpha=%v", a)
		}
		if n < NMaxFloor || n > NMaxCap {
			t.Fatalf("out of guardrail at alpha=%v: %d", a, n)
		}
		prev = n
	}
}

func TestClassifyDomain(t *testing.T) {
	cases := map[string]string{
		"def foo(x):\n  return x":          "code",
		"```python\nprint(1)\n```":         "code",
		"la latency del pipeline es clave": "technical",
		"el protocolo TCP usa ventanas":    "technical",
		"cuéntame un cuento de dragones":   "prose",
		"":                                 "prose",
	}
	for prompt, want := range cases {
		if got := ClassifyDomain(prompt); got != want {
			t.Fatalf("ClassifyDomain(%q) = %q; want %q", prompt, got, want)
		}
	}
}

func TestNextWaveNMax(t *testing.T) {
	high := 0.9
	low := 0.2
	code := "def f():\n  pass"
	prose := "un poema sobre el mar"
	if n := NextWaveNMax(&high, nil, 0); n != 3 {
		t.Fatalf("alpha 0.9 -> %d; want 3", n)
	}
	if n := NextWaveNMax(&low, nil, 0); n != 2 {
		t.Fatalf("alpha 0.2 -> %d; want 2", n)
	}
	if n := NextWaveNMax(nil, &code, 0); n != 3 {
		t.Fatalf("code domain -> %d; want 3", n)
	}
	if n := NextWaveNMax(nil, &prose, 0); n != 2 {
		t.Fatalf("prose domain -> %d; want 2", n)
	}
	if n := NextWaveNMax(nil, nil, 0); n != 3 {
		t.Fatalf("neutral default (technical 0.59) -> %d; want 3", n)
	}
	if n := NextWaveNMax(&high, nil, 2); n != 2 {
		t.Fatalf("caller cap 2 -> %d; want 2", n)
	}
}

func TestAlphaEmaWarmupAndSmoothing(t *testing.T) {
	e, err := NewAlphaEma(0.3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if e.Get("k") != nil {
		t.Fatal("cold key must be nil")
	}
	e.Update("k", 10, 9)
	e.Update("k", 10, 9)
	if e.Get("k") != nil {
		t.Fatal("below warmup must be nil")
	}
	e.Update("k", 10, 9)
	v := e.Get("k")
	if v == nil || *v < 0.89 || *v > 0.91 {
		t.Fatalf("warmed ema = %v; want ~0.9", v)
	}
	// drafted <= 0 ignored
	e.Update("k", 0, 5)
	if v2 := e.Get("k"); *v2 != *v {
		t.Fatal("drafted<=0 must not move the ema")
	}
	// rate clamped to [0,1]
	e.Update("clamp", 10, 20)
	e.Update("clamp", 10, 20)
	e.Update("clamp", 10, 20)
	if c := e.Get("clamp"); c == nil || *c > 1.0 {
		t.Fatalf("rate not clamped: %v", c)
	}
	if _, err := NewAlphaEma(0.0, 3); err == nil {
		t.Fatal("smoothing 0 must error")
	}
}

func TestRingPythonParity(t *testing.T) {
	// Values computed with the Python prototype (behavioral contract): the Go
	// ring must route identically so the port does not reshuffle the cache.
	if h := ringHash("0#0"); h != 12347569217287482404 {
		t.Fatalf("ringHash('0#0') = %d", h)
	}
	if h := ringHash("foo"); h != 3181428560199927439 {
		t.Fatalf("ringHash('foo') = %d", h)
	}
	r := NewRing([]string{"0", "1", "2", "3"}, 64)
	if m, _ := r.Route("k1"); m != "2" {
		t.Fatalf("Route(k1) = %s; python prototype says 2", m)
	}
	if m, _ := r.Route("hello-world"); m != "3" {
		t.Fatalf("Route(hello-world) = %s; python prototype says 3", m)
	}
}

func TestRingDeterministicAndStableUnderResize(t *testing.T) {
	r1 := NewRing([]string{"a", "b", "c"}, 64)
	r2 := NewRing([]string{"a", "b", "c"}, 64)
	moved := 0
	total := 500
	before := make([]string, total)
	for i := 0; i < total; i++ {
		k := "key-" + strconv.Itoa(i)
		m1, _ := r1.Route(k)
		m2, _ := r2.Route(k)
		if m1 != m2 {
			t.Fatal("ring not deterministic")
		}
		before[i] = m1
	}
	r1.Add("d")
	for i := 0; i < total; i++ {
		m, _ := r1.Route("key-" + strconv.Itoa(i))
		if m != before[i] {
			moved++
		}
	}
	// adding 1 of 4 members should remap roughly 1/4 of keys, never most.
	if moved == 0 || moved > total/2 {
		t.Fatalf("resize remapped %d/%d keys", moved, total)
	}
	r1.Remove("d")
	for i := 0; i < total; i++ {
		m, _ := r1.Route("key-" + strconv.Itoa(i))
		if m != before[i] {
			t.Fatal("remove did not restore the original map")
		}
	}
}

func TestRingEmptyErrors(t *testing.T) {
	r := NewRing(nil, 64)
	if _, err := r.Route("x"); err == nil {
		t.Fatal("empty ring must error")
	}
}

func TestCanonicalizePrompt(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hola\r\nmundo\r\n", "hola\nmundo"},
		{"a  \nb\t\n", "a\nb"},
		{"\n\n\na\n\n\n\nb\n\n\n", "a\n\nb"},
		{"", ""},
		{"solo", "solo"},
	}
	for _, c := range cases {
		if got := CanonicalizePrompt(c.in); got != c.want {
			t.Fatalf("CanonicalizePrompt(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestPrefixKeyPythonParityAndTenantSeparation(t *testing.T) {
	if k := PrefixKey("Eres un asistente", "t1"); k != "5a1788bea2ec31fbaa4135b51003e186b76f2df4c696398ab8419963e1779a1f" {
		t.Fatalf("PrefixKey python parity broken: %s", k)
	}
	if PrefixKey("same", "t1") == PrefixKey("same", "t2") {
		t.Fatal("tenants must not share keys")
	}
	// incidental whitespace must not split the key
	if PrefixKey("Eres un asistente \n", "t1") != PrefixKey("Eres un asistente", "t1") {
		t.Fatal("canonicalization must absorb trailing whitespace")
	}
}

func TestRequestLogBoundedAndOrdered(t *testing.T) {
	l := NewRequestLog(3, false)
	for i := 0; i < 5; i++ {
		l.RecordEntry(Record{"i": i}, nil)
	}
	if l.Len() != 3 {
		t.Fatalf("len = %d; want 3", l.Len())
	}
	tail := l.Tail(10)
	if len(tail) != 3 || tail[0]["i"] != 2 || tail[2]["i"] != 4 {
		t.Fatalf("tail wrong: %v", tail)
	}
	if tail[2]["id"] != 5 {
		t.Fatalf("ids must keep counting: %v", tail[2]["id"])
	}
}

func TestRequestLogContentOptIn(t *testing.T) {
	off := NewRequestLog(10, false)
	rec := off.RecordEntry(Record{"a": 1}, Record{"prompt": "secreto"})
	if _, ok := rec["content"]; ok {
		t.Fatal("content must be dropped when keepContent=false")
	}
	on := NewRequestLog(10, true)
	rec = on.RecordEntry(Record{"a": 1}, Record{"prompt": "x"})
	if _, ok := rec["content"]; !ok {
		t.Fatal("content must be kept when keepContent=true")
	}
}

func TestStatusAggregationFailSoft(t *testing.T) {
	nodes := []NodeRef{{"n1", "u1"}, {"n2", "u2"}, {"n3", "u3"}}
	fetch := func(name, url string) (map[string]any, error) {
		if name == "n2" {
			return nil, fmt.Errorf("boom")
		}
		return map[string]any{"cpu": 10, "unexpected": "dropped"}, nil
	}
	doc := BuildStatus(nodes, fetch, nil, map[string]any{"quant": "q8"})
	if doc["nodes_up"] != 2 || doc["nodes_total"] != 3 {
		t.Fatalf("up/total wrong: %v/%v", doc["nodes_up"], doc["nodes_total"])
	}
	ns := doc["nodes"].([]map[string]any)
	if ns[1]["up"] != false || !strings.Contains(ns[1]["error"].(string), "boom") {
		t.Fatalf("down node not fail-soft: %v", ns[1])
	}
	if _, ok := ns[0]["unexpected"]; ok {
		t.Fatal("agent contract must be sanitized")
	}
	if ns[0]["cpu"] != 10 {
		t.Fatal("known agent field lost")
	}
	if doc["serving"].(map[string]any)["quant"] != "q8" {
		t.Fatal("serving summary lost")
	}
}
