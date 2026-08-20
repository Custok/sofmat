"""sofmat runtime — per-node microbench.

Measures this node's real ``ms_per_layer`` by timing forward passes over a
representative layer range, and emits an ANONYMOUS profile that the
partitioner consumes (``NodeProfile.ms_per_layer`` — measured, preferred over
declared ``mem_bandwidth_gbps``). "Perfiles MEDIDOS, no de catálogo"
(design-of-record invariant 3): paper specs lie (drivers, thermals, ARM).

The profile carries only the logical ``node_id`` (node-a/b/c/d) — never a real
hostname. It is JSON, safe to drop in bench_results (git-ignored) and to feed
the coordinator over the wire.

Pure stdlib; runs against any ``LayerExecutor`` (mock for CI, torch in prod).
"""

from __future__ import annotations

import json
import time
from dataclasses import asdict, dataclass

from worker import Activation, LayerExecutor, StageWorker, StageSpec


@dataclass(frozen=True)
class NodeMeasurement:
    node_id: str
    ms_per_layer: float
    n_layers_probed: int
    iters: int
    hidden_dim: int
    dtype: str

    def to_partitioner_profile(self) -> dict:
        """The subset the partitioner's NodeProfile needs (plus the cap, which
        comes from config, added by the caller)."""
        return {"id": self.node_id, "ms_per_layer": self.ms_per_layer}

    def to_json(self) -> str:
        return json.dumps(asdict(self), indent=2)


def measure_ms_per_layer(
    spec: StageSpec,
    executor: LayerExecutor,
    *,
    warmup: int = 3,
    iters: int = 20,
    clock=lambda: time.monotonic() * 1000.0,
) -> NodeMeasurement:
    """Time ``iters`` forward passes over ``spec``'s layers; return ms/layer.

    Warmup passes are discarded (JIT / cache / clocks settling). The per-layer
    figure divides the measured stage time by ``spec.n_layers`` so the
    partitioner can scale it to any layer count.
    """
    if iters < 1:
        raise ValueError("iters must be >= 1")
    worker = StageWorker(spec, executor, clock=clock)
    worker.load()
    act = Activation(seq_len=1, hidden_dim=spec.hidden_dim, dtype=spec.dtype, data=None)
    positions = (0,)

    for _ in range(max(0, warmup)):
        worker.run_step(act, positions)

    samples: list[float] = []
    for _ in range(iters):
        _out, tel = worker.run_step(act, positions)
        samples.append(tel.compute_ms)

    # Median is robust to the odd scheduler hiccup; the partitioner wants the
    # typical per-token cost, not the worst case.
    samples.sort()
    stage_ms = samples[len(samples) // 2]
    return NodeMeasurement(
        node_id=spec.node_id,
        ms_per_layer=stage_ms / spec.n_layers,
        n_layers_probed=spec.n_layers,
        iters=iters,
        hidden_dim=spec.hidden_dim,
        dtype=spec.dtype,
    )
