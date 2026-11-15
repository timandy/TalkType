package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_NotExist(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestLoad_KeepsDefaultsAndOverrides(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	data := `{"base_url":"https://example.com","poll":{"timeout_seconds":10},"filter":{"allowed_senders":["a"]}}`
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "https://example.com" {
		t.Errorf("BaseURL = %q", cfg.BaseURL)
	}
	if cfg.Poll.TimeoutSeconds != 10 {
		t.Errorf("Poll.TimeoutSeconds = %d", cfg.Poll.TimeoutSeconds)
	}
	// 未出现的字段保留默认值
	if cfg.BotType != defaultBotType || cfg.Reconnect.MaxDelayMS != 60000 || !cfg.Inject.PressEnter || !cfg.Signature.Enabled {
		t.Errorf("defaults not preserved: %+v", cfg)
	}
	if len(cfg.Filter.AllowedSenders) != 1 || cfg.Filter.AllowedSenders[0] != "a" {
		t.Errorf("AllowedSenders = %v", cfg.Filter.AllowedSenders)
	}
}

func TestValidate_EmptyBaseURL(t *testing.T) {
	for _, in := range []string{"", "   ", " / "} {
		c := &Config{BaseURL: in}
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "base_url") {
			t.Errorf("BaseURL %q: err = %v", in, err)
		}
	}
}

func TestValidate_FillsZeroValues(t *testing.T) {
	c := &Config{BaseURL: " https://x.com/// "}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "https://x.com" {
		t.Errorf("BaseURL = %q", c.BaseURL)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"BotType", c.BotType, defaultBotType},
		{"ChannelVersion", c.ChannelVersion, defaultChannelVersion},
		{"TokenFile", c.TokenFile, "token.json"},
		{"Poll.TimeoutSeconds", c.Poll.TimeoutSeconds, 35},
		{"Poll.HTTPTimeoutSeconds", c.Poll.HTTPTimeoutSeconds, 45},
		{"Login.PollIntervalMS", c.Login.PollIntervalMS, 2000},
		{"Reconnect.MinDelayMS", c.Reconnect.MinDelayMS, 1000},
		{"Reconnect.MaxDelayMS", c.Reconnect.MaxDelayMS, 1000},
		{"Inject.MaxRetries", c.Inject.MaxRetries, 1},
		{"Ack.MaxAckedIDs", c.Ack.MaxAckedIDs, defaultMaxAckedIDs},
	}
	for _, tc := range checks {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestValidate_KeepsValidValues(t *testing.T) {
	c := defaults()
	c.Poll.TimeoutSeconds = 20
	c.Poll.HTTPTimeoutSeconds = 60
	c.Reconnect.MinDelayMS = 500
	c.Reconnect.MaxDelayMS = 5000
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Poll.HTTPTimeoutSeconds != 60 || c.Reconnect.MaxDelayMS != 5000 || c.Reconnect.MinDelayMS != 500 {
		t.Errorf("valid values changed: %+v", c)
	}
}

func TestValidate_HTTPTimeoutMustExceedPollTimeout(t *testing.T) {
	c := defaults()
	c.Poll.TimeoutSeconds = 30
	c.Poll.HTTPTimeoutSeconds = 30
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Poll.HTTPTimeoutSeconds != 40 {
		t.Errorf("HTTPTimeoutSeconds = %d, want 40", c.Poll.HTTPTimeoutSeconds)
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
