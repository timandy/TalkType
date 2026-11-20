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

	"github.com/timandy/talktype/internal/source"
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

var _ source.Source = (*Source)(nil)

// Name 实现 source.Source。
// SourceName 是来源的显示名称。
const SourceName = "控制台"

func (s *Source) Name() string { return SourceName }

// Run 实现 source.Source。输入结束（EOF）或 ctx 结束时返回。
func (s *Source) Run(ctx context.Context, handle source.Handler) error {
	s.logger.Println("消息来源：控制台。每输入一行并回车即发送一条消息，按 Ctrl+C 退出。")

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
			s.logger.Printf("将在 %s 后输入到当前光标位置，请切换至目标窗口。", s.opts.CursorDelay)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(s.opts.CursorDelay):
			}
		}

		err := handle(ctx, source.Message{From: "控制台", Text: line})
		switch {
		case err == nil:
			s.logger.Printf("已发送：%s", line)
		case errors.Is(err, source.ErrDiscarded):
			s.logger.Println("消息未发送：锁定的目标已关闭。")
		case ctx.Err() != nil:
			return nil
		default:
			s.logger.Printf("发送失败：%v", err)
		}
	}
}
