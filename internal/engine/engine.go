// Package engine binds sofmat to the inference engine (libllama). The real
// backend uses cgo to call llama_state_seq_get_data / llama_state_seq_set_data
// to extract KV on the prefill node and inject it on the decode node — the core
// of the disaggregated handoff. This file declares the interface; the cgo
// backend lands behind a build tag when ready. Lane: engine (with transport).
package engine

import "errors"

// ErrNoBackend is returned until the cgo libllama backend is wired in.
var ErrNoBackend = errors.New("engine: no backend (cgo libllama binding pending)")

// KVCodec extracts and injects a sequence's KV state across the handoff.
type KVCodec interface {
	// GetSeqData serializes sequence seq's KV (prefill side).
	GetSeqData(seq int) ([]byte, error)
	// SetSeqData injects a serialized KV into sequence seq (decode side).
	SetSeqData(seq int, data []byte) error
}

// Default returns the active KV codec, or ErrNoBackend until the binding exists.
func Default() (KVCodec, error) { return nil, ErrNoBackend }
