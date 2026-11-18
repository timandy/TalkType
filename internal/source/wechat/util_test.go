package wechat

import (
	"context"
	"encoding/base64"
	"regexp"
	"strconv"
	"testing"
	"time"
)

func TestRandomHex(t *testing.T) {
	s := randomHex(8)
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(s) {
		t.Errorf("randomHex(8) = %q", s)
	}
	if randomHex(8) == randomHex(8) {
		t.Error("two calls returned identical values")
	}
}

func TestRandomWechatUIN(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(randomWechatUIN())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strconv.ParseUint(string(raw), 10, 32); err != nil {
		t.Errorf("decoded %q is not a uint32 decimal: %v", raw, err)
	}
}

func TestSleepCtx(t *testing.T) {
	if err := sleepCtx(context.Background(), 0); err != nil {
		t.Errorf("zero duration: %v", err)
	}
	if err := sleepCtx(context.Background(), 5*time.Millisecond); err != nil {
		t.Errorf("short sleep: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); err != context.Canceled {
		t.Errorf("cancelled ctx: err = %v", err)
	}
	if err := sleepCtx(ctx, 0); err != context.Canceled {
		t.Errorf("cancelled ctx with zero duration: err = %v", err)
	}
}

func TestJitter(t *testing.T) {
	if jitter(0) != 0 || jitter(-time.Second) != 0 || jitter(500*time.Microsecond) != 0 {
		t.Error("non-positive or sub-millisecond max should return 0")
	}
	max := 100 * time.Millisecond
	for i := 0; i < 200; i++ {
		if j := jitter(max); j < 0 || j >= max {
			t.Fatalf("jitter = %v, want in [0,%v)", j, max)
		}
	}
}

func TestNextBackoff(t *testing.T) {
	tests := []struct {
		cur, max, want time.Duration
	}{
		{time.Second, time.Minute, 2 * time.Second},
		{40 * time.Second, time.Minute, time.Minute},
		{time.Minute, time.Minute, time.Minute},
		{0, time.Minute, time.Minute},
	}
	for _, tc := range tests {
		if got := nextBackoff(tc.cur, tc.max); got != tc.want {
			t.Errorf("nextBackoff(%v,%v) = %v, want %v", tc.cur, tc.max, got, tc.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"hello", 5, "hello"},
		{"hello", 3, "hel..."},
		{"", 3, ""},
		{"你好世界", 2, "你好..."},
		{"你好", 2, "你好"},
	}
	for _, tc := range tests {
		if got := truncate(tc.in, tc.n); got != tc.want {
			t.Errorf("truncate(%q,%d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
