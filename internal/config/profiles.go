// Profiles: the bridge config -> partitioner (consensus 20-08, topic
// escalado-batching-desagregado). Precedence per the solver's design of
// record: MEASURED profile (machine-written by the microbench/agent) >
// DECLARED value in the cluster config > fail-closed error. Never a baked-in
// default: a declared split that lies about capacity is exactly how today's
// BF16 OOM happened.
package config

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/Custok/sofmat/internal/partitioner"
)

// MeasuredProfile is one node's measured numbers, published by the bench /
// node agent into the profiles file. Humans do not edit this.
type MeasuredProfile struct {
	MsPerLayer         float64 `json:"ms_per_layer"`
	BoundaryOverheadMS float64 `json:"boundary_overhead_ms"`
}

// Profiles is the machine-written measured-profiles file: node id -> profile.
type Profiles map[string]MeasuredProfile

// LoadProfiles reads the measured profiles file. A missing file is NOT an
// error (returns an empty map): measurements are optional, the declared
// config is the fallback, and the loader fails closed only when a node has
// neither.
func LoadProfiles(path string) (Profiles, error) {
	if path == "" {
		path = "profiles.local.json"
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Profiles{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("profiles: %w", err)
	}
	var p Profiles
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("profiles: parse %s: %w", path, err)
	}
	return p, nil
}

// NodeProfiles maps the cluster config + measured profiles onto solver
// inputs. Fail-closed: a node without a usable capacity (model_mem_cap_gb or
// vram_gb) or without any speed source (measured ms_per_layer or declared
// mem_bandwidth_gbps) is an error naming the node, never a guess.
func NodeProfiles(cfg *Config, measured Profiles) ([]partitioner.NodeProfile, error) {
	if len(cfg.Nodes) == 0 {
		return nil, fmt.Errorf("config: no nodes declared")
	}
	out := make([]partitioner.NodeProfile, 0, len(cfg.Nodes))
	for _, n := range cfg.Nodes {
		capGB := n.ModelMemCapGB
		if capGB == 0 {
			capGB = n.VRAMGB
		}
		if capGB <= 0 {
			return nil, fmt.Errorf(
				"config: node %s: no usable capacity (set model_mem_cap_gb or vram_gb) — refusing to guess (fail-closed)",
				n.ID)
		}
		p := partitioner.NodeProfile{ID: n.ID, ModelMemCapGB: capGB}
		if m, ok := measured[n.ID]; ok && m.MsPerLayer > 0 {
			p.MsPerLayer = m.MsPerLayer // measured wins over declared
		} else if n.MemBandwidthGBps > 0 {
			p.MemBandwidthGBps = n.MemBandwidthGBps
		} else {
			return nil, fmt.Errorf(
				"config: node %s: no measured ms_per_layer and no declared mem_bandwidth_gbps — refusing to guess (fail-closed)",
				n.ID)
		}
		out = append(out, p)
	}
	return out, nil
}

// ModelSpecFrom builds the solver's model description from the config,
// delegating validation to partitioner.NewModelSpec (fail-closed on zero or
// negative sizes).
func ModelSpecFrom(cfg *Config) (partitioner.ModelSpec, error) {
	m := cfg.Model
	spec, err := partitioner.NewModelSpec(
		m.NLayers, m.WeightsGB, m.KVCacheGB, m.BatchSlots, m.PrefixCacheGB)
	if err != nil {
		return spec, fmt.Errorf(
			"config: model section incomplete (n_layers/weights_gb/kv_cache_gb from the GGUF header + context budget): %w", err)
	}
	return spec, nil
}

// BoundaryOverhead resolves the per-boundary overhead with the same
// precedence: any measured per-node value (max across nodes, conservative)
// wins over the declared config value; neither -> error.
func BoundaryOverhead(cfg *Config, measured Profiles) (float64, error) {
	best := 0.0
	for _, m := range measured {
		if m.BoundaryOverheadMS > best {
			best = m.BoundaryOverheadMS
		}
	}
	if best > 0 {
		return best, nil
	}
	if cfg.BoundaryOverheadMS > 0 {
		return cfg.BoundaryOverheadMS, nil
	}
	return 0, fmt.Errorf(
		"config: no measured boundary overhead and no declared boundary_overhead_ms — refusing to guess (fail-closed)")
}
