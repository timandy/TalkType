//go:build windows

package wintype

// 后台写入控制台：通过 AttachConsole + WriteConsoleInputW 把按键记录直接写进目标
// 控制台的输入缓冲区，不需要窗口焦点。对传统控制台窗口和 ConPTY 伪控制台
// （Windows Terminal、JetBrains/VS Code 终端）都有效。
//
// 一个进程同一时间只能挂在一个控制台上，而本程序自己有控制台用于打印日志，
// 所以实际的挂接和写入由一个短命的辅助子进程完成：同一个 exe，以 HelperArg
// 作为第一个参数启动，通过 stdin/stdout 交换 JSON。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

// HelperArg 是辅助子进程模式的命令行标记，main 需要在解析参数前识别它。
const HelperArg = "--clawbot-console-helper"

const (
	detachedProcess   = 0x00000008
	createNoWindow    = 0x08000000
	errInvalidParam   = 87
	helperExitGone    = 3
	helperTimeout     = 15 * time.Second
	keyEventType      = 1
	scanReturn        = 0x1C
	scanTab           = 0x0F
	consoleBatchSize  = 512
	gaRootOwner       = 3
	titleProbeTimeout = 600 * time.Millisecond
)

var (
	procFreeConsole        = kernel32DLL.NewProc("FreeConsole")
	procAttachConsole      = kernel32DLL.NewProc("AttachConsole")
	procWriteConsoleInputW = kernel32DLL.NewProc("WriteConsoleInputW")
	procGetConsoleWindow   = kernel32DLL.NewProc("GetConsoleWindow")
	procGetConsoleTitleW   = kernel32DLL.NewProc("GetConsoleTitleW")
	procSetConsoleTitleW   = kernel32DLL.NewProc("SetConsoleTitleW")
	procGetConsoleSBInfo   = kernel32DLL.NewProc("GetConsoleScreenBufferInfo")
	procReadConsoleOutChar = kernel32DLL.NewProc("ReadConsoleOutputCharacterW")
)

var errConsoleGone = errors.New("目标进程已退出")

// ConsoleInfo 描述某个进程所在的控制台。
type ConsoleInfo struct {
	PID    uint32  `json:"pid"`
	Window uintptr `json:"window"` // GetConsoleWindow：传统控制台是可见窗口，ConPTY 是隐藏的伪窗口
	Owner  uintptr `json:"owner"`  // 伪窗口的顶层所有者，Windows Terminal 会设为自己的窗口
	Title  string  `json:"title"`
	Line   string  `json:"line"` // 光标所在行的文字，通常是提示符，例如 "PS E:\work>"
	Err    string  `json:"err,omitempty"`
}

type writeRequest struct {
	PID        uint32 `json:"pid"`
	Text       string `json:"text"`
	PressEnter bool   `json:"press_enter"`
	DelayMS    int64  `json:"delay_ms"`
}

type activeTabRequest struct {
	Window uintptr  `json:"window"`
	PIDs   []uint32 `json:"pids"`
}

// ---------------------------------------------------------------------------
// 父进程侧
// ---------------------------------------------------------------------------

func runHelper(cmd string, in, out any) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(in)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), helperTimeout)
	defer cancel()

	c := exec.CommandContext(ctx, exe, HelperArg, cmd)
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | createNoWindow, HideWindow: true}
	c.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	err = c.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == helperExitGone {
			return ErrTargetClosed
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s", msg)
		}
		return err
	}
	if out != nil {
		return json.Unmarshal(stdout.Bytes(), out)
	}
	return nil
}

// OwnConsoleWindow 返回本进程所在控制台的窗口句柄，没有控制台时为 0。
func OwnConsoleWindow() uintptr {
	w, _, _ := procGetConsoleWindow.Call()
	return w
}

// ProbeConsoles 查询每个进程所在的控制台。无法挂接的进程在 Err 中说明原因。
func ProbeConsoles(pids []uint32) ([]ConsoleInfo, error) {
	var out []ConsoleInfo
	if err := runHelper("probe", pids, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// FindActiveTab 用于一个终端窗口里有多个控制台（多个标签页）的情况：依次把每个
// 控制台的标题临时改成随机标记，看窗口标题是否跟着变，变了的就是当前选中的标签。
// 标题会立即恢复。返回匹配到的 PID。
func FindActiveTab(window uintptr, pids []uint32) ([]uint32, error) {
	var out []uint32
	if err := runHelper("activetab", activeTabRequest{Window: window, PIDs: pids}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// TypeToConsoleWithRetry 在后台把文本写入 pid 所在控制台的输入缓冲区。
// 进程已退出时返回 ErrTargetClosed，不重试。
func TypeToConsoleWithRetry(pid uint32, text string, opts Options) error {
	attempts := opts.MaxRetries
	if attempts <= 0 {
		attempts = 1
	}
	req := writeRequest{
		PID:        pid,
		Text:       text,
		PressEnter: opts.PressEnter,
		DelayMS:    (opts.TypeDelay + opts.EnterDelay).Milliseconds(),
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		err := runHelper("write", req, nil)
		if err == nil || errors.Is(err, ErrTargetClosed) {
			return err
		}
		lastErr = err
		time.Sleep(time.Duration(150*(i+1)) * time.Millisecond)
	}
	return fmt.Errorf("重试 %d 次后仍失败: %w", attempts, lastErr)
}

// ---------------------------------------------------------------------------
// 辅助子进程侧
// ---------------------------------------------------------------------------

// RunConsoleHelper 是辅助子进程的入口，返回进程退出码。
func RunConsoleHelper(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "缺少子命令")
		return 2
	}
	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var result any
	switch args[0] {
	case "write":
		var req writeRequest
		if err = json.Unmarshal(in, &req); err == nil {
			err = helperWrite(req)
		}
	case "probe":
		var pids []uint32
		if err = json.Unmarshal(in, &pids); err == nil {
			result = helperProbe(pids)
		}
	case "activetab":
		var req activeTabRequest
		if err = json.Unmarshal(in, &req); err == nil {
			result = helperActiveTab(req)
		}
	default:
		err = fmt.Errorf("未知子命令 %q", args[0])
	}
	if errors.Is(err, errConsoleGone) {
		fmt.Fprintln(os.Stderr, err)
		return helperExitGone
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if result != nil {
		_ = json.NewEncoder(os.Stdout).Encode(result)
	}
	return 0
}

func attachTo(pid uint32) error {
	procFreeConsole.Call()
	r, _, e := procAttachConsole.Call(uintptr(pid))
	if r == 0 {
		if errno, ok := e.(syscall.Errno); ok && errno == errInvalidParam {
			return errConsoleGone
		}
		if !ProcessAlive(pid, 0) {
			return errConsoleGone
		}
		return fmt.Errorf("挂接控制台失败 (pid=%d): %v", pid, e)
	}
	return nil
}

func detach() { procFreeConsole.Call() }

func consoleTitle() string {
	buf := make([]uint16, 1024)
	n, _, _ := procGetConsoleTitleW.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf[:n])
}

// cursorLine 读取当前控制台光标所在行的文字。
func cursorLine() string {
	name, _ := syscall.UTF16PtrFromString("CONOUT$")
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		return ""
	}
	defer syscall.CloseHandle(h)
	// CONSOLE_SCREEN_BUFFER_INFO：dwSize(X,Y) dwCursorPosition(X,Y) ...
	var info [11]int16
	if r, _, _ := procGetConsoleSBInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&info[0]))); r == 0 {
		return ""
	}
	width, y := info[0], info[3]
	if width <= 0 || y < 0 {
		return ""
	}
	buf := make([]uint16, width)
	var n uint32
	coord := uintptr(uint32(uint16(y)) << 16)
	procReadConsoleOutChar.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(width), coord, uintptr(unsafe.Pointer(&n)))
	return strings.TrimSpace(syscall.UTF16ToString(buf[:n]))
}

func setConsoleTitle(s string) {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return
	}
	procSetConsoleTitleW.Call(uintptr(unsafe.Pointer(p)))
}

func helperProbe(pids []uint32) []ConsoleInfo {
	out := make([]ConsoleInfo, 0, len(pids))
	for _, pid := range pids {
		info := ConsoleInfo{PID: pid}
		if err := attachTo(pid); err != nil {
			info.Err = err.Error()
		} else {
			w, _, _ := procGetConsoleWindow.Call()
			info.Window = w
			if w != 0 {
				o, _, _ := procGetAncestor.Call(w, gaRootOwner)
				info.Owner = o
			}
			info.Title = consoleTitle()
			info.Line = cursorLine()
			detach()
		}
		out = append(out, info)
	}
	return out
}

func helperActiveTab(req activeTabRequest) []uint32 {
	matched := []uint32{}
	for _, pid := range req.PIDs {
		if attachTo(pid) != nil {
			continue
		}
		old := consoleTitle()
		token := "clawbot-probe-" + fmt.Sprint(pid, time.Now().UnixNano()%100000)
		setConsoleTitle(token)
		hit := false
		deadline := time.Now().Add(titleProbeTimeout)
		for time.Now().Before(deadline) {
			if strings.Contains(WindowTitle(req.Window), token) {
				hit = true
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		setConsoleTitle(old)
		detach()
		if hit {
			matched = append(matched, pid)
		}
	}
	return matched
}

// inputRecord 对应 KEY_EVENT 类型的 INPUT_RECORD（20 字节）。
type inputRecord struct {
	eventType uint16
	_         uint16
	keyDown   int32
	repeat    uint16
	vk        uint16
	scan      uint16
	char      uint16
	ctrlState uint32
}

func keyPair(vk, scan, ch uint16) []inputRecord {
	return []inputRecord{
		{eventType: keyEventType, keyDown: 1, repeat: 1, vk: vk, scan: scan, char: ch},
		{eventType: keyEventType, keyDown: 0, repeat: 1, vk: vk, scan: scan, char: ch},
	}
}

func buildConsoleRecords(text string) []inputRecord {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	recs := make([]inputRecord, 0, len(text)*2)
	for _, r := range text {
		switch r {
		case '\n':
			recs = append(recs, keyPair(vkReturn, scanReturn, '\r')...)
		case '\t':
			recs = append(recs, keyPair(vkTab, scanTab, '\t')...)
		default:
			for _, u := range utf16.Encode([]rune{r}) {
				recs = append(recs, keyPair(0, 0, u)...)
			}
		}
	}
	return recs
}

func writeRecords(h syscall.Handle, recs []inputRecord) error {
	for start := 0; start < len(recs); start += consoleBatchSize {
		end := start + consoleBatchSize
		if end > len(recs) {
			end = len(recs)
		}
		var n uint32
		r, _, e := procWriteConsoleInputW.Call(uintptr(h), uintptr(unsafe.Pointer(&recs[start])),
			uintptr(end-start), uintptr(unsafe.Pointer(&n)))
		if r == 0 {
			return fmt.Errorf("写入控制台输入失败: %v", e)
		}
	}
	return nil
}

func helperWrite(req writeRequest) error {
	if err := attachTo(req.PID); err != nil {
		return err
	}
	defer detach()

	name, _ := syscall.UTF16PtrFromString("CONIN$")
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		return fmt.Errorf("打开控制台输入失败: %w", err)
	}
	defer syscall.CloseHandle(h)

	if recs := buildConsoleRecords(req.Text); len(recs) > 0 {
		if err := writeRecords(h, recs); err != nil {
			return err
		}
	}
	if req.PressEnter {
		if req.DelayMS > 0 {
			time.Sleep(time.Duration(req.DelayMS) * time.Millisecond)
		}
		return writeRecords(h, keyPair(vkReturn, scanReturn, '\r'))
	}
	return nil
}
