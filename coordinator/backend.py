"""sofmat coordinator — stage backend interface + a dependency-free mock.

A :class:`StageBackend` owns a contiguous range of transformer layers on one
node and computes the forward pass of those layers over a hidden-state tensor.

  * The REAL backend (node-b, torch/CUDA runtime) plugs in here.
  * :class:`MockBackend` lets the whole pipeline run end-to-end with **no torch,
    no GPU and no model weights**, so anyone can test the install and the
    orchestration on a single machine or in CI. Pure standard library (matches
    partitioner / transport / leak-guard).

The activation on the wire is a raw contiguous tensor buffer described by a
``framing.ActivationHeader`` (dtype + shape). A backend receives those bytes,
computes, and returns bytes of the SAME shape/dtype — the coordinator never
interprets the payload, only moves it (README invariant 1).
"""

from __future__ import annotations

import abc
import array


class StageBackend(abc.ABC):
    """The forward pass of one contiguous layer range on one node."""

    @abc.abstractmethod
    def load(self, first_layer: int, n_layers: int) -> None:
        """Load weights for layers ``[first_layer, first_layer+n_layers)``."""

    @abc.abstractmethod
    def forward(self, hidden: bytes, shape: tuple[int, ...]) -> bytes:
        """Run this stage's layers over ``hidden`` (float32 contiguous, ``shape``)
        and return the new hidden state, same shape/dtype, as bytes."""

    @property
    @abc.abstractmethod
    def hidden_size(self) -> int: ...


class MockBackend(StageBackend):
    """Deterministic float32 stand-in for a transformer stage.

    Each layer adds exactly ``1.0`` to every element of the hidden state. That
    is intentionally trivial and side-effect free: after a hidden vector passes
    through ALL ``n_layers`` of the model (however the partitioner split them
    across stages), the result equals ``input + n_layers`` — a value the
    integration test can assert exactly, independent of the split. This proves
    the wiring (partitioner -> stages -> transport -> reassembly) end to end
    without a model or a GPU.
    """

    DTYPE_ITEMSIZE = 4  # float32

    def __init__(self, hidden_size: int = 8) -> None:
        self._h = hidden_size
        self._first = 0
        self._n = 0
        self._loaded = False

    @property
    def hidden_size(self) -> int:
        return self._h

    def load(self, first_layer: int, n_layers: int) -> None:
        if first_layer < 0 or n_layers < 1:
            raise ValueError(f"invalid layer range: first={first_layer} n={n_layers}")
        self._first = first_layer
        self._n = n_layers
        self._loaded = True

    def forward(self, hidden: bytes, shape: tuple[int, ...]) -> bytes:
        if not self._loaded:
            raise RuntimeError("MockBackend.forward before load()")
        expected = self.DTYPE_ITEMSIZE
        for d in shape:
            expected *= d
        if len(hidden) != expected:
            raise ValueError(
                f"hidden {len(hidden)}B != shape {shape} float32 ({expected}B)")
        buf = array.array("f")
        buf.frombytes(hidden)
        # One deterministic op per layer this stage owns.
        delta = float(self._n)
        for i in range(len(buf)):
            buf[i] += delta
        return buf.tobytes()
