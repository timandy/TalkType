//go:build windows

package winlock

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"time"

	"clawbot-ilink-relay/internal/wintype"
)

// 视为“终端会话”的 shell 进程。
var shellExes = map[string]bool{
	"powershell.exe": true,
	"pwsh.exe":       true,
	"cmd.exe":        true,
	"bash.exe":       true,
	"wsl.exe":        true,
	"nu.exe":         true,
}

const maxAncestorDepth = 32

// resizeProbeSteps 是尺寸探测的各级：底边上移的逻辑像素数（96 DPI 下，按显示器 DPI 换算）
// 和这一级最多等待的时间。正在显示的终端实测 10～60 毫秒内有反应。
// 24 像素大于一行文字的高度；终端停靠在 IDE 主窗口里、只分到一部分高度时，靠后两级保证行数会变。
var resizeProbeSteps = []struct {
	pixels int32
	wait   time.Duration
}{
	{24, 60 * time.Millisecond},
	{64, 60 * time.Millisecond},
	{120, 100 * time.Millisecond},
}

// 自身就是终端的窗口类名：传统控制台和 Windows Terminal。
var terminalClasses = map[string]bool{
	"ConsoleWindowClass":            true,
	"CASCADIA_HOSTING_WINDOW_CLASS": true,
}

const (
	resizeProbePoll   = 10 * time.Millisecond
	resizeProbeSettle = 30 * time.Millisecond // 发现第一个反应后再等一会儿，看有没有同时显示的其他终端
)

// resolveConsole 找出窗口 hwnd 里承载的终端会话：
//  1. 枚举所有 shell 进程，查询它们所在的控制台；
//  2. 控制台窗口就是 hwnd（传统控制台），或伪控制台的所有者是 hwnd（Windows Terminal）；
//  3. 都没有时，退回到“shell 是 hwnd 所属进程的子孙”（JetBrains、VS Code 终端）；
//  4. 同一个控制台里的多个 shell 只算一个；
//  5. 窗口本身是终端时：只有一个就直接锁定，多个标签用标题探测找当前标签；
//  6. IDE 里的终端：用尺寸探测确认正在显示的那一个，确认不了就按普通窗口处理。
//
// 返回匹配结果和候选数量；数量为 0 或大于 1 时调用方按普通窗口处理。
// 返回 -1 表示当前选中的是本程序自己所在的终端，不能锁定（否则消息会输回自己）。
func resolveConsole(hwnd uintptr, winPID uint32) (consoleMatch, int) {
	procs, err := wintype.Processes()
	if err != nil {
		return consoleMatch{}, 0
	}
	own := uint32(os.Getpid())
	parent := make(map[uint32]uint32, len(procs))
	for _, p := range procs {
		parent[p.PID] = p.PPID
	}
	// 终端窗口（传统控制台、Windows Terminal）里的 shell 不一定是窗口进程的子孙，
	// 要查询全部 shell；其他窗口只需查询它自己的子孙 shell，没有就直接按普通窗口处理，
	// 省去启动查询的开销（例如浏览器）。
	terminalWindow := terminalClasses[wintype.WindowClass(hwnd)]
	var shells []uint32
	for _, p := range procs {
		if p.PID == own || !shellExes[strings.ToLower(p.Exe)] {
			continue
		}
		if terminalWindow || isDescendant(p.PID, winPID, parent) {
			shells = append(shells, p.PID)
		}
	}
	if len(shells) == 0 {
		return consoleMatch{}, 0
	}
	infos, err := wintype.ProbeConsoles(shells)
	if err != nil {
		return consoleMatch{}, 0
	}

	ownConsole := wintype.OwnConsoleWindow()
	var direct, nested []wintype.ConsoleInfo
	var selfDirect, selfNested *wintype.ConsoleInfo
	for _, in := range infos {
		if in.Err != "" {
			continue
		}
		isSelf := ownConsole != 0 && in.Window == ownConsole
		switch {
		case in.Window == hwnd || in.Owner == hwnd:
			if isSelf {
				selfDirect = &in
			} else {
				direct = append(direct, in)
			}
		case isDescendant(in.PID, winPID, parent):
			if isSelf {
				selfNested = &in
			} else {
				nested = append(nested, in)
			}
		}
	}
	cands, self, viaOwner := direct, selfDirect, true
	if len(cands) == 0 && selfDirect == nil {
		cands, self, viaOwner = nested, selfNested, false
	}
	cands = dedupeByConsole(cands, parent)

	if viaOwner {
		// 窗口本身就是终端（传统控制台、Windows Terminal）。
		switch {
		case len(cands) == 0 && self != nil:
			return consoleMatch{}, -1
		case len(cands) == 0:
			return consoleMatch{}, 0
		case len(cands) == 1 && self == nil:
			return toMatch(cands[0]), 1
		}
		// 多个标签：窗口标题跟随当前标签，用标题探测。
		pids := make([]uint32, len(cands))
		for i, c := range cands {
			pids[i] = c.PID
		}
		if active, err := wintype.FindActiveTab(hwnd, pids); err == nil && len(active) == 1 {
			for _, c := range cands {
				if c.PID == active[0] {
					return toMatch(c), 1
				}
			}
		}
	}

	// IDE 里的终端（或标题探测失败）：终端不一定显示在窗口里，必须用尺寸探测确认。
	// 只有确认到唯一一个正在显示的终端才后台写入，否则按普通窗口处理（发送时切前台）。
	all := cands
	if self != nil {
		all = append(append([]wintype.ConsoleInfo(nil), cands...), *self)
	}
	if len(all) == 0 {
		return consoleMatch{}, 0
	}
	visible := visibleBySize(hwnd, all)
	switch len(visible) {
	case 0:
		logf("窗口里没有正在显示的终端，按普通窗口处理")
		return consoleMatch{}, 0
	case 1:
		if self != nil && visible[0].PID == self.PID {
			return consoleMatch{}, -1
		}
		return toMatch(visible[0]), 1
	default:
		return consoleMatch{}, len(visible)
	}
}

// visibleBySize 找出窗口里正在显示的终端：把窗口底边临时上移几个像素再恢复。
// IDE 只会调整正在显示的终端的尺寸，尺寸变化会通过 ConPTY 传给该终端的宿主进程，
// 使它的读计数增加；隐藏的终端和后台标签不受影响。有终端在显示时约 0.1 秒，
// 没有时要等完所有级别（约 0.22 秒），窗口底部会轻微闪一下。窗口最小化等无法探测的情况返回空。
func visibleBySize(hwnd uintptr, cands []wintype.ConsoleInfo) []wintype.ConsoleInfo {
	hosts := make([]uint32, len(cands))
	before := make([]uint64, len(cands))
	for i, c := range cands {
		h, ok := wintype.ConsoleHostPID(c.PID)
		if !ok {
			continue
		}
		hosts[i] = h
		before[i], _ = wintype.ProcessReadOps(h)
	}
	changedNow := func(skip []int) []int {
		var out []int
		for i, h := range hosts {
			if h == 0 || contains(skip, i) {
				continue
			}
			if n, ok := wintype.ProcessReadOps(h); ok && n != before[i] {
				out = append(out, i)
			}
		}
		return out
	}

	runtime.LockOSThread() // HeightShrinker 会临时调整本线程的 DPI 设置
	defer runtime.UnlockOSThread()
	sh, ok := wintype.BeginShrinkHeight(hwnd)
	if !ok {
		logf("尺寸探测：窗口无法调整尺寸（可能已最小化）")
		return nil
	}

	// 逐级加大上移距离：终端单独成窗口时第一级就有反应；终端停靠在 IDE 主窗口里时，
	// 上移的高度可能有一部分分给编辑区，后面几级保证终端面板也分到至少一行。
	var changed []int
	start := time.Now()
probe:
	for i, step := range resizeProbeSteps {
		px := sh.Shrink(step.pixels)
		stepEnd := time.Now().Add(step.wait)
		for time.Now().Before(stepEnd) {
			time.Sleep(resizeProbePoll)
			if changed = changedNow(nil); len(changed) > 0 {
				logf("尺寸探测：第 %d 级（上移 %d 像素）在 %v 后有反应", i+1, px, time.Since(start).Round(time.Millisecond))
				time.Sleep(resizeProbeSettle)
				changed = append(changed, changedNow(changed)...)
				break probe
			}
		}
	}
	sh.Restore()
	out := make([]wintype.ConsoleInfo, 0, len(changed))
	for _, i := range changed {
		logf("尺寸探测：正在显示的终端 pid=%d 提示符=%q", cands[i].PID, cands[i].Line)
		out = append(out, cands[i])
	}
	return out
}

func contains(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// consoleOfPID 用于 -lock：直接锁定指定进程所在的控制台。
func consoleOfPID(pid uint32) (consoleMatch, error) {
	infos, err := wintype.ProbeConsoles([]uint32{pid})
	if err != nil {
		return consoleMatch{}, err
	}
	if len(infos) != 1 {
		return consoleMatch{}, errors.New("没有查询到控制台信息")
	}
	if infos[0].Err != "" {
		return consoleMatch{}, errors.New(infos[0].Err)
	}
	if own := wintype.OwnConsoleWindow(); own != 0 && infos[0].Window == own {
		return consoleMatch{}, errors.New("不能锁定本程序所在的终端")
	}
	return toMatch(infos[0]), nil
}

func toMatch(in wintype.ConsoleInfo) consoleMatch {
	start, _ := wintype.ProcessStartTime(in.PID)
	return consoleMatch{PID: in.PID, Start: start, Title: in.Title, Line: in.Line}
}

func isDescendant(pid, ancestor uint32, parent map[uint32]uint32) bool {
	if ancestor == 0 {
		return false
	}
	for i := 0; i < maxAncestorDepth; i++ {
		p, ok := parent[pid]
		if !ok || p == 0 || p == pid {
			return false
		}
		if p == ancestor {
			return true
		}
		pid = p
	}
	return false
}

// dedupeByConsole 合并同一控制台里的多个 shell（例如 PowerShell 里又开了 cmd），
// 保留最外层的那个。
func dedupeByConsole(in []wintype.ConsoleInfo, parent map[uint32]uint32) []wintype.ConsoleInfo {
	groups := make(map[uintptr][]wintype.ConsoleInfo)
	var order []uintptr
	for _, c := range in {
		key := c.Window
		if key == 0 {
			key = uintptr(c.PID) | 1<<62 // 没有控制台窗口时各自独立
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], c)
	}
	out := make([]wintype.ConsoleInfo, 0, len(order))
	for _, k := range order {
		g := groups[k]
		pick := g[0]
		for _, c := range g {
			outer := true
			for _, o := range g {
				if o.PID != c.PID && isDescendant(c.PID, o.PID, parent) {
					outer = false
					break
				}
			}
			if outer {
				pick = c
				break
			}
		}
		out = append(out, pick)
	}
	return out
}
