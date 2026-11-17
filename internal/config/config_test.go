package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, data string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_NotExist(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	if _, err := Load(writeConfig(t, "{bad")); err == nil {
		t.Fatal("expected parse error")
	}
}

// 仓库根目录的 config.json 必须能被加载，且与默认值一致。
func TestLoad_RepoConfigMatchesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := defaults()
	// 示例里写的是空数组，默认值是 nil，含义相同
	want.WeChat.AllowedSenders, want.Feishu.AllowedSenders = []string{}, []string{}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("config.json differs from defaults:\n got %+v\nwant %+v", cfg, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoad_KeepsDefaultsAndOverrides(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"source":"Feishu","wechat":{"base_url":"https://example.com","poll":{"timeout_seconds":10},"allowed_senders":["a"]},"feishu":{"app_id":"cli","app_secret":"s"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source != "Feishu" || cfg.WeChat.BaseURL != "https://example.com" || cfg.WeChat.Poll.TimeoutSeconds != 10 {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	// 未出现的字段保留默认值，包括同一对象里没写的字段
	if cfg.WeChat.BotType != defaultBotType || cfg.WeChat.Poll.HTTPTimeoutSeconds != 45 || !cfg.WeChat.Signature.Enabled ||
		cfg.Reconnect.MaxDelayMS != 60000 || !cfg.Inject.PressEnter || !cfg.Login.AutoOpenQR ||
		cfg.Feishu.Domain != FeishuDomain || cfg.Feishu.TokenFile != defaultFeishuTokenFile {
		t.Errorf("defaults not preserved: %+v", cfg)
	}
	if len(cfg.WeChat.AllowedSenders) != 1 || cfg.WeChat.AllowedSenders[0] != "a" {
		t.Errorf("AllowedSenders = %v", cfg.WeChat.AllowedSenders)
	}
}

func TestLoad_IgnoresUTF8BOM(t *testing.T) {
	cfg, err := Load(writeConfig(t, "\xef\xbb\xbf{\"source\":\"feishu\"}"))
	if err != nil || cfg.Source != "feishu" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
}

func TestLoad_RejectsUnknownField(t *testing.T) {
	_, err := Load(writeConfig(t, `{"wechat":{"base_ulr":"https://x"}}`))
	if err == nil || !strings.Contains(err.Error(), "base_ulr") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidate_Source(t *testing.T) {
	for in, want := range map[string]string{"": SourceWeChat, " WeChat ": SourceWeChat, "FEISHU": SourceFeishu, "Console": SourceConsole} {
		c := defaults()
		c.Source = in
		if err := c.Validate(); err != nil || c.Source != want {
			t.Errorf("source %q: got %q err=%v", in, c.Source, err)
		}
	}
	c := defaults()
	c.Source = "dingtalk"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "source") {
		t.Errorf("err = %v", err)
	}
}

// 只校验正在使用的来源。
func TestValidate_OnlyActiveSource(t *testing.T) {
	c := defaults()
	c.WeChat.BaseURL = " "
	c.Feishu.AppID = "cli" // 缺 app_secret
	c.Source = SourceConsole
	if err := c.Validate(); err != nil {
		t.Errorf("console should ignore wechat/feishu errors: %v", err)
	}
	c.Source = SourceWeChat
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "wechat.base_url") {
		t.Errorf("wechat: err = %v", err)
	}
	c.Source = SourceFeishu
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "feishu.app_id") {
		t.Errorf("feishu: err = %v", err)
	}
}

func TestValidate_FillsZeroValues(t *testing.T) {
	c := &Config{WeChat: WeChatConfig{BaseURL: " https://x.com/// "}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"Source", c.Source, SourceWeChat},
		{"WeChat.BaseURL", c.WeChat.BaseURL, "https://x.com"},
		{"WeChat.BotType", c.WeChat.BotType, defaultBotType},
		{"WeChat.ChannelVersion", c.WeChat.ChannelVersion, defaultChannelVersion},
		{"WeChat.TokenFile", c.WeChat.TokenFile, defaultWeChatTokenFile},
		{"WeChat.QRPollIntervalMS", c.WeChat.QRPollIntervalMS, defaultQRPollMS},
		{"WeChat.Poll.TimeoutSeconds", c.WeChat.Poll.TimeoutSeconds, 35},
		{"WeChat.Poll.HTTPTimeoutSeconds", c.WeChat.Poll.HTTPTimeoutSeconds, 45},
		{"Reconnect.MinDelayMS", c.Reconnect.MinDelayMS, 1000},
		{"Reconnect.MaxDelayMS", c.Reconnect.MaxDelayMS, 1000},
		{"Inject.MaxRetries", c.Inject.MaxRetries, 1},
		{"Ack.MaxAckedIDs", c.Ack.MaxAckedIDs, defaultMaxAckedIDs},
		{"Feishu.Domain", c.Feishu.Domain, FeishuDomain},
		{"Feishu.TokenFile", c.Feishu.TokenFile, defaultFeishuTokenFile},
	}
	for _, tc := range checks {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestValidate_KeepsValidValues(t *testing.T) {
	c := defaults()
	c.WeChat.Poll.TimeoutSeconds = 20
	c.WeChat.Poll.HTTPTimeoutSeconds = 60
	c.Reconnect.MinDelayMS = 500
	c.Reconnect.MaxDelayMS = 5000
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.WeChat.Poll.HTTPTimeoutSeconds != 60 || c.Reconnect.MaxDelayMS != 5000 || c.Reconnect.MinDelayMS != 500 {
		t.Errorf("valid values changed: %+v", c)
	}
}

func TestValidate_HTTPTimeoutMustExceedPollTimeout(t *testing.T) {
	c := defaults()
	c.WeChat.Poll.TimeoutSeconds = 30
	c.WeChat.Poll.HTTPTimeoutSeconds = 30
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.WeChat.Poll.HTTPTimeoutSeconds != 40 {
		t.Errorf("HTTPTimeoutSeconds = %d, want 40", c.WeChat.Poll.HTTPTimeoutSeconds)
	}
}

func TestValidate_MaxDelayRaisedToMin(t *testing.T) {
	c := defaults()
	c.Reconnect.MinDelayMS = 3000
	c.Reconnect.MaxDelayMS = 100
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Reconnect.MaxDelayMS != 3000 {
		t.Errorf("MaxDelayMS = %d, want 3000", c.Reconnect.MaxDelayMS)
	}
}

func TestValidate_Feishu(t *testing.T) {
	c := &Config{Source: SourceFeishu, Feishu: FeishuConfig{Domain: " Lark ", AppID: " cli ", AppSecret: " s "}}
	if err := c.Validate(); err != nil || c.Feishu.Domain != LarkDomain || c.Feishu.AppID != "cli" || c.Feishu.AppSecret != "s" {
		t.Errorf("normalize: %+v err=%v", c.Feishu, err)
	}
	for _, f := range []FeishuConfig{{Domain: "wechat"}, {AppID: "cli"}, {AppSecret: "s"}} {
		c := &Config{Source: SourceFeishu, Feishu: f}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "feishu") {
			t.Errorf("%+v: err = %v", f, err)
		}
	}
}
