package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"maxpilot/backend/internal/app"
)

func TestWordstatRateLimitIsPublic429WithRetryAfter(t *testing.T) {
	t.Parallel()
	response := httptest.NewRecorder()
	(&Server{}).writeError(response, fmt.Errorf("suggest keywords: %w", &app.WordstatRateLimitError{
		Reason: "provider", RetryAfter: 1500 * time.Millisecond,
	}))
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "2" ||
		response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response = %d Retry-After=%q Cache-Control=%q body=%s",
			response.Code, response.Header().Get("Retry-After"),
			response.Header().Get("Cache-Control"), response.Body.String())
	}
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Reason            string `json:"reason"`
				RetryAfterSeconds int64  `json:"retry_after_seconds"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Code != "wordstat_rate_limited" || payload.Error.Details.Reason != "provider" ||
		payload.Error.Details.RetryAfterSeconds != 2 {
		t.Fatalf("payload = %#v", payload)
	}
}
