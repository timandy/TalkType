package wintype

import (
	"errors"
	"time"
)

// Options 控制文本输入行为。
type Options struct {
	TypeDelay  time.Duration // 文本输入完成后的等待
	EnterDelay time.Duration // 回车前的等待
	PressEnter bool          // 是否在文本后发送回车
	MaxRetries int           // 失败重试次数
}

// ErrTargetClosed 表示锁定的目标窗口已经不存在。
var ErrTargetClosed = errors.New("wintype: 目标窗口已关闭")
