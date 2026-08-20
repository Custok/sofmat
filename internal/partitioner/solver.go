// Package partitioner — heterogeneous pipeline-parallel layer assignment.
//
// Go port of the validated Python prototype (partitioner/solver.py); the
// Python test suite is the behavioral contract and is ported 1:1 in
// solver_test.go. Design of record (README.md, "Invariantes de diseño"):
//
//  1. HARD constraint: weights + KV-cache budget must fit inside each node's
//     ModelMemCapGB (fail-closed: no measured/declared profile -> refuse).
//  2. Objective (decision "speed floor"): capacity is the hard constraint;
//     among valid partitions the objective is SPEED. MinUsableTokensS is a
//     floor: fewest hosts that reach it; unreachable -> maximize speed;
//     0 -> pure host parsimony; nil -> maximize speed.
//  3. Ties broken toward fewer hosts.
//  4. Reject any map where the network fraction of token time exceeds
//     networkTimeBudget (KPI: "transparent to the network", <10-15%).
//  5. Emit N-1 fallback maps so the coordinator can re-shard instantly.
//
// Pure stdlib on purpose: the solver is arithmetic, not ML. Profiles must
// come measured or user-declared in config — never baked-in defaults.
package partitioner

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// PartitionError is raised when no valid partition exists for the inputs.
type PartitionError struct{ msg string }

func (e *PartitionError) Error() string { return e.msg }

func errf(format string, args ...any) *PartitionError {
	return &PartitionError{msg: fmt.Sprintf(format, args...)}
}

// NodeProfile is one pool member, as described in config + measured by the
// runtime microbench. ModelMemCapGB is the memory usable for weights+KV on
// this node — NOT the total memory. Exactly one of MsPerLayer (measured,
// preferred) or MemBandwidthGBps (declared fallback) must be > 0; with
// neither, the solver refuses the node (fail-closed). The zero value of
// Absent means present (mirrors the Python default present=True).
type NodeProfile struct {
	ID               string
	ModelMemCapGB    float64
	MsPerLayer       float64 // 0 = not measured
	MemBandwidthGBps float64 // 0 = not declared
	Absent           bool
}

// StageMS estimates per-token compute time for nLayers on this node.
// gbPerLayer must be the TOTAL bytes touched per layer per token (weights +
// this layer's share of KV-cache) — decode reads both.
func (n NodeProfile) StageMS(nLayers int, gbPerLayer float64) (float64, error) {
	if n.MsPerLayer > 0 {
		return float64(nLayers) * n.MsPerLayer, nil
	}
	if n.MemBandwidthGBps > 0 {
		// Decode is memory-bound: time ~ bytes touched / memory bandwidth.
		return float64(nLayers) * gbPerLayer / n.MemBandwidthGBps * 1000.0, nil
	}
	return 0, errf(
		"node %s: no measured MsPerLayer and no declared MemBandwidthGBps — refusing to guess (fail-closed)",
		n.ID)
}

// ModelSpec is the model to place: weights + KV budget for the configured
// context. KVCacheGB is the KV budget of ONE serving slot at max_context;
// BatchSlots multiplies it. PrefixCacheGB is KV kept RESIDENT for cached
// shared prefixes (single-copy figure under slot-affinity).
type ModelSpec struct {
	NLayers       int
	WeightsGB     float64
	KVCacheGB     float64
	BatchSlots    int
	PrefixCacheGB float64
}

// NewModelSpec validates at construction (contract: zero layers rejected).
// batchSlots 0 is normalized to 1 for ergonomics with struct-literal-free
// call sites; negative values are rejected like the Python __post_init__.
func NewModelSpec(nLayers int, weightsGB, kvCacheGB float64, batchSlots int, prefixCacheGB float64) (ModelSpec, error) {
	if batchSlots == 0 {
		batchSlots = 1
	}
	m := ModelSpec{
		NLayers: nLayers, WeightsGB: weightsGB, KVCacheGB: kvCacheGB,
		BatchSlots: batchSlots, PrefixCacheGB: prefixCacheGB,
	}
	if m.NLayers < 1 {
		return m, errf("n_layers must be >= 1, got %d", m.NLayers)
	}
	if m.BatchSlots < 1 {
		return m, errf("batch_slots must be >= 1, got %d", m.BatchSlots)
	}
	if m.WeightsGB <= 0 || m.KVCacheGB < 0 || m.PrefixCacheGB < 0 {
		return m, errf("invalid sizes: weights_gb=%v, kv_cache_gb=%v, prefix_cache_gb=%v",
			m.WeightsGB, m.KVCacheGB, m.PrefixCacheGB)
	}
	return m, nil
}

// KVTotalGB is the resident KV: all serving slots plus cached prefixes.
func (m ModelSpec) KVTotalGB() float64 {
	return m.KVCacheGB*float64(m.BatchSlots) + m.PrefixCacheGB
}

// GBPerLayerTotal is bytes touched per layer per SINGLE-STREAM token:
// weights + one slot's KV share (other slots are resident, not read).
func (m ModelSpec) GBPerLayerTotal() float64 {
	return (m.WeightsGB + m.KVCacheGB) / float64(m.NLayers)
}

// GBPerLayerResident is memory HELD per layer (capacity term).
func (m ModelSpec) GBPerLayerResident() float64 {
	return (m.WeightsGB + m.KVTotalGB()) / float64(m.NLayers)
}

// TotalGB is the full resident footprint of one replica.
func (m ModelSpec) TotalGB() float64 { return m.WeightsGB + m.KVTotalGB() }

func (m ModelSpec) gbPerLayer() float64 { return m.WeightsGB / float64(m.NLayers) }

// Stage is one pipeline stage of a partition plan.
type Stage struct {
	NodeID     string
	FirstLayer int // inclusive
	NLayers    int
	MemGB      float64 // weights + proportional KV held by this stage
	StageMS    float64
}

// PartitionPlan is one valid node->layers map with its cost split.
type PartitionPlan struct {
	Stages          []Stage
	TokenMS         float64
	NetworkMS       float64
	NetworkFraction float64
}

// NodeIDs returns the stage node ids in pipeline order.
func (p PartitionPlan) NodeIDs() []string {
	ids := make([]string, len(p.Stages))
	for i, s := range p.Stages {
		ids[i] = s.NodeID
	}
	return ids
}

// PartitionResult is the chosen plan plus N-1 fallbacks:
// Fallbacks[nodeID] = best plan WITHOUT that node (nil if impossible).
type PartitionResult struct {
	Plan      PartitionPlan
	Fallbacks map[string]*PartitionPlan
}

func fits(nodes []NodeProfile, model ModelSpec) bool {
	sum := 0.0
	for _, n := range nodes {
		sum += n.ModelMemCapGB
	}
	return sum >= model.TotalGB()
}

// assignLayers splits model.NLayers across subset minimizing per-token time.
// EXACT: with a constant per-layer cost per node the sum is minimized by
// loading the fastest nodes up to their memory caps (greedy by speed —
// provably optimal for the sum objective). Every member must hold >= 1
// layer. Returns nil when the caps cannot hold the model.
func assignLayers(subset []NodeProfile, model ModelSpec) ([]int, error) {
	gbLayer := model.GBPerLayerResident() // capacity term (weights + ALL KV)
	caps := make([]int, len(subset))
	total := 0
	for i, n := range subset {
		caps[i] = int(n.ModelMemCapGB / gbLayer)
		if caps[i] < 1 {
			return nil, nil
		}
		total += caps[i]
	}
	if total < model.NLayers {
		return nil, nil
	}
	counts := make([]int, len(subset))
	for i := range counts {
		counts[i] = 1
	}
	remaining := model.NLayers - len(subset)
	if remaining < 0 {
		return nil, nil
	}
	// Fastest node first (lowest ms per layer), fill to cap.
	order := make([]int, len(subset))
	speeds := make([]float64, len(subset))
	for i, n := range subset {
		ms, err := n.StageMS(1, model.GBPerLayerTotal())
		if err != nil {
			return nil, err
		}
		order[i], speeds[i] = i, ms
	}
	sort.SliceStable(order, func(a, b int) bool { return speeds[order[a]] < speeds[order[b]] })
	for _, i := range order {
		take := min(remaining, caps[i]-counts[i])
		counts[i] += take
		remaining -= take
		if remaining == 0 {
			break
		}
	}
	if remaining != 0 {
		return nil, nil
	}
	return counts, nil
}

func planForSubset(subset []NodeProfile, model ModelSpec, boundaryOverheadMS, networkTimeBudget float64) (*PartitionPlan, error) {
	counts, err := assignLayers(subset, model)
	if err != nil {
		return nil, err
	}
	if counts == nil {
		return nil, nil
	}
	gbLayerKV := model.KVTotalGB() / float64(model.NLayers)
	stages := make([]Stage, 0, len(subset))
	first := 0
	for i, node := range subset {
		ms, err := node.StageMS(counts[i], model.GBPerLayerTotal())
		if err != nil {
			return nil, err
		}
		stages = append(stages, Stage{
			NodeID:     node.ID,
			FirstLayer: first,
			NLayers:    counts[i],
			MemGB:      float64(counts[i]) * (model.gbPerLayer() + gbLayerKV),
			StageMS:    ms,
		})
		first += counts[i]
	}
	networkMS := float64(len(subset)-1) * boundaryOverheadMS
	computeMS := 0.0
	for _, s := range stages {
		computeMS += s.StageMS
	}
	tokenMS := computeMS + networkMS
	fraction := 0.0
	if tokenMS > 0 {
		fraction = networkMS / tokenMS
	}
	if len(subset) > 1 && fraction > networkTimeBudget {
		return nil, nil // invariant 4: never emit a network-dominated map
	}
	return &PartitionPlan{
		Stages: stages, TokenMS: tokenMS, NetworkMS: networkMS, NetworkFraction: fraction,
	}, nil
}

// Floor wraps a tokens/s floor for Solve's MinUsableTokensS parameter
// (nil = maximize speed; Floor(0) = pure host parsimony).
func Floor(v float64) *float64 { return &v }

func presentSorted(nodes []NodeProfile) []NodeProfile {
	present := make([]NodeProfile, 0, len(nodes))
	for _, n := range nodes {
		if !n.Absent {
			present = append(present, n)
		}
	}
	sort.Slice(present, func(a, b int) bool { return present[a].ID < present[b].ID })
	return present
}

func combinations(items []NodeProfile, k int, fn func([]NodeProfile) error) error {
	idx := make([]int, k)
	var rec func(start, depth int) error
	rec = func(start, depth int) error {
		if depth == k {
			subset := make([]NodeProfile, k)
			for i, j := range idx {
				subset[i] = items[j]
			}
			return fn(subset)
		}
		for i := start; i <= len(items)-(k-depth); i++ {
			idx[depth] = i
			if err := rec(i+1, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if k == 0 {
		return fn(nil)
	}
	return rec(0, 0)
}

// Solve computes the partition plan per the design-of-record objective.
// Returns a PartitionError when the model cannot fit the present nodes
// under the caps + KV budget, or when every fitting map violates the
// network KPI.
func Solve(nodes []NodeProfile, model ModelSpec, boundaryOverheadMS, networkTimeBudget float64, minUsableTokensS *float64, withFallbacks bool) (PartitionResult, error) {
	var zero PartitionResult
	present := presentSorted(nodes)
	if len(present) == 0 {
		return zero, errf("no nodes present in the pool")
	}
	if !fits(present, model) {
		sum := 0.0
		for _, n := range present {
			sum += n.ModelMemCapGB
		}
		return zero, errf(
			"model needs %.1f GB (weights+KV) but present caps sum %.1f GB",
			model.TotalGB(), sum)
	}

	// Best valid plan for each pipeline depth k (ties inside k -> fastest).
	bestPerK := map[int]*PartitionPlan{}
	for k := 1; k <= len(present); k++ {
		err := combinations(present, k, func(subset []NodeProfile) error {
			plan, err := planForSubset(subset, model, boundaryOverheadMS, networkTimeBudget)
			if err != nil {
				return err
			}
			if plan != nil && (bestPerK[k] == nil || plan.TokenMS < bestPerK[k].TokenMS) {
				bestPerK[k] = plan
			}
			return nil
		})
		if err != nil {
			return zero, err
		}
	}
	if len(bestPerK) == 0 {
		return zero, errf(
			"model fits by raw capacity but no map satisfies the network budget (%.0f%%) — stages too small for the boundary overhead; add memory per node or accept higher budget",
			networkTimeBudget*100)
	}

	fastest := func() *PartitionPlan {
		var best *PartitionPlan
		for _, p := range bestPerK {
			if best == nil || p.TokenMS < best.TokenMS ||
				(p.TokenMS == best.TokenMS && len(p.Stages) < len(best.Stages)) {
				best = p
			}
		}
		return best
	}

	var best *PartitionPlan
	if minUsableTokensS == nil {
		best = fastest()
	} else {
		floorMS := math.Inf(1)
		if *minUsableTokensS > 0 {
			floorMS = 1000.0 / *minUsableTokensS
		}
		ks := make([]int, 0, len(bestPerK))
		for k := range bestPerK {
			ks = append(ks, k)
		}
		sort.Ints(ks)
		for _, k := range ks {
			if bestPerK[k].TokenMS <= floorMS {
				best = bestPerK[k]
				break
			}
		}
		if best == nil {
			best = fastest()
		}
	}

	fallbacks := map[string]*PartitionPlan{}
	if withFallbacks {
		for _, nodeID := range best.NodeIDs() {
			rest := make([]NodeProfile, 0, len(present)-1)
			for _, n := range present {
				if n.ID != nodeID {
					rest = append(rest, n)
				}
			}
			sub, err := Solve(rest, model, boundaryOverheadMS, networkTimeBudget, minUsableTokensS, false)
			if err != nil {
				fallbacks[nodeID] = nil
			} else {
				plan := sub.Plan
				fallbacks[nodeID] = &plan
			}
		}
	}
	return PartitionResult{Plan: *best, Fallbacks: fallbacks}, nil
}

// ReplicaPlan is a service-level plan: independent full replicas over
// disjoint sub-pools. AggregateTokensS is a PROXY (sum of single-stream
// speeds); the per-replica occupancy curve is the v1 refinement.
type ReplicaPlan struct {
	Replicas         []PartitionPlan
	AggregateTokensS float64
	IdleNodeIDs      []string
}

func setKey(ids map[string]bool) string {
	keys := make([]string, 0, len(ids))
	for id := range ids {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	return strings.Join(keys, "|")
}

// SolveReplicas places N independent replicas of model on disjoint
// sub-pools, maximizing aggregate throughput; ties break toward FEWER
// replicas. Exact search, memoized over node subsets.
func SolveReplicas(nodes []NodeProfile, model ModelSpec, boundaryOverheadMS, networkTimeBudget float64) (ReplicaPlan, error) {
	var zero ReplicaPlan
	present := presentSorted(nodes)
	if len(present) == 0 {
		return zero, errf("no nodes present in the pool")
	}
	byID := map[string]NodeProfile{}
	for _, n := range present {
		byID[n.ID] = n
	}

	planCache := map[string]*PartitionPlan{}
	planFor := func(ids map[string]bool) (*PartitionPlan, error) {
		key := setKey(ids)
		if plan, ok := planCache[key]; ok {
			return plan, nil
		}
		group := make([]NodeProfile, 0, len(ids))
		for id := range ids {
			group = append(group, byID[id])
		}
		result, err := Solve(group, model, boundaryOverheadMS, networkTimeBudget, nil, false)
		if err != nil {
			var pe *PartitionError
			if ok := asPartitionError(err, &pe); ok {
				planCache[key] = nil
				return nil, nil
			}
			return nil, err
		}
		plan := result.Plan
		planCache[key] = &plan
		return &plan, nil
	}

	type assignment struct {
		agg   float64
		plans []PartitionPlan
	}
	bestCache := map[string]assignment{}

	var bestAssignment func(ids map[string]bool) (assignment, error)
	bestAssignment = func(ids map[string]bool) (assignment, error) {
		if len(ids) == 0 {
			return assignment{}, nil
		}
		key := setKey(ids)
		if a, ok := bestCache[key]; ok {
			return a, nil
		}
		first := ""
		for id := range ids {
			if first == "" || id < first {
				first = id
			}
		}
		// Option: first stays idle.
		rest := map[string]bool{}
		for id := range ids {
			if id != first {
				rest[id] = true
			}
		}
		best, err := bestAssignment(rest)
		if err != nil {
			return assignment{}, err
		}
		// Options: first serves in a replica with each subset of the rest.
		restIDs := make([]string, 0, len(rest))
		for id := range rest {
			restIDs = append(restIDs, id)
		}
		sort.Strings(restIDs)
		for mask := 0; mask < 1<<len(restIDs); mask++ {
			group := map[string]bool{first: true}
			for i, id := range restIDs {
				if mask&(1<<i) != 0 {
					group[id] = true
				}
			}
			plan, err := planFor(group)
			if err != nil {
				return assignment{}, err
			}
			if plan == nil {
				continue
			}
			remaining := map[string]bool{}
			for id := range ids {
				if !group[id] {
					remaining[id] = true
				}
			}
			sub, err := bestAssignment(remaining)
			if err != nil {
				return assignment{}, err
			}
			agg := 1000.0/plan.TokenMS + sub.agg
			candPlans := append([]PartitionPlan{*plan}, sub.plans...)
			// Higher aggregate wins; ties (within rounding) -> fewer replicas.
			if agg > best.agg+1e-9 ||
				(math.Abs(agg-best.agg) <= 1e-9 && len(candPlans) < len(best.plans)) {
				best = assignment{agg: agg, plans: candPlans}
			}
		}
		bestCache[key] = best
		return best, nil
	}

	all := map[string]bool{}
	for id := range byID {
		all[id] = true
	}
	best, err := bestAssignment(all)
	if err != nil {
		return zero, err
	}
	if len(best.plans) == 0 {
		return zero, errf(
			"model needs %.1f GB (weights+KV) per replica but no sub-pool of the present nodes can hold one",
			model.TotalGB())
	}
	used := map[string]bool{}
	for _, p := range best.plans {
		for _, id := range p.NodeIDs() {
			used[id] = true
		}
	}
	idle := []string{}
	for _, n := range present {
		if !used[n.ID] {
			idle = append(idle, n.ID)
		}
	}
	sort.Slice(best.plans, func(a, b int) bool {
		return strings.Join(best.plans[a].NodeIDs(), "|") < strings.Join(best.plans[b].NodeIDs(), "|")
	})
	return ReplicaPlan{Replicas: best.plans, AggregateTokensS: best.agg, IdleNodeIDs: idle}, nil
}

func asPartitionError(err error, target **PartitionError) bool {
	pe, ok := err.(*PartitionError)
	if ok {
		*target = pe
	}
	return ok
}

// RoleDemand declares dedicated roles wanted BESIDES the decode pipeline
// (disaggregation + F2). PrefillModel is the FULL model as resident on one
// prefill node (weights + KV of the longest admitted prompt). DraftModel is
// the small external draft of continuous speculation; DraftLeadFactor is
// how many times faster than the decode traversal the draft must run
// (measured reference ~4x; must be > 1). A nil model means "not demanded" —
// the scheduler capacity gate (spec §5.1) re-solves with DraftModel=nil,
// releasing the draft node back to the pool.
type RoleDemand struct {
	PrefillModel    *ModelSpec
	DraftModel      *ModelSpec
	DraftLeadFactor float64 // 0 = default 2.0
}

func (d RoleDemand) leadFactor() float64 {
	if d.DraftLeadFactor == 0 {
		return 2.0
	}
	return d.DraftLeadFactor
}

// RolePlan is the full disaggregated map: node -> role -> layers. Decode
// keeps the N-1 fallbacks of Solve (restricted to the decode sub-pool).
// PrefillNodeID / DraftNodeID may name the SAME node (double role); empty
// when the role was not demanded. DraftMS is the draft's per-token time on
// its node (consumed by the c*/L* scheduler pacing).
type RolePlan struct {
	Decode        PartitionResult
	PrefillNodeID string
	DraftNodeID   string
	DraftMS       float64 // 0 when no draft role
	IdleNodeIDs   []string
}

func residentMS(node NodeProfile, model ModelSpec) (float64, error) {
	return node.StageMS(model.NLayers, model.GBPerLayerTotal())
}

// SolveRoles assigns roles (prefill / draft / decode stage) across the
// pool. Isolation is the point of both roles, so neither may share a node
// with a decode stage; prefill and draft MAY share one node when its cap
// holds both residents. Objective in order: decode speed (live product),
// fewer reserved nodes, faster prefill node. The draft placement is
// validated against the CHOSEN decode plan (>= leadFactor x faster than
// the decode traversal). Fail-closed throughout.
func SolveRoles(nodes []NodeProfile, decodeModel ModelSpec, boundaryOverheadMS float64, demand RoleDemand, networkTimeBudget float64, minUsableTokensS *float64) (RolePlan, error) {
	var zero RolePlan
	if demand.leadFactor() <= 1.0 {
		return zero, errf("draft_lead_factor must be > 1 (draft strictly faster than decode), got %v",
			demand.DraftLeadFactor)
	}
	present := presentSorted(nodes)
	if len(present) == 0 {
		return zero, errf("no nodes present in the pool")
	}

	wantPrefill := demand.PrefillModel != nil
	wantDraft := demand.DraftModel != nil
	if !wantPrefill && !wantDraft {
		decode, err := Solve(present, decodeModel, boundaryOverheadMS, networkTimeBudget, minUsableTokensS, true)
		if err != nil {
			return zero, err
		}
		used := map[string]bool{}
		for _, id := range decode.Plan.NodeIDs() {
			used[id] = true
		}
		idle := []string{}
		for _, n := range present {
			if !used[n.ID] {
				idle = append(idle, n.ID)
			}
		}
		return RolePlan{Decode: decode, IdleNodeIDs: idle}, nil
	}

	fitsRoles := func(node NodeProfile, models ...*ModelSpec) bool {
		sum := 0.0
		for _, m := range models {
			sum += m.TotalGB()
		}
		return node.ModelMemCapGB >= sum
	}

	// Candidate reservations: (prefillNodeID, draftNodeID), "" = role unset.
	type reservation struct{ prefill, draft string }
	reservations := []reservation{}
	switch {
	case wantPrefill && wantDraft:
		for _, n := range present { // double role on one node
			if fitsRoles(n, demand.PrefillModel, demand.DraftModel) {
				reservations = append(reservations, reservation{n.ID, n.ID})
			}
		}
		for _, p := range present {
			for _, d := range present {
				if p.ID != d.ID && fitsRoles(p, demand.PrefillModel) && fitsRoles(d, demand.DraftModel) {
					reservations = append(reservations, reservation{p.ID, d.ID})
				}
			}
		}
	case wantPrefill:
		for _, n := range present {
			if fitsRoles(n, demand.PrefillModel) {
				reservations = append(reservations, reservation{prefill: n.ID})
			}
		}
	default:
		for _, n := range present {
			if fitsRoles(n, demand.DraftModel) {
				reservations = append(reservations, reservation{draft: n.ID})
			}
		}
	}
	if len(reservations) == 0 {
		pGB, dGB := 0.0, 0.0
		if wantPrefill {
			pGB = demand.PrefillModel.TotalGB()
		}
		if wantDraft {
			dGB = demand.DraftModel.TotalGB()
		}
		return zero, errf(
			"no node can hold the demanded role resident(s): prefill=%.1f GB, draft=%.1f GB", pGB, dGB)
	}

	byID := map[string]NodeProfile{}
	for _, n := range present {
		byID[n.ID] = n
	}
	type scored struct {
		tokenMS   float64
		nReserved int
		prefillMS float64
		plan      RolePlan
	}
	var best *scored
	var lastErr error
	for _, r := range reservations {
		reserved := map[string]bool{}
		if r.prefill != "" {
			reserved[r.prefill] = true
		}
		if r.draft != "" {
			reserved[r.draft] = true
		}
		decodePool := make([]NodeProfile, 0, len(present))
		for _, n := range present {
			if !reserved[n.ID] {
				decodePool = append(decodePool, n)
			}
		}
		decode, err := Solve(decodePool, decodeModel, boundaryOverheadMS, networkTimeBudget, minUsableTokensS, true)
		if err != nil {
			lastErr = err
			continue
		}
		draftMS := 0.0
		if r.draft != "" {
			draftMS, err = residentMS(byID[r.draft], *demand.DraftModel)
			if err != nil {
				lastErr = err
				continue
			}
			if draftMS*demand.leadFactor() > decode.Plan.TokenMS {
				lastErr = errf(
					"draft on %s runs %.1f ms/token — cannot stay %.1fx ahead of a %.1f ms decode traversal",
					r.draft, draftMS, demand.leadFactor(), decode.Plan.TokenMS)
				continue
			}
		}
		prefillMS := 0.0
		if r.prefill != "" {
			prefillMS, err = residentMS(byID[r.prefill], *demand.PrefillModel)
			if err != nil {
				lastErr = err
				continue
			}
		}
		cand := scored{
			tokenMS: decode.Plan.TokenMS, nReserved: len(reserved), prefillMS: prefillMS,
		}
		if best == nil ||
			cand.tokenMS < best.tokenMS ||
			(cand.tokenMS == best.tokenMS && cand.nReserved < best.nReserved) ||
			(cand.tokenMS == best.tokenMS && cand.nReserved == best.nReserved && cand.prefillMS < best.prefillMS) {
			used := map[string]bool{}
			for _, id := range decode.Plan.NodeIDs() {
				used[id] = true
			}
			for id := range reserved {
				used[id] = true
			}
			idle := []string{}
			for _, n := range present {
				if !used[n.ID] {
					idle = append(idle, n.ID)
				}
			}
			cand.plan = RolePlan{
				Decode:        decode,
				PrefillNodeID: r.prefill,
				DraftNodeID:   r.draft,
				DraftMS:       draftMS,
				IdleNodeIDs:   idle,
			}
			best = &cand
		}
	}
	if best == nil {
		return zero, errf(
			"roles reservable but no assignment leaves a valid decode pipeline (last failure: %v)", lastErr)
	}
	return best.plan, nil
}
