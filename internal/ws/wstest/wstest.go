// Package wstest 提供测试用的 WebSocket 服务端：完成握手，读取客户端（带掩码）的帧，发送不带掩码的帧。
// 只用于测试，不在正式代码里引用。
package wstest

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/timandy/talktype/internal/ws"
)

// ServerConn 是服务端这一侧的连接。
type ServerConn struct {
	Conn net.Conn
	r    *bufio.Reader
}

// Accept 在 HTTP handler 里完成 WebSocket 握手。
func Accept(t *testing.T, w http.ResponseWriter, r *http.Request) *ServerConn {
	t.Helper()
	if r.Header.Get("Upgrade") != "websocket" || r.Header.Get("Sec-WebSocket-Version") != "13" {
		t.Errorf("bad upgrade request: %v", r.Header)
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Accept: "+ws.AcceptKey(key)+"\r\n\r\n")
	return &ServerConn{Conn: conn, r: rw.Reader}
}

// Close 关闭连接。
func (s *ServerConn) Close() error { return s.Conn.Close() }

// Write 发送一个帧。fin 为 false 时是分片的一部分。
func (s *ServerConn) Write(fin bool, op byte, payload []byte) error {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	b := []byte{b0}
	switch n := len(payload); {
	case n <= 125:
		b = append(b, byte(n))
	case n <= 0xFFFF:
		b = append(b, 126)
		b = binary.BigEndian.AppendUint16(b, uint16(n))
	default:
		b = append(b, 127)
		b = binary.BigEndian.AppendUint64(b, uint64(n))
	}
	_, err := s.Conn.Write(append(b, payload...))
	return err
}

// Read 读取客户端发来的一个帧，并去掉掩码。客户端帧没有掩码时报错。
func (s *ServerConn) Read() (op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(s.r, h[:]); err != nil {
		return
	}
	op = h[0] & 0x0F
	if h[1]&0x80 == 0 {
		return 0, nil, errors.New("client frame not masked")
	}
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var e [2]byte
		if _, err = io.ReadFull(s.r, e[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(e[:]))
	case 127:
		var e [8]byte
		if _, err = io.ReadFull(s.r, e[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(e[:])
	}
	var mask [4]byte
	if _, err = io.ReadFull(s.r, mask[:]); err != nil {
		return
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(s.r, payload); err != nil {
		return
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return
}

// URL 把 httptest 服务器地址换成 ws://。
func URL(srv *httptest.Server) string {
	return "ws://" + strings.TrimPrefix(srv.URL, "http://")
}
