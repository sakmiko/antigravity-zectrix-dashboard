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
	"sync"
	"time"
)

var ErrUnauthorized = errors.New("upstream returned HTTP 401 unauthorized")

const DefaultUserAgent = "antigravity/1.15.8 darwin/arm64"

const (
	TierUnknown = ""
	TierFree    = "FREE"
	TierPlus    = "PLUS"
	TierPro     = "PRO"
	TierUltra   = "ULTRA"
)

// PoolQuota captures the remaining fraction and reset timestamp for a quota pool.
type PoolQuota struct {
	Name         string    `json:"name"`
	RemainingPct float64   `json:"remaining_pct"` // 0..100
	ResetAt      time.Time `json:"reset_at"`
	Available    bool      `json:"available"`
}

// Snapshot is a single verified observation of Antigravity quotas and plan tier.
type Snapshot struct {
	CapturedAt time.Time `json:"captured_at"`
	PlanTier   string    `json:"plan_tier,omitempty"` // FREE, PLUS, PRO, ULTRA
	ProjectID  string    `json:"project_id,omitempty"`
	Gemini     PoolQuota `json:"gemini"`
	Claude     PoolQuota `json:"claude"`
	Weekly     PoolQuota `json:"weekly"`
}

type CodeAssistInfo struct {
	ProjectID string
	PlanTier  string
	RawTierID string
}

type Client struct {
	mu        sync.RWMutex
	BaseURLs  []string
	UserAgent string
	HTTP      *http.Client
}

func NewClient(primaryBaseURL, proxyURL, userAgent string) *Client {
	c := &Client{}
	c.UpdateConfig(primaryBaseURL, proxyURL, userAgent)
	return c
}

func (c *Client) UpdateConfig(primaryBaseURL, proxyURL, userAgent string) {
	tr := &http.Transport{IdleConnTimeout: 30 * time.Second}
	if strings.TrimSpace(proxyURL) != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	base := strings.TrimRight(strings.TrimSpace(primaryBaseURL), "/")
	if base == "" {
		base = "https://cloudcode-pa.googleapis.com"
	}
	bases := []string{base}
	if strings.Contains(base, "googleapis.com") {
		if !strings.Contains(base, "daily-cloudcode-pa") {
			bases = append(bases, "https://daily-cloudcode-pa.googleapis.com")
		}
		if !strings.Contains(base, "cloudcode-pa.googleapis.com") || strings.Contains(base, "daily-cloudcode-pa") {
			bases = append(bases, "https://cloudcode-pa.googleapis.com")
		}
	}
	ua := strings.TrimSpace(userAgent)
	if ua == "" {
		ua = DefaultUserAgent
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.BaseURLs = bases
	c.UserAgent = ua
	c.HTTP = &http.Client{Transport: tr, Timeout: 20 * time.Second}
}

func (c *Client) snapshotConfig() ([]string, string, *http.Client) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	bases := make([]string, len(c.BaseURLs))
	copy(bases, c.BaseURLs)
	return bases, c.UserAgent, c.HTTP
}

// NormalizeTier maps Google Cloud Code / Antigravity tier IDs to standard display badges.
func NormalizeTier(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return TierUnknown
	}
	switch {
	case strings.Contains(s, "ultra"):
		return TierUltra
	case strings.Contains(s, "pro"):
		return TierPro
	case strings.Contains(s, "plus") || strings.Contains(s, "standard"):
		return TierPlus
	case strings.Contains(s, "free") || strings.Contains(s, "legacy") || strings.Contains(s, "starter"):
		return TierFree
	default:
		return strings.ToUpper(s)
	}
}

type tierObj struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type loadCodeAssistResp struct {
	CurrentTier             *tierObj        `json:"currentTier"`
	PaidTier                *tierObj        `json:"paidTier"`
	CloudAICompanionProject json.RawMessage `json:"cloudaicompanionProject"`
}

// LoadCodeAssist performs the lightweight Cloud Code session handshake (never calls
// any LLM inference endpoint) to discover project ID and subscription tier.
func (c *Client) LoadCodeAssist(ctx context.Context, accessToken, projectID string) (*CodeAssistInfo, error) {
	if accessToken == "" {
		return nil, ErrUnauthorized
	}
	bases, ua, httpClient := c.snapshotConfig()

	reqMap := map[string]any{
		"metadata": map[string]string{
			"ideType":    "ANTIGRAVITY",
			"platform":   "PLATFORM_UNSPECIFIED",
			"pluginType": "GEMINI",
		},
	}
	if projectID != "" {
		reqMap["cloudaicompanionProject"] = projectID
	}
	bodyBytes, _ := json.Marshal(reqMap)

	var lastErr error
	for _, base := range bases {
		endpoint := base + "/v1internal:loadCodeAssist"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		applyAntigravityHeaders(req, accessToken, ua)

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
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

		var parsed loadCodeAssistResp
		if err := json.Unmarshal(raw, &parsed); err != nil {
			lastErr = fmt.Errorf("decode loadCodeAssist: %w", err)
			continue
		}

		info := &CodeAssistInfo{
			ProjectID: parseCompanionProject(parsed.CloudAICompanionProject),
		}
		if info.ProjectID == "" {
			info.ProjectID = projectID
		}
		// Prefer paidTier if present and non-free; fallback to currentTier.
		if parsed.PaidTier != nil {
			cand := parsed.PaidTier.ID
			if cand == "" {
				cand = parsed.PaidTier.Name
			}
			if norm := NormalizeTier(cand); norm != "" {
				info.PlanTier = norm
				info.RawTierID = cand
			}
		}
		if info.PlanTier == "" || info.PlanTier == TierFree {
			if parsed.CurrentTier != nil {
				cand := parsed.CurrentTier.ID
				if cand == "" {
					cand = parsed.CurrentTier.Name
				}
				if norm := NormalizeTier(cand); norm != "" && (info.PlanTier == "" || norm != TierFree) {
					info.PlanTier = norm
					info.RawTierID = cand
				}
			}
		}
		return info, nil
	}
	return nil, fmt.Errorf("loadCodeAssist failed: %w", lastErr)
}

func parseCompanionProject(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	var strVal string
	if err := json.Unmarshal(raw, &strVal); err == nil {
		return strings.TrimSpace(strVal)
	}
	var objVal struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &objVal); err == nil {
		return strings.TrimSpace(objVal.ID)
	}
	return ""
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

type quotaSummaryResp struct {
	Buckets []struct {
		ModelID           string   `json:"modelId"`
		ModelGroup        string   `json:"modelGroup"`
		BucketID          string   `json:"bucketId"`
		RemainingFraction *float64 `json:"remainingFraction"`
		ResetTime         string   `json:"resetTime"`
	} `json:"buckets"`
}

// FetchQuota queries loadCodeAssist (to discover project_id and plan tier) and
// fetchAvailableModels / retrieveUserQuotaSummary across primary & daily endpoints,
// selecting the most informative verified snapshot without fabricating missing pools.
func (c *Client) FetchQuota(ctx context.Context, accessToken, projectID string) (*Snapshot, error) {
	if accessToken == "" {
		return nil, ErrUnauthorized
	}

	effectiveProject := strings.TrimSpace(projectID)
	discoveredTier := TierUnknown
	if info, err := c.LoadCodeAssist(ctx, accessToken, effectiveProject); err == nil && info != nil {
		if info.ProjectID != "" {
			effectiveProject = info.ProjectID
		}
		discoveredTier = info.PlanTier
	} else if errors.Is(err, ErrUnauthorized) {
		return nil, ErrUnauthorized
	}

	bases, ua, httpClient := c.snapshotConfig()
	reqBody := map[string]any{}
	if effectiveProject != "" {
		reqBody["project"] = effectiveProject
	}
	bodyBytes, _ := json.Marshal(reqBody)

	var bestSnap *Snapshot
	var lastErr error
	for _, base := range bases {
		snap, err := c.fetchModelsFromBase(ctx, httpClient, base, ua, accessToken, bodyBytes)
		if errors.Is(err, ErrUnauthorized) {
			return nil, ErrUnauthorized
		}
		if err != nil {
			lastErr = err
			// Fallback: try retrieveUserQuotaSummary on the same base
			if sumSnap, serr := c.fetchQuotaSummaryFromBase(ctx, httpClient, base, ua, accessToken, bodyBytes); serr == nil {
				snap = sumSnap
			} else {
				if errors.Is(serr, ErrUnauthorized) {
					return nil, ErrUnauthorized
				}
				continue
			}
		}

		snap.PlanTier = discoveredTier
		snap.ProjectID = effectiveProject

		if bestSnap == nil || isMoreInformativeSnapshot(snap, bestSnap) {
			bestSnap = snap
		}
		// If we already observed both pools and at least one shows active consumption (< 100%)
		// or weekly info is present, no need to probe further endpoints.
		if bestSnap.Gemini.Available && bestSnap.Claude.Available &&
			(bestSnap.Gemini.RemainingPct < 99.9 || bestSnap.Claude.RemainingPct < 99.9) {
			break
		}
	}

	if bestSnap != nil {
		return bestSnap, nil
	}
	return nil, fmt.Errorf("antigravity quota fetch failed: %w", lastErr)
}

func (c *Client) fetchModelsFromBase(ctx context.Context, httpClient *http.Client, base, ua, accessToken string, bodyBytes []byte) (*Snapshot, error) {
	endpoint := base + "/v1internal:fetchAvailableModels"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	applyAntigravityHeaders(req, accessToken, ua)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s HTTP %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var parsed fetchModelsResp
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode fetchAvailableModels: %w", err)
	}
	snap := aggregatePools(parsed, time.Now().UTC())
	if !snap.Gemini.Available && !snap.Claude.Available {
		return nil, fmt.Errorf("%s returned no quotaInfo fields", endpoint)
	}
	return snap, nil
}

func (c *Client) fetchQuotaSummaryFromBase(ctx context.Context, httpClient *http.Client, base, ua, accessToken string, bodyBytes []byte) (*Snapshot, error) {
	endpoint := base + "/v1internal:retrieveUserQuotaSummary"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	applyAntigravityHeaders(req, accessToken, ua)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s HTTP %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var parsed quotaSummaryResp
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode retrieveUserQuotaSummary: %w", err)
	}
	snap := aggregateSummaryBuckets(parsed, time.Now().UTC())
	if !snap.Gemini.Available && !snap.Claude.Available {
		return nil, fmt.Errorf("%s returned no usable buckets", endpoint)
	}
	return snap, nil
}

func applyAntigravityHeaders(req *http.Request, accessToken, ua string) {
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("X-Goog-Api-Client", "google-cloud-sdk vscode_cloudshelleditor/0.1")
	req.Header.Set("Client-Metadata", `{"ideType":"ANTIGRAVITY","platform":"PLATFORM_UNSPECIFIED","pluginType":"GEMINI"}`)
}

func isMoreInformativeSnapshot(cand, cur *Snapshot) bool {
	candPools := boolToInt(cand.Gemini.Available) + boolToInt(cand.Claude.Available) + boolToInt(cand.Weekly.Available)
	curPools := boolToInt(cur.Gemini.Available) + boolToInt(cur.Claude.Available) + boolToInt(cur.Weekly.Available)
	if candPools != curPools {
		return candPools > curPools
	}
	// If one endpoint reports active consumption (< 100%) while the other reports static 100%,
	// prefer the one showing real consumption.
	candDepleted := (cand.Gemini.Available && cand.Gemini.RemainingPct < 99.9) || (cand.Claude.Available && cand.Claude.RemainingPct < 99.9)
	curDepleted := (cur.Gemini.Available && cur.Gemini.RemainingPct < 99.9) || (cur.Claude.Available && cur.Claude.RemainingPct < 99.9)
	if candDepleted != curDepleted {
		return candDepleted
	}
	return false
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func aggregatePools(resp fetchModelsResp, now time.Time) *Snapshot {
	snap := &Snapshot{
		CapturedAt: now,
		Gemini:     PoolQuota{Name: "GEMINI"},
		Claude:     PoolQuota{Name: "CLAUDE"},
		Weekly:     PoolQuota{Name: "WEEKLY"},
	}

	for modelID, m := range resp.Models {
		lower := strings.ToLower(modelID + " " + m.DisplayName)
		if isIgnoredAuxiliaryModel(lower) {
			continue
		}
		if m.QuotaInfo != nil {
			frac, ok := resolveRemainingFraction(m.QuotaInfo.RemainingFraction, m.QuotaInfo.ResetTime)
			if ok {
				switch classifyModelPool(lower) {
				case "gemini":
					updatePoolMin(&snap.Gemini, frac, m.QuotaInfo.ResetTime)
				case "claude":
					updatePoolMin(&snap.Claude, frac, m.QuotaInfo.ResetTime)
				}
			}
		}
		if m.WeeklyQuotaInfo != nil {
			if frac, ok := resolveRemainingFraction(m.WeeklyQuotaInfo.RemainingFraction, m.WeeklyQuotaInfo.ResetTime); ok {
				updatePoolMin(&snap.Weekly, frac, m.WeeklyQuotaInfo.ResetTime)
			}
		}
	}
	return snap
}

func aggregateSummaryBuckets(resp quotaSummaryResp, now time.Time) *Snapshot {
	snap := &Snapshot{
		CapturedAt: now,
		Gemini:     PoolQuota{Name: "GEMINI"},
		Claude:     PoolQuota{Name: "CLAUDE"},
		Weekly:     PoolQuota{Name: "WEEKLY"},
	}
	for _, b := range resp.Buckets {
		frac, ok := resolveRemainingFraction(b.RemainingFraction, b.ResetTime)
		if !ok {
			continue
		}
		key := strings.ToLower(b.ModelID + " " + b.ModelGroup + " " + b.BucketID)
		if strings.Contains(key, "weekly") {
			updatePoolMin(&snap.Weekly, frac, b.ResetTime)
			continue
		}
		switch classifyModelPool(key) {
		case "gemini":
			updatePoolMin(&snap.Gemini, frac, b.ResetTime)
		case "claude":
			updatePoolMin(&snap.Claude, frac, b.ResetTime)
		}
	}
	return snap
}

// resolveRemainingFraction handles Protobuf JSON omitting remainingFraction when it is 0.0
// while still emitting resetTime on active quota buckets.
func resolveRemainingFraction(frac *float64, resetTime string) (float64, bool) {
	if frac != nil {
		return *frac, true
	}
	if strings.TrimSpace(resetTime) != "" {
		return 0.0, true
	}
	return 0, false
}

func isIgnoredAuxiliaryModel(lower string) bool {
	return strings.Contains(lower, "embedding") ||
		strings.Contains(lower, "imagen") ||
		strings.Contains(lower, "veo") ||
		strings.Contains(lower, "aqa")
}

func classifyModelPool(lower string) string {
	if strings.Contains(lower, "gemini") {
		return "gemini"
	}
	if strings.Contains(lower, "claude") ||
		strings.Contains(lower, "opus") ||
		strings.Contains(lower, "sonnet") ||
		strings.Contains(lower, "haiku") ||
		strings.Contains(lower, "gpt") ||
		strings.Contains(lower, "3p") {
		return "claude"
	}
	return ""
}

func updatePoolMin(dst *PoolQuota, frac float64, resetStr string) {
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
	} else if dst.ResetAt.IsZero() && !rt.IsZero() {
		dst.ResetAt = rt
	}
}
