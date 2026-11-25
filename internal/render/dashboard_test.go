package render

import (
	"bytes"
	"image/png"
	"testing"
	"time"

	"github.com/sakmiko/antigravity-zectrix-dashboard/internal/predictor"
)

func TestRenderDashboardDimensionsAndTiers(t *testing.T) {
	now := time.Date(2026, 11, 24, 14, 30, 0, 0, time.UTC)
	cla := predictor.PoolPrediction{
		Name: "CLAUDE", RemainingPct: 38, BurnRatePctH: 8.5,
		RunwayHours: 4.5, ExhaustAt: now.Add(270 * time.Minute),
		ResetAt: now.Add(270 * time.Minute), Available: true,
		History: []predictor.Point{
			{Time: now.Add(-6 * time.Hour), Pct: 84},
			{Time: now.Add(-3 * time.Hour), Pct: 61},
			{Time: now, Pct: 38},
		},
	}
	gem := predictor.PoolPrediction{
		Name: "GEMINI", RemainingPct: 74, BurnRatePctH: 3.2,
		RunwayHours: 23.0, ResetAt: now.Add(14 * time.Hour), Available: true,
		History: []predictor.Point{
			{Time: now.Add(-6 * time.Hour), Pct: 93},
			{Time: now, Pct: 74},
		},
	}
	wk := predictor.PoolPrediction{
		Name: "WEEKLY", RemainingPct: 62, ResetAt: now.Add(86 * time.Hour), Available: true, RunwayHours: -1,
	}

	tiers := []string{"FREE", "PLUS", "PRO", "ULTRA"}
	hashes := make(map[string]string)
	for _, tier := range tiers {
		fc := &predictor.Forecast{
			UpdatedAt: now, PlanTier: tier, KeepaliveEnabled: true,
			Gemini: gem, Claude: cla, Weekly: wk, Bottleneck: cla,
		}
		r1, err := RenderDashboard(fc, time.UTC)
		if err != nil {
			t.Fatalf("RenderDashboard(%s): %v", tier, err)
		}
		img, err := png.Decode(bytes.NewReader(r1.PNG))
		if err != nil {
			t.Fatalf("png.Decode(%s): %v", tier, err)
		}
		if img.Bounds().Dx() != DisplayWidth || img.Bounds().Dy() != DisplayHeight {
			t.Fatalf("unexpected bounds: %v", img.Bounds())
		}
		r2, err := RenderDashboard(fc, time.UTC)
		if err != nil {
			t.Fatalf("RenderDashboard 2(%s): %v", tier, err)
		}
		if r1.SHA256Hex != r2.SHA256Hex {
			t.Fatalf("expected deterministic frame hash for %s", tier)
		}
		hashes[tier] = r1.SHA256Hex
	}
	if len(hashes) != len(tiers) {
		t.Fatalf("expected distinct hashes across tiers")
	}
}
