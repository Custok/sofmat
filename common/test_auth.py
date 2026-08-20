"""Unit tests for the shared auth module (pure stdlib, no infra).

Covers the socket challenge-response primitives, the fail-closed token loader,
and the HTTP range-request helpers used by the served-weights endpoint.
"""

import unittest

import auth

TOKEN = b"throwaway-shared-secret-for-tests"


class TestChallengeResponse(unittest.TestCase):
    def test_sign_verify_roundtrip(self):
        nonce = auth.new_nonce()
        self.assertTrue(auth.verify(TOKEN, nonce, auth.sign(TOKEN, nonce)))

    def test_verify_rejects_wrong_token(self):
        nonce = auth.new_nonce()
        self.assertFalse(auth.verify(TOKEN, nonce, auth.sign(b"other", nonce)))

    def test_empty_token_fails_closed(self):
        with self.assertRaises(auth.AuthError):
            auth.sign(b"", auth.new_nonce())

    def test_bad_nonce_length(self):
        with self.assertRaises(auth.AuthError):
            auth.sign(TOKEN, b"short")


class TestLoadToken(unittest.TestCase):
    def test_requires_env(self):
        with self.assertRaises(auth.AuthError):
            auth.load_token(env={})

    def test_reads_default_var(self):
        self.assertEqual(auth.load_token(env={"SOFMAT_AUTH_TOKEN": " abc "}),
                         b"abc")

    def test_custom_var(self):
        self.assertEqual(
            auth.load_token(env={"MY_TOKEN": "x"}, var="MY_TOKEN"), b"x")


class TestHttpHelpers(unittest.TestCase):
    def test_request_headers_verify_accepts(self):
        headers = auth.request_headers(TOKEN)
        self.assertIn(auth.HDR_NONCE, headers)
        self.assertIn(auth.HDR_AUTH, headers)
        self.assertTrue(auth.verify_request(TOKEN, headers))

    def test_verify_request_rejects_wrong_token(self):
        headers = auth.request_headers(b"other-token")
        self.assertFalse(auth.verify_request(TOKEN, headers))

    def test_verify_request_rejects_missing_headers(self):
        self.assertFalse(auth.verify_request(TOKEN, {}))

    def test_verify_request_rejects_tampered_mac(self):
        headers = auth.request_headers(TOKEN)
        headers[auth.HDR_AUTH] = "00" * 32
        self.assertFalse(auth.verify_request(TOKEN, headers))

    def test_verify_request_rejects_malformed(self):
        self.assertFalse(auth.verify_request(
            TOKEN, {auth.HDR_NONCE: "not-base64!!", auth.HDR_AUTH: "zz"}))


if __name__ == "__main__":
    unittest.main()
