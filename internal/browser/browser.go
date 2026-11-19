// Package browser 用系统默认浏览器打开链接，供各来源打开扫码页面。无内部依赖。
package browser

import (
	"os/exec"
	"runtime"
)

// Open 用系统默认浏览器打开 u，不等待浏览器退出。链接是否合法由调用方检查。
func Open(u string) error {
	cmd := command(runtime.GOOS, u)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }() // 回收子进程
	return nil
}

// command 返回在 goos 上打开链接的命令：macOS 用 open，其余（Windows）用 rundll32。
func command(goos, u string) *exec.Cmd {
	if goos == "darwin" {
		return exec.Command("open", u)
	}
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
}
