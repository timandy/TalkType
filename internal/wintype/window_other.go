//go:build !windows

package wintype

// 非 Windows 平台的占位实现，仅用于通过编译。

const HelperArg = "--wetype-console-helper"

func ForegroundWindow() uintptr                            { return 0 }
func RootWindow(h uintptr) uintptr                         { return h }
func IsWindow(uintptr) bool                                { return false }
func WindowTitle(uintptr) string                           { return "" }
func WindowClass(uintptr) string                           { return "" }
func WindowPID(uintptr) uint32                             { return 0 }
func ProcessAlive(uint32, uint64) bool                     { return false }
func RunConsoleHelper([]string) int                        { return 1 }
func TypeToWindowWithRetry(uintptr, string, Options) error { return ErrUnsupported }
func TypeToConsoleWithRetry(uint32, string, Options) error { return ErrUnsupported }
