//go:build windows

package wintype

import (
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

const (
	swShowMinNoActive = 7
	swRestore         = 9
	gaRoot            = 2

	foregroundWait = 1500 * time.Millisecond
	focusSettle    = 80 * time.Millisecond
	restoreSettle  = 120 * time.Millisecond
)

var (
	procGetForegroundWindow   = user32DLL.NewProc("GetForegroundWindow")
	procSetForegroundWindow   = user32DLL.NewProc("SetForegroundWindow")
	procBringWindowToTop      = user32DLL.NewProc("BringWindowToTop")
	procIsWindow              = user32DLL.NewProc("IsWindow")
	procIsIconic              = user32DLL.NewProc("IsIconic")
	procShowWindow            = user32DLL.NewProc("ShowWindow")
	procGetAncestor           = user32DLL.NewProc("GetAncestor")
	procGetWindowTextW        = user32DLL.NewProc("GetWindowTextW")
	procGetClassNameW         = user32DLL.NewProc("GetClassNameW")
	procGetWindowThreadProcID = user32DLL.NewProc("GetWindowThreadProcessId")
	procAttachThreadInput     = user32DLL.NewProc("AttachThreadInput")
	procGetCurrentThreadID    = kernel32DLL.NewProc("GetCurrentThreadId")
)

var (
	procGetWindowRect         = user32DLL.NewProc("GetWindowRect")
	procSetWindowPos          = user32DLL.NewProc("SetWindowPos")
	procGetDpiForWindow       = user32DLL.NewProc("GetDpiForWindow")
	procSetThreadDpiAwareness = user32DLL.NewProc("SetThreadDpiAwarenessContext")
	dpiAwarenessPerMonitorV2  = ^uintptr(3) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4
)

type winRect struct{ left, top, right, bottom int32 }

const swpResizeOnly = 0x0002 | 0x0004 | 0x0010 // SWP_NOMOVE | SWP_NOZORDER | SWP_NOACTIVATE

// WindowShrinker 临时缩小窗口：记下原始尺寸，按需把右边和底边同时内移不同距离，最后恢复原样。
// 左上角始终不变。用于让窗口里正在显示的终端收到尺寸变化。
// 宽度和高度都要改：JetBrains 2026.3 的嵌入终端只在宽度变化时通知终端，
// 浮动终端改宽度或高度都会通知。
//
// 调用方需要已经 runtime.LockOSThread，从 BeginShrink 到 Restore 都在同一线程上：
// 期间线程临时切到按显示器 DPI 工作，读写的都是物理像素，恢复时尺寸分毫不差。
type WindowShrinker struct {
	h      uintptr
	w, ht  int32 // 原始宽高（物理像素）
	dpi    int64
	oldCtx uintptr
}

// BeginShrink 记下窗口当前尺寸。窗口最小化或取不到尺寸时返回 false。
func BeginShrink(h uintptr) (*WindowShrinker, bool) {
	if h == 0 || isIconic(h) {
		return nil, false
	}
	s := &WindowShrinker{h: h, dpi: 96}
	if procSetThreadDpiAwareness.Find() == nil {
		s.oldCtx, _, _ = procSetThreadDpiAwareness.Call(dpiAwarenessPerMonitorV2)
	}
	var r winRect
	if res, _, _ := procGetWindowRect.Call(h, uintptr(unsafe.Pointer(&r))); res == 0 || r.right <= r.left || r.bottom <= r.top {
		s.resetThread()
		return nil, false
	}
	s.w, s.ht = r.right-r.left, r.bottom-r.top
	if procGetDpiForWindow.Find() == nil {
		if d, _, _ := procGetDpiForWindow.Call(h); d != 0 {
			s.dpi = int64(d)
		}
	}
	return s, true
}

// Shrink 把右边和底边相对原始位置各内移 by 个逻辑像素（96 DPI 下的像素，按窗口所在显示器的 DPI 换算），
// 每个方向最多内移原始尺寸的一半。返回实际内移的物理像素数（两个方向中较大的一个）。
func (s *WindowShrinker) Shrink(by int32) int32 {
	d := int32((int64(by)*s.dpi + 48) / 96)
	if d <= 0 {
		return 0
	}
	dx, dy := min(d, s.w/2), min(d, s.ht/2)
	procSetWindowPos.Call(s.h, 0, 0, 0, uintptr(s.w-dx), uintptr(s.ht-dy), swpResizeOnly)
	return max(dx, dy)
}

// Restore 恢复原始尺寸，并恢复线程的 DPI 设置。
func (s *WindowShrinker) Restore() {
	procSetWindowPos.Call(s.h, 0, 0, 0, uintptr(s.w), uintptr(s.ht), swpResizeOnly)
	s.resetThread()
}

func (s *WindowShrinker) resetThread() {
	if s.oldCtx != 0 {
		procSetThreadDpiAwareness.Call(s.oldCtx)
	}
}

// ForegroundWindow 返回当前前台窗口句柄，没有时返回 0。
func ForegroundWindow() uintptr {
	h, _, _ := procGetForegroundWindow.Call()
	return h
}

// RootWindow 返回 h 的顶层窗口。
func RootWindow(h uintptr) uintptr {
	if h == 0 {
		return 0
	}
	r, _, _ := procGetAncestor.Call(h, gaRoot)
	if r == 0 {
		return h
	}
	return r
}

// IsWindow 判断窗口句柄是否仍然有效。
func IsWindow(h uintptr) bool {
	if h == 0 {
		return false
	}
	r, _, _ := procIsWindow.Call(h)
	return r != 0
}

// WindowTitle 返回窗口标题。
func WindowTitle(h uintptr) string {
	buf := make([]uint16, 256)
	n, _, _ := procGetWindowTextW.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf[:n])
}

// WindowClass 返回窗口类名。
func WindowClass(h uintptr) string {
	buf := make([]uint16, 256)
	n, _, _ := procGetClassNameW.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf[:n])
}

// WindowPID 返回窗口所属进程 ID。
func WindowPID(h uintptr) uint32 {
	var pid uint32
	procGetWindowThreadProcID.Call(h, uintptr(unsafe.Pointer(&pid)))
	return pid
}

func windowThread(h uintptr) uint32 {
	t, _, _ := procGetWindowThreadProcID.Call(h, 0)
	return uint32(t)
}

func isIconic(h uintptr) bool {
	r, _, _ := procIsIconic.Call(h)
	return r != 0
}

// forceForeground 把 h 切到前台。跨进程时系统会限制 SetForegroundWindow，
// 因此临时把本线程的输入队列与当前前台线程关联后再切换。
// 调用方需要已经 LockOSThread。
func forceForeground(h uintptr) bool {
	if ForegroundWindow() == h {
		return true
	}
	cur, _, _ := procGetCurrentThreadID.Call()
	fgThread := windowThread(ForegroundWindow())
	attached := false
	if fgThread != 0 && uintptr(fgThread) != cur {
		r, _, _ := procAttachThreadInput.Call(cur, uintptr(fgThread), 1)
		attached = r != 0
	}
	procBringWindowToTop.Call(h)
	procSetForegroundWindow.Call(h)
	if attached {
		procAttachThreadInput.Call(cur, uintptr(fgThread), 0)
	}

	deadline := time.Now().Add(foregroundWait)
	for time.Now().Before(deadline) {
		if ForegroundWindow() == h {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return ForegroundWindow() == h
}

// typeToWindow 临时把目标窗口切到前台（最小化则先还原），输入文本后
// 恢复原来的前台窗口，目标原先是最小化的也会重新最小化。
func typeToWindow(h uintptr, text string, opts Options) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	prev := ForegroundWindow()
	wasIconic := isIconic(h)
	if wasIconic {
		procShowWindow.Call(h, swRestore)
	}
	defer func() {
		time.Sleep(restoreSettle)
		if wasIconic && IsWindow(h) {
			procShowWindow.Call(h, swShowMinNoActive)
		}
		if prev != 0 && prev != h && IsWindow(prev) {
			forceForeground(prev)
		}
	}()

	if !forceForeground(h) {
		return fmt.Errorf("无法把目标窗口切到前台")
	}
	time.Sleep(focusSettle)
	return Type(text, opts)
}

// TypeToWindowWithRetry 把文本输入到指定窗口，失败时按 MaxRetries 重试。
// 窗口已关闭时立即返回 ErrTargetClosed，不重试。
func TypeToWindowWithRetry(h uintptr, text string, opts Options) error {
	attempts := opts.MaxRetries
	if attempts <= 0 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if !IsWindow(h) {
			return ErrTargetClosed
		}
		if err := typeToWindow(h, text, opts); err != nil {
			lastErr = err
			time.Sleep(time.Duration(150*(i+1)) * time.Millisecond)
			continue
		}
		return nil
	}
	return fmt.Errorf("重试 %d 次后仍失败: %w", attempts, lastErr)
}
