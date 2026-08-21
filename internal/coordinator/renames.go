package coordinator

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"time"
)

// Node display names ("2xRTX5080_W", "Dgx Spark") are UI-only labels the
// operator sets with the panel's pencil. They must behave like a shared setting:
// renaming on ANY node's panel shows on EVERY panel, and the label survives a
// restart. So a rename is (1) persisted to renames.local.json and (2) gossiped
// to the other coordinators over the discovery mesh. The anonymous node id
// (node-a, node-<hash>) never changes — only its display label.

// renameClient gossips renames with a short timeout: a peer that's down must not
// stall the pencil.
var renameClient = &http.Client{Timeout: 800 * time.Millisecond}

// renamesPath is where the display-name overrides persist. Next to the config /
// working dir by default (mounted + durable for the container); override with
// SOFMAT_RENAMES.
func renamesPath() string {
	if p := os.Getenv("SOFMAT_RENAMES"); p != "" {
		return p
	}
	return "renames.local.json"
}

// loadRenames seeds pstate.renames from disk at startup, so labels survive a
// restart of this node's soflink.
func loadRenames() {
	b, err := os.ReadFile(renamesPath())
	if err != nil {
		return
	}
	var m map[string]string
	if json.Unmarshal(b, &m) != nil {
		return
	}
	pstate.mu.Lock()
	for k, v := range m {
		pstate.renames[k] = v
	}
	pstate.mu.Unlock()
}

// saveRenames persists a snapshot of the current labels.
func saveRenames(snap map[string]string) {
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(renamesPath(), b, 0644)
}

// applyRename sets (or clears, when name is empty) a node's display label,
// persists it, and — when this is the origin of the change (propagate) — gossips
// it to the other coordinators. Gossip receivers call with propagate=false so a
// rename fans out exactly one hop (no loops).
func (s *Server) applyRename(node, name string, propagate bool) {
	if node == "" {
		return
	}
	pstate.mu.Lock()
	if name == "" {
		delete(pstate.renames, node)
	} else {
		pstate.renames[node] = name
	}
	snap := copyStr(pstate.renames)
	pstate.mu.Unlock()
	saveRenames(snap)
	if propagate {
		s.propagateRename(node, name)
	}
}

// peerCoordinators lists the other soflink coordinators to gossip to: every
// config node's host and every LAN-discovered peer, at the listen port, minus
// self. Using the config hosts (not only discovery) means the containerized
// node-a — which can't sweep the LAN from inside Docker — still reaches its
// peers by their configured IPs.
func (s *Server) peerCoordinators() []string {
	port := portOf(s.cfg.Listen)
	if port == 0 {
		port = 1357
	}
	selfHost := hostOf(s.cfg.PublicURL)
	seen := map[string]bool{}
	var out []string
	add := func(host string) {
		if host == "" || host == selfHost {
			return
		}
		addr := host + ":" + strconv.Itoa(port)
		if seen[addr] {
			return
		}
		seen[addr] = true
		out = append(out, addr)
	}
	for _, n := range s.cfg.Nodes {
		add(hostOf(n.Agent))
	}
	for _, p := range s.discoveredPeers() {
		add(hostOf(p.Addr))
	}
	return out
}

// propagateRename fans a label change out to every peer coordinator, best-effort
// and concurrent. The ?gossip=1 marker tells the receiver not to re-propagate.
func (s *Server) propagateRename(node, name string) {
	body, _ := json.Marshal(map[string]string{"node": node, "name": name})
	for _, addr := range s.peerCoordinators() {
		go func(addr string) {
			defer recoverProbe()
			req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/soflink/rename?gossip=1", bytes.NewReader(body))
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := renameClient.Do(req)
			if err != nil {
				return
			}
			_ = resp.Body.Close()
		}(addr)
	}
}

// peerRename receives a gossiped label change from another coordinator and
// applies it locally WITHOUT re-propagating (one-hop fan-out, no loops).
func (s *Server) peerRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Node string `json:"node"`
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.applyRename(req.Node, req.Name, false)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// renamesList returns this node's current labels, so a peer that was down during
// a rename can pull and catch up on startup.
func (s *Server) renamesList(w http.ResponseWriter, r *http.Request) {
	pstate.mu.Lock()
	snap := copyStr(pstate.renames)
	pstate.mu.Unlock()
	writeJSON(w, http.StatusOK, snap)
}

// reconcileRenames pulls labels from peers and adds any this node is MISSING
// (never overwrites a local label — gossip owns live edits; this only heals a
// node that was offline when a rename happened). Called after each discovery
// sweep.
func (s *Server) reconcileRenames() {
	changed := false
	for _, addr := range s.peerCoordinators() {
		resp, err := renameClient.Get("http://" + addr + "/soflink/renames")
		if err != nil {
			continue
		}
		var m map[string]string
		e := json.NewDecoder(resp.Body).Decode(&m)
		_ = resp.Body.Close()
		if e != nil {
			continue
		}
		pstate.mu.Lock()
		for k, v := range m {
			if _, ok := pstate.renames[k]; !ok {
				pstate.renames[k] = v
				changed = true
			}
		}
		pstate.mu.Unlock()
	}
	if changed {
		pstate.mu.Lock()
		snap := copyStr(pstate.renames)
		pstate.mu.Unlock()
		saveRenames(snap)
	}
}
