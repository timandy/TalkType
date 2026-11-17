package feishu

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// wsServerConn 是测试用的服务端：完成握手，读取客户端（带掩码）的帧，发送不带掩码的帧。
type wsServerConn struct {
	conn net.Conn
	r    *bufio.Reader
}

func acceptWS(t *testing.T, w http.ResponseWriter, r *http.Request) *wsServerConn {
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
		"Sec-WebSocket-Accept: "+acceptKey(key)+"\r\n\r\n")
	return &wsServerConn{conn: conn, r: rw.Reader}
}

func (s *wsServerConn) write(fin bool, op byte, payload []byte) error {
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
	_, err := s.conn.Write(append(b, payload...))
	return err
}

func (s *wsServerConn) read() (op byte, payload []byte, err error) {
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
		io.ReadFull(s.r, e[:])
		n = uint64(binary.BigEndian.Uint16(e[:]))
	case 127:
		var e [8]byte
		io.ReadFull(s.r, e[:])
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

func wsURL(srv *httptest.Server) string {
	return "ws://" + strings.TrimPrefix(srv.URL, "http://")
}

func TestWSReadWriteAndControlFrames(t *testing.T) {
	big := strings.Repeat("x", 70000) // 走 64 位长度
	done := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := acceptWS(t, w, r)
		defer s.conn.Close()
		done <- func() error {
			// 客户端发来的消息原样核对
			op, p, err := s.read()
			if err != nil || op != opBinary || string(p) != "hello" {
				return errors.New("unexpected client message: " + string(p))
			}
			// ping 应得到相同内容的 pong
			if err := s.write(true, opPing, []byte("pp")); err != nil {
				return err
			}
			op, p, err = s.read()
			if err != nil || op != opPong || string(p) != "pp" {
				return errors.New("expected pong")
			}
			// 分片消息，中间夹一个 ping
			s.write(false, opBinary, []byte("ab"))
			s.write(true, opPing, nil)
			s.write(false, opContinuation, []byte("cd"))
			s.write(true, opContinuation, []byte("ef"))
			if op, _, err = s.read(); err != nil || op != opPong {
				return errors.New("expected pong for interleaved ping")
			}
			s.write(true, opBinary, []byte(big))
			s.write(true, opClose, []byte{0x03, 0xE8, 'b', 'y', 'e'})
			op, p, err = s.read()
			if err != nil || op != opClose || len(p) != 2 {
				return errors.New("expected close echo")
			}
			return nil
		}()
	}))
	defer srv.Close()

	c, err := dialWS(context.Background(), newWSTransport(), wsURL(srv))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.WriteBinary([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	op, data, err := c.ReadMessage()
	if err != nil || op != opBinary || string(data) != "abcdef" {
		t.Fatalf("fragmented message: op=%d data=%q err=%v", op, data, err)
	}
	if _, data, err = c.ReadMessage(); err != nil || string(data) != big {
		t.Fatalf("big message: len=%d err=%v", len(data), err)
	}
	_, _, err = c.ReadMessage()
	if !errors.Is(err, errWSClosed) || !strings.Contains(err.Error(), "1000") {
		t.Fatalf("expected close error with code, got %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not finish")
	}
}

func TestWSHandshakeRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Handshake-Status", "514")
		w.Header().Set("Handshake-Msg", "auth failed")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	_, err := dialWS(context.Background(), newWSTransport(), wsURL(srv))
	var he *handshakeError
	if !errors.As(err, &he) || he.Status != http.StatusForbidden || !strings.Contains(err.Error(), "auth failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestWSBadAcceptKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		defer conn.Close()
		io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: wrong\r\n\r\n")
	}))
	defer srv.Close()
	if _, err := dialWS(context.Background(), newWSTransport(), wsURL(srv)); err == nil {
		t.Fatal("expected error for bad accept key")
	}
}

func TestWSRejectsBadURL(t *testing.T) {
	if _, err := dialWS(context.Background(), newWSTransport(), "http://example.com"); err == nil {
		t.Fatal("expected error")
	}
}
