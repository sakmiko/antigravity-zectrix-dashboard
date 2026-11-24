package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type PauseWindow struct {
	StartMin int
	EndMin   int
}

type Config struct {
	CPAAuthPath            string
	EncryptionKeyFile      string
	OAuthClientID          string
	OAuthClientSecret      string
	HTTPProxyURL           string
	QuotaBaseURL           string
	QuotaPollSeconds       int
	AuthRefreshSeconds     int
	PauseWindows           []PauseWindow
	Timezone               string
	Location               *time.Location
	StateFile              string
	HistoryMaxHours        int
	StaleAfterSeconds      int
	ZectrixBaseURL         string
	ZectrixDeviceID        string
	ZectrixAPIKey          string
	ZectrixPageID          string
	ZectrixDither          bool
	MinPushIntervalSeconds int
}

// LoadDotEnv loads KEY=VALUE pairs from path if it exists without overriding existing env vars.
func LoadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
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
		v = strings.Trim(v, `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
}

func Load() (*Config, error) {
	LoadDotEnv(".env")

	tz := envOr("TIMEZONE", "Asia/Shanghai")
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}

	pauseRaw := strings.TrimSpace(os.Getenv("QUOTA_PAUSE_WINDOWS"))
	var windows []PauseWindow
	if pauseRaw != "" {
		for _, part := range strings.Split(pauseRaw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			w, err := parsePauseWindow(part)
			if err != nil {
				return nil, fmt.Errorf("invalid QUOTA_PAUSE_WINDOWS entry %q: %w", part, err)
			}
			windows = append(windows, w)
		}
	}

	cfg := &Config{
		CPAAuthPath:            envOr("CPA_AUTH_PATH", "./fixtures/cpa-antigravity-example.json"),
		EncryptionKeyFile:      strings.TrimSpace(os.Getenv("CACHE_ENCRYPTION_KEY_FILE")),
		OAuthClientID:          strings.TrimSpace(os.Getenv("ANTIGRAVITY_OAUTH_CLIENT_ID")),
		OAuthClientSecret:      strings.TrimSpace(os.Getenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET")),
		HTTPProxyURL:           strings.TrimSpace(os.Getenv("HTTP_PROXY_URL")),
		QuotaBaseURL:           strings.TrimRight(envOr("ANTIGRAVITY_QUOTA_BASE_URL", "https://cloudcode-pa.googleapis.com"), "/"),
		QuotaPollSeconds:       envInt("QUOTA_POLL_SECONDS", 1800, 300),
		AuthRefreshSeconds:     envInt("ANTIGRAVITY_AUTH_REFRESH_SECONDS", 3600, 300),
		PauseWindows:           windows,
		Timezone:               tz,
		Location:               loc,
		StateFile:              envOr("STATE_FILE", "./state.json"),
		HistoryMaxHours:        envInt("HISTORY_MAX_HOURS", 12, 1),
		StaleAfterSeconds:      envInt("STALE_AFTER_SECONDS", 5400, 300),
		ZectrixBaseURL:         strings.TrimRight(envOr("ZECTRIX_BASE_URL", "https://cloud.zectrix.com"), "/"),
		ZectrixDeviceID:        strings.TrimSpace(os.Getenv("ZECTRIX_DEVICE_ID")),
		ZectrixAPIKey:          strings.TrimSpace(os.Getenv("ZECTRIX_API_KEY")),
		ZectrixPageID:          envOr("ZECTRIX_PAGE_ID", "antigravity-quota"),
		ZectrixDither:          strings.EqualFold(os.Getenv("ZECTRIX_DITHER"), "true"),
		MinPushIntervalSeconds: envInt("MIN_PUSH_INTERVAL_SECONDS", 60, 60),
	}
	return cfg, nil
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
			// Overnight window e.g. 23:00-07:00
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
	return fmt.Sprintf(
		"CPA_AUTH_PATH=%s\nCACHE_ENCRYPTION_KEY_FILE=%s\nQUOTA_BASE_URL=%s\nQUOTA_POLL_SECONDS=%d\nAUTH_REFRESH_SECONDS=%d\nTIMEZONE=%s\nSTATE_FILE=%s\nZECTRIX_BASE_URL=%s\nZECTRIX_DEVICE_ID=%s\nZECTRIX_API_KEY=%s\nZECTRIX_PAGE_ID=%s\nZECTRIX_DITHER=%v\nMIN_PUSH_INTERVAL_SECONDS=%d",
		c.CPAAuthPath, mask(c.EncryptionKeyFile), c.QuotaBaseURL,
		c.QuotaPollSeconds, c.AuthRefreshSeconds, c.Timezone, c.StateFile,
		c.ZectrixBaseURL, mask(c.ZectrixDeviceID), mask(c.ZectrixAPIKey),
		c.ZectrixPageID, c.ZectrixDither, c.MinPushIntervalSeconds,
	)
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

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def, minVal int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < minVal {
		return def
	}
	return n
}
