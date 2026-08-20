package coordinator

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Custok/sofmat/internal/config"
)

func TestBackendConfigFromResolvesRoles(t *testing.T) {
	cfg := &config.Config{Instances: []config.Instance{
		{Key: "decode", Endpoint: "http://decode:8080"},
		{Key: "prefill", Endpoint: "http://prefill:8081"},
	}}
	bc := backendConfigFrom(cfg)
	if bc.DecodeEntryURL != "http://decode:8080" {
		t.Fatalf("decode entry = %q, want http://decode:8080", bc.DecodeEntryURL)
	}
	if !bc.PrefillAvailable() || bc.PrefillURL != "http://prefill:8081" {
		t.Fatalf("prefill = %q (available=%v)", bc.PrefillURL, bc.PrefillAvailable())
	}
}

func TestBackendConfigDecodeOnly(t *testing.T) {
	cfg := &config.Config{Instances: []config.Instance{
		{Key: "decode", Endpoint: "http://decode:8080"},
	}}
	if bc := backendConfigFrom(cfg); bc.PrefillAvailable() {
		t.Fatal("single decode instance must be decode-only (no prefill)")
	}
}

func TestNewServerWiresGateway(t *testing.T) {
	cfg := &config.Config{Listen: ":8080", Instances: []config.Instance{
		{Key: "decode", Endpoint: "http://decode:8080"},
	}}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if s.Handler() == nil {
		t.Fatal("nil handler")
	}
}

// The whole pipeline must be reachable through the coordinator: /prefill/* is
// proxied to the prefill backend so a dashboard reads both stages from Go.
func TestPrefillRouteProxies(t *testing.T) {
	prefill := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("prefill got path %q, want /health", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"status":"ok","stage":"prefill"}`)
	}))
	defer prefill.Close()

	cfg := &config.Config{Instances: []config.Instance{
		{Key: "decode", Endpoint: "http://decode:8080"},
		{Key: "prefill", Endpoint: prefill.URL},
	}}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/prefill/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/prefill/health status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != `{"status":"ok","stage":"prefill"}` {
		t.Fatalf("/prefill/health body = %q", body)
	}
}

// Decode-only configs must not register /prefill routes (fail-soft, no panic).
func TestPrefillRouteAbsentWhenDecodeOnly(t *testing.T) {
	cfg := &config.Config{Instances: []config.Instance{
		{Key: "decode", Endpoint: "http://decode:8080"},
	}}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/prefill/health", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/prefill/health with no prefill = %d, want 404", rec.Code)
	}
}
