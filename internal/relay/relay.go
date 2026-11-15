// Package relay 定义消息来源与投递的抽象：
//
//	Source（微信、控制台……） --Message--> Handler（Deliverer）--> 光标 / 锁定的窗口或终端
//
// 来源只负责“拿到消息”以及它自己的确认语义（例如微信的 ACK 和游标），
// 投递统一由 Deliverer 完成。
package relay

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"wetype/internal/wintype"
)

// Message 是一条待投递的文本消息。
type Message struct {
	From string // 发送者，用于日志
	Text string
}

// Handler 投递一条消息。
//   - 返回 nil：已投递；
//   - 返回的错误满足 errors.Is(err, ErrDiscarded)：目标已不存在，消息被丢弃，来源不应重试；
//   - 其他错误：投递失败，来源可以稍后重试。
type Handler func(ctx context.Context, m Message) error

// Source 是消息来源。Run 持续读取消息并交给 handle，直到 ctx 结束或来源耗尽。
type Source interface {
	Name() string
	Run(ctx context.Context, handle Handler) error
}

// ErrDiscarded 表示消息因为锁定的目标已关闭而被丢弃。
var ErrDiscarded = errors.New("锁定的目标已关闭，消息已丢弃")

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

// Deliver 实现 Handler。
func (d *Deliverer) Deliver(ctx context.Context, m Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	err := d.inj.Inject(m.Text, d.opts)
	if errors.Is(err, wintype.ErrTargetClosed) {
		d.inj.Notify(closedNoticeTitle, closedNoticeText)
		return fmt.Errorf("%w: %w", ErrDiscarded, err)
	}
	return err
}
