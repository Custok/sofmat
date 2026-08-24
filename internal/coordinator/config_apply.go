package coordinator

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Custok/sofmat/internal/config"
)

// configApply atomically swaps the served model: eject the current main, wait for
// VRAM to drain, then load the new one — routed to the control plane of the MAIN
// node of the chosen layout, NOT a fixed control node. So the panel can (re)launch a
// decode whose main is a REMOTE node (e.g. the prod decode on node-c), not only the
// local node-a.
//
// The body is EITHER a preset selection {"config":"<key>"} (the panel's Load) OR a
// ready launch spec {"exe","args"} (from /admin/load or a future fleet-load). One
// call = one model swap, so a dashboard changes models with a single button
// (POST /api/apply, /config/apply).
func (s *Server) configApply(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	var sel struct {
		Config string `json:"config"`
		Exe    string `json:"exe"`
	}
	_ = json.Unmarshal(raw, &sel)

	// Resolve the control agent from the preset's MAIN node; fall back to the
	// default control node when the body carries a raw spec with no preset.
	agent := ""
	var preset *config.Preset
	if sel.Config != "" {
		for i := range s.cfg.Presets {
			if s.cfg.Presets[i].Key == sel.Config {
				preset = &s.cfg.Presets[i]
				break
			}
		}
		if preset != nil && preset.Main != "" {
			agent = s.agentBase(preset.Main)
		}
		// remember the operator's pick so every panel reflects the active config.
		pstate.mu.Lock()
		pstate.activeConfig = sel.Config
		pstate.mu.Unlock()
	}
	if agent == "" {
		agent = s.controlNode()
	}
	if agent == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no control agent configured"})
		return
	}

	// Build the launch spec. A raw {exe,args} body is forwarded verbatim; a bare
	// preset selection is turned into a spec by presetSpec (which blocks a
	// multi-node UNIÓN whose fleet-load — ggml-rpc-server/rpc_exe on the workers —
	// is a later phase, rather than firing a partial launch that could disturb a
	// running prod decode).
	spec := raw
	if sel.Exe == "" && preset != nil {
		s2, blocked := s.presetSpec(preset)
		if blocked != "" {
			// don't eject: we can't load a replacement, so leave the current model
			// serving and report why. Routing target is still resolved above.
			writeJSON(w, http.StatusOK, map[string]any{
				"launched": false, "blocked": blocked,
				"main": preset.Main, "active_config": sel.Config,
			})
			return
		}
		spec = s2
	}

	// 1) eject the current main ON THE MAIN NODE (best-effort — fine if nothing runs).
	if resp, err := s.client.Post(agent+"/control/eject", "application/json", strings.NewReader("{}")); err == nil {
		_ = resp.Body.Close()
	}
	// 2) let the GPUs release the old model before allocating the new one.
	time.Sleep(3 * time.Second)
	// 3) load the new main on the same node.
	resp, err := s.client.Post(agent+"/control/load", "application/json", bytes.NewReader(spec))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// presetSpec builds the {exe,args} launch spec for a preset, or returns a non-empty
// `blocked` reason when it cannot yet. A preset carrying an explicit arg vector is
// launched verbatim; otherwise a real spec must be synthesized from the topology,
// which for a multi-node UNIÓN needs ggml-rpc-server (rpc_exe) on the worker nodes.
// That fleet-load is a later phase (see rpcExePath TODO), so we route the request to
// the right node but don't fire a partial/guessed launch.
func (s *Server) presetSpec(p *config.Preset) (spec []byte, blocked string) {
	if len(p.Args) > 0 {
		// A preset may carry its own launcher (a remote main on a different OS/path,
		// e.g. a Linux decode's llama-server.sh wrapper); otherwise the main node's
		// own llama_exe. The control plane still allowlists the basename.
		exe := p.Exe
		if exe == "" {
			exe = s.llamaExePath()
		}
		b, _ := json.Marshal(map[string]any{"exe": exe, "args": p.Args})
		return b, ""
	}
	return nil, "preset sin args de arranque: falta el fleet-load (ggml-rpc-server/rpc_exe en los nodos worker)"
}
