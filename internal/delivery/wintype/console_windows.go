//go:build windows

package wintype

// 后台写入控制台：通过 AttachConsole + WriteConsoleInputW 把按键记录直接写进目标
// 控制台的输入缓冲区，不需要窗口焦点。对传统控制台窗口和 ConPTY 伪控制台
// （Windows Terminal、JetBrains/VS Code 终端）都有效。
//
// 一个进程同一时间只能挂在一个控制台上，而本程序自己有控制台用于打印日志，
// 所以实际的挂接和写入由一个常驻的辅助子进程完成：同一个 exe，以 HelperArg serve
// 启动，通过 stdin/stdout 逐行交换 JSON。辅助进程在第一次使用时启动，之后复用，
// 省去每次启动进程的开销（实测约 45 毫秒）；本程序退出时 stdin 关闭，它随之退出。

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

// HelperArg 是辅助子进程模式的命令行标记，main 需要在解析参数前识别它。
const HelperArg = "--talktype-console-helper"

const (
	detachedProcess   = 0x00000008
	createNoWindow    = 0x08000000
	errInvalidParam   = 87
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
	procSetConsoleCtrlHdlr = kernel32DLL.NewProc("SetConsoleCtrlHandler")
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

type helperRequest struct {
	Cmd  string          `json:"cmd"`
	Data json.RawMessage `json:"data"`
}

type helperResponse struct {
	Gone   bool            `json:"gone,omitempty"` // 目标进程已退出
	Err    string          `json:"err,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// helperClient 管理常驻的辅助进程。请求串行处理。
type helperClient struct {
	mu  sync.Mutex
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

var helper helperClient

// errNotSent 表示请求还没送到辅助进程（进程没启动或已经退出），可以安全地重启后重发。
type errNotSent struct{ err error }

func (e errNotSent) Error() string { return e.err.Error() }

func (c *helperClient) start() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, HelperArg, "serve")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | createNoWindow, HideWindow: true}
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	c.cmd, c.in, c.out = cmd, in, bufio.NewReaderSize(out, 64<<10)
	return nil
}

func (c *helperClient) stop() {
	if c.cmd == nil {
		return
	}
	_ = c.in.Close()
	_ = c.cmd.Process.Kill()
	_ = c.cmd.Wait()
	c.cmd, c.in, c.out = nil, nil, nil
}

// roundTrip 发送一个请求并等待响应，超时后返回错误（调用方负责结束辅助进程）。
func (c *helperClient) roundTrip(req []byte) (helperResponse, error) {
	in, out := c.in, c.out
	type result struct {
		resp helperResponse
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		if _, err := in.Write(req); err != nil {
			ch <- result{err: errNotSent{err}}
			return
		}
		line, err := out.ReadBytes('\n')
		if err != nil {
			ch <- result{err: err}
			return
		}
		var r helperResponse
		ch <- result{r, json.Unmarshal(line, &r)}
	}()
	select {
	case r := <-ch:
		return r.resp, r.err
	case <-time.After(helperTimeout):
		return helperResponse{}, errors.New("辅助进程无响应")
	}
}

// call 把请求交给常驻辅助进程。辅助进程不存在或已退出时自动重启；
// 请求已经送达但辅助进程中途出错时不重发，避免重复输入。
func (c *helperClient) call(cmd string, in, out any) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := json.Marshal(helperRequest{Cmd: cmd, Data: data})
	if err != nil {
		return err
	}
	req = append(req, '\n')

	c.mu.Lock()
	defer c.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if c.cmd == nil {
			if err := c.start(); err != nil {
				return fmt.Errorf("启动辅助进程失败: %w", err)
			}
		}
		resp, err := c.roundTrip(req)
		if err != nil {
			c.stop()
			lastErr = err
			var ns errNotSent
			if errors.As(err, &ns) {
				continue
			}
			return err
		}
		switch {
		case resp.Gone:
			return ErrTargetClosed
		case resp.Err != "":
			return errors.New(resp.Err)
		case out != nil && len(resp.Result) > 0:
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	}
	return lastErr
}

func runHelper(cmd string, in, out any) error { return helper.call(cmd, in, out) }

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

// FindActiveTab 用于一个终端窗口里有多个控制台（多个标签页）的情况：同时把每个
// 控制台的标题临时改成不同的随机标记，看窗口标题变成了哪一个，就是当前选中的标签。
// 标题随后恢复。返回匹配到的 PID。
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
	if len(args) == 0 || args[0] != "serve" {
		fmt.Fprintln(os.Stderr, "用法：--talktype-console-helper serve")
		return 2
	}
	// 挂接期间目标控制台里按 Ctrl+C、Ctrl+Break 或关闭窗口，都会通知到本进程，
	// 默认处理会让进程退出。这里全部吞掉，进程照常服务。
	procSetConsoleCtrlHdlr.Call(syscall.NewCallback(func(uintptr) uintptr { return 1 }), 1)

	r := bufio.NewReaderSize(os.Stdin, 64<<10)
	w := bufio.NewWriter(os.Stdout)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			b, _ := json.Marshal(handleHelperRequest(line))
			_, _ = w.Write(append(b, '\n'))
			if w.Flush() != nil {
				return 0
			}
		}
		if err != nil { // 本程序已退出
			return 0
		}
	}
}

func handleHelperRequest(line []byte) (resp helperResponse) {
	var req helperRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return helperResponse{Err: "请求格式错误: " + err.Error()}
	}
	var result any
	var err error
	switch req.Cmd {
	case "write":
		var in writeRequest
		if err = json.Unmarshal(req.Data, &in); err == nil {
			err = helperWrite(in)
		}
	case "probe":
		var pids []uint32
		if err = json.Unmarshal(req.Data, &pids); err == nil {
			result = helperProbe(pids)
		}
	case "activetab":
		var in activeTabRequest
		if err = json.Unmarshal(req.Data, &in); err == nil {
			result = helperActiveTab(in)
		}
	default:
		err = fmt.Errorf("未知命令 %q", req.Cmd)
	}
	switch {
	case errors.Is(err, errConsoleGone):
		return helperResponse{Gone: true}
	case err != nil:
		return helperResponse{Err: err.Error()}
	}
	if result != nil {
		b, _ := json.Marshal(result)
		resp.Result = b
	}
	return resp
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
	type probe struct {
		pid        uint32
		old, token string
	}
	// 1. 同时给所有候选控制台设置不同的临时标题。
	seed := time.Now().UnixNano() % 1000000
	var probes []probe
	for i, pid := range req.PIDs {
		if attachTo(pid) != nil {
			continue
		}
		p := probe{pid: pid, old: consoleTitle(), token: fmt.Sprintf("#%06d-%d#", seed, i)}
		setConsoleTitle(p.token)
		detach()
		probes = append(probes, p)
	}
	// 2. 等窗口标题变成其中一个标记。
	matched := []uint32{}
	deadline := time.Now().Add(titleProbeTimeout)
	for len(probes) > 0 && len(matched) == 0 && time.Now().Before(deadline) {
		title := WindowTitle(req.Window)
		for _, p := range probes {
			if strings.Contains(title, p.token) {
				matched = append(matched, p.pid)
			}
		}
		if len(matched) == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	// 3. 恢复原标题。
	for _, p := range probes {
		if attachTo(p.pid) == nil {
			setConsoleTitle(p.old)
			detach()
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
