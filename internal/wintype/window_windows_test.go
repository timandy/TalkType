//go:build windows

package wintype

import (
	"runtime"
	"syscall"
	"testing"
	"unsafe"
)

var (
	procCreateWindowExW = user32DLL.NewProc("CreateWindowExW")
	procDestroyWindow   = user32DLL.NewProc("DestroyWindow")
)

const wsOverlappedWindow = 0x00CF0000

// newHiddenWindow 创建一个不显示的顶层窗口，调整它的尺寸不会出现在屏幕上。
func newHiddenWindow(t *testing.T, w, h int32) uintptr {
	t.Helper()
	cls, _ := syscall.UTF16PtrFromString("STATIC")
	hwnd, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)), 0, wsOverlappedWindow,
		100, 100, uintptr(w), uintptr(h), 0, 0, 0, 0)
	if hwnd == 0 {
		t.Fatalf("CreateWindowEx: %v", err)
	}
	t.Cleanup(func() { procDestroyWindow.Call(hwnd) })
	return hwnd
}

func rectOf(h uintptr) winRect {
	var r winRect
	procGetWindowRect.Call(h, uintptr(unsafe.Pointer(&r)))
	return r
}

func TestHeightShrinker(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	h := newHiddenWindow(t, 800, 600)
	orig := rectOf(h)

	s, ok := BeginShrinkHeight(h)
	if !ok {
		t.Fatal("BeginShrinkHeight failed")
	}
	for _, by := range []int32{24, 64, 120} {
		dy := s.Shrink(by)
		r := rectOf(h)
		if dy <= 0 || r.bottom != orig.bottom-dy {
			t.Errorf("Shrink(%d): dy=%d bottom=%d, want %d", by, dy, r.bottom, orig.bottom-dy)
		}
		if r.left != orig.left || r.top != orig.top || r.right != orig.right {
			t.Errorf("Shrink(%d) moved other edges: %+v vs %+v", by, r, orig)
		}
	}
	// 每一级都相对原始尺寸计算，而不是在上一级的基础上继续缩
	if dy := s.Shrink(24); rectOf(h).bottom != orig.bottom-dy {
		t.Error("shrink should be relative to the original size")
	}
	// 最多上移原始高度的一半
	if dy := s.Shrink(10000); dy != (orig.bottom-orig.top)/2 {
		t.Errorf("Shrink should be capped at half height, got %d", dy)
	}
	s.Restore()
	if got := rectOf(h); got != orig {
		t.Errorf("Restore: %+v, want %+v", got, orig)
	}
}

func TestBeginShrinkHeightRejectsInvalidWindow(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if _, ok := BeginShrinkHeight(0); ok {
		t.Error("window 0 should be rejected")
	}
}
