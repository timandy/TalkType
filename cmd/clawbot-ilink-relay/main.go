// clawbot-ilink-relay 把收到的文本消息输入到当前光标处并回车（不占用剪贴板），
// 或输入到用 Alt+/ 锁定的窗口或终端（终端在后台直接写入，不切换窗口）。
//
// 消息来源可替换：
//   - wechat（默认）：通过官方 iLink Bot API 扫码登录、长轮询接收微信消息，替代 OpenClaw 的微信渠道；
//   - console：调试用，从本程序的控制台逐行读取输入。
//
// 程序仅依赖 Windows win32 API，只能在 Windows 上运行。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"clawbot-ilink-relay/internal/config"
	"clawbot-ilink-relay/internal/console"
	"clawbot-ilink-relay/internal/ilink"
	"clawbot-ilink-relay/internal/relay"
	"clawbot-ilink-relay/internal/store"
	"clawbot-ilink-relay/internal/winlock"
	"clawbot-ilink-relay/internal/wintype"
)

const consoleCursorDelay = 3 * time.Second

var logger = log.New(os.Stdout, "[clawbot] ", log.LstdFlags)

func main() {
	// 后台写终端的辅助子进程，必须在任何其他初始化之前处理。
	if len(os.Args) > 1 && os.Args[1] == wintype.HelperArg {
		os.Exit(wintype.RunConsoleHelper(os.Args[2:]))
	}

	if runtime.GOOS != "windows" {
		logger.Fatal("clawbot-ilink-relay 仅支持 Windows")
	}
	wintype.SetupConsole()

	configPath := flag.String("config", "config.json", "配置文件路径")
	sourceName := flag.String("source", "wechat", "消息来源：wechat（微信）或 console（调试用，从本窗口逐行输入）")
	relogin := flag.Bool("relogin", false, "清除本地凭证并强制重新扫码登录（仅 wechat）")
	lock := flag.Bool("lock", false, "在要接收消息的终端里运行：让正在运行的 clawbot-ilink-relay 锁定这个终端")
	unlock := flag.Bool("unlock", false, "让正在运行的 clawbot-ilink-relay 解除锁定")
	flag.Parse()

	if *lock || *unlock {
		os.Exit(runLockCommand(*lock))
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Fatalf("加载配置失败: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		logger.Fatalf("配置不合法: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	winlock.SetLogger(logger)
	locker := winlock.New()
	var injector relay.Injector = relay.CursorInjector{}
	toCursor := func() bool { return true }
	if err := locker.Start(ctx); err != nil {
		logger.Printf("窗口锁定功能不可用，将始终输入到当前光标: %v", err)
	} else {
		injector = locker
		toCursor = func() bool { return !locker.IsLocked() }
		logger.Println("Alt+/ 锁定当前窗口或终端（在别处按会切换过去，在锁定的目标里按会解除）；连按两下 Alt+/ 解除锁定")
	}

	var src relay.Source
	switch strings.ToLower(strings.TrimSpace(*sourceName)) {
	case "wechat":
		src, err = newWeChatSource(cfg, *configPath, *relogin)
	case "console":
		src = console.New(os.Stdin, logger, console.Options{CursorDelay: consoleCursorDelay, ToCursor: toCursor})
	default:
		err = fmt.Errorf("未知的消息来源 %q，可选 wechat 或 console", *sourceName)
	}
	if err != nil {
		logger.Fatal(err)
	}

	deliverer := relay.NewDeliverer(injector, injectOptions(cfg))
	logger.Printf("消息来源: %s", src.Name())
	if err := src.Run(ctx, deliverer.Deliver); err != nil && ctx.Err() == nil {
		logger.Fatalf("运行中止: %v", err)
	}
	logger.Println("已退出")
}

func injectOptions(cfg *config.Config) wintype.Options {
	return wintype.Options{
		TypeDelay:  time.Duration(cfg.Inject.TypeDelayMS) * time.Millisecond,
		EnterDelay: time.Duration(cfg.Inject.EnterDelayMS) * time.Millisecond,
		PressEnter: cfg.Inject.PressEnter,
		MaxRetries: cfg.Inject.MaxRetries,
	}
}

// newWeChatSource 加载凭证并创建微信来源。
func newWeChatSource(cfg *config.Config, configPath string, relogin bool) (*ilink.Client, error) {
	tokenPath := cfg.TokenFile
	if !filepath.IsAbs(tokenPath) {
		tokenPath = filepath.Join(filepath.Dir(absPath(configPath)), tokenPath)
	}

	st, err := store.Load(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("加载凭证失败: %w", err)
	}
	if relogin {
		if err := st.Delete(); err != nil {
			logger.Printf("清除本地凭证失败: %v", err)
		}
		logger.Println("已清除本地凭证，将重新扫码登录")
	}
	if st.Token() == "" && strings.TrimSpace(cfg.Token) != "" {
		st.SetSession(strings.TrimSpace(cfg.Token), "", "", "")
		if err := st.Save(); err != nil {
			logger.Printf("保存 config.token 失败: %v", err)
		}
	}
	logger.Printf("凭证文件: %s", tokenPath)
	logger.Println("提示：未锁定时，收到消息后会直接键入当前光标并回车，请先把目标窗口置于前台并点中输入框")
	return ilink.NewClient(cfg, st, logger), nil
}

// runLockCommand 处理 -lock / -unlock：通知正在运行的实例，然后退出。
// -lock 锁定的是运行这条命令的 shell（父进程）所在的终端。
func runLockCommand(lock bool) int {
	var err error
	if lock {
		err = winlock.SendLock(uint32(os.Getppid()))
	} else {
		err = winlock.SendUnlock()
	}
	if err != nil {
		logger.Printf("失败: %v", err)
		return 1
	}
	return 0
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}
