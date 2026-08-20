// Contract tests for the config -> partitioner bridge: precedence
// measured > declared > fail-closed (consensus 20-08). Fictitious values only.
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Custok/sofmat/internal/partitioner"
)

func testConfig() *Config {
	return &Config{
		Nodes: []Node{
			{ID: "node-a", ModelMemCapGB: 24, MemBandwidthGBps: 900},
			{ID: "node-b", VRAMGB: 64, MemBandwidthGBps: 250},
		},
		Model: Model{
			NLayers: 40, WeightsGB: 20, KVCacheGB: 2, BatchSlots: 1,
		},
		BoundaryOverheadMS: 1.5,
	}
}

func TestMeasuredWinsOverDeclared(t *testing.T) {
	cfg := testConfig()
	measured := Profiles{"node-a": {MsPerLayer: 0.4}}
	nodes, err := NodeProfiles(cfg, measured)
	if err != nil {
		t.Fatal(err)
	}
	if nodes[0].MsPerLayer != 0.4 || nodes[0].MemBandwidthGBps != 0 {
		t.Fatalf("measured must win over declared: %+v", nodes[0])
	}
	if nodes[1].MemBandwidthGBps != 250 {
		t.Fatalf("unmeasured node keeps declared fallback: %+v", nodes[1])
	}
}

func TestCapacityDefaultsToVRAM(t *testing.T) {
	nodes, err := NodeProfiles(testConfig(), Profiles{})
	if err != nil {
		t.Fatal(err)
	}
	if nodes[1].ModelMemCapGB != 64 {
		t.Fatalf("cap must default to vram_gb: %+v", nodes[1])
	}
}

func TestFailClosedWithoutCapacity(t *testing.T) {
	cfg := testConfig()
	cfg.Nodes = append(cfg.Nodes, Node{ID: "node-x", MemBandwidthGBps: 900})
	if _, err := NodeProfiles(cfg, Profiles{}); err == nil || !strings.Contains(err.Error(), "node-x") {
		t.Fatalf("must fail closed naming the node, got %v", err)
	}
}

func TestFailClosedWithoutAnySpeedSource(t *testing.T) {
	cfg := testConfig()
	cfg.Nodes = append(cfg.Nodes, Node{ID: "node-y", ModelMemCapGB: 16})
	if _, err := NodeProfiles(cfg, Profiles{}); err == nil || !strings.Contains(err.Error(), "node-y") {
		t.Fatalf("must fail closed naming the node, got %v", err)
	}
}

func TestModelSpecValidatedFailClosed(t *testing.T) {
	cfg := testConfig()
	spec, err := ModelSpecFrom(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if spec.NLayers != 40 || spec.WeightsGB != 20 {
		t.Fatalf("spec mismatch: %+v", spec)
	}
	cfg.Model = Model{} // empty model section must refuse, not guess
	if _, err := ModelSpecFrom(cfg); err == nil {
		t.Fatal("empty model section must fail closed")
	}
}

func TestBoundaryOverheadPrecedence(t *testing.T) {
	cfg := testConfig()
	got, err := BoundaryOverhead(cfg, Profiles{"node-a": {BoundaryOverheadMS: 3.8}})
	if err != nil || got != 3.8 {
		t.Fatalf("measured boundary must win: %v %v", got, err)
	}
	got, err = BoundaryOverhead(cfg, Profiles{})
	if err != nil || got != 1.5 {
		t.Fatalf("declared fallback: %v %v", got, err)
	}
	cfg.BoundaryOverheadMS = 0
	if _, err := BoundaryOverhead(cfg, Profiles{}); err == nil {
		t.Fatal("neither measured nor declared must fail closed")
	}
}

func TestLoadProfilesMissingFileIsEmpty(t *testing.T) {
	p, err := LoadProfiles(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || len(p) != 0 {
		t.Fatalf("missing file must yield empty profiles: %v %v", p, err)
	}
}

func TestLoadProfilesReadsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.local.json")
	if err := os.WriteFile(path, []byte(`{"node-a":{"ms_per_layer":0.37,"boundary_overhead_ms":3.8}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadProfiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if p["node-a"].MsPerLayer != 0.37 {
		t.Fatalf("parse mismatch: %+v", p)
	}
}

func TestEndToEndSolveRolesFromConfig(t *testing.T) {
	// The consensus goal: config -> loader -> SolveRoles with real types.
	cfg := testConfig()
	nodes, err := NodeProfiles(cfg, Profiles{})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := ModelSpecFrom(cfg)
	if err != nil {
		t.Fatal(err)
	}
	boundary, err := BoundaryOverhead(cfg, Profiles{})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := partitioner.SolveRoles(nodes, spec, boundary, partitioner.RoleDemand{}, 0.15, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Decode.Plan.Stages) == 0 {
		t.Fatal("expected a decode plan from config-derived inputs")
	}
}
