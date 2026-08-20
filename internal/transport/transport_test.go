package transport

import (
	"bytes"
	"net"
	"testing"

	"github.com/Custok/sofmat/internal/framing"
)

func pair(t *testing.T, srvToken, cliToken []byte) (*TcpTransport, *TcpTransport, error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	type res struct {
		tx  *TcpTransport
		err error
	}
	ch := make(chan res, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			ch <- res{nil, err}
			return
		}
		tx, err := Accept(conn, srvToken, 8)
		ch <- res{tx, err}
	}()
	cli, cerr := Connect("127.0.0.1", port, cliToken, 8)
	srv := <-ch
	if cerr != nil {
		return nil, nil, cerr
	}
	if srv.err != nil {
		return nil, nil, srv.err
	}
	return cli, srv.tx, nil
}

func TestActivationRoundtripOverSocket(t *testing.T) {
	token := []byte("token-loopback")
	cli, srv, err := pair(t, token, token)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	defer cli.Close()
	defer srv.Close()

	h := framing.ActivationHeader{StageID: 5, TokenIndex: 99, DType: framing.Int8, Shape: []uint32{6}}
	payload := []byte{10, 20, 30, 40, 50, 60}
	done := make(chan struct{})
	var got framing.ActivationHeader
	var gp []byte
	var rerr error
	go func() {
		got, gp, rerr = srv.RecvActivation()
		close(done)
	}()
	if err := cli.SendActivation(h, payload); err != nil {
		t.Fatalf("send: %v", err)
	}
	<-done
	if rerr != nil {
		t.Fatalf("recv: %v", rerr)
	}
	if got.StageID != 5 || got.TokenIndex != 99 || !bytes.Equal(gp, payload) {
		t.Fatalf("mismatch: %+v %v", got, gp)
	}
}

func TestWrongTokenRejected(t *testing.T) {
	_, _, err := pair(t, []byte("token-bueno"), []byte("token-malo"))
	if err == nil {
		t.Fatal("un token distinto debe romper el handshake")
	}
}
