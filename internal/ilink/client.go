// Package ilink 实现微信 iLink Bot API 客户端：签名、扫码登录、长轮询、
// 消息 ACK、断线重连与文本回复。
package ilink

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"wetype/internal/config"
	"wetype/internal/relay"
	"wetype/internal/store"
)

const (
	msgTypeUser   = 1 // 用户发来的消息
	msgTypeBot    = 2 // Bot 发出的消息
	stateFinish   = 2
	itemTypeText  = 1
	itemTypeImage = 2
	itemTypeVoice = 3
	itemTypeFile  = 4
	itemTypeVideo = 5
)

// ErrSessionExpired 表示 iLink 会话过期（errcode/ret = -14），需要重新扫码。
var ErrSessionExpired = errors.New("ilink 会话已过期")

// Client 封装 iLink Bot API 的登录、长轮询与发送逻辑。
// settings 是微信来源的内置参数，不在 config.json 里配置。
type settings struct {
	baseURL        string        // iLink Bot API 地址；登录后服务端可能下发独立的 baseurl，存在凭证文件里
	botType        string        // 扫码登录的 bot 类型
	channelVersion string        // base_info.channel_version，随官方插件升级
	qrPollInterval time.Duration // 扫码状态轮询间隔
	pollTimeout    time.Duration // getupdates 长轮询超时，服务端可能下发新的值
	httpTimeout    time.Duration // 单次 HTTP 超时，需大于长轮询超时
	sign           bool          // 是否附加 HMAC-SHA256 签名头（密钥取自 bot_token）
}

var defaultSettings = settings{
	baseURL:        "https://ilinkai.weixin.qq.com",
	botType:        "3",
	channelVersion: "2.1.6",
	qrPollInterval: 2 * time.Second,
	pollTimeout:    35 * time.Second,
	httpTimeout:    45 * time.Second,
	sign:           true,
}

type Client struct {
	cfg        *config.Config
	set        settings
	store      *store.TokenStore
	acked      *store.AckedIDs
	logger     *log.Logger
	httpClient *http.Client
	handle     relay.Handler
}

// NewClient 创建 iLink 客户端。acked 记录已处理的消息 ID，用于游标重放时去重。
func NewClient(cfg *config.Config, st *store.TokenStore, acked *store.AckedIDs, logger *log.Logger) *Client {
	if logger == nil {
		logger = log.Default()
	}
	return &Client{cfg: cfg, set: defaultSettings, store: st, acked: acked, logger: logger, httpClient: &http.Client{}}
}

func (c *Client) baseURL() string {
	if b := c.store.Base(); b != "" {
		return strings.TrimRight(b, "/")
	}
	return strings.TrimRight(c.set.baseURL, "/")
}

func (c *Client) doPost(ctx context.Context, endpoint string, payload any, timeout time.Duration) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		c.baseURL()+"/"+strings.TrimLeft(endpoint, "/"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	req.Header.Set("iLink-App-Id", "bot")
	req.Header.Set("X-WECHAT-UIN", randomWechatUIN())
	if tok := c.store.Token(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	c.signRequest(req, body)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("%w: HTTP %d %s", ErrSessionExpired, resp.StatusCode, truncate(string(data), 200))
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	return data, nil
}

func (c *Client) doGet(ctx context.Context, rawURL string, out any, timeout time.Duration) error {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("iLink-App-Id", "bot")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	return json.Unmarshal(data, out)
}

// signRequest 对业务 POST 请求计算 HMAC-SHA256 签名。
//
//	canonical = METHOD \n PATH \n TIMESTAMP \n NONCE \n SHA256(body)
//	signature = "v1=" + base64(HMAC-SHA256(secret, canonical))
func (c *Client) signRequest(req *http.Request, body []byte) {
	if !c.set.sign {
		return
	}
	secret := tokenSecret(c.store.Token())
	if secret == "" {
		return
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := randomHex(16)
	sum := sha256.Sum256(body)
	canonical := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		ts,
		nonce,
		hex.EncodeToString(sum[:]),
	}, "\n")

	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	req.Header.Set("X-ILink-Timestamp", ts)
	req.Header.Set("X-ILink-Nonce", nonce)
	req.Header.Set("X-ILink-Signature", "v1="+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
}

func tokenSecret(token string) string {
	if i := strings.LastIndex(token, ":"); i >= 0 && i+1 < len(token) {
		return token[i+1:]
	}
	return token
}

// ---------------------------------------------------------------------------
// 扫码登录
// ---------------------------------------------------------------------------

func (c *Client) login(ctx context.Context) error {
	loginBase := strings.TrimRight(c.set.baseURL, "/")
	interval := c.set.qrPollInterval

outer:
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		var qr QRCodeResponse
		qrURL := loginBase + "/ilink/bot/get_bot_qrcode?bot_type=" + url.QueryEscape(c.set.botType)
		if err := c.doGet(ctx, qrURL, &qr, 20*time.Second); err != nil {
			return fmt.Errorf("获取登录二维码失败: %w", err)
		}
		if qr.QRCode == "" {
			return errors.New("登录二维码响应为空")
		}

		c.logger.Printf("请使用微信扫码登录 ClawBot：\n%s", qr.QRCodeImgContent)
		if c.cfg.Login.AutoOpenQR {
			c.openURL(qr.QRCodeImgContent)
		}

		scannedLogged := false
		for {
			if err := sleepCtx(ctx, interval); err != nil {
				return err
			}
			st, err := c.pollQRStatus(ctx, loginBase, qr.QRCode)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				c.logger.Printf("查询扫码状态失败: %v", err)
				continue
			}
			switch st.Status {
			case "wait", "":
				// 等待扫码
			case "scaned", "scanned":
				if !scannedLogged {
					c.logger.Println("已扫码，请在手机上确认登录")
					scannedLogged = true
				}
			case "scaned_but_redirect":
				if st.RedirectHost != "" {
					loginBase = "https://" + st.RedirectHost
					c.logger.Printf("登录域名已切换为 %s", loginBase)
				}
			case "confirmed":
				if st.BotToken == "" {
					return errors.New("扫码已确认，但服务端未返回 bot_token")
				}
				c.store.Reset()
				c.store.SetSession(st.BotToken, st.ILinkBotID, st.ILinkUserID, st.BaseURL)
				if err := c.store.Save(); err != nil {
					return fmt.Errorf("保存登录凭证失败: %w", err)
				}
				c.logger.Printf("登录成功，bot_id=%s，凭证已保存到 %s", st.ILinkBotID, c.store.Path())
				return nil
			case "expired":
				c.logger.Println("二维码已过期，重新获取")
				continue outer
			default:
				c.logger.Printf("未知扫码状态: %s", st.Status)
			}
		}
	}
}

func (c *Client) pollQRStatus(ctx context.Context, base, qrcode string) (*QRStatusResponse, error) {
	u := strings.TrimRight(base, "/") + "/ilink/bot/get_qrcode_status?qrcode=" + url.QueryEscape(qrcode)
	var st QRStatusResponse
	if err := c.doGet(ctx, u, &st, 20*time.Second); err != nil {
		return nil, err
	}
	return &st, nil
}

// ---------------------------------------------------------------------------
// 消息轮询 / ACK / 重连
// ---------------------------------------------------------------------------

var _ relay.Source = (*Client)(nil)

// Name 实现 relay.Source。
func (c *Client) Name() string { return "微信" }

// Run 实现 relay.Source，常驻运行：未登录先扫码，随后进入长轮询；异常时按退避策略恢复。
func (c *Client) Run(ctx context.Context, handle relay.Handler) error {
	c.handle = handle
	minDelay := time.Duration(c.cfg.Reconnect.MinDelayMS) * time.Millisecond
	maxDelay := time.Duration(c.cfg.Reconnect.MaxDelayMS) * time.Millisecond
	backoff := minDelay

	for {
		if ctx.Err() != nil {
			return nil
		}

		if c.store.Token() == "" {
			if err := c.login(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				c.logger.Printf("登录失败: %v，%s 后重试", err, backoff)
				_ = sleepCtx(ctx, backoff+jitter(backoff))
				backoff = nextBackoff(backoff, maxDelay)
				continue
			}
			backoff = minDelay
		}

		err := c.pollLoop(ctx, func() { backoff = minDelay })
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			continue
		}
		if errors.Is(err, ErrSessionExpired) {
			c.logger.Println("会话已过期（-14），清除本地凭证并重新扫码")
			c.resetSession()
			backoff = minDelay
			continue
		}

		c.logger.Printf("消息轮询中断: %v，%s 后重连", err, backoff)
		_ = sleepCtx(ctx, backoff+jitter(backoff))
		backoff = nextBackoff(backoff, maxDelay)
	}
}

func (c *Client) pollLoop(ctx context.Context, onHealthy func()) error {
	pollTimeout := c.set.pollTimeout

	for {
		resp, err := c.getUpdates(ctx, c.store.Buf(), pollTimeout)
		if err != nil {
			return err
		}
		if resp.Ret == -14 || resp.ErrCode == -14 {
			return ErrSessionExpired
		}
		if resp.Ret != 0 || resp.ErrCode != 0 {
			return fmt.Errorf("getupdates 失败: ret=%d errcode=%d errmsg=%s",
				resp.Ret, resp.ErrCode, resp.ErrMsg)
		}

		onHealthy()
		if resp.LongPollingTimeoutMs > 1000 {
			pollTimeout = time.Duration(resp.LongPollingTimeoutMs) * time.Millisecond
		}

		allOK := true
		for i := range resp.Msgs {
			if !c.handleMessage(ctx, &resp.Msgs[i]) {
				allOK = false
			}
		}
		if !allOK {
			// 不推进游标，下一轮会重新收到该批消息；已处理的会通过 ACK 记录去重。
			return errors.New("有消息输入失败，保留游标等待重试")
		}

		buf := resp.GetUpdatesBuf
		if buf == "" {
			buf = resp.SyncBuf
		}
		if buf != "" {
			c.store.SetBuf(buf)
		}
		if err := c.store.Save(); err != nil {
			c.logger.Printf("保存 token/游标失败: %v", err)
		}
	}
}

func (c *Client) getUpdates(ctx context.Context, buf string, pollTimeout time.Duration) (*GetUpdatesResp, error) {
	httpTimeout := c.set.httpTimeout
	if httpTimeout <= pollTimeout {
		httpTimeout = pollTimeout + 10*time.Second
	}
	req := GetUpdatesReq{
		GetUpdatesBuf: buf,
		BaseInfo:      &BaseInfo{ChannelVersion: c.set.channelVersion},
	}
	data, err := c.doPost(ctx, "ilink/bot/getupdates", req, httpTimeout)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			// 长轮询客户端超时等价于“本轮没有新消息”。
			return &GetUpdatesResp{}, nil
		}
		return nil, err
	}
	var resp GetUpdatesResp
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("解析 getupdates 响应失败: %w", err)
	}
	return &resp, nil
}

// handleMessage 处理单条消息。返回 false 表示输入失败，需要保留游标重试。
func (c *Client) handleMessage(ctx context.Context, msg *WeixinMessage) bool {
	if msg.MessageType != msgTypeUser || msg.FromUserID == "" {
		return true
	}

	key := msg.MessageID
	if key == 0 {
		key = msg.Seq
	}
	if key != 0 && c.acked.Has(strconv.FormatInt(key, 10)) {
		return true
	}

	if msg.ContextToken != "" {
		c.store.SetContextToken(msg.FromUserID, msg.ContextToken)
	}

	text := extractText(msg)
	if !c.senderAllowed(msg.FromUserID) {
		c.logger.Printf("忽略非白名单发送者 %s", msg.FromUserID)
		c.markAcked(key)
		return true
	}
	if text == "" {
		// 非文字消息只在控制台提示，不输入、不回复。
		c.logger.Printf("忽略%s from=%s id=%d：暂只支持文字消息", describeItems(msg.ItemList), msg.FromUserID, key)
		c.markAcked(key)
		return true
	}

	c.logger.Printf("收到文本消息 from=%s id=%d: %s", msg.FromUserID, key, truncate(text, 100))
	err := c.handle(ctx, relay.Message{From: msg.FromUserID, Text: text})
	if errors.Is(err, relay.ErrDiscarded) {
		c.logger.Printf("消息 id=%d 未发送: %v", key, err)
		c.markAcked(key)
		return true
	}
	if err != nil {
		c.logger.Printf("文字输入失败: %v", err)
		return false
	}
	c.markAcked(key)

	if reply := strings.TrimSpace(c.cfg.Ack.ReplyText); reply != "" {
		if err := c.sendText(ctx, msg.FromUserID, reply, msg.ContextToken); err != nil {
			c.logger.Printf("发送 ACK 回复失败: %v", err)
		}
	}
	c.logger.Println("消息已投递并 ACK")
	return true
}

func (c *Client) markAcked(key int64) {
	if key == 0 {
		return
	}
	if err := c.acked.Add(strconv.FormatInt(key, 10)); err != nil {
		c.logger.Printf("保存已处理消息记录失败: %v", err)
	}
}

// senderAllowed 只接受扫码人本人（登录时返回的 ilink_user_id，与消息的 from_user_id 是同一套 ID）。
// 与微信官方插件 @tencent-weixin/openclaw-weixin 一致：凭证里没有扫码人时接受所有人。
func (c *Client) senderAllowed(userID string) bool {
	owner := c.store.Owner()
	return owner == "" || userID == owner
}

func (c *Client) sendText(ctx context.Context, to, text, contextToken string) error {
	req := SendMessageReq{
		Msg: &WeixinMessage{
			ToUserID:     to,
			ClientID:     "wetype-" + randomHex(8),
			MessageType:  msgTypeBot,
			MessageState: stateFinish,
			ContextToken: contextToken,
			ItemList: []MessageItem{{
				Type:     itemTypeText,
				TextItem: &TextItem{Text: text},
			}},
		},
		BaseInfo: &BaseInfo{ChannelVersion: c.set.channelVersion},
	}
	_, err := c.doPost(ctx, "ilink/bot/sendmessage", req, 15*time.Second)
	return err
}

func (c *Client) resetSession() {
	if err := c.store.Delete(); err != nil {
		c.logger.Printf("删除旧凭证失败: %v", err)
	}
}

func extractText(msg *WeixinMessage) string {
	var parts []string
	for _, item := range msg.ItemList {
		if item.Type == itemTypeText && item.TextItem != nil {
			if t := item.TextItem.Text; strings.TrimSpace(t) != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// itemTypeNames 是消息条目类型的中文名称，用于控制台提示。
var itemTypeNames = map[int]string{
	itemTypeText:  "空白文字",
	itemTypeImage: "图片",
	itemTypeVoice: "语音",
	itemTypeFile:  "文件",
	itemTypeVideo: "视频",
}

// describeItems 概括非文字消息包含的内容，例如“语音消息”“图片、文件消息”。
func describeItems(items []MessageItem) string {
	var names []string
	seen := map[string]bool{}
	for _, it := range items {
		name, ok := itemTypeNames[it.Type]
		if !ok {
			name = fmt.Sprintf("未知类型 %d", it.Type)
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "空消息"
	}
	return strings.Join(names, "、") + "消息"
}
