"""sofmat partitioner — heterogeneous pipeline-parallel layer assignment.

Design of record (README.md, "Invariantes de diseño"):

  1. HARD constraint: weights + KV-cache budget must fit inside each node's
     ``model_mem_cap_gb``. This decides IF the model fits at all (fail-closed:
     no measured/declared profile -> refuse to partition, never assume).
  2. Objective (decision "speed floor"): capacity is the hard constraint;
     among valid partitions the objective is SPEED (minimize per-token time).
     ``min_usable_tokens_s`` is a floor: the solver adds hosts only until the
     floor is met (fewest hosts that reach it); with no reachable floor it
     maximizes speed outright; ``min_usable_tokens_s=0`` recovers pure host
     parsimony (fewest hosts that fit); ``None`` (default) = maximize speed.
  3. Ties broken toward fewer hosts (every boundary adds overhead and risk).
  4. Reject any map where the network fraction of token time exceeds
     ``network_time_budget`` (KPI: "transparent to the network", <10-15%).
  5. Emit N-1 fallback maps (best map excluding each host) so the coordinator
     can re-shard instantly when a worker disappears mid-forward.

Pure stdlib on purpose: the solver is arithmetic, not ML. Profiles must come
measured (runtime microbench: ms/layer per node, boundary overhead per link)
or user-declared in config — never baked-in defaults.
"""

from __future__ import annotations

import itertools
import math
from dataclasses import dataclass, field


class PartitionError(Exception):
    """Raised when no valid partition exists for the given inputs."""


@dataclass(frozen=True)
class NodeProfile:
    """One pool member, as described in config + measured by the microbench.

    ``model_mem_cap_gb`` is the memory usable for weights+KV on this node —
    NOT the total memory (e.g. a unified-memory node may cap 80 of 128 GB,
    reserving the rest as system RAM). The solver only ever sees the cap.

    Exactly one of ``ms_per_layer`` (measured, preferred) or
    ``mem_bandwidth_gbps`` (declared fallback) must be provided; with
    neither, the solver refuses the node (fail-closed).
    """

    id: str
    model_mem_cap_gb: float
    ms_per_layer: float | None = None
    mem_bandwidth_gbps: float | None = None
    present: bool = True

    def stage_ms(self, n_layers: int, gb_per_layer: float) -> float:
        """Estimated per-token compute time for ``n_layers`` on this node.

        ``gb_per_layer`` must be the TOTAL bytes touched per layer per token
        (weights + this layer's share of KV-cache) — decode reads both, so
        pricing weights alone underestimates token time when KV is large.
        A measured ``ms_per_layer`` is used as-is (the microbench measures
        whatever it measures; keep its conditions consistent with serving).
        """
        if self.ms_per_layer is not None:
            return n_layers * self.ms_per_layer
        if self.mem_bandwidth_gbps is not None:
            # Decode is memory-bound: time ~ bytes touched / memory bandwidth.
            return (n_layers * gb_per_layer) / self.mem_bandwidth_gbps * 1000.0
        raise PartitionError(
            f"node {self.id}: no measured ms_per_layer and no declared "
            f"mem_bandwidth_gbps — refusing to guess (fail-closed)"
        )


@dataclass(frozen=True)
class ModelSpec:
    """The model to place: weights + KV budget for the configured context."""

    n_layers: int
    weights_gb: float
    kv_cache_gb: float  # TOTAL KV budget at max_context; hard constraint.

    def __post_init__(self) -> None:
        if self.n_layers < 1:
            raise PartitionError(f"n_layers must be >= 1, got {self.n_layers}")
        if self.weights_gb <= 0 or self.kv_cache_gb < 0:
            raise PartitionError(
                f"invalid sizes: weights_gb={self.weights_gb}, "
                f"kv_cache_gb={self.kv_cache_gb}"
            )

    @property
    def gb_per_layer(self) -> float:
        return self.weights_gb / self.n_layers

    @property
    def gb_per_layer_total(self) -> float:
        """Bytes touched per layer per token: weights + its KV share."""
        return (self.weights_gb + self.kv_cache_gb) / self.n_layers

    @property
    def total_gb(self) -> float:
        return self.weights_gb + self.kv_cache_gb


@dataclass(frozen=True)
class Stage:
    node_id: str
    first_layer: int  # inclusive
    n_layers: int
    mem_gb: float  # weights + proportional KV held by this stage
    stage_ms: float


@dataclass(frozen=True)
class PartitionPlan:
    stages: tuple[Stage, ...]
    token_ms: float  # sum of stage compute + boundary overhead
    network_ms: float  # boundary overhead share of token_ms
    network_fraction: float

    @property
    def node_ids(self) -> tuple[str, ...]:
        return tuple(s.node_id for s in self.stages)


@dataclass(frozen=True)
class PartitionResult:
    plan: PartitionPlan
    fallbacks: dict[str, PartitionPlan | None] = field(default_factory=dict)
    # fallbacks[node_id] = best plan WITHOUT that node (None if impossible).


def _fits(nodes: list[NodeProfile], model: ModelSpec) -> bool:
    return sum(n.model_mem_cap_gb for n in nodes) >= model.total_gb


def _assign_layers(
    subset: list[NodeProfile], model: ModelSpec
) -> list[int] | None:
    """Split model.n_layers across ``subset`` minimizing per-token time. EXACT.

    In batch=1 autoregressive decode the token traverses every stage
    sequentially, so compute time is the SUM of stage times: with a constant
    per-layer cost per node, the sum is minimized by loading the fastest
    nodes up to their memory caps (greedy by speed — provably optimal; no DP
    needed for this objective). Every node in the subset must hold >= 1
    layer (a member that would get 0 belongs to a smaller subset, which the
    caller also enumerates). Returns layer counts aligned with ``subset``
    order, or None if the caps cannot hold the model.

    Note: minimizing the SLOWEST stage (v0 behaviour) is the throughput
    objective for pipelined streams — deferred to Fase 1.5 as an alternate
    allocation mode.
    """
    gb_layer = model.gb_per_layer_total
    caps_layers = [int(n.model_mem_cap_gb / gb_layer) for n in subset]
    if sum(caps_layers) < model.n_layers or any(c < 1 for c in caps_layers):
        return None

    counts = [1] * len(subset)  # every member holds at least one layer
    remaining = model.n_layers - len(subset)
    if remaining < 0:
        return None
    # Fastest node first (lowest ms per layer), fill to cap.
    order = sorted(
        range(len(subset)), key=lambda i: subset[i].stage_ms(1, model.gb_per_layer_total)
    )
    for i in order:
        take = min(remaining, caps_layers[i] - counts[i])
        counts[i] += take
        remaining -= take
        if remaining == 0:
            break
    if remaining != 0:
        return None
    return counts


def _plan_for_subset(
    subset: list[NodeProfile],
    model: ModelSpec,
    boundary_overhead_ms: float,
    network_time_budget: float,
) -> PartitionPlan | None:
    counts = _assign_layers(subset, model)
    if counts is None:
        return None
    # Order stages fastest-last is irrelevant for latency (sum is the same);
    # keep subset order deterministic (sorted by id at call site).
    gb_layer_kv = model.kv_cache_gb / model.n_layers
    stages, first = [], 0
    for node, n_layers in zip(subset, counts):
        stages.append(
            Stage(
                node_id=node.id,
                first_layer=first,
                n_layers=n_layers,
                mem_gb=n_layers * (model.gb_per_layer + gb_layer_kv),
                stage_ms=node.stage_ms(n_layers, model.gb_per_layer_total),
            )
        )
        first += n_layers
    n_boundaries = len(subset) - 1
    network_ms = n_boundaries * boundary_overhead_ms
    compute_ms = sum(s.stage_ms for s in stages)
    token_ms = compute_ms + network_ms
    fraction = network_ms / token_ms if token_ms > 0 else 0.0
    if len(subset) > 1 and fraction > network_time_budget:
        return None  # invariant 4: never emit a network-dominated map
    return PartitionPlan(
        stages=tuple(stages),
        token_ms=token_ms,
        network_ms=network_ms,
        network_fraction=fraction,
    )


def solve(
    nodes: list[NodeProfile],
    model: ModelSpec,
    boundary_overhead_ms: float,
    network_time_budget: float = 0.15,
    min_usable_tokens_s: float | None = None,
    with_fallbacks: bool = True,
) -> PartitionResult:
    """Compute the partition plan per the design-of-record objective.

    Raises PartitionError when the model cannot fit the present nodes under
    the caps + KV budget, or when every fitting map violates the network KPI.
    """
    present = sorted((n for n in nodes if n.present), key=lambda n: n.id)
    if not present:
        raise PartitionError("no nodes present in the pool")
    if not _fits(present, model):
        raise PartitionError(
            f"model needs {model.total_gb:.1f} GB (weights+KV) but present "
            f"caps sum {sum(n.model_mem_cap_gb for n in present):.1f} GB"
        )

    # Best valid plan for each pipeline depth k (ties inside k -> fastest).
    best_per_k: dict[int, PartitionPlan] = {}
    for k in range(1, len(present) + 1):
        for subset in itertools.combinations(present, k):
            plan = _plan_for_subset(
                list(subset), model, boundary_overhead_ms, network_time_budget
            )
            if plan is not None and (
                k not in best_per_k or plan.token_ms < best_per_k[k].token_ms
            ):
                best_per_k[k] = plan
    if not best_per_k:
        raise PartitionError(
            "model fits by raw capacity but no map satisfies the network "
            f"budget ({network_time_budget:.0%}) — stages too small for the "
            "boundary overhead; add memory per node or accept higher budget"
        )

    if min_usable_tokens_s is None:
        # Objective: speed. Fastest plan overall; ties toward fewer hosts.
        best = min(best_per_k.values(), key=lambda p: (p.token_ms, len(p.stages)))
    else:
        # Speed floor: fewest hosts whose best plan reaches the floor
        # (min_usable_tokens_s=0 -> every plan qualifies -> pure parsimony);
        # floor unreachable -> maximize speed instead.
        floor_ms = math.inf if min_usable_tokens_s <= 0 else 1000.0 / min_usable_tokens_s
        meeting = [k for k in sorted(best_per_k) if best_per_k[k].token_ms <= floor_ms]
        if meeting:
            best = best_per_k[meeting[0]]
        else:
            best = min(
                best_per_k.values(), key=lambda p: (p.token_ms, len(p.stages))
            )

    fallbacks: dict[str, PartitionPlan | None] = {}
    if with_fallbacks:
        for node in best.node_ids:
            rest = [n for n in present if n.id != node]
            try:
                fallbacks[node] = solve(
                    rest,
                    model,
                    boundary_overhead_ms,
                    network_time_budget,
                    min_usable_tokens_s,
                    with_fallbacks=False,
                ).plan
            except PartitionError:
                fallbacks[node] = None
    return PartitionResult(plan=best, fallbacks=fallbacks)
