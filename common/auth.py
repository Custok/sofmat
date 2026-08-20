"""sofmat — shared master<->worker authentication (OWASP A01).

One auth module for the whole project. Two peers must prove knowledge of a
shared secret before either exchanges data:

  * the **activation transport** (``transport/``) — a socket challenge-response
    handshake on connect;
  * the **served-weights endpoint** (``runtime/served_loader`` + its HTTP
    range-server) — a per-request HMAC on each Range request.

Both use the SAME primitive: ``HMAC-SHA256(token, nonce)``, verified in
constant time (``hmac.compare_digest``). The token never crosses the wire and
is read from the environment only (``SOFMAT_AUTH_TOKEN``), never hardcoded
(OWASP A02). This is the floor that stops a stray LAN process from injecting a
forged activation or downloading the model — not a replacement for a private
network. mTLS/RDMA can replace it behind the same surface.

Pure standard library (``hmac``, ``hashlib``, ``os.urandom``, ``base64``).
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import os

NONCE_LEN = 32
DEFAULT_TOKEN_ENV = "SOFMAT_AUTH_TOKEN"

# HTTP header names for the served-weights range-server auth (see request_headers).
HDR_NONCE = "X-Sofmat-Nonce"
HDR_AUTH = "X-Sofmat-Auth"


class AuthError(Exception):
    """Raised when authentication fails or is misconfigured. Fail closed."""


# -- core primitives (used by the socket handshake) ----------------------
def new_nonce() -> bytes:
    """A fresh random challenge. ``os.urandom`` is a CSPRNG."""
    return os.urandom(NONCE_LEN)


def sign(token: bytes, nonce: bytes) -> bytes:
    """HMAC-SHA256 of the challenge under the shared token."""
    if not token:
        raise AuthError("empty auth token (set %s)" % DEFAULT_TOKEN_ENV)
    if len(nonce) != NONCE_LEN:
        raise AuthError("bad nonce length")
    return hmac.new(token, nonce, hashlib.sha256).digest()


def verify(token: bytes, nonce: bytes, response: bytes) -> bool:
    """Constant-time check of a challenge response (no timing side channel)."""
    expected = sign(token, nonce)
    return hmac.compare_digest(expected, response)


def load_token(env: "dict[str, str] | None" = None,
               var: str = DEFAULT_TOKEN_ENV) -> bytes:
    """Read the shared token from the environment. Fail-closed if unset/empty.

    ``config.local.yaml`` / the deploy injects ``SOFMAT_AUTH_TOKEN``; the value
    never lives in code or in the public repo (OWASP A02).
    """
    src = os.environ if env is None else env
    raw = (src.get(var) or "").strip()
    if not raw:
        raise AuthError(
            "%s is unset — refusing to run without master<->worker auth "
            "(config.local.yaml / env)" % var)
    return raw.encode("utf-8")


# -- HTTP helper (used by the served-weights range client/server) --------
def request_headers(token: bytes, *, nonce: "bytes | None" = None
                    ) -> "dict[str, str]":
    """Auth headers a range-request client attaches to EACH HTTP request.

    Stateless: the client picks a fresh nonce and signs it; the server verifies
    with :func:`verify_request`. Proving the HMAC proves knowledge of the shared
    token (A01). Replaying a signed GET only re-reads the same bytes (read-only
    weight fetch), so a client nonce is sufficient here; a server that wants
    anti-replay can additionally track seen nonces.
    """
    n = nonce if nonce is not None else new_nonce()
    mac = sign(token, n)
    return {
        HDR_NONCE: base64.b64encode(n).decode("ascii"),
        HDR_AUTH: mac.hex(),
    }


def verify_request(token: bytes, headers: "dict[str, str]") -> bool:
    """Server side: verify the auth headers of one HTTP range request.

    Accepts any mapping with case-sensitive header names as produced by
    :func:`request_headers`. Returns False (never raises) on missing or
    malformed headers so the server can answer 401 uniformly.
    """
    try:
        nonce = base64.b64decode(headers[HDR_NONCE])
        response = bytes.fromhex(headers[HDR_AUTH])
    except (KeyError, ValueError, TypeError):
        return False
    if len(nonce) != NONCE_LEN:
        return False
    return verify(token, nonce, response)
