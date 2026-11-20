package wechat

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/timandy/talktype/internal/browser"
)

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// randomWechatUIN 生成 X-WECHAT-UIN：随机 uint32 的十进制字符串再 base64。
func randomWechatUIN() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return base64.StdEncoding.EncodeToString([]byte(strconv.FormatInt(time.Now().UnixNano()&0xffffffff, 10)))
	}
	v := binary.LittleEndian.Uint32(b)
	return base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(uint64(v), 10)))
}

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
	if max <= 0 {
		return 0
	}
	ms := max.Milliseconds()
	if ms <= 0 {
		return 0
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return 0
	}
	randMS := int64(binary.LittleEndian.Uint32(b))
	return time.Duration(randMS%ms) * time.Millisecond
}

func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next > max {
		next = max
	}
	if next <= 0 {
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

// openURL 用系统默认浏览器打开二维码链接。
func (c *Client) openURL(u string) {
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return
	}
	if err := browser.Open(u); err != nil {
		c.logger.Printf("打开二维码链接失败: %v", err)
	}
}
