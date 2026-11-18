package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWeComStoreRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "wecom_token.json")
	s, err := LoadWeCom(p)
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := s.Credentials(); id != "" {
		t.Fatal("new store should be empty")
	}
	s.SetCredentials("bot", "sec")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s2, err := LoadWeCom(p)
	if err != nil {
		t.Fatal(err)
	}
	if id, sec := s2.Credentials(); id != "bot" || sec != "sec" {
		t.Errorf("credentials = %s %s", id, sec)
	}
	if err := s2.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("file not deleted")
	}
	if id, _ := s2.Credentials(); id != "" {
		t.Error("credentials not cleared")
	}
	if err := s2.Delete(); err != nil {
		t.Errorf("second delete: %v", err)
	}
}

func TestLoadWeComInvalidJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "w.json")
	os.WriteFile(p, []byte("{"), 0o600)
	if _, err := LoadWeCom(p); err == nil {
		t.Fatal("expected parse error")
	}
}
