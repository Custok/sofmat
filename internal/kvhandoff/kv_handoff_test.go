package kvhandoff

import (
	"bytes"
	"net"
	"testing"

	"github.com/Custok/sofmat/internal/transport"
)

// TestSeqIDToSlot verifica la guarda pactada con coordinator-lane: seqID uint32 se
// castea a int32 fail-closed, rechazando el bit alto (seq negativo en el C-API).
func TestSeqIDToSlot(t *testing.T) {
	// Válidos: 0, uno intermedio, y el máximo exacto (2^31-1).
	for _, in := range []uint32{0, 1, 1 << 20, MaxSeqID} {
		got, err := SeqIDToSlot(in)
		if err != nil {
			t.Fatalf("SeqIDToSlot(%d) devolvió error inesperado: %v", in, err)
		}
		if got < 0 {
			t.Fatalf("SeqIDToSlot(%d) = %d < 0 — coló un seq negativo", in, got)
		}
		if uint32(got) != in {
			t.Fatalf("SeqIDToSlot(%d) = %d — round-trip roto", in, got)
		}
	}
	// Inválidos: primer valor con bit alto puesto, y el máximo uint32.
	for _, in := range []uint32{1 << 31, (1 << 31) + 1, ^uint32(0)} {
		if _, err := SeqIDToSlot(in); err == nil {
			t.Fatalf("SeqIDToSlot(%d) debía rechazar el bit alto y no lo hizo", in)
		}
	}
}

func pair(t *testing.T) (*transport.TcpTransport, *transport.TcpTransport) {
	t.Helper()
	token := []byte("kv-loopback-token")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	ch := make(chan *transport.TcpTransport, 1)
	go func() {
		conn, _ := ln.Accept()
		tx, _ := transport.Accept(conn, token, 8)
		ch <- tx
	}()
	cli, err := transport.Connect("127.0.0.1", port, token, 8)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return cli, <-ch
}

func roundtrip(t *testing.T, blob []byte, chunkBytes int) []byte {
	send, recv := pair(t)
	defer send.Close()
	defer recv.Close()
	type res struct {
		seq  uint32
		blob []byte
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		seq, b, err := RecvKV(recv)
		ch <- res{seq, b, err}
	}()
	if _, err := SendKV(send, 7, blob, chunkBytes); err != nil {
		t.Fatalf("send: %v", err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatalf("recv: %v", r.err)
	}
	if r.seq != 7 {
		t.Fatalf("seq %d != 7", r.seq)
	}
	return r.blob
}

func TestRoundtripMultichunk(t *testing.T) {
	blob := make([]byte, 20*1024*1024+123)
	for i := range blob {
		blob[i] = byte(i*131 + 7)
	}
	if got := roundtrip(t, blob, 4*1024*1024); !bytes.Equal(got, blob) {
		t.Fatal("blob no cuadra byte-exacto")
	}
}

func TestRoundtripSingleChunk(t *testing.T) {
	blob := bytes.Repeat([]byte("hola KV"), 1000)
	if got := roundtrip(t, blob, 4*1024*1024); !bytes.Equal(got, blob) {
		t.Fatal("single chunk mismatch")
	}
}

func TestRoundtripEmpty(t *testing.T) {
	if got := roundtrip(t, []byte{}, 1024); len(got) != 0 {
		t.Fatalf("empty debería devolver vacío, got %d", len(got))
	}
}

func TestChunkOverCapRejected(t *testing.T) {
	send, recv := pair(t)
	defer send.Close()
	defer recv.Close()
	if _, err := SendKV(send, 0, []byte("x"), 99*1024*1024); err == nil {
		t.Fatal("chunk > cap debe fallar")
	}
}
