// Package delivery 把消息投递到目标：当前光标，或用 Alt+. 锁定的窗口 / 终端。
//
//	source.Source --source.Message--> Deliverer --> Injector（winlock 或 CursorInjector）--> wintype 键入
//
// 子包：winlock 负责快捷键锁定与目标识别，wintype 负责 Win32 键入和后台写终端。
package delivery

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/timandy/talktype/internal/delivery/wintype"
	"github.com/timandy/talktype/internal/source"
)

// 目标关闭时的通知文案。
const (
	closedNoticeTitle = "消息未发送"
	closedNoticeText  = "锁定的目标已关闭，消息已丢弃\n按 Alt+/ 解除锁定，或在新目标中按 Alt+. 重新锁定"
)

// Injector 把文本输入到目标，并能在需要时通知用户。
// 目标已关闭时 Inject 返回 wintype.ErrTargetClosed。
type Injector interface {
	Inject(text string, opts wintype.Options) error
	Notify(title, msg string)
}

// CursorInjector 输入到当前光标处，不弹通知；锁定功能不可用时使用。
type CursorInjector struct{}

func (CursorInjector) Inject(text string, opts wintype.Options) error {
	return wintype.TypeWithRetry(text, opts)
}

func (CursorInjector) Notify(string, string) {}

// Deliverer 把消息投递到 Injector。多个来源并发调用时按顺序逐条输入，避免文字交错。
type Deliverer struct {
	mu   sync.Mutex
	inj  Injector
	opts wintype.Options
}

// NewDeliverer 创建投递器；inj 为 nil 时输入到当前光标。
func NewDeliverer(inj Injector, opts wintype.Options) *Deliverer {
	if inj == nil {
		inj = CursorInjector{}
	}
	return &Deliverer{inj: inj, opts: opts}
}

// Deliver 实现 source.Handler。
func (d *Deliverer) Deliver(ctx context.Context, m source.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	err := d.inj.Inject(m.Text, d.opts)
	if errors.Is(err, wintype.ErrTargetClosed) {
		d.inj.Notify(closedNoticeTitle, closedNoticeText)
		return fmt.Errorf("%w: %w", source.ErrDiscarded, err)
	}
	return err
}
