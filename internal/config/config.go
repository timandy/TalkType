// Package config 定义 WeType 的配置结构、默认值与校验规则。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	defaultBaseURL        = "https://ilinkai.weixin.qq.com"
	defaultBotType        = "3"
	defaultChannelVersion = "2.1.6"
	defaultMaxAckedIDs    = 500
)

// Config 对应 config.json。
type Config struct {
	BaseURL        string          `json:"base_url"`
	BotType        string          `json:"bot_type"`
	ChannelVersion string          `json:"channel_version"`
	TokenFile      string          `json:"token_file"`
	Token          string          `json:"token"`
	Signature      SignatureConfig `json:"signature"`
	Login          LoginConfig     `json:"login"`
	Poll           PollConfig      `json:"poll"`
	Reconnect      ReconnectConfig `json:"reconnect"`
	Inject         InjectConfig    `json:"inject"`
	Ack            AckConfig       `json:"ack"`
	Filter         FilterConfig    `json:"filter"`
}

// SignatureConfig 控制自研请求签名层。
// 官方 iLink 用 Bearer bot_token 鉴权；这里的 HMAC-SHA256 签名是对接方的加固
// 扩展，未启用校验的服务端会忽略这些额外请求头。
type SignatureConfig struct {
	Enabled bool   `json:"enabled"`
	Secret  string `json:"secret"`
}

type LoginConfig struct {
	AutoOpenQR     bool `json:"auto_open_qr"`
	PollIntervalMS int  `json:"poll_interval_ms"`
}

type PollConfig struct {
	TimeoutSeconds     int `json:"timeout_seconds"`
	HTTPTimeoutSeconds int `json:"http_timeout_seconds"`
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
	ReplyText   string `json:"reply_text"`
	MaxAckedIDs int    `json:"max_acked_ids"`
}

type FilterConfig struct {
	AllowedSenders []string `json:"allowed_senders"`
}

func defaults() *Config {
	return &Config{
		BaseURL:        defaultBaseURL,
		BotType:        defaultBotType,
		ChannelVersion: defaultChannelVersion,
		TokenFile:      "token.json",
		Signature:      SignatureConfig{Enabled: true},
		Login:          LoginConfig{AutoOpenQR: true, PollIntervalMS: 2000},
		Poll:           PollConfig{TimeoutSeconds: 35, HTTPTimeoutSeconds: 45},
		Reconnect:      ReconnectConfig{MinDelayMS: 1000, MaxDelayMS: 60000},
		Inject: InjectConfig{
			TypeDelayMS:  60,
			EnterDelayMS: 120,
			PressEnter:   true,
			MaxRetries:   5,
		},
		Ack: AckConfig{MaxAckedIDs: defaultMaxAckedIDs},
	}
}

// Load 读取配置文件；未出现的字段保留默认值。
func Load(path string) (*Config, error) {
	cfg := defaults()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("未找到配置文件 %s，请先创建 config.json", path)
		}
		return nil, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}
	return cfg, nil
}

// Validate 补齐缺省值并检查关键配置。
func (c *Config) Validate() error {
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if c.BaseURL == "" {
		return errors.New("base_url 不能为空")
	}
	if c.BotType == "" {
		c.BotType = defaultBotType
	}
	if c.ChannelVersion == "" {
		c.ChannelVersion = defaultChannelVersion
	}
	if c.TokenFile == "" {
		c.TokenFile = "token.json"
	}
	if c.Poll.TimeoutSeconds <= 0 {
		c.Poll.TimeoutSeconds = 35
	}
	if c.Poll.HTTPTimeoutSeconds <= c.Poll.TimeoutSeconds {
		c.Poll.HTTPTimeoutSeconds = c.Poll.TimeoutSeconds + 10
	}
	if c.Login.PollIntervalMS <= 0 {
		c.Login.PollIntervalMS = 2000
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
	return nil
}
