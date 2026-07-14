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
	"maxpilot/backend/internal/store"
)

func TestAdminKeyProtectsManagementAPI(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer storage.Close()
	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatalf("open media store: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application := app.New(storage, mediaStore, nil, nil, nil, logger)
	handler := New(application, logger, "http://localhost:4321", "webhook-secret", "0123456789abcdefghijklmn").Handler()

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

	authorizedRequest := httptest.NewRequest(http.MethodGet, "/api/v1/posts", nil)
	authorizedRequest.Header.Set("X-Admin-Key", "0123456789abcdefghijklmn")
	authorized := httptest.NewRecorder()
	handler.ServeHTTP(authorized, authorizedRequest)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, body = %s", authorized.Code, authorized.Body.String())
	}
}

func TestCORSAllowsAdminHeaderAndCredentialCookies(t *testing.T) {
	t.Parallel()

	server := &Server{frontendOrigin: "http://localhost:4321"}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	request := httptest.NewRequest(http.MethodOptions, "/api/v1/posts", nil)
	request.Header.Set("Origin", "http://localhost:4321")
	request.Header.Set("Access-Control-Request-Headers", "x-admin-key")
	response := httptest.NewRecorder()
	server.cors(next).ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d", response.Code)
	}
	if !strings.Contains(response.Header().Get("Access-Control-Allow-Headers"), "X-Admin-Key") {
		t.Fatalf("allow headers = %q", response.Header().Get("Access-Control-Allow-Headers"))
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
	missingOrigin.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "opaque-session"})
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
