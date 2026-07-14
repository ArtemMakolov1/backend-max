package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/observability"
	"maxpilot/backend/internal/store"
)

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
