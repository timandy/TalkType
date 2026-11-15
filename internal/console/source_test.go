package console

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"wetype/internal/relay"
)

func TestRun_SendsNonEmptyLinesUntilEOF(t *testing.T) {
	var got []relay.Message
	s := New(strings.NewReader("hello\n\n   \n  你好 世界  \r\nlast"), log.New(io.Discard, "", 0), Options{})
	err := s.Run(context.Background(), func(_ context.Context, m relay.Message) error {
		got = append(got, m)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"hello", "你好 世界", "last"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i, w := range want {
		if got[i].Text != w || got[i].From != "控制台" {
			t.Errorf("msg %d = %+v, want %q", i, got[i], w)
		}
	}
	if s.Name() != "控制台" {
		t.Errorf("Name = %q", s.Name())
	}
}

func TestRun_ReportsResults(t *testing.T) {
	var out bytes.Buffer
	results := []error{nil, relay.ErrDiscarded, errors.New("boom")}
	i := 0
	s := New(strings.NewReader("a\nb\nc\n"), log.New(&out, "", 0), Options{})
	if err := s.Run(context.Background(), func(context.Context, relay.Message) error {
		err := results[i]
		i++
		return err
	}); err != nil {
		t.Fatal(err)
	}
	log := out.String()
	for _, want := range []string{"已发送：a", "消息未发送", "发送失败：boom"} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
}

func TestRun_CursorDelayOnlyWhenToCursor(t *testing.T) {
	delay := 80 * time.Millisecond
	toCursor := true
	var stamps []time.Duration
	start := time.Now()
	s := New(strings.NewReader("a\nb\n"), log.New(io.Discard, "", 0), Options{
		CursorDelay: delay,
		ToCursor:    func() bool { return toCursor },
	})
	_ = s.Run(context.Background(), func(context.Context, relay.Message) error {
		stamps = append(stamps, time.Since(start))
		toCursor = false // 第二条模拟已锁定
		return nil
	})
	if len(stamps) != 2 {
		t.Fatalf("stamps = %v", stamps)
	}
	if stamps[0] < delay {
		t.Errorf("first message should wait %v, waited %v", delay, stamps[0])
	}
	if stamps[1]-stamps[0] >= delay {
		t.Errorf("second message should not wait, gap %v", stamps[1]-stamps[0])
	}
}

func TestRun_StopsOnCancel(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- New(r, log.New(io.Discard, "", 0), Options{}).Run(ctx, func(context.Context, relay.Message) error { return nil })
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("err = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
