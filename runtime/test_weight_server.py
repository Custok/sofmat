"""End-to-end auth test for the served-weights server + client.

Starts the real ``weight_server`` on a loopback port and drives it with
``served_loader.HttpRangeSource``, using the REAL ``common.auth`` module so the
whole HMAC challenge/response is exercised: an authenticated range request gets
206 + the right bytes; an unauthenticated or wrong-token one gets 401.

Run:  python3 -m unittest test_weight_server   (from the runtime/ directory,
with sofmat/common on the path — the bootstrap below adds it relative to the
repo root, same pattern the other modules use).
"""

from __future__ import annotations

import os
import sys
import tempfile
import threading
import unittest
import urllib.error

_HERE = os.path.dirname(__file__)
sys.path.insert(0, _HERE)
# common/ lives at <repo>/common; runtime/ is <repo>/runtime.
sys.path.insert(0, os.path.join(_HERE, os.pardir, "common"))

try:
    import auth  # sofmat/common/auth.py
    _HAS_AUTH = True
except ImportError:  # common/ not on path (e.g. isolated checkout) -> skip
    _HAS_AUTH = False

from served_loader import HttpRangeSource  # noqa: E402
from weight_server import make_server  # noqa: E402


@unittest.skipUnless(_HAS_AUTH, "sofmat/common/auth.py not importable")
class ServedWeightsAuth(unittest.TestCase):
    def setUp(self):
        self.token = b"unit-test-shared-token-1234567890"
        self.blob = bytes((i % 251) for i in range(50_000))
        fh = tempfile.NamedTemporaryFile(delete=False)
        fh.write(self.blob)
        fh.close()
        self.path = fh.name

        verify = lambda h: auth.verify_request(self.token, h)  # noqa: E731
        self.server = make_server({"/model": self.path}, verify, "127.0.0.1", 0)
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.url = f"http://127.0.0.1:{self.port}/model"

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        os.unlink(self.path)

    def _authed(self, token):
        return lambda: auth.request_headers(token)

    def test_authenticated_range_returns_correct_bytes(self):
        src = HttpRangeSource(self.url, auth_headers=self._authed(self.token))
        data = src.fetch(1000, 256)
        self.assertEqual(data, self.blob[1000:1256])

    def test_unauthenticated_is_rejected(self):
        src = HttpRangeSource(self.url)  # no auth headers
        with self.assertRaises(urllib.error.HTTPError) as ctx:
            src.fetch(0, 100)
        self.assertEqual(ctx.exception.code, 401)

    def test_wrong_token_is_rejected(self):
        src = HttpRangeSource(self.url, auth_headers=self._authed(b"the-wrong-token"))
        with self.assertRaises(urllib.error.HTTPError) as ctx:
            src.fetch(0, 100)
        self.assertEqual(ctx.exception.code, 401)

    def test_unknown_object_is_404_even_when_authed(self):
        bad = f"http://127.0.0.1:{self.port}/nope"
        src = HttpRangeSource(bad, auth_headers=self._authed(self.token))
        with self.assertRaises(urllib.error.HTTPError) as ctx:
            src.fetch(0, 100)
        self.assertEqual(ctx.exception.code, 404)


if __name__ == "__main__":
    unittest.main()
