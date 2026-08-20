"""sofmat transport — binary wire framing for pipeline activations.

Design of record (README.md, "Seguridad — OWASP Top 10 / Transporte"):

  * A08 (integrity): **no ``pickle`` on the hot path.** Activations cross the
    network as a fixed binary header + a raw contiguous tensor buffer. Pickle
    over a socket is remote code execution by construction; this format cannot
    execute anything on decode.
  * A04 (resilient design): every field is validated on decode BEFORE the
    payload is handed to the caller — magic, version, bounded ``ndim`` and
    dimensions, and ``prod(shape) * dtype_size == payload_len``. A malformed
    frame raises ``FrameError``; it never reaches the GPU.
  * Min-copy: the payload is returned as a ``memoryview`` over the received
    buffer. The runtime rebuilds the tensor zero-copy
    (``torch.frombuffer(buf, dtype=...).reshape(shape)``) after validation.

Wire layout (network byte order, big-endian):

    magic      4s   b"SOFM"
    version    B    protocol version (PROTO_VERSION)
    msg_type   B    MsgType
    flags      B    reserved bit flags (0 in v0)
    _pad       B    reserved, must be 0
    --- ACTIVATION metadata (msg_type == ACTIVATION) ---
    stage_id   H    source pipeline stage (0..65535)
    token_ix   I    position in the autoregressive loop
    dtype      B    DType code
    ndim       B    number of shape dimensions (0..MAX_NDIM)
    shape      I*ndim   dimensions
    n_bytes    Q    payload length in bytes
    <payload>  n_bytes contiguous tensor bytes

Pure standard library on purpose (matches partitioner/leak-guard): no install
step, identical on every host and in CI.
"""

from __future__ import annotations

import struct
from dataclasses import dataclass
from enum import IntEnum

MAGIC = b"SOFM"
PROTO_VERSION = 1

# Safety bounds (A04 / DoS). A frame claiming more than these is rejected
# before any allocation the size implies.
MAX_NDIM = 8
MAX_DIM = 1 << 24            # 16M elements per axis is already absurd for an activation
DEFAULT_MAX_PAYLOAD = 8 * 1024 * 1024   # mirrors cluster.transport.max_activation_mb (8 MiB)


class MsgType(IntEnum):
    ACTIVATION = 1
    AUTH_CHALLENGE = 2
    AUTH_RESPONSE = 3
    ACK = 4
    ERROR = 5


class DType(IntEnum):
    """Numeric dtype codes. Values are stable across versions (append-only)."""

    FLOAT32 = 1
    FLOAT16 = 2
    BFLOAT16 = 3
    INT8 = 4
    UINT8 = 5
    INT32 = 6
    INT64 = 7


_DTYPE_SIZE = {
    DType.FLOAT32: 4,
    DType.FLOAT16: 2,
    DType.BFLOAT16: 2,
    DType.INT8: 1,
    DType.UINT8: 1,
    DType.INT32: 4,
    DType.INT64: 8,
}


def dtype_size(dtype: "DType") -> int:
    try:
        return _DTYPE_SIZE[DType(dtype)]
    except (ValueError, KeyError):
        raise FrameError(f"unknown dtype code {dtype!r}")


class FrameError(Exception):
    """Raised on any malformed or out-of-bounds frame. Never trust the wire."""


_PREFIX = struct.Struct("!4sBBBB")          # magic, version, msg_type, flags, pad
_ACT_FIXED = struct.Struct("!HIBB")          # stage_id, token_ix, dtype, ndim
_LEN = struct.Struct("!Q")                   # payload length


@dataclass(frozen=True)
class ActivationHeader:
    """Validated metadata for one activation tensor crossing a stage boundary."""

    stage_id: int
    token_index: int
    dtype: DType
    shape: tuple[int, ...]

    def expected_bytes(self) -> int:
        n = dtype_size(self.dtype)
        for d in self.shape:
            n *= d
        return n


def encode_activation(header: ActivationHeader, payload: bytes) -> bytes:
    """Serialize one activation into a single wire frame (header + payload).

    ``payload`` must be the raw contiguous tensor bytes and must match
    ``header`` exactly (dtype/shape consistency is enforced here too so a
    sender bug cannot emit a frame the receiver will reject).
    """
    if not isinstance(header.dtype, DType):
        header = ActivationHeader(header.stage_id, header.token_index,
                                  DType(header.dtype), tuple(header.shape))
    _validate_shape(header.shape)
    if len(payload) != header.expected_bytes():
        raise FrameError(
            f"payload {len(payload)}B != shape/dtype implies "
            f"{header.expected_bytes()}B")
    parts = [
        _PREFIX.pack(MAGIC, PROTO_VERSION, MsgType.ACTIVATION, 0, 0),
        _ACT_FIXED.pack(header.stage_id, header.token_index,
                        int(header.dtype), len(header.shape)),
        struct.pack("!%dI" % len(header.shape), *header.shape),
        _LEN.pack(len(payload)),
        payload,
    ]
    return b"".join(parts)


def decode_activation(buf: bytes, *, max_payload: int = DEFAULT_MAX_PAYLOAD
                      ) -> tuple[ActivationHeader, memoryview]:
    """Parse a full activation frame. Returns (header, payload_view).

    Every field is bounds-checked before the payload view is returned. The
    payload is a ``memoryview`` into ``buf`` (no copy). Raises ``FrameError``
    on anything unexpected — the caller must treat that as a protocol abort.
    """
    view = memoryview(buf)
    off = 0
    magic, version, msg_type, flags, pad = _PREFIX.unpack_from(view, off)
    off += _PREFIX.size
    if magic != MAGIC:
        raise FrameError("bad magic (not a sofmat frame)")
    if version != PROTO_VERSION:
        raise FrameError(f"unsupported protocol version {version}")
    if msg_type != MsgType.ACTIVATION:
        raise FrameError(f"expected ACTIVATION, got msg_type {msg_type}")
    if flags != 0 or pad != 0:
        raise FrameError("reserved bits set")

    stage_id, token_ix, dtype_code, ndim = _ACT_FIXED.unpack_from(view, off)
    off += _ACT_FIXED.size
    if ndim > MAX_NDIM:
        raise FrameError(f"ndim {ndim} exceeds MAX_NDIM {MAX_NDIM}")
    shape = struct.unpack_from("!%dI" % ndim, view, off)
    off += 4 * ndim
    _validate_shape(shape)

    (n_bytes,) = _LEN.unpack_from(view, off)
    off += _LEN.size
    if n_bytes > max_payload:
        raise FrameError(f"payload {n_bytes}B exceeds cap {max_payload}B")
    if off + n_bytes != len(view):
        raise FrameError("declared payload length does not match frame size")

    header = ActivationHeader(stage_id, token_ix, DType(dtype_code), tuple(shape))
    if n_bytes != header.expected_bytes():
        raise FrameError("payload length inconsistent with shape/dtype")
    return header, view[off:off + n_bytes]


def _validate_shape(shape) -> None:
    if len(shape) > MAX_NDIM:
        raise FrameError(f"too many dims: {len(shape)}")
    for d in shape:
        if d < 0 or d > MAX_DIM:
            raise FrameError(f"dimension {d} out of bounds")


def frame_control(msg_type: "MsgType", payload: bytes = b"") -> bytes:
    """Encode a small control frame (auth handshake, ACK, ERROR)."""
    if len(payload) > 4096:
        raise FrameError("control payload too large")
    return _PREFIX.pack(MAGIC, PROTO_VERSION, int(msg_type), 0, 0) + \
        _LEN.pack(len(payload)) + payload


def decode_control(buf: bytes) -> tuple["MsgType", bytes]:
    """Parse a control frame produced by :func:`frame_control`."""
    view = memoryview(buf)
    magic, version, msg_type, flags, pad = _PREFIX.unpack_from(view, 0)
    if magic != MAGIC:
        raise FrameError("bad magic (not a sofmat frame)")
    if version != PROTO_VERSION:
        raise FrameError(f"unsupported protocol version {version}")
    (n,) = _LEN.unpack_from(view, _PREFIX.size)
    body = bytes(view[_PREFIX.size + _LEN.size:_PREFIX.size + _LEN.size + n])
    if len(body) != n:
        raise FrameError("control payload length mismatch")
    return MsgType(msg_type), body
