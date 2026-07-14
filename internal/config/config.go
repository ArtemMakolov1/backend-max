package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHost                = "127.0.0.1"
	defaultPort                = "8080"
	defaultDatabasePath        = "./data/maxpilot.db"
	defaultMediaDir            = "./media"
	defaultPublicBaseURL       = "http://localhost:8080"
	defaultFrontendOrigin      = "http://localhost:4321"
	defaultMAXAPIBaseURL       = "https://platform-api2.max.ru"
	defaultOpenAIAPIBaseURL    = "https://api.openai.com"
	defaultOpenAIImageModel    = "gpt-image-2"
	defaultOpenAIResearchModel = "gpt-5.4-mini"
	defaultSchedulerInterval   = 15 * time.Second
	defaultAuthSessionTTL      = 12 * time.Hour
)

type Config struct {
	Host                 string
	Port                 string
	DatabasePath         string
	MediaDir             string
	PublicBaseURL        string
	FrontendOrigin       string
	MAXAPIBaseURL        string
	MAXBotToken          string
	MAXWebhookSecret     string
	MAXCACertFile        string
	AdminAPIKey          string
	AllowInsecureNoAuth  bool
	OAuthTrustXRealIP    bool
	OAuthRateLimitAtEdge bool
	YandexClientID       string
	YandexClientSecret   string
	YandexRedirectURI    string
	YandexAllowedUsers   []string
	AuthSessionTTL       time.Duration
	OpenAIAPIKey         string
	OpenAIAPIBaseURL     string
	OpenAIImageModel     string
	OpenAIResearchModel  string
	SchedulerInterval    time.Duration
}

func Load() (Config, error) {
	intervalText := env("SCHEDULER_INTERVAL", defaultSchedulerInterval.String())
	interval, err := time.ParseDuration(intervalText)
	if err != nil || interval <= 0 {
		return Config{}, fmt.Errorf("SCHEDULER_INTERVAL must be a positive duration: %q", intervalText)
	}
	sessionTTLText := env("AUTH_SESSION_TTL", defaultAuthSessionTTL.String())
	sessionTTL, err := time.ParseDuration(sessionTTLText)
	if err != nil || sessionTTL < 15*time.Minute || sessionTTL > 30*24*time.Hour {
		return Config{}, fmt.Errorf("AUTH_SESSION_TTL must be between 15m and 720h: %q", sessionTTLText)
	}
	allowInsecureText := env("ALLOW_INSECURE_NO_AUTH", "false")
	allowInsecureNoAuth, err := strconv.ParseBool(allowInsecureText)
	if err != nil {
		return Config{}, fmt.Errorf("ALLOW_INSECURE_NO_AUTH must be true or false: %q", allowInsecureText)
	}
	trustXRealIPText := env("OAUTH_TRUST_X_REAL_IP", "false")
	trustXRealIP, err := strconv.ParseBool(trustXRealIPText)
	if err != nil {
		return Config{}, fmt.Errorf("OAUTH_TRUST_X_REAL_IP must be true or false: %q", trustXRealIPText)
	}
	rateLimitAtEdgeText := env("OAUTH_RATE_LIMIT_AT_EDGE", "false")
	rateLimitAtEdge, err := strconv.ParseBool(rateLimitAtEdgeText)
	if err != nil {
		return Config{}, fmt.Errorf("OAUTH_RATE_LIMIT_AT_EDGE must be true or false: %q", rateLimitAtEdgeText)
	}

	cfg := Config{
		Host:                 env("HOST", defaultHost),
		Port:                 env("PORT", defaultPort),
		DatabasePath:         env("DATABASE_PATH", defaultDatabasePath),
		MediaDir:             env("MEDIA_DIR", defaultMediaDir),
		PublicBaseURL:        strings.TrimRight(env("PUBLIC_BASE_URL", defaultPublicBaseURL), "/"),
		FrontendOrigin:       strings.TrimRight(env("FRONTEND_ORIGIN", defaultFrontendOrigin), "/"),
		MAXAPIBaseURL:        strings.TrimRight(env("MAX_API_BASE_URL", defaultMAXAPIBaseURL), "/"),
		MAXBotToken:          strings.TrimSpace(os.Getenv("MAX_BOT_TOKEN")),
		MAXWebhookSecret:     strings.TrimSpace(os.Getenv("MAX_WEBHOOK_SECRET")),
		MAXCACertFile:        strings.TrimSpace(os.Getenv("MAX_CA_CERT_FILE")),
		AdminAPIKey:          strings.TrimSpace(os.Getenv("ADMIN_API_KEY")),
		AllowInsecureNoAuth:  allowInsecureNoAuth,
		OAuthTrustXRealIP:    trustXRealIP,
		OAuthRateLimitAtEdge: rateLimitAtEdge,
		YandexClientID:       strings.TrimSpace(os.Getenv("YANDEX_CLIENT_ID")),
		YandexClientSecret:   strings.TrimSpace(os.Getenv("YANDEX_CLIENT_SECRET")),
		YandexRedirectURI:    strings.TrimSpace(os.Getenv("YANDEX_REDIRECT_URI")),
		YandexAllowedUsers:   splitNormalizedCSV(os.Getenv("YANDEX_ALLOWED_USERS")),
		AuthSessionTTL:       sessionTTL,
		OpenAIAPIKey:         strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		OpenAIAPIBaseURL:     strings.TrimRight(env("OPENAI_API_BASE_URL", defaultOpenAIAPIBaseURL), "/"),
		OpenAIImageModel:     env("OPENAI_IMAGE_MODEL", defaultOpenAIImageModel),
		OpenAIResearchModel:  env("OPENAI_RESEARCH_MODEL", defaultOpenAIResearchModel),
		SchedulerInterval:    interval,
	}

	if cfg.Host == "" || cfg.Port == "" || cfg.DatabasePath == "" || cfg.MediaDir == "" || cfg.PublicBaseURL == "" {
		return Config{}, fmt.Errorf("HOST, PORT, DATABASE_PATH, MEDIA_DIR and PUBLIC_BASE_URL must not be empty")
	}
	frontendURL, err := validateFrontendOrigin(cfg.FrontendOrigin)
	if err != nil {
		return Config{}, err
	}
	if cfg.AdminAPIKey != "" && len(cfg.AdminAPIKey) < 24 {
		return Config{}, fmt.Errorf("ADMIN_API_KEY must contain at least 24 characters")
	}
	oauthValues := []string{cfg.YandexClientID, cfg.YandexClientSecret, cfg.YandexRedirectURI}
	oauthParts := 0
	for _, value := range oauthValues {
		if value != "" {
			oauthParts++
		}
	}
	if oauthParts != 0 && oauthParts != len(oauthValues) {
		return Config{}, fmt.Errorf("YANDEX_CLIENT_ID, YANDEX_CLIENT_SECRET and YANDEX_REDIRECT_URI must be configured together")
	}
	if cfg.YandexAuthEnabled() {
		if len(cfg.YandexAllowedUsers) == 0 {
			return Config{}, fmt.Errorf("YANDEX_ALLOWED_USERS must contain at least one Yandex ID, login or email")
		}
		if err := validateYandexRedirectURI(cfg.YandexRedirectURI); err != nil {
			return Config{}, err
		}
		if frontendURL.Scheme != "https" && !isLoopbackHost(frontendURL.Hostname()) {
			return Config{}, fmt.Errorf("FRONTEND_ORIGIN must use HTTPS when Yandex OAuth is enabled outside localhost")
		}
	} else if len(cfg.YandexAllowedUsers) != 0 {
		return Config{}, fmt.Errorf("YANDEX_ALLOWED_USERS requires Yandex OAuth credentials")
	}
	if cfg.AllowInsecureNoAuth && !isLoopbackHost(cfg.Host) {
		return Config{}, fmt.Errorf("ALLOW_INSECURE_NO_AUTH may only be enabled on a loopback HOST")
	}
	if cfg.AdminAPIKey == "" && !cfg.YandexAuthEnabled() && !cfg.AllowInsecureNoAuth {
		return Config{}, fmt.Errorf("ADMIN_API_KEY or Yandex OAuth is required; set ALLOW_INSECURE_NO_AUTH=true only for explicit loopback development")
	}
	return cfg, nil
}

func (c Config) YandexAuthEnabled() bool {
	return c.YandexClientID != "" && c.YandexClientSecret != "" && c.YandexRedirectURI != ""
}

func validateYandexRedirectURI(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
		return fmt.Errorf("YANDEX_REDIRECT_URI must be an absolute HTTP(S) URL without credentials, query or fragment")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return fmt.Errorf("YANDEX_REDIRECT_URI must use HTTPS outside localhost")
	}
	if parsed.Path != "/api/v1/auth/yandex/callback" {
		return fmt.Errorf("YANDEX_REDIRECT_URI path must be /api/v1/auth/yandex/callback")
	}
	return nil
}

func validateFrontendOrigin(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("FRONTEND_ORIGIN must be an exact HTTP(S) origin without path, query or fragment")
	}
	return parsed, nil
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func env(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

func splitNormalizedCSV(value string) []string {
	seen := make(map[string]struct{})
	items := make([]string, 0)
	for _, raw := range strings.Split(value, ",") {
		item := strings.ToLower(strings.TrimSpace(raw))
		if item == "" {
			continue
		}
		if _, exists := seen[item]; exists {
			continue
		}
		seen[item] = struct{}{}
		items = append(items, item)
	}
	return items
}
