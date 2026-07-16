package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/observability"
	"maxpilot/backend/internal/openaiimg"
	"maxpilot/backend/internal/store"
)

func TestWriteErrorTranslatesUnsupportedMAXChannelNotification(t *testing.T) {
	t.Parallel()

	server := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	response := httptest.NewRecorder()
	server.writeError(response, &maxclient.Error{
		StatusCode: http.StatusBadRequest,
		Message:    "errors.send-message.channel-notify",
	})

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
	}
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Error.Code != "max_channel_notify_unsupported" || strings.Contains(payload.Error.Message, "errors.send-message") {
		t.Fatalf("error payload = %#v", payload.Error)
	}
}

func TestWriteErrorDoesNotExposeUpstreamOrConflictDetails(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		err          error
		wantCode     string
		forbidden    []string
		wantCauseLog bool
	}{
		{
			name:      "MAX protocol error",
			err:       &maxclient.Error{StatusCode: http.StatusBadRequest, Code: "proto.payload", Message: "errors.send-message.internal-secret", RequestID: "upstream-secret"},
			wantCode:  "max_api_error",
			forbidden: []string{"errors.send-message", "upstream-secret", "upstream_status", "request_id"},
		},
		{
			name:      "OpenAI location error",
			err:       &openaiimg.Error{StatusCode: http.StatusForbidden, Code: "unsupported_country_region_territory", Message: "Country, region, or territory not supported", RequestID: "openai-secret"},
			wantCode:  "openai_api_error",
			forbidden: []string{"Country, region", "openai-secret", "upstream_status", "request_id"},
		},
		{
			name:      "stale editor revision",
			err:       fmt.Errorf("%w: post changed in another session; reload before saving", store.ErrConflict),
			wantCode:  "state_conflict",
			forbidden: []string{"another session"},
		},
		{
			name:         "untyped invalid upstream response",
			err:          errors.New("MAX message response contains an invalid chat ID"),
			wantCode:     "validation_error",
			forbidden:    []string{"invalid chat ID", "MAX message response"},
			wantCauseLog: true,
		},
		{
			name:         "untyped unsupported upstream response",
			err:          errors.New("unsupported MAX message envelope: proto.payload"),
			wantCode:     "validation_error",
			forbidden:    []string{"unsupported MAX", "proto.payload"},
			wantCauseLog: true,
		},
		{
			name:         "untyped internal required error",
			err:          errors.New("database connection string is required: secret-db.internal"),
			wantCode:     "validation_error",
			forbidden:    []string{"database connection", "secret-db.internal"},
			wantCauseLog: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			server := &Server{logger: slog.New(slog.NewTextHandler(&logs, nil))}
			response := httptest.NewRecorder()
			server.writeError(response, test.err)

			var payload struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if payload.Error.Code != test.wantCode {
				t.Fatalf("code = %q, want %q", payload.Error.Code, test.wantCode)
			}
			body := response.Body.String()
			for _, forbidden := range test.forbidden {
				if strings.Contains(body, forbidden) {
					t.Fatalf("technical detail %q leaked in %q", forbidden, body)
				}
			}
			if test.wantCauseLog && !strings.Contains(logs.String(), test.err.Error()) {
				t.Fatalf("server log omitted original cause %q: %s", test.err, logs.String())
			}
		})
	}
}

func TestWriteErrorTranslatesMediaQuotaFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		{err: store.ErrMediaQuotaExceeded, wantStatus: http.StatusRequestEntityTooLarge, wantCode: "media_quota_exceeded"},
		{err: store.ErrMediaUploadBusy, wantStatus: http.StatusConflict, wantCode: "media_upload_in_progress"},
	}
	for _, test := range tests {
		response := httptest.NewRecorder()
		server := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		server.writeError(response, test.err)
		if response.Code != test.wantStatus {
			t.Fatalf("status=%d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
		}
		var payload struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Error.Code != test.wantCode || payload.Error.Message == "" || strings.Contains(payload.Error.Message, "quota") {
			t.Fatalf("payload=%#v", payload.Error)
		}
	}
}

func TestMetricsEndpointAllowsOnlyDirectPrivateScrapers(t *testing.T) {
	t.Parallel()

	metrics := observability.New()
	handler := (&Server{logger: slog.Default(), frontendOrigin: "https://maxposty.ru", metrics: metrics}).Handler()

	missing := httptest.NewRequest(http.MethodGet, "/not-a-real-route/private-id", nil)
	missing.RemoteAddr = "172.20.0.4:54321"
	handler.ServeHTTP(httptest.NewRecorder(), missing)

	privateRequest := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	privateRequest.RemoteAddr = "172.20.0.5:53110"
	privateResponse := httptest.NewRecorder()
	handler.ServeHTTP(privateResponse, privateRequest)
	if privateResponse.Code != http.StatusOK {
		t.Fatalf("private metrics status = %d, body = %s", privateResponse.Code, privateResponse.Body.String())
	}
	if body := privateResponse.Body.String(); !strings.Contains(body, `maxposty_http_requests_total{method="GET",route="unmatched",status_class="4xx"} 1`) ||
		strings.Contains(body, "private-id") {
		t.Fatalf("unexpected metrics body: %s", body)
	}

	publicRequest := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	publicRequest.RemoteAddr = "203.0.113.10:42000"
	publicResponse := httptest.NewRecorder()
	handler.ServeHTTP(publicResponse, publicRequest)
	if publicResponse.Code != http.StatusNotFound {
		t.Fatalf("public metrics status = %d, want 404", publicResponse.Code)
	}

	forwardedRequest := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	forwardedRequest.RemoteAddr = "172.20.0.2:42000"
	forwardedRequest.Header.Set("X-Forwarded-For", "203.0.113.10")
	forwardedResponse := httptest.NewRecorder()
	handler.ServeHTTP(forwardedResponse, forwardedRequest)
	if forwardedResponse.Code != http.StatusNotFound {
		t.Fatalf("forwarded metrics status = %d, want 404", forwardedResponse.Code)
	}
}

func TestManagementAPIRequiresYandexSessionAndRejectsAdminKeyFallback(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := storage.Close(); closeErr != nil {
			t.Errorf("close store: %v", closeErr)
		}
	})
	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatalf("open media store: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application := app.New(storage, mediaStore, nil, nil, nil, logger)
	handler := New(application, logger, "http://localhost:4321", "webhook-secret").Handler()

	healthRequest := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	healthResponse := httptest.NewRecorder()
	handler.ServeHTTP(healthResponse, healthRequest)
	if healthResponse.Code != http.StatusOK {
		t.Fatalf("health status = %d", healthResponse.Code)
	}
	var health map[string]any
	if err := json.Unmarshal(healthResponse.Body.Bytes(), &health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if health["auth_required"] != true || health["authenticated"] != false {
		t.Fatalf("health auth state = %#v", health)
	}

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/posts", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want 401", unauthorized.Code)
	}

	legacyRequest := httptest.NewRequest(http.MethodGet, "/api/v1/posts", nil)
	legacyRequest.Header.Set("X-Admin-Key", "test-only-legacy-admin-key")
	legacy := httptest.NewRecorder()
	handler.ServeHTTP(legacy, legacyRequest)
	if legacy.Code != http.StatusUnauthorized {
		t.Fatalf("legacy admin key status = %d, want 401", legacy.Code)
	}
}

func TestCORSAllowsCredentialCookiesWithoutAdminHeader(t *testing.T) {
	t.Parallel()

	server := &Server{frontendOrigin: "http://localhost:4321"}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	request := httptest.NewRequest(http.MethodOptions, "/api/v1/posts", nil)
	request.Header.Set("Origin", "http://localhost:4321")
	response := httptest.NewRecorder()
	server.cors(next).ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d", response.Code)
	}
	if strings.Contains(response.Header().Get("Access-Control-Allow-Headers"), "X-Admin-Key") {
		t.Fatalf("legacy admin header is still allowed: %q", response.Header().Get("Access-Control-Allow-Headers"))
	}
	if got := response.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("allow credentials = %q, want true", got)
	}
}

func TestCORSRejectsForeignOriginAndSessionMutationWithoutOrigin(t *testing.T) {
	t.Parallel()

	server := &Server{frontendOrigin: "http://localhost:4321"}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	foreign := httptest.NewRequest(http.MethodPost, "/api/v1/posts", nil)
	foreign.Header.Set("Origin", "https://evil.example")
	foreignResponse := httptest.NewRecorder()
	server.cors(next).ServeHTTP(foreignResponse, foreign)
	if foreignResponse.Code != http.StatusForbidden {
		t.Fatalf("foreign origin status = %d, want 403", foreignResponse.Code)
	}

	missingOrigin := httptest.NewRequest(http.MethodPost, "/api/v1/posts", nil)
	missingOrigin.AddCookie(&http.Cookie{
		Name: sessionCookieName, Value: "opaque-session",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	missingOriginResponse := httptest.NewRecorder()
	server.cors(next).ServeHTTP(missingOriginResponse, missingOrigin)
	if missingOriginResponse.Code != http.StatusForbidden {
		t.Fatalf("session mutation without Origin status = %d, want 403", missingOriginResponse.Code)
	}
}

func TestMAXChatIDValidationUsesSignedInt64Range(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"0", "9223372036854775807", "-9223372036854775808"} {
		if !validMAXChatID(value) {
			t.Errorf("validMAXChatID(%q) = false, want true", value)
		}
	}
	for _, value := range []string{"", "+1", "9223372036854775808", "-9223372036854775809", "1.0"} {
		if validMAXChatID(value) {
			t.Errorf("validMAXChatID(%q) = true, want false", value)
		}
	}
}
