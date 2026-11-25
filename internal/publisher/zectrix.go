package publisher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const MaxImageBytes = 2 * 1024 * 1024 // 2 MiB hard device limit

type Decision string

const (
	DecisionPublish  Decision = "publish"
	DecisionSkipSame Decision = "skip_unchanged_hash"
	DecisionSkipRate Decision = "skip_min_interval"
)

type ZectrixClient struct {
	mu              sync.Mutex
	BaseURL         string
	DeviceID        string
	APIKey          string
	PageID          string
	Dither          bool
	MinPushInterval time.Duration
	HTTP            *http.Client

	lastHash   string
	lastPushed time.Time
}

func NewZectrixClient(baseURL, deviceID, apiKey, pageID string, dither bool, minIntervalSec int) *ZectrixClient {
	z := &ZectrixClient{
		HTTP: &http.Client{Timeout: 25 * time.Second},
	}
	z.UpdateConfig(baseURL, deviceID, apiKey, pageID, dither, minIntervalSec)
	return z
}

func (z *ZectrixClient) UpdateConfig(baseURL, deviceID, apiKey, pageID string, dither bool, minIntervalSec int) {
	if minIntervalSec < 60 {
		minIntervalSec = 60
	}
	z.mu.Lock()
	defer z.mu.Unlock()
	z.BaseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	z.DeviceID = strings.TrimSpace(deviceID)
	z.APIKey = strings.TrimSpace(apiKey)
	z.PageID = strings.TrimSpace(pageID)
	z.Dither = dither
	z.MinPushInterval = time.Duration(minIntervalSec) * time.Second
}

func (z *ZectrixClient) SeedState(lastHash string, lastPushed time.Time) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.lastHash = lastHash
	z.lastPushed = lastPushed
}

func (z *ZectrixClient) LastState() (string, time.Time) {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.lastHash, z.lastPushed
}

func (z *ZectrixClient) Decide(hashHex string, now time.Time, force bool) Decision {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.decideLocked(hashHex, now, force)
}

func (z *ZectrixClient) decideLocked(hashHex string, now time.Time, force bool) Decision {
	if !force && z.lastHash != "" && z.lastHash == hashHex {
		return DecisionSkipSame
	}
	if !force && !z.lastPushed.IsZero() && now.Sub(z.lastPushed) < z.MinPushInterval {
		return DecisionSkipRate
	}
	return DecisionPublish
}

func (z *ZectrixClient) Publish(ctx context.Context, pngBytes []byte, hashHex string, force bool) (Decision, error) {
	if len(pngBytes) > MaxImageBytes {
		return "", fmt.Errorf("rendered PNG (%d bytes) exceeds 2 MiB device limit", len(pngBytes))
	}

	z.mu.Lock()
	now := time.Now()
	if dec := z.decideLocked(hashHex, now, force); dec != DecisionPublish {
		z.mu.Unlock()
		return dec, nil
	}
	baseURL := z.BaseURL
	deviceID := z.DeviceID
	apiKey := z.APIKey
	pageID := z.PageID
	dither := z.Dither
	httpClient := z.HTTP
	z.mu.Unlock()

	if deviceID == "" || apiKey == "" {
		return "", errors.New("ZECTRIX_DEVICE_ID and ZECTRIX_API_KEY must be configured")
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("pageId", pageID)
	_ = mw.WriteField("dither", strconv.FormatBool(dither))
	fw, err := mw.CreateFormFile("images", "antigravity-dashboard.png")
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(pngBytes); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	url := fmt.Sprintf("%s/open/v1/devices/%s/display/image", baseURL, deviceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-API-Key", apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("zectrix push request: %w", err)
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("zectrix push HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	z.mu.Lock()
	z.lastHash = hashHex
	z.lastPushed = now
	z.mu.Unlock()
	return DecisionPublish, nil
}
