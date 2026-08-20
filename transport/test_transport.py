"""Unit tests for the sofmat transport (pure stdlib, no infra, no real hosts).

Everything runs on 127.0.0.1 loopback with a throwaway shared token. Covers:
framing round-trip + rejection of malformed/oversized frames (A08/A04), the
auth challenge-response (A01, constant-time), and an end-to-end authenticated
activation exchange over a real socket.
"""

import os
import socket
import sys
import threading
import unittest

_COMMON = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "common")
if os.path.isdir(_COMMON) and _COMMON not in sys.path:
    sys.path.insert(0, _COMMON)

import auth        # noqa: E402  common/auth.py
import framing     # noqa: E402
import pipelined   # noqa: E402
import transport   # noqa: E402
from framing import ActivationHeader, DType, FrameError  # noqa: E402

TOKEN = b"throwaway-shared-secret-for-tests"


def _make_activation():
    # A plausible decode-step activation: [batch=1, seq=1, hidden=16] float16.
    shape = (1, 1, 16)
    payload = bytes(range(32))  # 16 float16 elems * 2 bytes = 32 bytes
    return ActivationHeader(stage_id=2, token_index=7, dtype=DType.FLOAT16,
                            shape=shape), payload


class TestFraming(unittest.TestCase):
    def test_activation_round_trip(self):
        header, payload = _make_activation()
        frame = framing.encode_activation(header, payload)
        got_h, got_view = framing.decode_activation(frame)
        self.assertEqual(got_h, header)
        self.assertEqual(bytes(got_view), payload)

    def test_payload_shape_mismatch_on_encode(self):
        header, _ = _make_activation()
        with self.assertRaises(FrameError):
            framing.encode_activation(header, b"\x00" * 8)  # too short for shape

    def test_bad_magic(self):
        with self.assertRaises(FrameError):
            framing.decode_activation(b"XXXX" + b"\x00" * 20)

    def test_oversize_payload_rejected(self):
        header, payload = _make_activation()
        frame = framing.encode_activation(header, payload)
        with self.assertRaises(FrameError):
            framing.decode_activation(frame, max_payload=4)

    def test_trailing_bytes_rejected(self):
        header, payload = _make_activation()
        frame = framing.encode_activation(header, payload) + b"garbage"
        with self.assertRaises(FrameError):
            framing.decode_activation(frame)

    def test_control_round_trip(self):
        f = framing.frame_control(framing.MsgType.AUTH_CHALLENGE, b"nonce123")
        mtype, body = framing.decode_control(f)
        self.assertEqual(mtype, framing.MsgType.AUTH_CHALLENGE)
        self.assertEqual(body, b"nonce123")

    def test_unknown_dtype_rejected(self):
        with self.assertRaises(FrameError):
            framing.dtype_size(99)


class TestAuth(unittest.TestCase):
    def test_sign_verify_accepts_correct(self):
        nonce = auth.new_nonce()
        resp = auth.sign(TOKEN, nonce)
        self.assertTrue(auth.verify(TOKEN, nonce, resp))

    def test_verify_rejects_wrong_token(self):
        nonce = auth.new_nonce()
        resp = auth.sign(b"other-token", nonce)
        self.assertFalse(auth.verify(TOKEN, nonce, resp))

    def test_empty_token_fails_closed(self):
        with self.assertRaises(auth.AuthError):
            auth.sign(b"", auth.new_nonce())

    def test_load_token_requires_env(self):
        with self.assertRaises(auth.AuthError):
            auth.load_token(env={})
        self.assertEqual(auth.load_token(env={"SOFMAT_AUTH_TOKEN": "abc"}),
                         b"abc")


class _Worker:
    """A one-connection echo worker on loopback, in a background thread."""

    def __init__(self, token):
        self.token = token
        self.srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self.srv.bind(("127.0.0.1", 0))
        self.srv.listen(1)
        self.host, self.port = self.srv.getsockname()
        self.auth_result = None
        self.thread = threading.Thread(target=self._run, daemon=True)
        self.thread.start()

    def _run(self):
        conn, _ = self.srv.accept()
        try:
            t = transport.accept(conn, self.token, max_activation_mb=8)
        except auth.AuthError:
            self.auth_result = "rejected"
            conn.close()
            return
        self.auth_result = "ok"
        try:
            while True:
                header, view = t.recv_activation()
                t.send_activation(header, bytes(view))  # echo back
        except transport.TransportError:
            pass
        finally:
            t.close()

    def close(self):
        try:
            self.srv.close()
        except OSError:
            pass


class TestTcpEndToEnd(unittest.TestCase):
    def test_authenticated_activation_round_trip(self):
        w = _Worker(TOKEN)
        try:
            t = transport.connect(w.host, w.port, TOKEN, max_activation_mb=8)
            header, payload = _make_activation()
            t.send_activation(header, payload)
            got_h, got_view = t.recv_activation()
            self.assertEqual(got_h, header)
            self.assertEqual(bytes(got_view), payload)
            # boundary overhead probe returns a sane positive number (ms)
            ms = transport.probe_boundary_overhead_ms(t, header, payload, rounds=5)
            self.assertGreaterEqual(ms, 0.0)
            self.assertLess(ms, 1000.0)
            t.close()
        finally:
            w.close()
        self.assertEqual(w.auth_result, "ok")

    def test_wrong_token_is_rejected(self):
        w = _Worker(TOKEN)
        try:
            with self.assertRaises(auth.AuthError):
                transport.connect(w.host, w.port, b"WRONG-TOKEN",
                                  max_activation_mb=8)
        finally:
            w.close()
        w.thread.join(timeout=2.0)
        self.assertEqual(w.auth_result, "rejected")


class TestBufferedSender(unittest.TestCase):
    def test_overlapped_send_preserves_order(self):
        w = _Worker(TOKEN)
        try:
            t = transport.connect(w.host, w.port, TOKEN, max_activation_mb=8)
            sender = pipelined.BufferedSender(t, depth=2)
            payload = bytes(32)
            n = 12
            for i in range(n):
                h = ActivationHeader(stage_id=1, token_index=i,
                                     dtype=DType.FLOAT16, shape=(1, 1, 16))
                sender.send_activation(h, payload)
            # recv happens on this thread while the worker thread sends: overlap
            got = [t.recv_activation()[0].token_index for _ in range(n)]
            self.assertEqual(got, list(range(n)))  # FIFO preserved
            sender.flush()
            sender.close()
            t.close()
        finally:
            w.close()

    def test_send_error_surfaces(self):
        w = _Worker(TOKEN)
        t = transport.connect(w.host, w.port, TOKEN, max_activation_mb=8)
        w.close()
        t._sock.close()  # break the underlying socket under the buffer
        sender = pipelined.BufferedSender(t, depth=2)
        h = ActivationHeader(stage_id=1, token_index=0,
                             dtype=DType.FLOAT16, shape=(1, 1, 16))
        with self.assertRaises(transport.TransportError):
            for _ in range(5):
                sender.send_activation(h, bytes(32))
            sender.close()


if __name__ == "__main__":
    unittest.main()
