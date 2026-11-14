// clawbot-ilink-relay 是一个自研的微信 iLink / ClawBot 客户端。
//
// 它替代 OpenClaw 的微信渠道：通过官方 iLink Bot API 扫码登录、长轮询接收
// 文本消息，然后在 Windows 上用 SendInput 的 Unicode 键事件把消息内容直接
// 输入到当前光标处并回车（不占用剪贴板）。程序仅依赖 Windows win32 API，
// 只能在 Windows 上运行。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"clawbot-ilink-relay/internal/config"
	"clawbot-ilink-relay/internal/ilink"
	"clawbot-ilink-relay/internal/store"
	"clawbot-ilink-relay/internal/wintype"
)

var logger = log.New(os.Stdout, "[clawbot] ", log.LstdFlags)

func main() {
	if runtime.GOOS != "windows" {
		logger.Fatal("clawbot-ilink-relay 仅支持 Windows")
	}
	wintype.SetupConsole()

	configPath := flag.String("config", "config.json", "配置文件路径")
	relogin := flag.Bool("relogin", false, "清除本地凭证并强制重新扫码登录")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Fatalf("加载配置失败: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		logger.Fatalf("配置不合法: %v", err)
	}

	tokenPath := cfg.TokenFile
	if !filepath.IsAbs(tokenPath) {
		tokenPath = filepath.Join(filepath.Dir(absPath(*configPath)), tokenPath)
	}

	st, err := store.Load(tokenPath)
	if err != nil {
		logger.Fatalf("加载凭证失败: %v", err)
	}
	if *relogin {
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := ilink.NewClient(cfg, st, logger)
	logger.Printf("clawbot-ilink-relay 启动，凭证文件: %s", tokenPath)
	logger.Println("提示：请先把目标窗口置于前台并点中输入框，收到消息后会直接键入当前光标并回车")

	if err := client.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Fatalf("运行中止: %v", err)
	}
	logger.Println("已退出")
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}
