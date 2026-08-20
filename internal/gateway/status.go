package gateway

// Status aggregation — the /api/status payload the panel polls. Fans out to
// each node's agent endpoint (:50060 reporter), fails SOFT (an unreachable
// node marks itself down, never the whole call), and folds in the
// coordinator/runtime telemetry the agents cannot see. The node fetcher is
// injected so aggregation is testable with zero network; node endpoints come
// from config (anonymous labels), never hardcoded.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// NodeFetcher takes (name, url) and returns the node agent's JSON document.
type NodeFetcher func(name, url string) (map[string]any, error)

// NodeRef is one configured node: {"name": label, "url": agent-url}.
type NodeRef struct {
	Name string
	URL  string
}

// HTTPNodeFetcher is the real fetcher: GET the agent endpoint, parse JSON.
func HTTPNodeFetcher(timeout time.Duration) NodeFetcher {
	client := &http.Client{Timeout: timeout}
	return func(name, url string) (map[string]any, error) {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("agent %s: HTTP %d", name, resp.StatusCode)
		}
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, err
		}
		return out, nil
	}
}

// agentFields is the known agent contract; anything else an agent adds is
// dropped so the panel contract stays stable.
var agentFields = []string{"gpus", "cpu", "ram_used_mb", "ram_total_mb",
	"rx", "tx", "rtt_ms"}

func sanitizeAgent(data map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range agentFields {
		if v, ok := data[k]; ok {
			out[k] = v
		}
	}
	return out
}

// AggregateNodes fans out to every node agent concurrently, fail-soft.
// Returns one entry per node, order preserved (stable panel layout): either
// {name, up: true, ...agent fields} or {name, up: false, error}.
func AggregateNodes(nodes []NodeRef, fetch NodeFetcher) []map[string]any {
	results := make([]map[string]any, len(nodes))
	var wg sync.WaitGroup
	for i, node := range nodes {
		wg.Add(1)
		go func(i int, node NodeRef) {
			defer wg.Done()
			name := node.Name
			if name == "" {
				name = fmt.Sprintf("node-%d", i)
			}
			data, err := fetch(name, node.URL)
			if err != nil { // fail-soft: node down, not the whole call
				results[i] = map[string]any{"name": name, "up": false,
					"error": err.Error()}
				return
			}
			entry := map[string]any{"name": name, "up": true}
			for k, v := range sanitizeAgent(data) {
				entry[k] = v
			}
			results[i] = entry
		}(i, node)
	}
	wg.Wait()
	return results
}

// BuildStatus assembles the full status document. pipeline (coordinator
// telemetry: barrier_ms per stage, kv, wait%) and serving (live serve config
// summary) are passed in by the runtime; the gateway does not compute them.
func BuildStatus(nodes []NodeRef, fetch NodeFetcher,
	pipeline, serving map[string]any) map[string]any {
	nodeStatus := AggregateNodes(nodes, fetch)
	up := 0
	for _, n := range nodeStatus {
		if b, ok := n["up"].(bool); ok && b {
			up++
		}
	}
	if pipeline == nil {
		pipeline = map[string]any{}
	}
	if serving == nil {
		serving = map[string]any{}
	}
	return map[string]any{
		"nodes":       nodeStatus,
		"nodes_up":    up,
		"nodes_total": len(nodeStatus),
		"pipeline":    pipeline,
		"serving":     serving,
	}
}
