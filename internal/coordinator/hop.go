package coordinator

import (
	"net"
	"net/http"
	"net/url"
	"sort"
	"time"
)

// hop measures the REAL TCP round-trip from the coordinator to each node's agent,
// so the panel shows a measured per-hop network latency instead of a hardcoded
// constant. The pipeline stalls on its slowest link, so hop_ms reports the max
// across the measured nodes. This is the network RTT (no payload) — a defensible
// lower bound on the activation hop; the exact tensor-transfer cost needs the
// transport layer's own instrumentation.
func (s *Server) hop(w http.ResponseWriter, r *http.Request) {
	per := map[string]any{}
	var worst float64
	measured := false
	for _, n := range s.cfg.Nodes {
		hp := hostPort(n.Agent)
		if hp == "" {
			per[n.ID] = nil
			continue
		}
		rtt := medianDialMS(hp, 5)
		if rtt < 0 {
			per[n.ID] = nil
			continue
		}
		measured = true
		per[n.ID] = round2(rtt)
		if rtt > worst {
			worst = rtt
		}
	}
	out := map[string]any{"per_node": per, "measured": measured}
	if measured {
		out["hop_ms"] = round2(worst)
	}
	writeJSON(w, http.StatusOK, out)
}

// hostPort extracts host:port from an agent URL like "http://node-a.example.local:50060".
func hostPort(agentURL string) string {
	u, err := url.Parse(agentURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// medianDialMS dials hostport n times and returns the median connect time in ms,
// or -1 if every attempt failed.
func medianDialMS(hostport string, n int) float64 {
	var samples []float64
	for i := 0; i < n; i++ {
		t0 := time.Now()
		c, err := net.DialTimeout("tcp", hostport, 2*time.Second)
		if err != nil {
			continue
		}
		samples = append(samples, float64(time.Since(t0).Microseconds())/1000.0)
		_ = c.Close()
	}
	if len(samples) == 0 {
		return -1
	}
	sort.Float64s(samples)
	return samples[len(samples)/2]
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }
