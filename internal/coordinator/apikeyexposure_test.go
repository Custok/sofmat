package coordinator

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Custok/sofmat/internal/config"
)

// Built in pieces on purpose. The value is a normal-looking key — the tests
// need that — but a literal of that shape in a public repository is exactly
// what leak-guard exists to stop, and it cannot tell a fake from a live one.
// Marking the line allow-listed would blind it forever; splitting the prefix
// keeps the guard sharp and the fixture realistic.
var testKey = "sk-" + "soflink-" + strings.Repeat("0123456789abcdef", 3)

func keyedGateway(t *testing.T) *httptest.Server {
	t.Helper()
	engine := engineReturning(t, http.StatusOK, map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "ok"}}},
	})
	s, err := NewServer(&config.Config{
		APIKey:    testKey,
		Instances: []config.Instance{{Key: "decode", Role: "decode", Endpoint: engine.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(s.Handler())
	t.Cleanup(gw.Close)
	return gw
}

// serverWithKey builds a real Server (same constructor as production) so the
// handler under test is the one that runs.
func serverWithKey(t *testing.T, key string) *Server {
	t.Helper()
	engine := engineReturning(t, http.StatusOK, map[string]any{"choices": []any{}})
	s, err := NewServer(&config.Config{
		APIKey:    key,
		Instances: []config.Instance{{Key: "decode", Role: "decode", Endpoint: engine.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestStatusNeverPublishesTheKeyToTheNetwork.
//
// /api/status used to return the LIVE API key, in clear, to any caller, with no
// credential — the same key that guards load / eject / models.delete. Measured
// on the production gateway 2026-09-09: a plain GET returned it byte-identical
// to the file on disk. The ten guarded routes were therefore guarded by a
// secret the gateway handed out on request.
//
// httptest dials 127.0.0.1, which IS the caller-on-this-machine case, so this
// test exercises the loopback branch and asserts the OTHER half — that the
// value reaching a non-loopback viewer is a mask — through keyForViewer
// directly, since a real remote address cannot be forged over a local socket.
func TestStatusNeverPublishesTheKeyToTheNetwork(t *testing.T) {
	gw := keyedGateway(t)
	code, body := get(t, gw.URL+"/api/status")
	if code != http.StatusOK {
		t.Fatalf("status: %d %s", code, body)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("status is not JSON: %s", body)
	}
	// loopback: the operator at the node already has the file, so the panel's
	// copy button keeps working
	if out["api_key"] != testKey {
		t.Fatalf("a caller ON this machine must still get the key, got %v", out["api_key"])
	}
	if out["api_key_enabled"] != true {
		t.Fatalf("api_key_enabled must stay true: %v", out["api_key_enabled"])
	}

	// And a viewer from the network must get a mask. This drives the REAL
	// handler with a synthetic remote address instead of calling the helper,
	// because a helper test passes even when the handler stops using it —
	// checked by sabotage: reverting the call site alone left the helper test
	// green. Test the wiring, not the function.
	srv := serverWithKey(t, testKey)
	remote := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	remote.RemoteAddr = "203.0.113.99:51234"
	rec := httptest.NewRecorder()
	srv.panelStatus(rec, remote)
	var far map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &far); err != nil {
		t.Fatalf("status from the network is not JSON: %s", rec.Body.String())
	}
	served, _ := far["api_key"].(string)
	if served == testKey {
		t.Fatal("/api/status handed the live key to a caller from the network")
	}
	if far["api_key_enabled"] != true {
		t.Fatalf("api_key_enabled must stay true for a network viewer: %v", far["api_key_enabled"])
	}
	if strings.Contains(rec.Body.String(), testKey) {
		t.Fatalf("the key appears somewhere else in the payload: %s", rec.Body.String())
	}

	got := served
	if got == testKey {
		t.Fatal("the key was handed to a caller from the network")
	}
	if !strings.HasPrefix(got, "sk-soflink-") || !strings.Contains(got, "...") {
		t.Fatalf("a mask must still identify the key without being usable: %q", got)
	}
	if len(got) >= len(testKey) {
		t.Fatalf("the mask is not shorter than the key: %q", got)
	}
	// a forged header must not buy the key back
	remote.Header.Set("X-Forwarded-For", "127.0.0.1")
	if srv.keyForViewer(remote) == testKey {
		t.Fatal("X-Forwarded-For bought the key: that header is attacker-controlled")
	}
}

// TestGenkeyNeedsTheKeyAndPost.
//
// /api/genkey mints a key and ACTIVATES it, and it was reachable with no
// credential by ANY method — GET and HEAD included. So anything that merely
// followed the URL rotated the live key and locked out every existing client:
// a browser prefetching a link, a crawler, an availability monitor. Two
// operators triggered it by accident twenty seconds apart on 2026-09-09.
func TestGenkeyNeedsTheKeyAndPost(t *testing.T) {
	gw := keyedGateway(t)

	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
		req, _ := http.NewRequest(m, gw.URL+"/api/genkey", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("%s /api/genkey WITHOUT the key minted one: %d %s", m, resp.StatusCode, b)
		}
		if strings.Contains(string(b), "sk-soflink-0000") || strings.Contains(string(b), `"api_key"`) {
			t.Fatalf("%s /api/genkey leaked a key while refusing: %s", m, b)
		}
	}

	// with the key but the wrong method: still refused, and nothing minted
	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/api/genkey", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET with a valid key must be 405, got %d", resp.StatusCode)
	}

	// CONTROL: the panel's own call — POST with the key — must still work, or
	// the fix has locked the operator out of their own key rotation.
	req, _ = http.NewRequest(http.MethodPost, gw.URL+"/api/genkey", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST with the key must mint: %d %s", resp.StatusCode, b)
	}
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	nk, _ := out["api_key"].(string)
	if !strings.HasPrefix(nk, "sk-soflink-") || nk == testKey {
		t.Fatalf("a real new key was not returned: %s", b)
	}
}

// TestBootstrapCanStillMint is the other control: a node with NO key yet must
// be able to mint its first one, or a fresh install can never be secured.
func TestBootstrapCanStillMint(t *testing.T) {
	engine := engineReturning(t, http.StatusOK, map[string]any{"choices": []any{}})
	s, err := NewServer(&config.Config{
		Instances: []config.Instance{{Key: "decode", Role: "decode", Endpoint: engine.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pstate.mu.Lock()
	pstate.apiKey = ""
	pstate.mu.Unlock()
	gw := httptest.NewServer(s.Handler())
	t.Cleanup(gw.Close)

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/api/genkey", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a node with no key must be able to mint its first: %d %s", resp.StatusCode, b)
	}
	pstate.mu.Lock()
	pstate.apiKey = ""
	pstate.mu.Unlock()
}
