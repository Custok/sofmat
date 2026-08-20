"""End-to-end tests for the reference CPU backend. Pure stdlib unittest.

The whole point: prove the pipeline runs correctly WITHOUT a GPU, and that
splitting the model across stages/hosts reproduces the single-host result
exactly — the assertion a "street user" runs to trust the install.

Run:  python3 -m unittest test_reference   (from the runtime/ directory)
"""

from __future__ import annotations

import array
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(__file__))

from worker import StageSpec, WorkerError  # noqa: E402
from reference_backend import (  # noqa: E402
    RefCpuExecutor, run_reference_model, run_partition, seed_hidden,
    hidden_to_bytes, hidden_from_bytes,
)

HID = 16
LAYERS = 32


def _specs(boundaries: list[int]) -> list[StageSpec]:
    """Build contiguous stages from cut points, e.g. [0,12,24,32]."""
    specs = []
    for i in range(len(boundaries) - 1):
        first = boundaries[i]
        specs.append(StageSpec(
            node_id=f"node-{chr(ord('a') + i)}",
            first_layer=first,
            n_layers=boundaries[i + 1] - first,
            hidden_dim=HID,
        ))
    return specs


def _approx_equal(a: array.array, b: array.array, tol: float = 1e-12) -> bool:
    return len(a) == len(b) and all(abs(x - y) <= tol for x, y in zip(a, b))


class Determinism(unittest.TestCase):
    def test_reference_model_is_deterministic(self):
        a = run_reference_model(HID, LAYERS, seed=7)
        b = run_reference_model(HID, LAYERS, seed=7)
        self.assertTrue(_approx_equal(a, b))

    def test_different_seed_differs(self):
        self.assertFalse(_approx_equal(
            run_reference_model(HID, LAYERS, seed=1),
            run_reference_model(HID, LAYERS, seed=2),
        ))

    def test_values_bounded(self):
        for v in run_reference_model(HID, 200):  # many layers, no blow-up
            self.assertLessEqual(abs(v), 1.0 + 1e-9)


class SplitEquivalence(unittest.TestCase):
    """A correct partition == single host, whatever the cut points."""

    def setUp(self):
        self.truth = run_reference_model(HID, LAYERS, seed=1)

    def test_single_stage_matches_truth(self):
        out = run_partition(_specs([0, LAYERS]), seed=1)
        self.assertTrue(_approx_equal(out, self.truth))

    def test_three_way_split_matches_truth(self):
        out = run_partition(_specs([0, 12, 24, 32]), seed=1)
        self.assertTrue(_approx_equal(out, self.truth))

    def test_uneven_split_matches_truth(self):
        out = run_partition(_specs([0, 1, 30, 32]), seed=1)  # tiny + big + tiny
        self.assertTrue(_approx_equal(out, self.truth))

    def test_split_matches_with_and_without_bytes(self):
        a = run_partition(_specs([0, 12, 24, 32]), seed=1, through_bytes=True)
        b = run_partition(_specs([0, 12, 24, 32]), seed=1, through_bytes=False)
        self.assertTrue(_approx_equal(a, b))


class BrokenPipelineDetected(unittest.TestCase):
    def test_dropped_stage_changes_result(self):
        truth = run_reference_model(HID, LAYERS, seed=1)
        # Partition that skips layers [24,32): NOT covering the model.
        broken = run_partition(_specs([0, 12, 24]), seed=1)
        self.assertFalse(_approx_equal(broken, truth))

    def test_non_contiguous_rejected(self):
        bad = [
            StageSpec(node_id="node-a", first_layer=0, n_layers=12, hidden_dim=HID),
            StageSpec(node_id="node-b", first_layer=20, n_layers=12, hidden_dim=HID),  # gap
        ]
        with self.assertRaises(WorkerError):
            run_partition(bad, seed=1)


class WireFormat(unittest.TestCase):
    def test_bytes_roundtrip(self):
        h = seed_hidden(HID, seed=3)
        back = hidden_from_bytes(hidden_to_bytes(h), HID)
        self.assertTrue(_approx_equal(h, back))

    def test_wrong_length_rejected(self):
        h = seed_hidden(HID, seed=3)
        with self.assertRaises(WorkerError):
            hidden_from_bytes(hidden_to_bytes(h), HID + 1)


class BackendProtocol(unittest.TestCase):
    def test_forward_before_load_fails(self):
        ex = RefCpuExecutor(HID)
        from worker import Activation
        with self.assertRaises(WorkerError):
            ex.forward(Activation(1, HID, "bf16", data=seed_hidden(HID)), (0,))


if __name__ == "__main__":
    unittest.main()
