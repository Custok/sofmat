"""Tests for strict config loading (JSON path = stdlib-only, no PyYAML needed).

All fixtures use fictitious placeholder values (governance: no real infra).
"""

import json
import os
import unittest

from config import ClusterConfig, ConfigError, load_cluster_config


def base_config() -> dict:
    return {
        "cluster": {
            "master": "node-a",
            "transport": {"backend": "tcp", "port": 50051},
            "nodes": [
                {"id": "node-a", "vram_gb": 24, "mem_bandwidth_gbps": 900},
                {
                    "id": "node-b",
                    "vram_gb": 96,
                    "model_mem_cap_gb": 64,
                    "mem_bandwidth_gbps": 250,
                },
            ],
        },
        "partitioner": {"objective": "speed", "network_time_budget": 0.15},
    }


def load(cfg: dict) -> ClusterConfig:
    return load_cluster_config(json.dumps(cfg), fmt="json")


class TestHappyPath(unittest.TestCase):
    def test_minimal_valid_config(self):
        cc = load(base_config())
        self.assertEqual(cc.master, "node-a")
        self.assertEqual(len(cc.nodes), 2)
        self.assertIsNone(cc.settings.min_usable_tokens_s)  # speed default

    def test_cap_defaults_to_vram(self):
        cc = load(base_config())
        by_id = {n.id: n for n in cc.nodes}
        self.assertEqual(by_id["node-a"].model_mem_cap_gb, 24.0)
        self.assertEqual(by_id["node-b"].model_mem_cap_gb, 64.0)

    def test_capacity_objective_maps_to_parsimony_floor(self):
        cfg = base_config()
        cfg["partitioner"]["objective"] = "capacity"
        cc = load(cfg)
        self.assertEqual(cc.settings.min_usable_tokens_s, 0.0)


class TestFailClosed(unittest.TestCase):
    def test_unknown_node_key_rejected(self):
        cfg = base_config()
        cfg["cluster"]["nodes"][0]["favourite_color"] = "green"
        with self.assertRaises(ConfigError):
            load(cfg)

    def test_unknown_partitioner_key_rejected(self):
        cfg = base_config()
        cfg["partitioner"]["turbo"] = True
        with self.assertRaises(ConfigError):
            load(cfg)

    def test_bad_node_id_rejected(self):
        cfg = base_config()
        cfg["cluster"]["nodes"][0]["id"] = "Node A!"  # not an anonymous label
        with self.assertRaises(ConfigError):
            load(cfg)

    def test_duplicate_node_id_rejected(self):
        cfg = base_config()
        cfg["cluster"]["nodes"][1]["id"] = "node-a"
        with self.assertRaises(ConfigError):
            load(cfg)

    def test_master_must_be_declared_node(self):
        cfg = base_config()
        cfg["cluster"]["master"] = "node-z"
        with self.assertRaises(ConfigError):
            load(cfg)

    def test_cap_above_vram_rejected(self):
        cfg = base_config()
        cfg["cluster"]["nodes"][0]["model_mem_cap_gb"] = 999
        with self.assertRaises(ConfigError):
            load(cfg)

    def test_wrong_type_rejected(self):
        cfg = base_config()
        cfg["cluster"]["nodes"][0]["vram_gb"] = "mucho"
        with self.assertRaises(ConfigError):
            load(cfg)

    def test_bool_is_not_a_number(self):
        cfg = base_config()
        cfg["cluster"]["nodes"][0]["vram_gb"] = True
        with self.assertRaises(ConfigError):
            load(cfg)

    def test_unknown_transport_backend_rejected(self):
        cfg = base_config()
        cfg["cluster"]["transport"]["backend"] = "carrier-pigeon"
        with self.assertRaises(ConfigError):
            load(cfg)

    def test_port_out_of_range_rejected(self):
        cfg = base_config()
        cfg["cluster"]["transport"]["port"] = 70000
        with self.assertRaises(ConfigError):
            load(cfg)

    def test_network_budget_out_of_range_rejected(self):
        cfg = base_config()
        cfg["partitioner"]["network_time_budget"] = 1.5
        with self.assertRaises(ConfigError):
            load(cfg)

    def test_invalid_json_rejected(self):
        with self.assertRaises(ConfigError):
            load_cluster_config("{not json", fmt="json")


class TestSecrets(unittest.TestCase):
    def test_env_ref_must_exist(self):
        cfg = base_config()
        cfg["cluster"]["transport"]["auth_token"] = "${SOFMAT_TEST_TOKEN_MISSING}"
        os.environ.pop("SOFMAT_TEST_TOKEN_MISSING", None)
        with self.assertRaises(ConfigError) as ctx:
            load(cfg)
        # A02: the error names the VARIABLE, never a value.
        self.assertIn("SOFMAT_TEST_TOKEN_MISSING", str(ctx.exception))

    def test_env_ref_resolves_but_value_not_retained(self):
        cfg = base_config()
        cfg["cluster"]["transport"]["auth_token"] = "${SOFMAT_TEST_TOKEN}"
        os.environ["SOFMAT_TEST_TOKEN"] = "supersecret-fixture"
        try:
            cc = load(cfg)
            # The partitioner's view of the config holds NO secret material.
            self.assertNotIn("supersecret-fixture", repr(cc))
        finally:
            del os.environ["SOFMAT_TEST_TOKEN"]


class TestSolverIntegration(unittest.TestCase):
    def test_profiles_feed_solve_directly(self):
        from solver import ModelSpec, solve

        cc = load(base_config())
        model = ModelSpec(n_layers=40, weights_gb=30.0, kv_cache_gb=2.0)
        result = solve(
            list(cc.nodes),
            model,
            boundary_overhead_ms=1.5,
            network_time_budget=cc.settings.network_time_budget,
            min_usable_tokens_s=cc.settings.min_usable_tokens_s,
        )
        self.assertGreaterEqual(len(result.plan.stages), 1)


if __name__ == "__main__":
    unittest.main()
