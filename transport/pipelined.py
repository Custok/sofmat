"""sofmat transport — overlapped (double-buffered) send.

Prima.cpp's Pipelined-Ring Parallelism (arXiv 2504.08791) overlaps
communication with compute. On our side that means a stage should not block on
the socket write of token N while it could already be computing token N+1.

``BufferedSender`` wraps any :class:`~transport.Transport` and moves the send
off the caller's thread: a single background worker owns the write, so
``send_activation`` returns as soon as the frame is queued. Depth 2 = classic
double buffer (one in flight, one queued). The single worker preserves FIFO
order; a send failure is captured and re-raised on the next call or on close,
so errors are never swallowed (fault-tolerance chain: a dead peer still
surfaces as ``TransportError`` for the coordinator).

The public shape stays ``send_activation`` / ``close`` — the coordinator can
wrap or not wrap a transport with zero interface change.

Ownership note: once queued, the caller must not mutate ``payload`` until the
next token (the worker still holds it). Decode activations are tiny; if the
caller reuses buffers it should pass a copy.

Pure standard library (``threading``, ``queue``).
"""

from __future__ import annotations

import queue
import threading

import framing
import transport as _transport


class BufferedSender:
    """Non-blocking, order-preserving send in front of a Transport."""

    _CLOSE = object()

    def __init__(self, inner: "_transport.Transport", depth: int = 2) -> None:
        if depth < 1:
            raise ValueError("depth must be >= 1")
        self._inner = inner
        self._q: "queue.Queue" = queue.Queue(maxsize=depth)
        self._error: "BaseException | None" = None
        self._closed = False
        self._worker = threading.Thread(target=self._run, daemon=True)
        self._worker.start()

    def send_activation(self, header: "framing.ActivationHeader",
                        payload: bytes) -> None:
        """Queue a frame for background send. Blocks only when the buffer is
        full (natural backpressure), never for the whole round-trip."""
        self._raise_if_failed()
        if self._closed:
            raise _transport.TransportError("send on closed BufferedSender")
        self._q.put((header, payload))
        self._raise_if_failed()

    def flush(self) -> None:
        """Block until every queued frame has been written."""
        self._q.join()
        self._raise_if_failed()

    def close(self) -> None:
        if self._closed:
            return
        self._closed = True
        self._q.put(self._CLOSE)
        self._worker.join(timeout=30.0)
        self._raise_if_failed()  # surface a send error that happened before close

    def _run(self) -> None:
        while True:
            item = self._q.get()
            if item is self._CLOSE:
                self._q.task_done()
                return
            header, payload = item
            try:
                if self._error is None:
                    self._inner.send_activation(header, payload)
            except Exception as e:  # capture, stop sending, re-raise on the caller's thread
                self._error = e
            finally:
                self._q.task_done()

    def _raise_if_failed(self) -> None:
        if self._error is not None:
            err, self._error = self._error, None
            raise err

    def __enter__(self) -> "BufferedSender":
        return self

    def __exit__(self, *exc) -> None:
        self.close()
