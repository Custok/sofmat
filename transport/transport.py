"""sofmat transport — inter-host activation channel (interface + TCP v0).

This is ``node-c``'s module. The coordinator (``node-a``) and the stage
workers (``node-b/c/d``) move pipeline activations across the 10 GbE boundary
through this interface, and nothing else:

    send_activation(header, payload)      # push my stage's output downstream
    header, view = recv_activation()      # pull the previous stage's output

The interface hides the wire so the backend can move from TCP to RDMA later
without the coordinator changing a line (README invariant 1). v0 is a raw
stdlib ``socket`` backend — no gRPC/torch dependency yet — chosen so the
channel is testable on any host and in CI today; gRPC and a zero-copy RDMA
backend slot in behind the same ``Transport`` base.

Security (README "Transporte"):
  * A01 — every connection completes the :mod:`auth` challenge-response before
    a single activation is accepted. No open port that takes tensors from the
    LAN.
  * A08 — frames are the binary format in :mod:`framing`; ``pickle`` never
    touches the socket.
  * A04 / DoS — the length prefix is bounded by ``max_frame_bytes`` (derived
    from ``cluster.transport.max_activation_mb``); an oversized or truncated
    frame aborts the connection instead of allocating.

The measured per-boundary overhead (RTT + serialization) that this backend
adds is exactly the ``boundary_overhead_ms`` input the partitioner's cost
model consumes — see :func:`probe_boundary_overhead_ms`.
"""

from __future__ import annotations

import abc
import os
import socket
import struct
import sys
import time

# ``auth`` lives in the shared ``common/`` module (one auth module for the
# transport and the served-weights endpoint). Put it on the path so a plain
# checkout runs — same bootstrap the coordinator uses. ``framing`` is a sibling
# in this directory.
_COMMON = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "common")
if os.path.isdir(_COMMON) and _COMMON not in sys.path:
    sys.path.insert(0, _COMMON)

import auth      # noqa: E402  common/auth.py
import framing   # noqa: E402  sibling

_LEN_PREFIX = struct.Struct("!I")
_HEADER_SLACK = 512          # room for the frame header on top of the payload cap


class TransportError(Exception):
    """Connection-level protocol/IO failure. The stage boundary is broken."""


class Transport(abc.ABC):
    """A bidirectional activation channel to exactly one peer."""

    @abc.abstractmethod
    def send_activation(self, header: framing.ActivationHeader,
                        payload: bytes) -> None: ...

    @abc.abstractmethod
    def recv_activation(self) -> "tuple[framing.ActivationHeader, memoryview]": ...

    @abc.abstractmethod
    def close(self) -> None: ...

    def __enter__(self) -> "Transport":
        return self

    def __exit__(self, *exc) -> None:
        self.close()


class TcpTransport(Transport):
    """Raw-socket TCP backend. Construct via :func:`connect` / :func:`accept`,
    not directly, so the auth handshake always runs first."""

    def __init__(self, sock: socket.socket, max_activation_mb: int = 8) -> None:
        self._sock = sock
        self.max_frame_bytes = max_activation_mb * 1024 * 1024 + _HEADER_SLACK
        self._max_payload = max_activation_mb * 1024 * 1024

    # -- activation path -------------------------------------------------
    def send_activation(self, header: framing.ActivationHeader,
                        payload: bytes) -> None:
        frame = framing.encode_activation(header, payload)
        self._send_frame(frame)

    def recv_activation(self) -> "tuple[framing.ActivationHeader, memoryview]":
        frame = self._recv_frame()
        return framing.decode_activation(frame, max_payload=self._max_payload)

    def close(self) -> None:
        try:
            self._sock.close()
        except OSError:
            pass

    # -- low-level framed IO (length-prefixed, bounded) ------------------
    def _send_frame(self, frame: bytes) -> None:
        try:
            self._sock.sendall(_LEN_PREFIX.pack(len(frame)) + frame)
        except OSError as e:
            raise TransportError(f"send failed: {e}") from e

    def _recv_frame(self) -> bytes:
        raw_len = self._recv_exact(_LEN_PREFIX.size)
        (n,) = _LEN_PREFIX.unpack(raw_len)
        if n > self.max_frame_bytes:
            raise TransportError(
                f"frame {n}B exceeds cap {self.max_frame_bytes}B — aborting")
        return self._recv_exact(n)

    def _recv_exact(self, n: int) -> bytes:
        chunks = []
        got = 0
        while got < n:
            try:
                b = self._sock.recv(min(n - got, 1 << 20))
            except OSError as e:
                raise TransportError(f"recv failed: {e}") from e
            if not b:
                raise TransportError("peer closed mid-frame")
            chunks.append(b)
            got += len(b)
        return b"".join(chunks)


# -- connection setup (handshake runs here, always) ----------------------
def connect(host: str, port: int, token: bytes, *,
            max_activation_mb: int = 8, timeout: float = 10.0) -> TcpTransport:
    """Master side: dial a worker and complete the client handshake."""
    sock = socket.create_connection((host, port), timeout=timeout)
    sock.settimeout(timeout)
    _set_tcp_nodelay(sock)
    t = TcpTransport(sock, max_activation_mb)
    _client_handshake(t, token)
    return t


def accept(sock: socket.socket, token: bytes, *,
           max_activation_mb: int = 8) -> TcpTransport:
    """Worker side: wrap an accepted socket and complete the server handshake.

    Raises :class:`auth.AuthError` (and closes) if the peer cannot prove the
    shared token — the port never yields an activation channel to an
    unauthenticated caller.
    """
    _set_tcp_nodelay(sock)
    t = TcpTransport(sock, max_activation_mb)
    _server_handshake(t, token)
    return t


def _server_handshake(t: TcpTransport, token: bytes) -> None:
    nonce = auth.new_nonce()
    t._send_frame(framing.frame_control(framing.MsgType.AUTH_CHALLENGE, nonce))
    mtype, body = framing.decode_control(t._recv_frame())
    if mtype != framing.MsgType.AUTH_RESPONSE or not auth.verify(token, nonce, body):
        try:
            t._send_frame(framing.frame_control(framing.MsgType.ERROR, b"auth"))
        except TransportError:
            pass
        t.close()
        raise auth.AuthError("worker rejected peer: bad token")
    t._send_frame(framing.frame_control(framing.MsgType.ACK))


def _client_handshake(t: TcpTransport, token: bytes) -> None:
    mtype, nonce = framing.decode_control(t._recv_frame())
    if mtype != framing.MsgType.AUTH_CHALLENGE:
        t.close()
        raise auth.AuthError(f"expected AUTH_CHALLENGE, got {mtype}")
    t._send_frame(framing.frame_control(
        framing.MsgType.AUTH_RESPONSE, auth.sign(token, nonce)))
    mtype, _ = framing.decode_control(t._recv_frame())
    if mtype != framing.MsgType.ACK:
        t.close()
        raise auth.AuthError("master rejected by worker (bad token)")


def _set_tcp_nodelay(sock: socket.socket) -> None:
    # Activations are small and latency-critical on the decode hot path;
    # Nagle would coalesce them and add a boundary of delay per token.
    try:
        sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
    except OSError:
        pass


def probe_boundary_overhead_ms(t: TcpTransport,
                               header: framing.ActivationHeader,
                               payload: bytes, *, rounds: int = 20,
                               warmup: int = 3) -> float:
    """Round-trip an activation against an echo peer and return the **median**
    one-way boundary overhead in ms (stable estimate).

    This is the ``boundary_overhead_ms`` the partitioner's cost model needs
    (README invariant 3: measured, not catalogue). Run it per link at startup;
    the coordinator feeds the result into the solver.

    Stability: the first ``warmup`` round-trips are DISCARDED (TCP slow-start,
    cold caches, first-allocation) and the estimate is the median of the rest,
    so a single outlier cannot move it. ``rounds`` is the measured count on top
    of warmup.
    """
    if rounds < 1:
        raise ValueError("rounds must be >= 1")
    for _ in range(max(0, warmup)):
        t.send_activation(header, payload)
        t.recv_activation()
    samples = []
    for _ in range(rounds):
        t0 = time.perf_counter()
        t.send_activation(header, payload)
        t.recv_activation()
        samples.append((time.perf_counter() - t0) * 1000.0 / 2.0)
    samples.sort()
    return samples[len(samples) // 2]
