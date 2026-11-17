package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readLines(t *testing.T, p string) []string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func TestAckedIDs_MissingFile(t *testing.T) {
	a, err := LoadAckedIDs(filepath.Join(t.TempDir(), "sub", "x.ids"), 5)
	if err != nil || a.Has("1") {
		t.Fatalf("a=%v err=%v", a, err)
	}
	// 目录不存在时 Add 会自动创建
	if err := a.Add("1"); err != nil {
		t.Fatal(err)
	}
	if got := readLines(t, a.Path()); len(got) != 1 || got[0] != "1" {
		t.Errorf("file = %v", got)
	}
}

// 追加写入：每个 ID 追加一行；重复、空、含换行的 ID 不写。
func TestAckedIDs_AppendOneLinePerID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.ids")
	a, _ := LoadAckedIDs(p, 5)
	for _, id := range []string{"a", "b", "a", "", "c\nd", "c"} {
		if err := a.Add(id); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(readLines(t, p), ","); got != "a,b,c" {
		t.Errorf("file = %q, want a,b,c", got)
	}
	if !a.Has("a") || !a.Has("c") || a.Has("c\nd") || a.Has("") {
		t.Error("Has wrong")
	}
	// 重新加载后记录还在
	b, err := LoadAckedIDs(p, 5)
	if err != nil || !b.Has("a") || !b.Has("b") || !b.Has("c") {
		t.Fatalf("reload: err=%v", err)
	}
}

// 追加写入：未达到压缩阈值前，已有的行不会被重写。
func TestAckedIDs_AppendDoesNotRewrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.ids")
	a, _ := LoadAckedIDs(p, 3)
	a.Add("1")
	// 在文件开头做一个标记：如果后续 Add 重写了文件，标记会消失
	data, _ := os.ReadFile(p)
	os.WriteFile(p, append([]byte("marker\n"), data...), 0o600)
	a.lines++
	a.Add("2")
	if got := readLines(t, p); got[0] != "marker" || got[len(got)-1] != "2" {
		t.Errorf("file was rewritten instead of appended: %v", got)
	}
}

// 内存里只保留最近 limit 个；文件行数超过 2*limit 时压缩成最近 limit 个。
func TestAckedIDs_LimitAndCompaction(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.ids")
	a, _ := LoadAckedIDs(p, 3)
	for i := 1; i <= 6; i++ {
		a.Add(fmt.Sprint(i))
	}
	if got := len(readLines(t, p)); got != 6 {
		t.Fatalf("before compaction: %d lines, want 6", got)
	}
	if a.Has("3") || !a.Has("4") || !a.Has("6") {
		t.Error("memory should keep only the latest 3")
	}
	a.Add("7") // 第 7 行超过 2*3，触发压缩
	if got := strings.Join(readLines(t, p), ","); got != "5,6,7" {
		t.Errorf("after compaction file = %q, want 5,6,7", got)
	}
	a.Add("8")
	if got := strings.Join(readLines(t, p), ","); got != "5,6,7,8" {
		t.Errorf("append after compaction = %q", got)
	}
	// 重新加载：文件里有 4 行，只保留最近 3 个
	b, _ := LoadAckedIDs(p, 3)
	if b.Has("5") || !b.Has("6") || !b.Has("8") {
		t.Error("reload should keep only the latest 3")
	}
}

// 容错：最后一行没有换行符（写到一半中断）时丢弃它，下次写入前重写文件，不会和新行粘在一起。
func TestAckedIDs_PartialLastLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.ids")
	os.WriteFile(p, []byte("a\nb\nhalf"), 0o600)
	a, err := LoadAckedIDs(p, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Has("a") || !a.Has("b") || a.Has("half") {
		t.Error("partial line should be dropped")
	}
	a.Add("c")
	if got := strings.Join(readLines(t, p), ","); got != "a,b,c" {
		t.Errorf("file = %q, want a,b,c", got)
	}

	// 整个文件只有半行
	os.WriteFile(p, []byte("only"), 0o600)
	a, _ = LoadAckedIDs(p, 10)
	if a.Has("only") {
		t.Error("partial-only file should be empty")
	}
	a.Add("x")
	if got := strings.Join(readLines(t, p), ","); got != "x" {
		t.Errorf("file = %q, want x", got)
	}
}

func TestAckedIDs_ToleratesBlankLinesAndCRLF(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.ids")
	os.WriteFile(p, []byte("a\r\n\r\n  b  \n\n"), 0o600)
	a, err := LoadAckedIDs(p, 10)
	if err != nil || !a.Has("a") || !a.Has("b") || a.lines != 2 {
		t.Fatalf("a=%+v err=%v", a, err)
	}
}

func TestAckedIDs_DefaultLimit(t *testing.T) {
	a, _ := LoadAckedIDs(filepath.Join(t.TempDir(), "x.ids"), 0)
	if a.limit != defaultAckCap {
		t.Errorf("limit = %d", a.limit)
	}
}
