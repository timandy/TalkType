// Package wecom 是企业微信消息来源：扫码创建“智能机器人”（或使用手动填写的 bot_id / secret），
// 通过长连接接收机器人单聊里的文本消息并键入，可选回一条回执。
//
// 不依赖企业微信 SDK。协议取自官方 SDK @wecom/aibot-node-sdk：WebSocket 上收发 JSON 文本帧，
// 统一格式 {cmd, headers: {req_id}, body}；认证、心跳、回执的响应没有 cmd，只带 errcode / errmsg。
package wecom

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"wetype/internal/config"
	"wetype/internal/source"
	"wetype/internal/store"
	"wetype/internal/ws"
)

const (
	cmdSubscribe = "aibot_subscribe"
	cmdPing      = "ping"
	cmdCallback  = "aibot_msg_callback"
	cmdEvent     = "aibot_event_callback"
	cmdRespond   = "aibot_respond_msg"

	defaultHeartbeat = 30 * time.Second
	maxMissedPong    = 2 // 连续这么多次心跳没有回应，判定连接已死（与官方 SDK 一致）
	authTimeout      = 15 * time.Second

	// errcodeInvalidCredentials 是认证响应里 "invalid bot_id or secret" 的错误码（实测），收到即判定凭证无效。
	errcodeInvalidCredentials = 853000

	defaultQCBase = "https://work.weixin.qq.com"
	defaultWSURL  = "wss://openws.work.weixin.qq.com"
)

var (
	errAuthFailed = errors.New("企业微信机器人认证失败")
	// errInvalidCredentials 表示服务端明确返回凭证无效，不必再重试。
	errInvalidCredentials = fmt.Errorf("%w：bot_id 或 secret 无效", errAuthFailed)
	// errReplaced 表示同一个机器人在别处建立了新的长连接，服务端断开了本连接。
	errReplaced = errors.New("该机器人已在别处建立长连接")
)

// Client 是企业微信来源，实现 source.Source。
type Client struct {
	cfg        *config.Config
	store      *store.WeComStore
	acked      *store.AckedIDs
	logger     *log.Logger
	httpClient *http.Client
	wsRT       http.RoundTripper

	// 测试时替换。
	qcBase    string
	wsURL     string
	heartbeat time.Duration
	sleep     func(ctx context.Context, d time.Duration) error

	handle source.Handler

	// startedAt 是客户端创建的时间。此前创建的消息是启动前积压的历史消息，只确认、不键入。
	startedAt time.Time

	botID, secret string
	// pairCode 是未配对时等待用户发来的配对码；已配对时为空。
	pairCode string
}

// NewClient 创建企业微信来源。acked 记录已处理的消息 ID，用于去重。
func NewClient(cfg *config.Config, st *store.WeComStore, acked *store.AckedIDs, logger *log.Logger) *Client {
	if logger == nil {
		logger = log.Default()
	}
	return &Client{
		cfg:        cfg,
		store:      st,
		acked:      acked,
		logger:     logger,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		wsRT:       ws.NewTransport(),
		qcBase:     defaultQCBase,
		wsURL:      defaultWSURL,
		heartbeat:  defaultHeartbeat,
		sleep:      sleepCtx,
		startedAt:  time.Now(),
	}
}

var _ source.Source = (*Client)(nil)

// Name 实现 source.Source。
func (c *Client) Name() string { return "企业微信" }

// Check 实现 source.Authorizer：凭证缺失或不完整（没有 bot_id 或 secret），或认证时服务端明确返回凭证无效时返回
// ErrAuthRequired，其余情况（包括网络错误、超时）视为暂时无法判断，返回 nil。
func (c *Client) Check(ctx context.Context) error {
	if id, secret := c.store.Credentials(); id == "" || secret == "" {
		// 只剩一半凭证没有用，清掉，让 Authorize 重新扫码。
		if id != "" || secret != "" {
			c.logger.Println("凭证文件里的 bot_id 或 secret 缺失，视为未授权，清除后重新扫码")
			if err := c.store.Delete(); err != nil {
				c.logger.Printf("删除凭证文件失败: %v", err)
			}
		}
		return source.ErrAuthRequired
	}
	if err := c.loadCredentials(ctx, false); err != nil {
		return nil
	}
	if !c.credentialsRejected(ctx) {
		return nil
	}
	c.logger.Printf("%v，清除本地凭证", errInvalidCredentials)
	if err := c.store.Delete(); err != nil {
		c.logger.Printf("删除凭证文件失败: %v", err)
	}
	c.botID, c.secret = "", ""
	return source.ErrAuthRequired
}

// credentialsRejected 连一次长连接并认证，只在服务端明确返回凭证无效时返回 true，随后立即断开。
func (c *Client) credentialsRejected(ctx context.Context) bool {
	probeCtx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()
	conn, err := ws.Dial(probeCtx, c.wsRT, c.wsURL)
	if err != nil {
		return false
	}
	defer conn.Close()
	go func() {
		<-probeCtx.Done()
		conn.Close()
	}()
	if err := send(conn, cmdSubscribe, newReqID(cmdSubscribe), map[string]string{"bot_id": c.botID, "secret": c.secret}); err != nil {
		return false
	}
	for {
		op, data, err := conn.ReadMessage()
		if err != nil {
			return false
		}
		if op != ws.OpText && op != ws.OpBinary {
			continue
		}
		var f frame
		if json.Unmarshal(data, &f) != nil || f.Cmd != "" || !strings.HasPrefix(f.Headers.ReqID, cmdSubscribe) {
			continue
		}
		return f.ErrCode == errcodeInvalidCredentials
	}
}

// Authorize 实现 source.Authorizer：凭证文件中没有凭证时扫码创建智能机器人，失败则退避重试。
// 配对码在连上之后由用户发送，不在这里等待。
func (c *Client) Authorize(ctx context.Context) error {
	if c.botID != "" {
		return nil
	}
	return c.loadWithRetry(ctx, false)
}

// Reauthorize 实现 source.Authorizer：不管现有凭证，直接扫码创建智能机器人，并要求重新配对。
// 扫码成功才覆盖旧凭证，失败、超时或被跳过时旧凭证和配对记录原样保留。
func (c *Client) Reauthorize(ctx context.Context) error {
	return c.loadWithRetry(ctx, true)
}

// loadWithRetry 取得凭证（force 为真时一定扫码），失败则退避重试，直到成功或 ctx 结束。
func (c *Client) loadWithRetry(ctx context.Context, force bool) error {
	minDelay := time.Duration(c.cfg.Reconnect.MinDelayMS) * time.Millisecond
	maxDelay := time.Duration(c.cfg.Reconnect.MaxDelayMS) * time.Millisecond
	backoff := minDelay
	for ctx.Err() == nil {
		err := c.loadCredentials(ctx, force)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			break
		}
		c.logger.Printf("登录失败: %v，%s 后重试", err, backoff)
		_ = c.sleep(ctx, backoff+jitter(backoff))
		backoff = nextBackoff(backoff, maxDelay)
	}
	return ctx.Err()
}

// Run 实现 source.Source：准备好凭证后保持长连接，断开时按退避策略重连。
func (c *Client) Run(ctx context.Context, handle source.Handler) error {
	c.handle = handle
	minDelay := time.Duration(c.cfg.Reconnect.MinDelayMS) * time.Millisecond
	maxDelay := time.Duration(c.cfg.Reconnect.MaxDelayMS) * time.Millisecond
	backoff := minDelay
	for {
		if ctx.Err() != nil {
			return nil
		}
		if c.botID == "" {
			if err := c.loadCredentials(ctx, false); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				c.logger.Printf("登录失败: %v，%s 后重试", err, backoff)
				_ = c.sleep(ctx, backoff+jitter(backoff))
				backoff = nextBackoff(backoff, maxDelay)
				continue
			}
			backoff = minDelay
		}

		err := c.session(ctx, func() { backoff = minDelay })
		if ctx.Err() != nil {
			return nil
		}
		switch {
		case errors.Is(err, errReplaced):
			return fmt.Errorf("%w，本程序停止运行，避免两边互相挤下线", err)
		case errors.Is(err, errInvalidCredentials):
			// 只有服务端明确说凭证无效才清除并重新扫码；其他认证失败（含超时、未知错误码）只退避重连。
			c.logger.Printf("%v，清除本地凭证并重新扫码", err)
			if err := c.store.Delete(); err != nil {
				c.logger.Printf("删除凭证文件失败: %v", err)
			}
			c.botID, backoff = "", minDelay
			continue
		}
		c.logger.Printf("企业微信长连接中断: %v，%s 后重连", err, backoff)
		_ = c.sleep(ctx, backoff+jitter(backoff))
		backoff = nextBackoff(backoff, maxDelay)
	}
}

// loadCredentials 使用凭证文件中保存的凭证，没有时（或 force 为真时）扫码创建，扫码成功才覆盖旧凭证。凭证不支持手动填写。
func (c *Client) loadCredentials(ctx context.Context, force bool) error {
	if id, secret := c.store.Credentials(); !force && id != "" && secret != "" {
		c.botID, c.secret = id, secret
	} else {
		id, secret, err := c.scan(ctx)
		if err != nil {
			return err
		}
		c.store.SetCredentials(id, secret)
		if force {
			c.store.SetOwner("") // 重新扫码后必须重新配对，即使扫到的还是同一个机器人
		}
		if err := c.store.Save(); err != nil {
			return fmt.Errorf("保存企业微信凭证失败: %w", err)
		}
		c.botID, c.secret = id, secret
		c.logger.Printf("扫码成功，已接入智能机器人 bot_id=%s，凭证已保存到 %s", id, c.store.Path())
	}
	if owner := c.store.Owner(); owner != "" {
		c.logger.Printf("已配对，只接受 userid=%s 的消息", owner)
		return nil
	}
	c.pairCode = newPairCode()
	return nil
}

// newPairCode 生成 6 位数字配对码。
func newPairCode() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	n := (uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])) % 1000000
	return fmt.Sprintf("%06d", n)
}

// logPairing 提示用户发送配对码。每次连上都提示，避免日志被刷走后找不到。
func (c *Client) logPairing() {
	if c.pairCode != "" {
		c.logger.Printf("尚未配对：请在企业微信里给该机器人发送配对码 %s。配对后只接受你本人的消息，配对前不键入任何消息", c.pairCode)
	}
}

// ---------------------------------------------------------------------------
// 长连接
// ---------------------------------------------------------------------------

// frame 是长连接上收发的 JSON 帧。
type frame struct {
	Cmd     string `json:"cmd,omitempty"`
	Headers struct {
		ReqID string `json:"req_id"`
	} `json:"headers"`
	Body    json.RawMessage `json:"body,omitempty"`
	ErrCode int             `json:"errcode"`
	ErrMsg  string          `json:"errmsg"`
}

type outFrame struct {
	Cmd     string            `json:"cmd"`
	Headers map[string]string `json:"headers"`
	Body    any               `json:"body,omitempty"`
}

func send(conn *ws.Conn, cmd, reqID string, body any) error {
	data, err := json.Marshal(outFrame{Cmd: cmd, Headers: map[string]string{"req_id": reqID}, Body: body})
	if err != nil {
		return err
	}
	return conn.WriteText(data)
}

// newReqID 生成 req_id，格式与官方 SDK 一致：{prefix}_{毫秒时间戳}_{随机串}。
// 服务端的响应只带 req_id，靠前缀区分是认证、心跳还是回执的响应。
func newReqID(prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + strconv.FormatInt(time.Now().UnixMilli(), 10) + "_" + hex.EncodeToString(b[:])
}

// session 建立一次长连接并持续读取，直到连接断开或 ctx 结束。
func (c *Client) session(ctx context.Context, onHealthy func()) error {
	conn, err := ws.Dial(ctx, c.wsRT, c.wsURL)
	if err != nil {
		return err
	}
	sessCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer conn.Close()
	go func() {
		<-sessCtx.Done()
		conn.Close()
	}()

	if err := send(conn, cmdSubscribe, newReqID(cmdSubscribe), map[string]string{"bot_id": c.botID, "secret": c.secret}); err != nil {
		return err
	}
	var authed, authTimedOut, dead atomic.Bool
	authTimer := time.AfterFunc(authTimeout, func() {
		if !authed.Load() {
			authTimedOut.Store(true)
			conn.Close()
		}
	})
	defer authTimer.Stop()
	var missed atomic.Int32

	for {
		op, data, err := conn.ReadMessage()
		if err != nil {
			switch {
			case authTimedOut.Load():
				return errors.New("等待认证响应超时")
			case dead.Load():
				return errors.New("长时间没有收到企业微信的心跳回应")
			}
			return err
		}
		if op != ws.OpText && op != ws.OpBinary {
			continue
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			c.logger.Printf("解析企业微信帧失败: %v", err)
			continue
		}
		switch f.Cmd {
		case cmdCallback:
			c.handleMessage(ctx, conn, &f)
			continue
		case cmdEvent:
			if eventType(f.Body) == "disconnected_event" {
				return errReplaced
			}
			continue
		case "":
		default:
			continue
		}
		// 没有 cmd 的是响应帧，按 req_id 前缀区分。
		switch id := f.Headers.ReqID; {
		case strings.HasPrefix(id, cmdSubscribe):
			if f.ErrCode == errcodeInvalidCredentials {
				return fmt.Errorf("%w (errcode=%d)", errInvalidCredentials, f.ErrCode)
			}
			if f.ErrCode != 0 {
				return fmt.Errorf("%w: errcode=%d %s", errAuthFailed, f.ErrCode, f.ErrMsg)
			}
			if !authed.Swap(true) {
				authTimer.Stop()
				c.logger.Println("企业微信长连接已建立，等待消息（只处理机器人单聊）")
				c.logPairing()
				onHealthy()
				go c.heartbeatLoop(sessCtx, conn, &missed, &dead)
			}
		case strings.HasPrefix(id, cmdPing):
			if f.ErrCode == 0 {
				missed.Store(0)
			}
		default: // 回执的响应：req_id 沿用消息回调的 req_id
			if f.ErrCode != 0 {
				c.logger.Printf("发送回执失败: errcode=%d %s", f.ErrCode, f.ErrMsg)
			}
		}
	}
}

// heartbeatLoop 定时发心跳；连续 maxMissedPong 次没有回应时关闭连接触发重连（dead 记录原因）。
func (c *Client) heartbeatLoop(ctx context.Context, conn *ws.Conn, missed *atomic.Int32, dead *atomic.Bool) {
	for {
		if err := sleepCtx(ctx, c.heartbeat); err != nil {
			return
		}
		if missed.Load() >= maxMissedPong {
			dead.Store(true)
			conn.Close()
			return
		}
		missed.Add(1)
		if err := send(conn, cmdPing, newReqID(cmdPing), nil); err != nil {
			conn.Close()
			return
		}
	}
}

func eventType(body json.RawMessage) string {
	var b struct {
		Event struct {
			EventType string `json:"eventtype"`
		} `json:"event"`
	}
	_ = json.Unmarshal(body, &b)
	return b.Event.EventType
}

// ---------------------------------------------------------------------------
// 消息
// ---------------------------------------------------------------------------

type messageBody struct {
	MsgID    string `json:"msgid"`
	ChatType string `json:"chattype"` // single 单聊，group 群聊
	From     struct {
		UserID string `json:"userid"`
	} `json:"from"`
	// CreateTime 是消息创建时间（秒或毫秒时间戳）。官方文档未列出该字段，服务端不给时为 0，不做过期判断。
	CreateTime int64  `json:"create_time"`
	MsgType    string `json:"msgtype"`
	Text       struct {
		Content string `json:"content"`
	} `json:"text"`
}

// msgTypeNames 是非文字消息类型的中文名称，用于控制台提示。
var msgTypeNames = map[string]string{
	"image": "图片",
	"mixed": "图文混排",
	"voice": "语音",
	"file":  "文件",
	"video": "视频",
}

// handleMessage 处理一条消息回调。企业微信不需要逐条确认，也不会因为键入失败而重推。
func (c *Client) handleMessage(ctx context.Context, conn *ws.Conn, f *frame) {
	var m messageBody
	if err := json.Unmarshal(f.Body, &m); err != nil {
		c.logger.Printf("解析企业微信消息失败: %v", err)
		return
	}
	id, from := m.MsgID, m.From.UserID
	if id != "" && c.acked.Has(id) {
		return
	}
	if source.IsStale(m.CreateTime, c.startedAt) {
		c.logger.Printf("忽略启动前积压的历史消息 id=%s（不键入）", id)
		c.markAcked(id)
		return
	}
	if m.ChatType != "single" {
		c.logger.Printf("忽略群聊消息 id=%s：只支持机器人单聊", id)
		c.markAcked(id)
		return
	}
	if c.pairCode != "" {
		c.tryPair(conn, f, &m)
		c.markAcked(id)
		return
	}
	if !c.senderAllowed(from) {
		c.logger.Printf("忽略非本人的消息 from=%s", from)
		c.markAcked(id)
		return
	}
	if m.MsgType != "text" {
		name, ok := msgTypeNames[m.MsgType]
		if !ok {
			name = "未知类型 " + m.MsgType + " "
		}
		c.logger.Printf("忽略%s消息 from=%s id=%s：暂只支持文字消息", name, from, id)
		c.markAcked(id)
		return
	}
	text := strings.TrimSpace(m.Text.Content)
	if text == "" {
		c.logger.Printf("忽略空白文字消息 from=%s id=%s", from, id)
		c.markAcked(id)
		return
	}

	c.logger.Printf("收到文本消息 from=%s id=%s: %s", from, id, truncate(text, 100))
	err := c.handle(ctx, source.Message{From: from, Text: text})
	if errors.Is(err, source.ErrDiscarded) {
		c.logger.Printf("消息 id=%s 未发送: %v", id, err)
		c.markAcked(id)
		return
	}
	if err != nil {
		c.logger.Printf("文字输入失败: %v", err)
		return
	}
	c.markAcked(id)

	if reply := strings.TrimSpace(c.cfg.Ack.ReplyText); reply != "" {
		c.reply(conn, f, reply)
	}
	c.logger.Println("消息已投递")
}

// tryPair 处理未配对时收到的单聊消息：内容是配对码就把发送者记为本人，否则忽略。消息本身不键入。
func (c *Client) tryPair(conn *ws.Conn, f *frame, m *messageBody) {
	from := m.From.UserID
	if m.MsgType != "text" || strings.TrimSpace(m.Text.Content) != c.pairCode || from == "" {
		c.logger.Printf("尚未配对，忽略来自 %s 的消息", from)
		c.logPairing()
		return
	}
	c.store.SetOwner(from)
	if err := c.store.Save(); err != nil {
		c.logger.Printf("保存配对记录失败: %v", err)
		return // 不清除配对码，用户可以再发一次
	}
	c.pairCode = ""
	c.logger.Printf("配对成功：之后只接受 userid=%s 的消息", from)
	c.reply(conn, f, "配对成功，之后只接受你的消息")
}

// reply 用回调的 req_id 回一条文本（流式消息一次结束）。
func (c *Client) reply(conn *ws.Conn, f *frame, text string) {
	if conn == nil {
		return
	}
	body := map[string]any{"msgtype": "stream", "stream": map[string]any{"id": newReqID("stream"), "finish": true, "content": text}}
	if err := send(conn, cmdRespond, f.Headers.ReqID, body); err != nil {
		c.logger.Printf("发送回执失败: %v", err)
	}
}

func (c *Client) markAcked(id string) {
	if err := c.acked.Add(id); err != nil {
		c.logger.Printf("保存已处理消息记录失败: %v", err)
	}
}

// senderAllowed 只接受配对的本人。
func (c *Client) senderAllowed(userID string) bool {
	return userID != "" && userID == c.store.Owner()
}
