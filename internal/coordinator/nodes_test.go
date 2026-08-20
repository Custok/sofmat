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

// The panel reads the whole cluster through the coordinator: /nodes fans out to
// each node's agent and reports the ones without an agent as down.
func TestNodesAggregatesAgents(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gpu" {
			t.Errorf("agent path = %q, want /gpu", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"node":"node-x","gpus":[{"idx":0}],"cpu":7}`)
	}))
	defer agent.Close()

	cfg := &config.Config{
		Instances: []config.Instance{{Key: "decode", Endpoint: "http://decode:8080"}},
		Nodes: []config.Node{
			{ID: "node-x", Agent: agent.URL, GPUs: 1},
			{ID: "node-y", GPUs: 2}, // no agent -> reported down, not an error
		},
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nodes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/nodes status = %d, want 200", rec.Code)
	}
	var out map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("json: %v", err)
	}
	if out["node-x"]["up"] != true {
		t.Errorf("node-x up = %v, want true", out["node-x"]["up"])
	}
	if out["node-x"]["cpu"] != float64(7) {
		t.Errorf("node-x cpu = %v, want 7", out["node-x"]["cpu"])
	}
	if out["node-y"]["up"] != false {
		t.Errorf("node-y up = %v, want false (no agent)", out["node-y"]["up"])
	}
}

// load/eject is forwarded to the control node's host agent (the container can't
// touch the host itself).
func TestForwardControlToAgent(t *testing.T) {
	var gotPath string
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer agent.Close()

	cfg := &config.Config{
		Instances: []config.Instance{{Key: "decode", Endpoint: "http://decode:8080"}},
		Nodes:     []config.Node{{ID: "node-a", Agent: agent.URL}},
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/eject", strings.NewReader("{}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("/admin/eject status = %d, want 200", rec.Code)
	}
	if gotPath != "/control/eject" {
		t.Fatalf("agent received %q, want /control/eject", gotPath)
	}
}

// With no control agent configured, admin control fails soft with 503.
func TestAdminNoControlAgent(t *testing.T) {
	cfg := &config.Config{Instances: []config.Instance{{Key: "decode", Endpoint: "http://decode:8080"}}}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/eject", strings.NewReader("{}")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/admin/eject with no agent = %d, want 503", rec.Code)
	}
}
