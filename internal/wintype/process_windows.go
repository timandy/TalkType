//go:build windows

package wintype

import (
	"syscall"
	"unsafe"
)

var (
	ntdllDLL                      = syscall.NewLazyDLL("ntdll.dll")
	procNtQueryInformationProcess = ntdllDLL.NewProc("NtQueryInformationProcess")
	procGetProcessIoCounters      = kernel32DLL.NewProc("GetProcessIoCounters")
)

const processConsoleHostProcess = 49

const (
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
)

// ProcessEntry 是进程快照中的一项。
type ProcessEntry struct {
	PID  uint32
	PPID uint32
	Exe  string
}

// Processes 返回当前系统的进程列表。
func Processes() ([]ProcessEntry, error) {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.CloseHandle(snap)

	var pe syscall.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := syscall.Process32First(snap, &pe); err != nil {
		return nil, err
	}
	var out []ProcessEntry
	for {
		out = append(out, ProcessEntry{
			PID:  pe.ProcessID,
			PPID: pe.ParentProcessID,
			Exe:  syscall.UTF16ToString(pe.ExeFile[:]),
		})
		if err := syscall.Process32Next(snap, &pe); err != nil {
			break
		}
	}
	return out, nil
}

// ProcessStartTime 返回进程创建时间（FILETIME 数值），用于识别 PID 复用。
func ProcessStartTime(pid uint32) (uint64, bool) {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, pid)
	if err != nil {
		return 0, false
	}
	defer syscall.CloseHandle(h)
	var c, e, k, u syscall.Filetime
	if err := syscall.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		return 0, false
	}
	return uint64(c.HighDateTime)<<32 | uint64(c.LowDateTime), true
}

// ProcessAlive 判断 pid 对应的进程仍在运行，且就是创建时间为 start 的那个进程。
// start 为 0 时只检查进程是否存在。
func ProcessAlive(pid uint32, start uint64) bool {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, pid)
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil || code != stillActive {
		return false
	}
	if start == 0 {
		return true
	}
	var c, e, k, u syscall.Filetime
	if err := syscall.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		return false
	}
	return uint64(c.HighDateTime)<<32|uint64(c.LowDateTime) == start
}

// ConsoleHostPID 返回 pid 所在控制台的宿主进程（conhost.exe / OpenConsole.exe）。
func ConsoleHostPID(pid uint32) (uint32, bool) {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, pid)
	if err != nil {
		return 0, false
	}
	defer syscall.CloseHandle(h)
	var v uintptr
	var n uint32
	st, _, _ := procNtQueryInformationProcess.Call(uintptr(h), processConsoleHostProcess,
		uintptr(unsafe.Pointer(&v)), unsafe.Sizeof(v), uintptr(unsafe.Pointer(&n)))
	if st != 0 || v == 0 {
		return 0, false
	}
	return uint32(v &^ 3), true // 低两位是标志位
}

type ioCounters struct {
	ReadOps, WriteOps, OtherOps       uint64
	ReadBytes, WriteBytes, OtherBytes uint64
}

// ProcessReadOps 返回进程累计的读操作次数。对 ConPTY 宿主来说，终端每发来一次输入
// （按键、焦点变化、窗口大小变化）都会产生一次读。
func ProcessReadOps(pid uint32) (uint64, bool) {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, pid)
	if err != nil {
		return 0, false
	}
	defer syscall.CloseHandle(h)
	var c ioCounters
	r, _, _ := procGetProcessIoCounters.Call(uintptr(h), uintptr(unsafe.Pointer(&c)))
	return c.ReadOps, r != 0
}
