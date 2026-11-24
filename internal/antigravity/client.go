package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrUnauthorized = errors.New("upstream returned HTTP 401 unauthorized")

// PoolQuota captures the remaining fraction and reset timestamp for a quota pool.
type PoolQuota struct {
	Name         string    `json:"name"`
	RemainingPct float64   `json:"remaining_pct"` // 0..100
	ResetAt      time.Time `json:"reset_at"`
	Available    bool      `json:"available"`
}

// Snapshot is a single verified observation of Antigravity quotas.
type Snapshot struct {
	CapturedAt time.Time `json:"captured_at"`
	Gemini     PoolQuota `json:"gemini"`
	Claude     PoolQuota `json:"claude"`
	Weekly     PoolQuota `json:"weekly"`
}

type Client struct {
	BaseURLs []string
	HTTP     *http.Client
}

func NewClient(primaryBaseURL, proxyURL string) *Client {
	tr := &http.Transport{IdleConnTimeout: 30 * time.Second}
	if strings.TrimSpace(proxyURL) != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	} else {
		// 未显式配置时尊重标准 HTTPS_PROXY / HTTP_PROXY / NO_PROXY
		tr.Proxy = http.ProxyFromEnvironment
	}
	bases := []string{strings.TrimRight(primaryBaseURL, "/")}
	if !strings.Contains(primaryBaseURL, "daily-cloudcode-pa") {
		bases = append(bases, "https://daily-cloudcode-pa.googleapis.com")
	}
	return &Client{
		BaseURLs: bases,
		HTTP:     &http.Client{Transport: tr, Timeout: 20 * time.Second},
	}
}

type fetchModelsResp struct {
	Models map[string]struct {
		DisplayName string `json:"displayName"`
		QuotaInfo   *struct {
			RemainingFraction *float64 `json:"remainingFraction"`
			ResetTime         string   `json:"resetTime"`
		} `json:"quotaInfo"`
		WeeklyQuotaInfo *struct {
			RemainingFraction *float64 `json:"remainingFraction"`
			ResetTime         string   `json:"resetTime"`
		} `json:"weeklyQuotaInfo"`
	} `json:"models"`
}

// FetchQuota queries Antigravity Cloud Code Assist fetchAvailableModels and aggregates
// models into Gemini, Claude, and Weekly quota pools.
func (c *Client) FetchQuota(ctx context.Context, accessToken, projectID string) (*Snapshot, error) {
	if accessToken == "" {
		return nil, ErrUnauthorized
	}
	reqBody := map[string]any{}
	if projectID != "" {
		reqBody["project"] = projectID
	}
	bodyBytes, _ := json.Marshal(reqBody)

	var lastErr error
	for _, base := range c.BaseURLs {
		endpoint := base + "/v1internal:fetchAvailableModels"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "antigravity/2.0")

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, ErrUnauthorized
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("%s HTTP %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(raw)))
			continue
		}

		var parsed fetchModelsResp
		if err := json.Unmarshal(raw, &parsed); err != nil {
			lastErr = fmt.Errorf("decode fetchAvailableModels: %w", err)
			continue
		}
		snap := aggregatePools(parsed, time.Now().UTC())
		if snap.Gemini.Available || snap.Claude.Available {
			return snap, nil
		}
		lastErr = fmt.Errorf("%s returned no quotaInfo fields", endpoint)
	}
	return nil, fmt.Errorf("antigravity quota fetch failed: %w", lastErr)
}

func aggregatePools(resp fetchModelsResp, now time.Time) *Snapshot {
	snap := &Snapshot{
		CapturedAt: now,
		Gemini:     PoolQuota{Name: "GEMINI"},
		Claude:     PoolQuota{Name: "CLAUDE"},
		Weekly:     PoolQuota{Name: "WEEKLY"},
	}

	updateMin := func(dst *PoolQuota, frac float64, resetStr string) {
		pct := frac * 100.0
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		rt, _ := time.Parse(time.RFC3339, resetStr)
		if !dst.Available || pct < dst.RemainingPct {
			dst.RemainingPct = pct
			if !rt.IsZero() {
				dst.ResetAt = rt
			}
			dst.Available = true
		}
	}

	for modelID, m := range resp.Models {
		lower := strings.ToLower(modelID + " " + m.DisplayName)
		if m.QuotaInfo != nil && m.QuotaInfo.RemainingFraction != nil {
			if strings.Contains(lower, "gemini") {
				updateMin(&snap.Gemini, *m.QuotaInfo.RemainingFraction, m.QuotaInfo.ResetTime)
			} else if strings.Contains(lower, "claude") || strings.Contains(lower, "opus") || strings.Contains(lower, "sonnet") {
				updateMin(&snap.Claude, *m.QuotaInfo.RemainingFraction, m.QuotaInfo.ResetTime)
			}
		}
		if m.WeeklyQuotaInfo != nil && m.WeeklyQuotaInfo.RemainingFraction != nil {
			updateMin(&snap.Weekly, *m.WeeklyQuotaInfo.RemainingFraction, m.WeeklyQuotaInfo.ResetTime)
		}
	}

	// If upstream did not return a separate weekly pool, fall back to the conservative minimum across active pools.
	if !snap.Weekly.Available && (snap.Gemini.Available || snap.Claude.Available) {
		best := 100.0
		var rt time.Time
		if snap.Gemini.Available && snap.Gemini.RemainingPct < best {
			best = snap.Gemini.RemainingPct
			rt = snap.Gemini.ResetAt
		}
		if snap.Claude.Available && snap.Claude.RemainingPct < best {
			best = snap.Claude.RemainingPct
			rt = snap.Claude.ResetAt
		}
		snap.Weekly = PoolQuota{Name: "WEEKLY", RemainingPct: best, ResetAt: rt, Available: true}
	}
	return snap
}
