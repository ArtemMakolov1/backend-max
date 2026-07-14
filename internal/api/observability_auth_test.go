package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

func TestObservabilityAuthRequiresAdminYandexSession(t *testing.T) {
	storage, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "observability-auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application := app.New(storage, mediaStore, nil, nil, nil, logger)
	server := New(application, logger, "http://localhost:4321", "webhook-secret", AuthOptions{
		YandexClient: &fakeYandexOAuth{}, ObservabilityAdmins: []string{"monitor-admin"},
	})
	handler := server.Handler()

	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/api/v1/observability/auth", nil))
	if unauthenticated.Code != http.StatusSeeOther || unauthenticated.Header().Get("Location") != "/app/" {
		t.Fatalf("unauthenticated status=%d location=%q", unauthenticated.Code, unauthenticated.Header().Get("Location"))
	}

	regular := httptest.NewRecorder()
	withTestSession(t, storage, handler, "regular-user").ServeHTTP(
		regular, httptest.NewRequest(http.MethodGet, "/api/v1/observability/auth", nil))
	if regular.Code != http.StatusForbidden || regular.Header().Get("X-WEBAUTH-USER") != "" {
		t.Fatalf("regular user status=%d auth-user=%q body=%s", regular.Code, regular.Header().Get("X-WEBAUTH-USER"), regular.Body.String())
	}

	admin := httptest.NewRecorder()
	adminHandler := withTestSession(t, storage, handler, "monitor-admin")
	adminHandler.ServeHTTP(
		admin, httptest.NewRequest(http.MethodGet, "/api/v1/observability/auth", nil))
	if admin.Code != http.StatusNoContent || admin.Header().Get("X-WEBAUTH-USER") != "monitor-admin" ||
		admin.Header().Get("X-WEBAUTH-ROLE") != "Viewer" {
		t.Fatalf("admin status=%d headers=%v body=%s", admin.Code, admin.Header(), admin.Body.String())
	}

	adminSession := httptest.NewRecorder()
	adminHandler.ServeHTTP(
		adminSession, httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil))
	var sessionPayload authStatusPayload
	if err := json.Unmarshal(adminSession.Body.Bytes(), &sessionPayload); err != nil {
		t.Fatal(err)
	}
	if !sessionPayload.ObservabilityAccess {
		t.Fatal("monitoring admin session must advertise observability access")
	}
	adminHealth := httptest.NewRecorder()
	adminHandler.ServeHTTP(adminHealth, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	var healthPayload struct {
		ObservabilityAccess bool `json:"observability_access"`
	}
	if err := json.Unmarshal(adminHealth.Body.Bytes(), &healthPayload); err != nil {
		t.Fatal(err)
	}
	if !healthPayload.ObservabilityAccess {
		t.Fatal("health payload must advertise observability access to the frontend")
	}

	snapshot, err := storage.ProductAnalyticsSnapshot(t.Context(), server.now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DailyActiveUsers != 1 {
		t.Fatalf("DAU=%d, want only the authorized monitoring user to be active", snapshot.DailyActiveUsers)
	}
}
