package gateway

// RolePlan consumption — the bridge from the partitioner's disaggregated
// role map to this gateway's backend wiring. The solver decides WHICH node
// holds each role; config decides how to REACH a node (endpoints live in
// config.local, never in code); the gateway composes both.

import (
	"fmt"

	"github.com/Custok/sofmat/internal/partitioner"
)

// BackendConfig is what Gateway needs from a RolePlan: where decode enters
// and, when the role exists, where prefill lives.
type BackendConfig struct {
	DecodeEntryURL string // serving endpoint of the first decode stage's node
	PrefillURL     string // "" = no prefill role (gateway stays decode-only)
	DecodeNodeIDs  []string
}

// PrefillAvailable reports whether the plan carries a reachable prefill role.
func (b BackendConfig) PrefillAvailable() bool { return b.PrefillURL != "" }

// BackendsFromRolePlan resolves a solver RolePlan against the configured
// node endpoints (nodeID -> base URL).
//
// Decode is the live product: a plan whose entry node has no endpoint is a
// config error and FAILS CLOSED. Prefill is an optimization: a prefill role
// without an endpoint degrades to decode-only (fail-soft, same philosophy as
// the admission path) — the plan is usable, just not disaggregated.
func BackendsFromRolePlan(plan partitioner.RolePlan,
	endpoints map[string]string) (BackendConfig, error) {
	stages := plan.Decode.Plan.Stages
	if len(stages) == 0 {
		return BackendConfig{}, fmt.Errorf("role plan has no decode stages")
	}
	entry := stages[0].NodeID
	entryURL, ok := endpoints[entry]
	if !ok || entryURL == "" {
		return BackendConfig{}, fmt.Errorf(
			"no endpoint configured for decode entry node %q", entry)
	}
	cfg := BackendConfig{
		DecodeEntryURL: entryURL,
		DecodeNodeIDs:  plan.Decode.Plan.NodeIDs(),
	}
	if plan.PrefillNodeID != "" {
		if u, ok := endpoints[plan.PrefillNodeID]; ok && u != "" {
			cfg.PrefillURL = u
		}
		// else: fail-soft — prefill role planned but unreachable; decode-only.
	}
	return cfg, nil
}
