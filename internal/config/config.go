package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	maxWebhookSecretPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{5,256}$`)
	observabilityAdminPattern = regexp.MustCompile(`^[a-z0-9._+-]{1,128}$`)
)

const (
	defaultHost                = "127.0.0.1"
	defaultPort                = "8080"
	defaultMediaDir            = "./media"
	defaultPublicBaseURL       = "http://localhost:8080"
	defaultFrontendOrigin      = "http://localhost:4321"
	defaultMAXAPIBaseURL       = "https://platform-api2.max.ru"
	defaultOpenAIAPIBaseURL    = "https://api.openai.com"
	defaultOpenAIImageModel    = "gpt-image-2"
	defaultOpenAIResearchModel = "gpt-5.4-mini"
	defaultSchedulerInterval   = 15 * time.Second
	defaultAuthSessionTTL      = 12 * time.Hour
	defaultAIGlobalConcurrent  = 4
	defaultAIUserConcurrent    = 1
	defaultAIImagePerMinute    = 2
	defaultAIImagePerDay       = 20
	defaultAIResearchPerMinute = 2
	defaultAIResearchPerDay    = 20
	defaultAILeaseTTL          = 4 * time.Minute
	maxAIConfiguredConcurrent  = 100
	maxAIConfiguredPerMinute   = 10_000
	maxAIConfiguredPerDay      = 1_000_000
	maxAIConfiguredLeaseTTL    = 24 * time.Hour
	aiHandlerTimeout           = 3 * time.Minute
)

type Config struct {
	Host                 string
	Port                 string
	DatabaseURL          string
	MediaDir             string
	PublicBaseURL        string
	FrontendOrigin       string
	MAXAPIBaseURL        string
	MAXBotToken          string
	MAXWebhookSecret     string
	MAXCACertFile        string
	OAuthTrustXRealIP    bool
	OAuthRateLimitAtEdge bool
	AuthBootstrapMode    bool
	YandexClientID       string
	YandexClientSecret   string
	YandexRedirectURI    string
	YandexAllowedUsers   []string
	ObservabilityAdmins  []string
	AuthSessionTTL       time.Duration
	OpenAIAPIKey         string
	OpenAIAPIBaseURL     string
	OpenAIImageModel     string
	OpenAIResearchModel  string
	AIGlobalConcurrent   int
	AIUserConcurrent     int
	AIImagePerMinute     int
	AIImagePerDay        int
	AIResearchPerMinute  int
	AIResearchPerDay     int
	AILeaseTTL           time.Duration
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
	authBootstrapText := env("AUTH_BOOTSTRAP_MODE", "false")
	authBootstrapMode, err := strconv.ParseBool(authBootstrapText)
	if err != nil {
		return Config{}, fmt.Errorf("AUTH_BOOTSTRAP_MODE must be true or false: %q", authBootstrapText)
	}
	aiGlobalConcurrent, err := boundedPositiveIntEnv("AI_GLOBAL_MAX_CONCURRENT", defaultAIGlobalConcurrent, maxAIConfiguredConcurrent)
	if err != nil {
		return Config{}, err
	}
	aiUserConcurrent, err := boundedPositiveIntEnv("AI_USER_MAX_CONCURRENT", defaultAIUserConcurrent, maxAIConfiguredConcurrent)
	if err != nil {
		return Config{}, err
	}
	aiImagePerMinute, err := boundedPositiveIntEnv("AI_IMAGE_PER_MINUTE", defaultAIImagePerMinute, maxAIConfiguredPerMinute)
	if err != nil {
		return Config{}, err
	}
	aiImagePerDay, err := boundedPositiveIntEnv("AI_IMAGE_PER_DAY", defaultAIImagePerDay, maxAIConfiguredPerDay)
	if err != nil {
		return Config{}, err
	}
	aiResearchPerMinute, err := boundedPositiveIntEnv("AI_RESEARCH_PER_MINUTE", defaultAIResearchPerMinute, maxAIConfiguredPerMinute)
	if err != nil {
		return Config{}, err
	}
	aiResearchPerDay, err := boundedPositiveIntEnv("AI_RESEARCH_PER_DAY", defaultAIResearchPerDay, maxAIConfiguredPerDay)
	if err != nil {
		return Config{}, err
	}
	aiLeaseTTLText := env("AI_LEASE_TTL", defaultAILeaseTTL.String())
	aiLeaseTTL, err := time.ParseDuration(aiLeaseTTLText)
	if err != nil || aiLeaseTTL <= aiHandlerTimeout || aiLeaseTTL > maxAIConfiguredLeaseTTL {
		return Config{}, fmt.Errorf("AI_LEASE_TTL must be greater than 3m and at most 24h: %q", aiLeaseTTLText)
	}

	cfg := Config{
		Host:                 env("HOST", defaultHost),
		Port:                 env("PORT", defaultPort),
		DatabaseURL:          strings.TrimSpace(os.Getenv("DATABASE_URL")),
		MediaDir:             env("MEDIA_DIR", defaultMediaDir),
		PublicBaseURL:        strings.TrimRight(env("PUBLIC_BASE_URL", defaultPublicBaseURL), "/"),
		FrontendOrigin:       strings.TrimRight(env("FRONTEND_ORIGIN", defaultFrontendOrigin), "/"),
		MAXAPIBaseURL:        env("MAX_API_BASE_URL", defaultMAXAPIBaseURL),
		MAXBotToken:          strings.TrimSpace(os.Getenv("MAX_BOT_TOKEN")),
		MAXWebhookSecret:     strings.TrimSpace(os.Getenv("MAX_WEBHOOK_SECRET")),
		MAXCACertFile:        strings.TrimSpace(os.Getenv("MAX_CA_CERT_FILE")),
		OAuthTrustXRealIP:    trustXRealIP,
		OAuthRateLimitAtEdge: rateLimitAtEdge,
		AuthBootstrapMode:    authBootstrapMode,
		YandexClientID:       strings.TrimSpace(os.Getenv("YANDEX_CLIENT_ID")),
		YandexClientSecret:   strings.TrimSpace(os.Getenv("YANDEX_CLIENT_SECRET")),
		YandexRedirectURI:    strings.TrimSpace(os.Getenv("YANDEX_REDIRECT_URI")),
		YandexAllowedUsers:   splitNormalizedCSV(os.Getenv("YANDEX_ALLOWED_USERS")),
		ObservabilityAdmins:  splitNormalizedCSV(os.Getenv("OBSERVABILITY_ADMIN_USERS")),
		AuthSessionTTL:       sessionTTL,
		OpenAIAPIKey:         strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		OpenAIAPIBaseURL:     env("OPENAI_API_BASE_URL", defaultOpenAIAPIBaseURL),
		OpenAIImageModel:     env("OPENAI_IMAGE_MODEL", defaultOpenAIImageModel),
		OpenAIResearchModel:  env("OPENAI_RESEARCH_MODEL", defaultOpenAIResearchModel),
		AIGlobalConcurrent:   aiGlobalConcurrent,
		AIUserConcurrent:     aiUserConcurrent,
		AIImagePerMinute:     aiImagePerMinute,
		AIImagePerDay:        aiImagePerDay,
		AIResearchPerMinute:  aiResearchPerMinute,
		AIResearchPerDay:     aiResearchPerDay,
		AILeaseTTL:           aiLeaseTTL,
		SchedulerInterval:    interval,
	}

	if cfg.Host == "" || cfg.Port == "" || cfg.DatabaseURL == "" || cfg.MediaDir == "" || cfg.PublicBaseURL == "" {
		return Config{}, fmt.Errorf("HOST, PORT, DATABASE_URL, MEDIA_DIR and PUBLIC_BASE_URL must not be empty")
	}
	if err := validateMAXAPIBaseURL(cfg.MAXAPIBaseURL); err != nil {
		return Config{}, err
	}
	cfg.MAXAPIBaseURL = strings.TrimSuffix(cfg.MAXAPIBaseURL, "/")
	if err := validateOpenAIAPIBaseURL(cfg.OpenAIAPIBaseURL); err != nil {
		return Config{}, err
	}
	cfg.OpenAIAPIBaseURL = strings.TrimSuffix(cfg.OpenAIAPIBaseURL, "/")
	if cfg.MAXBotToken != "" && !maxWebhookSecretPattern.MatchString(cfg.MAXWebhookSecret) {
		return Config{}, fmt.Errorf("MAX_WEBHOOK_SECRET is required with MAX_BOT_TOKEN and must contain 5-256 letters, digits, underscores or hyphens")
	}
	frontendURL, err := validateFrontendOrigin(cfg.FrontendOrigin)
	if err != nil {
		return Config{}, err
	}
	oauthValues := []string{cfg.YandexClientID, cfg.YandexClientSecret, cfg.YandexRedirectURI}
	oauthParts := 0
	for _, value := range oauthValues {
		if value != "" {
			oauthParts++
		}
	}
	if cfg.AuthBootstrapMode {
		if oauthParts != 0 || len(cfg.YandexAllowedUsers) != 0 || len(cfg.ObservabilityAdmins) != 0 {
			return Config{}, fmt.Errorf("AUTH_BOOTSTRAP_MODE requires Yandex OAuth credentials and allowlist to be empty")
		}
		if cfg.MAXBotToken != "" || cfg.MAXWebhookSecret != "" || cfg.OpenAIAPIKey != "" {
			return Config{}, fmt.Errorf("AUTH_BOOTSTRAP_MODE requires MAX and OpenAI integrations to be disabled")
		}
		if err := validateBootstrapOrigins(cfg.PublicBaseURL, frontendURL); err != nil {
			return Config{}, err
		}
		return cfg, nil
	}
	if oauthParts != 0 && oauthParts != len(oauthValues) {
		return Config{}, fmt.Errorf("YANDEX_CLIENT_ID, YANDEX_CLIENT_SECRET and YANDEX_REDIRECT_URI must be configured together")
	}
	if cfg.YandexAuthEnabled() {
		if err := validateYandexRedirectURI(cfg.YandexRedirectURI); err != nil {
			return Config{}, err
		}
		if frontendURL.Scheme != "https" && !isLoopbackHost(frontendURL.Hostname()) {
			return Config{}, fmt.Errorf("FRONTEND_ORIGIN must use HTTPS when Yandex OAuth is enabled outside localhost")
		}
	} else if len(cfg.YandexAllowedUsers) != 0 {
		return Config{}, fmt.Errorf("YANDEX_ALLOWED_USERS requires Yandex OAuth credentials")
	}
	for _, identity := range cfg.ObservabilityAdmins {
		if !observabilityAdminPattern.MatchString(identity) {
			return Config{}, fmt.Errorf("OBSERVABILITY_ADMIN_USERS contains an invalid Yandex identity %q", identity)
		}
	}
	if !cfg.YandexAuthEnabled() {
		return Config{}, fmt.Errorf("yandex OAuth is required: configure YANDEX_CLIENT_ID, YANDEX_CLIENT_SECRET and YANDEX_REDIRECT_URI")
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
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || !isLoopbackHost(parsed.Hostname())) {
		return fmt.Errorf("YANDEX_REDIRECT_URI must use HTTPS outside localhost")
	}
	if parsed.Path != "/api/v1/auth/yandex/callback" {
		return fmt.Errorf("YANDEX_REDIRECT_URI path must be /api/v1/auth/yandex/callback")
	}
	return nil
}

func validateMAXAPIBaseURL(raw string) error {
	const official = "https://platform-api2.max.ru"
	normalized := strings.TrimSuffix(strings.TrimSpace(raw), "/")
	if normalized != official {
		return fmt.Errorf("MAX_API_BASE_URL must be exactly %s (one trailing slash is allowed)", official)
	}
	return nil
}

func validateOpenAIAPIBaseURL(raw string) error {
	const official = "https://api.openai.com"
	normalized := strings.TrimSuffix(strings.TrimSpace(raw), "/")
	if normalized != official {
		return fmt.Errorf("OPENAI_API_BASE_URL must be exactly %s (one trailing slash is allowed)", official)
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

func validateBootstrapOrigins(publicBaseURL string, frontendURL *url.URL) error {
	publicURL, err := url.Parse(publicBaseURL)
	if err != nil || publicURL.Scheme == "" || publicURL.Host == "" || publicURL.User != nil ||
		(publicURL.Path != "" && publicURL.Path != "/") || publicURL.RawQuery != "" || publicURL.Fragment != "" {
		return fmt.Errorf("PUBLIC_BASE_URL must be an exact HTTP origin in AUTH_BOOTSTRAP_MODE")
	}
	if frontendURL == nil || publicURL.Scheme != "http" || frontendURL.Scheme != "http" ||
		publicURL.Host != frontendURL.Host || net.ParseIP(publicURL.Hostname()) == nil {
		return fmt.Errorf("AUTH_BOOTSTRAP_MODE requires PUBLIC_BASE_URL and FRONTEND_ORIGIN to be the same plain-HTTP IP origin")
	}
	return nil
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

func boundedPositiveIntEnv(name string, fallback, maximum int) (int, error) {
	raw := env(name, strconv.Itoa(fallback))
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 || value > maximum {
		return 0, fmt.Errorf("%s must be an integer between 1 and %d: %q", name, maximum, raw)
	}
	return value, nil
}
