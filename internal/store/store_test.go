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
	if s.Token() != "" || s.ContextTokens == nil || len(s.AckedIDs) != 0 {
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
	s.MarkAcked(7, 10)
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
	if got.Buf() != "buf1" || got.ContextTokens["u1"] != "ctx1" || !got.IsAcked(7) {
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

func TestMarkAcked(t *testing.T) {
	s := newStore(t)
	s.MarkAcked(0, 5) // 忽略 0
	if len(s.AckedIDs) != 0 {
		t.Fatalf("0 should be ignored: %v", s.AckedIDs)
	}
	s.MarkAcked(1, 5)
	s.MarkAcked(1, 5) // 去重
	if len(s.AckedIDs) != 1 {
		t.Fatalf("duplicate recorded: %v", s.AckedIDs)
	}
	if !s.IsAcked(1) || s.IsAcked(2) {
		t.Error("IsAcked wrong")
	}
}

func TestMarkAcked_TrimsToLimit(t *testing.T) {
	s := newStore(t)
	for i := int64(1); i <= 5; i++ {
		s.MarkAcked(i, 3)
	}
	want := []int64{3, 4, 5}
	if len(s.AckedIDs) != len(want) {
		t.Fatalf("AckedIDs = %v", s.AckedIDs)
	}
	for i, v := range want {
		if s.AckedIDs[i] != v {
			t.Fatalf("AckedIDs = %v, want %v", s.AckedIDs, want)
		}
	}
	if s.IsAcked(1) {
		t.Error("oldest id should be evicted")
	}
}

func TestMarkAcked_NonPositiveLimitUsesDefault(t *testing.T) {
	s := newStore(t)
	for i := int64(1); i <= defaultAckCap+10; i++ {
		s.MarkAcked(i, 0)
	}
	if len(s.AckedIDs) != defaultAckCap {
		t.Errorf("len = %d, want %d", len(s.AckedIDs), defaultAckCap)
	}
}

func TestReset(t *testing.T) {
	s := newStore(t)
	s.SetSession("tok", "b", "u", "base")
	s.SetBuf("x")
	s.SetContextToken("u", "c")
	s.MarkAcked(1, 5)
	s.Reset()
	if s.Token() != "" || s.Base() != "" || s.Buf() != "" || s.BotID != "" || s.UserID != "" ||
		len(s.ContextTokens) != 0 || s.ContextTokens == nil || len(s.AckedIDs) != 0 {
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
			s.MarkAcked(i, 100)
			s.SetBuf("b")
			s.SetContextToken("u", "c")
			_ = s.IsAcked(i)
			_ = s.Buf()
			_ = s.Save()
		}(int64(i))
	}
	wg.Wait()
	if len(s.AckedIDs) != 20 {
		t.Errorf("len = %d", len(s.AckedIDs))
	}
}
