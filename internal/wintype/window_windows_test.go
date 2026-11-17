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

func TestWindowShrinker(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	h := newHiddenWindow(t, 800, 600)
	orig := rectOf(h)

	s, ok := BeginShrink(h)
	if !ok {
		t.Fatal("BeginShrink failed")
	}
	for _, by := range []int32{24, 64, 120} {
		d := s.Shrink(by)
		r := rectOf(h)
		if d <= 0 || r.bottom != orig.bottom-d || r.right != orig.right-d {
			t.Errorf("Shrink(%d): d=%d rect=%+v, orig %+v", by, d, r, orig)
		}
		if r.left != orig.left || r.top != orig.top {
			t.Errorf("Shrink(%d) moved top-left: %+v vs %+v", by, r, orig)
		}
	}
	// 每一级都相对原始尺寸计算，而不是在上一级的基础上继续缩
	if d := s.Shrink(24); rectOf(h).bottom != orig.bottom-d {
		t.Error("shrink should be relative to the original size")
	}
	// 每个方向最多内移原始尺寸的一半
	s.Shrink(10000)
	if r := rectOf(h); r.bottom-r.top != (orig.bottom-orig.top)-(orig.bottom-orig.top)/2 ||
		r.right-r.left != (orig.right-orig.left)-(orig.right-orig.left)/2 {
		t.Errorf("Shrink should be capped at half size, got %+v", r)
	}
	s.Restore()
	if got := rectOf(h); got != orig {
		t.Errorf("Restore: %+v, want %+v", got, orig)
	}
}

func TestBeginShrinkRejectsInvalidWindow(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if _, ok := BeginShrink(0); ok {
		t.Error("window 0 should be rejected")
	}
}
