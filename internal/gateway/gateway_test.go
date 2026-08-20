package gateway

import (
	"errors"
	"strings"
	"testing"
)

var bigPrompt = strings.Repeat("x", 20000) // ~5000 est. tokens > threshold

func okResp() Body {
	return Body{"choices": []any{map[string]any{"ok": true}}, "timings": map[string]any{}}
}

type calls struct {
	prefill []Headers
	decode  []Headers
	handoff []string
}

func newTestGW(t *testing.T, mutate func(*Options)) (*Gateway, *calls) {
	t.Helper()
	c := &calls{}
	o := Options{
		Verify: func(Headers) bool { return true },
		BackendCall: func(body Body, extra Headers) (Body, error) {
			c.decode = append(c.decode, extra)
			return okResp(), nil
		},
		StatusProvider: func() Body { return Body{} },
		PrefillCall: func(body Body, extra Headers) (Body, error) {
			c.prefill = append(c.prefill, extra)
			return Body{"handoff_id": "h-1"}, nil
		},
		Handoff: func(hid, slot string) bool {
			c.handoff = append(c.handoff, hid)
			return true
		},
	}
	if mutate != nil {
		mutate(&o)
	}
	gw, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return gw, c
}

func chatBody(system, user string) Body {
	return Body{"messages": []any{
		map[string]any{"role": "system", "content": system},
		map[string]any{"role": "user", "content": user},
	}}
}

func TestAuthRejected(t *testing.T) {
	gw, _ := newTestGW(t, func(o *Options) {
		o.Verify = func(Headers) bool { return false }
	})
	if _, err := gw.Chat(Headers{}, chatBody("s", "u")); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
	if _, err := gw.Status(Headers{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("status must auth")
	}
	if _, err := gw.Requests(Headers{}, 5); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("requests must auth")
	}
}

func TestLargePromptGoesThroughPrefillAndHandoff(t *testing.T) {
	gw, c := newTestGW(t, nil)
	if _, err := gw.Chat(Headers{}, chatBody(bigPrompt, "hola")); err != nil {
		t.Fatal(err)
	}
	if len(c.prefill) != 1 || len(c.handoff) != 1 || c.handoff[0] != "h-1" {
		t.Fatalf("prefill/handoff not driven: %+v", c)
	}
	if c.decode[0]["x-sofmat-kv-handoff"] != "h-1" {
		t.Fatal("decode must receive the handoff marker")
	}
	if c.decode[0]["x-sofmat-slot"] == "" {
		t.Fatal("decode must receive a slot")
	}
}

func TestSecondRequestSamePrefixIsDecodeDirect(t *testing.T) {
	gw, c := newTestGW(t, nil)
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	gw.Chat(Headers{}, chatBody(bigPrompt, "otra pregunta corta"))
	if len(c.prefill) != 1 {
		t.Fatalf("prefix hot: second prefill must not happen (%d)", len(c.prefill))
	}
	if len(c.decode) != 2 {
		t.Fatalf("both requests must reach decode (%d)", len(c.decode))
	}
	if _, ok := c.decode[1]["x-sofmat-kv-handoff"]; ok {
		t.Fatal("second request must not carry a handoff marker")
	}
}

func TestSmallPromptNeverTouchesPrefill(t *testing.T) {
	gw, c := newTestGW(t, nil)
	gw.Chat(Headers{}, chatBody("corto", "hola"))
	if len(c.prefill) != 0 || len(c.decode) != 1 {
		t.Fatalf("small prompt routing wrong: %+v", c)
	}
}

func TestNoPrefillConfiguredDegradesSilently(t *testing.T) {
	gw, c := newTestGW(t, func(o *Options) {
		o.PrefillCall = nil
		o.Handoff = nil
	})
	resp, err := gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if err != nil || resp == nil {
		t.Fatalf("must degrade, not fail: %v", err)
	}
	if len(c.decode) != 1 {
		t.Fatal("decode must still be called")
	}
}

func TestPrefillErrorFallsBackToDecode(t *testing.T) {
	gw, c := newTestGW(t, func(o *Options) {
		o.PrefillCall = func(Body, Headers) (Body, error) {
			return nil, errors.New("prefill node down")
		}
	})
	resp, err := gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if err != nil || resp == nil {
		t.Fatalf("request must survive: %v", err)
	}
	if _, ok := c.decode[0]["x-sofmat-kv-handoff"]; ok {
		t.Fatal("failed prefill must not mark a handoff")
	}
}

func TestPrefillPanicFallsBackToDecode(t *testing.T) {
	gw, c := newTestGW(t, func(o *Options) {
		o.PrefillCall = func(Body, Headers) (Body, error) { panic("boom") }
	})
	resp, err := gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if err != nil || resp == nil {
		t.Fatalf("request must survive a panic: %v", err)
	}
	if len(c.decode) != 1 {
		t.Fatal("decode must still run")
	}
}

func TestHandoffFalseFallsBackToDecode(t *testing.T) {
	gw, c := newTestGW(t, func(o *Options) {
		o.Handoff = func(string, string) bool { return false }
	})
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	if _, ok := c.decode[0]["x-sofmat-kv-handoff"]; ok {
		t.Fatal("failed handoff must not mark the decode call")
	}
}

func TestAdmissionMetricsLogged(t *testing.T) {
	gw, _ := newTestGW(t, nil)
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	rows, _ := gw.Requests(Headers{}, 1)
	last := rows[len(rows)-1]
	if last["admission"] != "large-new-prefill" || last["admitted_via"] != "prefill" {
		t.Fatalf("metrics wrong: %v", last)
	}
}

func TestFallbackVisibleInMetrics(t *testing.T) {
	gw, _ := newTestGW(t, func(o *Options) {
		o.Handoff = func(string, string) bool { return false }
	})
	gw.Chat(Headers{}, chatBody(bigPrompt, "hola"))
	rows, _ := gw.Requests(Headers{}, 1)
	if rows[len(rows)-1]["admitted_via"] != "decode-fallback" {
		t.Fatalf("fallback must be visible: %v", rows[len(rows)-1])
	}
}

func TestNextWaveDefaultAndCallerOverride(t *testing.T) {
	var seen Body
	gw, _ := newTestGW(t, func(o *Options) {
		o.BackendCall = func(body Body, extra Headers) (Body, error) {
			seen = body
			return okResp(), nil
		}
	})
	gw.Chat(Headers{}, chatBody("s", "def f():\n  pass"))
	if seen[SpeculativeNMaxKey] != 3 {
		t.Fatalf("code prompt must default n_max 3: %v", seen[SpeculativeNMaxKey])
	}
	body := chatBody("s", "def f():\n  pass")
	body[SpeculativeNMaxKey] = 4
	gw.Chat(Headers{}, body)
	if seen[SpeculativeNMaxKey] != 4 {
		t.Fatal("caller-set n_max must not be overridden")
	}
}

func TestAlphaEmaFedFromTimings(t *testing.T) {
	n := 0
	gw, _ := newTestGW(t, func(o *Options) {
		o.BackendCall = func(body Body, extra Headers) (Body, error) {
			n++
			return Body{"timings": map[string]any{
				"draft_n": float64(10), "draft_n_accepted": float64(3)}}, nil
		}
	})
	h := Headers{"x-sofmat-tenant": "t1"}
	for i := 0; i < 4; i++ { // warmup 3, then the live alpha drives n_max
		gw.Chat(h, chatBody("s", "def f():\n  pass"))
	}
	rows, _ := gw.Requests(Headers{}, 1)
	last := rows[len(rows)-1]
	// alpha ~0.3 < 0.55 -> n_max 2 despite the code-domain prompt
	if last["n_max"] != 2 {
		t.Fatalf("live alpha must supersede domain guess: %v", last)
	}
	if last["alpha_ema"] == nil {
		t.Fatal("alpha_ema must be logged once warmed")
	}
}

func TestSlotAffinityDeterministic(t *testing.T) {
	gw, c := newTestGW(t, nil)
	gw.Chat(Headers{}, chatBody("mismo prefijo", "a"))
	gw.Chat(Headers{}, chatBody("mismo prefijo", "b"))
	if c.decode[0]["x-sofmat-slot"] != c.decode[1]["x-sofmat-slot"] {
		t.Fatal("same prefix must pin to the same slot")
	}
}

func TestBackendErrorPropagates(t *testing.T) {
	gw, _ := newTestGW(t, func(o *Options) {
		o.BackendCall = func(Body, Headers) (Body, error) {
			return nil, errors.New("engine down")
		}
	})
	if _, err := gw.Chat(Headers{}, chatBody("s", "u")); err == nil {
		t.Fatal("backend errors must propagate")
	}
}
