// Package store 负责 bot_token、长轮询游标、context_token 与 ACK 记录的持久化。
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const defaultAckCap = 500

// TokenStore 保存运行期凭证与游标。写入采用临时文件 + rename，避免进程中断
// 造成半截文件。
type TokenStore struct {
	path string

	mu            sync.RWMutex
	BotToken      string            `json:"bot_token"`
	BotID         string            `json:"bot_id"`
	UserID        string            `json:"user_id"`
	BaseURL       string            `json:"base_url"`
	GetUpdatesBuf string            `json:"get_updates_buf"`
	ContextTokens map[string]string `json:"context_tokens"`
	AckedIDs      []int64           `json:"acked_ids"`
}

// Load 读取凭证文件；文件不存在时返回空存储。
func Load(path string) (*TokenStore, error) {
	s := &TokenStore{path: path, ContextTokens: make(map[string]string)}
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
	if s.ContextTokens == nil {
		s.ContextTokens = make(map[string]string)
	}
	return s, nil
}

// Path 返回凭证文件路径。
func (s *TokenStore) Path() string { return s.path }

// Save 原子写回凭证文件。
func (s *TokenStore) Save() error {
	s.mu.RLock()
	data, err := json.MarshalIndent(s, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *TokenStore) Token() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.BotToken
}

func (s *TokenStore) Base() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.BaseURL
}

func (s *TokenStore) Buf() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.GetUpdatesBuf
}

func (s *TokenStore) SetBuf(buf string) {
	s.mu.Lock()
	s.GetUpdatesBuf = buf
	s.mu.Unlock()
}

// SetSession 更新登录会话信息。
func (s *TokenStore) SetSession(token, botID, userID, baseURL string) {
	s.mu.Lock()
	s.BotToken = token
	s.BotID = botID
	s.UserID = userID
	s.BaseURL = baseURL
	s.mu.Unlock()
}

func (s *TokenStore) SetContextToken(userID, token string) {
	if userID == "" || token == "" {
		return
	}
	s.mu.Lock()
	s.ContextTokens[userID] = token
	s.mu.Unlock()
}

func (s *TokenStore) IsAcked(id int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, v := range s.AckedIDs {
		if v == id {
			return true
		}
	}
	return false
}

// MarkAcked 记录已处理的消息 ID，并按 limit 保留最近的记录。
func (s *TokenStore) MarkAcked(id int64, limit int) {
	if id == 0 {
		return
	}
	if limit <= 0 {
		limit = defaultAckCap
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.AckedIDs {
		if v == id {
			return
		}
	}
	s.AckedIDs = append(s.AckedIDs, id)
	if len(s.AckedIDs) > limit {
		s.AckedIDs = append([]int64(nil), s.AckedIDs[len(s.AckedIDs)-limit:]...)
	}
}

// Reset 清空内存中的会话状态。
func (s *TokenStore) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.BotToken = ""
	s.BotID = ""
	s.UserID = ""
	s.BaseURL = ""
	s.GetUpdatesBuf = ""
	s.ContextTokens = make(map[string]string)
	s.AckedIDs = nil
}

// Delete 清空状态并删除凭证文件，用于会话过期后的重新登录。
func (s *TokenStore) Delete() error {
	s.Reset()
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
