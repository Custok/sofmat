"""Tests for the sofmat runtime worker + microbench. Pure stdlib unittest.

Run:  python3 -m unittest test_runtime   (from the runtime/ directory)

A fake clock makes timing deterministic: it advances a fixed step on every
read, so the delta the worker measures around executor.forward is exactly that
step regardless of absolute time — no sleeps, no flakiness.
"""

from __future__ import annotations

import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(__file__))

from worker import (  # noqa: E402
    Activation, StubLayerExecutor, StageSpec, StageWorker, StepTelemetry,
    WorkerError,
)
from microbench import measure_ms_per_layer, NodeMeasurement  # noqa: E402


class FakeClock:
    """Returns t, then advances by `step` on every call."""

    def __init__(self, step: float = 5.0):
        self.t = 0.0
        self.step = step

    def __call__(self) -> float:
        v = self.t
        self.t += self.step
        return v


def _spec(**kw) -> StageSpec:
    base = dict(node_id="node-b", first_layer=10, n_layers=8, hidden_dim=4096)
    base.update(kw)
    return StageSpec(**base)


def _act(**kw) -> Activation:
    base = dict(seq_len=1, hidden_dim=4096, dtype="bf16", data=None)
    base.update(kw)
    return Activation(**base)


class SpecValidation(unittest.TestCase):
    def test_rejects_bad_layer_count(self):
        with self.assertRaises(WorkerError):
            _spec(n_layers=0)

    def test_rejects_negative_first_layer(self):
        with self.assertRaises(WorkerError):
            _spec(first_layer=-1)

    def test_rejects_bad_hidden(self):
        with self.assertRaises(WorkerError):
            _spec(hidden_dim=0)

    def test_last_layer(self):
        self.assertEqual(_spec(first_layer=10, n_layers=8).last_layer, 17)


class LoadAndValidate(unittest.TestCase):
    def setUp(self):
        self.spec = _spec()
        self.ex = StubLayerExecutor(hidden_dim=self.spec.hidden_dim)
        self.w = StageWorker(self.spec, self.ex, clock=FakeClock())

    def test_forward_before_load_fails(self):
        with self.assertRaises(WorkerError):
            self.w.run_step(_act(), (0,))

    def test_hidden_dim_mismatch_rejected(self):
        self.w.load()
        with self.assertRaises(WorkerError):
            self.w.run_step(_act(hidden_dim=2048), (0,))

    def test_seq_len_out_of_range_rejected(self):
        self.w.load()
        with self.assertRaises(WorkerError):
            self.w.run_step(_act(seq_len=999999), (0,))

    def test_dtype_mismatch_rejected(self):
        self.w.load()
        with self.assertRaises(WorkerError):
            self.w.run_step(_act(dtype="fp32"), (0,))


class Telemetry(unittest.TestCase):
    def setUp(self):
        self.spec = _spec()
        self.ex = StubLayerExecutor(hidden_dim=self.spec.hidden_dim)
        self.w = StageWorker(self.spec, self.ex, clock=FakeClock(step=5.0))
        self.w.load()

    def test_compute_ms_measured(self):
        _out, tel = self.w.run_step(_act(), (0,), recv_ms=2.0, wait_ms=1.0)
        self.assertIsInstance(tel, StepTelemetry)
        self.assertEqual(tel.compute_ms, 5.0)   # one fake-clock step around forward
        self.assertEqual(tel.recv_ms, 2.0)
        self.assertEqual(tel.wait_ms, 1.0)
        self.assertEqual(tel.send_ms, 0.0)
        self.assertEqual(tel.network_ms, 2.0)   # recv + send
        self.assertEqual(tel.total_ms, 5.0 + 2.0 + 0.0 + 1.0)

    def test_send_charged_to_send_ms(self):
        sent = []
        _out, tel = self.w.run_step(_act(), (0,), send=sent.append)
        self.assertEqual(len(sent), 1)
        self.assertEqual(tel.send_ms, 5.0)      # one step around the send call
        self.assertEqual(tel.network_ms, tel.recv_ms + tel.send_ms)

    def test_output_shape_preserved(self):
        out, _ = self.w.run_step(_act(seq_len=1), (0,))
        self.assertEqual(out.hidden_dim, self.spec.hidden_dim)


class KvBudget(unittest.TestCase):
    def test_kv_bytes_scales_with_layers_and_context(self):
        spec = _spec(n_layers=8, max_context=8192)
        w = StageWorker(spec, StubLayerExecutor(hidden_dim=spec.hidden_dim))
        # 2 bytes/token/layer (fp16 KV, 1 head-dim unit) -> deterministic
        self.assertEqual(w.kv_bytes_estimate(2), 8 * 8192 * 2)


class Microbench(unittest.TestCase):
    def test_ms_per_layer_from_fake_clock(self):
        spec = _spec(n_layers=8)
        ex = StubLayerExecutor(hidden_dim=spec.hidden_dim)
        m = measure_ms_per_layer(spec, ex, warmup=2, iters=11, clock=FakeClock(step=40.0))
        self.assertIsInstance(m, NodeMeasurement)
        # each stage forward measured at 40ms over 8 layers -> 5 ms/layer
        self.assertAlmostEqual(m.ms_per_layer, 5.0)
        self.assertEqual(m.node_id, "node-b")
        self.assertEqual(m.iters, 11)

    def test_partitioner_profile_shape(self):
        spec = _spec()
        m = measure_ms_per_layer(spec, StubLayerExecutor(hidden_dim=spec.hidden_dim),
                                 warmup=1, iters=3, clock=FakeClock(step=16.0))
        prof = m.to_partitioner_profile()
        # Matches partitioner.NodeProfile fields it feeds: id + ms_per_layer.
        self.assertEqual(set(prof.keys()), {"id", "ms_per_layer"})
        self.assertEqual(prof["id"], "node-b")


if __name__ == "__main__":
    unittest.main()
