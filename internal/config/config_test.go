package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadTOMLAndHotReloadFingerprint(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	envPath := filepath.Join(dir, ".env")

	tomlContent := `
[auth]
cpa_auth_path = "/tmp/cpa/antigravity-user.json"
account_email = "user@example.com"

[keepalive]
enabled = true
interval = "15m"
mode = "token"
respect_pause_windows = true

[display]
plan_tier = "pro"

[quota]
pause_windows = ["01:00-06:30"]
poll_interval = "20m"

[zectrix]
page_id = "2"
`
	if err := os.WriteFile(tomlPath, []byte(tomlContent), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte("ZECTRIX_DEVICE_ID=dev123\n"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadWithOptions(LoadOptions{ConfigFile: tomlPath, EnvFile: envPath})
	if err != nil {
		t.Fatalf("LoadWithOptions: %v", err)
	}
	if !cfg.Keepalive.Enabled || cfg.Keepalive.IntervalSeconds != 900 || cfg.Keepalive.Mode != "token" || !cfg.Keepalive.RespectPauseWindows {
		t.Fatalf("unexpected keepalive config: %+v", cfg.Keepalive)
	}
	if cfg.QuotaPollSeconds != 1200 {
		t.Fatalf("expected QuotaPollSeconds=1200, got %d", cfg.QuotaPollSeconds)
	}
	if cfg.PlanTier != "pro" || cfg.ZectrixDeviceID != "dev123" || cfg.ZectrixPageID != "2" {
		t.Fatalf("unexpected fields: tier=%s dev=%s page=%s", cfg.PlanTier, cfg.ZectrixDeviceID, cfg.ZectrixPageID)
	}
	if cfg.ZectrixBaseURL != "https://cloud.zectrix.com" {
		t.Fatalf("expected cloud.zectrix.com default, got %s", cfg.ZectrixBaseURL)
	}

	// Update TOML and verify fingerprint changes
	fp1 := cfg.SourceFingerprint
	if err := os.WriteFile(tomlPath, []byte(tomlContent+"\n# modified\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg2, err := LoadWithOptions(LoadOptions{ConfigFile: tomlPath, EnvFile: envPath})
	if err != nil {
		t.Fatalf("LoadWithOptions reload: %v", err)
	}
	if cfg2.SourceFingerprint == fp1 {
		t.Fatalf("expected fingerprint change after TOML modification")
	}
}

func TestKeepaliveDisabledByDefaultAndMinValidation(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(tomlPath, []byte("[keepalive]\ninterval = \"10s\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWithOptions(LoadOptions{ConfigFile: tomlPath, EnvFile: filepath.Join(dir, "missing.env")}); err == nil {
		t.Fatalf("expected error for keepalive interval below 60s")
	}

	if err := os.WriteFile(tomlPath, []byte("[quota]\npoll_interval = \"10m\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithOptions(LoadOptions{ConfigFile: tomlPath, EnvFile: filepath.Join(dir, "missing.env")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Keepalive.Enabled {
		t.Fatalf("keepalive must be disabled by default")
	}
}
