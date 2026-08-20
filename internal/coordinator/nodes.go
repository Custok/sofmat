package coordinator

import (
	"bytes"
	"io"
	"net/http"
	"time"

	"github.com/Custok/sofmat/internal/gateway"
)

// nodes aggregates every configured node's sensor agent (GET {agent}/gpu) into
// one payload, so a dashboard reads the whole cluster's GPU/host stats THROUGH
// the coordinator instead of polling each node itself. The concurrent fail-soft
// fan-out is gateway.AggregateNodes (shared, tested); here it is just keyed by
// node id for the panel's per-node lookup.
func (s *Server) nodes(w http.ResponseWriter, r *http.Request) {
	refs := make([]gateway.NodeRef, 0, len(s.cfg.Nodes))
	for _, n := range s.cfg.Nodes {
		refs = append(refs, gateway.NodeRef{Name: n.ID, URL: n.Agent + "/gpu"})
	}
	list := gateway.AggregateNodes(refs, gateway.HTTPNodeFetcher(2500*time.Millisecond))
	out := map[string]any{}
	for _, n := range list {
		name, _ := n["name"].(string)
		out[name] = n
	}
	writeJSON(w, http.StatusOK, out)
}

// controlNode returns the agent base of the node that actuates load/eject (the
// node whose main process the panel launches). Prefers "node-a", else the first
// node exposing an agent.
func (s *Server) controlNode() string {
	for _, n := range s.cfg.Nodes {
		if n.ID == "node-a" && n.Agent != "" {
			return n.Agent
		}
	}
	for _, n := range s.cfg.Nodes {
		if n.Agent != "" {
			return n.Agent
		}
	}
	return ""
}

func (s *Server) adminEject(w http.ResponseWriter, r *http.Request) {
	s.forwardControl(w, r, "/control/eject")
}

func (s *Server) adminLoad(w http.ResponseWriter, r *http.Request) {
	s.forwardControl(w, r, "/control/load")
}

// forwardControl proxies a control POST to the control node's host agent, which
// performs the actuation — a Linux container cannot start or kill a Windows host
// process, so the coordinator is the control plane and the agent is the hands.
func (s *Server) forwardControl(w http.ResponseWriter, r *http.Request, path string) {
	agent := s.controlNode()
	if agent == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no control agent configured"})
		return
	}
	defer r.Body.Close()
	body, _ := io.ReadAll(r.Body)
	req, err := http.NewRequest(http.MethodPost, agent+path, bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
