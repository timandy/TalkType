// Package source 定义消息来源的接口。各来源在子包里：
//
//	wechat   微信（iLink Bot API）
//	feishu   飞书机器人
//	wecom    企业微信智能机器人
//	console  控制台（调试用）
//
// 来源只负责“拿到消息”以及它自己的确认语义（例如微信的 ACK 和游标），
// 把消息交给 Handler；投递到光标或锁定的目标由 internal/delivery 完成。
package source

import (
	"context"
	"errors"
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
