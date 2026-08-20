// 1:1 port of the Python contract suite (partitioner/test_solver.py).
// Node profiles are INVENTED round numbers (governance: no real infra).
package partitioner

import (
	"math"
	"testing"
)

func node(id string, capGB, bwGBps float64) NodeProfile {
	return NodeProfile{ID: id, ModelMemCapGB: capGB, MemBandwidthGBps: bwGBps}
}

var (
	fastA  = node("node-a", 24, 900)
	bigB   = node("node-b", 64, 250)
	smallC = node("node-c", 12, 900)
	fastD  = node("node-d", 24, 900)
	pool   = []NodeProfile{fastA, bigB, smallC, fastD}
)

func mustModel(t *testing.T, nLayers int, weightsGB, kvGB float64) ModelSpec {
	t.Helper()
	m, err := NewModelSpec(nLayers, weightsGB, kvGB, 1, 0)
	if err != nil {
		t.Fatalf("model: %v", err)
	}
	return m
}

func smallModel(t *testing.T) ModelSpec { return mustModel(t, 40, 20.0, 2.0) }

func mustSolve(t *testing.T, nodes []NodeProfile, m ModelSpec, minUsable *float64) PartitionResult {
	t.Helper()
	r, err := Solve(nodes, m, 1.5, 0.15, minUsable, true)
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	return r
}

func eqIDs(a []string, b ...string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- TestCostModelFindings ---

func TestZeroLayersRejectedAtConstruction(t *testing.T) {
	if _, err := NewModelSpec(0, 10.0, 1.0, 1, 0); err == nil {
		t.Fatal("expected PartitionError for n_layers=0")
	}
}

func TestBandwidthCostIncludesKVBytes(t *testing.T) {
	light := mustModel(t, 40, 10.0, 0.0)
	heavy := mustModel(t, 40, 10.0, 10.0)
	n := node("node-x", 64, 500)
	tLight := mustSolve(t, []NodeProfile{n}, light, nil).Plan.TokenMS
	tHeavy := mustSolve(t, []NodeProfile{n}, heavy, nil).Plan.TokenMS
	if ratio := tHeavy / tLight; math.Abs(ratio-2.0) > 0.01 {
		t.Fatalf("token time must ~double with KV as big as weights, ratio=%v", ratio)
	}
}

// --- TestHardConstraints ---

func TestRefusesModelBiggerThanPool(t *testing.T) {
	huge := mustModel(t, 100, 200.0, 20.0)
	if _, err := Solve(pool, huge, 1.5, 0.15, nil, true); err == nil {
		t.Fatal("expected PartitionError")
	}
}

func TestKVCountsInsideTheCap(t *testing.T) {
	m := mustModel(t, 40, 23.0, 3.0)
	r := mustSolve(t, []NodeProfile{fastA, fastD}, m, nil)
	if len(r.Plan.Stages) <= 1 {
		t.Fatalf("23 GB weights + 3 GB KV must NOT fit a 24 GB cap alone")
	}
}

func TestFailClosedWithoutProfile(t *testing.T) {
	blind := NodeProfile{ID: "node-x", ModelMemCapGB: 64}
	if _, err := Solve([]NodeProfile{blind}, smallModel(t), 1.5, 0.15, nil, true); err == nil {
		t.Fatal("expected PartitionError for node without profile")
	}
}

// --- TestSpeedObjective ---

func TestDefaultMaximizesSpeedTwoFastBeatOneSlow(t *testing.T) {
	m := mustModel(t, 48, 40.0, 4.0)
	r := mustSolve(t, pool, m, nil)
	if eqIDs(r.Plan.NodeIDs(), "node-b") {
		t.Fatal("objective=speed must split instead of parking on node-b")
	}
	if len(r.Plan.Stages) <= 1 {
		t.Fatal("expected multi-stage plan")
	}
}

func TestFloorMetWithFewestHosts(t *testing.T) {
	m := mustModel(t, 48, 40.0, 4.0)
	oneHost := mustSolve(t, pool, m, Floor(0))
	floor := 1000.0 / oneHost.Plan.TokenMS
	r := mustSolve(t, pool, m, Floor(floor))
	if !eqIDs(r.Plan.NodeIDs(), "node-b") {
		t.Fatalf("floor met by 1 host -> stay on node-b, got %v", r.Plan.NodeIDs())
	}
}

func TestUnreachableFloorFallsBackToFastest(t *testing.T) {
	m := mustModel(t, 48, 40.0, 4.0)
	fastest := mustSolve(t, pool, m, nil)
	r := mustSolve(t, pool, m, Floor(1e9))
	if r.Plan.TokenMS != fastest.Plan.TokenMS {
		t.Fatalf("unreachable floor must fall back to fastest plan")
	}
}

// --- TestParsimonyFloorZero ---

func TestSingleHostWhenItFits(t *testing.T) {
	r := mustSolve(t, pool, smallModel(t), Floor(0))
	if len(r.Plan.Stages) != 1 || r.Plan.NetworkMS != 0.0 {
		t.Fatalf("parsimony must keep one host, got %v", r.Plan.NodeIDs())
	}
}

func TestSingleSlowHostWinsAtFloorZero(t *testing.T) {
	m := mustModel(t, 48, 40.0, 4.0)
	r := mustSolve(t, pool, m, Floor(0))
	if !eqIDs(r.Plan.NodeIDs(), "node-b") {
		t.Fatalf("parsimony mode keeps 1 host (node-b), got %v", r.Plan.NodeIDs())
	}
}

func TestUsesExactlyTheHostsNeededNotAll(t *testing.T) {
	m := mustModel(t, 70, 66.0, 4.0)
	r := mustSolve(t, pool, m, Floor(0))
	if len(r.Plan.Stages) != 2 {
		t.Fatalf("two hosts suffice, got %d", len(r.Plan.Stages))
	}
}

// --- TestBottleneckBalance ---

func TestFastNodeMaxedBeforeSlowNodeAbsorbsMore(t *testing.T) {
	m := mustModel(t, 70, 66.0, 4.0)
	r := mustSolve(t, []NodeProfile{fastA, bigB}, m, nil)
	for _, s := range r.Plan.Stages {
		if s.NodeID == "node-a" && s.MemGB < 22.0 {
			t.Fatalf("fast node must be pinned near its cap, mem=%v", s.MemGB)
		}
	}
}

func TestBigSlowNodeAbsorbsWhenCapacityDemands(t *testing.T) {
	m := mustModel(t, 80, 92.0, 8.0)
	r := mustSolve(t, pool, m, nil)
	found := false
	for _, s := range r.Plan.Stages {
		if s.NodeID == "node-b" {
			found = true
			if s.MemGB <= 40.0 {
				t.Fatalf("node-b must absorb heavily, mem=%v", s.MemGB)
			}
		}
	}
	if !found {
		t.Fatal("node-b must be in the plan")
	}
}

// --- TestNetworkBudget ---

func TestNetworkFractionRespected(t *testing.T) {
	m := mustModel(t, 48, 40.0, 4.0)
	r := mustSolve(t, pool, m, nil)
	if r.Plan.NetworkFraction > 0.15 {
		t.Fatalf("network fraction %v > 0.15", r.Plan.NetworkFraction)
	}
}

func TestRejectsMapsDominatedByNetwork(t *testing.T) {
	tiny := []NodeProfile{node("node-a", 3, 900), node("node-b", 3, 900)}
	m := mustModel(t, 8, 5.0, 0.5)
	if _, err := Solve(tiny, m, 50.0, 0.15, nil, true); err == nil {
		t.Fatal("expected PartitionError for network-dominated maps")
	}
}

// --- TestElasticityAndFallbacks ---

func TestAbsentNodesAreIgnored(t *testing.T) {
	absentD := fastD
	absentD.Absent = true
	r := mustSolve(t, []NodeProfile{fastA, absentD}, smallModel(t), nil)
	if !eqIDs(r.Plan.NodeIDs(), "node-a") {
		t.Fatalf("absent node used: %v", r.Plan.NodeIDs())
	}
}

func TestNMinus1FallbacksEmitted(t *testing.T) {
	m := mustModel(t, 48, 40.0, 4.0)
	r := mustSolve(t, pool, m, nil)
	for _, nodeID := range r.Plan.NodeIDs() {
		fb, ok := r.Fallbacks[nodeID]
		if !ok {
			t.Fatalf("missing fallback entry for %s", nodeID)
		}
		if fb != nil {
			for _, id := range fb.NodeIDs() {
				if id == nodeID {
					t.Fatalf("fallback for %s contains itself", nodeID)
				}
			}
		}
	}
}

func TestFallbackNilWhenPoolCannotAbsorbLoss(t *testing.T) {
	m := mustModel(t, 80, 110.0, 8.0)
	r := mustSolve(t, pool, m, nil)
	inPlan := false
	for _, id := range r.Plan.NodeIDs() {
		if id == "node-b" {
			inPlan = true
		}
	}
	if !inPlan {
		t.Fatal("node-b must be in the plan")
	}
	if r.Fallbacks["node-b"] != nil {
		t.Fatal("losing node-b must be fatal (nil fallback)")
	}
}

func TestLayerRangesAreContiguousAndComplete(t *testing.T) {
	m := mustModel(t, 48, 40.0, 4.0)
	plan := mustSolve(t, pool, m, nil).Plan
	expectedFirst := 0
	for _, s := range plan.Stages {
		if s.FirstLayer != expectedFirst {
			t.Fatalf("stage %s first_layer=%d, want %d", s.NodeID, s.FirstLayer, expectedFirst)
		}
		expectedFirst += s.NLayers
	}
	if expectedFirst != m.NLayers {
		t.Fatalf("layers covered %d != %d", expectedFirst, m.NLayers)
	}
}

// --- TestBatchSlotsAndPrefixBudget ---

func TestBatchSlotsMultiplyKVInCapacity(t *testing.T) {
	one := mustModel(t, 40, 23.0, 0.5)
	four, err := NewModelSpec(40, 23.0, 0.5, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	pair := []NodeProfile{fastA, fastD}
	if len(mustSolve(t, pair, one, nil).Plan.Stages) != 1 {
		t.Fatal("1 slot must fit one 24 GB cap")
	}
	if len(mustSolve(t, pair, four, nil).Plan.Stages) <= 1 {
		t.Fatal("4 slots must force a split")
	}
}

func TestPrefixCacheCountsInsideTheCap(t *testing.T) {
	lean := mustModel(t, 40, 23.0, 0.5)
	cached, err := NewModelSpec(40, 23.0, 0.5, 1, 1.0)
	if err != nil {
		t.Fatal(err)
	}
	pair := []NodeProfile{fastA, fastD}
	if len(mustSolve(t, pair, lean, nil).Plan.Stages) != 1 {
		t.Fatal("lean must fit one cap")
	}
	if len(mustSolve(t, pair, cached, nil).Plan.Stages) <= 1 {
		t.Fatal("resident prefix cache must force a split")
	}
}

func TestResidentKVNotChargedToSingleStreamBandwidth(t *testing.T) {
	n := node("node-x", 64, 500)
	one := mustModel(t, 40, 20.0, 1.0)
	four, err := NewModelSpec(40, 20.0, 1.0, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	tOne := mustSolve(t, []NodeProfile{n}, one, nil).Plan.TokenMS
	tFour := mustSolve(t, []NodeProfile{n}, four, nil).Plan.TokenMS
	if math.Abs(tOne-tFour) > 1e-6 {
		t.Fatalf("token time must not change with batch_slots: %v vs %v", tOne, tFour)
	}
}

// --- TestReplicaPlanner ---

func TestTwoSmallReplicasBeatOnePipeline(t *testing.T) {
	r, err := SolveReplicas([]NodeProfile{fastA, fastD}, smallModel(t), 1.5, 0.15)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Replicas) != 2 {
		t.Fatalf("want 2 replicas, got %d", len(r.Replicas))
	}
	single := mustSolve(t, []NodeProfile{fastA}, smallModel(t), nil).Plan
	want := 2 * 1000.0 / single.TokenMS
	if math.Abs(r.AggregateTokensS-want) > 1e-6 {
		t.Fatalf("aggregate %v != %v", r.AggregateTokensS, want)
	}
}

func TestBigModelForcesOneSpanningReplica(t *testing.T) {
	m := mustModel(t, 70, 66.0, 4.0)
	r, err := SolveReplicas(pool, m, 1.5, 0.15)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Replicas) != 1 {
		t.Fatalf("want 1 replica, got %d", len(r.Replicas))
	}
}

func TestReplicasAreDisjointAndIdleReported(t *testing.T) {
	r, err := SolveReplicas(pool, smallModel(t), 1.5, 0.15)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	count := 0
	for _, p := range r.Replicas {
		for _, id := range p.NodeIDs() {
			if seen[id] {
				t.Fatalf("node %s in two replicas", id)
			}
			seen[id] = true
			count++
		}
	}
	if count+len(r.IdleNodeIDs) != len(pool) {
		t.Fatalf("used+idle=%d != pool=%d", count+len(r.IdleNodeIDs), len(pool))
	}
}

func TestNoReplicaFitsRaises(t *testing.T) {
	huge := mustModel(t, 100, 200.0, 20.0)
	if _, err := SolveReplicas(pool, huge, 1.5, 0.15); err == nil {
		t.Fatal("expected PartitionError")
	}
}

func TestReplicaCapacityContextTradeoffVisible(t *testing.T) {
	twoSlots, err := NewModelSpec(40, 20.0, 3.0, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	p := []NodeProfile{fastA, fastD, node("node-e", 24, 900), node("node-f", 24, 900)}
	r, err := SolveReplicas(p, twoSlots, 1.5, 0.15)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Replicas) != 2 {
		t.Fatalf("want 2 two-node replicas, got %d", len(r.Replicas))
	}
	for _, plan := range r.Replicas {
		if len(plan.Stages) != 2 {
			t.Fatalf("each replica must span 2 nodes, got %d", len(plan.Stages))
		}
	}
}

// --- TestSolveRoles ---

func demand(t *testing.T, prefill, draft bool, lead float64) RoleDemand {
	t.Helper()
	d := RoleDemand{DraftLeadFactor: lead}
	if prefill {
		m := mustModel(t, 40, 20.0, 8.0)
		d.PrefillModel = &m
	}
	if draft {
		m := mustModel(t, 12, 2.0, 0.5)
		d.DraftModel = &m
	}
	return d
}

func mustRoles(t *testing.T, nodes []NodeProfile, m ModelSpec, d RoleDemand) RolePlan {
	t.Helper()
	r, err := SolveRoles(nodes, m, 1.5, d, 0.15, nil)
	if err != nil {
		t.Fatalf("solve_roles: %v", err)
	}
	return r
}

func TestNoDemandDegeneratesToSolve(t *testing.T) {
	plan := mustRoles(t, pool, smallModel(t), RoleDemand{})
	if plan.PrefillNodeID != "" || plan.DraftNodeID != "" {
		t.Fatal("no roles demanded -> none assigned")
	}
	want := mustSolve(t, pool, smallModel(t), nil).Plan.TokenMS
	if plan.Decode.Plan.TokenMS != want {
		t.Fatalf("must degenerate to solve: %v != %v", plan.Decode.Plan.TokenMS, want)
	}
}

func TestRolesNeverColocateWithDecodeStages(t *testing.T) {
	plan := mustRoles(t, pool, smallModel(t), demand(t, true, true, 2.0))
	for _, id := range plan.Decode.Plan.NodeIDs() {
		if id == plan.PrefillNodeID || id == plan.DraftNodeID {
			t.Fatalf("role co-located with decode stage %s", id)
		}
	}
}

func TestDoubleRolePreferredWhenDecodeTies(t *testing.T) {
	m := mustModel(t, 40, 18.0, 2.0)
	plan := mustRoles(t, pool, m, demand(t, true, true, 2.0))
	if plan.PrefillNodeID != plan.DraftNodeID {
		t.Fatalf("double role must win: prefill=%s draft=%s", plan.PrefillNodeID, plan.DraftNodeID)
	}
}

func TestDraftThatCannotStayAheadIsRejected(t *testing.T) {
	if _, err := SolveRoles(pool, smallModel(t), 1.5, demand(t, true, true, 500.0), 0.15, nil); err == nil {
		t.Fatal("expected PartitionError (never silently drop the role)")
	}
}

func TestCapacityGateReleasesDraftNode(t *testing.T) {
	withDraft := mustRoles(t, pool, smallModel(t), demand(t, true, true, 2.0))
	without := mustRoles(t, pool, smallModel(t), demand(t, true, false, 2.0))
	if withDraft.DraftNodeID == "" {
		t.Fatal("draft demanded -> assigned")
	}
	if without.DraftNodeID != "" {
		t.Fatal("draft released")
	}
	if without.PrefillNodeID == "" {
		t.Fatal("prefill kept")
	}
}

func TestUnplaceablePrefillRaises(t *testing.T) {
	giant := mustModel(t, 40, 200.0, 50.0)
	d := RoleDemand{PrefillModel: &giant}
	if _, err := SolveRoles(pool, smallModel(t), 1.5, d, 0.15, nil); err == nil {
		t.Fatal("expected PartitionError")
	}
}

func TestDraftMSReportedForScheduler(t *testing.T) {
	plan := mustRoles(t, pool, smallModel(t), demand(t, true, true, 2.0))
	if plan.DraftMS <= 0 {
		t.Fatal("draft_ms must be reported")
	}
	if plan.DraftMS*2.0 > plan.Decode.Plan.TokenMS+1e-9 {
		t.Fatalf("draft %v cannot stay 2x ahead of %v", plan.DraftMS, plan.Decode.Plan.TokenMS)
	}
}
