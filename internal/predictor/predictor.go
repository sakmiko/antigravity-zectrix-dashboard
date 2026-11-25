package predictor

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sakmiko/antigravity-zectrix-dashboard/internal/antigravity"
)

type Point struct {
	Time    time.Time `json:"time"`
	Pct     float64   `json:"pct"`
	ResetAt time.Time `json:"reset_at,omitempty"`
}

type PoolPrediction struct {
	Name         string    `json:"name"`
	RemainingPct float64   `json:"remaining_pct"`
	BurnRatePctH float64   `json:"burn_rate_pct_h"` // positive = burning %/h
	RunwayHours  float64   `json:"runway_hours"`    // -1 = stable / no burn
	ExhaustAt    time.Time `json:"exhaust_at"`
	ResetAt      time.Time `json:"reset_at"`
	Available    bool      `json:"available"`
	History      []Point   `json:"history"`
}

type Forecast struct {
	UpdatedAt        time.Time      `json:"updated_at"`
	Stale            bool           `json:"stale"`
	PlanTier         string         `json:"plan_tier,omitempty"`
	KeepaliveEnabled bool           `json:"keepalive_enabled,omitempty"`
	Gemini           PoolPrediction `json:"gemini"`
	Claude           PoolPrediction `json:"claude"`
	Weekly           PoolPrediction `json:"weekly"`
	Bottleneck       PoolPrediction `json:"bottleneck"`
}

type State struct {
	Snapshots    []antigravity.Snapshot `json:"snapshots"`
	LastPushHash string                 `json:"last_push_hash,omitempty"`
	LastPushedAt time.Time              `json:"last_pushed_at,omitempty"`
}

func LoadState(path string) (*State, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{}, nil
		}
		return nil, err
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return &State{}, nil
	}
	return &st, nil
}

func (s *State) AppendAndPrune(snap antigravity.Snapshot, maxHours int) {
	s.Snapshots = append(s.Snapshots, snap)
	cutoff := snap.CapturedAt.Add(-time.Duration(maxHours) * time.Hour)
	kept := s.Snapshots[:0]
	for _, item := range s.Snapshots {
		if !item.CapturedAt.Before(cutoff) {
			kept = append(kept, item)
		}
	}
	s.Snapshots = kept
}

func (s *State) SaveAtomic(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Compute builds a runway & burn-rate forecast from recorded snapshots.
// It never fabricates predictions when no snapshots exist.
func (s *State) Compute(now time.Time, staleAfter time.Duration, tierOverride string, keepaliveEnabled bool) *Forecast {
	tier := resolvePlanTier(tierOverride, "")
	if len(s.Snapshots) == 0 {
		return &Forecast{
			UpdatedAt:        now,
			Stale:            true,
			PlanTier:         tier,
			KeepaliveEnabled: keepaliveEnabled,
		}
	}
	latest := s.Snapshots[len(s.Snapshots)-1]
	stale := now.Sub(latest.CapturedAt) > staleAfter
	tier = resolvePlanTier(tierOverride, latest.PlanTier)

	gemPts := make([]Point, 0, len(s.Snapshots))
	claPts := make([]Point, 0, len(s.Snapshots))
	for _, sn := range s.Snapshots {
		if sn.Gemini.Available {
			gemPts = append(gemPts, Point{Time: sn.CapturedAt, Pct: sn.Gemini.RemainingPct, ResetAt: sn.Gemini.ResetAt})
		}
		if sn.Claude.Available {
			claPts = append(claPts, Point{Time: sn.CapturedAt, Pct: sn.Claude.RemainingPct, ResetAt: sn.Claude.ResetAt})
		}
	}

	gem := predictPool(latest.Gemini, gemPts, latest.CapturedAt)
	cla := predictPool(latest.Claude, claPts, latest.CapturedAt)
	wk := PoolPrediction{
		Name:         "WEEKLY",
		RemainingPct: latest.Weekly.RemainingPct,
		ResetAt:      latest.Weekly.ResetAt,
		Available:    latest.Weekly.Available,
		RunwayHours:  -1,
	}

	bottleneck := pickBottleneck(gem, cla)
	return &Forecast{
		UpdatedAt:        latest.CapturedAt,
		Stale:            stale,
		PlanTier:         tier,
		KeepaliveEnabled: keepaliveEnabled,
		Gemini:           gem,
		Claude:           cla,
		Weekly:           wk,
		Bottleneck:       bottleneck,
	}
}

func resolvePlanTier(override, detected string) string {
	ov := strings.ToLower(strings.TrimSpace(override))
	if ov != "" && ov != "auto" {
		return strings.ToUpper(ov)
	}
	return strings.ToUpper(strings.TrimSpace(detected))
}

func predictPool(cur antigravity.PoolQuota, pts []Point, now time.Time) PoolPrediction {
	p := PoolPrediction{
		Name:         cur.Name,
		RemainingPct: cur.RemainingPct,
		ResetAt:      cur.ResetAt,
		Available:    cur.Available,
		RunwayHours:  -1,
	}
	if !cur.Available {
		return p
	}
	// Detect quota reset by either upward jump (>10pp) or upstream ResetAt advancing (>30m).
	startIdx := 0
	for i := 1; i < len(pts); i++ {
		if pts[i].Pct-pts[i-1].Pct > 10.0 {
			startIdx = i
			continue
		}
		if !pts[i].ResetAt.IsZero() && !pts[i-1].ResetAt.IsZero() && pts[i].ResetAt.Sub(pts[i-1].ResetAt) > 30*time.Minute {
			startIdx = i
		}
	}
	window := pts[startIdx:]
	p.History = window
	if len(window) < 2 {
		return p
	}

	first := window[0]
	last := window[len(window)-1]
	dtHours := last.Time.Sub(first.Time).Hours()
	if dtHours < 0.05 {
		return p
	}

	// Weighted slope: 60% full-window average + 40% recent-segment burn rate
	avgBurn := (first.Pct - last.Pct) / dtHours
	recentBurn := avgBurn
	prev := window[len(window)-2]
	if rdt := last.Time.Sub(prev.Time).Hours(); rdt >= 0.05 {
		recentBurn = (prev.Pct - last.Pct) / rdt
	}
	burn := 0.6*avgBurn + 0.4*recentBurn
	if burn <= 0.05 {
		p.BurnRatePctH = 0
		p.RunwayHours = -1
		return p
	}

	p.BurnRatePctH = math.Round(burn*10) / 10
	runway := cur.RemainingPct / burn
	p.RunwayHours = runway
	if runway > 0 && runway <= 24*365 {
		p.ExhaustAt = now.Add(time.Duration(runway * float64(time.Hour)))
	}
	return p
}

func pickBottleneck(a, b PoolPrediction) PoolPrediction {
	if !a.Available {
		return b
	}
	if !b.Available {
		return a
	}
	if a.RunwayHours >= 0 && b.RunwayHours >= 0 {
		if a.RunwayHours <= b.RunwayHours {
			return a
		}
		return b
	}
	if a.RunwayHours >= 0 {
		return a
	}
	if b.RunwayHours >= 0 {
		return b
	}
	if a.RemainingPct <= b.RemainingPct {
		return a
	}
	return b
}

// FormatRunwayShort formats runway hours for compact bitmap displays (e.g. "4.5h", "23.0h", "3.2d", "--").
func FormatRunwayShort(hours float64) string {
	if hours < 0 {
		return "--"
	}
	if hours >= 48 {
		return fmt.Sprintf("%.1fd", hours/24.0)
	}
	return fmt.Sprintf("%.1fh", hours)
}

// FormatDurationCompact formats remaining duration like "3d 14h" or "4h 20m".
func FormatDurationCompact(d time.Duration) string {
	if d <= 0 {
		return "0h"
	}
	hours := int(d.Hours())
	days := hours / 24
	remH := hours % 24
	if days > 0 {
		return fmt.Sprintf("%dd %dh", days, remH)
	}
	mins := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh %02dm", remH, mins)
}
