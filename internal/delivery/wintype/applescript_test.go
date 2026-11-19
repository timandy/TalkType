package wintype

import (
	"strings"
	"testing"
	"time"
)

func TestBuildAppleScript(t *testing.T) {
	opts := Options{TypeDelay: 60 * time.Millisecond, EnterDelay: 120 * time.Millisecond, PressEnter: true}
	got := buildAppleScript("你好 \"世界\" \\", opts)
	want := "tell application \"System Events\"\n" +
		"\tkeystroke \"你好 \\\"世界\\\" \\\\\"\n" +
		"\tdelay 0.060\n" +
		"\tdelay 0.120\n" +
		"\tkey code 36\n" +
		"end tell\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildAppleScript_NewlineTabAndControlChars(t *testing.T) {
	got := buildAppleScript("a\r\nb\tc\x01d\re", Options{})
	want := "tell application \"System Events\"\n" +
		"\tkeystroke \"a\"\n\tkey code 36\n" +
		"\tkeystroke \"b\"\n\tkey code 48\n" +
		"\tkeystroke \"cd\"\n\tkey code 36\n" +
		"\tkeystroke \"e\"\n" +
		"end tell\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildAppleScript_NoEnterNoDelay(t *testing.T) {
	got := buildAppleScript("hi", Options{})
	if strings.Contains(got, "delay") || strings.Contains(got, "key code") {
		t.Errorf("不应有延迟和回车:\n%s", got)
	}
}

func TestEscapeAppleScript(t *testing.T) {
	if got := escapeAppleScript(`a"b\c`); got != `a\"b\\c` {
		t.Errorf("got %q", got)
	}
}

func TestIsPermissionError(t *testing.T) {
	for out, want := range map[string]bool{
		"execution error: System Events got an error: osascript is not allowed to send keystrokes. (1002)": true,
		"execution error: “System Events”遇到一个错误：osascript不允许发送按键。 (1002)":                                  true,
		"execution error: Application isn’t running. (-600)":                                               false,
		"": false,
	} {
		if got := isPermissionError(out); got != want {
			t.Errorf("isPermissionError(%q) = %v，期望 %v", out, got, want)
		}
	}
}
