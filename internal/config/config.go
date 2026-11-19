// Package config 定义 WeType 的配置结构、默认值与校验规则。
//
// config.json 只放各来源共用的配置：消息来源 source、扫码、重连、键入和回执。
// 各来源的服务地址、协议参数都内置在各自的包里；凭证、站点（飞书 / Lark）、本人身份
// 由扫码自动获得，存进配置文件所在目录的 *_token.json。config.json 可以不存在，此时全部使用默认值。
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
	SourceWeCom   = "wecom"
	SourceConsole = "console"
)

// 飞书的两个站点。扫码时先连国内版，按扫码账号自动切换到 Lark，结果存进 feishu_token.json。
const (
	FeishuDomain = "feishu" // 国内版飞书
	LarkDomain   = "lark"   // 国际版 Lark
)

const defaultMaxAckedIDs = 500

// Config 对应 config.json。
type Config struct {
	Source    string          `json:"source"` // 逗号分隔可同时启用多个，如 wechat,feishu；单个取值 wechat（默认）、feishu、wecom 或 console
	Login     LoginConfig     `json:"login"`
	Reconnect ReconnectConfig `json:"reconnect"`
	Inject    InjectConfig    `json:"inject"`
	Ack       AckConfig       `json:"ack"`
}

// LoginConfig 是扫码登录的配置。
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
	}
}

// Default 返回默认配置（已校验）。
func Default() *Config {
	c := defaults()
	_ = c.Validate()
	return c
}

// ErrNotFound 表示配置文件不存在；Load 此时仍返回默认配置。
var ErrNotFound = errors.New("配置文件不存在")

// Load 读取配置文件；未出现的字段保留默认值。出现不认识的字段时报错，
// 避免拼错的字段被静默忽略。文件不存在时返回默认配置和 ErrNotFound。
func Load(path string) (*Config, error) {
	cfg := defaults()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, fmt.Errorf("%w: %s", ErrNotFound, path)
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

// Validate 规范化 source 并补齐缺省值。
func (c *Config) Validate() error {
	names, err := parseSources(c.Source)
	if err != nil {
		return err
	}
	c.Source = strings.Join(names, ",")
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

// Sources 返回 Source 拆分后的来源名列表，顺序与配置一致。需先调用 Validate。
func (c *Config) Sources() []string {
	return strings.Split(c.Source, ",")
}

// parseSources 把逗号分隔的 source 拆成规范化（小写、去空白）的来源名，顺序不变。
// 空串按默认的微信处理；空项（如末尾多余的逗号）忽略；包含 console 时只保留 console；来源名重复或不认识时报错。
func parseSources(s string) ([]string, error) {
	var names []string
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" {
			continue
		}
		switch name {
		case SourceWeChat, SourceFeishu, SourceWeCom, SourceConsole:
		default:
			return nil, fmt.Errorf("source 只能是 %s、%s、%s 或 %s（多个用英文逗号分隔），当前包含 %q", SourceWeChat, SourceFeishu, SourceWeCom, SourceConsole, name)
		}
		if seen[name] {
			return nil, fmt.Errorf("source 中的 %q 重复了", name)
		}
		seen[name] = true
		names = append(names, name)
	}
	if len(names) == 0 {
		names = []string{SourceWeChat}
	}
	// console 独占标准输入，包含它时只启用 console，其余来源不启动。
	if len(names) > 1 && seen[SourceConsole] {
		names = []string{SourceConsole}
	}
	return names, nil
}
