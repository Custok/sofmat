// Package config loads the sofmat cluster configuration. Real endpoints and
// hosts live only in config.local.json (gitignored); the repo ships an
// anonymized config.example.json. Node identifiers are anonymous labels.
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config is the whole-cluster configuration.
type Config struct {
	Listen    string     `json:"listen"` // API bind address, e.g. ":8080"
	Nodes     []Node     `json:"nodes"`
	Instances []Instance `json:"instances"`

	// Model describes the model to place (solver input). Optional for plain
	// serving; required (and validated fail-closed) by ModelSpecFrom.
	Model Model `json:"model"`

	// ProfilesPath points to the MACHINE-WRITTEN measured profiles file
	// (ms/layer per node, boundary overhead) published by the microbench /
	// node agent. Optional; default "profiles.local.json". Humans edit the
	// declared values in Nodes, never this file.
	ProfilesPath string `json:"profiles_path"`

	// BoundaryOverheadMS is the DECLARED per-boundary overhead fallback used
	// when no measured value exists in the profiles file.
	BoundaryOverheadMS float64 `json:"boundary_overhead_ms"`
}

// Node is one host in the pool.
type Node struct {
	ID       string `json:"id"`       // anonymous label, e.g. "node-a"
	Endpoint string `json:"endpoint"` // RPC/agent endpoint (local-only)
	GPUs     int    `json:"gpus"`

	// Agent is the node's sensor/control HTTP base (GPU+host stats via GET /gpu,
	// actuation via POST /control/*), e.g. "http://node-a.example.local:50060". The
	// coordinator aggregates /gpu from every node and forwards load/eject to the
	// local node's agent — a Linux container can't touch a host GPU or process.
	Agent string `json:"agent"`

	// Capacity: GPU memory usable for weights+KV on this node. Defaults to
	// VRAMGB when omitted (mirrors config.example.yaml semantics).
	ModelMemCapGB float64 `json:"model_mem_cap_gb"`
	VRAMGB        float64 `json:"vram_gb"`

	// Declared speed fallback (used only when no measured profile exists).
	MemBandwidthGBps float64 `json:"mem_bandwidth_gbps"`

	Elastic bool `json:"elastic"`
}

// Model is the solver-facing model description. NLayers/WeightsGB should come
// from the GGUF header (tools/gguf_modelspec); KVCacheGB is the budget of one
// serving slot at MaxContext with the configured cache type.
type Model struct {
	Path          string  `json:"path"`
	NLayers       int     `json:"n_layers"`
	WeightsGB     float64 `json:"weights_gb"`
	KVCacheGB     float64 `json:"kv_cache_gb"`
	BatchSlots    int     `json:"batch_slots"`
	PrefixCacheGB float64 `json:"prefix_cache_gb"`
	MaxContext    int     `json:"max_context"`
}

// Instance is a role served on the cluster (decode, prefill, ...).
type Instance struct {
	Key      string  `json:"key"`
	Role     string  `json:"role"`
	Endpoint string  `json:"endpoint"`
	Main     string  `json:"main"` // node id hosting the main process
	Topology []Stage `json:"topology"`
}

// Stage is a node's inclusive layer range within an instance's pipeline.
type Stage struct {
	Node   string `json:"node"`
	GPUs   int    `json:"gpus"`
	Layers [2]int `json:"layers"`
}

// Load reads and validates a JSON config from path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	return &c, nil
}

// Instance returns the instance with the given key, or false.
func (c *Config) Instance(key string) (Instance, bool) {
	for _, in := range c.Instances {
		if in.Key == key {
			return in, true
		}
	}
	return Instance{}, false
}
