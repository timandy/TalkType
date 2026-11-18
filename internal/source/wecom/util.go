package wecom

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"os/exec"
	"strings"
	"time"
)

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func jitter(max time.Duration) time.Duration {
	ms := max.Milliseconds()
	if ms <= 0 {
		return 0
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	return time.Duration(int64(binary.LittleEndian.Uint32(b[:]))%ms) * time.Millisecond
}

func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next > max || next <= 0 {
		next = max
	}
	return next
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// openURL 用系统默认浏览器打开扫码页面。
func (c *Client) openURL(u string) {
	if !strings.HasPrefix(u, "https://") {
		return
	}
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	if err := cmd.Start(); err != nil {
		c.logger.Printf("打开扫码页面失败: %v", err)
		return
	}
	go func() { _ = cmd.Wait() }()
}
