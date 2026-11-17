package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFeishuStoreRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "feishu_token.json")
	s, err := LoadFeishu(p)
	if err != nil {
		t.Fatal(err)
	}
	if id, _, _, _ := s.Credentials(); id != "" {
		t.Fatal("new store should be empty")
	}
	s.SetCredentials("cli", "sec", "feishu", "ou")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Size() == 0 {
		t.Fatalf("file not written: %v", err)
	}
	s2, err := LoadFeishu(p)
	if err != nil {
		t.Fatal(err)
	}
	if id, sec, d, o := s2.Credentials(); id != "cli" || sec != "sec" || d != "feishu" || o != "ou" {
		t.Errorf("credentials = %s %s %s %s", id, sec, d, o)
	}
	s2.ClearCredentials()
	if id, _, _, _ := s2.Credentials(); id != "" {
		t.Error("ClearCredentials should clear credentials")
	}
	if err := s2.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("file not deleted")
	}
	if err := s2.Delete(); err != nil {
		t.Errorf("second delete: %v", err)
	}
}

func TestLoadFeishuInvalidJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.json")
	os.WriteFile(p, []byte("{"), 0o600)
	if _, err := LoadFeishu(p); err == nil {
		t.Fatal("expected parse error")
	}
}
