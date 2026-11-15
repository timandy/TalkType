// Package winlock 提供“用 Alt+/ 锁定目标”的功能：
//   - 未锁定时文本输入到当前光标处；
//   - 锁定终端（PowerShell、cmd、Windows Terminal、JetBrains/VS Code 终端）时，
//     在后台直接写入它的控制台，不切换窗口；
//   - 锁定其他窗口时，发送瞬间把它切到前台输入，再切回来；
//   - 锁定的目标关闭后不再发送。
//
// 快捷键：
//   - Alt+/ ：未锁定时锁定当前目标；已锁定时，当前目标就是锁定的那个则解除，
//     否则把锁定切换到当前目标；
//   - 快速连按两下 Alt+/ ：无论在哪里，直接解除锁定。
//
// 状态变化通过 Windows 通知栏提示。
package winlock

import (
	"fmt"
	"log"
	"os"
	"sync"

	"clawbot-ilink-relay/internal/wintype"
)

const maxTitleRunes = 40

// logf 输出诊断日志，默认丢弃；用 SetLogger 打开。
var logf = func(string, ...any) {}

// SetLogger 设置诊断日志输出，例如锁定终端时各候选标签的最近输入时间。
func SetLogger(l *log.Logger) {
	if l == nil {
		logf = func(string, ...any) {}
		return
	}
	logf = l.Printf
}

// 不允许锁定的系统窗口类名（桌面、任务栏）。
var protectedClasses = map[string]bool{
	"Progman":                true,
	"WorkerW":                true,
	"Shell_TrayWnd":          true,
	"Shell_SecondaryTrayWnd": true,
}

type targetKind int

const (
	kindWindow  targetKind = iota + 1 // 切前台输入
	kindConsole                       // 后台写控制台
)

type target struct {
	kind  targetKind
	hwnd  uintptr // 窗口目标的窗口；终端目标锁定时所在的窗口（-lock 锁定时为 0）
	pid   uint32
	start uint64
}

// same 判断按下 Alt+/ 时解析出的目标 next 是否就是当前锁定的 t。
func (t target) same(next target) bool {
	switch {
	case t.kind == kindConsole && next.kind == kindConsole:
		return t.pid == next.pid && t.start == next.start
	case t.kind == kindWindow && next.kind == kindWindow:
		return t.hwnd == next.hwnd
	case t.kind == kindConsole && next.kind == kindWindow:
		// 锁定的终端所在窗口里分不清是哪个标签时，按同一个窗口处理。
		return t.hwnd != 0 && t.hwnd == next.hwnd
	}
	return false
}

// consoleMatch 是解析出来的一个控制台会话。
type consoleMatch struct {
	PID   uint32
	Start uint64
	Title string
	Line  string // 光标所在行，通常是提示符，用来让用户认出是哪个标签
}

// Manager 保存锁定状态，并决定文本该输入到哪里。
type Manager struct {
	mu     sync.Mutex
	locked bool
	tgt    target
	notify func(title, msg string)

	// 以下为可替换的系统调用，便于测试。
	ownPID      uint32
	foreground  func() uintptr
	isWindow    func(uintptr) bool
	describe    func(uintptr) (title, class string, pid uint32)
	resolve     func(hwnd uintptr, pid uint32) (consoleMatch, int) // int: 匹配到的控制台数量，-1 表示本程序自己的终端
	consoleOf   func(pid uint32) (consoleMatch, error)
	alive       func(pid uint32, start uint64) bool
	typeCursor  func(string, wintype.Options) error
	typeTo      func(uintptr, string, wintype.Options) error
	typeConsole func(uint32, string, wintype.Options) error
}

// New 创建使用真实系统调用的 Manager。
func New() *Manager {
	return &Manager{
		ownPID: uint32(os.Getpid()),
		foreground: func() uintptr {
			return wintype.RootWindow(wintype.ForegroundWindow())
		},
		isWindow: wintype.IsWindow,
		describe: func(h uintptr) (string, string, uint32) {
			return wintype.WindowTitle(h), wintype.WindowClass(h), wintype.WindowPID(h)
		},
		resolve:     resolveConsole,
		consoleOf:   consoleOfPID,
		alive:       wintype.ProcessAlive,
		typeCursor:  wintype.TypeWithRetry,
		typeTo:      wintype.TypeToWindowWithRetry,
		typeConsole: wintype.TypeToConsoleWithRetry,
	}
}

// IsLocked 报告当前是否处于锁定状态。
func (m *Manager) IsLocked() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.locked
}

func (m *Manager) current() (target, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tgt, m.locked
}

func (m *Manager) set(t target) {
	m.mu.Lock()
	m.locked, m.tgt = true, t
	m.mu.Unlock()
}

// Unlock 解除锁定，恢复为输入到当前光标。连按两下 Alt+/ 和 -unlock 都走这里。
func (m *Manager) Unlock() {
	m.mu.Lock()
	was := m.locked
	m.locked, m.tgt = false, target{}
	m.mu.Unlock()
	if was {
		m.Notify("已解除锁定", "已恢复为输入到当前光标位置。")
	} else {
		m.Notify("当前没有锁定", "消息会输入到当前光标位置。")
	}
}

// Toggle 按当前前台窗口处理一次 Alt+/ 单击。
func (m *Manager) Toggle() { m.press(m.foreground()) }

// press 处理一次 Alt+/ 单击，h 是按下时的前台窗口。
func (m *Manager) press(h uintptr) {
	if h == 0 {
		m.Notify("没找到当前窗口", "请点一下要锁定的窗口后再按 Alt+/ 。连按两下 Alt+/ 可解除锁定。")
		return
	}
	title, class, pid := m.describe(h)
	if pid == m.ownPID || protectedClasses[class] {
		m.Notify("这个窗口不能锁定", "请先切换到要接收消息的窗口，再按 Alt+/ 。连按两下 Alt+/ 可解除锁定。")
		return
	}
	winName := displayName(title)

	c, n := m.resolve(h, pid)
	if n < 0 {
		m.Notify("这个终端不能锁定", "这是 clawbot-ilink-relay 自己所在的终端。请切到别的窗口或标签再按 Alt+/ ，"+
			"或在目标终端里运行 clawbot-ilink-relay -lock 。")
		return
	}
	next := target{kind: kindWindow, hwnd: h}
	if n == 1 {
		next = target{kind: kindConsole, hwnd: h, pid: c.PID, start: c.Start}
	}

	cur, locked := m.current()
	if locked && cur.same(next) {
		m.Unlock()
		return
	}
	m.set(next)

	verb := "已锁定"
	if locked {
		verb = "已切换到"
	}
	const tail = "在这里再按一次 Alt+/ 可解除。"
	switch {
	case n == 1:
		m.Notify(verb+"当前终端", fmt.Sprintf("消息会在后台发送到「%s」，不会切换窗口。%s",
			consoleName(winName, c), tail))
	case n > 1:
		m.Notify(verb+"当前窗口", fmt.Sprintf("「%s」里有 %d 个终端，分不清是哪一个，发送时会短暂切到该窗口。"+
			"在目标终端里随便按个键再按 Alt+/ ，就能锁定到它。", winName, n))
	default:
		m.Notify(verb+"当前窗口", fmt.Sprintf("消息只会发送到「%s」，发送时会短暂切换到该窗口。%s", winName, tail))
	}
}

// LockConsolePID 锁定 pid 所在的终端会话，供 -lock 命令使用。
func (m *Manager) LockConsolePID(pid uint32) {
	c, err := m.consoleOf(pid)
	if err != nil {
		m.Notify("没能锁定终端", fmt.Sprintf("这个终端无法后台写入：%v", err))
		return
	}
	m.set(target{kind: kindConsole, pid: c.PID, start: c.Start})
	m.Notify("已锁定这个终端", fmt.Sprintf("消息会在后台发送到「%s」，不会切换窗口。连按两下 Alt+/ 可解除。",
		consoleName("", c)))
}

// Inject 按当前锁定状态输入文本。锁定的目标已关闭时返回 wintype.ErrTargetClosed。
func (m *Manager) Inject(text string, opts wintype.Options) error {
	t, locked := m.current()
	if !locked {
		return m.typeCursor(text, opts)
	}
	if t.kind == kindConsole {
		if !m.alive(t.pid, t.start) {
			return wintype.ErrTargetClosed
		}
		return m.typeConsole(t.pid, text, opts)
	}
	if !m.isWindow(t.hwnd) {
		return wintype.ErrTargetClosed
	}
	return m.typeTo(t.hwnd, text, opts)
}

// SetNotifier 设置通知函数，nil 表示不再通知。
func (m *Manager) SetNotifier(fn func(title, msg string)) {
	m.mu.Lock()
	m.notify = fn
	m.mu.Unlock()
}

// Notify 在 Windows 通知栏弹出提示；没有可用通知通道时忽略。
func (m *Manager) Notify(title, msg string) {
	m.mu.Lock()
	fn := m.notify
	m.mu.Unlock()
	if fn != nil {
		fn(title, msg)
	}
}

func displayName(title string) string {
	if title == "" {
		return "无标题窗口"
	}
	r := []rune(title)
	if len(r) > maxTitleRunes {
		return string(r[:maxTitleRunes]) + "…"
	}
	return title
}

// consoleName 组合窗口名和终端信息，让用户认出锁定的是哪个标签：
// 优先用光标所在行（通常是带目录的提示符），没有时用终端标题；与窗口名相同的部分不重复。
func consoleName(winName string, c consoleMatch) string {
	detail := c.Line
	if detail == "" {
		detail = c.Title
	}
	switch {
	case detail == "" && winName == "":
		return "无标题终端"
	case detail == "":
		return winName
	}
	d := displayName(detail)
	if winName == "" || d == winName {
		return d
	}
	return winName + " · " + d
}
