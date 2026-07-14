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

func TestLoadRequiresYandexOAuthEvenWhenLegacyAdminVariablesAreSet(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOST", "0.0.0.0")
	t.Setenv("ADMIN_API_KEY", strings.Repeat("a", 32))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "yandex OAuth is required") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadAllowsExplicitFailClosedBootstrapWithoutOAuth(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOST", "0.0.0.0")
	t.Setenv("PUBLIC_BASE_URL", "http://178.159.94.83")
	t.Setenv("FRONTEND_ORIGIN", "http://178.159.94.83")
	t.Setenv("AUTH_BOOTSTRAP_MODE", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.YandexAuthEnabled() || !cfg.AuthBootstrapMode {
		t.Fatalf("bootstrap must stay fail-closed without an OAuth client: %#v", cfg)
	}
}

func TestLoadRejectsCredentialsAndHostnamesInBootstrapMode(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("PUBLIC_BASE_URL", "http://staging.example.ru")
	t.Setenv("FRONTEND_ORIGIN", "http://staging.example.ru")
	t.Setenv("AUTH_BOOTSTRAP_MODE", "true")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "plain-HTTP IP origin") {
		t.Fatalf("hostname bootstrap error = %v", err)
	}

	t.Setenv("PUBLIC_BASE_URL", "http://178.159.94.83")
	t.Setenv("FRONTEND_ORIGIN", "http://178.159.94.83")
	t.Setenv("YANDEX_CLIENT_ID", "must-not-be-present")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "requires Yandex OAuth credentials") {
		t.Fatalf("credentialed bootstrap error = %v", err)
	}
}

func TestLoadRejectsPartialYandexOAuth(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("YANDEX_CLIENT_ID", "client-id")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "configured together") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadAcceptsOAuthWithoutAllowlistForPublicSignup(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("YANDEX_CLIENT_ID", "client-id")
	t.Setenv("YANDEX_CLIENT_SECRET", "client-secret")
	t.Setenv("YANDEX_REDIRECT_URI", "http://localhost:8080/api/v1/auth/yandex/callback")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.YandexAllowedUsers) != 0 {
		t.Fatalf("allowed users = %#v", cfg.YandexAllowedUsers)
	}
}

func TestLoadUsesSafeAIQuotaDefaultsAndAcceptsBoundedOverrides(t *testing.T) {
	clearAuthEnv(t)
	setValidLocalYandexAuth(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AIGlobalConcurrent != 4 || cfg.AIUserConcurrent != 1 ||
		cfg.AIImagePerMinute != 2 || cfg.AIImagePerDay != 20 ||
		cfg.AIResearchPerMinute != 2 || cfg.AIResearchPerDay != 20 || cfg.AILeaseTTL != 4*time.Minute {
		t.Fatalf("unsafe or unexpected AI defaults: %#v", cfg)
	}

	t.Setenv("AI_GLOBAL_MAX_CONCURRENT", "8")
	t.Setenv("AI_USER_MAX_CONCURRENT", "2")
	t.Setenv("AI_IMAGE_PER_MINUTE", "3")
	t.Setenv("AI_IMAGE_PER_DAY", "30")
	t.Setenv("AI_RESEARCH_PER_MINUTE", "4")
	t.Setenv("AI_RESEARCH_PER_DAY", "40")
	t.Setenv("AI_LEASE_TTL", "5m")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AIGlobalConcurrent != 8 || cfg.AIUserConcurrent != 2 || cfg.AIImagePerMinute != 3 ||
		cfg.AIImagePerDay != 30 || cfg.AIResearchPerMinute != 4 || cfg.AIResearchPerDay != 40 || cfg.AILeaseTTL != 5*time.Minute {
		t.Fatalf("AI overrides were not loaded: %#v", cfg)
	}
}

func TestLoadRejectsUnsafeAIQuotaValues(t *testing.T) {
	tests := map[string]string{
		"AI_GLOBAL_MAX_CONCURRENT": "0",
		"AI_USER_MAX_CONCURRENT":   "101",
		"AI_IMAGE_PER_MINUTE":      "10001",
		"AI_IMAGE_PER_DAY":         "1000001",
		"AI_RESEARCH_PER_MINUTE":   "many",
		"AI_RESEARCH_PER_DAY":      "-1",
		"AI_LEASE_TTL":             "3m",
	}
	for name, value := range tests {
		name, value := name, value
		t.Run(name, func(t *testing.T) {
			clearAuthEnv(t)
			setValidLocalYandexAuth(t)
			t.Setenv(name, value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("Load() error = %v, want %s validation", err, name)
			}
		})
	}
}

func TestLoadPinsMAXAPIToOfficialHTTPSOrigin(t *testing.T) {
	for _, accepted := range []string{"https://platform-api2.max.ru", "https://platform-api2.max.ru/"} {
		clearAuthEnv(t)
		setValidLocalYandexAuth(t)
		t.Setenv("MAX_API_BASE_URL", accepted)
		if _, err := Load(); err != nil {
			t.Fatalf("official MAX API URL %q rejected: %v", accepted, err)
		}
	}
	for _, rejected := range []string{
		"http://platform-api2.max.ru", "https://api.example.com", "https://platform-api2.max.ru:443",
		"https://platform-api2.max.ru/v1", "https://user@platform-api2.max.ru", "https://platform-api2.max.ru?x=1",
		"https://platform-api2.max.ru//",
	} {
		clearAuthEnv(t)
		setValidLocalYandexAuth(t)
		t.Setenv("MAX_API_BASE_URL", rejected)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MAX_API_BASE_URL") {
			t.Fatalf("MAX API URL %q error = %v", rejected, err)
		}
	}
}

func TestLoadPinsOpenAIAPIToOfficialHTTPSOrigin(t *testing.T) {
	for _, accepted := range []string{"https://api.openai.com", "https://api.openai.com/"} {
		clearAuthEnv(t)
		setValidLocalYandexAuth(t)
		t.Setenv("OPENAI_API_BASE_URL", accepted)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("official OpenAI API URL %q rejected: %v", accepted, err)
		}
		if cfg.OpenAIAPIBaseURL != "https://api.openai.com" {
			t.Fatalf("normalized OpenAI API URL = %q", cfg.OpenAIAPIBaseURL)
		}
	}
	for _, rejected := range []string{
		"http://api.openai.com", "https://openai.example.com", "https://api.openai.com:443",
		"https://api.openai.com/v1", "https://user@api.openai.com", "https://api.openai.com?x=1",
		"https://api.openai.com//",
	} {
		clearAuthEnv(t)
		setValidLocalYandexAuth(t)
		t.Setenv("OPENAI_API_BASE_URL", rejected)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "OPENAI_API_BASE_URL") {
			t.Fatalf("OpenAI API URL %q error = %v", rejected, err)
		}
	}
}

func TestLoadRequiresValidWebhookSecretWithMAXToken(t *testing.T) {
	for _, secret := range []string{"", "abcd", "contains space", "bad!secret"} {
		clearAuthEnv(t)
		setValidLocalYandexAuth(t)
		t.Setenv("MAX_BOT_TOKEN", "server-only-token")
		t.Setenv("MAX_WEBHOOK_SECRET", secret)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MAX_WEBHOOK_SECRET") {
			t.Fatalf("MAX_WEBHOOK_SECRET %q error = %v", secret, err)
		}
	}
	clearAuthEnv(t)
	setValidLocalYandexAuth(t)
	t.Setenv("MAX_BOT_TOKEN", "server-only-token")
	t.Setenv("MAX_WEBHOOK_SECRET", "valid_webhook-secret_123")
	if _, err := Load(); err != nil {
		t.Fatalf("valid MAX token/secret pair rejected: %v", err)
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
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "yandex OAuth is required") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsUnauthenticatedLoopbackEvenWithLegacyOptIn(t *testing.T) {
	clearAuthEnv(t)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "yandex OAuth is required") {
		t.Fatalf("Load() error = %v", err)
	}
	t.Setenv("ALLOW_INSECURE_NO_AUTH", "true")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "yandex OAuth is required") {
		t.Fatalf("Load() error = %v", err)
	}
}

func clearAuthEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOST", "127.0.0.1")
	t.Setenv("FRONTEND_ORIGIN", "http://localhost:4321")
	t.Setenv("DATABASE_URL", "postgres://app:secret@localhost:6432/maxstudio?sslmode=disable")
	for _, name := range []string{
		"ADMIN_API_KEY", "YANDEX_CLIENT_ID", "YANDEX_CLIENT_SECRET", "YANDEX_REDIRECT_URI",
		"YANDEX_ALLOWED_USERS", "AUTH_SESSION_TTL", "ALLOW_INSECURE_NO_AUTH", "AUTH_BOOTSTRAP_MODE", "OAUTH_TRUST_X_REAL_IP",
		"OAUTH_RATE_LIMIT_AT_EDGE", "AI_GLOBAL_MAX_CONCURRENT", "AI_USER_MAX_CONCURRENT",
		"AI_IMAGE_PER_MINUTE", "AI_IMAGE_PER_DAY", "AI_RESEARCH_PER_MINUTE", "AI_RESEARCH_PER_DAY", "AI_LEASE_TTL",
		"MAX_API_BASE_URL", "MAX_BOT_TOKEN", "MAX_WEBHOOK_SECRET", "MAX_CA_CERT_FILE",
		"OPENAI_API_BASE_URL",
	} {
		t.Setenv(name, "")
	}
}

func setValidLocalYandexAuth(t *testing.T) {
	t.Helper()
	t.Setenv("YANDEX_CLIENT_ID", "client-id")
	t.Setenv("YANDEX_CLIENT_SECRET", "client-secret")
	t.Setenv("YANDEX_REDIRECT_URI", "http://localhost:8080/api/v1/auth/yandex/callback")
}
