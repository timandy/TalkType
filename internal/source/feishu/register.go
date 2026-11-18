package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"wetype/internal/config"
)

// 扫码创建机器人：飞书的应用注册接口，OAuth 设备码流程（init → begin → poll）。
// 接口和参数取自飞书官方安装工具 @larksuite/openclaw-lark-tools，开放平台没有公开文档。

const (
	registrationPath = "/oauth/v1/app/registration"
	// registrationFrom 加在授权链接上。不能再带 tp=ob_cli_app（第三方 OpenClaw 插件的写法）：
	// 实测那样创建的是“飞书 CLI”类应用，机器人处于未激活状态（activate_status=0），收不到消息。
	registrationFrom = "onboard"

	defaultRegistrationInterval = 5 * time.Second
	defaultRegistrationExpire   = 10 * time.Minute
)

// errScanUnavailable 表示飞书接口不支持扫码创建（init 没有返回 client_secret 方式）。
var errScanUnavailable = errors.New("当前环境不支持扫码创建飞书机器人")

// errScanRestart 表示本次二维码作废（过期、被拒绝），需要重新获取。
var errScanRestart = errors.New("二维码已失效")

type registrationResp struct {
	SupportedAuthMethods []string `json:"supported_auth_methods"`

	DeviceCode              string `json:"device_code"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`

	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	UserInfo     struct {
		OpenID      string `json:"open_id"`
		TenantBrand string `json:"tenant_brand"`
	} `json:"user_info"`

	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// scanResult 是扫码成功后得到的应用凭证。
type scanResult struct {
	AppID, AppSecret, Domain, OpenID string
}

// postRegistration 发送表单请求。轮询未完成时服务端返回非 2xx 状态码，但响应体仍是 JSON，
// 因此不论状态码都解析响应体，解析不了再报错。
func (c *Client) postRegistration(ctx context.Context, domain string, form url.Values) (*registrationResp, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.accountsBase(domain)+registrationPath,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var r registrationResp
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	return &r, nil
}

// scan 扫码创建机器人，直到成功、ctx 结束或确认不支持扫码。二维码失效时自动重新获取。
func (c *Client) scan(ctx context.Context) (*scanResult, error) {
	domain := config.FeishuDomain // 先连国内版，扫码账号属于 Lark 时自动切换
	init, err := c.postRegistration(ctx, domain, url.Values{"action": {"init"}})
	if err != nil {
		return nil, fmt.Errorf("扫码初始化失败: %w", err)
	}
	if !containsString(init.SupportedAuthMethods, "client_secret") {
		return nil, errScanUnavailable
	}
	for {
		res, err := c.scanOnce(ctx, domain)
		if errors.Is(err, errScanRestart) {
			c.logger.Println("重新获取二维码")
			continue
		}
		return res, err
	}
}

func (c *Client) scanOnce(ctx context.Context, domain string) (*scanResult, error) {
	begin, err := c.postRegistration(ctx, domain, url.Values{
		"action":            {"begin"},
		"archetype":         {"PersonalAgent"},
		"auth_method":       {"client_secret"},
		"request_user_info": {"open_id"},
	})
	if err != nil {
		return nil, fmt.Errorf("获取二维码失败: %w", err)
	}
	if begin.DeviceCode == "" || begin.VerificationURIComplete == "" {
		return nil, fmt.Errorf("获取二维码失败: %s %s", begin.Error, begin.ErrorDescription)
	}
	qr, err := url.Parse(begin.VerificationURIComplete)
	if err != nil {
		return nil, fmt.Errorf("二维码地址不合法: %w", err)
	}
	q := qr.Query()
	q.Set("from", registrationFrom)
	qr.RawQuery = q.Encode()

	c.logger.Printf("请用飞书 App 扫描该链接的二维码，创建并绑定机器人：\n%s", qr.String())
	if c.cfg.Login.AutoOpenQR {
		c.openURL(qr.String())
	}

	interval := time.Duration(begin.Interval) * time.Second
	if interval <= 0 {
		interval = defaultRegistrationInterval
	}
	expire := time.Duration(begin.ExpiresIn) * time.Second
	if expire <= 0 {
		expire = defaultRegistrationExpire
	}
	deadline := time.Now().Add(expire)
	switched := false

	for time.Now().Before(deadline) {
		if err := c.sleep(ctx, interval); err != nil {
			return nil, err
		}
		r, err := c.postRegistration(ctx, domain, url.Values{
			"action":      {"poll"},
			"device_code": {begin.DeviceCode},
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			c.logger.Printf("查询扫码状态失败: %v", err)
			continue
		}
		// 扫码人属于国际版 Lark 时，改用 Lark 的地址继续轮询。
		if !switched && r.UserInfo.TenantBrand == config.LarkDomain && domain != config.LarkDomain {
			domain, switched = config.LarkDomain, true
			c.logger.Println("扫码账号属于 Lark 国际版，切换到 Lark 地址")
			continue
		}
		if r.ClientID != "" && r.ClientSecret != "" {
			return &scanResult{AppID: r.ClientID, AppSecret: r.ClientSecret, Domain: domain, OpenID: r.UserInfo.OpenID}, nil
		}
		switch r.Error {
		case "", "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		case "access_denied":
			c.logger.Println("扫码后取消了授权")
			return nil, errScanRestart
		case "expired_token":
			c.logger.Println("二维码已过期")
			return nil, errScanRestart
		default:
			return nil, fmt.Errorf("扫码失败: %s %s", r.Error, r.ErrorDescription)
		}
	}
	c.logger.Println("二维码已过期")
	return nil, errScanRestart
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
