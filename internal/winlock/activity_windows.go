//go:build windows

package winlock

import (
	"context"
	"strings"
	"sync"
	"time"

	"clawbot-ilink-relay/internal/wintype"
)

const (
	activitySampleInterval  = 200 * time.Millisecond
	activityRefreshInterval = 2 * time.Second
)

// activityTracker 记录每个终端最近一次收到输入的时间。
//
// ConPTY 终端（JetBrains、VS Code、Windows Terminal）里，每个标签页有一个独立的
// 宿主进程（OpenConsole.exe / conhost.exe），终端把用户的按键、焦点变化等写进它的
// 输入管道，宿主每读一次，进程的读操作计数就加一。后台标签收不到输入，计数不变。
// 所以“最近一次读计数变化”的那个宿主，就是用户刚刚在用的标签。
// 只读取计数，不碰任何窗口，也不往终端里写东西。
type activityTracker struct {
	mu    sync.Mutex
	ops   map[uint32]uint64    // 宿主 PID -> 上次读计数
	last  map[uint32]time.Time // 宿主 PID -> 最近一次收到输入的时间
	hosts []uint32
}

var tracker = newActivityTracker()

func newActivityTracker() *activityTracker {
	return &activityTracker{ops: map[uint32]uint64{}, last: map[uint32]time.Time{}}
}

func (a *activityTracker) run(ctx context.Context) {
	sample := time.NewTicker(activitySampleInterval)
	defer sample.Stop()
	var refreshed time.Time
	for {
		if time.Since(refreshed) >= activityRefreshInterval {
			a.refresh()
			refreshed = time.Now()
		}
		a.sample()
		select {
		case <-ctx.Done():
			return
		case <-sample.C:
		}
	}
}

// refresh 重新收集所有 shell 所在控制台的宿主进程。
func (a *activityTracker) refresh() {
	procs, err := wintype.Processes()
	if err != nil {
		return
	}
	seen := map[uint32]bool{}
	var hosts []uint32
	for _, p := range procs {
		if !shellExes[strings.ToLower(p.Exe)] {
			continue
		}
		if h, ok := wintype.ConsoleHostPID(p.PID); ok && !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hosts = hosts
	for h := range a.ops {
		if !seen[h] {
			delete(a.ops, h)
			delete(a.last, h)
		}
	}
}

func (a *activityTracker) sample() {
	a.mu.Lock()
	hosts := append([]uint32(nil), a.hosts...)
	a.mu.Unlock()

	now := time.Now()
	for _, h := range hosts {
		n, ok := wintype.ProcessReadOps(h)
		if !ok {
			continue
		}
		a.mu.Lock()
		if prev, seen := a.ops[h]; seen && n != prev {
			a.last[h] = now
		}
		a.ops[h] = n
		a.mu.Unlock()
	}
}

// lastInput 返回宿主最近一次收到输入的时间；程序启动后没收到过输入时为零值。
func (a *activityTracker) lastInput(host uint32) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last[host]
}
