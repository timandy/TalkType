package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// AckedIDs 记录已处理的消息 ID，用于重放或重推时去重。文件是纯文本，一行一个 ID：
//
//   - 每记录一个 ID 就在文件末尾追加一行，不重写整个文件；
//   - 行数超过保留数量的两倍时整份重写一次，只留最近 limit 个，避免文件一直变长；
//   - 进程中断时最后一行可能只写了一半（没有换行符），读取时丢弃它，并在下次写入前重写文件。
type AckedIDs struct {
	path  string
	limit int

	mu    sync.Mutex
	ids   []string // 按记录顺序，最旧的在前
	set   map[string]struct{}
	lines int  // 文件里的完整行数
	dirty bool // 文件末尾有不完整的行，下次写入前需要重写
}

// LoadAckedIDs 读取记录文件；文件不存在时返回空记录。limit 是保留的 ID 数量，<= 0 时取默认值。
func LoadAckedIDs(path string, limit int) (*AckedIDs, error) {
	if limit <= 0 {
		limit = defaultAckCap
	}
	a := &AckedIDs{path: path, limit: limit, set: make(map[string]struct{})}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return a, nil
		}
		return nil, err
	}
	complete := data
	if i := bytes.LastIndexByte(data, '\n'); i < 0 {
		complete = nil
	} else {
		complete = data[:i+1]
	}
	a.dirty = len(complete) != len(data)
	for _, line := range strings.Split(string(complete), "\n") {
		if id := strings.TrimSpace(line); id != "" {
			a.lines++
			a.remember(id)
		}
	}
	return a, nil
}

// Path 返回记录文件路径。
func (a *AckedIDs) Path() string { return a.path }

// Has 判断 ID 是否已记录。
func (a *AckedIDs) Has(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.set[id]
	return ok
}

// Add 记录一个 ID 并写入文件。空 ID、含换行的 ID 和已记录的 ID 直接忽略。
func (a *AckedIDs) Add(id string) error {
	if id == "" || strings.ContainsAny(id, "\r\n") {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.set[id]; ok {
		return nil
	}
	a.remember(id)
	if a.dirty || a.lines+1 > 2*a.limit {
		return a.rewrite()
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(a.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(id + "\n")
	cerr := f.Close()
	if werr != nil {
		a.dirty = true // 可能写了半行
		return fmt.Errorf("写入 %s 失败: %w", a.path, werr)
	}
	if cerr != nil {
		return cerr
	}
	a.lines++
	return nil
}

// remember 在内存里记录 ID，超过 limit 时丢掉最旧的。调用方持有锁。
func (a *AckedIDs) remember(id string) {
	if _, ok := a.set[id]; ok {
		return
	}
	a.ids = append(a.ids, id)
	a.set[id] = struct{}{}
	if n := len(a.ids) - a.limit; n > 0 {
		for _, old := range a.ids[:n] {
			delete(a.set, old)
		}
		a.ids = append([]string(nil), a.ids[n:]...)
	}
}

// rewrite 用内存里的 ID 原子重写整个文件。调用方持有锁。
func (a *AckedIDs) rewrite() error {
	var b strings.Builder
	for _, id := range a.ids {
		b.WriteString(id)
		b.WriteByte('\n')
	}
	if err := writeAtomic(a.path, []byte(b.String())); err != nil {
		return err
	}
	a.lines, a.dirty = len(a.ids), false
	return nil
}
