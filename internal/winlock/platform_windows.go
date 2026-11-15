//go:build windows

package winlock

import (
	"context"
	"encoding/binary"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	wmDestroy  = 0x0002
	wmClose    = 0x0010
	wmHotkey   = 0x0312
	wmTimer    = 0x0113
	wmCopyData = 0x004A
	wmNotify   = 0x8000 + 1 // WM_APP+1：让 UI 线程弹出排队的提示卡片

	modAlt      = 0x0001
	modNoRepeat = 0x4000
	vkOem2      = 0xBF // 美式键盘上的 “/ ?” 键
	hotkeyID    = 1
	tapTimerID  = 1

	// doubleTapWindow 内第二次按下 Alt+/ 算作连按两下。单击要等这段时间确认不是连按，
	// 所以单击会有这么长的延迟。
	doubleTapWindow = 350 * time.Millisecond

	copyLock   = 1
	copyUnlock = 2
	className  = "ClawbotIlinkRelayHost"
)

var hwndMessage = ^uintptr(2) // HWND_MESSAGE = -3

var (
	user32               = syscall.NewLazyDLL("user32.dll")
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procRegisterClassExW = user32.NewProc("RegisterClassExW")
	procUnregisterClassW = user32.NewProc("UnregisterClassW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procPostMessageW     = user32.NewProc("PostMessageW")
	procSendMessageW     = user32.NewProc("SendMessageW")
	procFindWindowExW    = user32.NewProc("FindWindowExW")
	procSetTimer         = user32.NewProc("SetTimer")
	procKillTimer        = user32.NewProc("KillTimer")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procRegisterHotKey   = user32.NewProc("RegisterHotKey")
	procUnregisterHotKey = user32.NewProc("UnregisterHotKey")
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
)

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type point struct{ x, y int32 }

type winMsg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

type copyDataStruct struct {
	dwData uintptr
	cbData uint32
	lpData uintptr
}

type notice struct {
	level      Level
	title, msg string
}

// host 是承载热键和提示卡片的隐藏窗口，所有 UI 调用都在它所在的线程上完成。
type host struct {
	m    *Manager
	hwnd uintptr

	mu    sync.Mutex
	queue []notice

	toasts toaster

	// 只在窗口线程上访问。
	tapPending bool
	tapWindow  uintptr // 第一次按下时的前台窗口
}

// 窗口过程是进程级回调，只会有一个 host 实例。
var active *host

// enqueue 可在任意 goroutine 调用：把通知排队，再让 UI 线程弹出。
func (h *host) enqueue(level Level, title, msg string) {
	h.mu.Lock()
	h.queue = append(h.queue, notice{level, title, msg})
	h.mu.Unlock()
	procPostMessageW.Call(h.hwnd, wmNotify, 0, 0)
}

func (h *host) drain() {
	h.mu.Lock()
	q := h.queue
	h.queue = nil
	h.mu.Unlock()
	for _, n := range q {
		h.toasts.show(n.level, n.title, n.msg)
	}
}

// onHotkey 区分单击和连按：第一次按下时记住前台窗口并等待 doubleTapWindow，
// 期间再按一次就是连按（解除锁定），否则按单击处理。
func (h *host) onHotkey() {
	if h.tapPending {
		procKillTimer.Call(h.hwnd, tapTimerID)
		h.tapPending = false
		h.m.Unlock()
		return
	}
	h.tapPending = true
	h.tapWindow = h.m.foreground()
	procSetTimer.Call(h.hwnd, tapTimerID, uintptr(doubleTapWindow.Milliseconds()), 0)
}

func (h *host) onTapTimeout() {
	procKillTimer.Call(h.hwnd, tapTimerID)
	if !h.tapPending {
		return
	}
	h.tapPending = false
	h.m.press(h.tapWindow)
}

func wndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	h := active
	switch msg {
	case wmHotkey:
		if h != nil && wParam == hotkeyID {
			h.onHotkey()
		}
		return 0
	case wmTimer:
		if h != nil && wParam == tapTimerID {
			h.onTapTimeout()
		}
		if h != nil && wParam == toastTimerID {
			h.toasts.tick()
		}
		return 0
	case wmCopyData:
		if h == nil || lParam == 0 {
			return 0
		}
		// lParam 是系统传入的 COPYDATASTRUCT 指针，在本次回调期间有效。
		cds := (*copyDataStruct)(unsafe.Add(unsafe.Pointer(nil), lParam))
		switch cds.dwData {
		case copyLock:
			if cds.cbData != 4 || cds.lpData == 0 {
				return 0
			}
			pid := *(*uint32)(unsafe.Add(unsafe.Pointer(nil), cds.lpData))
			h.m.LockConsolePID(pid)
		case copyUnlock:
			h.m.Unlock()
		default:
			return 0
		}
		return 1
	case wmNotify:
		if h != nil {
			h.drain()
		}
		return 0
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		if h != nil {
			h.m.SetNotifier(nil)
			procUnregisterHotKey.Call(hwnd, hotkeyID)
			h.toasts.closeAll()
		}
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

// Start 注册 Alt+/ 全局热键并准备提示卡片，在 ctx 结束时清理。
// 热键被其他程序占用时返回错误，此时锁定功能不可用，其余功能不受影响。
func (m *Manager) Start(ctx context.Context) error {
	ready := make(chan error, 1)
	go m.run(ctx, ready)
	return <-ready
}

func (m *Manager) run(ctx context.Context, ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// 本线程按显示器 DPI 工作：提示卡片按实际 DPI 渲染，不会被系统拉伸变糊。
	if procSetThreadDpiCtx.Find() == nil {
		procSetThreadDpiCtx.Call(dpiPerMonitorV2)
	}

	hInst, _, _ := procGetModuleHandleW.Call(0)
	registerToastClass(hInst)
	cls, _ := syscall.UTF16PtrFromString(className)
	wc := wndClassEx{
		lpfnWndProc:   syscall.NewCallback(wndProc),
		hInstance:     hInst,
		lpszClassName: cls,
	}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		ready <- fmt.Errorf("注册窗口类失败: %v", err)
		return
	}
	defer procUnregisterClassW.Call(uintptr(unsafe.Pointer(cls)), hInst)

	hwnd, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(cls)),
		0, 0, 0, 0, 0, hwndMessage, 0, hInst, 0)
	if hwnd == 0 {
		ready <- fmt.Errorf("创建消息窗口失败: %v", err)
		return
	}

	h := &host{m: m, hwnd: hwnd}
	h.toasts.host = hwnd
	active = h
	defer func() { active = nil }()

	if r, _, err := procRegisterHotKey.Call(hwnd, hotkeyID, modAlt|modNoRepeat, vkOem2); r == 0 {
		procDestroyWindow.Call(hwnd)
		ready <- fmt.Errorf("注册 Alt+/ 失败（可能已被其他程序占用）: %v", err)
		return
	}
	m.SetNotifier(h.enqueue)
	ready <- nil

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			procPostMessageW.Call(hwnd, wmClose, 0, 0)
		case <-done:
		}
	}()

	var msg winMsg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func sendCommand(kind uintptr, data []byte) error {
	cls, _ := syscall.UTF16PtrFromString(className)
	hwnd, _, _ := procFindWindowExW.Call(hwndMessage, 0, uintptr(unsafe.Pointer(cls)), 0)
	if hwnd == 0 {
		return fmt.Errorf("未找到正在运行的本程序实例")
	}
	cds := copyDataStruct{dwData: kind, cbData: uint32(len(data))}
	if len(data) > 0 {
		cds.lpData = uintptr(unsafe.Pointer(&data[0]))
	}
	r, _, _ := procSendMessageW.Call(hwnd, wmCopyData, 0, uintptr(unsafe.Pointer(&cds)))
	runtime.KeepAlive(data)
	if r == 0 {
		return fmt.Errorf("正在运行的本程序实例未接受请求")
	}
	return nil
}

// SendLock 通知正在运行的 clawbot-ilink-relay 锁定 pid 所在的终端。
func SendLock(pid uint32) error {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, pid)
	return sendCommand(copyLock, b)
}

// SendUnlock 通知正在运行的 clawbot-ilink-relay 解除锁定。
func SendUnlock() error { return sendCommand(copyUnlock, nil) }
