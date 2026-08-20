package coordinator

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"path"
	"time"

	"github.com/Custok/sofmat/internal/gateway"
)

//go:embed panel.html
var panelHTML []byte

// panelPage serves the embedded live dashboard, so a node's binary carries its
// own UI — no separate web server, no Python interpreter to ship.
func (s *Server) panelPage(w http.ResponseWriter, r *http.Request) {
	// "/" is registered as the catch-all, so guard the path to keep unknown
	// routes a real 404 (only the dashboard home serves the page).
	if r.URL.Path != "/" && r.URL.Path != "/panel" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(panelHTML)
}

// getJSON GETs and decodes a path from the decode backend (served-model info).
func (s *Server) getJSON(pth string) map[string]any {
	if s.bc.DecodeEntryURL == "" {
		return nil
	}
	resp, err := s.client.Get(s.bc.DecodeEntryURL + pth)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var m map[string]any
	if json.NewDecoder(resp.Body).Decode(&m) != nil {
		return nil
	}
	return m
}

// panelStatus assembles the dashboard status from the coordinator's own data —
// the served model (decode backend /props + /v1/models) and every node's
// GPU/host stats (shared aggregator) — so the embedded panel renders live.
// Phase 1: served model + live node cards + load/eject. Instances, pipeline
// topology and measure come in phase 2.
func (s *Server) panelStatus(w http.ResponseWriter, r *http.Request) {
	props := s.getJSON("/props")
	models := s.getJSON("/v1/models")
	loaded := props != nil

	var (
		model, quant  string
		nctx, slots   = 0, 4
		sizeGB        float64
	)
	if data, ok := models["data"].([]any); ok && len(data) > 0 {
		if d0, ok := data[0].(map[string]any); ok {
			model = path.Base(pstr(d0["id"]))
			if meta, ok := d0["meta"].(map[string]any); ok {
				nctx = pint(meta["n_ctx"])
				quant = pstr(meta["ftype"])
				sizeGB = round1(pnum(meta["size"]) / 1e9)
			}
		}
	}
	if props != nil {
		if v := pint(props["total_slots"]); v > 0 {
			slots = v
		}
		if quant == "" {
			quant = pstr(props["model_ftype"])
		}
	}

	refs := make([]gateway.NodeRef, 0, len(s.cfg.Nodes))
	for _, n := range s.cfg.Nodes {
		refs = append(refs, gateway.NodeRef{Name: n.ID, URL: n.Agent + "/gpu"})
	}
	list := gateway.AggregateNodes(refs, gateway.HTTPNodeFetcher(2500*time.Millisecond))
	nodes := make([]map[string]any, 0, len(list))
	for _, n := range list {
		up, _ := n["up"].(bool)
		role := "worker (rpc)"
		if pstr(n["name"]) == "node-a" {
			role = "master + worker"
		}
		if !up {
			role = "libre · disponible"
		}
		nodes = append(nodes, map[string]any{
			"node":   n["name"],
			"role":   role,
			"up":     up,
			"active": up && loaded,
			"gpus":   n["gpus"],
			"stats": map[string]any{
				"cpu": n["cpu"], "ram_used_mb": n["ram_used_mb"],
				"ram_total_mb": n["ram_total_mb"],
			},
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"loaded": loaded, "loading": false,
		"model": model, "quant": quant, "size_gb": sizeGB,
		"n_ctx": nctx, "slots": slots,
		"endpoint":      "coordinator Go",
		"nodes":         nodes,
		"configs":       []any{},
		"instances":     []any{},
		"pipeline":      nil,
		"cluster_tokps": nil,
		"tokps":         nil,
		"selected":      "decode",
		"active_config": nil,
	})
}

// apiStub answers panel actions not yet ported to Go (chat, measure, …) so the
// UI degrades gracefully instead of erroring while phase 2 lands.
func (s *Server) apiStub(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "panel phase 2 (pending Go port)"})
}

func pstr(v any) string    { s, _ := v.(string); return s }
func pnum(v any) float64   { f, _ := v.(float64); return f }
func pint(v any) int       { return int(pnum(v)) }
func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}
