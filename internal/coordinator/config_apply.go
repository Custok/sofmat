package coordinator

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"
)

// configApply atomically swaps the served model on the control node: eject the
// current main, wait for VRAM to drain, then load the new one. The body is the
// launch spec ({exe, args}) the node agent understands — the same one /admin/load
// takes. One call = one model swap, so a dashboard can change models with a single
// button (POST /config/apply). Cross-node role changes (a main that must move to a
// different node, RPC workers) need each node's agent to expose control; today the
// control node (node-a) is the one that swaps its local main.
func (s *Server) configApply(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	spec, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	agent := s.controlNode()
	if agent == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no control agent configured"})
		return
	}
	// 1) eject the current main (best-effort — fine if nothing is running).
	if resp, err := s.client.Post(agent+"/control/eject", "application/json", strings.NewReader("{}")); err == nil {
		_ = resp.Body.Close()
	}
	// 2) let the GPUs release the old model before allocating the new one.
	time.Sleep(3 * time.Second)
	// 3) load the new main.
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
