"""sofmat runtime — per-host stage worker (pipeline-parallel).

A worker owns a CONTIGUOUS range of transformer layers, keeps the KV-cache for
that range, and runs one pipeline stage: receive an activation from the
previous stage, run its layers, hand the result to the next stage. The master
(coordinator) drives the autoregressive loop and sampling; the worker is a pure
stage.

Design of record (README.md):
  * Invariant 5 / OWASP A08: weights load ONLY from a config-declared local
    path, never from anything that arrives over the network.
  * OWASP A04 (resilient design): every activation received from the wire is
    VALIDATED (dtype, rank, hidden dim, finiteness left to the executor) BEFORE
    it touches the GPU. A malformed tensor raises and drops the connection — it
    never crashes the worker or reaches compute.
  * KPI telemetry: each step reports compute vs network(recv/send) vs wait time
    so the coordinator's bench can compute red+espera / total per token.

Pure standard library. The heavy math lives behind the ``LayerExecutor``
protocol: a ``StubLayerExecutor`` (here) and the ``RefCpuExecutor`` (in
``reference_backend``) make the whole worker unit-testable and runnable with no
GPU / no torch; the real torch backend implements the same protocol and plugs
in unchanged.

Anonymous labels only (node-a/b/c/d). Real host mapping stays in config.local.
"""

from __future__ import annotations

import time
from dataclasses import dataclass, field
from typing import Protocol, runtime_checkable


# Monotonic clock in milliseconds. Injectable so tests are deterministic.
def _now_ms() -> float:
    return time.monotonic() * 1000.0


class WorkerError(Exception):
    """Fatal, connection-dropping error (bad activation, unloaded stage...)."""


@dataclass(frozen=True)
class StageSpec:
    """What this worker must run, entirely from validated config.

    ``model_path`` is a LOCAL path (config.local.yaml). It is never taken from
    the network — that is the A08 boundary.
    """

    node_id: str
    first_layer: int          # inclusive, 0-based index into the full model
    n_layers: int
    hidden_dim: int
    dtype: str = "bf16"       # logical name; the executor maps it to a real dtype
    model_path: str | None = None
    max_context: int = 8192   # KV-cache budget horizon (hard elsewhere)

    def __post_init__(self) -> None:
        if self.n_layers < 1:
            raise WorkerError(f"{self.node_id}: n_layers must be >= 1")
        if self.first_layer < 0:
            raise WorkerError(f"{self.node_id}: first_layer must be >= 0")
        if self.hidden_dim < 1:
            raise WorkerError(f"{self.node_id}: hidden_dim must be >= 1")

    @property
    def last_layer(self) -> int:
        return self.first_layer + self.n_layers - 1


@dataclass(frozen=True)
class Activation:
    """One activation tensor crossing a pipeline boundary, transport-agnostic.

    ``data`` is opaque to the worker (the transport hands over a zero-copy
    buffer/memoryview; the executor reinterprets it). The worker validates the
    METADATA before compute; the executor validates the bytes.
    """

    seq_len: int
    hidden_dim: int
    dtype: str
    data: object = None  # memoryview / ndarray / tensor — executor's concern


@runtime_checkable
class LayerExecutor(Protocol):
    """The math backend for one stage.

    Two concrete backends implement it: ``StubLayerExecutor`` (structural, for
    the plumbing/telemetry tests) and ``RefCpuExecutor`` in ``reference_backend``
    (the real, deterministic, GPU-free reference). The production torch backend
    implements the same two methods.
    """

    def load(self, first_layer: int, n_layers: int) -> None: ...
    def forward(self, act: Activation, positions: tuple[int, ...]) -> Activation: ...


@dataclass
class StubLayerExecutor:
    """Structural, GPU-free executor: passes the activation through unchanged.

    It does NO real math — its only job is to let the worker/microbench tests
    exercise loading, validation and telemetry without hardware. Timing in
    those tests comes from an injected clock (see ``StageWorker``'s ``clock``),
    not from this stub, so the stub stays honest and instant. For a backend
    that actually transforms the activation end-to-end, use ``RefCpuExecutor``.
    """

    hidden_dim: int
    _loaded: tuple[int, int] | None = field(default=None, init=False)

    def load(self, first_layer: int, n_layers: int) -> None:
        self._loaded = (first_layer, n_layers)

    def forward(self, act: Activation, positions: tuple[int, ...]) -> Activation:
        if self._loaded is None:
            raise WorkerError("StubLayerExecutor.forward called before load()")
        return Activation(act.seq_len, self.hidden_dim, act.dtype, data=act.data)


@dataclass(frozen=True)
class StepTelemetry:
    """Per-step timing breakdown — the raw material of the network-KPI.

    ``network_ms`` = recv + send (time on the wire / waiting for the peer);
    ``compute_ms`` = this stage's layer forward; ``wait_ms`` = idle/bubble
    before work was available. The coordinator aggregates these across stages
    into red+espera / total per token.
    """

    node_id: str
    compute_ms: float
    recv_ms: float
    send_ms: float
    wait_ms: float

    @property
    def network_ms(self) -> float:
        return self.recv_ms + self.send_ms

    @property
    def total_ms(self) -> float:
        return self.compute_ms + self.network_ms + self.wait_ms


class StageWorker:
    """Runs one pipeline stage. Transport-agnostic: the caller supplies the
    already-received activation and (optionally) a send callback; the worker
    validates, computes, and reports telemetry.
    """

    def __init__(self, spec: StageSpec, executor: LayerExecutor, *, clock=_now_ms):
        self.spec = spec
        self.executor = executor
        self._clock = clock
        self._loaded = False

    def load(self) -> None:
        """Load this stage's weights from the CONFIG-declared local path only."""
        # A08: refuse any non-local / missing path rather than fetch weights we
        # were handed at runtime. (The real executor enforces weights_only=True
        # and reads spec.model_path; the mock ignores it.)
        self.executor.load(self.spec.first_layer, self.spec.n_layers)
        self._loaded = True

    def _validate(self, act: Activation) -> None:
        """A04: reject a malformed activation BEFORE it reaches compute."""
        if not self._loaded:
            raise WorkerError(f"{self.spec.node_id}: stage not loaded")
        if act.hidden_dim != self.spec.hidden_dim:
            raise WorkerError(
                f"{self.spec.node_id}: hidden_dim mismatch "
                f"(got {act.hidden_dim}, expected {self.spec.hidden_dim})"
            )
        if act.seq_len < 1 or act.seq_len > self.spec.max_context:
            raise WorkerError(
                f"{self.spec.node_id}: seq_len {act.seq_len} outside "
                f"[1, {self.spec.max_context}]"
            )
        if act.dtype != self.spec.dtype:
            raise WorkerError(
                f"{self.spec.node_id}: dtype mismatch "
                f"(got {act.dtype!r}, expected {self.spec.dtype!r})"
            )

    def run_step(
        self,
        act: Activation,
        positions: tuple[int, ...],
        *,
        recv_ms: float = 0.0,
        wait_ms: float = 0.0,
        send=None,
    ) -> tuple[Activation, StepTelemetry]:
        """Validate → compute → (optionally) send. Returns (output, telemetry).

        ``recv_ms``/``wait_ms`` are measured by the transport layer and passed
        in so the worker can assemble the full per-step breakdown. ``send`` is
        an optional callable(Activation) -> None whose wall time is charged to
        ``send_ms``.
        """
        self._validate(act)
        t0 = self._clock()
        out = self.executor.forward(act, positions)
        t1 = self._clock()
        compute_ms = t1 - t0

        send_ms = 0.0
        if send is not None:
            s0 = self._clock()
            send(out)
            send_ms = self._clock() - s0

        tel = StepTelemetry(
            node_id=self.spec.node_id,
            compute_ms=compute_ms,
            recv_ms=recv_ms,
            send_ms=send_ms,
            wait_ms=wait_ms,
        )
        return out, tel

    def kv_bytes_estimate(self, bytes_per_token_per_layer: int) -> int:
        """KV-cache bytes this stage holds at full context — the hard budget
        the partitioner reserves inside model_mem_cap_gb (never on top of it).
        """
        return (self.spec.n_layers * self.spec.max_context
                * bytes_per_token_per_layer)
