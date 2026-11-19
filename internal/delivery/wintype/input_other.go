//go:build !windows && !darwin

package wintype

// SetupConsole 在 Windows 以外的平台无需处理。
func SetupConsole() {}

// Type 在不支持的平台返回 ErrUnsupported。
func Type(string, Options) error { return ErrUnsupported }

// TypeWithRetry 在不支持的平台返回 ErrUnsupported。
func TypeWithRetry(string, Options) error { return ErrUnsupported }
