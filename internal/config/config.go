// Package config 定义 WeType 的配置结构、默认值与校验规则。
//
// config.json 的布局：顶层是 source（消息来源）和各来源共用的配置，
// 微信、飞书独有的配置分别放在 wechat、feishu 两个对象里。
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// 消息来源（source 的取值，不区分大小写）。
const (
	SourceWeChat  = "wechat"
	SourceFeishu  = "feishu"
	SourceConsole = "console"
)

// 飞书的两个站点（feishu.domain 的取值）。
const (
	FeishuDomain = "feishu" // 国内版飞书
	LarkDomain   = "lark"   // 国际版 Lark
)

const (
	defaultBaseURL         = "https://ilinkai.weixin.qq.com"
	defaultBotType         = "3"
	defaultChannelVersion  = "2.1.6"
	defaultWeChatTokenFile = "wechat_token.json"
	defaultFeishuTokenFile = "feishu_token.json"
	defaultMaxAckedIDs     = 500
	defaultQRPollMS        = 2000
	defaultPollTimeout     = 35
)

// Config 对应 config.json。
type Config struct {
	Source string `json:"source"` // wechat（默认）、feishu 或 console

	// 各来源共用。
	Login     LoginConfig     `json:"login"`
	Reconnect ReconnectConfig `json:"reconnect"`
	Inject    InjectConfig    `json:"inject"`
	Ack       AckConfig       `json:"ack"`

	// 各来源独有。
	WeChat WeChatConfig `json:"wechat"`
	Feishu FeishuConfig `json:"feishu"`
}

// LoginConfig 是扫码登录的共用配置。
type LoginConfig struct {
	AutoOpenQR bool `json:"auto_open_qr"` // 自动用浏览器打开二维码链接
}

type ReconnectConfig struct {
	MinDelayMS int `json:"min_delay_ms"`
	MaxDelayMS int `json:"max_delay_ms"`
}

type InjectConfig struct {
	TypeDelayMS  int  `json:"type_delay_ms"`
	EnterDelayMS int  `json:"enter_delay_ms"`
	PressEnter   bool `json:"press_enter"`
	MaxRetries   int  `json:"max_retries"`
}

type AckConfig struct {
	ReplyText   string `json:"reply_text"`    // 非空时，键入成功后向发送者回一条文本
	MaxAckedIDs int    `json:"max_acked_ids"` // 本地保留的已处理消息 ID 数量
}

// WeChatConfig 是微信来源（source = wechat）的配置。
type WeChatConfig struct {
	BaseURL          string          `json:"base_url"`
	BotType          string          `json:"bot_type"`
	ChannelVersion   string          `json:"channel_version"`
	TokenFile        string          `json:"token_file"`
	Token            string          `json:"token"`               // 可直接填 bot_id:secret 免扫码
	QRPollIntervalMS int             `json:"qr_poll_interval_ms"` // 扫码状态轮询间隔
	Signature        SignatureConfig `json:"signature"`
	Poll             PollConfig      `json:"poll"`
	AllowedSenders   []string        `json:"allowed_senders"` // 发送者白名单，空表示全部允许
}

// SignatureConfig 控制自研请求签名层。
// 官方 iLink 用 Bearer bot_token 鉴权；这里的 HMAC-SHA256 签名是对接方的加固
// 扩展，未启用校验的服务端会忽略这些额外请求头。
type SignatureConfig struct {
	Enabled bool   `json:"enabled"`
	Secret  string `json:"secret"`
}

type PollConfig struct {
	TimeoutSeconds     int `json:"timeout_seconds"`
	HTTPTimeoutSeconds int `json:"http_timeout_seconds"`
}

// FeishuConfig 是飞书来源（source = feishu）的配置。
// app_id 和 app_secret 留空时扫码创建机器人；填写后直接使用，适合扫码不可用的情况。
type FeishuConfig struct {
	Domain    string `json:"domain"` // feishu（国内版）或 lark（国际版）
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
	TokenFile string `json:"token_file"`
	// AllowedSenders 是发送者 open_id 白名单。为空时只接受扫码人本人的消息；
	// 手动填写凭证、又不知道本人 open_id 时接受所有人。
	AllowedSenders []string `json:"allowed_senders"`
}

func defaults() *Config {
	return &Config{
		Source:    SourceWeChat,
		Login:     LoginConfig{AutoOpenQR: true},
		Reconnect: ReconnectConfig{MinDelayMS: 1000, MaxDelayMS: 60000},
		Inject: InjectConfig{
			TypeDelayMS:  60,
			EnterDelayMS: 120,
			PressEnter:   true,
			MaxRetries:   5,
		},
		Ack: AckConfig{MaxAckedIDs: defaultMaxAckedIDs},
		WeChat: WeChatConfig{
			BaseURL:          defaultBaseURL,
			BotType:          defaultBotType,
			ChannelVersion:   defaultChannelVersion,
			TokenFile:        defaultWeChatTokenFile,
			QRPollIntervalMS: defaultQRPollMS,
			Signature:        SignatureConfig{Enabled: true},
			Poll:             PollConfig{TimeoutSeconds: defaultPollTimeout, HTTPTimeoutSeconds: 45},
		},
		Feishu: FeishuConfig{Domain: FeishuDomain, TokenFile: defaultFeishuTokenFile},
	}
}

// Load 读取配置文件；未出现的字段保留默认值。出现不认识的字段时报错，
// 避免拼错的字段被静默忽略。
func Load(path string) (*Config, error) {
	cfg := defaults()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("未找到配置文件 %s，请先创建 config.json", path)
		}
		return nil, err
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // 记事本等编辑器保存的 UTF-8 BOM
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}
	return cfg, nil
}

// Validate 规范化 source、补齐缺省值，并检查所选来源的关键配置。
// 只校验正在使用的来源，另一个来源的配置填了一半也不影响启动。
func (c *Config) Validate() error {
	c.Source = strings.ToLower(strings.TrimSpace(c.Source))
	switch c.Source {
	case "":
		c.Source = SourceWeChat
	case SourceWeChat, SourceFeishu, SourceConsole:
	default:
		return fmt.Errorf("source 只能是 %s、%s 或 %s，当前为 %q", SourceWeChat, SourceFeishu, SourceConsole, c.Source)
	}

	if c.Reconnect.MinDelayMS <= 0 {
		c.Reconnect.MinDelayMS = 1000
	}
	if c.Reconnect.MaxDelayMS < c.Reconnect.MinDelayMS {
		c.Reconnect.MaxDelayMS = c.Reconnect.MinDelayMS
	}
	if c.Inject.MaxRetries <= 0 {
		c.Inject.MaxRetries = 1
	}
	if c.Ack.MaxAckedIDs <= 0 {
		c.Ack.MaxAckedIDs = defaultMaxAckedIDs
	}

	wechatErr := c.WeChat.validate()
	feishuErr := c.Feishu.validate()
	switch c.Source {
	case SourceWeChat:
		return wechatErr
	case SourceFeishu:
		return feishuErr
	}
	return nil
}

func (w *WeChatConfig) validate() error {
	w.BaseURL = strings.TrimRight(strings.TrimSpace(w.BaseURL), "/")
	if w.BotType == "" {
		w.BotType = defaultBotType
	}
	if w.ChannelVersion == "" {
		w.ChannelVersion = defaultChannelVersion
	}
	if w.TokenFile == "" {
		w.TokenFile = defaultWeChatTokenFile
	}
	if w.QRPollIntervalMS <= 0 {
		w.QRPollIntervalMS = defaultQRPollMS
	}
	if w.Poll.TimeoutSeconds <= 0 {
		w.Poll.TimeoutSeconds = defaultPollTimeout
	}
	if w.Poll.HTTPTimeoutSeconds <= w.Poll.TimeoutSeconds {
		w.Poll.HTTPTimeoutSeconds = w.Poll.TimeoutSeconds + 10
	}
	if w.BaseURL == "" {
		return errors.New("wechat.base_url 不能为空")
	}
	return nil
}

func (f *FeishuConfig) validate() error {
	f.AppID = strings.TrimSpace(f.AppID)
	f.AppSecret = strings.TrimSpace(f.AppSecret)
	if f.TokenFile == "" {
		f.TokenFile = defaultFeishuTokenFile
	}
	f.Domain = strings.ToLower(strings.TrimSpace(f.Domain))
	switch f.Domain {
	case "":
		f.Domain = FeishuDomain
	case FeishuDomain, LarkDomain:
	default:
		return fmt.Errorf("feishu.domain 只能是 %s 或 %s", FeishuDomain, LarkDomain)
	}
	if (f.AppID == "") != (f.AppSecret == "") {
		return errors.New("feishu.app_id 和 feishu.app_secret 需要同时填写或同时留空")
	}
	return nil
}
