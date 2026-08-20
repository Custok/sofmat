// Package transport — canal de activación inter-host (interfaz + backend TCP v0).
// Port de `transport/transport.py`. El coordinador y los workers mueven las
// activaciones del pipeline por esta interfaz y nada más:
//
//	SendActivation(h, payload)   // empuja la salida de mi etapa aguas abajo
//	h, payload, err := RecvActivation()
//
// Seguridad: A01 el handshake auth corre SIEMPRE al conectar (Connect/Accept);
// A08 los frames son el binario de `framing` (sin deserialización dinámica);
// A04/DoS el prefijo de longitud está acotado por maxFrameBytes.
package transport

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/Custok/sofmat/internal/auth"
	"github.com/Custok/sofmat/internal/framing"
)

// ErrTransport: fallo de conexión/protocolo. La frontera de etapa está rota.
var ErrTransport = errors.New("transport: fallo de conexión/protocolo")

const headerSlack = 512

type TcpTransport struct {
	conn          net.Conn
	maxFrameBytes int
	maxPayload    int
}

// New envuelve una conexión ya establecida. Usa Connect/Accept (corren el
// handshake); New directo es para tests o backends alternativos.
func New(conn net.Conn, maxActivationMB int) *TcpTransport {
	if maxActivationMB <= 0 {
		maxActivationMB = 8
	}
	mp := maxActivationMB * 1024 * 1024
	return &TcpTransport{conn: conn, maxFrameBytes: mp + headerSlack, maxPayload: mp}
}

// MaxPayload expone el cap de payload (para el troceo de kv_handoff).
func (t *TcpTransport) MaxPayload() int { return t.maxPayload }

func (t *TcpTransport) SendActivation(h framing.ActivationHeader, payload []byte) error {
	frame, err := framing.EncodeActivation(h, payload)
	if err != nil {
		return err
	}
	return t.sendFrame(frame)
}

func (t *TcpTransport) RecvActivation() (framing.ActivationHeader, []byte, error) {
	frame, err := t.recvFrame()
	if err != nil {
		return framing.ActivationHeader{}, nil, err
	}
	return framing.DecodeActivation(frame, t.maxPayload)
}

func (t *TcpTransport) Close() error { return t.conn.Close() }

// -- IO framed (prefijo de longitud big-endian, acotado) --

func (t *TcpTransport) sendFrame(frame []byte) error {
	buf := make([]byte, 4+len(frame))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(frame)))
	copy(buf[4:], frame)
	if _, err := t.conn.Write(buf); err != nil {
		return fmt.Errorf("%w: send: %v", ErrTransport, err)
	}
	return nil
}

func (t *TcpTransport) recvFrame() ([]byte, error) {
	var lp [4]byte
	if _, err := io.ReadFull(t.conn, lp[:]); err != nil {
		return nil, fmt.Errorf("%w: recv len: %v", ErrTransport, err)
	}
	n := int(binary.BigEndian.Uint32(lp[:]))
	if n > t.maxFrameBytes {
		return nil, fmt.Errorf("%w: frame %dB excede cap %dB — abortando", ErrTransport, n, t.maxFrameBytes)
	}
	frame := make([]byte, n)
	if _, err := io.ReadFull(t.conn, frame); err != nil {
		return nil, fmt.Errorf("%w: recv frame: %v", ErrTransport, err)
	}
	return frame, nil
}

// -- establecimiento de conexión (el handshake corre aquí, siempre) --

// Connect: lado master — marca a un worker y completa el handshake de cliente.
func Connect(host string, port int, token []byte, maxActivationMB int) (*TcpTransport, error) {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%w: dial: %v", ErrTransport, err)
	}
	setNoDelay(conn)
	t := New(conn, maxActivationMB)
	if err := t.clientHandshake(token); err != nil {
		t.Close()
		return nil, err
	}
	return t, nil
}

// Accept: lado worker — envuelve un socket aceptado y completa el handshake de
// servidor. Si el par no prueba el token, cierra y devuelve error (el puerto
// nunca entrega canal a un llamante no autenticado).
func Accept(conn net.Conn, token []byte, maxActivationMB int) (*TcpTransport, error) {
	setNoDelay(conn)
	t := New(conn, maxActivationMB)
	if err := t.serverHandshake(token); err != nil {
		t.Close()
		return nil, err
	}
	return t, nil
}

func (t *TcpTransport) serverHandshake(token []byte) error {
	nonce, err := auth.NewNonce()
	if err != nil {
		return err
	}
	ch, _ := framing.FrameControl(framing.MsgAuthChallenge, nonce)
	if err := t.sendFrame(ch); err != nil {
		return err
	}
	raw, err := t.recvFrame()
	if err != nil {
		return err
	}
	mt, body, err := framing.DecodeControl(raw)
	if err != nil {
		return err
	}
	if mt != framing.MsgAuthResponse || !auth.Verify(token, nonce, body) {
		errFrame, _ := framing.FrameControl(framing.MsgError, []byte("auth"))
		_ = t.sendFrame(errFrame)
		return fmt.Errorf("%w: worker rechaza al par: token inválido", auth.ErrAuth)
	}
	ack, _ := framing.FrameControl(framing.MsgAck, nil)
	return t.sendFrame(ack)
}

func (t *TcpTransport) clientHandshake(token []byte) error {
	raw, err := t.recvFrame()
	if err != nil {
		return err
	}
	mt, nonce, err := framing.DecodeControl(raw)
	if err != nil {
		return err
	}
	if mt != framing.MsgAuthChallenge {
		return fmt.Errorf("%w: esperaba AUTH_CHALLENGE, got %d", auth.ErrAuth, mt)
	}
	sig, err := auth.Sign(token, nonce)
	if err != nil {
		return err
	}
	resp, _ := framing.FrameControl(framing.MsgAuthResponse, sig)
	if err := t.sendFrame(resp); err != nil {
		return err
	}
	raw, err = t.recvFrame()
	if err != nil {
		return err
	}
	mt, _, err = framing.DecodeControl(raw)
	if err != nil {
		return err
	}
	if mt != framing.MsgAck {
		return fmt.Errorf("%w: worker rechaza al master (token inválido)", auth.ErrAuth)
	}
	return nil
}

func setNoDelay(conn net.Conn) {
	// Activaciones pequeñas y críticas en latencia; Nagle las coalescería y
	// metería una frontera de retardo por token.
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
}
