"""sofmat runtime — authenticated served-weights range server.

The server side of "deploy the model once, each worker pulls its slice"
(``served_loader.py`` is the client). One node runs this pointing at the local
GGUF file(s); every stage worker fetches ONLY the byte ranges of its layers.

SECURITY (OWASP A01/A02, per node-c's review): serving model weights on the LAN
UNAUTHENTICATED lets anyone pull the whole model. So every request is verified
against the shared token before a single byte is served (``verify`` callable,
wired by the deployment to ``common.auth.verify_request(token, headers)``); a
failed or missing credential gets a uniform 401. The token lives only in the
environment, never here. ``verify`` is injected as a callable so this module has
no hard import of ``common`` and is unit-testable on its own.

Pure standard library (http.server). Honours a single ``bytes=START-END`` Range
and answers 206 Partial Content — exactly what ``served_loader.HttpRangeSource``
sends. Read-only: it never writes, lists a directory, or serves a path outside
its explicit file map (no traversal).
"""

from __future__ import annotations

import re
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Callable

_RANGE_RE = re.compile(r"^bytes=(\d+)-(\d+)$")

# verify(headers) -> bool  (deployment wires it to common.auth.verify_request)
VerifyFn = Callable[[dict], bool]


def make_handler(file_map: dict[str, str], verify: VerifyFn):
    """Build a request handler serving byte ranges of ``file_map`` (URL path ->
    local file) to callers that pass ``verify``. No path escapes the map.
    """

    class WeightRangeHandler(BaseHTTPRequestHandler):
        # Quiet by default; a deployment can attach real logging. Never log the
        # auth headers (would defeat the point).
        def log_message(self, *_args):  # noqa: D401
            pass

        def _deny(self, code: int, msg: str) -> None:
            self.send_response(code)
            self.send_header("Content-Length", "0")
            self.end_headers()
            del msg  # body omitted on purpose (no info leak)

        def do_GET(self) -> None:  # noqa: N802 (http.server API)
            # 1. Auth FIRST — before touching the filesystem or the range.
            headers = {k: v for k, v in self.headers.items()}
            if not verify(headers):
                self._deny(401, "unauthorized")
                return
            # 2. Path must be an explicit, known object (no traversal, no listing).
            local = file_map.get(self.path)
            if local is None:
                self._deny(404, "unknown object")
                return
            # 3. A single concrete byte range is required (this is a slice fetch).
            rng = self.headers.get("Range", "")
            m = _RANGE_RE.match(rng.strip())
            if not m:
                self._deny(400, "a single bytes=START-END range is required")
                return
            start, end = int(m.group(1)), int(m.group(2))
            if end < start:
                self._deny(416, "range not satisfiable")
                return
            nbytes = end - start + 1
            try:
                with open(local, "rb") as fh:
                    fh.seek(start)
                    data = fh.read(nbytes)
            except OSError:
                self._deny(404, "object unreadable")
                return
            if len(data) != nbytes:
                self._deny(416, "range beyond end of object")
                return
            self.send_response(206)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(len(data)))
            self.send_header("Content-Range", f"bytes {start}-{end}/*")
            self.end_headers()
            self.wfile.write(data)

    return WeightRangeHandler


def make_server(
    file_map: dict[str, str],
    verify: VerifyFn,
    host: str = "0.0.0.0",
    port: int = 0,
) -> ThreadingHTTPServer:
    """Create (not start) an authenticated weight range server.

    ``port=0`` binds a free port (useful for tests / ephemeral serving). Call
    ``serve_forever()`` to run it, or drive it from the deployment. The caller
    owns the token and passes ``verify = lambda h: auth.verify_request(tok, h)``.
    """
    return ThreadingHTTPServer((host, port), make_handler(file_map, verify))
