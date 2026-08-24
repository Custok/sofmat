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

	// Presets are the loadable model layouts the panel offers in its Load
	// picker: each names a model + per-node roles/topology. Real endpoints and
	// gguf paths live only in config.local.json (gitignored).
	Presets []Preset `json:"presets"`

	// PublicURL is the coordinator's reachable API base (the single entry point
	// that fronts every role — "Reachable at" in the panel). APIKey, when set,
	// is the optional bearer key clients must send; empty = auth disabled.
	PublicURL string `json:"public_url"`
	APIKey    string `json:"api_key"`

	// AutoStart, when true, means the operator asked soflink to launch on boot (the
	// panel's "Arrancar al inicio" check) AND auto-bring-up the configured unión,
	// so the fleet self-recovers after a reboot without anyone clicking. Persisted
	// in config.local.json; the platform install (a per-user startup task on
	// Windows, which keeps GPU access in the user session) is (de)registered when it
	// flips. AutoStartGroup names which group to raise on boot; empty = the first
	// group found.
	AutoStart      bool   `json:"auto_start"`
	AutoStartGroup string `json:"auto_start_group"`

	// SelfID is this node's anonymous label for LAN discovery (the /soflink/hello
	// id). When empty, a stable anonymized id is derived from the hostname —
	// never the raw hostname on the wire.
	SelfID string `json:"self_id"`

	// LlamaExe is the host's llama-server binary the control agent may launch
	// (real machine path lives only in config.local.json, gitignored). Empty =
	// "llama-server" on PATH. ModelsHostDir is where the HOST sees downloaded
	// GGUFs (the host-side path for the launch -m flag). Empty = the models dir.
	LlamaExe      string `json:"llama_exe"`
	ModelsHostDir string `json:"models_host_dir"`

	// RpcExe is the host's ggml-rpc-server binary a worker node launches to join a
	// multi-node UNIÓN pipeline (weights split by --tensor-split, RPC to the main).
	// Real machine path lives only in config.local.json (gitignored). Empty =
	// "ggml-rpc-server" on PATH. Used by the fleet-load phase (see rpcExePath).
	RpcExe string `json:"rpc_exe"`

	// GPUIndexReversed: the engine (e.g. a Vulkan build) enumerates GPUs in the
	// REVERSE of nvidia-smi order, so --main-gpu / --tensor-split must be remapped.
	// Set per node in config.local.json when confirmed (see the launch log's
	// "VulkanN … (PCI)" lines vs nvidia-smi pci.bus_id). Default false (CUDA order).
	GPUIndexReversed bool `json:"gpu_index_reversed"`

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

	// GitHubToken authenticates the auto-updater's GitHub API calls. The public
	// API is 60 req/h per IP (a shared LAN egress exhausts it fast); with a token
	// it's 5000 req/h, so the fleet never loses its update channel. Lives ONLY in
	// config.local.json (gitignored) — never committed. Empty = anonymous.
	GitHubToken string `json:"github_token"`
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

	// ModelName is the display name of the model this instance serves. When set it
	// overrides the served-model label on the panel card, so a role serving a
	// DIFFERENT model (e.g. a Q4_0 prefill running beside a Q8_0 decode) shows its
	// OWN model instead of inheriting the decode's served-model name. Empty = fall
	// back to the live served-model reported by the endpoint. Model, when set, is
	// the gguf path (informational; the running process owns the real path).
	ModelName string `json:"model_name"`
	Model     string `json:"model"`
}

// Stage is a node's inclusive layer range within an instance's pipeline.
type Stage struct {
	Node   string `json:"node"`
	GPUs   int    `json:"gpus"`
	Layers [2]int `json:"layers"`
}

// Preset is a loadable model layout shown in the panel's Load picker. `Layers`
// in Topology is a per-node layer COUNT (the pipeline share), not a range.
type Preset struct {
	Key       string        `json:"key"`
	Label     string        `json:"label"`
	ModelName string        `json:"model_name"`
	Model     string        `json:"model"` // gguf path (local main only)

	// Exe is the launcher binary for THIS preset's main node, when it differs from
	// the coordinator host's llama_exe — e.g. a remote decode whose main is a Linux
	// node using a bundle wrapper (llama-server.sh) at its own path. Empty = the
	// main node's own llama_exe (llamaExePath). Only *llama-server* basenames (incl.
	// a llama-server.sh wrapper) are allowlisted by the control plane.
	Exe string `json:"exe"`
	Quant     string        `json:"quant"`
	Ctx       string        `json:"ctx"`
	KV        string        `json:"kv"`
	Main      string        `json:"main"`     // node id running the main process
	Endpoint  string        `json:"endpoint"` // where the served API answers
	Remote    bool          `json:"remote"`
	Note      string        `json:"note"`
	SizeGB    float64       `json:"size_gb"`
	RPC       string        `json:"rpc"`
	Args      []string      `json:"args"`
	Topology  []PresetStage `json:"topology"`

	// Group ties presets that run TOGETHER as one deployment (e.g. a disaggregated
	// decode+prefill unión): the panel renders them as a single card with one
	// "Cargar unión" button, and /api/apply-union brings the whole group up
	// idempotently (a role already answering /health is left running). Empty =
	// standalone preset (its own card + button). Order within a group follows the
	// presets array (bring the base/decode up first).
	Group string `json:"group,omitempty"`
}

// PresetStage is one node's share of a preset's pipeline (layer count + GPUs).
type PresetStage struct {
	Node   string `json:"node"`
	GPUs   int    `json:"gpus"`
	Layers int    `json:"layers"`
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
