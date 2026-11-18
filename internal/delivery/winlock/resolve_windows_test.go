//go:build windows

package winlock

import (
	"testing"

	"wetype/internal/delivery/wintype"
)

func TestIsDescendant(t *testing.T) {
	parent := map[uint32]uint32{10: 5, 5: 1, 1: 0, 20: 20}
	if !isDescendant(10, 5, parent) || !isDescendant(10, 1, parent) {
		t.Error("expected descendant")
	}
	if isDescendant(5, 10, parent) || isDescendant(10, 0, parent) || isDescendant(20, 1, parent) {
		t.Error("unexpected descendant")
	}
}

func TestDedupeByConsole(t *testing.T) {
	// 100 是 powershell，101 是在它里面启动的 cmd（同一控制台 0xA）；200 在另一个控制台
	parent := map[uint32]uint32{101: 100, 100: 1, 200: 1}
	in := []wintype.ConsoleInfo{
		{PID: 101, Window: 0xA},
		{PID: 100, Window: 0xA},
		{PID: 200, Window: 0xB},
	}
	out := dedupeByConsole(in, parent)
	if len(out) != 2 || out[0].PID != 100 || out[1].PID != 200 {
		t.Errorf("out = %+v", out)
	}
}
