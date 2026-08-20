package gateway

import (
	"testing"

	"github.com/Custok/sofmat/internal/partitioner"
)

func rolePlan(prefill string, stageNodes ...string) partitioner.RolePlan {
	stages := make([]partitioner.Stage, len(stageNodes))
	for i, n := range stageNodes {
		stages[i] = partitioner.Stage{NodeID: n, FirstLayer: i, NLayers: 1}
	}
	return partitioner.RolePlan{
		Decode:        partitioner.PartitionResult{Plan: partitioner.PartitionPlan{Stages: stages}},
		PrefillNodeID: prefill,
	}
}

func TestBackendsFromRolePlan(t *testing.T) {
	eps := map[string]string{"node-c": "http://decode.local", "node-a": "http://prefill.local"}

	cfg, err := BackendsFromRolePlan(rolePlan("node-a", "node-c", "node-d"), eps)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DecodeEntryURL != "http://decode.local" || cfg.PrefillURL != "http://prefill.local" {
		t.Fatalf("wiring wrong: %+v", cfg)
	}
	if !cfg.PrefillAvailable() {
		t.Fatal("prefill must be available")
	}
	if len(cfg.DecodeNodeIDs) != 2 || cfg.DecodeNodeIDs[1] != "node-d" {
		t.Fatalf("decode node ids wrong: %v", cfg.DecodeNodeIDs)
	}
}

func TestDecodeEntryWithoutEndpointFailsClosed(t *testing.T) {
	if _, err := BackendsFromRolePlan(rolePlan("", "node-x"), map[string]string{}); err == nil {
		t.Fatal("missing decode endpoint must fail closed")
	}
	if _, err := BackendsFromRolePlan(partitioner.RolePlan{}, map[string]string{}); err == nil {
		t.Fatal("empty plan must fail closed")
	}
}

func TestPrefillWithoutEndpointFailsSoft(t *testing.T) {
	eps := map[string]string{"node-c": "http://decode.local"} // no prefill URL
	cfg, err := BackendsFromRolePlan(rolePlan("node-a", "node-c"), eps)
	if err != nil {
		t.Fatalf("prefill without endpoint must not fail the plan: %v", err)
	}
	if cfg.PrefillAvailable() {
		t.Fatal("prefill must degrade to unavailable")
	}
}

func TestNoPrefillRoleIsDecodeOnly(t *testing.T) {
	eps := map[string]string{"node-c": "http://decode.local"}
	cfg, err := BackendsFromRolePlan(rolePlan("", "node-c"), eps)
	if err != nil || cfg.PrefillAvailable() {
		t.Fatalf("no-prefill plan must be decode-only: %+v %v", cfg, err)
	}
}
