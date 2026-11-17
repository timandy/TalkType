package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// FeishuStore 保存飞书来源的凭证。已处理消息 ID 另存在 AckedIDs 里。
// 与微信的 TokenStore 分开存放，切换消息来源时互不覆盖。
type FeishuStore struct {
	path string

	mu          sync.RWMutex
	AppID       string `json:"app_id"`
	AppSecret   string `json:"app_secret"`
	Domain      string `json:"domain"`        // feishu 或 lark
	OwnerOpenID string `json:"owner_open_id"` // 扫码人的 open_id，默认只接受他的消息
}

// LoadFeishu 读取飞书凭证文件；文件不存在时返回空存储。
func LoadFeishu(path string) (*FeishuStore, error) {
	s := &FeishuStore{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	return s, nil
}

// Path 返回凭证文件路径。
func (s *FeishuStore) Path() string { return s.path }

// Save 原子写回凭证文件。
func (s *FeishuStore) Save() error {
	s.mu.RLock()
	data, err := json.MarshalIndent(s, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	return writeAtomic(s.path, data)
}

// Credentials 返回保存的应用凭证。
func (s *FeishuStore) Credentials() (appID, appSecret, domain, ownerOpenID string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.AppID, s.AppSecret, s.Domain, s.OwnerOpenID
}

// SetCredentials 保存扫码得到的应用凭证。
func (s *FeishuStore) SetCredentials(appID, appSecret, domain, ownerOpenID string) {
	s.mu.Lock()
	s.AppID, s.AppSecret, s.Domain, s.OwnerOpenID = appID, appSecret, domain, ownerOpenID
	s.mu.Unlock()
}

// ClearCredentials 清除应用凭证。
func (s *FeishuStore) ClearCredentials() {
	s.SetCredentials("", "", "", "")
}

// Delete 清空状态并删除凭证文件，用于 -relogin。
func (s *FeishuStore) Delete() error {
	s.mu.Lock()
	s.AppID, s.AppSecret, s.Domain, s.OwnerOpenID = "", "", "", ""
	s.mu.Unlock()
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// writeAtomic 先写临时文件再 rename，避免进程中断造成半截文件。权限 0600。
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
