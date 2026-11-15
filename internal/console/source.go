// Package console 是调试用的消息来源：从控制台（标准输入）逐行读取，每一行当作一条消息。
package console

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"time"

	"clawbot-ilink-relay/internal/relay"
)

// Options 控制控制台来源的行为。
type Options struct {
	// CursorDelay 是消息要输入到当前光标时的等待时间。光标此时还在本控制台里，
	// 留出时间让用户切到目标窗口。
	CursorDelay time.Duration
	// ToCursor 报告下一条消息是否会输入到当前光标（即未锁定）。nil 表示总是。
	ToCursor func() bool
}

// Source 从 io.Reader 逐行读取消息。
type Source struct {
	in     io.Reader
	logger *log.Logger
	opts   Options
}

// New 创建控制台来源。
func New(in io.Reader, logger *log.Logger, opts Options) *Source {
	if logger == nil {
		logger = log.Default()
	}
	return &Source{in: in, logger: logger, opts: opts}
}

var _ relay.Source = (*Source)(nil)

// Name 实现 relay.Source。
func (s *Source) Name() string { return "控制台" }

// Run 实现 relay.Source。输入结束（EOF）或 ctx 结束时返回。
func (s *Source) Run(ctx context.Context, handle relay.Handler) error {
	s.logger.Println("消息来源：控制台。在这里输入一行文字并回车，就会当作一条消息发送；Ctrl+C 退出。")

	lines := make(chan string)
	errc := make(chan error, 1)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(s.in)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
		errc <- sc.Err()
	}()

	for {
		var line string
		select {
		case <-ctx.Done():
			return nil
		case l, ok := <-lines:
			if !ok {
				select {
				case err := <-errc:
					return err
				default:
					return nil
				}
			}
			line = strings.TrimSpace(l)
		}
		if line == "" {
			continue
		}

		if s.opts.CursorDelay > 0 && (s.opts.ToCursor == nil || s.opts.ToCursor()) {
			s.logger.Printf("%s 后输入到当前光标位置，请切到目标窗口……", s.opts.CursorDelay)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(s.opts.CursorDelay):
			}
		}

		err := handle(ctx, relay.Message{From: "控制台", Text: line})
		switch {
		case err == nil:
			s.logger.Printf("已发送: %s", line)
		case errors.Is(err, relay.ErrDiscarded):
			s.logger.Println("锁定的窗口或终端已关闭，这条没有发送。按 Alt+/ 解除锁定。")
		case ctx.Err() != nil:
			return nil
		default:
			s.logger.Printf("发送失败: %v", err)
		}
	}
}
