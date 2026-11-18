//go:build windows

// Package wintype 使用 Windows SendInput 的 Unicode 键事件把文本键入当前前台窗口。
package wintype

import (
	"encoding/binary"
	"fmt"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

const (
	inputKeyboard       = 1
	keyeventfKeyup      = 0x0002
	keyeventfUnicode    = 0x0004
	vkReturn            = 0x0D
	vkTab               = 0x09
	inputBatchSize      = 512
	consoleUTF8CodePage = 65001
)

var (
	user32DLL              = syscall.NewLazyDLL("user32.dll")
	kernel32DLL            = syscall.NewLazyDLL("kernel32.dll")
	procSendInput          = user32DLL.NewProc("SendInput")
	procSetConsoleOutputCP = kernel32DLL.NewProc("SetConsoleOutputCP")
)

// SetupConsole 把控制台输出代码页设置为 UTF-8，避免中文日志乱码。
func SetupConsole() {
	_, _, _ = procSetConsoleOutputCP.Call(consoleUTF8CodePage)
}

// keyEvent 是一个待注入的键盘事件。
type keyEvent struct {
	vk    uint16 // 虚拟键码；Unicode 字符模式为 0
	scan  uint16 // Unicode 模式下为 UTF-16 码元
	flags uint32
}

func keyDown(vk uint16) keyEvent { return keyEvent{vk: vk} }
func keyUp(vk uint16) keyEvent   { return keyEvent{vk: vk, flags: keyeventfKeyup} }

// unicodeEvents 把单个字符转换为按下/抬起两个 KEYEVENTF_UNICODE 事件。
func unicodeEvents(r rune) []keyEvent {
	units := utf16.Encode([]rune{r})
	events := make([]keyEvent, 0, len(units)*2)
	for _, u := range units {
		events = append(events,
			keyEvent{scan: u, flags: keyeventfUnicode},
			keyEvent{scan: u, flags: keyeventfUnicode | keyeventfKeyup},
		)
	}
	return events
}

// buildTextEvents 把文本转换为键事件；换行、制表符使用虚拟键，其余字符使用
// Unicode 码元，因此中文等非 ASCII 文本也能直接输入。
func buildTextEvents(text string) []keyEvent {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	events := make([]keyEvent, 0, len(text)*2)
	for _, r := range text {
		switch r {
		case '\n':
			events = append(events, keyDown(vkReturn), keyUp(vkReturn))
		case '\t':
			events = append(events, keyDown(vkTab), keyUp(vkTab))
		default:
			events = append(events, unicodeEvents(r)...)
		}
	}
	return events
}

// sendInputBatch 一次注入一批事件。INPUT 结构在 64 位下为 40 字节（联合体偏移 8），
// 32 位下为 28 字节（偏移 4）。
func sendInputBatch(events []keyEvent) error {
	if len(events) == 0 {
		return nil
	}
	var inputSize, unionOff int
	if unsafe.Sizeof(uintptr(0)) == 8 {
		inputSize, unionOff = 40, 8
	} else {
		inputSize, unionOff = 28, 4
	}

	buf := make([]byte, len(events)*inputSize)
	for i, ev := range events {
		off := i * inputSize
		binary.LittleEndian.PutUint32(buf[off:off+4], inputKeyboard)
		binary.LittleEndian.PutUint16(buf[off+unionOff:off+unionOff+2], ev.vk)
		binary.LittleEndian.PutUint16(buf[off+unionOff+2:off+unionOff+4], ev.scan)
		binary.LittleEndian.PutUint32(buf[off+unionOff+4:off+unionOff+8], ev.flags)
	}

	r, _, callErr := procSendInput.Call(uintptr(len(events)), uintptr(unsafe.Pointer(&buf[0])), uintptr(inputSize))
	if int(r) != len(events) {
		return fmt.Errorf("SendInput 注入 %d 个事件失败（成功 %d）: %v", len(events), r, callErr)
	}
	return nil
}

func sendEvents(events []keyEvent) error {
	for start := 0; start < len(events); start += inputBatchSize {
		end := start + inputBatchSize
		if end > len(events) {
			end = len(events)
		}
		if err := sendInputBatch(events[start:end]); err != nil {
			return err
		}
		if end < len(events) {
			time.Sleep(5 * time.Millisecond)
		}
	}
	return nil
}

// Type 使用 SendInput 的 KEYEVENTF_UNICODE 把文本直接输入到当前前台窗口，
// 不读写剪贴板；可选在输入完成后发送回车。
func Type(text string, opts Options) error {
	if err := sendEvents(buildTextEvents(text)); err != nil {
		return err
	}
	if opts.TypeDelay > 0 {
		time.Sleep(opts.TypeDelay)
	}
	if opts.PressEnter {
		if opts.EnterDelay > 0 {
			time.Sleep(opts.EnterDelay)
		}
		if err := sendEvents([]keyEvent{keyDown(vkReturn), keyUp(vkReturn)}); err != nil {
			return err
		}
	}
	return nil
}

// TypeWithRetry 在失败时按 MaxRetries 重试。
func TypeWithRetry(text string, opts Options) error {
	attempts := opts.MaxRetries
	if attempts <= 0 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := Type(text, opts); err != nil {
			lastErr = err
			time.Sleep(time.Duration(150*(i+1)) * time.Millisecond)
			continue
		}
		return nil
	}
	return fmt.Errorf("重试 %d 次后仍失败: %w", attempts, lastErr)
}
