package feishu

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// 最小的 WebSocket 客户端（RFC 6455），只实现长连接用到的部分：
// 握手、二进制消息、分片重组、ping/pong、close。不支持扩展（如 permessage-deflate）。
//
// 握手借用 net/http：服务端返回 101 后，响应的 Body 就是可读写的底层连接，
// 因此 TLS 和系统代理设置都与普通 HTTP 请求一致。

const (
	wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA

	maxMessageSize = 16 << 20 // 单条消息上限，防止异常数据耗尽内存
)

// errWSClosed 表示连接已关闭（对方发送了 close 帧或本地已关闭）。
var errWSClosed = errors.New("websocket 连接已关闭")

// handshakeError 是握手被拒绝的错误，带上服务端返回的状态和说明。
type handshakeError struct {
	Status int
	Header http.Header
	Body   string
}

func (e *handshakeError) Error() string {
	msg := fmt.Sprintf("websocket 握手失败: HTTP %d", e.Status)
	if s := e.Header.Get("Handshake-Status"); s != "" {
		msg += " handshake-status=" + s
	}
	if s := e.Header.Get("Handshake-Msg"); s != "" {
		msg += " handshake-msg=" + s
	}
	if e.Body != "" {
		msg += " " + e.Body
	}
	return msg
}

type wsConn struct {
	rw io.ReadWriteCloser

	wmu sync.Mutex // 写帧互斥：心跳和事件响应可能并发写

	closeOnce sync.Once
}

// newWSTransport 返回只走 HTTP/1.1 的 Transport：WebSocket 握手不能走 HTTP/2。
func newWSTransport() *http.Transport {
	return &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		ForceAttemptHTTP2: false,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
}

// dialWS 建立 WebSocket 连接。rawURL 为 ws:// 或 wss:// 地址。
func dialWS(ctx context.Context, rt http.RoundTripper, rawURL string) (*wsConn, error) {
	httpURL := rawURL
	switch {
	case strings.HasPrefix(rawURL, "wss://"):
		httpURL = "https://" + strings.TrimPrefix(rawURL, "wss://")
	case strings.HasPrefix(rawURL, "ws://"):
		httpURL = "http://" + strings.TrimPrefix(rawURL, "ws://")
	default:
		return nil, fmt.Errorf("不支持的 websocket 地址: %s", rawURL)
	}

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", key)

	resp, err := rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, &handshakeError{Status: resp.StatusCode, Header: resp.Header, Body: strings.TrimSpace(string(body))}
	}
	rw, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return nil, errors.New("websocket 握手成功，但无法取得底层连接")
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") || resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) {
		rw.Close()
		return nil, errors.New("websocket 握手响应不合法")
	}
	return &wsConn{rw: rw}, nil
}

func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// ReadMessage 读取一条完整消息（自动重组分片），并在内部处理 ping 和 close。
func (c *wsConn) ReadMessage() (opcode byte, data []byte, err error) {
	var msgOp byte
	var buf []byte
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case opPing:
			if err := c.writeFrame(opPong, payload); err != nil {
				return 0, nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			_ = c.writeFrame(opClose, closePayload(payload))
			c.Close()
			return 0, nil, fmt.Errorf("%w%s", errWSClosed, describeClose(payload))
		case opText, opBinary:
			if msgOp != 0 {
				return 0, nil, errors.New("websocket 协议错误：上一条分片消息未结束")
			}
			msgOp = op
		case opContinuation:
			if msgOp == 0 {
				return 0, nil, errors.New("websocket 协议错误：意外的续帧")
			}
		default:
			return 0, nil, fmt.Errorf("websocket 协议错误：未知帧类型 %#x", op)
		}
		if len(buf)+len(payload) > maxMessageSize {
			return 0, nil, errors.New("websocket 消息超过大小上限")
		}
		buf = append(buf, payload...)
		if fin {
			return msgOp, buf, nil
		}
	}
}

func (c *wsConn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.rw, h[:]); err != nil {
		return
	}
	fin = h[0]&0x80 != 0
	if h[0]&0x70 != 0 {
		err = errors.New("websocket 协议错误：未协商扩展却设置了 RSV 位")
		return
	}
	op = h[0] & 0x0F
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.rw, ext[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.rw, ext[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	if op >= opClose && (n > 125 || !fin) {
		err = errors.New("websocket 协议错误：控制帧过长或被分片")
		return
	}
	if n > maxMessageSize {
		err = errors.New("websocket 帧超过大小上限")
		return
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.rw, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.rw, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

// WriteBinary 发送一条二进制消息。
func (c *wsConn) WriteBinary(data []byte) error {
	return c.writeFrame(opBinary, data)
}

// writeFrame 发送一个不分片的帧。客户端发出的帧必须加掩码。
func (c *wsConn) writeFrame(op byte, payload []byte) error {
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	b := make([]byte, 0, 14+len(payload))
	b = append(b, 0x80|op)
	n := len(payload)
	switch {
	case n <= 125:
		b = append(b, 0x80|byte(n))
	case n <= 0xFFFF:
		b = append(b, 0x80|126)
		b = binary.BigEndian.AppendUint16(b, uint16(n))
	default:
		b = append(b, 0x80|127)
		b = binary.BigEndian.AppendUint64(b, uint64(n))
	}
	b = append(b, mask[:]...)
	start := len(b)
	b = append(b, payload...)
	for i := range payload {
		b[start+i] ^= mask[i%4]
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.rw.Write(b)
	return err
}

// Close 关闭底层连接。可重复调用。
func (c *wsConn) Close() error {
	var err error
	c.closeOnce.Do(func() { err = c.rw.Close() })
	return err
}

// closePayload 回应 close 帧时原样带回状态码（没有状态码时不带）。
func closePayload(p []byte) []byte {
	if len(p) >= 2 {
		return p[:2]
	}
	return nil
}

func describeClose(p []byte) string {
	if len(p) < 2 {
		return ""
	}
	code := binary.BigEndian.Uint16(p[:2])
	if reason := strings.TrimSpace(string(p[2:])); reason != "" {
		return fmt.Sprintf("（状态码 %d：%s）", code, reason)
	}
	return fmt.Sprintf("（状态码 %d）", code)
}
