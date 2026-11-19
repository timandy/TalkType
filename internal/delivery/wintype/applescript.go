package wintype

import (
	"errors"
	"fmt"
	"strings"
)

// macOS 键入通过 osascript 驱动 System Events 完成。脚本的生成与错误识别不依赖平台，
// 放在这里，方便在任何平台上测试；真正执行 osascript 的代码在 input_darwin.go。

const (
	keyCodeReturn = 36 // macOS 虚拟键码：回车
	keyCodeTab    = 48 // macOS 虚拟键码：Tab
)

// ErrPermission 表示没有辅助功能权限，System Events 不允许发送按键。重试没有意义。
var ErrPermission = errors.New("wintype: 没有辅助功能权限")

// escapeAppleScript 转义 AppleScript 字符串字面量里的反斜杠和双引号。
func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

// buildAppleScript 生成键入 text 的 AppleScript：换行、制表符使用按键，其余字符交给
// keystroke（支持中文等非 ASCII 文本，不读写剪贴板）；按 opts 在输入后等待，并可选发送回车。
// 其他控制字符无法键入，直接丢弃。
func buildAppleScript(text string, opts Options) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	var b strings.Builder
	b.WriteString("tell application \"System Events\"\n")
	var seg strings.Builder
	flush := func() {
		if seg.Len() > 0 {
			fmt.Fprintf(&b, "\tkeystroke \"%s\"\n", escapeAppleScript(seg.String()))
			seg.Reset()
		}
	}
	for _, r := range text {
		switch {
		case r == '\n':
			flush()
			fmt.Fprintf(&b, "\tkey code %d\n", keyCodeReturn)
		case r == '\t':
			flush()
			fmt.Fprintf(&b, "\tkey code %d\n", keyCodeTab)
		case r < 0x20 || r == 0x7f:
			// 其他控制字符：丢弃
		default:
			seg.WriteRune(r)
		}
	}
	flush()
	if opts.TypeDelay > 0 {
		fmt.Fprintf(&b, "\tdelay %.3f\n", opts.TypeDelay.Seconds())
	}
	if opts.PressEnter {
		if opts.EnterDelay > 0 {
			fmt.Fprintf(&b, "\tdelay %.3f\n", opts.EnterDelay.Seconds())
		}
		fmt.Fprintf(&b, "\tkey code %d\n", keyCodeReturn)
	}
	b.WriteString("end tell\n")
	return b.String()
}

// isPermissionError 判断 osascript 的输出是不是“没有辅助功能权限”。
func isPermissionError(out string) bool {
	out = strings.ToLower(out)
	for _, s := range []string{"(1002)", "(-1719)", "not allowed to send keystrokes", "not allowed assistive access", "不允许发送按键", "不允许辅助访问"} {
		if strings.Contains(out, s) {
			return true
		}
	}
	return false
}
