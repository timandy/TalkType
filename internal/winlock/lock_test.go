package winlock

import (
	"errors"
	"strings"
	"testing"

	"clawbot-ilink-relay/internal/wintype"
)

type fixture struct {
	m        *Manager
	fg       uintptr
	alive    map[uintptr]bool
	titles   map[uintptr]string
	classes  map[uintptr]string
	pids     map[uintptr]uint32
	consoles map[uintptr][]consoleMatch // 每个窗口里的终端会话
	procs    map[uint32]bool            // 存活的进程
	notes    [][2]string
	cursor   []string
	windowed []uintptr
	consoled []uint32
	typeErr  error
}

func newFixture() *fixture {
	f := &fixture{
		alive:    map[uintptr]bool{},
		titles:   map[uintptr]string{},
		classes:  map[uintptr]string{},
		pids:     map[uintptr]uint32{},
		consoles: map[uintptr][]consoleMatch{},
		procs:    map[uint32]bool{},
	}
	f.m = &Manager{
		ownPID:     100,
		foreground: func() uintptr { return f.fg },
		isWindow:   func(h uintptr) bool { return f.alive[h] },
		describe:   func(h uintptr) (string, string, uint32) { return f.titles[h], f.classes[h], f.pids[h] },
		resolve: func(h uintptr, _ uint32) (consoleMatch, int) {
			cs := f.consoles[h]
			if len(cs) == 1 {
				return cs[0], 1
			}
			return consoleMatch{}, len(cs)
		},
		consoleOf: func(pid uint32) (consoleMatch, error) {
			if !f.procs[pid] {
				return consoleMatch{}, errors.New("no console")
			}
			return consoleMatch{PID: pid, Start: 1, Title: "PowerShell"}, nil
		},
		alive:      func(pid uint32, _ uint64) bool { return f.procs[pid] },
		typeCursor: func(s string, _ wintype.Options) error { f.cursor = append(f.cursor, s); return f.typeErr },
		typeTo: func(h uintptr, _ string, _ wintype.Options) error {
			f.windowed = append(f.windowed, h)
			return f.typeErr
		},
		typeConsole: func(pid uint32, _ string, _ wintype.Options) error {
			f.consoled = append(f.consoled, pid)
			return f.typeErr
		},
	}
	f.m.SetNotifier(func(title, msg string) { f.notes = append(f.notes, [2]string{title, msg}) })
	return f
}

func (f *fixture) addWindow(h uintptr, title string) {
	f.alive[h], f.titles[h], f.pids[h] = true, title, 200
}

func (f *fixture) addConsole(h uintptr, pid uint32, title string) {
	f.consoles[h] = append(f.consoles[h], consoleMatch{PID: pid, Start: 1, Title: title})
	f.procs[pid] = true
}

func (f *fixture) lastNote() [2]string { return f.notes[len(f.notes)-1] }

func TestToggle_LockWindowAndUnlockInSameWindow(t *testing.T) {
	f := newFixture()
	f.addWindow(1, "Chrome")
	f.fg = 1

	f.m.Toggle()
	if !f.m.IsLocked() || f.m.tgt.kind != kindWindow || f.m.tgt.hwnd != 1 {
		t.Fatalf("tgt = %+v", f.m.tgt)
	}
	if n := f.lastNote(); n[0] != "已锁定当前窗口" || !strings.Contains(n[1], "Chrome") || !strings.Contains(n[1], "切换") {
		t.Errorf("note = %v", n)
	}

	// 在同一个窗口再按一次：解除
	f.m.Toggle()
	if f.m.IsLocked() {
		t.Fatal("should be unlocked")
	}
	if n := f.lastNote(); n[0] != "已解除锁定" || !strings.Contains(n[1], "光标") {
		t.Errorf("note = %v", n)
	}
}

func TestToggle_OtherWindowSwitchesLock(t *testing.T) {
	f := newFixture()
	f.addWindow(1, "Chrome")
	f.addWindow(2, "Notepad")
	f.fg = 1
	f.m.Toggle()

	f.fg = 2
	f.m.Toggle()
	if !f.m.IsLocked() || f.m.tgt.hwnd != 2 {
		t.Fatalf("tgt = %+v", f.m.tgt)
	}
	if n := f.lastNote(); n[0] != "已切换到当前窗口" || !strings.Contains(n[1], "Notepad") {
		t.Errorf("note = %v", n)
	}
}

func TestToggle_SwitchBetweenTerminalTabsAndBack(t *testing.T) {
	f := newFixture()
	f.addWindow(1, "IDEA")
	f.fg = 1
	active := consoleMatch{PID: 42, Start: 1, Line: `PS E:\a>`}
	f.procs[42], f.procs[43] = true, true
	f.m.resolve = func(uintptr, uint32) (consoleMatch, int) { return active, 1 }

	f.m.Toggle()
	if f.m.tgt.pid != 42 || f.lastNote()[0] != "已锁定当前终端" || !strings.Contains(f.lastNote()[1], `IDEA · PS E:\a>`) {
		t.Fatalf("tgt=%+v note=%v", f.m.tgt, f.lastNote())
	}

	// 同一窗口里换了一个标签：切换，而不是解除
	active = consoleMatch{PID: 43, Start: 1, Line: `PS E:\b>`}
	f.m.Toggle()
	if !f.m.IsLocked() || f.m.tgt.pid != 43 || f.lastNote()[0] != "已切换到当前终端" {
		t.Fatalf("tgt=%+v note=%v", f.m.tgt, f.lastNote())
	}

	// 还在这个标签：解除
	f.m.Toggle()
	if f.m.IsLocked() {
		t.Fatal("should be unlocked")
	}
}

func TestToggle_SameWindowWhenTabUnknownUnlocks(t *testing.T) {
	f := newFixture()
	f.addWindow(1, "IDEA")
	f.fg = 1
	f.procs[42] = true
	n := 1
	f.m.resolve = func(uintptr, uint32) (consoleMatch, int) {
		if n == 1 {
			return consoleMatch{PID: 42, Start: 1}, 1
		}
		return consoleMatch{}, n
	}
	f.m.Toggle()
	n = 2 // 第二次按时分不清是哪个标签，但还是同一个窗口
	f.m.Toggle()
	if f.m.IsLocked() {
		t.Errorf("same window should unlock, tgt=%+v", f.m.tgt)
	}
}

func TestUnlock_DoubleTapFromAnyWindow(t *testing.T) {
	f := newFixture()
	f.addWindow(1, "Chrome")
	f.fg = 1
	f.m.Toggle()
	f.fg = 2 // 在别的窗口连按两下
	f.m.Unlock()
	if f.m.IsLocked() || f.lastNote()[0] != "已解除锁定" {
		t.Errorf("locked=%v note=%v", f.m.IsLocked(), f.lastNote())
	}
}
func TestToggle_LockTerminalInBackground(t *testing.T) {
	f := newFixture()
	f.addWindow(1, "GoLand")
	f.addConsole(1, 42, "Windows PowerShell")
	f.fg = 1

	f.m.Toggle()
	if f.m.tgt.kind != kindConsole || f.m.tgt.pid != 42 {
		t.Fatalf("tgt = %+v", f.m.tgt)
	}
	n := f.lastNote()
	if n[0] != "已锁定当前终端" || !strings.Contains(n[1], "GoLand · Windows PowerShell") || !strings.Contains(n[1], "后台") {
		t.Errorf("note = %v", n)
	}

	if err := f.m.Inject("hi", wintype.Options{}); err != nil {
		t.Fatal(err)
	}
	if len(f.consoled) != 1 || f.consoled[0] != 42 || len(f.windowed) != 0 || len(f.cursor) != 0 {
		t.Errorf("consoled=%v windowed=%v cursor=%v", f.consoled, f.windowed, f.cursor)
	}
}

func TestToggle_SameTitleShownOnce(t *testing.T) {
	f := newFixture()
	f.addWindow(1, "Windows PowerShell")
	f.addConsole(1, 42, "Windows PowerShell")
	f.fg = 1
	f.m.Toggle()
	if n := f.lastNote(); strings.Contains(n[1], "·") {
		t.Errorf("note = %v", n)
	}
}

func TestToggle_AmbiguousTerminalsFallBackToWindow(t *testing.T) {
	f := newFixture()
	f.addWindow(1, "IDEA")
	f.addConsole(1, 42, "a")
	f.addConsole(1, 43, "b")
	f.fg = 1

	f.m.Toggle()
	if f.m.tgt.kind != kindWindow || f.m.tgt.hwnd != 1 {
		t.Fatalf("tgt = %+v", f.m.tgt)
	}
	if n := f.lastNote(); !strings.Contains(n[1], "2 个终端") || !strings.Contains(n[1], "按个键") {
		t.Errorf("note = %v", n)
	}
}

func TestToggle_RejectsOwnTerminal(t *testing.T) {
	f := newFixture()
	f.addWindow(1, "Windows Terminal")
	f.fg = 1
	f.m.resolve = func(uintptr, uint32) (consoleMatch, int) { return consoleMatch{}, -1 }
	f.m.Toggle()
	if f.m.IsLocked() || f.lastNote()[0] != "这个终端不能锁定" {
		t.Errorf("locked=%v note=%v", f.m.IsLocked(), f.lastNote())
	}
}

func TestToggle_RejectsUnlockableWindows(t *testing.T) {
	f := newFixture()
	f.addWindow(1, "self")
	f.pids[1] = 100 // 本进程
	f.addWindow(2, "")
	f.classes[2] = "Shell_TrayWnd"

	for _, h := range []uintptr{1, 2, 0} {
		f.fg = h
		f.m.Toggle()
		if f.m.IsLocked() {
			t.Fatalf("window %d should not be lockable", h)
		}
	}
	if len(f.notes) != 3 {
		t.Errorf("each rejection should notify, got %v", f.notes)
	}
}

func TestToggle_UntitledAndLongTitle(t *testing.T) {
	f := newFixture()
	f.addWindow(1, "")
	f.fg = 1
	f.m.Toggle()
	if !strings.Contains(f.notes[0][1], "无标题窗口") {
		t.Errorf("msg = %q", f.notes[0][1])
	}
	if got := displayName(strings.Repeat("长", 50)); len([]rune(got)) != maxTitleRunes+1 || !strings.HasSuffix(got, "…") {
		t.Errorf("displayName = %q", got)
	}
}

func TestLockConsolePID(t *testing.T) {
	f := newFixture()
	f.procs[7] = true
	f.m.LockConsolePID(7)
	if f.m.tgt.kind != kindConsole || f.m.tgt.pid != 7 || f.lastNote()[0] != "已锁定这个终端" {
		t.Fatalf("tgt=%+v note=%v", f.m.tgt, f.lastNote())
	}

	// 已锁定时 -lock 直接替换目标
	f.procs[8] = true
	f.m.LockConsolePID(8)
	if f.m.tgt.pid != 8 {
		t.Errorf("tgt = %+v", f.m.tgt)
	}

	f.m.LockConsolePID(9) // 无控制台
	if f.m.tgt.pid != 8 || f.lastNote()[0] != "没能锁定终端" {
		t.Errorf("failed lock should keep previous target: tgt=%+v note=%v", f.m.tgt, f.lastNote())
	}
}

func TestUnlock_WhenNotLocked(t *testing.T) {
	f := newFixture()
	f.m.Unlock()
	if f.lastNote()[0] != "当前没有锁定" {
		t.Errorf("note = %v", f.lastNote())
	}
}

func TestInject_Unlocked_UsesCursor(t *testing.T) {
	f := newFixture()
	if err := f.m.Inject("hi", wintype.Options{}); err != nil {
		t.Fatal(err)
	}
	if len(f.cursor) != 1 || f.cursor[0] != "hi" || len(f.windowed)+len(f.consoled) != 0 {
		t.Errorf("cursor=%v windowed=%v consoled=%v", f.cursor, f.windowed, f.consoled)
	}
}

func TestInject_LockedWindow_UsesTargetOnly(t *testing.T) {
	f := newFixture()
	f.addWindow(7, "Editor")
	f.fg = 7
	f.m.Toggle()
	if err := f.m.Inject("hi", wintype.Options{}); err != nil {
		t.Fatal(err)
	}
	if len(f.windowed) != 1 || f.windowed[0] != 7 || len(f.cursor) != 0 {
		t.Errorf("cursor=%v windowed=%v", f.cursor, f.windowed)
	}
}

func TestInject_TargetClosed(t *testing.T) {
	t.Run("window", func(t *testing.T) {
		f := newFixture()
		f.addWindow(7, "Editor")
		f.fg = 7
		f.m.Toggle()
		f.alive[7] = false
		if err := f.m.Inject("hi", wintype.Options{}); !errors.Is(err, wintype.ErrTargetClosed) {
			t.Fatalf("err = %v", err)
		}
		if len(f.cursor)+len(f.windowed) != 0 {
			t.Error("nothing should be sent")
		}
	})

	t.Run("console", func(t *testing.T) {
		f := newFixture()
		f.addWindow(1, "Terminal")
		f.addConsole(1, 42, "pwsh")
		f.fg = 1
		f.m.Toggle()
		f.procs[42] = false
		if err := f.m.Inject("hi", wintype.Options{}); !errors.Is(err, wintype.ErrTargetClosed) {
			t.Fatalf("err = %v", err)
		}
		if len(f.cursor)+len(f.consoled) != 0 {
			t.Error("nothing should be sent")
		}
		// 保持锁定，直到用户手动解除；解除后恢复光标模式
		if !f.m.IsLocked() {
			t.Error("lock should persist until user toggles")
		}
		f.m.Toggle()
		if err := f.m.Inject("again", wintype.Options{}); err != nil || len(f.cursor) != 1 {
			t.Errorf("err=%v cursor=%v", err, f.cursor)
		}
	})
}

func TestInject_PropagatesTypeError(t *testing.T) {
	f := newFixture()
	f.typeErr = errors.New("boom")
	if err := f.m.Inject("x", wintype.Options{}); err == nil {
		t.Error("expected error")
	}
}

func TestNotify_NoNotifier(t *testing.T) {
	m := &Manager{}
	m.Notify("a", "b") // 不应 panic
}

func TestConsoleName(t *testing.T) {
	tests := []struct {
		win  string
		c    consoleMatch
		want string
	}{
		{"GoLand", consoleMatch{Title: "Windows PowerShell", Line: `PS E:\core>`}, `GoLand · PS E:\core>`},
		{"GoLand", consoleMatch{Title: "Windows PowerShell"}, "GoLand · Windows PowerShell"},
		{"Windows PowerShell", consoleMatch{Title: "Windows PowerShell"}, "Windows PowerShell"},
		{"GoLand", consoleMatch{}, "GoLand"},
		{"", consoleMatch{Line: `PS E:\>`}, `PS E:\>`},
		{"", consoleMatch{}, "无标题终端"},
	}
	for _, tc := range tests {
		if got := consoleName(tc.win, tc.c); got != tc.want {
			t.Errorf("consoleName(%q,%+v) = %q, want %q", tc.win, tc.c, got, tc.want)
		}
	}
}
