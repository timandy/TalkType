// Package winlock 提供“用快捷键锁定目标”的功能：
//   - 未锁定时文本输入到当前光标处；
//   - 锁定终端（PowerShell、cmd、Windows Terminal、JetBrains/VS Code 终端）时，
//     在后台直接写入它的控制台，不切换窗口；
//   - 锁定其他窗口时，发送瞬间把它切到前台输入，再切回来；
//   - 锁定的目标关闭后不再发送。
//
// 快捷键：
//   - Alt+. ：未锁定时锁定当前目标；已锁定时，当前目标就是锁定的那个则解除，
//     否则把锁定切换到当前目标；
//   - Alt+/ ：无论在哪里，直接解除锁定。
//
// 左右两个 Alt 键都可以。
//
// 状态变化在屏幕右下角弹出提示卡片。
package winlock

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"wetype/internal/wintype"
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

// same 判断按下 Alt+. 时解析出的目标 next 是否就是当前锁定的 t。
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
	notify func(Notice)

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

// Unlock 解除锁定，恢复为输入到当前光标。Alt+/ 和 -unlock 都走这里。
func (m *Manager) Unlock() {
	m.mu.Lock()
	was := m.locked
	m.locked, m.tgt = false, target{}
	m.mu.Unlock()
	if was {
		m.notifyAs(LevelInfo, "已解除锁定", "消息将输入到当前光标位置")
	} else {
		m.notifyAs(LevelInfo, "未锁定任何目标", "消息将输入到当前光标位置")
	}
}

// 通知正文按行组织：目标、发送方式、操作提示各占一行，每行不超过卡片宽度。
const (
	lineBackground = "方式：后台发送，不切换窗口"
	lineForeground = "方式：切换至该窗口后发送"
	lineUnlock     = "按 Alt+/ 解除锁定"
)

// Toggle 按当前前台窗口处理一次 Alt+. 。
func (m *Manager) Toggle() { m.press(m.foreground()) }

// press 处理一次 Alt+. ，h 是按下时的前台窗口。
func (m *Manager) press(h uintptr) {
	if h == 0 {
		m.notifyAs(LevelWarning, "未检测到活动窗口", "请先激活目标窗口，再按 Alt+.")
		return
	}
	title, class, pid := m.describe(h)
	if pid == m.ownPID || protectedClasses[class] {
		m.notifyAs(LevelWarning, "无法锁定此窗口", "桌面、任务栏和本程序窗口不支持锁定")
		return
	}
	winName := displayName(title)

	c, n := m.resolve(h, pid)
	if n < 0 {
		m.notifyAs(LevelWarning, "无法锁定此终端", "本程序所在的终端不支持锁定\n请切换至其他窗口或终端标签")
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

	prefix := "已锁定"
	if locked {
		prefix = "已切换至"
	}
	switch {
	case n == 1:
		m.notifyAs(LevelSuccess, prefix+"终端", lockedBody(consoleName(winName, c), lineBackground, lineUnlock))
	case n > 1:
		m.notifyAs(LevelSuccess, prefix+"窗口", lockedBody(winName, lineForeground,
			fmt.Sprintf("检测到 %d 个可见终端，无法确定具体目标", n), "如需后台发送，请在目标终端中以 -lock 参数运行本程序"))
	default:
		m.notifyAs(LevelSuccess, prefix+"窗口", lockedBody(winName, lineForeground, lineUnlock))
	}
}

// lockedBody 组成锁定通知的正文：第一行是目标，之后每项一行。
func lockedBody(name string, lines ...string) string {
	return strings.Join(append([]string{"目标：" + name}, lines...), "\n")
}

// LockConsolePID 锁定 pid 所在的终端会话，供 -lock 命令使用。
func (m *Manager) LockConsolePID(pid uint32) {
	c, err := m.consoleOf(pid)
	if err != nil {
		m.notifyAs(LevelError, "终端锁定失败", fmt.Sprintf("无法后台写入此终端：%v", err))
		return
	}
	m.set(target{kind: kindConsole, pid: c.PID, start: c.Start})
	m.notifyAs(LevelSuccess, "已锁定终端", lockedBody(consoleName("", c), lineBackground, lineUnlock))
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

// Level 是通知的样式，决定提示卡片的颜色和图标。
type Level int

const (
	LevelInfo    Level = iota // 蓝色：一般状态，例如解除锁定
	LevelSuccess              // 绿色：锁定成功
	LevelWarning              // 橙色：没有锁定、消息没有发送
	LevelError                // 红色：操作失败
)

// Notice 是一条通知。
type Notice struct {
	Level      Level
	Title, Msg string
}

// SetNotifier 设置通知函数，nil 表示不再通知。
func (m *Manager) SetNotifier(fn func(Notice)) {
	m.mu.Lock()
	m.notify = fn
	m.mu.Unlock()
}

// Notify 弹出一条提醒（橙色）。供投递器在锁定目标已关闭时调用。
func (m *Manager) Notify(title, msg string) { m.notifyAs(LevelWarning, title, msg) }

// notifyAs 在屏幕右下角弹出提示卡片；没有可用通知通道时忽略。
func (m *Manager) notifyAs(level Level, title, msg string) {
	m.post(Notice{Level: level, Title: title, Msg: msg})
}

func (m *Manager) post(n Notice) {
	m.mu.Lock()
	fn := m.notify
	m.mu.Unlock()
	if fn != nil {
		fn(n)
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
