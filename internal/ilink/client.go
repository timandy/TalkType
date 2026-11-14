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

	"clawbot-ilink-relay/internal/config"
	"clawbot-ilink-relay/internal/store"
	"clawbot-ilink-relay/internal/wintype"
)

const (
	msgTypeUser  = 1 // 用户发来的消息
	msgTypeBot   = 2 // Bot 发出的消息
	stateFinish  = 2
	itemTypeText = 1
)

// ErrSessionExpired 表示 iLink 会话过期（errcode/ret = -14），需要重新扫码。
var ErrSessionExpired = errors.New("ilink 会话已过期")

// Client 封装 iLink Bot API 的登录、长轮询与发送逻辑。
type Client struct {
	cfg        *config.Config
	store      *store.TokenStore
	logger     *log.Logger
	httpClient *http.Client
}

// NewClient 创建 iLink 客户端。
func NewClient(cfg *config.Config, st *store.TokenStore, logger *log.Logger) *Client {
	if logger == nil {
		logger = log.Default()
	}
	return &Client{cfg: cfg, store: st, logger: logger, httpClient: &http.Client{}}
}

func (c *Client) baseURL() string {
	if b := c.store.Base(); b != "" {
		return strings.TrimRight(b, "/")
	}
	return strings.TrimRight(c.cfg.BaseURL, "/")
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
	if !c.cfg.Signature.Enabled {
		return
	}
	secret := strings.TrimSpace(c.cfg.Signature.Secret)
	if secret == "" {
		secret = tokenSecret(c.store.Token())
	}
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
	loginBase := strings.TrimRight(c.cfg.BaseURL, "/")
	interval := time.Duration(c.cfg.Login.PollIntervalMS) * time.Millisecond

outer:
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		var qr QRCodeResponse
		qrURL := loginBase + "/ilink/bot/get_bot_qrcode?bot_type=" + url.QueryEscape(c.cfg.BotType)
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

// Run 常驻运行：未登录先扫码，随后进入长轮询；异常时按退避策略恢复。
func (c *Client) Run(ctx context.Context) error {
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
	pollTimeout := time.Duration(c.cfg.Poll.TimeoutSeconds) * time.Second

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
	httpTimeout := time.Duration(c.cfg.Poll.HTTPTimeoutSeconds) * time.Second
	if httpTimeout <= pollTimeout {
		httpTimeout = pollTimeout + 10*time.Second
	}
	req := GetUpdatesReq{
		GetUpdatesBuf: buf,
		BaseInfo:      &BaseInfo{ChannelVersion: c.cfg.ChannelVersion},
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
	if key != 0 && c.store.IsAcked(key) {
		return true
	}

	if msg.ContextToken != "" {
		c.store.SetContextToken(msg.FromUserID, msg.ContextToken)
	}

	text := extractText(msg)
	if text == "" {
		c.markAcked(key)
		return true
	}
	if !c.senderAllowed(msg.FromUserID) {
		c.logger.Printf("忽略非白名单发送者 %s", msg.FromUserID)
		c.markAcked(key)
		return true
	}

	c.logger.Printf("收到文本消息 from=%s id=%d: %s", msg.FromUserID, key, truncate(text, 100))
	opts := wintype.Options{
		TypeDelay:  time.Duration(c.cfg.Inject.TypeDelayMS) * time.Millisecond,
		EnterDelay: time.Duration(c.cfg.Inject.EnterDelayMS) * time.Millisecond,
		PressEnter: c.cfg.Inject.PressEnter,
		MaxRetries: c.cfg.Inject.MaxRetries,
	}
	if err := wintype.TypeWithRetry(text, opts); err != nil {
		c.logger.Printf("文字输入失败: %v", err)
		return false
	}
	c.markAcked(key)

	if reply := strings.TrimSpace(c.cfg.Ack.ReplyText); reply != "" {
		if err := c.sendText(ctx, msg.FromUserID, reply, msg.ContextToken); err != nil {
			c.logger.Printf("发送 ACK 回复失败: %v", err)
		}
	}
	c.logger.Println("已输入到光标并回车，消息已 ACK")
	return true
}

func (c *Client) markAcked(key int64) {
	if key == 0 {
		return
	}
	c.store.MarkAcked(key, c.cfg.Ack.MaxAckedIDs)
}

func (c *Client) senderAllowed(userID string) bool {
	allowed := c.cfg.Filter.AllowedSenders
	if len(allowed) == 0 {
		return true
	}
	for _, v := range allowed {
		if v == userID {
			return true
		}
	}
	return false
}

func (c *Client) sendText(ctx context.Context, to, text, contextToken string) error {
	req := SendMessageReq{
		Msg: &WeixinMessage{
			ToUserID:     to,
			ClientID:     "clawbot-" + randomHex(8),
			MessageType:  msgTypeBot,
			MessageState: stateFinish,
			ContextToken: contextToken,
			ItemList: []MessageItem{{
				Type:     itemTypeText,
				TextItem: &TextItem{Text: text},
			}},
		},
		BaseInfo: &BaseInfo{ChannelVersion: c.cfg.ChannelVersion},
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
