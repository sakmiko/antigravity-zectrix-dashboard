package auth

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const envelopeFormat = "ag-zectrix-aes256gcm-v1"

// DefaultOAuthClientID / DefaultOAuthClientSecret 默认留空：仓库不内置任何 OAuth 客户端凭据。
// CPA JSON 未携带 client_id/client_secret 时，请自行在 config.toml 的 [auth] 段
// （oauth_client_id / oauth_client_secret）或环境变量 ANTIGRAVITY_OAUTH_CLIENT_ID/_SECRET 提供。
// 也可在构建时注入：go build -ldflags "-X '...internal/auth.DefaultOAuthClientSecret=xxx'"
var (
	DefaultOAuthClientID     = ""
	DefaultOAuthClientSecret = ""
)

// CPACredential represents CLIProxyAPI's Antigravity auth JSON schema
// (typically stored as antigravity-<email>.json under CPA auth-dir).
type CPACredential struct {
	Type         string `json:"type,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Email        string `json:"email,omitempty"`
	ProjectID    string `json:"project_id,omitempty"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	TokenURI     string `json:"token_uri,omitempty"`
	ExpiresIn    int64  `json:"expires_in,omitempty"`
	Timestamp    int64  `json:"timestamp,omitempty"`
	Expired      string `json:"expired,omitempty"`
	ProxyURL     string `json:"proxy_url,omitempty"`
}

type cachedAuthEnvelope struct {
	SourceFile string        `json:"source_file,omitempty"`
	CachedAt   time.Time     `json:"cached_at"`
	Credential CPACredential `json:"credential"`
}

type encryptedEnvelope struct {
	Format     string `json:"format"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// Store reads CLIProxyAPI Antigravity JSON files strictly read-only and stores
// refreshed tokens in a separate local cache file (optionally AES-256-GCM encrypted).
type Store struct {
	mu           sync.RWMutex
	Path         string
	AccountEmail string
	CacheFile    string
	KeyFile      string
	FallbackCID  string
	FallbackCSec string
	HTTPProxyURL string
}

func NewStore(path, accountEmail, cacheFile, keyFile, fallbackCID, fallbackCSec, proxyURL string) *Store {
	return &Store{
		Path:         path,
		AccountEmail: strings.TrimSpace(accountEmail),
		CacheFile:    cacheFile,
		KeyFile:      keyFile,
		FallbackCID:  fallbackCID,
		FallbackCSec: fallbackCSec,
		HTTPProxyURL: proxyURL,
	}
}

func (s *Store) UpdateConfig(path, accountEmail, cacheFile, keyFile, fallbackCID, fallbackCSec, proxyURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Path = path
	s.AccountEmail = strings.TrimSpace(accountEmail)
	s.CacheFile = cacheFile
	s.KeyFile = keyFile
	s.FallbackCID = fallbackCID
	s.FallbackCSec = fallbackCSec
	s.HTTPProxyURL = proxyURL
}

func (s *Store) snapshotConfig() (path, email, cacheFile, keyFile, cid, csec, proxy string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Path, s.AccountEmail, s.CacheFile, s.KeyFile, s.FallbackCID, s.FallbackCSec, s.HTTPProxyURL
}

// ResolveAuthFile resolves a direct CPA JSON file or selects a matching antigravity-*.json
// inside a CPA auth directory (matching AccountEmail if configured, else newest mtime).
func (s *Store) ResolveAuthFile() (string, error) {
	path, accountEmail, _, _, _, _, _ := s.snapshotConfig()
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat CPA auth path %q: %w", path, err)
	}
	if !info.IsDir() {
		return path, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", fmt.Errorf("read CPA auth dir %q: %w", path, err)
	}
	type candidate struct {
		path    string
		modTime time.Time
	}
	var candidates []candidate
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		lower := strings.ToLower(e.Name())
		if strings.Contains(lower, "antigravity") || strings.Contains(lower, "agy") {
			fi, err := e.Info()
			var mt time.Time
			if err == nil {
				mt = fi.ModTime()
			}
			candidates = append(candidates, candidate{
				path:    filepath.Join(path, e.Name()),
				modTime: mt,
			})
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no antigravity-*.json found in %q", path)
	}

	if accountEmail != "" {
		want := strings.ToLower(accountEmail)
		for _, c := range candidates {
			if cred, err := s.readRawCredential(c.path); err == nil {
				if strings.EqualFold(strings.TrimSpace(cred.Email), want) {
					return c.path, nil
				}
			}
			if strings.Contains(strings.ToLower(filepath.Base(c.path)), want) {
				return c.path, nil
			}
		}
		return "", fmt.Errorf("no CPA antigravity JSON matching email %q in %q", accountEmail, path)
	}

	// Prefer newest modified file; deterministic tie-break by filename.
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].modTime.Equal(candidates[j].modTime) {
			return candidates[i].modTime.After(candidates[j].modTime)
		}
		return candidates[i].path < candidates[j].path
	})
	return candidates[0].path, nil
}

func (s *Store) readRawCredential(file string) (*CPACredential, error) {
	_, _, _, keyFile, _, _, _ := s.snapshotConfig()
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read auth file: %w", err)
	}

	var env encryptedEnvelope
	if err := json.Unmarshal(raw, &env); err == nil && env.Format == envelopeFormat && env.Ciphertext != "" {
		if keyFile == "" {
			return nil, errors.New("auth file is encrypted but CACHE_ENCRYPTION_KEY_FILE is not set")
		}
		key, err := deriveKey(keyFile)
		if err != nil {
			return nil, fmt.Errorf("derive decryption key: %w", err)
		}
		plain, err := decryptAESGCM(key, env)
		if err != nil {
			return nil, fmt.Errorf("decrypt CPA auth JSON: %w", err)
		}
		raw = plain
	}

	var cred CPACredential
	if err := json.Unmarshal(raw, &cred); err != nil {
		return nil, fmt.Errorf("parse CPA Antigravity JSON: %w", err)
	}
	if cred.AccessToken == "" && cred.RefreshToken == "" {
		return nil, errors.New("CPA auth JSON missing both access_token and refresh_token")
	}
	return &cred, nil
}

// Load reads the source CPA JSON read-only and merges any fresher refreshed credential
// from the separate AuthCacheFile (never mutating the source CPA JSON).
func (s *Store) Load() (*CPACredential, bool, error) {
	file, err := s.ResolveAuthFile()
	if err != nil {
		return nil, false, err
	}
	srcCred, err := s.readRawCredential(file)
	if err != nil {
		return nil, false, err
	}

	cached, cacheEncrypted, err := s.loadCache()
	if err == nil && cached != nil {
		sameAccount := (srcCred.Email != "" && strings.EqualFold(srcCred.Email, cached.Credential.Email)) ||
			(srcCred.RefreshToken != "" && srcCred.RefreshToken == cached.Credential.RefreshToken) ||
			(cached.SourceFile == file)
		if sameAccount && isFresher(&cached.Credential, srcCred) {
			merged := *srcCred
			if cached.Credential.AccessToken != "" {
				merged.AccessToken = cached.Credential.AccessToken
			}
			if cached.Credential.RefreshToken != "" {
				merged.RefreshToken = cached.Credential.RefreshToken
			}
			if cached.Credential.ProjectID != "" {
				merged.ProjectID = cached.Credential.ProjectID
			}
			if cached.Credential.ExpiresIn > 0 {
				merged.ExpiresIn = cached.Credential.ExpiresIn
			}
			if cached.Credential.Timestamp > 0 {
				merged.Timestamp = cached.Credential.Timestamp
			}
			if cached.Credential.Expired != "" {
				merged.Expired = cached.Credential.Expired
			}
			return &merged, cacheEncrypted, nil
		}
	}
	return srcCred, false, nil
}

func isFresher(a, b *CPACredential) bool {
	if b.AccessToken == "" && a.AccessToken != "" {
		return true
	}
	ta := credentialUpdatedAt(a)
	tb := credentialUpdatedAt(b)
	if !ta.IsZero() && !tb.IsZero() {
		return ta.After(tb)
	}
	return a.Timestamp > b.Timestamp
}

func credentialUpdatedAt(c *CPACredential) time.Time {
	if c.Timestamp > 0 {
		// Handle both milliseconds and seconds timestamps.
		if c.Timestamp > 1e12 {
			return time.UnixMilli(c.Timestamp)
		}
		return time.Unix(c.Timestamp, 0)
	}
	if c.Expired != "" && c.ExpiresIn > 0 {
		if exp, err := time.Parse(time.RFC3339, c.Expired); err == nil {
			return exp.Add(-time.Duration(c.ExpiresIn) * time.Second)
		}
	}
	return time.Time{}
}

// IsTokenExpiringSoon returns true if access_token is empty or expires within margin.
func IsTokenExpiringSoon(c *CPACredential, margin time.Duration) bool {
	if c == nil || c.AccessToken == "" {
		return true
	}
	now := time.Now().UTC()
	if c.Expired != "" {
		if exp, err := time.Parse(time.RFC3339, c.Expired); err == nil {
			return now.Add(margin).After(exp)
		}
	}
	if c.Timestamp > 0 && c.ExpiresIn > 0 {
		issued := credentialUpdatedAt(c)
		exp := issued.Add(time.Duration(c.ExpiresIn) * time.Second)
		return now.Add(margin).After(exp)
	}
	return false
}

func (s *Store) loadCache() (*cachedAuthEnvelope, bool, error) {
	_, _, cacheFile, keyFile, _, _, _ := s.snapshotConfig()
	if cacheFile == "" {
		return nil, false, os.ErrNotExist
	}
	raw, err := os.ReadFile(cacheFile)
	if err != nil {
		return nil, false, err
	}
	isEnc := false
	var env encryptedEnvelope
	if err := json.Unmarshal(raw, &env); err == nil && env.Format == envelopeFormat && env.Ciphertext != "" {
		isEnc = true
		if keyFile == "" {
			return nil, true, errors.New("auth cache encrypted but CACHE_ENCRYPTION_KEY_FILE is unset")
		}
		key, err := deriveKey(keyFile)
		if err != nil {
			return nil, true, err
		}
		plain, err := decryptAESGCM(key, env)
		if err != nil {
			return nil, true, err
		}
		raw = plain
	}
	var cached cachedAuthEnvelope
	if err := json.Unmarshal(raw, &cached); err != nil {
		return nil, isEnc, err
	}
	return &cached, isEnc, nil
}

// SaveCacheAtomic writes the refreshed credential into the separate AuthCacheFile
// (never touching the original CPA JSON file).
func (s *Store) SaveCacheAtomic(cred *CPACredential) error {
	_, _, cacheFile, keyFile, _, _, _ := s.snapshotConfig()
	if cacheFile == "" {
		return errors.New("auth cache file path is empty")
	}
	srcFile, _ := s.ResolveAuthFile()
	record := cachedAuthEnvelope{
		SourceFile: srcFile,
		CachedAt:   time.Now().UTC(),
		Credential: *cred,
	}
	plain, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	var payload []byte
	if keyFile != "" {
		key, err := deriveKey(keyFile)
		if err != nil {
			return fmt.Errorf("derive encryption key: %w", err)
		}
		env, err := encryptAESGCM(key, plain)
		if err != nil {
			return fmt.Errorf("encrypt auth cache JSON: %w", err)
		}
		payload, err = json.MarshalIndent(env, "", "  ")
		if err != nil {
			return err
		}
	} else {
		payload = plain
	}

	dir := filepath.Dir(cacheFile)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create auth cache dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".ag-auth-cache-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp auth cache file: %w", err)
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, cacheFile)
}

// RefreshToken refreshes the Google OAuth access_token with built-in Antigravity
// client fallback and cold-proxy EOF retry.
func (s *Store) RefreshToken(ctx context.Context, cred *CPACredential) (*CPACredential, error) {
	if cred.RefreshToken == "" {
		return nil, errors.New("cannot refresh: refresh_token is empty")
	}
	_, _, _, _, fallbackCID, fallbackCSec, proxyURL := s.snapshotConfig()

	tokenURI := cred.TokenURI
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}
	cid := cred.ClientID
	if cid == "" {
		cid = fallbackCID
	}
	if cid == "" {
		cid = DefaultOAuthClientID
	}
	csec := cred.ClientSecret
	if csec == "" {
		csec = fallbackCSec
	}
	if csec == "" && cid == DefaultOAuthClientID {
		csec = DefaultOAuthClientSecret
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", cred.RefreshToken)
	form.Set("client_id", cid)
	if csec != "" {
		form.Set("client_secret", csec)
	}

	client := buildHTTPClient(cred.ProxyURL, proxyURL, 20*time.Second)

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURI, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", "antigravity-zectrix-dashboard/1.1")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("oauth refresh HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var tok struct {
			AccessToken  string `json:"access_token"`
			ExpiresIn    int64  `json:"expires_in"`
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.Unmarshal(body, &tok); err != nil {
			return nil, fmt.Errorf("decode oauth refresh response: %w", err)
		}
		if tok.AccessToken == "" {
			return nil, errors.New("oauth refresh returned empty access_token")
		}

		updated := *cred
		updated.AccessToken = tok.AccessToken
		if tok.RefreshToken != "" {
			updated.RefreshToken = tok.RefreshToken
		}
		if tok.ExpiresIn > 0 {
			updated.ExpiresIn = tok.ExpiresIn
			updated.Expired = time.Now().UTC().Add(time.Duration(tok.ExpiresIn) * time.Second).Format(time.RFC3339)
		}
		updated.Timestamp = time.Now().UnixMilli()
		return &updated, nil
	}
	return nil, fmt.Errorf("oauth refresh transport error: %w", lastErr)
}

func buildHTTPClient(credProxy, defaultProxy string, timeout time.Duration) *http.Client {
	tr := &http.Transport{
		IdleConnTimeout: 30 * time.Second,
	}
	proxyRaw := strings.TrimSpace(credProxy)
	if proxyRaw == "" {
		proxyRaw = strings.TrimSpace(defaultProxy)
	}
	if proxyRaw != "" {
		if u, err := url.Parse(proxyRaw); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

func deriveKey(keyFile string) ([]byte, error) {
	keyMaterial, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read CACHE_ENCRYPTION_KEY_FILE: %w", err)
	}
	machineID := readMachineID()
	h := sha256.New()
	h.Write([]byte("antigravity-zectrix-aes256gcm-v1:"))
	h.Write(bytes.TrimSpace(keyMaterial))
	h.Write([]byte(":"))
	h.Write([]byte(machineID))
	return h.Sum(nil), nil
}

func readMachineID() string {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(p); err == nil {
			if s := strings.TrimSpace(string(b)); s != "" {
				return s
			}
		}
	}
	host, _ := os.Hostname()
	return "host:" + host
}

func encryptAESGCM(key, plaintext []byte) (encryptedEnvelope, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return encryptedEnvelope{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return encryptedEnvelope{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return encryptedEnvelope{}, err
	}
	ct := gcm.Seal(nil, nonce, plaintext, []byte(envelopeFormat))
	return encryptedEnvelope{
		Format:     envelopeFormat,
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ct),
	}, nil
}

func decryptAESGCM(key []byte, env encryptedEnvelope) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil {
		return nil, err
	}
	ct, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ct, []byte(envelopeFormat))
}
