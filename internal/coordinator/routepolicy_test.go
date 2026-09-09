package coordinator

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Custok/sofmat/internal/config"
)

// mutatingRoutes is the list the 2026-09-09 review produced by ENUMERATING the
// route table instead of checking the ones already suspected. Every one of
// these changes state; several were reachable with no credential and by any
// method, including two URLs that reached the same eject as a guarded twin.
var mutatingRoutes = []string{
	"/api/eject", "/api/load", "/api/apply", "/api/apply-union",
	"/api/models/load", "/api/models/eject", "/api/models/delete",
	"/api/genkey", "/api/autoupdate", "/api/update", "/api/update/fleet",
	"/api/rename", "/api/selectinstance", "/api/setconfig",
	"/api/measure", "/api/hf/download",
	"/admin/eject", "/admin/load", "/config/apply",
}

// peerRoutes are node-to-node. They must refuse a stranger; loopback (which is
// what httptest dials) must still pass, or the KV handoff dies.
var peerRoutes = []string{
	"/control/eject", "/control/kill", "/control/load", "/control/kv-fetch",
	"/soflink/rename", "/kv/algo",
}

func try(t *testing.T, method, url string, key string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestEveryMutatingRouteNeedsKeyAndPost walks the whole list. A single route
// left out of the policy is a bypass of all the others: /api/eject asked for
// the key while /control/eject did not, and both reached the same eject.
func TestEveryMutatingRouteNeedsKeyAndPost(t *testing.T) {
	gw := keyedGateway(t)
	for _, route := range mutatingRoutes {
		// no credential, any method: must never act
		for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
			code, body := try(t, m, gw.URL+route, "")
			if code != http.StatusUnauthorized {
				t.Errorf("%s %s sin clave → %d (esperaba 401): %s", m, route, code, body)
			}
		}
		// with the key but a read method: refused, because a prefetch or a
		// crawler only ever issues GET and HEAD
		if code, body := try(t, http.MethodGet, gw.URL+route, testKey); code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s con clave → %d (esperaba 405): %s", route, code, body)
		}
	}
}

// TestMutatingRoutesStillWorkForThePanel is the control: a policy that refused
// everything would pass the test above and leave the operator unable to run
// their own node. The panel's real call is POST + the key.
func TestMutatingRoutesStillWorkForThePanel(t *testing.T) {
	gw := keyedGateway(t)
	// /api/selectinstance is chosen because it changes state and cannot damage
	// anything in a test rig; the point is that the policy LETS IT THROUGH.
	code, body := try(t, http.MethodPost, gw.URL+"/api/selectinstance", testKey)
	if code == http.StatusUnauthorized || code == http.StatusMethodNotAllowed {
		t.Fatalf("el panel no puede operar su propio nodo: %d %s", code, body)
	}
}

// TestPeerRoutesRefuseStrangersButNotPeers.
//
// These carry the KV handoff and the engine control between nodes, and the
// nodes send no credential to each other, so they cannot be behind the key.
// They were behind NOTHING: /control/kill freed a port by killing whatever
// held it, for anyone who asked.
func TestPeerRoutesRefuseStrangersButNotPeers(t *testing.T) {
	engine := engineReturning(t, http.StatusOK, map[string]any{"choices": []any{}})
	s, err := NewServer(&config.Config{
		APIKey: testKey,
		// RFC 5737 documentation address: a test must not encode the real fleet
		Nodes:     []config.Node{{ID: "node-c", Agent: "http://198.51.100.51:1357"}},
		Instances: []config.Instance{{Key: "decode", Role: "decode", Endpoint: engine.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(s.Handler())
	t.Cleanup(gw.Close)

	// a stranger is refused
	for _, route := range peerRoutes {
		req := httptest.NewRequest(http.MethodPost, route, strings.NewReader("{}"))
		req.RemoteAddr = "203.0.113.7:44321"
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s desde un desconocido → %d (esperaba 401)", route, rec.Code)
		}
	}
	// a declared node is let in (not 401 — whatever the handler then decides)
	for _, route := range peerRoutes {
		req := httptest.NewRequest(http.MethodPost, route, strings.NewReader("{}"))
		req.RemoteAddr = "198.51.100.51:44321"
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("%s desde un nodo DECLARADO fue rechazado: el handoff moriría", route)
		}
	}
	// and loopback too: the coordinator talks to its own control surface
	for _, route := range peerRoutes {
		if code, _ := try(t, http.MethodPost, gw.URL+route, ""); code == http.StatusUnauthorized {
			t.Errorf("%s desde loopback fue rechazado", route)
		}
	}
}

// TestInferenceStaysOpenUnlessAsked: the switch must be off by default, or
// deploying this release logs every client out at once.
func TestInferenceStaysOpenUnlessAsked(t *testing.T) {
	gw := keyedGateway(t)
	code, body := try(t, http.MethodPost, gw.URL+"/v1/chat/completions", "")
	if code == http.StatusUnauthorized {
		t.Fatalf("la inferencia exige clave sin que nadie lo haya pedido: %s", body)
	}
}

// TestInferenceCanBeClosed is the other half: the switch has to actually work,
// or it is a comment pretending to be a feature.
func TestInferenceCanBeClosed(t *testing.T) {
	engine := engineReturning(t, http.StatusOK, map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "ok"}}},
	})
	s, err := NewServer(&config.Config{
		APIKey: testKey, RequireAPIKey: true,
		Instances: []config.Instance{{Key: "decode", Role: "decode", Endpoint: engine.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(s.Handler())
	t.Cleanup(gw.Close)

	if code, body := try(t, http.MethodPost, gw.URL+"/v1/chat/completions", ""); code != http.StatusUnauthorized {
		t.Fatalf("con require_api_key la inferencia sin clave debe ser 401, fue %d: %s", code, body)
	}
	if code, body := try(t, http.MethodPost, gw.URL+"/v1/chat/completions", "sk-"+"soflink-equivocada"); code != http.StatusUnauthorized {
		t.Fatalf("una clave incorrecta debe ser 401, fue %d: %s", code, body)
	}
	// CONTROL: con la clave buena tiene que pasar
	code, body := try(t, http.MethodPost, gw.URL+"/v1/chat/completions", testKey)
	if code == http.StatusUnauthorized {
		t.Fatalf("con la clave correcta debe pasar: %d %s", code, body)
	}
}
