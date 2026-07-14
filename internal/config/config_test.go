package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadAcceptsYandexOAuthAsPublicHostProtection(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOST", "0.0.0.0")
	t.Setenv("YANDEX_CLIENT_ID", "client-id")
	t.Setenv("YANDEX_CLIENT_SECRET", "client-secret")
	t.Setenv("YANDEX_REDIRECT_URI", "https://studio.example.ru/api/v1/auth/yandex/callback")
	t.Setenv("YANDEX_ALLOWED_USERS", " 12345, Editor@Example.ru,12345 ")
	t.Setenv("AUTH_SESSION_TTL", "8h")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.YandexAuthEnabled() || cfg.AuthSessionTTL != 8*time.Hour {
		t.Fatalf("unexpected auth config: %#v", cfg)
	}
	if got := strings.Join(cfg.YandexAllowedUsers, ","); got != "12345,editor@example.ru" {
		t.Fatalf("allowed users = %q", got)
	}
}

func TestLoadAcceptsAdminKeyWithoutPartialYandexConfig(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOST", "0.0.0.0")
	t.Setenv("ADMIN_API_KEY", strings.Repeat("a", 32))

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminAPIKey == "" || cfg.YandexAuthEnabled() || cfg.AllowInsecureNoAuth {
		t.Fatalf("unexpected admin-only config: %#v", cfg)
	}
}

func TestLoadRejectsPartialYandexOAuth(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("YANDEX_CLIENT_ID", "client-id")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "configured together") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsOAuthWithoutAllowlist(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("YANDEX_CLIENT_ID", "client-id")
	t.Setenv("YANDEX_CLIENT_SECRET", "client-secret")
	t.Setenv("YANDEX_REDIRECT_URI", "http://localhost:8080/api/v1/auth/yandex/callback")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "YANDEX_ALLOWED_USERS") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsInsecureExternalRedirect(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("YANDEX_CLIENT_ID", "client-id")
	t.Setenv("YANDEX_CLIENT_SECRET", "client-secret")
	t.Setenv("YANDEX_REDIRECT_URI", "http://studio.example.ru/api/v1/auth/yandex/callback")
	t.Setenv("YANDEX_ALLOWED_USERS", "12345")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsUnexpectedYandexCallbackPath(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("YANDEX_CLIENT_ID", "client-id")
	t.Setenv("YANDEX_CLIENT_SECRET", "client-secret")
	t.Setenv("YANDEX_REDIRECT_URI", "http://localhost:8080/oauth/callback")
	t.Setenv("YANDEX_ALLOWED_USERS", "12345")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "/api/v1/auth/yandex/callback") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadStillRejectsUnprotectedPublicHost(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOST", "0.0.0.0")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ADMIN_API_KEY or Yandex OAuth") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRequiresExplicitOptInForUnauthenticatedLoopback(t *testing.T) {
	clearAuthEnv(t)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ALLOW_INSECURE_NO_AUTH=true") {
		t.Fatalf("Load() error = %v", err)
	}

	t.Setenv("ALLOW_INSECURE_NO_AUTH", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AllowInsecureNoAuth {
		t.Fatal("AllowInsecureNoAuth = false, want true")
	}
}

func TestLoadRejectsInsecureOptInOnPublicHost(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOST", "0.0.0.0")
	t.Setenv("ALLOW_INSECURE_NO_AUTH", "true")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "loopback HOST") {
		t.Fatalf("Load() error = %v", err)
	}
}

func clearAuthEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOST", "127.0.0.1")
	t.Setenv("FRONTEND_ORIGIN", "http://localhost:4321")
	for _, name := range []string{
		"ADMIN_API_KEY", "YANDEX_CLIENT_ID", "YANDEX_CLIENT_SECRET", "YANDEX_REDIRECT_URI",
		"YANDEX_ALLOWED_USERS", "AUTH_SESSION_TTL", "ALLOW_INSECURE_NO_AUTH", "OAUTH_TRUST_X_REAL_IP",
		"OAUTH_RATE_LIMIT_AT_EDGE",
	} {
		t.Setenv(name, "")
	}
}
