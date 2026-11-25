package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sakmiko/antigravity-zectrix-dashboard/internal/antigravity"
	"github.com/sakmiko/antigravity-zectrix-dashboard/internal/auth"
	"github.com/sakmiko/antigravity-zectrix-dashboard/internal/config"
	"github.com/sakmiko/antigravity-zectrix-dashboard/internal/predictor"
	"github.com/sakmiko/antigravity-zectrix-dashboard/internal/publisher"
	"github.com/sakmiko/antigravity-zectrix-dashboard/internal/render"
)

const appVersion = "1.1.0"

func main() {
	configPath := flag.String("config", "", "Path to TOML config file (optional, defaults to ./config.toml if present)")
	envPath := flag.String("env-file", "", "Path to .env file (optional, defaults to ./.env)")
	showVersion := flag.Bool("version", false, "Print version and exit")
	showEnv := flag.Bool("env", false, "Print redacted effective configuration and exit")
	previewOut := flag.String("preview", "", "Render offline 400x300 16-gray PNG preview to path and exit")
	previewTier := flag.String("preview-tier", "", "Override tier badge for --preview (free|plus|pro|ultra)")
	doCheck := flag.Bool("check", false, "Verify CPA JSON, fetch quota once, print prediction summary")
	doTrigger := flag.Bool("trigger-refresh", false, "Poll quota, render frame, and push to ZECTRIX once")
	doRefreshAuth := flag.Bool("refresh-auth", false, "Force OAuth token refresh and save to separate auth cache")
	doKeepaliveOnce := flag.Bool("keepalive-once", false, "Run one keepalive cycle (OAuth + optional loadCodeAssist) and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("antigravity-zectrix %s\n", appVersion)
		return
	}

	loadOpts := config.LoadOptions{ConfigFile: *configPath, EnvFile: *envPath}
	cfg, err := config.LoadWithOptions(loadOpts)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	if *showEnv {
		fmt.Println(cfg.RedactedSummary())
		return
	}

	if *previewOut != "" {
		if err := runPreview(cfg, *previewOut, *previewTier); err != nil {
			log.Fatalf("preview failed: %v", err)
		}
		return
	}

	store := auth.NewStore(
		cfg.CPAAuthPath, cfg.AccountEmail, cfg.AuthCacheFile,
		cfg.EncryptionKeyFile, cfg.OAuthClientID, cfg.OAuthClientSecret, cfg.HTTPProxyURL,
	)
	agClient := antigravity.NewClient(cfg.QuotaBaseURL, cfg.HTTPProxyURL, cfg.UserAgent)
	pub := publisher.NewZectrixClient(
		cfg.ZectrixBaseURL, cfg.ZectrixDeviceID, cfg.ZectrixAPIKey,
		cfg.ZectrixPageID, cfg.ZectrixDither, cfg.MinPushIntervalSeconds,
	)
	if st, err := predictor.LoadState(cfg.StateFile); err == nil && st.LastPushHash != "" {
		pub.SeedState(st.LastPushHash, st.LastPushedAt)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if *doRefreshAuth {
		cred, _, err := store.Load()
		if err != nil {
			log.Fatalf("load CPA auth: %v", err)
		}
		updated, err := store.RefreshToken(ctx, cred)
		if err != nil {
			log.Fatalf("refresh auth: %v", err)
		}
		if err := store.SaveCacheAtomic(updated); err != nil {
			log.Fatalf("save auth cache: %v", err)
		}
		fmt.Printf("OAuth token refreshed and saved to separate auth cache (%s); source CPA JSON untouched.\n", cfg.AuthCacheFile)
		return
	}

	if *doKeepaliveOnce {
		if err := runKeepaliveCycle(ctx, cfg, store, agClient, true); err != nil {
			log.Fatalf("keepalive failed: %v", err)
		}
		fmt.Printf("Keepalive completed (mode=%s); source CPA JSON untouched.\n", cfg.Keepalive.Mode)
		return
	}

	if *doCheck {
		fc, err := pollAndPredict(ctx, cfg, store, agClient)
		if err != nil {
			log.Fatalf("check failed: %v", err)
		}
		tierLabel := fc.PlanTier
		if tierLabel == "" {
			tierLabel = "UNKNOWN"
		}
		fmt.Printf("Updated=%s Stale=%v Tier=%s | GEMINI=%.1f%% (-%.1f%%/h, %s) | CLAUDE=%.1f%% (-%.1f%%/h, %s) | WEEKLY=%.1f%% (avail=%v)\n",
			fc.UpdatedAt.In(cfg.Location).Format(time.RFC3339), fc.Stale, tierLabel,
			fc.Gemini.RemainingPct, fc.Gemini.BurnRatePctH, predictor.FormatRunwayShort(fc.Gemini.RunwayHours),
			fc.Claude.RemainingPct, fc.Claude.BurnRatePctH, predictor.FormatRunwayShort(fc.Claude.RunwayHours),
			fc.Weekly.RemainingPct, fc.Weekly.Available,
		)
		return
	}

	if *doTrigger {
		if err := runOneCycle(ctx, cfg, store, agClient, pub, true); err != nil {
			log.Fatalf("trigger-refresh failed: %v", err)
		}
		return
	}

	// Headless daemon mode with hot-reload & optional keepalive
	log.Printf("antigravity-zectrix %s starting (poll=%ds, auth_refresh=%ds, keepalive=%v/%ds/%s, hot_reload=%v/%ds, tz=%s)",
		appVersion, cfg.QuotaPollSeconds, cfg.AuthRefreshSeconds,
		cfg.Keepalive.Enabled, cfg.Keepalive.IntervalSeconds, cfg.Keepalive.Mode,
		cfg.HotReloadEnabled, cfg.HotReloadSeconds, cfg.Timezone)

	if !cfg.InPauseWindow(time.Now()) {
		if err := runOneCycle(ctx, cfg, store, agClient, pub, false); err != nil {
			log.Printf("initial cycle warning: %v", err)
		}
	}

	pollTicker := time.NewTicker(time.Duration(cfg.QuotaPollSeconds) * time.Second)
	defer pollTicker.Stop()
	authTicker := time.NewTicker(time.Duration(cfg.AuthRefreshSeconds) * time.Second)
	defer authTicker.Stop()

	kaTicker, kaCh := newOptionalTicker(cfg.Keepalive.Enabled, cfg.Keepalive.IntervalSeconds)
	defer stopTicker(kaTicker)
	hrTicker, hrCh := newOptionalTicker(cfg.HotReloadEnabled, cfg.HotReloadSeconds)
	defer stopTicker(hrTicker)

	sighupCh := make(chan os.Signal, 1)
	signal.Notify(sighupCh, syscall.SIGHUP)
	defer signal.Stop(sighupCh)

	applyHotReload := func(reason string) {
		newCfg, err := config.LoadWithOptions(loadOpts)
		if err != nil {
			log.Printf("hot-reload (%s) rejected, keeping active config: %v", reason, err)
			return
		}
		if reason == "watch" && newCfg.SourceFingerprint == cfg.SourceFingerprint {
			return
		}

		store.UpdateConfig(
			newCfg.CPAAuthPath, newCfg.AccountEmail, newCfg.AuthCacheFile,
			newCfg.EncryptionKeyFile, newCfg.OAuthClientID, newCfg.OAuthClientSecret, newCfg.HTTPProxyURL,
		)
		agClient.UpdateConfig(newCfg.QuotaBaseURL, newCfg.HTTPProxyURL, newCfg.UserAgent)
		pub.UpdateConfig(
			newCfg.ZectrixBaseURL, newCfg.ZectrixDeviceID, newCfg.ZectrixAPIKey,
			newCfg.ZectrixPageID, newCfg.ZectrixDither, newCfg.MinPushIntervalSeconds,
		)

		if newCfg.QuotaPollSeconds != cfg.QuotaPollSeconds {
			pollTicker.Reset(time.Duration(newCfg.QuotaPollSeconds) * time.Second)
		}
		if newCfg.AuthRefreshSeconds != cfg.AuthRefreshSeconds {
			authTicker.Reset(time.Duration(newCfg.AuthRefreshSeconds) * time.Second)
		}
		if newCfg.Keepalive != cfg.Keepalive {
			stopTicker(kaTicker)
			kaTicker, kaCh = newOptionalTicker(newCfg.Keepalive.Enabled, newCfg.Keepalive.IntervalSeconds)
		}
		if newCfg.HotReloadEnabled != cfg.HotReloadEnabled || newCfg.HotReloadSeconds != cfg.HotReloadSeconds {
			stopTicker(hrTicker)
			hrTicker, hrCh = newOptionalTicker(newCfg.HotReloadEnabled, newCfg.HotReloadSeconds)
		}

		cfg = newCfg
		log.Printf("hot-reload (%s) applied: poll=%ds keepalive=%v/%ds/%s tier=%s",
			reason, cfg.QuotaPollSeconds, cfg.Keepalive.Enabled, cfg.Keepalive.IntervalSeconds, cfg.Keepalive.Mode, cfg.PlanTier)
	}

	for {
		select {
		case <-ctx.Done():
			log.Println("shutting down cleanly")
			return
		case <-sighupCh:
			applyHotReload("sighup")
		case <-hrCh:
			applyHotReload("watch")
		case <-authTicker.C:
			if err := refreshTokenIfNeeded(ctx, store, false); err != nil {
				log.Printf("auth ticker error: %v", err)
			}
		case now := <-kaCh:
			if cfg.Keepalive.RespectPauseWindows && cfg.InPauseWindow(now) {
				log.Println("skipping keepalive: inside QUOTA_PAUSE_WINDOWS")
				continue
			}
			if err := runKeepaliveCycle(ctx, cfg, store, agClient, false); err != nil {
				log.Printf("keepalive cycle error: %v", err)
			}
		case now := <-pollTicker.C:
			if cfg.InPauseWindow(now) {
				log.Println("skipping poll: inside QUOTA_PAUSE_WINDOWS")
				continue
			}
			if err := runOneCycle(ctx, cfg, store, agClient, pub, false); err != nil {
				log.Printf("poll cycle error: %v", err)
			}
		}
	}
}

func newOptionalTicker(enabled bool, seconds int) (*time.Ticker, <-chan time.Time) {
	if !enabled || seconds <= 0 {
		return nil, nil
	}
	t := time.NewTicker(time.Duration(seconds) * time.Second)
	return t, t.C
}

func stopTicker(t *time.Ticker) {
	if t != nil {
		t.Stop()
	}
}

func refreshTokenIfNeeded(ctx context.Context, store *auth.Store, force bool) error {
	cred, _, err := store.Load()
	if err != nil {
		return err
	}
	if !force && !auth.IsTokenExpiringSoon(cred, 10*time.Minute) {
		return nil
	}
	updated, err := store.RefreshToken(ctx, cred)
	if err != nil {
		return err
	}
	return store.SaveCacheAtomic(updated)
}

func runKeepaliveCycle(ctx context.Context, cfg *config.Config, store *auth.Store, agClient *antigravity.Client, forceRefresh bool) error {
	cred, _, err := store.Load()
	if err != nil {
		return err
	}
	if forceRefresh || cfg.Keepalive.Mode == "token" || auth.IsTokenExpiringSoon(cred, 15*time.Minute) {
		updated, err := store.RefreshToken(ctx, cred)
		if err != nil {
			return err
		}
		cred = updated
		if err := store.SaveCacheAtomic(cred); err != nil {
			return err
		}
	}
	if cfg.Keepalive.Mode == "session" {
		info, err := agClient.LoadCodeAssist(ctx, cred.AccessToken, cred.ProjectID)
		if errors.Is(err, antigravity.ErrUnauthorized) {
			updated, rerr := store.RefreshToken(ctx, cred)
			if rerr != nil {
				return rerr
			}
			cred = updated
			_ = store.SaveCacheAtomic(cred)
			info, err = agClient.LoadCodeAssist(ctx, cred.AccessToken, cred.ProjectID)
		}
		if err != nil {
			return err
		}
		if info != nil && info.ProjectID != "" && info.ProjectID != cred.ProjectID {
			cred.ProjectID = info.ProjectID
			_ = store.SaveCacheAtomic(cred)
		}
		log.Printf("keepalive session ok (tier=%s project=%s)", info.PlanTier, info.ProjectID)
		return nil
	}
	log.Printf("keepalive token ok (expires=%s)", cred.Expired)
	return nil
}

func pollAndPredict(ctx context.Context, cfg *config.Config, store *auth.Store, agClient *antigravity.Client) (*predictor.Forecast, error) {
	st, err := predictor.LoadState(cfg.StateFile)
	if err != nil {
		return nil, err
	}
	cred, _, err := store.Load()
	if err != nil {
		return nil, err
	}

	if auth.IsTokenExpiringSoon(cred, 3*time.Minute) && cred.RefreshToken != "" {
		if updated, rerr := store.RefreshToken(ctx, cred); rerr == nil {
			cred = updated
			_ = store.SaveCacheAtomic(cred)
		}
	}

	snap, err := agClient.FetchQuota(ctx, cred.AccessToken, cred.ProjectID)
	if errors.Is(err, antigravity.ErrUnauthorized) {
		log.Println("quota API returned 401; refreshing OAuth token and retrying once")
		updated, rerr := store.RefreshToken(ctx, cred)
		if rerr != nil {
			return st.Compute(time.Now(), time.Duration(cfg.StaleAfterSeconds)*time.Second, cfg.PlanTier, cfg.Keepalive.Enabled), rerr
		}
		cred = updated
		_ = store.SaveCacheAtomic(cred)
		snap, err = agClient.FetchQuota(ctx, cred.AccessToken, cred.ProjectID)
	}
	if err != nil {
		if len(st.Snapshots) == 0 {
			return nil, err
		}
		fc := st.Compute(time.Now(), time.Duration(cfg.StaleAfterSeconds)*time.Second, cfg.PlanTier, cfg.Keepalive.Enabled)
		fc.Stale = true
		return fc, err
	}

	if snap.ProjectID != "" && snap.ProjectID != cred.ProjectID {
		cred.ProjectID = snap.ProjectID
		_ = store.SaveCacheAtomic(cred)
	}

	st.AppendAndPrune(*snap, cfg.HistoryMaxHours)
	_ = st.SaveAtomic(cfg.StateFile)
	return st.Compute(time.Now(), time.Duration(cfg.StaleAfterSeconds)*time.Second, cfg.PlanTier, cfg.Keepalive.Enabled), nil
}

func runOneCycle(ctx context.Context, cfg *config.Config, store *auth.Store, agClient *antigravity.Client, pub *publisher.ZectrixClient, force bool) error {
	fc, pollErr := pollAndPredict(ctx, cfg, store, agClient)
	if fc == nil {
		return pollErr
	}
	frame, err := render.RenderDashboard(fc, cfg.Location)
	if err != nil {
		return err
	}
	dec, err := pub.Publish(ctx, frame.PNG, frame.SHA256Hex, force)
	if err != nil {
		return err
	}
	if dec == publisher.DecisionPublish {
		if st, serr := predictor.LoadState(cfg.StateFile); serr == nil {
			h, ts := pub.LastState()
			st.LastPushHash = h
			st.LastPushedAt = ts
			_ = st.SaveAtomic(cfg.StateFile)
		}
	}
	log.Printf("frame sha256=%s bytes=%d tier=%s decision=%s", frame.SHA256Hex[:12], len(frame.PNG), fc.PlanTier, dec)
	return pollErr
}

func runPreview(cfg *config.Config, outPath, tierOverride string) error {
	now := time.Now().In(cfg.Location)
	tier := cfg.PlanTier
	if tierOverride != "" {
		tier = tierOverride
	}
	st, _ := predictor.LoadState(cfg.StateFile)
	var fc *predictor.Forecast
	if len(st.Snapshots) > 0 {
		fc = st.Compute(now, time.Duration(cfg.StaleAfterSeconds)*time.Second, tier, cfg.Keepalive.Enabled)
	} else {
		fc = sampleForecast(now, tier, cfg.Keepalive.Enabled)
	}
	frame, err := render.RenderDashboard(fc, cfg.Location)
	if err != nil {
		return err
	}
	if err := os.WriteFile(outPath, frame.PNG, 0644); err != nil {
		return err
	}
	fmt.Printf("Rendered %dx%d 16-gray PNG (tier=%s) to %s (%d bytes, sha256=%s)\n",
		frame.Width, frame.Height, fc.PlanTier, outPath, len(frame.PNG), frame.SHA256Hex[:12])
	return nil
}

func sampleForecast(now time.Time, tierOverride string, keepalive bool) *predictor.Forecast {
	tier := "PRO"
	switch tierOverride {
	case "free", "FREE":
		tier = "FREE"
	case "plus", "PLUS":
		tier = "PLUS"
	case "pro", "PRO":
		tier = "PRO"
	case "ultra", "ULTRA":
		tier = "ULTRA"
	}

	gemHist := []predictor.Point{
		{Time: now.Add(-6 * time.Hour), Pct: 93},
		{Time: now.Add(-4 * time.Hour), Pct: 87},
		{Time: now.Add(-2 * time.Hour), Pct: 80},
		{Time: now, Pct: 74},
	}
	claHist := []predictor.Point{
		{Time: now.Add(-6 * time.Hour), Pct: 84},
		{Time: now.Add(-4 * time.Hour), Pct: 69},
		{Time: now.Add(-2 * time.Hour), Pct: 52},
		{Time: now, Pct: 38},
	}
	cla := predictor.PoolPrediction{
		Name: "CLAUDE", RemainingPct: 38, BurnRatePctH: 8.5,
		RunwayHours: 4.5, ExhaustAt: now.Add(270 * time.Minute),
		ResetAt: now.Add(270 * time.Minute), Available: true, History: claHist,
	}
	gem := predictor.PoolPrediction{
		Name: "GEMINI", RemainingPct: 74, BurnRatePctH: 3.2,
		RunwayHours: 23.0, ExhaustAt: now.Add(23 * time.Hour),
		ResetAt: now.Add(13*time.Hour + 30*time.Minute), Available: true, History: gemHist,
	}
	wk := predictor.PoolPrediction{
		Name: "WEEKLY", RemainingPct: 62, ResetAt: now.Add(86 * time.Hour),
		Available: true, RunwayHours: -1,
	}
	return &predictor.Forecast{
		UpdatedAt: now, Stale: false, PlanTier: tier, KeepaliveEnabled: keepalive,
		Gemini: gem, Claude: cla, Weekly: wk, Bottleneck: cla,
	}
}
