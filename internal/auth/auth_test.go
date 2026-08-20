package auth

import (
	"bytes"
	"testing"
)

func TestSignVerifyRoundtrip(t *testing.T) {
	token := []byte("secreto-compartido")
	nonce, err := NewNonce()
	if err != nil || len(nonce) != NonceLen {
		t.Fatalf("nonce: %v len=%d", err, len(nonce))
	}
	sig, err := Sign(token, nonce)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !Verify(token, nonce, sig) {
		t.Fatal("verify debería pasar con token correcto")
	}
	if Verify([]byte("otro-token"), nonce, sig) {
		t.Fatal("verify NO debería pasar con token distinto")
	}
}

func TestEmptyTokenFails(t *testing.T) {
	nonce, _ := NewNonce()
	if _, err := Sign(nil, nonce); err == nil {
		t.Fatal("token vacío debe fallar")
	}
}

func TestNoncesDiffer(t *testing.T) {
	a, _ := NewNonce()
	b, _ := NewNonce()
	if bytes.Equal(a, b) {
		t.Fatal("dos nonces no deberían coincidir")
	}
}
