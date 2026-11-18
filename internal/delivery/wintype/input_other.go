//go:build !windows

package wintype

import "errors"

// ErrUnsupported 表示当前平台不支持 win32 键盘注入。
var ErrUnsupported = errors.New("wintype: 仅支持 Windows")

// SetupConsole 在非 Windows 平台无需处理。
func SetupConsole() {}

// Type 在非 Windows 平台返回 ErrUnsupported。
func Type(string, Options) error { return ErrUnsupported }

// TypeWithRetry 在非 Windows 平台返回 ErrUnsupported。
func TypeWithRetry(string, Options) error { return ErrUnsupported }
