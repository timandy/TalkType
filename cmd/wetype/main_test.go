package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveConfigPath(t *testing.T) {
	dir := filepath.Join(os.TempDir(), "app", "wetype")
	abs := filepath.Join(os.TempDir(), "etc", "wetype.json") // 带盘符的绝对路径
	for _, tc := range []struct{ dir, in, want string }{
		{dir, "config.json", filepath.Join(dir, "config.json")},
		{dir, filepath.Join("conf", "a.json"), filepath.Join(dir, "conf", "a.json")},
		{dir, abs, abs},                    // 绝对路径原样使用
		{"", "config.json", "config.json"}, // 取不到 exe 目录时保持原样
	} {
		if got := resolveConfigPath(tc.dir, tc.in); got != tc.want {
			t.Errorf("resolveConfigPath(%q, %q) = %q，期望 %q", tc.dir, tc.in, got, tc.want)
		}
	}
}
