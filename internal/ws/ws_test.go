package ws_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/timandy/talktype/internal/ws"
	"github.com/timandy/talktype/internal/ws/wstest"
)

func TestReadWriteAndControlFrames(t *testing.T) {
	big := strings.Repeat("x", 70000) // 走 64 位长度
	done := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := wstest.Accept(t, w, r)
		defer s.Close()
		done <- func() error {
			// 客户端发来的二进制和文本消息原样核对
			op, p, err := s.Read()
			if err != nil || op != ws.OpBinary || string(p) != "hello" {
				return errors.New("unexpected binary message: " + string(p))
			}
			op, p, err = s.Read()
			if err != nil || op != ws.OpText || string(p) != `{"cmd":"ping"}` {
				return errors.New("unexpected text message: " + string(p))
			}
			// ping 应得到相同内容的 pong
			if err := s.Write(true, ws.OpPing, []byte("pp")); err != nil {
				return err
			}
			op, p, err = s.Read()
			if err != nil || op != ws.OpPong || string(p) != "pp" {
				return errors.New("expected pong")
			}
			// 分片消息，中间夹一个 ping
			s.Write(false, ws.OpText, []byte("ab"))
			s.Write(true, ws.OpPing, nil)
			s.Write(false, ws.OpContinuation, []byte("cd"))
			s.Write(true, ws.OpContinuation, []byte("ef"))
			if op, _, err = s.Read(); err != nil || op != ws.OpPong {
				return errors.New("expected pong for interleaved ping")
			}
			s.Write(true, ws.OpBinary, []byte(big))
			s.Write(true, ws.OpClose, []byte{0x03, 0xE8, 'b', 'y', 'e'})
			op, p, err = s.Read()
			if err != nil || op != ws.OpClose || len(p) != 2 {
				return errors.New("expected close echo")
			}
			return nil
		}()
	}))
	defer srv.Close()

	c, err := ws.Dial(context.Background(), ws.NewTransport(), wstest.URL(srv))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.WriteBinary([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteText([]byte(`{"cmd":"ping"}`)); err != nil {
		t.Fatal(err)
	}
	op, data, err := c.ReadMessage()
	if err != nil || op != ws.OpText || string(data) != "abcdef" {
		t.Fatalf("fragmented message: op=%d data=%q err=%v", op, data, err)
	}
	if _, data, err = c.ReadMessage(); err != nil || string(data) != big {
		t.Fatalf("big message: len=%d err=%v", len(data), err)
	}
	_, _, err = c.ReadMessage()
	if !errors.Is(err, ws.ErrClosed) || !strings.Contains(err.Error(), "1000") {
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

func TestHandshakeRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Handshake-Status", "514")
		w.Header().Set("Handshake-Msg", "auth failed")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	_, err := ws.Dial(context.Background(), ws.NewTransport(), wstest.URL(srv))
	var he *ws.HandshakeError
	if !errors.As(err, &he) || he.Status != http.StatusForbidden || !strings.Contains(err.Error(), "auth failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestBadAcceptKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		defer conn.Close()
		io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: wrong\r\n\r\n")
	}))
	defer srv.Close()
	if _, err := ws.Dial(context.Background(), ws.NewTransport(), wstest.URL(srv)); err == nil {
		t.Fatal("expected error for bad accept key")
	}
}

func TestRejectsBadURL(t *testing.T) {
	if _, err := ws.Dial(context.Background(), ws.NewTransport(), "http://example.com"); err == nil {
		t.Fatal("expected error")
	}
}

// 握手请求的头名必须按原样发送，且不带 User-Agent、Accept-Encoding：企业微信长连接服务端对 Go 默认的
// 请求（Sec-Websocket-Key、Go-http-client、gzip）返回 404。
func TestHandshakeRequestBytes(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			got <- ""
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		var req strings.Builder
		key := ""
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				break
			}
			req.WriteString(line)
			if strings.HasPrefix(line, "Sec-WebSocket-Key: ") {
				key = strings.TrimSpace(strings.TrimPrefix(line, "Sec-WebSocket-Key: "))
			}
			if line == "\r\n" {
				break
			}
		}
		io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+ws.AcceptKey(key)+"\r\n\r\n")
		got <- req.String()
	}()
	c, err := ws.Dial(context.Background(), ws.NewTransport(), "ws://"+ln.Addr().String()+"/path?a=1")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	req := <-got
	if !strings.HasPrefix(req, "GET /path?a=1 HTTP/1.1\r\n") {
		t.Errorf("request line: %q", req)
	}
	for _, want := range []string{"\r\nSec-WebSocket-Key: ", "\r\nSec-WebSocket-Version: 13\r\n", "\r\nUpgrade: websocket\r\n", "\r\nConnection: Upgrade\r\n"} {
		if !strings.Contains(req, want) {
			t.Errorf("missing %q in request:\n%s", want, req)
		}
	}
	for _, bad := range []string{"User-Agent", "Accept-Encoding", "Sec-Websocket-"} {
		if strings.Contains(req, bad) {
			t.Errorf("unexpected %q in request:\n%s", bad, req)
		}
	}
}
