package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func jsonResponse(status int, payload any) *http.Response {
	b, _ := json.Marshal(payload)
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(b)),
	}
}

func TestFetchQuotaWithLoadCodeAssistAndZeroFraction(t *testing.T) {
	c := NewClient("https://mock.antigravity.local", "", "")
	c.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			return jsonResponse(http.StatusOK, map[string]any{
				"cloudaicompanionProject": map[string]string{"id": "proj-discovered"},
				"currentTier":             map[string]string{"id": "free-tier"},
				"paidTier":                map[string]string{"id": "g1-ultra-tier"},
			}), nil
		case "/v1internal:fetchAvailableModels":
			var reqBody map[string]any
			_ = json.NewDecoder(r.Body).Decode(&reqBody)
			if reqBody["project"] != "proj-discovered" {
				t.Fatalf("expected discovered project in request, got %v", reqBody["project"])
			}
			return jsonResponse(http.StatusOK, map[string]any{
				"models": map[string]any{
					"gemini-2.5-pro": map[string]any{
						"displayName": "Gemini 2.5 Pro",
						"quotaInfo": map[string]any{
							"remainingFraction": 0.65,
							"resetTime":         "2026-11-26T04:00:00Z",
						},
					},
					"claude-sonnet-4-5": map[string]any{
						"displayName": "Claude Sonnet 4.5",
						// remainingFraction omitted by protobuf JSON when 0.0
						"quotaInfo": map[string]any{
							"resetTime": "2026-11-25T19:00:00Z",
						},
					},
				},
			}), nil
		default:
			return jsonResponse(http.StatusNotFound, map[string]any{"error": "not found"}), nil
		}
	})

	snap, err := c.FetchQuota(context.Background(), "ya29.test", "")
	if err != nil {
		t.Fatalf("FetchQuota: %v", err)
	}
	if snap.PlanTier != TierUltra {
		t.Fatalf("expected ULTRA tier, got %q", snap.PlanTier)
	}
	if snap.ProjectID != "proj-discovered" {
		t.Fatalf("expected proj-discovered, got %q", snap.ProjectID)
	}
	if !snap.Gemini.Available || snap.Gemini.RemainingPct != 65 {
		t.Fatalf("unexpected gemini pool: %+v", snap.Gemini)
	}
	if !snap.Claude.Available || snap.Claude.RemainingPct != 0 {
		t.Fatalf("expected exhausted 0%% claude pool when remainingFraction omitted with resetTime, got %+v", snap.Claude)
	}
	if snap.Weekly.Available {
		t.Fatalf("expected Weekly.Available=false when upstream omitted weeklyQuotaInfo, got %+v", snap.Weekly)
	}
}
