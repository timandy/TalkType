//go:build windows

package winlock

import (
	"errors"
	"os"
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

const (
	// 缩小的像素数要大于一个字符宽，保证终端列数一定会变。
	resizeProbePixels  = 24
	resizeProbeTimeout = 500 * time.Millisecond
)

// resolveConsole 找出窗口 hwnd 里承载的终端会话：
//  1. 枚举所有 shell 进程，查询它们所在的控制台；
//  2. 控制台窗口就是 hwnd（传统控制台），或伪控制台的所有者是 hwnd（Windows Terminal）；
//  3. 都没有时，退回到“shell 是 hwnd 所属进程的子孙”（JetBrains、VS Code 终端）；
//  4. 同一个控制台里的多个 shell 只算一个；仍有多个时找当前标签页：
//     - Windows Terminal：窗口标题跟随当前标签，用标题探测；
//     - JetBrains / VS Code（窗口标题与标签无关）：看哪个终端最近收到过键盘输入。
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
	var shells []uint32
	for _, p := range procs {
		parent[p.PID] = p.PPID
		if p.PID != own && shellExes[strings.ToLower(p.Exe)] {
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

	switch {
	case len(cands) == 0 && self != nil:
		return consoleMatch{}, -1
	case len(cands) == 0:
		return consoleMatch{}, 0
	case len(cands) == 1 && self == nil:
		return toMatch(cands[0]), 1
	}

	// 多个候选，或者窗口里还有本程序自己的终端：确认当前选中的是哪个标签。
	if viaOwner {
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

	all := cands
	if self != nil {
		all = append(append([]wintype.ConsoleInfo(nil), cands...), *self)
	}
	pick, ok := visibleBySize(hwnd, all)
	if !ok {
		pick, ok = mostRecentInput(all)
	}
	if ok {
		if self != nil && pick.PID == self.PID {
			return consoleMatch{}, -1
		}
		return toMatch(pick), 1
	}
	if self != nil {
		return consoleMatch{}, -1
	}
	return consoleMatch{}, len(cands)
}

// visibleBySize 找出窗口里当前显示的那个终端标签：把窗口临时缩小几个像素再恢复。
// IDE 只会调整可见终端的尺寸，而尺寸变化会通过 ConPTY 传给该标签的宿主进程，
// 使它的读计数增加；后台标签不受影响。整个过程约 0.1～0.5 秒，窗口边缘会轻微闪一下。
func visibleBySize(hwnd uintptr, cands []wintype.ConsoleInfo) (wintype.ConsoleInfo, bool) {
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

	restore, ok := wintype.ShrinkWindow(hwnd, resizeProbePixels)
	if !ok {
		return wintype.ConsoleInfo{}, false
	}
	var changed []int
	deadline := time.Now().Add(resizeProbeTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(30 * time.Millisecond)
		changed = changed[:0]
		for i, h := range hosts {
			if h == 0 {
				continue
			}
			if n, ok := wintype.ProcessReadOps(h); ok && n != before[i] {
				changed = append(changed, i)
			}
		}
		if len(changed) > 0 {
			time.Sleep(60 * time.Millisecond) // 给同时可见的其他终端一点时间，避免误判唯一
			for i, h := range hosts {
				if h == 0 || contains(changed, i) {
					continue
				}
				if n, ok := wintype.ProcessReadOps(h); ok && n != before[i] {
					changed = append(changed, i)
				}
			}
			break
		}
	}
	restore()

	for _, i := range changed {
		logf("尺寸探测：可见终端 pid=%d 提示符=%q", cands[i].PID, cands[i].Line)
	}
	if len(changed) != 1 {
		logf("尺寸探测没有得到唯一结果（%d 个终端有反应），改用最近输入判断", len(changed))
		return wintype.ConsoleInfo{}, false
	}
	return cands[changed[0]], true
}

func contains(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// mostRecentInput 从候选里挑出最近收到过键盘输入的终端。没有任何候选收到过输入时返回 false。
func mostRecentInput(cands []wintype.ConsoleInfo) (wintype.ConsoleInfo, bool) {
	var best wintype.ConsoleInfo
	var bestAt time.Time
	for _, c := range cands {
		host, ok := wintype.ConsoleHostPID(c.PID)
		if !ok {
			continue
		}
		at := tracker.lastInput(host)
		logf("候选终端 pid=%d 宿主=%d 最近输入=%s 提示符=%q", c.PID, host, fmtAgo(at), c.Line)
		if !at.IsZero() && at.After(bestAt) {
			best, bestAt = c, at
		}
	}
	if bestAt.IsZero() {
		return wintype.ConsoleInfo{}, false
	}
	logf("选中最近有输入的终端 pid=%d", best.PID)
	return best, true
}

func fmtAgo(t time.Time) string {
	if t.IsZero() {
		return "无"
	}
	return time.Since(t).Round(100*time.Millisecond).String() + "前"
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
		return consoleMatch{}, errors.New("这是 clawbot-ilink-relay 自己所在的终端")
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
