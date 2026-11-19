package source

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"
)

// DefaultSkipAfter 是单个来源等待扫码的最长时间，超时后跳过它，继续下一个。
const DefaultSkipAfter = 5 * time.Minute

// DefaultNoSourceDelay 是没有任何来源可用时，打印日志后到退出之间的等待时间。
const DefaultNoSourceDelay = 5 * time.Second

// ErrNoSource 表示所有来源都没有授权成功（被跳过或授权失败），没有任何来源在运行。
var ErrNoSource = errors.New("没有可用的消息来源")

var (
	errSkipTimeout = errors.New("等待扫码超时")
	errSkipKey     = errors.New("用户选择跳过")
)

// ErrAuthRequired 表示来源还没有可用的凭证（没有保存过，或被服务端明确拒绝），需要扫码。
var ErrAuthRequired = errors.New("需要扫码授权")

// Authorizer 由需要登录或扫码的来源实现。
type Authorizer interface {
	// Check 只检查已保存的凭证，不弹扫码。凭证有效，或因网络等原因暂时无法判断时返回 nil
	// （此时直接 Run，由 Run 自己退避重试）；没有凭证或凭证被服务端明确拒绝时返回
	// ErrAuthRequired，并清除失效的凭证。
	Check(ctx context.Context) error
	// Authorize 引导用户扫码并保存凭证，成功后才返回。
	Authorize(ctx context.Context) error
}

// Multi 同时运行多个来源。
type Multi struct {
	logger  *log.Logger
	sources []Source

	// SkipAfter 是单个来源等待扫码的最长时间，超时后跳过；0 表示用 DefaultSkipAfter。
	SkipAfter time.Duration
	// SkipInput 非空时，等待扫码期间读到一行 s（不区分大小写）就跳过当前来源。
	// 不能和同样读标准输入的来源（console）共用，此时留空。
	SkipInput io.Reader
	// NoSourceDelay 是没有任何来源可用时，打印日志后到退出的等待时间；0 表示用 DefaultNoSourceDelay。
	NoSourceDelay time.Duration
}

// NewMulti 按给定顺序组合多个来源。
func NewMulti(logger *log.Logger, sources ...Source) *Multi {
	return &Multi{logger: logger, sources: sources}
}

// Name 返回各来源名称，用顿号连接。
func (m *Multi) Name() string {
	names := make([]string, len(m.sources))
	for i, s := range m.sources {
		names[i] = s.Name()
	}
	return strings.Join(names, "、")
}

// Run 分两轮启动各来源：
//  1. 按配置顺序逐个检查凭证，有效的立即开始运行；没有凭证或凭证失效的先记下，不弹扫码。
//  2. 检查完后，按同样的顺序逐个弹扫码，前一个授权成功并开始运行后才弹下一个。
//
// 各来源收到的消息串行交给 handle。某个来源出错退出时，其余来源继续运行；
// 全部退出后返回各来源的错误。
func (m *Multi) Run(ctx context.Context, handle Handler) error {
	var handleMu sync.Mutex
	serial := func(ctx context.Context, msg Message) error {
		handleMu.Lock()
		defer handleMu.Unlock()
		return handle(ctx, msg)
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	fail := func(s Source, err error) {
		m.logger.Printf("%s 已停止: %v", s.Name(), err)
		mu.Lock()
		errs = append(errs, fmt.Errorf("%s: %w", s.Name(), err))
		mu.Unlock()
	}
	started := 0
	start := func(s Source) {
		started++
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Run(ctx, serial); err != nil && ctx.Err() == nil {
				fail(s, err)
			}
		}()
	}

	var pending []Source
	for _, s := range m.sources {
		if ctx.Err() != nil {
			break
		}
		a, ok := s.(Authorizer)
		if !ok {
			start(s)
			continue
		}
		if err := a.Check(ctx); err != nil {
			if ctx.Err() != nil {
				break
			}
			m.logger.Printf("%s 需要扫码授权，检查完所有来源后依次弹出", s.Name())
			pending = append(pending, s)
			continue
		}
		start(s)
	}
	// 输入协程只有一个，由它把 s 转给“当前正在扫码”的来源；没有来源在扫码时，输入被忽略。
	var (
		skipMu  sync.Mutex
		skipCur context.CancelCauseFunc
	)
	setCur := func(c context.CancelCauseFunc) {
		skipMu.Lock()
		skipCur = c
		skipMu.Unlock()
	}
	m.watchSkipInput(func() {
		skipMu.Lock()
		c := skipCur
		skipMu.Unlock()
		if c != nil {
			c(errSkipKey)
		}
	})
	for _, s := range pending {
		if ctx.Err() != nil {
			break
		}
		// 每个来源都可以跳过：超时，或在本窗口输入 s。
		authCtx, cancel := context.WithCancelCause(ctx)
		timer := time.AfterFunc(m.skipAfter(), func() { cancel(errSkipTimeout) })
		setCur(cancel)
		hint := fmt.Sprintf("%s 内未扫码将跳过", m.skipAfter())
		if m.SkipInput != nil {
			hint += "，也可以在本窗口输入 s 回车立即跳过"
		}
		m.logger.Printf("正在为 %s 扫码：%s", s.Name(), hint)
		err := s.(Authorizer).Authorize(authCtx)
		setCur(nil)
		timer.Stop()
		cause := context.Cause(authCtx)
		cancel(nil)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if errors.Is(cause, errSkipTimeout) || errors.Is(cause, errSkipKey) {
				m.logger.Printf("已跳过 %s 的扫码（%v），下次启动会再次检查", s.Name(), cause)
				continue
			}
			m.logger.Printf("%s 授权失败，跳过该来源: %v", s.Name(), err)
			mu.Lock()
			errs = append(errs, fmt.Errorf("%s: 授权失败: %w", s.Name(), err))
			mu.Unlock()
			continue
		}
		start(s)
	}
	if started == 0 && ctx.Err() == nil {
		delay := m.NoSourceDelay
		if delay <= 0 {
			delay = DefaultNoSourceDelay
		}
		m.logger.Printf("没有任何可用的消息来源（全部被跳过或授权失败），%s 后自动退出", delay)
		select {
		case <-ctx.Done():
		case <-time.After(delay):
		}
		errs = append(errs, ErrNoSource)
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (m *Multi) skipAfter() time.Duration {
	if m.SkipAfter > 0 {
		return m.SkipAfter
	}
	return DefaultSkipAfter
}

// watchSkipInput 在后台逐行读取 SkipInput，读到 s（不区分大小写）时调用 onSkip。
// 读取会阻塞到下一行或输入结束，所以只启动一个协程，由它转发给各来源。
func (m *Multi) watchSkipInput(onSkip func()) {
	if m.SkipInput == nil {
		return
	}
	go func() {
		sc := bufio.NewScanner(m.SkipInput)
		for sc.Scan() {
			if strings.EqualFold(strings.TrimSpace(sc.Text()), "s") {
				onSkip()
			}
		}
	}()
}
