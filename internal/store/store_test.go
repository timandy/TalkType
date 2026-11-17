package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func newStore(t *testing.T) *TokenStore {
	t.Helper()
	s, err := Load(filepath.Join(t.TempDir(), "sub", "token.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLoad_MissingFileReturnsEmptyStore(t *testing.T) {
	s := newStore(t)
	if s.Token() != "" || s.ContextTokens == nil {
		t.Errorf("unexpected state: %+v", s)
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.json")
	if err := os.WriteFile(p, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected error")
	}
}

func TestLoad_NullContextTokensInitialized(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.json")
	if err := os.WriteFile(p, []byte(`{"bot_token":"x","context_tokens":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.ContextTokens == nil {
		t.Fatal("ContextTokens should be non-nil")
	}
	s.SetContextToken("u", "c") // 不应 panic
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := newStore(t)
	s.SetSession("tok", "bot", "user", "https://base")
	s.SetBuf("buf1")
	s.SetContextToken("u1", "ctx1")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Path() + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("tmp file should be removed, stat err = %v", err)
	}

	got, err := Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got.Token() != "tok" || got.BotID != "bot" || got.UserID != "user" || got.Base() != "https://base" {
		t.Errorf("session mismatch: %+v", got)
	}
	if got.Buf() != "buf1" || got.ContextTokens["u1"] != "ctx1" {
		t.Errorf("state mismatch: %+v", got)
	}
}

func TestSetContextToken_IgnoresEmpty(t *testing.T) {
	s := newStore(t)
	s.SetContextToken("", "t")
	s.SetContextToken("u", "")
	if len(s.ContextTokens) != 0 {
		t.Errorf("ContextTokens = %v", s.ContextTokens)
	}
}

func TestReset(t *testing.T) {
	s := newStore(t)
	s.SetSession("tok", "b", "u", "base")
	s.SetBuf("x")
	s.SetContextToken("u", "c")
	s.Reset()
	if s.Token() != "" || s.Base() != "" || s.Buf() != "" || s.BotID != "" || s.UserID != "" ||
		len(s.ContextTokens) != 0 || s.ContextTokens == nil {
		t.Errorf("not reset: %+v", s)
	}
}

func TestDelete(t *testing.T) {
	s := newStore(t)
	// 文件不存在也不应报错
	if err := s.Delete(); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
	s.SetSession("tok", "b", "u", "base")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Errorf("file should be removed, stat err = %v", err)
	}
	if s.Token() != "" {
		t.Error("state should be reset")
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := newStore(t)
	var wg sync.WaitGroup
	for i := 1; i <= 20; i++ {
		wg.Add(1)
		go func(i int64) {
			defer wg.Done()
			s.SetBuf("b")
			s.SetContextToken("u", "c")
			_ = s.Buf()
			_ = s.Save()
		}(int64(i))
	}
	wg.Wait()
	if s.Buf() != "b" || s.ContextTokens["u"] != "c" {
		t.Errorf("state = %+v", s)
	}
}
