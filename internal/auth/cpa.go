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
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const envelopeFormat = "ag-zectrix-aes256gcm-v1"

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

type encryptedEnvelope struct {
	Format     string `json:"format"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// Store manages reading, decrypting, refreshing, and atomically encrypting CPA Antigravity JSON files.
type Store struct {
	Path         string
	KeyFile      string
	FallbackCID  string
	FallbackCSec string
	HTTPProxyURL string
	resolvedFile string
}

func NewStore(path, keyFile, fallbackCID, fallbackCSec, proxyURL string) *Store {
	return &Store{
		Path:         path,
		KeyFile:      keyFile,
		FallbackCID:  fallbackCID,
		FallbackCSec: fallbackCSec,
		HTTPProxyURL: proxyURL,
	}
}

// ResolveAuthFile resolves either a direct JSON file or the latest antigravity-*.json in a CPA auth-dir.
func (s *Store) ResolveAuthFile() (string, error) {
	info, err := os.Stat(s.Path)
	if err != nil {
		// 目标文件尚不存在时按"待创建的目标文件"处理，让 SaveAtomic 能首次落盘
		// （首次部署 / 换机 / 自动化测试都走这条路径）
		if errors.Is(err, fs.ErrNotExist) && !strings.HasSuffix(s.Path, string(os.PathSeparator)) {
			s.resolvedFile = s.Path
			return s.Path, nil
		}
		return "", fmt.Errorf("stat CPA auth path %q: %w", s.Path, err)
	}
	if !info.IsDir() {
		s.resolvedFile = s.Path
		return s.Path, nil
	}
	entries, err := os.ReadDir(s.Path)
	if err != nil {
		return "", fmt.Errorf("read CPA auth dir %q: %w", s.Path, err)
	}
	var candidates []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		lower := strings.ToLower(e.Name())
		if strings.Contains(lower, "antigravity") || strings.Contains(lower, "agy") {
			candidates = append(candidates, filepath.Join(s.Path, e.Name()))
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no antigravity-*.json found in %q", s.Path)
	}
	sort.Strings(candidates)
	s.resolvedFile = candidates[0]
	return s.resolvedFile, nil
}

// Load reads and (if encrypted) decrypts the CPA Antigravity JSON credential.
func (s *Store) Load() (*CPACredential, bool, error) {
	file, err := s.ResolveAuthFile()
	if err != nil {
		return nil, false, err
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, false, fmt.Errorf("read auth file: %w", err)
	}

	var env encryptedEnvelope
	isEncrypted := false
	if err := json.Unmarshal(raw, &env); err == nil && env.Format == envelopeFormat && env.Ciphertext != "" {
		isEncrypted = true
		key, err := s.deriveKey()
		if err != nil {
			return nil, true, fmt.Errorf("derive decryption key: %w", err)
		}
		plain, err := decryptAESGCM(key, env)
		if err != nil {
			return nil, true, fmt.Errorf("decrypt CPA auth JSON: %w", err)
		}
		raw = plain
	}

	var cred CPACredential
	if err := json.Unmarshal(raw, &cred); err != nil {
		return nil, isEncrypted, fmt.Errorf("parse CPA Antigravity JSON: %w", err)
	}
	if cred.AccessToken == "" && cred.RefreshToken == "" {
		return nil, isEncrypted, errors.New("CPA auth JSON missing both access_token and refresh_token")
	}
	return &cred, isEncrypted, nil
}

// SaveAtomic writes the credential back atomically; if CACHE_ENCRYPTION_KEY_FILE is set,
// it encrypts in-place with AES-256-GCM (matching codex-zectrix-dashboard security model).
func (s *Store) SaveAtomic(cred *CPACredential) error {
	file, err := s.ResolveAuthFile()
	if err != nil {
		return err
	}
	plain, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return err
	}
	var payload []byte
	if s.KeyFile != "" {
		key, err := s.deriveKey()
		if err != nil {
			return fmt.Errorf("derive encryption key: %w", err)
		}
		env, err := encryptAESGCM(key, plain)
		if err != nil {
			return fmt.Errorf("encrypt CPA auth JSON: %w", err)
		}
		payload, err = json.MarshalIndent(env, "", "  ")
		if err != nil {
			return err
		}
	} else {
		payload = plain
	}

	dir := filepath.Dir(file)
	tmp, err := os.CreateTemp(dir, ".ag-auth-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp auth file: %w", err)
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
	return os.Rename(tmpName, file)
}

// RefreshToken refreshes the Google OAuth access_token with cold-proxy EOF retry.
func (s *Store) RefreshToken(ctx context.Context, cred *CPACredential) (*CPACredential, error) {
	if cred.RefreshToken == "" {
		return nil, errors.New("cannot refresh: refresh_token is empty")
	}
	tokenURI := cred.TokenURI
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}
	cid := cred.ClientID
	if cid == "" {
		cid = s.FallbackCID
	}
	csec := cred.ClientSecret
	if csec == "" {
		csec = s.FallbackCSec
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", cred.RefreshToken)
	if cid != "" {
		form.Set("client_id", cid)
	}
	if csec != "" {
		form.Set("client_secret", csec)
	}

	client := s.buildHTTPClient(cred.ProxyURL, 20*time.Second)

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURI, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", "antigravity-zectrix-dashboard/1.0")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			// Retry once on cold proxy io.EOF / connection reset
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

func (s *Store) buildHTTPClient(credProxy string, timeout time.Duration) *http.Client {
	tr := &http.Transport{
		IdleConnTimeout: 30 * time.Second,
	}
	proxyRaw := strings.TrimSpace(credProxy)
	if proxyRaw == "" {
		proxyRaw = s.HTTPProxyURL
	}
	if proxyRaw != "" {
		if u, err := url.Parse(proxyRaw); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	} else {
		// 未显式配置时尊重标准 HTTPS_PROXY / HTTP_PROXY / NO_PROXY
		tr.Proxy = http.ProxyFromEnvironment
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

func (s *Store) deriveKey() ([]byte, error) {
	keyMaterial, err := os.ReadFile(s.KeyFile)
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
