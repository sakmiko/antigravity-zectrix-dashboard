package config

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type PauseWindow struct {
	StartMin int
	EndMin   int
}

type KeepaliveConfig struct {
	Enabled             bool
	IntervalSeconds     int
	Mode                string // "token" | "session"
	RespectPauseWindows bool
}

type Config struct {
	ConfigFile             string
	EnvFile                string
	SourceFingerprint      string
	CPAAuthPath            string
	AccountEmail           string
	AuthCacheFile          string
	EncryptionKeyFile      string
	OAuthClientID          string
	OAuthClientSecret      string
	HTTPProxyURL           string
	QuotaBaseURL           string
	UserAgent              string
	QuotaPollSeconds       int
	AuthRefreshSeconds     int
	PauseWindows           []PauseWindow
	Timezone               string
	Location               *time.Location
	StateFile              string
	HistoryMaxHours        int
	StaleAfterSeconds      int
	Keepalive              KeepaliveConfig
	PlanTier               string // "auto" | "free" | "plus" | "pro" | "ultra"
	ZectrixBaseURL         string
	ZectrixDeviceID        string
	ZectrixAPIKey          string
	ZectrixPageID          string
	ZectrixDither          bool
	MinPushIntervalSeconds int
	HotReloadEnabled       bool
	HotReloadSeconds       int
}

type LoadOptions struct {
	ConfigFile string
	EnvFile    string
}

// ParseDotEnv reads KEY=VALUE pairs from path without mutating process env.
func ParseDotEnv(path string) (map[string]string, error) {
	out := make(map[string]string)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:idx])
		v := strings.TrimSpace(line[idx+1:])
		v = trimMatchingQuotes(v)
		out[k] = v
	}
	return out, sc.Err()
}

func trimMatchingQuotes(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// Load loads effective configuration from OS env > .env > config.toml > defaults.
func Load() (*Config, error) {
	return LoadWithOptions(LoadOptions{})
}

func LoadWithOptions(opts LoadOptions) (*Config, error) {
	envFile := strings.TrimSpace(opts.EnvFile)
	if envFile == "" {
		envFile = strings.TrimSpace(os.Getenv("ENV_FILE"))
	}
	if envFile == "" {
		envFile = ".env"
	}
	dotEnv, err := ParseDotEnv(envFile)
	if err != nil {
		return nil, fmt.Errorf("read env file %q: %w", envFile, err)
	}

	cfgFile := strings.TrimSpace(opts.ConfigFile)
	if cfgFile == "" {
		cfgFile = strings.TrimSpace(os.Getenv("CONFIG_FILE"))
	}
	if cfgFile == "" {
		cfgFile = strings.TrimSpace(dotEnv["CONFIG_FILE"])
	}
	if cfgFile == "" {
		if _, statErr := os.Stat("config.toml"); statErr == nil {
			cfgFile = "config.toml"
		}
	}

	tomlData := make(map[string]any)
	if cfgFile != "" {
		raw, err := os.ReadFile(cfgFile)
		if err != nil {
			return nil, fmt.Errorf("read config file %q: %w", cfgFile, err)
		}
		parsed, err := parseTOML(string(raw))
		if err != nil {
			return nil, fmt.Errorf("parse config file %q: %w", cfgFile, err)
		}
		tomlData = parsed
	}

	src := &sourceReader{dotEnv: dotEnv, toml: tomlData}

	tz := src.str("TIMEZONE", "quota.timezone", "Asia/Shanghai")
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}

	pauseList := src.strSlice("QUOTA_PAUSE_WINDOWS", "quota.pause_windows")
	var windows []PauseWindow
	for _, part := range pauseList {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		w, err := parsePauseWindow(part)
		if err != nil {
			return nil, fmt.Errorf("invalid pause window %q: %w", part, err)
		}
		windows = append(windows, w)
	}

	pollSec, err := src.durationSeconds([]string{"QUOTA_POLL_INTERVAL", "QUOTA_POLL_SECONDS"}, "quota.poll_interval", 1800, 60)
	if err != nil {
		return nil, fmt.Errorf("quota poll interval: %w", err)
	}
	authRefreshSec, err := src.durationSeconds([]string{"AUTH_REFRESH_INTERVAL", "ANTIGRAVITY_AUTH_REFRESH_SECONDS"}, "quota.auth_refresh_interval", 3000, 60)
	if err != nil {
		return nil, fmt.Errorf("auth refresh interval: %w", err)
	}
	historyMaxH, err := src.intVal("HISTORY_MAX_HOURS", "quota.history_max_hours", 12, 1)
	if err != nil {
		return nil, fmt.Errorf("history_max_hours: %w", err)
	}
	staleSec, err := src.durationSeconds([]string{"STALE_AFTER", "STALE_AFTER_SECONDS"}, "quota.stale_after", 5400, 60)
	if err != nil {
		return nil, fmt.Errorf("stale_after: %w", err)
	}
	minPushSec, err := src.durationSeconds([]string{"MIN_PUSH_INTERVAL", "MIN_PUSH_INTERVAL_SECONDS"}, "zectrix.min_push_interval", 60, 10)
	if err != nil {
		return nil, fmt.Errorf("min_push_interval: %w", err)
	}
	if minPushSec < 60 {
		minPushSec = 60
	}

	kaEnabled, err := src.boolVal("KEEPALIVE_ENABLED", "keepalive.enabled", false)
	if err != nil {
		return nil, fmt.Errorf("keepalive.enabled: %w", err)
	}
	kaIntervalSec, err := src.durationSeconds([]string{"KEEPALIVE_INTERVAL", "KEEPALIVE_INTERVAL_SECONDS"}, "keepalive.interval", 1800, 60)
	if err != nil {
		return nil, fmt.Errorf("keepalive.interval: %w", err)
	}
	kaMode := strings.ToLower(src.str("KEEPALIVE_MODE", "keepalive.mode", "session"))
	if kaMode != "token" && kaMode != "session" {
		return nil, fmt.Errorf("invalid keepalive.mode %q (expected \"token\" or \"session\")", kaMode)
	}
	kaRespectPause, err := src.boolVal("KEEPALIVE_RESPECT_PAUSE_WINDOWS", "keepalive.respect_pause_windows", false)
	if err != nil {
		return nil, fmt.Errorf("keepalive.respect_pause_windows: %w", err)
	}

	hotEnabled, err := src.boolVal("HOT_RELOAD_ENABLED", "hot_reload.enabled", true)
	if err != nil {
		return nil, fmt.Errorf("hot_reload.enabled: %w", err)
	}
	hotIntervalSec, err := src.durationSeconds([]string{"HOT_RELOAD_INTERVAL", "HOT_RELOAD_SECONDS"}, "hot_reload.poll_interval", 5, 1)
	if err != nil {
		return nil, fmt.Errorf("hot_reload.poll_interval: %w", err)
	}

	zDither, err := src.boolVal("ZECTRIX_DITHER", "zectrix.dither", false)
	if err != nil {
		return nil, fmt.Errorf("zectrix.dither: %w", err)
	}

	planTier := strings.ToLower(src.str("ANTIGRAVITY_PLAN_TIER", "display.plan_tier", "auto"))
	switch planTier {
	case "auto", "free", "plus", "pro", "ultra":
	default:
		return nil, fmt.Errorf("invalid plan_tier %q (expected auto, free, plus, pro, or ultra)", planTier)
	}

	stateFile := src.str("STATE_FILE", "quota.state_file", "./state.json")
	authCacheFile := src.str("AUTH_CACHE_FILE", "auth.auth_cache_file", "")
	if authCacheFile == "" {
		authCacheFile = filepath.Join(filepath.Dir(stateFile), "auth-cache.json")
	}

	fp := ComputeFilesFingerprint(cfgFile, envFile)

	return &Config{
		ConfigFile:         cfgFile,
		EnvFile:            envFile,
		SourceFingerprint:  fp,
		CPAAuthPath:        src.str("CPA_AUTH_PATH", "auth.cpa_auth_path", "./fixtures/cpa-antigravity-example.json"),
		AccountEmail:       src.str("CPA_ACCOUNT_EMAIL", "auth.account_email", ""),
		AuthCacheFile:      authCacheFile,
		EncryptionKeyFile:  src.str("CACHE_ENCRYPTION_KEY_FILE", "auth.encryption_key_file", ""),
		OAuthClientID:      src.str("ANTIGRAVITY_OAUTH_CLIENT_ID", "auth.oauth_client_id", ""),
		OAuthClientSecret:  src.str("ANTIGRAVITY_OAUTH_CLIENT_SECRET", "auth.oauth_client_secret", ""),
		HTTPProxyURL:       src.str("HTTP_PROXY_URL", "network.http_proxy_url", ""),
		QuotaBaseURL:       strings.TrimRight(src.str("ANTIGRAVITY_QUOTA_BASE_URL", "quota.base_url", "https://cloudcode-pa.googleapis.com"), "/"),
		UserAgent:          src.str("ANTIGRAVITY_USER_AGENT", "quota.user_agent", ""),
		QuotaPollSeconds:   pollSec,
		AuthRefreshSeconds: authRefreshSec,
		PauseWindows:       windows,
		Timezone:           tz,
		Location:           loc,
		StateFile:          stateFile,
		HistoryMaxHours:    historyMaxH,
		StaleAfterSeconds:  staleSec,
		Keepalive: KeepaliveConfig{
			Enabled:             kaEnabled,
			IntervalSeconds:     kaIntervalSec,
			Mode:                kaMode,
			RespectPauseWindows: kaRespectPause,
		},
		PlanTier:               planTier,
		ZectrixBaseURL:         strings.TrimRight(src.str("ZECTRIX_BASE_URL", "zectrix.base_url", "https://cloud.zectrix.com"), "/"),
		ZectrixDeviceID:        src.str("ZECTRIX_DEVICE_ID", "zectrix.device_id", ""),
		ZectrixAPIKey:          src.str("ZECTRIX_API_KEY", "zectrix.api_key", ""),
		ZectrixPageID:          src.str("ZECTRIX_PAGE_ID", "zectrix.page_id", "1"),
		ZectrixDither:          zDither,
		MinPushIntervalSeconds: minPushSec,
		HotReloadEnabled:       hotEnabled,
		HotReloadSeconds:       hotIntervalSec,
	}, nil
}

// ComputeFilesFingerprint returns a combined SHA-256 fingerprint of config files
// for efficient hot-reload change detection.
func ComputeFilesFingerprint(paths ...string) string {
	h := sha256.New()
	for _, p := range paths {
		if p == "" {
			continue
		}
		h.Write([]byte(p))
		h.Write([]byte{0})
		if b, err := os.ReadFile(p); err == nil {
			h.Write([]byte{1})
			h.Write(b)
		} else {
			h.Write([]byte{2})
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (c *Config) InPauseWindow(now time.Time) bool {
	local := now.In(c.Location)
	min := local.Hour()*60 + local.Minute()
	for _, w := range c.PauseWindows {
		if w.StartMin <= w.EndMin {
			if min >= w.StartMin && min < w.EndMin {
				return true
			}
		} else {
			if min >= w.StartMin || min < w.EndMin {
				return true
			}
		}
	}
	return false
}

func (c *Config) RedactedSummary() string {
	mask := func(s string) string {
		if s == "" {
			return "(unset)"
		}
		if len(s) <= 6 {
			return "***"
		}
		return s[:3] + "..." + s[len(s)-3:]
	}
	cfgPath := c.ConfigFile
	if cfgPath == "" {
		cfgPath = "(none)"
	}
	return fmt.Sprintf(
		"CONFIG_FILE=%s\nENV_FILE=%s\nCPA_AUTH_PATH=%s\nCPA_ACCOUNT_EMAIL=%s\nAUTH_CACHE_FILE=%s\nCACHE_ENCRYPTION_KEY_FILE=%s\nQUOTA_BASE_URL=%s\nQUOTA_POLL_SECONDS=%d\nAUTH_REFRESH_SECONDS=%d\nKEEPALIVE_ENABLED=%v\nKEEPALIVE_INTERVAL_SECONDS=%d\nKEEPALIVE_MODE=%s\nKEEPALIVE_RESPECT_PAUSE_WINDOWS=%v\nPLAN_TIER=%s\nTIMEZONE=%s\nSTATE_FILE=%s\nZECTRIX_BASE_URL=%s\nZECTRIX_DEVICE_ID=%s\nZECTRIX_API_KEY=%s\nZECTRIX_PAGE_ID=%s\nZECTRIX_DITHER=%v\nMIN_PUSH_INTERVAL_SECONDS=%d\nHOT_RELOAD_ENABLED=%v\nHOT_RELOAD_SECONDS=%d",
		cfgPath, c.EnvFile, c.CPAAuthPath, c.AccountEmail, c.AuthCacheFile,
		mask(c.EncryptionKeyFile), c.QuotaBaseURL,
		c.QuotaPollSeconds, c.AuthRefreshSeconds,
		c.Keepalive.Enabled, c.Keepalive.IntervalSeconds, c.Keepalive.Mode, c.Keepalive.RespectPauseWindows,
		c.PlanTier, c.Timezone, c.StateFile,
		c.ZectrixBaseURL, mask(c.ZectrixDeviceID), mask(c.ZectrixAPIKey),
		c.ZectrixPageID, c.ZectrixDither, c.MinPushIntervalSeconds,
		c.HotReloadEnabled, c.HotReloadSeconds,
	)
}

type sourceReader struct {
	dotEnv map[string]string
	toml   map[string]any
}

func (r *sourceReader) lookup(envKeys []string, tomlKey string) (any, bool) {
	for _, k := range envKeys {
		if v, ok := os.LookupEnv(k); ok {
			if strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v), true
			}
		}
	}
	for _, k := range envKeys {
		if v, ok := r.dotEnv[k]; ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), true
		}
	}
	if tomlKey != "" {
		if v, ok := r.toml[tomlKey]; ok {
			return v, true
		}
	}
	return nil, false
}

func (r *sourceReader) str(envKey, tomlKey, def string) string {
	v, ok := r.lookup([]string{envKey}, tomlKey)
	if !ok {
		return def
	}
	s := strings.TrimSpace(fmt.Sprint(v))
	if s == "" {
		return def
	}
	return s
}

func (r *sourceReader) strSlice(envKey, tomlKey string) []string {
	v, ok := r.lookup([]string{envKey}, tomlKey)
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case []string:
		return t
	default:
		s := strings.TrimSpace(fmt.Sprint(v))
		if s == "" {
			return nil
		}
		return strings.Split(s, ",")
	}
}

func (r *sourceReader) boolVal(envKey, tomlKey string, def bool) (bool, error) {
	v, ok := r.lookup([]string{envKey}, tomlKey)
	if !ok {
		return def, nil
	}
	switch t := v.(type) {
	case bool:
		return t, nil
	default:
		s := strings.ToLower(strings.TrimSpace(fmt.Sprint(v)))
		switch s {
		case "true", "1", "yes", "on":
			return true, nil
		case "false", "0", "no", "off":
			return false, nil
		default:
			return false, fmt.Errorf("invalid boolean %q", s)
		}
	}
}

func (r *sourceReader) intVal(envKey, tomlKey string, def, minVal int) (int, error) {
	v, ok := r.lookup([]string{envKey}, tomlKey)
	if !ok {
		return def, nil
	}
	var n int
	switch t := v.(type) {
	case int64:
		n = int(t)
	case int:
		n = t
	default:
		parsed, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(v)))
		if err != nil {
			return 0, fmt.Errorf("invalid integer %q", v)
		}
		n = parsed
	}
	if n < minVal {
		return 0, fmt.Errorf("value %d is below minimum %d", n, minVal)
	}
	return n, nil
}

func (r *sourceReader) durationSeconds(envKeys []string, tomlKey string, defSec, minSec int) (int, error) {
	v, ok := r.lookup(envKeys, tomlKey)
	if !ok {
		return defSec, nil
	}
	var sec int
	switch t := v.(type) {
	case int64:
		sec = int(t)
	case int:
		sec = t
	default:
		s := strings.TrimSpace(fmt.Sprint(v))
		if n, err := strconv.Atoi(s); err == nil {
			sec = n
		} else {
			d, err := time.ParseDuration(s)
			if err != nil {
				return 0, fmt.Errorf("invalid duration %q (use seconds like 1800 or duration like 30m)", s)
			}
			sec = int(d.Seconds())
		}
	}
	if sec < minSec {
		return 0, fmt.Errorf("duration %ds is below minimum %ds", sec, minSec)
	}
	return sec, nil
}

func parsePauseWindow(s string) (PauseWindow, error) {
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return PauseWindow{}, fmt.Errorf("expected HH:MM-HH:MM")
	}
	s1, err := parseHHMM(strings.TrimSpace(parts[0]))
	if err != nil {
		return PauseWindow{}, err
	}
	s2, err := parseHHMM(strings.TrimSpace(parts[1]))
	if err != nil {
		return PauseWindow{}, err
	}
	return PauseWindow{StartMin: s1, EndMin: s2}, nil
}

func parseHHMM(s string) (int, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid time %q", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("invalid hour %q", s)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("invalid minute %q", s)
	}
	return h*60 + m, nil
}
