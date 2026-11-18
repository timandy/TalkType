// WeType 把收到的文本消息输入到当前光标处并回车（不占用剪贴板），
// 或输入到用 Alt+. 锁定的窗口或终端（终端在后台直接写入，不切换窗口）。
//
// 消息来源可替换：
//   - wechat（默认）：通过官方 iLink Bot API 扫码登录、长轮询接收微信消息，替代 OpenClaw 的微信渠道；
//   - feishu：扫码创建飞书机器人，通过长连接接收机器人单聊里的消息；
//   - wecom：扫码创建企业微信智能机器人，通过长连接接收机器人单聊里的消息；
//   - console：调试用，从本程序的控制台逐行读取输入。
//
// 程序仅依赖 Windows win32 API，只能在 Windows 上运行。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"wetype/internal/config"
	"wetype/internal/delivery"
	"wetype/internal/delivery/winlock"
	"wetype/internal/delivery/wintype"
	"wetype/internal/source"
	"wetype/internal/source/console"
	"wetype/internal/source/feishu"
	"wetype/internal/source/wechat"
	"wetype/internal/source/wecom"
	"wetype/internal/store"
)

const consoleCursorDelay = 3 * time.Second

// 凭证文件和已处理消息 ID 的记录文件（一行一个 ID），都放在配置文件所在目录。
const (
	wechatTokenFile = "wechat_token.json"
	feishuTokenFile = "feishu_token.json"
	wecomTokenFile  = "wecom_token.json"

	wechatAckedFile = "wechat_acked.ids"
	feishuAckedFile = "feishu_acked.ids"
	wecomAckedFile  = "wecom_acked.ids"
)

var logger = log.New(os.Stdout, "[wetype] ", log.LstdFlags)

func main() {
	// 后台写终端的辅助子进程，必须在任何其他初始化之前处理。
	if len(os.Args) > 1 && os.Args[1] == wintype.HelperArg {
		os.Exit(wintype.RunConsoleHelper(os.Args[2:]))
	}

	if runtime.GOOS != "windows" {
		logger.Fatal("本程序仅支持 Windows")
	}
	wintype.SetupConsole()

	configPath := flag.String("config", "config.json", "配置文件路径")
	sourceName := flag.String("source", "", "临时覆盖 config.json 的 source：wechat（微信）、feishu（飞书）、wecom（企业微信）或 console（调试用，从本窗口逐行输入），不区分大小写")
	relogin := flag.Bool("relogin", false, "清除本地凭证并强制重新扫码登录（wechat、feishu、wecom）")
	lock := flag.Bool("lock", false, "在目标终端中运行：通知正在运行的本程序实例锁定该终端")
	unlock := flag.Bool("unlock", false, "通知正在运行的本程序实例解除锁定")
	flag.Parse()

	if *lock || *unlock {
		os.Exit(runLockCommand(*lock))
	}

	cfg, err := config.Load(*configPath)
	if errors.Is(err, config.ErrNotFound) {
		logger.Printf("未找到配置文件 %s，使用默认配置", *configPath)
		err = nil
	} else if err != nil {
		logger.Fatalf("加载配置失败: %v", err)
	}
	if *sourceName != "" {
		cfg.Source = *sourceName
	}
	if err := cfg.Validate(); err != nil {
		logger.Fatalf("配置不合法: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	winlock.SetLogger(logger)
	locker := winlock.New()
	var injector delivery.Injector = delivery.CursorInjector{}
	toCursor := func() bool { return true }
	if err := locker.Start(ctx); err != nil {
		logger.Printf("锁定功能不可用，消息将始终输入到当前光标位置：%v", err)
	} else {
		injector = locker
		toCursor = func() bool { return !locker.IsLocked() }
		logger.Println("快捷键：Alt+. 锁定当前窗口或终端，在其他目标中按下则切换，在已锁定的目标中按下则解除；Alt+/ 在任意位置解除锁定")
	}

	var src source.Source
	switch cfg.Source {
	case config.SourceWeChat:
		src, err = newWeChatSource(cfg, *configPath, *relogin)
	case config.SourceFeishu:
		src, err = newFeishuSource(cfg, *configPath, *relogin)
	case config.SourceWeCom:
		src, err = newWeComSource(cfg, *configPath, *relogin)
	case config.SourceConsole:
		src = console.New(os.Stdin, logger, console.Options{CursorDelay: consoleCursorDelay, ToCursor: toCursor})
	}
	if err != nil {
		logger.Fatal(err)
	}

	deliverer := delivery.NewDeliverer(injector, injectOptions(cfg))
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
func newWeChatSource(cfg *config.Config, configPath string, relogin bool) (*wechat.Client, error) {
	tokenPath := resolvePath(configPath, wechatTokenFile)

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
	acked, err := store.LoadAckedIDs(filepath.Join(filepath.Dir(tokenPath), wechatAckedFile), cfg.Ack.MaxAckedIDs)
	if err != nil {
		return nil, fmt.Errorf("加载已处理消息记录失败: %w", err)
	}
	logger.Printf("凭证文件: %s", tokenPath)
	logger.Println("提示：未锁定时，消息将输入到当前光标位置并回车，请先激活目标窗口并定位到输入框")
	return wechat.NewClient(cfg, st, acked, logger), nil
}

// newFeishuSource 加载凭证并创建飞书来源。
func newFeishuSource(cfg *config.Config, configPath string, relogin bool) (*feishu.Client, error) {
	tokenPath := resolvePath(configPath, feishuTokenFile)
	st, err := store.LoadFeishu(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("加载飞书凭证失败: %w", err)
	}
	if relogin {
		if err := st.Delete(); err != nil {
			logger.Printf("清除飞书凭证失败: %v", err)
		}
		logger.Println("已清除本地飞书凭证，将重新扫码创建机器人")
	}
	acked, err := store.LoadAckedIDs(filepath.Join(filepath.Dir(tokenPath), feishuAckedFile), cfg.Ack.MaxAckedIDs)
	if err != nil {
		return nil, fmt.Errorf("加载已处理消息记录失败: %w", err)
	}
	logger.Printf("飞书凭证文件: %s", tokenPath)
	logger.Println("提示：未锁定时，消息将输入到当前光标位置并回车，请先激活目标窗口并定位到输入框")
	return feishu.NewClient(cfg, st, acked, logger), nil
}

// newWeComSource 加载凭证并创建企业微信来源。
func newWeComSource(cfg *config.Config, configPath string, relogin bool) (*wecom.Client, error) {
	tokenPath := resolvePath(configPath, wecomTokenFile)
	st, err := store.LoadWeCom(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("加载企业微信凭证失败: %w", err)
	}
	if relogin {
		if err := st.Delete(); err != nil {
			logger.Printf("清除企业微信凭证失败: %v", err)
		}
		logger.Println("已清除本地企业微信凭证，将重新扫码创建机器人")
	}
	acked, err := store.LoadAckedIDs(filepath.Join(filepath.Dir(tokenPath), wecomAckedFile), cfg.Ack.MaxAckedIDs)
	if err != nil {
		return nil, fmt.Errorf("加载已处理消息记录失败: %w", err)
	}
	logger.Printf("企业微信凭证文件: %s", tokenPath)
	logger.Println("提示：未锁定时，消息将输入到当前光标位置并回车，请先激活目标窗口并定位到输入框")
	return wecom.NewClient(cfg, st, acked, logger), nil
}

// resolvePath 把相对路径解析为相对配置文件所在目录。
func resolvePath(configPath, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(filepath.Dir(absPath(configPath)), p)
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
