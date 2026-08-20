"""Unit tests for the sofmat partitioner (pure stdlib, fictitious profiles).

Node profiles here are INVENTED round numbers (governance: no real infra in
code). They mimic the interesting shapes: a fast pair, a big-but-slow
unified-memory node, a small flaky node.
"""

import unittest

from solver import ModelSpec, NodeProfile, PartitionError, solve

# Fictitious pool: fast-a (24 GB), big-slow-b (cap 64 of 96), small-c (12 GB),
# fast-d (24 GB). Bandwidths in GB/s; overhead per boundary in ms.
FAST_A = NodeProfile("node-a", model_mem_cap_gb=24, mem_bandwidth_gbps=900)
BIG_B = NodeProfile("node-b", model_mem_cap_gb=64, mem_bandwidth_gbps=250)
SMALL_C = NodeProfile("node-c", model_mem_cap_gb=12, mem_bandwidth_gbps=900)
FAST_D = NodeProfile("node-d", model_mem_cap_gb=24, mem_bandwidth_gbps=900)
POOL = [FAST_A, BIG_B, SMALL_C, FAST_D]


def small_model(weights_gb=20.0, kv_gb=2.0, n_layers=40):
    return ModelSpec(n_layers=n_layers, weights_gb=weights_gb, kv_cache_gb=kv_gb)


class TestCostModelFindings(unittest.TestCase):
    """Regression tests for the two findings from the distributed review."""

    def test_zero_layers_rejected_at_construction(self):
        from solver import PartitionError

        with self.assertRaises(PartitionError):
            ModelSpec(n_layers=0, weights_gb=10.0, kv_cache_gb=1.0)

    def test_bandwidth_cost_includes_kv_bytes(self):
        # Same weights, KV as big as the weights: token time must ~double
        # in bandwidth mode (decode touches weights AND KV every token).
        light = ModelSpec(n_layers=40, weights_gb=10.0, kv_cache_gb=0.0)
        heavy = ModelSpec(n_layers=40, weights_gb=10.0, kv_cache_gb=10.0)
        node = NodeProfile("node-x", model_mem_cap_gb=64, mem_bandwidth_gbps=500)
        t_light = solve([node], light, boundary_overhead_ms=1.5).plan.token_ms
        t_heavy = solve([node], heavy, boundary_overhead_ms=1.5).plan.token_ms
        self.assertAlmostEqual(t_heavy / t_light, 2.0, places=2)


class TestHardConstraints(unittest.TestCase):
    def test_refuses_model_bigger_than_pool(self):
        huge = ModelSpec(n_layers=100, weights_gb=200.0, kv_cache_gb=20.0)
        with self.assertRaises(PartitionError):
            solve(POOL, huge, boundary_overhead_ms=1.5)

    def test_kv_counts_inside_the_cap(self):
        # 23 GB of weights fits a 24 GB cap alone; +3 GB KV must NOT.
        model = ModelSpec(n_layers=40, weights_gb=23.0, kv_cache_gb=3.0)
        result = solve([FAST_A, FAST_D], model, boundary_overhead_ms=1.5)
        self.assertGreater(len(result.plan.stages), 1)

    def test_fail_closed_without_profile(self):
        blind = NodeProfile("node-x", model_mem_cap_gb=64)
        with self.assertRaises(PartitionError):
            solve([blind], small_model(), boundary_overhead_ms=1.5)


class TestSpeedObjective(unittest.TestCase):
    """Decision 'speed floor': speed is the objective, parsimony a tie-break."""

    def test_default_maximizes_speed_two_fast_beat_one_slow(self):
        # 44 GB fits big-slow-b alone, but two fast nodes run it faster:
        # objective=speed (default) must split instead of parking on node-b.
        model = ModelSpec(n_layers=48, weights_gb=40.0, kv_cache_gb=4.0)
        result = solve(POOL, model, boundary_overhead_ms=1.5)
        self.assertNotEqual(result.plan.node_ids, ("node-b",))
        self.assertGreater(len(result.plan.stages), 1)

    def test_floor_met_with_fewest_hosts(self):
        # A generous floor that node-b alone already meets -> stay on 1 host.
        model = ModelSpec(n_layers=48, weights_gb=40.0, kv_cache_gb=4.0)
        one_host = solve(
            POOL, model, boundary_overhead_ms=1.5, min_usable_tokens_s=0
        )
        floor = 1000.0 / one_host.plan.token_ms  # exactly what 1 host gives
        result = solve(
            POOL, model, boundary_overhead_ms=1.5, min_usable_tokens_s=floor
        )
        self.assertEqual(result.plan.node_ids, ("node-b",))

    def test_unreachable_floor_falls_back_to_fastest(self):
        model = ModelSpec(n_layers=48, weights_gb=40.0, kv_cache_gb=4.0)
        fastest = solve(POOL, model, boundary_overhead_ms=1.5)
        result = solve(
            POOL, model, boundary_overhead_ms=1.5, min_usable_tokens_s=1e9
        )
        self.assertEqual(result.plan.token_ms, fastest.plan.token_ms)


class TestParsimonyFloorZero(unittest.TestCase):
    """min_usable_tokens_s=0 recovers pure host parsimony (legacy mode)."""

    def test_single_host_when_it_fits(self):
        result = solve(
            POOL, small_model(), boundary_overhead_ms=1.5, min_usable_tokens_s=0
        )
        self.assertEqual(len(result.plan.stages), 1)
        self.assertEqual(result.plan.network_ms, 0.0)

    def test_single_slow_host_wins_at_floor_zero(self):
        # 44 GB fits big-slow-b alone: parsimony mode keeps 1 host.
        model = ModelSpec(n_layers=48, weights_gb=40.0, kv_cache_gb=4.0)
        result = solve(
            POOL, model, boundary_overhead_ms=1.5, min_usable_tokens_s=0
        )
        self.assertEqual(result.plan.node_ids, ("node-b",))

    def test_uses_exactly_the_hosts_needed_not_all(self):
        # 70 GB exceeds any single cap; two hosts suffice → never three.
        model = ModelSpec(n_layers=70, weights_gb=66.0, kv_cache_gb=4.0)
        result = solve(
            POOL, model, boundary_overhead_ms=1.5, min_usable_tokens_s=0
        )
        self.assertEqual(len(result.plan.stages), 2)


class TestBottleneckBalance(unittest.TestCase):
    def test_fast_node_maxed_before_slow_node_absorbs_more(self):
        # 70 GB forces fast-a + big-slow-b; minimizing the slowest stage
        # should pin the fast node at (near) its cap.
        model = ModelSpec(n_layers=70, weights_gb=66.0, kv_cache_gb=4.0)
        result = solve([FAST_A, BIG_B], model, boundary_overhead_ms=1.5)
        by_id = {s.node_id: s for s in result.plan.stages}
        self.assertGreaterEqual(by_id["node-a"].mem_gb, 22.0)

    def test_big_slow_node_absorbs_when_capacity_demands(self):
        # 100 GB model: only fits using big-slow-b heavily (capacity mode).
        model = ModelSpec(n_layers=80, weights_gb=92.0, kv_cache_gb=8.0)
        result = solve(POOL, model, boundary_overhead_ms=1.5)
        by_id = {s.node_id: s for s in result.plan.stages}
        self.assertIn("node-b", by_id)
        self.assertGreater(by_id["node-b"].mem_gb, 40.0)


class TestNetworkBudget(unittest.TestCase):
    def test_network_fraction_respected(self):
        model = ModelSpec(n_layers=48, weights_gb=40.0, kv_cache_gb=4.0)
        result = solve(POOL, model, boundary_overhead_ms=1.5)
        self.assertLessEqual(result.plan.network_fraction, 0.15)

    def test_rejects_maps_dominated_by_network(self):
        # Tiny model forced across hosts by tiny caps + huge overhead:
        tiny_nodes = [
            NodeProfile("node-a", model_mem_cap_gb=3, mem_bandwidth_gbps=900),
            NodeProfile("node-b", model_mem_cap_gb=3, mem_bandwidth_gbps=900),
        ]
        model = ModelSpec(n_layers=8, weights_gb=5.0, kv_cache_gb=0.5)
        with self.assertRaises(PartitionError):
            solve(tiny_nodes, model, boundary_overhead_ms=50.0)


class TestElasticityAndFallbacks(unittest.TestCase):
    def test_absent_nodes_are_ignored(self):
        pool = [FAST_A, NodeProfile(
            "node-d", model_mem_cap_gb=24, mem_bandwidth_gbps=900, present=False
        )]
        result = solve(pool, small_model(), boundary_overhead_ms=1.5)
        self.assertEqual(result.plan.node_ids, ("node-a",))

    def test_n_minus_1_fallbacks_emitted(self):
        model = ModelSpec(n_layers=48, weights_gb=40.0, kv_cache_gb=4.0)
        result = solve(POOL, model, boundary_overhead_ms=1.5)
        for node_id in result.plan.node_ids:
            self.assertIn(node_id, result.fallbacks)
            fb = result.fallbacks[node_id]
            if fb is not None:
                self.assertNotIn(node_id, fb.node_ids)

    def test_fallback_none_when_pool_cannot_absorb_loss(self):
        # Model needs nearly the whole pool: losing big-b must be fatal.
        model = ModelSpec(n_layers=80, weights_gb=110.0, kv_cache_gb=8.0)
        result = solve(POOL, model, boundary_overhead_ms=1.5)
        self.assertIn("node-b", result.plan.node_ids)
        self.assertIsNone(result.fallbacks["node-b"])

    def test_layer_ranges_are_contiguous_and_complete(self):
        model = ModelSpec(n_layers=48, weights_gb=40.0, kv_cache_gb=4.0)
        plan = solve(POOL, model, boundary_overhead_ms=1.5).plan
        expected_first = 0
        for stage in plan.stages:
            self.assertEqual(stage.first_layer, expected_first)
            expected_first += stage.n_layers
        self.assertEqual(expected_first, model.n_layers)


if __name__ == "__main__":
    unittest.main()
