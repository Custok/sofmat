package coordinator

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Custok/sofmat/internal/config"
)

// engineReturning stands up an engine that answers every chat with the given
// status and body — llama-server's real error shape.
func engineReturning(t *testing.T, status int, body map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/props") {
			writeTestJSON(w, 200, map[string]any{"default_generation_settings": map[string]any{"n_ctx": 100096.0}})
			return
		}
		writeTestJSON(w, status, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func gatewayOver(t *testing.T, engine *httptest.Server) *httptest.Server {
	t.Helper()
	s, err := NewServer(&config.Config{Instances: []config.Instance{
		{Key: "decode", Role: "decode", Endpoint: engine.URL},
	}})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(s.Handler())
	t.Cleanup(gw.Close)
	return gw
}

// TestEngineErrorKeepsItsStatus is the defect that produced, for days, the only
// message the VS Code clients ever showed: "Response contained no choices."
//
// httpBackend read the engine's body, unmarshalled it and returned (body, nil)
// WHATEVER the status was; s.chat then wrote 200. So a 400 from llama-server
// arrived as HTTP 200 with {"error":{"code":400,...}} and no "choices" key —
// and every client reported the absence of choices instead of the reason,
// which was sitting in the body. A 200 tells a client there is nothing to look
// for.
//
// The streaming half of this same gateway always forwarded resp.StatusCode.
// This pins the two halves together.
func TestEngineErrorKeepsItsStatus(t *testing.T) {
	engine := engineReturning(t, http.StatusBadRequest, map[string]any{
		"error": map[string]any{"code": 400, "type": "invalid_request_error",
			"message": "the request exceeds the available context size"},
	})
	gw := gatewayOver(t, engine)

	code, data := postChat(t, gw.URL, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hola " + strings.Repeat("x", 400)}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("the engine said 400 and the gateway said %d: an error the client cannot see as one\nbody: %s", code, data)
	}

	// and the reason must survive the trip: a correct status with an empty body
	// would trade one silence for another
	var out struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("body is not the engine's error object: %s", data)
	}
	if !strings.Contains(out.Error.Message, "context size") {
		t.Fatalf("the engine's reason did not reach the client: %q", out.Error.Message)
	}
}

// TestEngineOKStillPassesThrough is the control: a gateway that answered every
// call with the engine's status would pass the test above while breaking every
// working request. The instrument has to be able to say "200" too.
func TestEngineOKStillPassesThrough(t *testing.T) {
	engine := engineReturning(t, http.StatusOK, map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "ok"}}},
		"timings": map[string]any{"prompt_n": 10.0, "cache_n": 0.0},
	})
	gw := gatewayOver(t, engine)

	code, data := postChat(t, gw.URL, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hola " + strings.Repeat("y", 400)}},
	})
	if code != http.StatusOK {
		t.Fatalf("a good answer must stay 200, got %d: %s", code, data)
	}
	if !strings.Contains(string(data), `"choices"`) {
		t.Fatalf("the answer did not come through: %s", data)
	}
}

// TestEngineErrorThatIsNotJSONKeepsItsStatus covers the path that has no body
// to forward: llama-server aborting mid-header, a proxy returning plain text.
// Before the fix this returned the JSON parse error as a 502 and lost the
// engine's status entirely.
func TestEngineErrorThatIsNotJSONKeepsItsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/props") {
			writeTestJSON(w, 200, map[string]any{"default_generation_settings": map[string]any{"n_ctx": 100096.0}})
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("upstream connect error or disconnect/reset before headers"))
	}))
	t.Cleanup(srv.Close)
	gw := gatewayOver(t, srv)

	code, data := postChat(t, gw.URL, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hola " + strings.Repeat("z", 400)}},
	})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("engine said 503, gateway said %d: %s", code, data)
	}
	if !strings.Contains(string(data), "upstream connect error") {
		t.Fatalf("the upstream text did not reach the client: %s", data)
	}
}

// TestMuteStreamSaysWhy is the same defect as TestEngineErrorKeepsItsStatus,
// on the OTHER path — the one the VS Code clients actually use.
//
// An engine that answers 200 to a streamed request and then sends nothing left
// the client with an empty stream, and every OpenAI-compatible client reports
// the only thing it can see: "Response contained no choices". The gateway knew
// why (it recorded engine_bytes 0 and engine_read_error) and told the request
// log instead of the client. The status line is already on the wire by then,
// so the reason has to travel in the stream itself.
func TestMuteStreamSaysWhy(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/props") {
			writeTestJSON(w, 200, map[string]any{"default_generation_settings": map[string]any{"n_ctx": 100096.0}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// and then nothing at all: the shape measured in production as
		// engine_status 200 / engine_bytes 0 / unexpected EOF
	}))
	t.Cleanup(engine.Close)
	gw := gatewayOver(t, engine)

	_, data := postChat(t, gw.URL, map[string]any{
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hola " + strings.Repeat("w", 400)}},
	})
	body := string(data)
	if strings.TrimSpace(body) == "" {
		t.Fatal("the stream closed empty: the client can only report the absence of choices")
	}
	if !strings.Contains(body, `"finish_reason":"error"`) {
		t.Fatalf("no finish_reason error for the client to read as a failure: %s", body)
	}
	if !strings.Contains(body, "sin enviar nada") && !strings.Contains(body, "EOF") {
		t.Fatalf("the reason did not travel with it: %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("the stream was not terminated, a client would hang: %s", body)
	}
}

// TestGoodStreamIsUntouched is the control: a gateway that appended an error to
// every stream would pass the test above and corrupt every working answer.
func TestGoodStreamIsUntouched(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/props") {
			writeTestJSON(w, 200, map[string]any{"default_generation_settings": map[string]any{"n_ctx": 100096.0}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hola\"},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"timings\":{\"prompt_n\":10}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(engine.Close)
	gw := gatewayOver(t, engine)

	_, data := postChat(t, gw.URL, map[string]any{
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hola " + strings.Repeat("v", 400)}},
	})
	body := string(data)
	if strings.Contains(body, `"finish_reason":"error"`) {
		t.Fatalf("an error was appended to a perfectly good stream: %s", body)
	}
	if !strings.Contains(body, "hola") || !strings.Contains(body, `"stop"`) {
		t.Fatalf("the answer did not come through intact: %s", body)
	}
}

// captureLog swaps the standard logger's sink for the duration of a test.
// Tests in this package are not parallel, so the swap is safe.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return &buf
}

// TestAnomalyReachesTheLogFile: the engine_* notes only ever went into the
// in-memory request log, so the evidence for exactly the failures they exist to
// diagnose vanished at the next restart. It happened twice on 2026-09-09 — a
// gateway restarted to rotate the API key wiped its own log, and a node
// reported "0 engine_bytes ever" when the truth was "never recorded". A
// counter that cannot survive a restart cannot answer a question asked after
// one.
func TestAnomalyReachesTheLogFile(t *testing.T) {
	buf := captureLog(t)
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/props") {
			writeTestJSON(w, 200, map[string]any{"default_generation_settings": map[string]any{"n_ctx": 100096.0}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(engine.Close)
	gw := gatewayOver(t, engine)

	_, _ = postChat(t, gw.URL, map[string]any{
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hola " + strings.Repeat("u", 400)}},
	})
	if !strings.Contains(buf.String(), "chat-anomalia") {
		t.Fatalf("a request that produced nothing left no trace on disk:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "bytes=0") {
		t.Fatalf("the line does not carry the byte count, which is what separates the cases:\n%s", buf.String())
	}
}

// TestHealthyRequestIsQuiet is the control: a logger that fired on every
// request would pass the test above and bury the anomalies it exists to
// surface — which is precisely how the 990 processes stayed invisible.
func TestHealthyRequestIsQuiet(t *testing.T) {
	buf := captureLog(t)
	engine := engineReturning(t, http.StatusOK, map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "ok"}}},
		"timings": map[string]any{"prompt_n": 10.0, "cache_n": 0.0},
	})
	gw := gatewayOver(t, engine)
	code, _ := postChat(t, gw.URL, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hola " + strings.Repeat("t", 400)}},
	})
	if code != http.StatusOK {
		t.Fatalf("setup: expected 200, got %d", code)
	}
	if strings.Contains(buf.String(), "chat-anomalia") {
		t.Fatalf("a healthy request logged an anomaly; the signal is worthless if it always fires:\n%s", buf.String())
	}
}
