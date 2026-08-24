package coordinator

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Custok/sofmat/internal/config"
)

// rpcWorkersFrom pulls the worker addresses from a preset's `--rpc host:port,...`
// arg, so the union raise can wait for each to accept before firing the main
// (launching the main with a worker down makes llama try to fit the whole model on
// the main's GPU → OOM, the failure debian/metahuman flagged).
func rpcWorkersFrom(args []string) []string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--rpc" {
			return strings.Split(args[i+1], ",")
		}
	}
	return nil
}

// waitWorkers blocks until every rpc worker accepts a TCP connection, or the budget
// runs out. A freshly-rebooted worker is listening within seconds but the main must
// not start before it does; a short retry also rides out the 1–2s socket-reset
// window when a worker restarts. Returns the first address still down, or "".
func waitWorkers(workers []string, budget time.Duration) string {
	deadline := time.Now().Add(budget)
	for _, w := range workers {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		for {
			c, err := net.DialTimeout("tcp", w, 1500*time.Millisecond)
			if err == nil {
				_ = c.Close()
				break
			}
			if time.Now().After(deadline) {
				return w
			}
			time.Sleep(time.Second)
		}
	}
	return ""
}

// unionPresets returns the presets of a group in config order. An empty group
// name selects the first group found, so a one-group cluster "just works".
func (s *Server) unionPresets(group string) []config.Preset {
	if group == "" {
		for i := range s.cfg.Presets {
			if s.cfg.Presets[i].Group != "" {
				group = s.cfg.Presets[i].Group
				break
			}
		}
	}
	out := []config.Preset{}
	for i := range s.cfg.Presets {
		if s.cfg.Presets[i].Group != "" && s.cfg.Presets[i].Group == group {
			out = append(out, s.cfg.Presets[i])
		}
	}
	return out
}

// endpointUp reports whether a served endpoint answers /health with 200, so the
// union raise can SKIP a role that is already running (idempotent: never restarts
// a live prod decode).
func endpointUp(endpoint string) bool {
	if endpoint == "" {
		return false
	}
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(strings.TrimRight(endpoint, "/") + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// raisePreset brings ONE preset up if it isn't already: it health-checks the
// preset's endpoint (skip when live), else builds the launch spec and, routed to
// the preset's MAIN node control plane, ejects the old main and loads the new one.
// Returns a per-role status the union handler collects. It never force-restarts a
// role that is already answering — so re-running the union is safe.
func (s *Server) raisePreset(p *config.Preset) map[string]any {
	st := map[string]any{"key": p.Key, "label": p.Label, "main": p.Main, "endpoint": p.Endpoint}
	if endpointUp(p.Endpoint) {
		st["action"] = "up"
		st["ok"] = true
		return st
	}
	spec, blocked := s.presetSpec(p)
	if blocked != "" {
		st["action"] = "blocked"
		st["blocked"] = blocked
		st["ok"] = false
		return st
	}
	// wait for this preset's rpc workers to accept before firing the main, or it
	// OOMs trying to fit the whole model locally (debian/metahuman's reboot-order
	// warning). Give a rebooting worker up to ~40s to come listening.
	if down := waitWorkers(rpcWorkersFrom(p.Args), 40*time.Second); down != "" {
		st["action"] = "blocked"
		st["blocked"] = "rpc worker no acepta: " + down + " (arráncalo/espera; con systemd Restart=always revive solo)"
		st["ok"] = false
		return st
	}
	agent := s.agentBase(p.Main)
	if agent == "" {
		agent = s.controlNode()
	}
	if agent == "" {
		st["action"] = "blocked"
		st["blocked"] = "sin control agent para el nodo main " + p.Main
		st["ok"] = false
		return st
	}
	// eject the old main on that node (best-effort — a no-op after a reboot), let
	// VRAM drain, then load. Mirrors configApply so a role comes up the same way
	// whether raised alone or as part of the union.
	if resp, err := s.client.Post(agent+"/control/eject", "application/json", strings.NewReader("{}")); err == nil {
		_ = resp.Body.Close()
	}
	time.Sleep(3 * time.Second)
	resp, err := s.client.Post(agent+"/control/load", "application/json", bytes.NewReader(spec))
	if err != nil {
		st["action"] = "error"
		st["error"] = err.Error()
		st["ok"] = false
		return st
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	st["action"] = "launched"
	st["ok"] = out["ok"] == true
	st["agent_reply"] = out
	return st
}

// configApplyUnion brings a WHOLE group (a disaggregated decode+prefill unión) up
// with one call: each role is raised in config order, idempotently (a role already
// serving /health is left running, so this never disturbs a live prod decode). One
// click = the whole deployment up, and it doubles as the boot auto-raise.
func (s *Server) configApplyUnion(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req struct {
		Group string `json:"group"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	presets := s.unionPresets(req.Group)
	if len(presets) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "sin presets en el grupo (falta el campo group en la config)"})
		return
	}
	roles := make([]map[string]any, 0, len(presets))
	allOK := true
	for i := range presets {
		st := s.raisePreset(&presets[i])
		if st["ok"] != true {
			allOK = false
		}
		roles = append(roles, st)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": allOK, "group": presets[0].Group, "roles": roles})
}
