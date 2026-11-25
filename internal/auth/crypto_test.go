package auth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadOnlyCPASourceAndEncryptedCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "master.key")
	srcFile := filepath.Join(dir, "antigravity-test.json")
	cacheFile := filepath.Join(dir, "auth-cache.json")

	if err := os.WriteFile(keyFile, []byte("super-secret-test-key-material-32bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	srcPlain := `{
  "type": "antigravity",
  "email": "dev@example.com",
  "project_id": "demo-project",
  "access_token": "ya29.old-access",
  "refresh_token": "1//test-refresh",
  "expires_in": 3599,
  "timestamp": 1700000000000
}`
	if err := os.WriteFile(srcFile, []byte(srcPlain), 0600); err != nil {
		t.Fatal(err)
	}

	s := NewStore(srcFile, "", cacheFile, keyFile, "", "", "")
	loadedInitial, wasEnc, err := s.Load()
	if err != nil {
		t.Fatalf("Load initial: %v", err)
	}
	if wasEnc || loadedInitial.AccessToken != "ya29.old-access" {
		t.Fatalf("unexpected initial load: enc=%v token=%s", wasEnc, loadedInitial.AccessToken)
	}

	refreshed := *loadedInitial
	refreshed.AccessToken = "ya29.new-access"
	refreshed.Timestamp = time.Now().UnixMilli()
	if err := s.SaveCacheAtomic(&refreshed); err != nil {
		t.Fatalf("SaveCacheAtomic: %v", err)
	}

	// Source CPA JSON must remain byte-for-byte untouched!
	afterSrc, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterSrc) != srcPlain {
		t.Fatalf("source CPA JSON was modified!")
	}

	loaded, cacheEnc, err := s.Load()
	if err != nil {
		t.Fatalf("Load after cache save: %v", err)
	}
	if !cacheEnc {
		t.Fatalf("expected auth cache to be encrypted on disk")
	}
	if loaded.AccessToken != "ya29.new-access" || loaded.RefreshToken != "1//test-refresh" {
		t.Fatalf("mismatch after decrypt: got %+v", loaded)
	}
}
