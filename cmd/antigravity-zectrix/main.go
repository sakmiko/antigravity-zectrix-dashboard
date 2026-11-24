package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sakmiko/antigravity-zectrix-dashboard/internal/antigravity"
	"github.com/sakmiko/antigravity-zectrix-dashboard/internal/auth"
	"github.com/sakmiko/antigravity-zectrix-dashboard/internal/config"
	"github.com/sakmiko/antigravity-zectrix-dashboard/internal/predictor"
	"github.com/sakmiko/antigravity-zectrix-dashboard/internal/publisher"
	"github.com/sakmiko/antigravity-zectrix-dashboard/internal/render"
)

const appVersion = "1.0.0"

func main() {
	showVersion := flag.Bool("version", false, "Print version and exit")
	showEnv := flag.Bool("env", false, "Print redacted effective configuration and exit")
	previewOut := flag.String("preview", "", "Render offline 400x300 16-gray PNG preview to path and exit")
	doCheck := flag.Bool("check", false, "Verify CPA JSON, fetch quota once, print prediction summary")
	doTrigger := flag.Bool("trigger-refresh", false, "Poll quota, render frame, and push to ZECTRIX once")
	doRefreshAuth := flag.Bool("refresh-auth", false, "Force OAuth token refresh and atomic encrypted save")
	flag.Parse()

	if *showVersion {
		fmt.Printf("antigravity-zectrix %s\n", appVersion)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	if *showEnv {
		fmt.Println(cfg.RedactedSummary())
		return
	}

	if *previewOut != "" {
		if err := runPreview(cfg, *previewOut); err != nil {
			log.Fatalf("preview failed: %v", err)
		}
		return
	}

	store := auth.NewStore(cfg.CPAAuthPath, cfg.EncryptionKeyFile, cfg.OAuthClientID, cfg.OAuthClientSecret, cfg.HTTPProxyURL)
	agClient := antigravity.NewClient(cfg.QuotaBaseURL, cfg.HTTPProxyURL)
	pub := publisher.NewZectrixClient(cfg.ZectrixBaseURL, cfg.ZectrixDeviceID, cfg.ZectrixAPIKey, cfg.ZectrixPageID, cfg.ZectrixDither, cfg.MinPushIntervalSeconds)

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
		if err := store.SaveAtomic(updated); err != nil {
			log.Fatalf("save auth: %v", err)
		}
		fmt.Println("OAuth token refreshed and saved atomically.")
		return
	}

	if *doCheck {
		fc, err := pollAndPredict(ctx, cfg, store, agClient)
		if err != nil {
			log.Fatalf("check failed: %v", err)
		}
		fmt.Printf("Updated=%s Stale=%v | GEMINI=%.1f%% (-%.1f%%/h, %s) | CLAUDE=%.1f%% (-%.1f%%/h, %s) | WEEKLY=%.1f%%\n",
			fc.UpdatedAt.In(cfg.Location).Format(time.RFC3339), fc.Stale,
			fc.Gemini.RemainingPct, fc.Gemini.BurnRatePctH, predictor.FormatRunwayShort(fc.Gemini.RunwayHours),
			fc.Claude.RemainingPct, fc.Claude.BurnRatePctH, predictor.FormatRunwayShort(fc.Claude.RunwayHours),
			fc.Weekly.RemainingPct,
		)
		return
	}

	if *doTrigger {
		if err := runOneCycle(ctx, cfg, store, agClient, pub, true); err != nil {
			log.Fatalf("trigger-refresh failed: %v", err)
		}
		return
	}

	// Headless daemon mode (systemd-ready)
	log.Printf("antigravity-zectrix %s starting (poll=%ds, auth_refresh=%ds, tz=%s)",
		appVersion, cfg.QuotaPollSeconds, cfg.AuthRefreshSeconds, cfg.Timezone)

	if !cfg.InPauseWindow(time.Now()) {
		if err := runOneCycle(ctx, cfg, store, agClient, pub, false); err != nil {
			log.Printf("initial cycle warning: %v", err)
		}
	}

	pollTicker := time.NewTicker(time.Duration(cfg.QuotaPollSeconds) * time.Second)
	defer pollTicker.Stop()
	authTicker := time.NewTicker(time.Duration(cfg.AuthRefreshSeconds) * time.Second)
	defer authTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("shutting down cleanly")
			return
		case <-authTicker.C:
			cred, _, err := store.Load()
			if err != nil {
				log.Printf("auth ticker load error: %v", err)
				continue
			}
			updated, err := store.RefreshToken(ctx, cred)
			if err != nil {
				log.Printf("auth ticker refresh error: %v", err)
				continue
			}
			if err := store.SaveAtomic(updated); err != nil {
				log.Printf("auth ticker save error: %v", err)
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

func pollAndPredict(ctx context.Context, cfg *config.Config, store *auth.Store, agClient *antigravity.Client) (*predictor.Forecast, error) {
	st, err := predictor.LoadState(cfg.StateFile)
	if err != nil {
		return nil, err
	}
	cred, wasEncrypted, err := store.Load()
	if err != nil {
		return nil, err
	}

	snap, err := agClient.FetchQuota(ctx, cred.AccessToken, cred.ProjectID)
	if errors.Is(err, antigravity.ErrUnauthorized) {
		log.Println("quota API returned 401; refreshing OAuth token and retrying once")
		updated, rerr := store.RefreshToken(ctx, cred)
		if rerr != nil {
			return st.Compute(time.Now(), time.Duration(cfg.StaleAfterSeconds)*time.Second), rerr
		}
		cred = updated
		_ = store.SaveAtomic(cred)
		snap, err = agClient.FetchQuota(ctx, cred.AccessToken, cred.ProjectID)
	}
	if err != nil {
		// Never fabricate predictions: degrade to cached state marked stale if available
		fc := st.Compute(time.Now(), time.Duration(cfg.StaleAfterSeconds)*time.Second)
		fc.Stale = true
		return fc, err
	}

	// First successful plaintext read -> atomically encrypt CPA JSON in-place if key file configured
	if !wasEncrypted && cfg.EncryptionKeyFile != "" {
		if serr := store.SaveAtomic(cred); serr != nil {
			log.Printf("warning: failed to encrypt CPA JSON in-place: %v", serr)
		} else {
			log.Println("CPA Antigravity JSON encrypted in-place with AES-256-GCM")
		}
	}

	st.AppendAndPrune(*snap, cfg.HistoryMaxHours)
	_ = st.SaveAtomic(cfg.StateFile)
	return st.Compute(time.Now(), time.Duration(cfg.StaleAfterSeconds)*time.Second), nil
}

func runOneCycle(ctx context.Context, cfg *config.Config, store *auth.Store, agClient *antigravity.Client, pub *publisher.ZectrixClient, force bool) error {
	fc, pollErr := pollAndPredict(ctx, cfg, store, agClient)
	if pollErr != nil && fc == nil {
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
	log.Printf("frame sha256=%s bytes=%d decision=%s", frame.SHA256Hex[:12], len(frame.PNG), dec)
	return pollErr
}

func runPreview(cfg *config.Config, outPath string) error {
	now := time.Now().In(cfg.Location)
	st, _ := predictor.LoadState(cfg.StateFile)
	var fc *predictor.Forecast
	if len(st.Snapshots) > 0 {
		fc = st.Compute(now, time.Duration(cfg.StaleAfterSeconds)*time.Second)
	} else {
		fc = sampleForecast(now)
	}
	frame, err := render.RenderDashboard(fc, cfg.Location)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(outPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create preview dir %q: %w", dir, err)
		}
	}
	if err := os.WriteFile(outPath, frame.PNG, 0644); err != nil {
		return err
	}
	fmt.Printf("Rendered %dx%d 16-gray PNG to %s (%d bytes, sha256=%s)\n",
		frame.Width, frame.Height, outPath, len(frame.PNG), frame.SHA256Hex[:12])
	return nil
}

func sampleForecast(now time.Time) *predictor.Forecast {
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
		UpdatedAt: now, Stale: false, Gemini: gem, Claude: cla, Weekly: wk, Bottleneck: cla,
	}
}
