package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
)

// WeComStore 保存企业微信来源扫码得到的机器人凭证，以及配对得到的本人 userid。已处理消息 ID 另存在 AckedIDs 里。
type WeComStore struct {
	path string

	mu     sync.RWMutex
	BotID  string `json:"bot_id"`
	Secret string `json:"secret"`
	// OwnerUserID 是发送配对码完成配对的人；未配置白名单时只接受此人的消息。
	OwnerUserID string `json:"owner_userid"`
}

// LoadWeCom 读取企业微信凭证文件；文件不存在时返回空存储。
func LoadWeCom(path string) (*WeComStore, error) {
	s := &WeComStore{path: path}
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
func (s *WeComStore) Path() string { return s.path }

// Save 原子写回凭证文件。
func (s *WeComStore) Save() error {
	s.mu.RLock()
	data, err := json.MarshalIndent(s, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	return writeAtomic(s.path, data)
}

// Credentials 返回保存的机器人凭证。
func (s *WeComStore) Credentials() (botID, secret string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.BotID, s.Secret
}

// SetCredentials 保存扫码得到的机器人凭证。换了机器人时清除配对记录。
func (s *WeComStore) SetCredentials(botID, secret string) {
	s.mu.Lock()
	if botID != s.BotID {
		s.OwnerUserID = ""
	}
	s.BotID, s.Secret = botID, secret
	s.mu.Unlock()
}

// Owner 返回配对的本人 userid，未配对时为空。
func (s *WeComStore) Owner() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.OwnerUserID
}

// SetOwner 记录配对的本人 userid。
func (s *WeComStore) SetOwner(userID string) {
	s.mu.Lock()
	s.OwnerUserID = userID
	s.mu.Unlock()
}

// Delete 清空凭证和配对记录并删除文件，用于 -relogin 和凭证失效后重新扫码。
func (s *WeComStore) Delete() error {
	s.SetCredentials("", "")
	s.SetOwner("")
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
