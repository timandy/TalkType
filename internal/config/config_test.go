package config

import (
	"errors"
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

// 配置文件不存在时使用默认配置。
func TestLoad_NotExistUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if !errors.Is(err, ErrNotFound) || cfg == nil {
		t.Fatalf("cfg=%v err=%v", cfg, err)
	}
	if !reflect.DeepEqual(cfg, defaults()) {
		t.Errorf("cfg = %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDefault(t *testing.T) {
	c := Default()
	if c.Source != SourceWeChat || !c.Login.AutoOpenQR || c.Ack.MaxAckedIDs != defaultMaxAckedIDs {
		t.Errorf("Default() = %+v", c)
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	if _, err := Load(writeConfig(t, "{bad")); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoad_IgnoresUTF8BOM(t *testing.T) {
	cfg, err := Load(writeConfig(t, "\xef\xbb\xbf{\"source\":\"feishu\"}"))
	if err != nil || cfg.Source != "feishu" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
}

// 仓库根目录的 config.json 必须能被加载，且与默认值一致。
func TestLoad_RepoConfigMatchesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, defaults()) {
		t.Errorf("config.json differs from defaults:\n got %+v\nwant %+v", cfg, defaults())
	}
}

func TestLoad_KeepsDefaultsAndOverrides(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"source":"Feishu","inject":{"press_enter":false},"reconnect":{"min_delay_ms":500}}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source != "Feishu" || cfg.Inject.PressEnter || cfg.Reconnect.MinDelayMS != 500 {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	// 未出现的字段保留默认值，包括同一对象里没写的字段
	if cfg.Inject.TypeDelayMS != 60 || cfg.Inject.MaxRetries != 5 || cfg.Reconnect.MaxDelayMS != 60000 ||
		!cfg.Login.AutoOpenQR || cfg.Ack.MaxAckedIDs != defaultMaxAckedIDs {
		t.Errorf("defaults not preserved: %+v", cfg)
	}
}

// 各来源的地址、协议参数内置；凭证、白名单、站点由扫码自动获得。写了这些字段会被当成未知字段拒绝。
func TestLoad_RejectsSourceSpecificFields(t *testing.T) {
	for _, data := range []string{
		`{"wechat":{"base_url":"https://x"}}`,
		`{"wechat":{"token":"a:b"}}`,
		`{"feishu":{"app_id":"cli"}}`,
		`{"feishu":{"domain":"lark"}}`,
		`{"wecom":{"bot_id":"b"}}`,
		`{"wecom":{"ws_url":"wss://x"}}`,
		`{"filter":{"allowed_senders":["u"]}}`,
	} {
		if _, err := Load(writeConfig(t, data)); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Errorf("%s: err = %v", data, err)
		}
	}
}

func TestValidate_Source(t *testing.T) {
	for in, want := range map[string]string{"": SourceWeChat, " WeChat ": SourceWeChat, "FEISHU": SourceFeishu, "WeCom": SourceWeCom, "Console": SourceConsole} {
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

func TestValidate_FillsZeroValues(t *testing.T) {
	c := &Config{}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Source != SourceWeChat || c.Reconnect.MinDelayMS != 1000 || c.Reconnect.MaxDelayMS != 1000 ||
		c.Inject.MaxRetries != 1 || c.Ack.MaxAckedIDs != defaultMaxAckedIDs {
		t.Errorf("zero values not filled: %+v", c)
	}
}

func TestValidate_KeepsValidValues(t *testing.T) {
	c := defaults()
	c.Reconnect.MinDelayMS = 500
	c.Reconnect.MaxDelayMS = 5000
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Reconnect.MaxDelayMS != 5000 || c.Reconnect.MinDelayMS != 500 {
		t.Errorf("valid values changed: %+v", c)
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

func TestValidate_MultiSource(t *testing.T) {
	c := Default()
	c.Source = " WeChat , feishu,, WeCom "
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.Sources(), "|"); got != "wechat|feishu|wecom" {
		t.Errorf("Sources() = %q", got)
	}
	for _, bad := range []string{"wechat,wechat", "wechat, dingtalk", "WeChat,wechat"} {
		c := Default()
		c.Source = bad
		if err := c.Validate(); err == nil {
			t.Errorf("source %q 应报错", bad)
		}
	}
}

func TestValidate_ConsoleOnly(t *testing.T) {
	c := Default()
	c.Source = "wechat, Console,feishu"
	if err := c.Validate(); err != nil || c.Source != SourceConsole {
		t.Errorf("got %q err=%v", c.Source, err)
	}
}
