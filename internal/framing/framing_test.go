package framing

import (
	"bytes"
	"errors"
	"testing"
)

func TestActivationRoundtrip(t *testing.T) {
	h := ActivationHeader{StageID: 3, TokenIndex: 42, DType: Float16, Shape: []uint32{2, 4}}
	payload := make([]byte, 2*4*2) // 2*4 elems * 2 bytes (float16)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	frame, err := EncodeActivation(h, payload)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, gp, err := DecodeActivation(frame, DefaultMaxPayload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.StageID != 3 || got.TokenIndex != 42 || got.DType != Float16 {
		t.Fatalf("header mismatch: %+v", got)
	}
	if len(got.Shape) != 2 || got.Shape[0] != 2 || got.Shape[1] != 4 {
		t.Fatalf("shape mismatch: %v", got.Shape)
	}
	if !bytes.Equal(gp, payload) {
		t.Fatalf("payload mismatch")
	}
}

func TestPayloadLenMismatchRejected(t *testing.T) {
	h := ActivationHeader{StageID: 0, TokenIndex: 0, DType: UInt8, Shape: []uint32{10}}
	_, err := EncodeActivation(h, []byte{1, 2, 3}) // 3 != 10
	if !errors.Is(err, ErrFrame) {
		t.Fatalf("esperaba ErrFrame, got %v", err)
	}
}

func TestBadMagicRejected(t *testing.T) {
	h := ActivationHeader{StageID: 0, TokenIndex: 0, DType: UInt8, Shape: []uint32{4}}
	frame, _ := EncodeActivation(h, []byte{1, 2, 3, 4})
	frame[0] = 'X'
	if _, _, err := DecodeActivation(frame, DefaultMaxPayload); !errors.Is(err, ErrFrame) {
		t.Fatalf("esperaba ErrFrame por magic")
	}
}

func TestPayloadOverCapRejected(t *testing.T) {
	h := ActivationHeader{StageID: 0, TokenIndex: 0, DType: UInt8, Shape: []uint32{100}}
	frame, _ := EncodeActivation(h, make([]byte, 100))
	if _, _, err := DecodeActivation(frame, 50); !errors.Is(err, ErrFrame) {
		t.Fatalf("esperaba ErrFrame por cap")
	}
}

func TestNdimOverMaxRejected(t *testing.T) {
	h := ActivationHeader{StageID: 0, TokenIndex: 0, DType: UInt8, Shape: []uint32{1, 1, 1, 1, 1, 1, 1, 1, 1}} // 9 > MaxNDim
	if _, err := EncodeActivation(h, make([]byte, 1)); !errors.Is(err, ErrFrame) {
		t.Fatalf("esperaba ErrFrame por ndim")
	}
}

func TestControlRoundtrip(t *testing.T) {
	nonce := []byte("nonce-abc-123")
	frame, err := FrameControl(MsgAuthChallenge, nonce)
	if err != nil {
		t.Fatalf("frame control: %v", err)
	}
	mt, body, err := DecodeControl(frame)
	if err != nil {
		t.Fatalf("decode control: %v", err)
	}
	if mt != MsgAuthChallenge || !bytes.Equal(body, nonce) {
		t.Fatalf("control mismatch: %d %q", mt, body)
	}
}

// Vector fijo: bytes exactos del wire (cross-check contra el formato Python).
// magic SOFM(4) ver 01 msgType 01 flags 00 pad 00 | stageID 0001 tokenIx 00000002
// dtype 05(uint8) ndim 01 | shape 00000003 | nBytes 0000000000000003 | payload 0A0B0C
func TestWireBytesExact(t *testing.T) {
	h := ActivationHeader{StageID: 1, TokenIndex: 2, DType: UInt8, Shape: []uint32{3}}
	frame, _ := EncodeActivation(h, []byte{0x0A, 0x0B, 0x0C})
	want := []byte{
		'S', 'O', 'F', 'M', 0x01, 0x01, 0x00, 0x00,
		0x00, 0x01, 0x00, 0x00, 0x00, 0x02, 0x05, 0x01,
		0x00, 0x00, 0x00, 0x03,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03,
		0x0A, 0x0B, 0x0C,
	}
	if !bytes.Equal(frame, want) {
		t.Fatalf("wire bytes distintos del formato Python:\n got %x\nwant %x", frame, want)
	}
}
