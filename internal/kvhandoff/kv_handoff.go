// Package kvhandoff — handoff de KV bulk-chunked para serving desagregado
// prefill→decode. Port de `transport/kv_handoff.py`. Mueve el
// `llama_state_seq_get_data` de una secuencia (0.5–10 GB, bandwidth-bound)
// troceándolo en chunks acotados por el cap del transporte, sobre el mismo
// Transport (hereda auth + framing binario + cap anti-DoS). El receptor
// reensambla verificando tamaño + sha256 ANTES de devolver.
//
// v1 = transferencia bulk fiable. El SCATTER cross-topología por-capa (v2) y el
// binding cgo a state_seq_get_data/set_data van encima.
package kvhandoff

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/Custok/sofmat/internal/framing"
)

const (
	stageManifest = 0xFFFE
	stageChunk    = 0xFFFD
	defaultChunk  = 4 * 1024 * 1024
	hashLen       = 32
	manifestLen   = 8 + 4 + hashLen // total(u64) + nChunks(u32) + sha256
)

// ErrHandoff: fallo de protocolo/integridad. Aborta la transferencia.
var ErrHandoff = errors.New("kvhandoff: fallo de protocolo/integridad")

// MaxSeqID es el mayor seqID admisible en el borde del engine. El seqID viaja
// por el wire como uint32, pero el C-API de llama.cpp
// (llama_state_seq_get_data/set_data, id de secuencia/slot) lo consume como
// int32: un valor con el bit alto puesto se volvería negativo y colaría un id
// inválido al backend. Guarda pactada con coordinator-lane al aceptar seqID uint32.
const MaxSeqID = uint32(1)<<31 - 1

// SeqIDToSlot valida un seqID de wire (uint32) y lo castea al int32 que consume
// el C-API de llama.cpp. Rechaza el bit alto (>MaxSeqID) fail-closed: en el
// borde del engine un seq negativo nunca llega al backend. Llámese SIEMPRE antes
// de pasar un seqID recibido a save/restore de KV.
func SeqIDToSlot(seqID uint32) (int32, error) {
	if seqID > MaxSeqID {
		return 0, fmt.Errorf("%w: seqID %d excede el máximo int32 (%d) — bit alto puesto, rechazado", ErrHandoff, seqID, MaxSeqID)
	}
	return int32(seqID), nil
}

// Transport es lo que kv_handoff necesita del canal (lo cumple *transport.TcpTransport).
type Transport interface {
	SendActivation(h framing.ActivationHeader, payload []byte) error
	RecvActivation() (framing.ActivationHeader, []byte, error)
	MaxPayload() int
}

// SendKV envía el KV serializado (blob) de la secuencia seqID troceado.
func SendKV(tx Transport, seqID uint32, blob []byte, chunkBytes int) (string, error) {
	if chunkBytes <= 0 {
		chunkBytes = defaultChunk
	}
	if cap := tx.MaxPayload(); cap > 0 && chunkBytes > cap {
		return "", fmt.Errorf("%w: chunkBytes %d > cap del transporte %d", ErrHandoff, chunkBytes, cap)
	}
	total := len(blob)
	nChunks := 0
	if total > 0 {
		nChunks = (total + chunkBytes - 1) / chunkBytes
	}
	sum := sha256.Sum256(blob)

	manifest := make([]byte, manifestLen)
	binary.BigEndian.PutUint64(manifest[0:], uint64(total))
	binary.BigEndian.PutUint32(manifest[8:], uint32(nChunks))
	copy(manifest[12:], sum[:])
	if err := tx.SendActivation(framing.ActivationHeader{
		StageID: stageManifest, TokenIndex: seqID, DType: framing.UInt8,
		Shape: []uint32{uint32(len(manifest))}}, manifest); err != nil {
		return "", err
	}

	for i := 0; i < nChunks; i++ {
		lo := i * chunkBytes
		hi := lo + chunkBytes
		if hi > total {
			hi = total
		}
		chunk := blob[lo:hi]
		if err := tx.SendActivation(framing.ActivationHeader{
			StageID: stageChunk, TokenIndex: uint32(i), DType: framing.UInt8,
			Shape: []uint32{uint32(len(chunk))}}, chunk); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%x", sum), nil
}

// RecvKV recibe un blob de KV. Devuelve (seqID, blob). Verifica orden, tamaño y
// sha256 ANTES de devolver — un blob corrupto/truncado nunca llega al consumidor.
func RecvKV(tx Transport) (uint32, []byte, error) {
	h, view, err := tx.RecvActivation()
	if err != nil {
		return 0, nil, err
	}
	if h.StageID != stageManifest {
		return 0, nil, fmt.Errorf("%w: esperaba manifiesto (stage %d), llegó %d", ErrHandoff, stageManifest, h.StageID)
	}
	if len(view) != manifestLen {
		return 0, nil, fmt.Errorf("%w: manifiesto de tamaño inesperado", ErrHandoff)
	}
	total := int(binary.BigEndian.Uint64(view[0:]))
	nChunks := int(binary.BigEndian.Uint32(view[8:]))
	wantHash := make([]byte, hashLen)
	copy(wantHash, view[12:])
	seqID := h.TokenIndex

	blob := make([]byte, 0, total)
	for i := 0; i < nChunks; i++ {
		ch, cview, err := tx.RecvActivation()
		if err != nil {
			return 0, nil, err
		}
		if ch.StageID != stageChunk {
			return 0, nil, fmt.Errorf("%w: esperaba chunk, llegó stage %d", ErrHandoff, ch.StageID)
		}
		if ch.TokenIndex != uint32(i) {
			return 0, nil, fmt.Errorf("%w: chunk fuera de orden: esperaba %d, llegó %d", ErrHandoff, i, ch.TokenIndex)
		}
		blob = append(blob, cview...)
		if len(blob) > total {
			return 0, nil, fmt.Errorf("%w: chunks exceden el total declarado", ErrHandoff)
		}
	}
	if len(blob) != total {
		return 0, nil, fmt.Errorf("%w: tamaño %d != total declarado %d", ErrHandoff, len(blob), total)
	}
	got := sha256.Sum256(blob)
	if !bytes.Equal(got[:], wantHash) {
		return 0, nil, fmt.Errorf("%w: sha256 no cuadra — blob corrupto, abortado", ErrHandoff)
	}
	return seqID, blob, nil
}
