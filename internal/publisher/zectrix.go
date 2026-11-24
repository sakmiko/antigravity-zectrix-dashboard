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
	if minIntervalSec < 60 {
		minIntervalSec = 60
	}
	return &ZectrixClient{
		BaseURL:         strings.TrimRight(baseURL, "/"),
		DeviceID:        deviceID,
		APIKey:          apiKey,
		PageID:          pageID,
		Dither:          dither,
		MinPushInterval: time.Duration(minIntervalSec) * time.Second,
		HTTP:            &http.Client{Timeout: 25 * time.Second},
	}
}

func (z *ZectrixClient) Decide(hashHex string, now time.Time, force bool) Decision {
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
	now := time.Now()
	dec := z.Decide(hashHex, now, force)
	if dec != DecisionPublish {
		return dec, nil
	}
	if z.DeviceID == "" || z.APIKey == "" {
		return "", errors.New("ZECTRIX_DEVICE_ID and ZECTRIX_API_KEY must be configured")
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("pageId", z.PageID)
	_ = mw.WriteField("dither", strconv.FormatBool(z.Dither))
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

	url := fmt.Sprintf("%s/open/v1/devices/%s/display/image", z.BaseURL, z.DeviceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-API-Key", z.APIKey)

	resp, err := z.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("zectrix push request: %w", err)
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("zectrix push HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	z.lastHash = hashHex
	z.lastPushed = now
	return DecisionPublish, nil
}
