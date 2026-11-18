// Package ws 是最小的 WebSocket 客户端（RFC 6455），供飞书、企业微信等长连接来源共用。
package ws

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

	OpContinuation = 0x0
	OpText         = 0x1
	OpBinary       = 0x2
	OpClose        = 0x8
	OpPing         = 0x9
	OpPong         = 0xA

	maxMessageSize = 16 << 20 // 单条消息上限，防止异常数据耗尽内存
)

// ErrClosed 表示连接已关闭（对方发送了 close 帧或本地已关闭）。
var ErrClosed = errors.New("websocket 连接已关闭")

// HandshakeError 是握手被拒绝的错误，带上服务端返回的状态和说明。
type HandshakeError struct {
	Status int
	Header http.Header
	Body   string
}

func (e *HandshakeError) Error() string {
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

type Conn struct {
	rw io.ReadWriteCloser

	wmu sync.Mutex // 写帧互斥：心跳和事件响应可能并发写

	closeOnce sync.Once
}

// NewTransport 返回只走 HTTP/1.1 的 Transport：WebSocket 握手不能走 HTTP/2。
func NewTransport() *http.Transport {
	return &http.Transport{
		Proxy:              http.ProxyFromEnvironment,
		ForceAttemptHTTP2:  false,
		DisableCompression: true, // 不自动加 Accept-Encoding: gzip
		TLSNextProto:       map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
}

// Dial 建立 WebSocket 连接。rawURL 为 ws:// 或 wss:// 地址。
func Dial(ctx context.Context, rt http.RoundTripper, rawURL string) (*Conn, error) {
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
	// 头名按 RFC 6455 的写法原样发送（Header.Set 会改成 Sec-Websocket-Key），不带 Go 默认的 User-Agent
	// 和 Accept-Encoding，与官方 SDK（Node ws 库）发出的请求一致。实测企业微信长连接服务端对 Go 的默认请求
	// 返回 404，并会临时屏蔽该 IP 的后续握手。
	req.Header["Connection"] = []string{"Upgrade"}
	req.Header["Upgrade"] = []string{"websocket"}
	req.Header["Sec-WebSocket-Version"] = []string{"13"}
	req.Header["Sec-WebSocket-Key"] = []string{key}
	req.Header["User-Agent"] = []string{""} // 空值表示不发送

	resp, err := rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, &HandshakeError{Status: resp.StatusCode, Header: resp.Header, Body: strings.TrimSpace(string(body))}
	}
	rw, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return nil, errors.New("websocket 握手成功，但无法取得底层连接")
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") || resp.Header.Get("Sec-WebSocket-Accept") != AcceptKey(key) {
		rw.Close()
		return nil, errors.New("websocket 握手响应不合法")
	}
	return &Conn{rw: rw}, nil
}

// AcceptKey 计算握手响应里的 Sec-WebSocket-Accept。
func AcceptKey(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// ReadMessage 读取一条完整消息（自动重组分片），并在内部处理 ping 和 close。
func (c *Conn) ReadMessage() (opcode byte, data []byte, err error) {
	var msgOp byte
	var buf []byte
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case OpPing:
			if err := c.writeFrame(OpPong, payload); err != nil {
				return 0, nil, err
			}
			continue
		case OpPong:
			continue
		case OpClose:
			_ = c.writeFrame(OpClose, closePayload(payload))
			c.Close()
			return 0, nil, fmt.Errorf("%w%s", ErrClosed, describeClose(payload))
		case OpText, OpBinary:
			if msgOp != 0 {
				return 0, nil, errors.New("websocket 协议错误：上一条分片消息未结束")
			}
			msgOp = op
		case OpContinuation:
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

func (c *Conn) readFrame() (fin bool, op byte, payload []byte, err error) {
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
	if op >= OpClose && (n > 125 || !fin) {
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
func (c *Conn) WriteBinary(data []byte) error {
	return c.writeFrame(OpBinary, data)
}

// WriteText 发送一条文本消息（data 须为 UTF-8）。
func (c *Conn) WriteText(data []byte) error {
	return c.writeFrame(OpText, data)
}

// writeFrame 发送一个不分片的帧。客户端发出的帧必须加掩码。
func (c *Conn) writeFrame(op byte, payload []byte) error {
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
func (c *Conn) Close() error {
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
