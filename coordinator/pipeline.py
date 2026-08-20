"""sofmat coordinator — the master: partitioner -> transport -> pipeline.

Ties the three modules into a running pipeline:

  * asks ``partitioner/solver.py`` HOW to split the model across the present
    nodes (fit + fastest, per the design-of-record objective);
  * dials each stage's worker over the authenticated binary transport
    (``transport/transport.py``) — never pickle, always the bounded frame;
  * pushes the hidden state through the stages token by token and measures the
    KPI: how much of each token's wall time went to the network vs compute.

v0 topology is a STAR: the master round-trips the activation to each worker in
plan order (master -> worker -> master -> next worker ...). It is the simplest
thing that is correct and testable on one host. v1 will chain workers directly
(worker -> worker, master only at the ends) to halve the boundary crossings;
the ``Transport`` interface already supports it (see docs/ROADMAP).
"""

from __future__ import annotations

import os
import socket
import sys
import time
from dataclasses import dataclass

# The sibling modules are flat (transport/ imports ``auth``/``framing``);
# put them on the path so a plain checkout runs. Proper packaging is tracked
# in docs/ROADMAP.md ("make sofmat an installable package").
_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
for _m in ("transport", "partitioner", "common"):
    _p = os.path.join(_ROOT, _m)
    if os.path.isdir(_p) and _p not in sys.path:
        sys.path.insert(0, _p)

import auth as _auth      # noqa: E402  transport/auth.py
import framing            # noqa: E402  transport/framing.py
import transport as _tp   # noqa: E402  transport/transport.py

from .backend import StageBackend


class StageFailure(Exception):
    """A worker broke mid-forward. Carries the node_id so the coordinator can
    re-shard onto a fallback plan (the partitioner emits one per node)."""

    def __init__(self, node_id: str, cause: Exception) -> None:
        super().__init__(f"stage {node_id} failed: {cause}")
        self.node_id = node_id
        self.cause = cause


@dataclass
class TokenMetrics:
    """Per-token timing. ``network_ms`` is the measured round-trip minus the
    partitioner's predicted stage compute — the KPI the design targets < ~15%."""

    token_index: int
    total_ms: float
    network_ms: float

    @property
    def network_fraction(self) -> float:
        return self.network_ms / self.total_ms if self.total_ms > 0 else 0.0


# --------------------------------------------------------------------------
# Worker side
# --------------------------------------------------------------------------
class StageWorker:
    """Serves one ``StageBackend`` over the transport.

    v0: accepts a single master connection and serves it until the peer closes.
    The real runtime supplies the torch ``StageBackend``; injecting
    it here lets the same runner drive the dependency-free mock in tests/CI.
    """

    def __init__(self, backend: StageBackend) -> None:
        self.backend = backend

    def serve(self, host: str, port: int, token: bytes,
              first_layer: int, n_layers: int, *,
              max_activation_mb: int = 8) -> None:
        self.backend.load(first_layer, n_layers)
        srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        srv.bind((host, port))
        srv.listen(1)
        try:
            conn, _peer = srv.accept()
            try:
                # accept() runs the auth handshake before any activation.
                t = _tp.accept(conn, token, max_activation_mb=max_activation_mb)
            except (_auth.AuthError, _tp.TransportError):
                conn.close()   # unauthenticated / broken caller — reject quietly
                return
            self._serve_conn(t)
        finally:
            srv.close()

    def _serve_conn(self, t) -> None:
        with t:
            while True:
                try:
                    header, view = t.recv_activation()
                except _tp.TransportError:
                    return  # master closed the channel — clean shutdown
                out = self.backend.forward(bytes(view), header.shape)
                reply = framing.ActivationHeader(
                    header.stage_id, header.token_index, header.dtype, header.shape)
                t.send_activation(reply, out)


# --------------------------------------------------------------------------
# Master side
# --------------------------------------------------------------------------
class Coordinator:
    """The master. Owns the plan and one authenticated channel per stage."""

    def __init__(self, plan, endpoints: dict, token: bytes, *,
                 max_activation_mb: int = 8) -> None:
        self.plan = plan                     # partitioner PartitionPlan
        self.endpoints = endpoints           # node_id -> (host, port)
        self.token = token
        self.max_activation_mb = max_activation_mb
        self._conns: dict = {}

    def connect(self, *, timeout: float = 10.0) -> None:
        for st in self.plan.stages:
            if st.node_id not in self.endpoints:
                raise KeyError(f"no endpoint configured for node {st.node_id}")
            host, port = self.endpoints[st.node_id]
            self._conns[st.node_id] = _tp.connect(
                host, port, self.token,
                max_activation_mb=self.max_activation_mb, timeout=timeout)

    def forward_token(self, hidden: bytes, shape: tuple[int, ...],
                      token_index: int) -> "tuple[bytes, TokenMetrics]":
        """Push one hidden state through every stage in order; return the final
        hidden state and the token's timing. Raises :class:`StageFailure` if a
        worker drops mid-forward (the caller re-shards onto a fallback plan)."""
        t0 = time.perf_counter()
        measured_ms = 0.0
        for st in self.plan.stages:
            conn = self._conns[st.node_id]
            hdr = framing.ActivationHeader(
                st.first_layer, token_index, framing.DType.FLOAT32, tuple(shape))
            s = time.perf_counter()
            try:
                conn.send_activation(hdr, hidden)
                _rhdr, view = conn.recv_activation()
            except _tp.TransportError as e:
                raise StageFailure(st.node_id, e) from e
            measured_ms += (time.perf_counter() - s) * 1000.0
            hidden = bytes(view)
        total_ms = (time.perf_counter() - t0) * 1000.0
        predicted_compute = sum(st.stage_ms for st in self.plan.stages)
        network_ms = max(0.0, measured_ms - predicted_compute)
        return hidden, TokenMetrics(token_index, total_ms, network_ms)

    def close(self) -> None:
        for c in self._conns.values():
            try:
                c.close()
            except Exception:
                pass
        self._conns.clear()

    def __enter__(self) -> "Coordinator":
        self.connect()
        return self

    def __exit__(self, *exc) -> None:
        self.close()
