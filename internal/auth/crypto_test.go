package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEncryptAndDecryptRoundTrip(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "master.key")
	authFile := filepath.Join(dir, "antigravity-test.json")

	if err := os.WriteFile(keyFile, []byte("super-secret-test-key-material-32bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	orig := &CPACredential{
		Type:         "antigravity",
		Email:        "dev@example.com",
		ProjectID:    "demo-project",
		AccessToken:  "ya29.test-access",
		RefreshToken: "1//test-refresh",
	}
	s := NewStore(authFile, keyFile, "", "", "")
	if err := s.SaveAtomic(orig); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	loaded, wasEnc, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !wasEnc {
		t.Fatalf("expected file to be encrypted on disk")
	}
	if loaded.AccessToken != orig.AccessToken || loaded.RefreshToken != orig.RefreshToken {
		t.Fatalf("mismatch after decrypt: got %+v", loaded)
	}
}
