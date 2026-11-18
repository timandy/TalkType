//go:build windows

package wintype

import (
	"testing"
	"unsafe"
)

func TestInputRecordSize(t *testing.T) {
	if s := unsafe.Sizeof(inputRecord{}); s != 20 {
		t.Fatalf("INPUT_RECORD size = %d, want 20", s)
	}
}

func TestBuildConsoleRecords(t *testing.T) {
	recs := buildConsoleRecords("a中\r\nb\t😀")
	// a, 中, Enter, b, Tab, 😀(两个 UTF-16 单元)，每个按下+抬起
	if len(recs) != 7*2 {
		t.Fatalf("len = %d", len(recs))
	}
	for i := 0; i < len(recs); i += 2 {
		if recs[i].keyDown != 1 || recs[i+1].keyDown != 0 || recs[i].char != recs[i+1].char {
			t.Fatalf("record %d not a down/up pair: %+v %+v", i, recs[i], recs[i+1])
		}
	}
	if recs[0].char != 'a' || recs[2].char != '中' {
		t.Errorf("chars = %c %c", recs[0].char, recs[2].char)
	}
	if recs[4].vk != vkReturn || recs[4].char != '\r' {
		t.Errorf("CRLF should become a single Enter: %+v", recs[4])
	}
	if recs[8].vk != vkTab {
		t.Errorf("tab = %+v", recs[8])
	}
	if recs[10].char != 0xD83D || recs[12].char != 0xDE00 {
		t.Errorf("surrogates = %#x %#x", recs[10].char, recs[12].char)
	}
}
