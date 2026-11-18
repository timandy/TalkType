//go:build windows

package wintype

import (
	"errors"
	"os"
	"testing"
	"time"
)

// 测试二进制本身充当辅助进程：helper 用 os.Executable() 启动自己。
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == HelperArg {
		os.Exit(RunConsoleHelper(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func helperPID(t *testing.T) int {
	t.Helper()
	helper.mu.Lock()
	defer helper.mu.Unlock()
	if helper.cmd == nil || helper.cmd.Process == nil {
		t.Fatal("helper not running")
	}
	return helper.cmd.Process.Pid
}

func TestHelperIsReusedAndFast(t *testing.T) {
	if _, err := ProbeConsoles(nil); err != nil {
		t.Fatal(err)
	}
	pid := helperPID(t)

	start := time.Now()
	for i := 0; i < 5; i++ {
		if _, err := ProbeConsoles(nil); err != nil {
			t.Fatal(err)
		}
	}
	if per := time.Since(start) / 5; per > 20*time.Millisecond {
		t.Errorf("reused helper call took %v, expected a few milliseconds", per)
	}
	if helperPID(t) != pid {
		t.Error("helper process should be reused")
	}
}

func TestHelperRestartsAfterCrash(t *testing.T) {
	if _, err := ProbeConsoles(nil); err != nil {
		t.Fatal(err)
	}
	old := helperPID(t)
	helper.mu.Lock()
	_ = helper.cmd.Process.Kill()
	_ = helper.cmd.Wait()
	helper.mu.Unlock()

	// 第一次调用可能在读响应时才发现进程已退出（请求已写入管道缓冲区），此时报错不重发；
	// 之后的调用必须自动重启成功。
	var err error
	for i := 0; i < 2; i++ {
		if _, err = ProbeConsoles(nil); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("helper did not recover: %v", err)
	}
	if helperPID(t) == old {
		t.Error("helper should have been restarted")
	}
}

func TestHelperReportsGoneProcess(t *testing.T) {
	// 找一个肯定不存在的 PID
	pid := uint32(0xFFFFFFF0)
	err := TypeToConsoleWithRetry(pid, "x", Options{MaxRetries: 1})
	if !errors.Is(err, ErrTargetClosed) {
		t.Errorf("err = %v, want ErrTargetClosed", err)
	}
	infos, err := ProbeConsoles([]uint32{pid})
	if err != nil || len(infos) != 1 || infos[0].Err == "" {
		t.Errorf("probe of missing pid: %v %+v", err, infos)
	}
}

func TestHelperRejectsUnknownCommand(t *testing.T) {
	if err := runHelper("nope", nil, nil); err == nil {
		t.Error("expected error for unknown command")
	}
}
