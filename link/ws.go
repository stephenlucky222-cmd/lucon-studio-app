package main

// A small WebSocket server (RFC 6455), enough for Lucon Studio to send the programme
// to Lucon Link and receive status messages back. Standard library only.

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type wsConn struct {
	c  net.Conn
	br *bufio.Reader
	mu sync.Mutex
}

func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		return nil, errors.New("not a websocket request")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, errors.New("missing key")
	}
	h, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("hijack not supported")
	}
	conn, brw, err := h.Hijack()
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		conn.Close()
		return nil, err
	}
	return &wsConn{c: conn, br: brw.Reader}, nil
}

// ReadMessage returns one complete message (opcode 1 = text, 2 = binary). Pings are answered.
func (ws *wsConn) ReadMessage(maxSize int) (int, []byte, error) {
	var msg []byte
	op := 0
	for {
		var hdr [2]byte
		if _, err := io.ReadFull(ws.br, hdr[:]); err != nil {
			return 0, nil, err
		}
		fin := hdr[0]&0x80 != 0
		opcode := int(hdr[0] & 0x0f)
		masked := hdr[1]&0x80 != 0
		n := uint64(hdr[1] & 0x7f)
		if n == 126 {
			var b [2]byte
			if _, err := io.ReadFull(ws.br, b[:]); err != nil {
				return 0, nil, err
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		} else if n == 127 {
			var b [8]byte
			if _, err := io.ReadFull(ws.br, b[:]); err != nil {
				return 0, nil, err
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		if !masked {
			return 0, nil, errors.New("client frames must be masked")
		}
		if n > uint64(maxSize) || len(msg)+int(n) > maxSize {
			return 0, nil, errors.New("message too large")
		}
		var mask [4]byte
		if _, err := io.ReadFull(ws.br, mask[:]); err != nil {
			return 0, nil, err
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(ws.br, payload); err != nil {
			return 0, nil, err
		}
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
		switch opcode {
		case 8: // close
			ws.writeFrame(8, nil)
			return 8, nil, io.EOF
		case 9: // ping
			ws.writeFrame(10, payload)
			continue
		case 10: // pong
			continue
		case 0: // continuation
			msg = append(msg, payload...)
		default:
			op = opcode
			msg = append(msg[:0], payload...)
		}
		if fin {
			return op, msg, nil
		}
	}
}

func (ws *wsConn) writeFrame(op int, data []byte) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	hdr := []byte{0x80 | byte(op)}
	n := len(data)
	switch {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n < 65536:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, uint64(n))
		hdr = append(append(hdr, 127), b...)
	}
	if _, err := ws.c.Write(hdr); err != nil {
		return err
	}
	_, err := ws.c.Write(data)
	return err
}

func (ws *wsConn) WriteText(s string) error { return ws.writeFrame(1, []byte(s)) }
func (ws *wsConn) Close() error             { return ws.c.Close() }
