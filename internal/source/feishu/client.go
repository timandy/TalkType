// Package feishu 是飞书消息来源：扫码创建机器人（或使用手动填写的凭证），
// 通过长连接（WebSocket）接收机器人单聊里的文本消息，键入成功后确认，并可选回一条回执。
//
// 不依赖飞书 SDK：帧编解码在本包里手写，WebSocket 客户端用 internal/ws。
package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/timandy/talktype/internal/config"
	"github.com/timandy/talktype/internal/source"
	"github.com/timandy/talktype/internal/store"
	"github.com/timandy/talktype/internal/ws"
)

const (
	defaultPingInterval = 120 * time.Second
	livenessGrace       = 60 * time.Second // 超过“心跳间隔 + 该值”没有收到任何帧，判定连接已死
	fragmentTTL         = time.Minute      // 分片超过该时间仍未收齐则丢弃

	eventMessageReceive = "im.message.receive_v1"
)

// 长连接地址接口的错误码（取自官方 SDK）。
const (
	codeOK            = 0
	codeSystemBusy    = 1
	codeAuthFailed    = 514 // 未实测，含义不明确，不当作凭证无效
	codeInternalError = 1000040343
	codeAppIDInvalid  = 1000040346
)

// errBadCredentials 表示 app_id / app_secret 无效。
var errBadCredentials = errors.New("飞书应用凭证无效")

// Client 是飞书消息来源，实现 source.Source。
type Client struct {
	cfg        *config.Config
	store      *store.FeishuStore
	acked      *store.AckedIDs
	logger     *log.Logger
	httpClient *http.Client
	wsRT       http.RoundTripper

	// 测试时替换为本地服务器地址。
	accountsBase func(domain string) string
	openBase     func(domain string) string
	sleep        func(ctx context.Context, d time.Duration) error

	handle source.Handler

	// startedAt 是客户端创建的时间。此前创建的消息是启动前积压的历史消息，只确认、不键入。
	startedAt time.Time

	// 当前使用的凭证。
	appID, appSecret, domain, owner string

	tokenMu     sync.Mutex
	tenantToken string
	tokenExpiry time.Time

	fragments map[string]*partialEvent
}

type partialEvent struct {
	parts   [][]byte
	got     int
	created time.Time
}

// NewClient 创建飞书来源。acked 记录已处理的消息 ID，用于飞书重推时去重。
func NewClient(cfg *config.Config, st *store.FeishuStore, acked *store.AckedIDs, logger *log.Logger) *Client {
	if logger == nil {
		logger = log.Default()
	}
	return &Client{
		cfg:          cfg,
		store:        st,
		acked:        acked,
		logger:       logger,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		wsRT:         ws.NewTransport(),
		accountsBase: defaultAccountsBase,
		openBase:     defaultOpenBase,
		sleep:        sleepCtx,
		fragments:    make(map[string]*partialEvent),
		startedAt:    time.Now(),
	}
}

func defaultAccountsBase(domain string) string {
	if domain == config.LarkDomain {
		return "https://accounts.larksuite.com"
	}
	return "https://accounts.feishu.cn"
}

func defaultOpenBase(domain string) string {
	if domain == config.LarkDomain {
		return "https://open.larksuite.com"
	}
	return "https://open.feishu.cn"
}

var _ source.Source = (*Client)(nil)

// Name 实现 source.Source。
// SourceName 是来源的显示名称。
const SourceName = "飞书"

func (c *Client) Name() string { return SourceName }

// Check 实现 source.Authorizer：凭证缺失或不完整（没有 app_id、secret 或扫码人），或服务端明确拒绝凭证时返回 ErrAuthRequired，
// 其余情况（包括网络错误）视为暂时无法判断，返回 nil。
func (c *Client) Check(ctx context.Context) error {
	id, secret, _, owner := c.store.Credentials()
	if id == "" || secret == "" {
		return source.ErrAuthRequired
	}
	// 没有扫码人时不会键入任何消息，凭证不完整，必须重新扫码。
	if owner == "" {
		c.logger.Println("凭证文件缺少扫码人的 open_id，视为未授权，清除后重新扫码")
		c.store.ClearCredentials()
		if err := c.store.Save(); err != nil {
			c.logger.Printf("保存凭证文件失败: %v", err)
		}
		return source.ErrAuthRequired
	}
	if err := c.loadCredentials(ctx, false); err != nil {
		return nil
	}
	if _, _, err := c.endpoint(ctx); errors.Is(err, errBadCredentials) {
		c.logger.Printf("%v，清除本地凭证", err)
		c.store.ClearCredentials()
		if err := c.store.Save(); err != nil {
			c.logger.Printf("保存凭证文件失败: %v", err)
		}
		c.appID = ""
		return source.ErrAuthRequired
	}
	return nil
}

// Authorize 实现 source.Authorizer：凭证文件中没有凭证时扫码创建机器人，失败则退避重试。
func (c *Client) Authorize(ctx context.Context) error {
	if c.appID != "" {
		return nil
	}
	return c.loadWithRetry(ctx, false)
}

// Reauthorize 实现 source.Authorizer：不管现有凭证，直接扫码创建机器人。扫码成功才覆盖旧凭证，
// 失败、超时或被跳过时旧凭证原样保留。
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
		if err == nil || errors.Is(err, errScanUnavailable) {
			return err
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
		if c.appID == "" {
			if err := c.loadCredentials(ctx, false); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				if errors.Is(err, errScanUnavailable) {
					return err
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
		if errors.Is(err, errBadCredentials) {
			c.logger.Printf("%v，清除本地凭证并重新扫码", err)
			c.store.ClearCredentials()
			if err := c.store.Save(); err != nil {
				c.logger.Printf("保存凭证文件失败: %v", err)
			}
			c.appID = ""
			backoff = minDelay
			continue
		}
		c.logger.Printf("飞书长连接中断: %v，%s 后重连", err, backoff)
		_ = c.sleep(ctx, backoff+jitter(backoff))
		backoff = nextBackoff(backoff, maxDelay)
	}
}

// loadCredentials 使用凭证文件中保存的凭证，没有时（或 force 为真时）扫码创建，扫码成功才覆盖旧凭证。凭证不支持手动填写。
func (c *Client) loadCredentials(ctx context.Context, force bool) error {
	// 换了应用就不能再用旧应用的访问凭证发回执。
	c.tokenMu.Lock()
	c.tenantToken, c.tokenExpiry = "", time.Time{}
	c.tokenMu.Unlock()

	if id, secret, domain, owner := c.store.Credentials(); !force && id != "" && secret != "" {
		if domain == "" {
			domain = config.FeishuDomain
		}
		c.appID, c.appSecret, c.domain, c.owner = id, secret, domain, owner
		if owner == "" {
			c.logger.Println("警告：凭证文件中没有扫码人的 open_id，不会键入任何消息；请用 -relogin 重新扫码")
		}
		return nil
	}
	res, err := c.scan(ctx)
	if err != nil {
		return err
	}
	c.store.SetCredentials(res.AppID, res.AppSecret, res.Domain, res.OpenID)
	if err := c.store.Save(); err != nil {
		return fmt.Errorf("保存飞书凭证失败: %w", err)
	}
	c.appID, c.appSecret, c.domain, c.owner = res.AppID, res.AppSecret, res.Domain, res.OpenID
	c.logger.Printf("扫码成功，已创建机器人 app_id=%s，凭证已保存到 %s", res.AppID, c.store.Path())
	return nil
}

// ---------------------------------------------------------------------------
// 长连接
// ---------------------------------------------------------------------------

type endpointResp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		URL          string `json:"URL"`
		ClientConfig struct {
			PingInterval int `json:"PingInterval"`
		} `json:"ClientConfig"`
	} `json:"data"`
}

// endpoint 获取长连接地址和心跳间隔。
func (c *Client) endpoint(ctx context.Context) (string, time.Duration, error) {
	body, _ := json.Marshal(map[string]string{"AppID": c.appID, "AppSecret": c.appSecret})
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.openBase(c.domain)+"/callback/ws/endpoint", bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("locale", "zh")
	var r endpointResp
	if err := c.doJSON(req, &r); err != nil {
		return "", 0, fmt.Errorf("获取长连接地址失败: %w", err)
	}
	switch r.Code {
	case codeOK:
	case codeAppIDInvalid:
		return "", 0, fmt.Errorf("%w: code=%d %s", errBadCredentials, r.Code, r.Msg)
	default:
		return "", 0, fmt.Errorf("获取长连接地址失败: code=%d %s", r.Code, r.Msg)
	}
	if r.Data.URL == "" {
		return "", 0, errors.New("获取长连接地址失败：响应中没有地址")
	}
	ping := time.Duration(r.Data.ClientConfig.PingInterval) * time.Second
	if ping <= 0 {
		ping = defaultPingInterval
	}
	return r.Data.URL, ping, nil
}

// session 建立一次长连接并持续读取，直到连接断开或 ctx 结束。
func (c *Client) session(ctx context.Context, onHealthy func()) error {
	wsURL, pingInterval, err := c.endpoint(ctx)
	if err != nil {
		return err
	}
	serviceID := int32(0)
	if u, err := url.Parse(wsURL); err == nil {
		if v, err := strconv.ParseInt(u.Query().Get("service_id"), 10, 32); err == nil {
			serviceID = int32(v)
		}
	}

	conn, err := ws.Dial(ctx, c.wsRT, wsURL)
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

	c.logger.Println("飞书长连接已建立，等待消息（只处理机器人单聊）")
	onHealthy()

	var lastRecv atomic.Int64
	lastRecv.Store(time.Now().UnixNano())
	var ping atomic.Int64
	ping.Store(int64(pingInterval))
	var dead atomic.Bool
	go c.pingLoop(sessCtx, conn, serviceID, &ping, &lastRecv, &dead)

	for {
		op, data, err := conn.ReadMessage()
		if err != nil {
			if dead.Load() {
				return errors.New("长时间没有收到飞书的心跳回应")
			}
			return err
		}
		lastRecv.Store(time.Now().UnixNano())
		if op != ws.OpBinary {
			continue
		}
		f, err := decodeFrame(data)
		if err != nil {
			c.logger.Printf("解析飞书帧失败: %v", err)
			continue
		}
		switch f.Method {
		case methodControl:
			c.handleControl(f, &ping)
		case methodData:
			c.handleData(ctx, conn, f)
		}
	}
}

// pingLoop 定时发送心跳，并在长时间收不到任何帧时关闭连接触发重连（dead 记录原因）。
func (c *Client) pingLoop(ctx context.Context, conn *ws.Conn, serviceID int32, ping, lastRecv *atomic.Int64, dead *atomic.Bool) {
	for {
		interval := time.Duration(ping.Load())
		if err := sleepCtx(ctx, interval); err != nil {
			return
		}
		if time.Since(time.Unix(0, lastRecv.Load())) > interval+livenessGrace {
			dead.Store(true)
			conn.Close()
			return
		}
		f := &frame{Service: serviceID, Method: methodControl, Headers: []header{{headerType, typePing}}}
		if err := conn.WriteBinary(f.encode()); err != nil {
			conn.Close()
			return
		}
	}
}

// handleControl 处理 pong：服务端可能在 payload 里下发新的心跳间隔。
func (c *Client) handleControl(f *frame, ping *atomic.Int64) {
	if f.header(headerType) != typePong || len(f.Payload) == 0 {
		return
	}
	var cfg struct {
		PingInterval int `json:"PingInterval"`
	}
	if json.Unmarshal(f.Payload, &cfg) == nil && cfg.PingInterval > 0 {
		ping.Store(int64(time.Duration(cfg.PingInterval) * time.Second))
	}
}

// handleData 收齐分片后处理事件，并把处理结果回给飞书。
func (c *Client) handleData(ctx context.Context, conn *ws.Conn, f *frame) {
	if f.header(headerType) != typeEvent {
		return
	}
	payload, ok := c.mergeFragments(f)
	if !ok {
		return
	}
	start := time.Now()
	code := c.processEvent(ctx, payload)

	resp := *f
	resp.Headers = append(append([]header(nil), f.Headers...), header{headerBizRT, strconv.FormatInt(time.Since(start).Milliseconds(), 10)})
	resp.Payload, _ = json.Marshal(map[string]int{"code": code})
	if err := conn.WriteBinary(resp.encode()); err != nil {
		c.logger.Printf("回复飞书事件确认失败: %v", err)
	}
}

// mergeFragments 按 message_id 收集分片（sum 片，seq 从 0 开始），收齐时返回拼接后的数据。
func (c *Client) mergeFragments(f *frame) ([]byte, bool) {
	sum, _ := strconv.Atoi(f.header(headerSum))
	if sum <= 1 {
		return f.Payload, true
	}
	seq, err := strconv.Atoi(f.header(headerSeq))
	id := f.header(headerMessageID)
	if err != nil || seq < 0 || seq >= sum || id == "" {
		c.logger.Printf("飞书事件分片信息不合法: message_id=%q sum=%d seq=%q", id, sum, f.header(headerSeq))
		return nil, false
	}
	now := time.Now()
	for k, p := range c.fragments {
		if now.Sub(p.created) > fragmentTTL {
			delete(c.fragments, k)
		}
	}
	p := c.fragments[id]
	if p == nil || len(p.parts) != sum {
		p = &partialEvent{parts: make([][]byte, sum), created: now}
		c.fragments[id] = p
	}
	if p.parts[seq] == nil {
		p.parts[seq] = f.Payload
		p.got++
	}
	if p.got < sum {
		return nil, false
	}
	delete(c.fragments, id)
	return bytes.Join(p.parts, nil), true
}

// ---------------------------------------------------------------------------
// 消息事件
// ---------------------------------------------------------------------------

type eventEnvelope struct {
	Header struct {
		EventID   string `json:"event_id"`
		EventType string `json:"event_type"`
	} `json:"header"`
	Event json.RawMessage `json:"event"`
}

type messageEvent struct {
	Sender struct {
		SenderID struct {
			OpenID string `json:"open_id"`
		} `json:"sender_id"`
		SenderType string `json:"sender_type"`
	} `json:"sender"`
	Message struct {
		MessageID   string `json:"message_id"`
		CreateTime  string `json:"create_time"` // 毫秒时间戳
		ChatType    string `json:"chat_type"`
		MessageType string `json:"message_type"`
		Content     string `json:"content"`
		Mentions    []struct {
			Key string `json:"key"`
		} `json:"mentions"`
	} `json:"message"`
}

// messageTypeNames 是非文字消息类型的中文名称，用于控制台提示。
var messageTypeNames = map[string]string{
	"post":          "富文本",
	"image":         "图片",
	"file":          "文件",
	"audio":         "语音",
	"media":         "视频",
	"sticker":       "表情",
	"interactive":   "卡片",
	"share_chat":    "群名片",
	"share_user":    "个人名片",
	"merge_forward": "合并转发",
	"location":      "位置",
}

// processEvent 处理一个事件，返回回给飞书的状态码：200 表示已处理（包括按规则忽略），
// 500 表示键入失败、希望飞书稍后重推。
func (c *Client) processEvent(ctx context.Context, payload []byte) int {
	var env eventEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		c.logger.Printf("解析飞书事件失败: %v", err)
		return http.StatusOK
	}
	if env.Header.EventType != eventMessageReceive {
		return http.StatusOK
	}
	var ev messageEvent
	if err := json.Unmarshal(env.Event, &ev); err != nil {
		c.logger.Printf("解析飞书消息事件失败: %v", err)
		return http.StatusOK
	}
	if ev.Sender.SenderType != "" && ev.Sender.SenderType != "user" {
		return http.StatusOK
	}
	id := ev.Message.MessageID
	if id == "" {
		id = env.Header.EventID
	}
	if id != "" && c.acked.Has(id) {
		return http.StatusOK
	}
	from := ev.Sender.SenderID.OpenID

	if created, _ := strconv.ParseInt(ev.Message.CreateTime, 10, 64); source.IsStale(created, c.startedAt) {
		c.logger.Printf("忽略启动前积压的历史消息 id=%s（不键入）", id)
		c.markAcked(id)
		return http.StatusOK
	}

	if ev.Message.ChatType != "p2p" {
		c.logger.Printf("忽略群聊消息 id=%s：只支持机器人单聊", id)
		c.markAcked(id)
		return http.StatusOK
	}
	if !c.senderAllowed(from) {
		c.logger.Printf("忽略非白名单发送者 %s", from)
		c.markAcked(id)
		return http.StatusOK
	}
	if ev.Message.MessageType != "text" {
		name, ok := messageTypeNames[ev.Message.MessageType]
		if !ok {
			name = "未知类型 " + ev.Message.MessageType + " "
		}
		c.logger.Printf("忽略%s消息 from=%s id=%s：暂只支持文字消息", name, from, id)
		c.markAcked(id)
		return http.StatusOK
	}
	text := extractText(ev.Message.Content, ev.Message.Mentions)
	if text == "" {
		c.logger.Printf("忽略空白文字消息 from=%s id=%s", from, id)
		c.markAcked(id)
		return http.StatusOK
	}

	c.logger.Printf("收到文本消息 from=%s id=%s: %s", from, id, truncate(text, 100))
	err := c.handle(ctx, source.Message{From: from, Text: text})
	if errors.Is(err, source.ErrDiscarded) {
		c.logger.Printf("消息 id=%s 未发送: %v", id, err)
		c.markAcked(id)
		return http.StatusOK
	}
	if err != nil {
		c.logger.Printf("文字输入失败: %v", err)
		return http.StatusInternalServerError
	}
	c.markAcked(id)

	if reply := strings.TrimSpace(c.cfg.Ack.ReplyText); reply != "" {
		if err := c.sendText(ctx, from, reply); err != nil {
			c.logger.Printf("发送回执失败: %v", err)
		}
	}
	c.logger.Println("消息已投递并确认")
	return http.StatusOK
}

// extractText 从 content（形如 {"text":"..."}）取出文字，并去掉 @ 提及的占位符（如 @_user_1）。
func extractText(content string, mentions []struct {
	Key string `json:"key"`
}) string {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &body); err != nil {
		return ""
	}
	text := body.Text
	for _, m := range mentions {
		if m.Key != "" {
			text = strings.ReplaceAll(text, m.Key, "")
		}
	}
	return strings.TrimSpace(text)
}

func (c *Client) markAcked(id string) {
	if id == "" {
		return
	}
	if err := c.acked.Add(id); err != nil {
		c.logger.Printf("保存已处理消息记录失败: %v", err)
	}
}

// senderAllowed 只接受扫码人本人。
func (c *Client) senderAllowed(openID string) bool {
	return openID != "" && openID == c.owner
}

// ---------------------------------------------------------------------------
// 回执
// ---------------------------------------------------------------------------

// sendText 以机器人身份给 openID 发送一条文本消息。
func (c *Client) sendText(ctx context.Context, openID, text string) error {
	tok, err := c.tenantAccessToken(ctx)
	if err != nil {
		return err
	}
	content, _ := json.Marshal(map[string]string{"text": text})
	body, _ := json.Marshal(map[string]string{"receive_id": openID, "msg_type": "text", "content": string(content)})
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		c.openBase(c.domain)+"/open-apis/im/v1/messages?receive_id_type=open_id", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+tok)
	var r struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := c.doJSON(req, &r); err != nil {
		return err
	}
	if r.Code != 0 {
		return fmt.Errorf("code=%d %s", r.Code, r.Msg)
	}
	return nil
}

// tenantAccessToken 获取并缓存应用访问凭证，过期前 5 分钟刷新。
func (c *Client) tenantAccessToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.tenantToken != "" && time.Now().Before(c.tokenExpiry) {
		return c.tenantToken, nil
	}
	body, _ := json.Marshal(map[string]string{"app_id": c.appID, "app_secret": c.appSecret})
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		c.openBase(c.domain)+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	var r struct {
		Code   int    `json:"code"`
		Msg    string `json:"msg"`
		Token  string `json:"tenant_access_token"`
		Expire int    `json:"expire"`
	}
	if err := c.doJSON(req, &r); err != nil {
		return "", fmt.Errorf("获取应用访问凭证失败: %w", err)
	}
	if r.Code != 0 || r.Token == "" {
		return "", fmt.Errorf("获取应用访问凭证失败: code=%d %s", r.Code, r.Msg)
	}
	c.tenantToken = r.Token
	c.tokenExpiry = time.Now().Add(time.Duration(r.Expire)*time.Second - 5*time.Minute)
	return r.Token, nil
}

// doJSON 发送请求并解析 JSON 响应。飞书接口出错时也常返回 JSON，因此先尝试解析，解析不了再报 HTTP 错误。
func (c *Client) doJSON(req *http.Request, out any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	return nil
}
