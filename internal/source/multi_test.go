package source

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSource struct {
	name     string
	events   *[]string
	mu       *sync.Mutex
	needAuth bool
	block    bool // Authorize 一直阻塞到 ctx 结束，模拟没人扫码
}

func (f *fakeSource) rec(s string) {
	f.mu.Lock()
	*f.events = append(*f.events, s)
	f.mu.Unlock()
}

func (f *fakeSource) Name() string { return f.name }

func (f *fakeSource) Check(ctx context.Context) error {
	f.rec("check:" + f.name)
	if f.needAuth {
		return ErrAuthRequired
	}
	return nil
}

func (f *fakeSource) Authorize(ctx context.Context) error {
	f.rec("auth:" + f.name)
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (f *fakeSource) Run(ctx context.Context, handle Handler) error {
	f.rec("run:" + f.name)
	return handle(ctx, Message{From: f.name, Text: "hi"})
}

// 先按顺序检查全部来源并启动凭证有效的，再按顺序给需要授权的来源弹扫码。
func TestMulti_CheckAllThenAuthorizeInOrder(t *testing.T) {
	var events []string
	var mu sync.Mutex
	mk := func(name string, need bool) *fakeSource {
		return &fakeSource{name: name, events: &events, mu: &mu, needAuth: need}
	}
	a, b, c, d := mk("a", false), mk("b", true), mk("c", true), mk("d", false)
	m := NewMulti(log.New(io.Discard, "", 0), a, b, c, d)

	got := 0
	if err := m.Run(context.Background(), func(ctx context.Context, msg Message) error { got++; return nil }); err != nil {
		t.Fatal(err)
	}
	if got != 4 {
		t.Errorf("收到 %d 条消息，期望 4", got)
	}
	idx := map[string]int{}
	for i, e := range events {
		idx[e] = i
	}
	before := [][2]string{
		{"check:a", "check:b"}, {"check:b", "check:c"}, {"check:c", "check:d"},
		{"check:d", "auth:b"}, // 全部检查完才弹扫码
		{"auth:b", "auth:c"},  // 按配置顺序逐个弹
		{"auth:b", "run:b"}, {"auth:c", "run:c"},
		{"check:a", "run:a"}, {"check:d", "run:d"},
	}
	for _, p := range before {
		if idx[p[0]] >= idx[p[1]] {
			t.Errorf("%s 应早于 %s: %v", p[0], p[1], events)
		}
	}
	if m.Name() != "a、b、c、d" {
		t.Errorf("Name = %q", m.Name())
	}
}

// 没人扫码：超时后跳过，继续下一个；被跳过的来源不启动。
func TestMulti_SkipOnTimeout(t *testing.T) {
	var events []string
	var mu sync.Mutex
	mk := func(name string, block bool) *fakeSource {
		return &fakeSource{name: name, events: &events, mu: &mu, needAuth: true, block: block}
	}
	a, b, c := mk("a", true), mk("b", true), mk("c", false)
	m := NewMulti(log.New(io.Discard, "", 0), a, b, c)
	m.SkipAfter = 20 * time.Millisecond
	if err := m.Run(context.Background(), func(ctx context.Context, msg Message) error { return nil }); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(events, ",")
	if strings.Contains(got, "run:a") || strings.Contains(got, "run:b") || !strings.Contains(got, "run:c") {
		t.Errorf("事件: %s", got)
	}
}

// 输入 s 立即跳过当前正在扫码的来源，最后一个也一样；全部跳过后等一会儿再返回 ErrNoSource。
func TestMulti_SkipByKey(t *testing.T) {
	var events []string
	var mu sync.Mutex
	a := &fakeSource{name: "a", events: &events, mu: &mu, needAuth: true, block: true}
	b := &fakeSource{name: "b", events: &events, mu: &mu, needAuth: true, block: true}
	m := NewMulti(log.New(io.Discard, "", 0), a, b)
	m.SkipAfter = time.Minute
	m.NoSourceDelay = 10 * time.Millisecond
	pr, pw := io.Pipe()
	defer pw.Close()
	m.SkipInput = pr

	has := func(e string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, x := range events {
			if x == e {
				return true
			}
		}
		return false
	}
	done := make(chan error, 1)
	go func() {
		done <- m.Run(context.Background(), func(ctx context.Context, msg Message) error { return nil })
	}()
	// 等对应来源开始扫码后再按 s，按键只对正在扫码的来源有效。
	for _, name := range []string{"a", "b"} {
		deadline := time.Now().Add(5 * time.Second)
		for !has("auth:"+name) && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		pw.Write([]byte("x\n"))
		pw.Write([]byte("S\n"))
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrNoSource) {
			t.Fatalf("err = %v，期望 ErrNoSource", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("按 s 后没有跳过")
	}
	if strings.Contains(strings.Join(events, ","), "run:") {
		t.Errorf("被跳过的来源不应启动: %v", events)
	}
}
