// Package auth — autenticación master<->worker compartida. Port del
// núcleo de `common/auth.py`: HMAC-SHA256(token, nonce) verificado en tiempo
// constante. El token NUNCA cruza el cable; se lee del entorno
// (SOFMAT_AUTH_TOKEN), jamás hardcodeado (A02). Piso que impide que un proceso
// perdido de la LAN inyecte una activación forjada.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	NonceLen        = 32
	DefaultTokenEnv = "SOFMAT_AUTH_TOKEN"
)

// ErrAuth: autenticación fallida o mal configurada. Fail-closed.
var ErrAuth = errors.New("auth: fallo o mala configuración")

// NewNonce devuelve un reto aleatorio fresco (crypto/rand = CSPRNG).
func NewNonce() ([]byte, error) {
	b := make([]byte, NonceLen)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("%w: rand: %v", ErrAuth, err)
	}
	return b, nil
}

// Sign devuelve HMAC-SHA256 del reto bajo el token compartido.
func Sign(token, nonce []byte) ([]byte, error) {
	if len(token) == 0 {
		return nil, fmt.Errorf("%w: token vacío (define %s)", ErrAuth, DefaultTokenEnv)
	}
	if len(nonce) != NonceLen {
		return nil, fmt.Errorf("%w: longitud de nonce inválida", ErrAuth)
	}
	m := hmac.New(sha256.New, token)
	m.Write(nonce)
	return m.Sum(nil), nil
}

// Verify comprueba en tiempo constante la respuesta a un reto (sin canal lateral de tiempo).
func Verify(token, nonce, response []byte) bool {
	expected, err := Sign(token, nonce)
	if err != nil {
		return false
	}
	return hmac.Equal(expected, response)
}

// LoadToken lee el token del entorno. Fail-closed si no está o está vacío.
func LoadToken(varName string) ([]byte, error) {
	if varName == "" {
		varName = DefaultTokenEnv
	}
	raw := strings.TrimSpace(os.Getenv(varName))
	if raw == "" {
		return nil, fmt.Errorf("%w: %s sin definir — sin auth master<->worker no arranco", ErrAuth, varName)
	}
	return []byte(raw), nil
}
