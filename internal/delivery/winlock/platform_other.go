//go:build !windows

package winlock

import (
	"context"

	"wetype/internal/delivery/wintype"
)

// Start 在非 Windows 平台不可用。
func (m *Manager) Start(context.Context) error { return wintype.ErrUnsupported }

// SendLock 在非 Windows 平台不可用。
func SendLock(uint32) error { return wintype.ErrUnsupported }

// SendUnlock 在非 Windows 平台不可用。
func SendUnlock() error { return wintype.ErrUnsupported }

func resolveConsole(uintptr, uint32) (consoleMatch, int) { return consoleMatch{}, 0 }

func consoleOfPID(uint32) (consoleMatch, error) { return consoleMatch{}, wintype.ErrUnsupported }
