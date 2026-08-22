// Package framing — formato binario de wire para las activaciones/handoff del
// pipeline sofmat. Port 1:1 del prototipo `transport/framing.py` (los tests
// Python son la spec de comportamiento).
//
// Seguridad (README):
//   - A08: nada de deserialización dinámica en el camino caliente; cabecera
//     fija y payload contiguo. Un frame no puede ejecutar nada al decodificar.
//   - A04: cada campo se valida ANTES de exponer el payload — magic, versión,
//     ndim/dims acotados, y prod(shape)*dtype_size == len(payload). Frame
//     malformado -> error, nunca llega a la GPU.
//
// Layout (big-endian): magic(4) ver(1) msgType(1) flags(1) pad(1)
//
//	ACTIVATION: stageID(2) tokenIx(4) dtype(1) ndim(1) shape(4*ndim) nBytes(8) payload
//	control:    len(8) payload
package framing

import (
	"encoding/binary"
	"errors"
	"fmt"
)

var Magic = [4]byte{'S', 'O', 'F', 'M'}

const (
	ProtoVersion      = 1
	MaxNDim           = 8
	MaxDim            = 1 << 24 // 16M elementos por eje: absurdo para una activación
	DefaultMaxPayload = 8 * 1024 * 1024
	prefixLen         = 8 // magic(4)+ver(1)+msgType(1)+flags(1)+pad(1)
	actFixedLen       = 8 // stageID(2)+tokenIx(4)+dtype(1)+ndim(1)
	lenLen            = 8 // nBytes / control len (uint64)
)

type MsgType uint8

const (
	MsgActivation    MsgType = 1
	MsgAuthChallenge MsgType = 2
	MsgAuthResponse  MsgType = 3
	MsgAck           MsgType = 4
	MsgError         MsgType = 5
)

type DType uint8

const (
	Float32  DType = 1
	Float16  DType = 2
	BFloat16 DType = 3
	Int8     DType = 4
	UInt8    DType = 5
	Int32    DType = 6
	Int64    DType = 7
)

var dtypeSize = map[DType]int{
	Float32: 4, Float16: 2, BFloat16: 2, Int8: 1, UInt8: 1, Int32: 4, Int64: 8,
}

// ErrFrame: cualquier frame malformado o fuera de límites. Nunca confiar en el wire.
var ErrFrame = errors.New("framing: frame malformado")

func DTypeSize(d DType) (int, error) {
	if s, ok := dtypeSize[d]; ok {
		return s, nil
	}
	return 0, fmt.Errorf("%w: dtype desconocido %d", ErrFrame, d)
}

// ActivationHeader — metadata validada de un tensor de activación en una frontera.
type ActivationHeader struct {
	StageID    uint16
	TokenIndex uint32
	DType      DType
	Shape      []uint32
}

func (h ActivationHeader) ExpectedBytes() (int, error) {
	s, err := DTypeSize(h.DType)
	if err != nil {
		return 0, err
	}
	n := s
	for _, d := range h.Shape {
		n *= int(d)
	}
	return n, nil
}

func validateShape(shape []uint32) error {
	if len(shape) > MaxNDim {
		return fmt.Errorf("%w: demasiadas dims %d", ErrFrame, len(shape))
	}
	for _, d := range shape {
		if d > MaxDim {
			return fmt.Errorf("%w: dim %d fuera de rango", ErrFrame, d)
		}
	}
	return nil
}

// EncodeActivation serializa una activación en un frame (cabecera + payload).
func EncodeActivation(h ActivationHeader, payload []byte) ([]byte, error) {
	if err := validateShape(h.Shape); err != nil {
		return nil, err
	}
	want, err := h.ExpectedBytes()
	if err != nil {
		return nil, err
	}
	if len(payload) != want {
		return nil, fmt.Errorf("%w: payload %dB != shape/dtype implica %dB", ErrFrame, len(payload), want)
	}
	buf := make([]byte, 0, prefixLen+actFixedLen+4*len(h.Shape)+lenLen+len(payload))
	buf = append(buf, Magic[:]...)
	buf = append(buf, ProtoVersion, byte(MsgActivation), 0, 0)
	var fixed [actFixedLen]byte
	binary.BigEndian.PutUint16(fixed[0:], h.StageID)
	binary.BigEndian.PutUint32(fixed[2:], h.TokenIndex)
	fixed[6] = byte(h.DType)
	fixed[7] = byte(len(h.Shape))
	buf = append(buf, fixed[:]...)
	for _, d := range h.Shape {
		var s [4]byte
		binary.BigEndian.PutUint32(s[:], d)
		buf = append(buf, s[:]...)
	}
	var nb [lenLen]byte
	binary.BigEndian.PutUint64(nb[:], uint64(len(payload)))
	buf = append(buf, nb[:]...)
	buf = append(buf, payload...)
	return buf, nil
}

// DecodeActivation parsea un frame ACTIVATION completo. Valida todo antes de
// devolver el payload (subslice, sin copia extra).
func DecodeActivation(buf []byte, maxPayload int) (ActivationHeader, []byte, error) {
	var h ActivationHeader
	if len(buf) < prefixLen+actFixedLen {
		return h, nil, fmt.Errorf("%w: frame demasiado corto", ErrFrame)
	}
	if [4]byte{buf[0], buf[1], buf[2], buf[3]} != Magic {
		return h, nil, fmt.Errorf("%w: bad magic", ErrFrame)
	}
	if buf[4] != ProtoVersion {
		return h, nil, fmt.Errorf("%w: versión %d no soportada", ErrFrame, buf[4])
	}
	if MsgType(buf[5]) != MsgActivation {
		return h, nil, fmt.Errorf("%w: esperaba ACTIVATION, msgType %d", ErrFrame, buf[5])
	}
	if buf[6] != 0 || buf[7] != 0 {
		return h, nil, fmt.Errorf("%w: bits reservados", ErrFrame)
	}
	off := prefixLen
	h.StageID = binary.BigEndian.Uint16(buf[off:])
	h.TokenIndex = binary.BigEndian.Uint32(buf[off+2:])
	h.DType = DType(buf[off+6])
	ndim := int(buf[off+7])
	off += actFixedLen
	if ndim > MaxNDim {
		return h, nil, fmt.Errorf("%w: ndim %d > MaxNDim", ErrFrame, ndim)
	}
	if len(buf) < off+4*ndim+lenLen {
		return h, nil, fmt.Errorf("%w: frame truncado en shape/len", ErrFrame)
	}
	h.Shape = make([]uint32, ndim)
	for i := 0; i < ndim; i++ {
		h.Shape[i] = binary.BigEndian.Uint32(buf[off:])
		off += 4
	}
	if err := validateShape(h.Shape); err != nil {
		return h, nil, err
	}
	nBytes := binary.BigEndian.Uint64(buf[off:])
	off += lenLen
	if int(nBytes) > maxPayload {
		return h, nil, fmt.Errorf("%w: payload %dB > cap %dB", ErrFrame, nBytes, maxPayload)
	}
	if off+int(nBytes) != len(buf) {
		return h, nil, fmt.Errorf("%w: len de payload no cuadra con el frame", ErrFrame)
	}
	want, err := h.ExpectedBytes()
	if err != nil {
		return h, nil, err
	}
	if int(nBytes) != want {
		return h, nil, fmt.Errorf("%w: payload inconsistente con shape/dtype", ErrFrame)
	}
	return h, buf[off : off+int(nBytes)], nil
}

// FrameControl codifica un frame de control pequeño (auth handshake, ACK, ERROR).
func FrameControl(mt MsgType, payload []byte) ([]byte, error) {
	if len(payload) > 4096 {
		return nil, fmt.Errorf("%w: control payload demasiado grande", ErrFrame)
	}
	buf := make([]byte, 0, prefixLen+lenLen+len(payload))
	buf = append(buf, Magic[:]...)
	buf = append(buf, ProtoVersion, byte(mt), 0, 0)
	var l [lenLen]byte
	binary.BigEndian.PutUint64(l[:], uint64(len(payload)))
	buf = append(buf, l[:]...)
	buf = append(buf, payload...)
	return buf, nil
}

// DecodeControl parsea un frame de control.
func DecodeControl(buf []byte) (MsgType, []byte, error) {
	if len(buf) < prefixLen+lenLen {
		return 0, nil, fmt.Errorf("%w: control corto", ErrFrame)
	}
	if [4]byte{buf[0], buf[1], buf[2], buf[3]} != Magic {
		return 0, nil, fmt.Errorf("%w: bad magic", ErrFrame)
	}
	if buf[4] != ProtoVersion {
		return 0, nil, fmt.Errorf("%w: versión %d", ErrFrame, buf[4])
	}
	n := binary.BigEndian.Uint64(buf[prefixLen:])
	body := buf[prefixLen+lenLen:]
	if uint64(len(body)) != n {
		return 0, nil, fmt.Errorf("%w: control len mismatch", ErrFrame)
	}
	return MsgType(buf[5]), body, nil
}
