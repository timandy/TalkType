//go:build darwin

package wintype

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// osascriptTimeout 是一次键入的最长执行时间（keystroke 输入长文本比较慢）。
const osascriptTimeout = 60 * time.Second

// SetupConsole 在 macOS 上无需处理（终端默认就是 UTF-8）。
func SetupConsole() {}

// Type 通过 osascript 驱动 System Events 把文本键入当前焦点窗口，不读写剪贴板；
// 可选在输入完成后发送回车。需要给运行本程序的终端（或程序）授予辅助功能权限。
func Type(text string, opts Options) error {
	ctx, cancel := context.WithTimeout(context.Background(), osascriptTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "osascript", "-")
	cmd.Stdin = strings.NewReader(buildAppleScript(text, opts))
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if isPermissionError(msg) {
			return fmt.Errorf("%w（请在“系统设置 → 隐私与安全性 → 辅助功能”里允许运行本程序的终端）: %s", ErrPermission, msg)
		}
		return fmt.Errorf("osascript 键入失败: %w: %s", err, msg)
	}
	return nil
}

// TypeWithRetry 在失败时按 MaxRetries 重试；没有权限时重试没有意义，直接返回。
func TypeWithRetry(text string, opts Options) error {
	attempts := opts.MaxRetries
	if attempts <= 0 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		err := Type(text, opts)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrPermission) {
			return err
		}
		lastErr = err
		time.Sleep(time.Duration(150*(i+1)) * time.Millisecond)
	}
	return fmt.Errorf("重试 %d 次后仍失败: %w", attempts, lastErr)
}
