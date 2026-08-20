"""sofmat runtime — reference CPU backend (no GPU, no torch, pure stdlib).

Lets the WHOLE pipeline run end-to-end on any machine so anyone who clones the
repo can verify the install without a single GPU: the coordinator wires
partitioner -> transport -> runtime with this backend over loopback and gets a
real, deterministic result.

The trick that makes it a valid end-to-end proof: the "model" is a fixed,
deterministic per-layer transform of a hidden vector. Applying layers
``[0..L)`` in one worker MUST equal applying ``[0..k)`` then ``[k..L)`` across
two workers — so a correct partition produces the SAME output as single-host,
and a broken pipeline (dropped/reordered stage) produces a different one. That
equality is the end-to-end assertion.

The hidden state is an ``array('d')`` (float64) so it serialises to bytes for
the transport (``to_bytes``/``from_bytes``) with zero pickle (OWASP A08).
"""

from __future__ import annotations

import array
import math
from dataclasses import dataclass, field

from worker import Activation, WorkerError


# ---- activation <-> bytes (transport-friendly, no pickle) -------------------

def hidden_to_bytes(hidden: array.array) -> bytes:
    if hidden.typecode != "d":
        raise WorkerError("reference hidden must be array('d')")
    return hidden.tobytes()


def hidden_from_bytes(blob: bytes, hidden_dim: int) -> array.array:
    a = array.array("d")
    a.frombytes(blob)
    if len(a) != hidden_dim:
        raise WorkerError(
            f"reference activation length {len(a)} != hidden_dim {hidden_dim}"
        )
    return a


def seed_hidden(hidden_dim: int, seed: int = 1) -> array.array:
    """A deterministic starting hidden vector (stands in for the embedding)."""
    a = array.array("d", [0.0] * hidden_dim)
    x = (seed * 2654435761) & 0xFFFFFFFF
    for i in range(hidden_dim):
        x = (1103515245 * x + 12345) & 0x7FFFFFFF   # POSIX LCG, reproducible
        a[i] = (x / 0x7FFFFFFF) * 2.0 - 1.0          # in [-1, 1)
    return a


# ---- the reference "layer" --------------------------------------------------

def _layer_forward(hidden: array.array, layer_index: int) -> None:
    """One deterministic, in-place layer transform.

    Depends ONLY on the current hidden state and the GLOBAL layer index, so the
    result of a range of layers is independent of how stages are split. Cheap
    (O(hidden)) and bounded (tanh keeps values in [-1, 1], no blow-up over many
    layers). Not a real transformer — a stable fingerprint of "the model ran".
    """
    n = len(hidden)
    a = 0.5 + 0.25 * math.sin(layer_index + 1)      # layer-specific scale
    b = 0.1 * math.cos(2 * layer_index + 1)         # layer-specific bias
    prev = hidden[n - 1]
    for i in range(n):
        cur = hidden[i]
        # mix with the previous element -> non-diagonal, order-sensitive
        hidden[i] = math.tanh(a * cur + b + 0.3 * prev)
        prev = cur


@dataclass
class RefCpuExecutor:
    """LayerExecutor backend that actually transforms the activation.

    Same ``LayerExecutor`` protocol as ``StubLayerExecutor`` and the (future)
    torch backend, so it drops into ``StageWorker`` unchanged. This one carries
    the real reference math; the stub is a structural pass-through.
    """

    hidden_dim: int
    _range: tuple[int, int] | None = field(default=None, init=False)

    def load(self, first_layer: int, n_layers: int) -> None:
        self._range = (first_layer, n_layers)

    def forward(self, act: Activation, positions: tuple[int, ...]) -> Activation:
        if self._range is None:
            raise WorkerError("RefCpuExecutor.forward before load()")
        first, n_layers = self._range
        hidden = act.data
        if not isinstance(hidden, array.array) or hidden.typecode != "d":
            raise WorkerError("RefCpuExecutor expects Activation.data = array('d')")
        if len(hidden) != self.hidden_dim:
            raise WorkerError(
                f"activation length {len(hidden)} != hidden_dim {self.hidden_dim}"
            )
        out = array.array("d", hidden)  # copy: don't mutate the caller's buffer
        for li in range(first, first + n_layers):
            _layer_forward(out, li)
        return Activation(act.seq_len, self.hidden_dim, act.dtype, data=out)


def run_reference_model(hidden_dim: int, n_layers: int, seed: int = 1) -> array.array:
    """Single-process ground truth: run ALL layers in one shot. The end-to-end
    pipeline (split across stages/hosts) must reproduce this exactly.
    """
    hidden = seed_hidden(hidden_dim, seed)
    for li in range(n_layers):
        _layer_forward(hidden, li)
    return hidden


def run_partition(specs, seed: int = 1, *, through_bytes: bool = True) -> array.array:
    """Drive one decode step through a partition of StageWorkers, in order.

    A stand-in for the coordinator's forward loop, kept here so the runtime is
    verifiable end-to-end on its own. ``through_bytes=True`` round-trips the
    activation via ``to_bytes``/``from_bytes`` at each boundary — the exact
    serialisation the transport does — so this also proves the wire format.

    ``specs`` is an ordered list of StageSpec covering [0, total_layers)
    contiguously. Returns the final hidden vector.
    """
    from worker import StageWorker  # local import: avoid a cycle at module load

    if not specs:
        raise WorkerError("run_partition: empty partition")
    hidden_dim = specs[0].hidden_dim
    act = Activation(seq_len=1, hidden_dim=hidden_dim, dtype=specs[0].dtype,
                     data=seed_hidden(hidden_dim, seed))
    expected_first = 0
    for spec in specs:
        if spec.hidden_dim != hidden_dim:
            raise WorkerError("run_partition: mixed hidden_dim across stages")
        if spec.first_layer != expected_first:
            raise WorkerError(
                f"run_partition: non-contiguous stages "
                f"(expected first_layer {expected_first}, got {spec.first_layer})"
            )
        worker = StageWorker(spec, RefCpuExecutor(hidden_dim))
        worker.load()
        out, _tel = worker.run_step(act, (0,))
        if through_bytes:
            blob = hidden_to_bytes(out.data)
            out = Activation(out.seq_len, out.hidden_dim, out.dtype,
                             data=hidden_from_bytes(blob, hidden_dim))
        act = out
        expected_first = spec.last_layer + 1
    return act.data
