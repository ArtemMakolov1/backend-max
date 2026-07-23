package config

import (
	"encoding/base64"
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
	defaultHost                      = "127.0.0.1"
	defaultPort                      = "8080"
	defaultMediaDir                  = "./media"
	defaultMediaUserMaxFiles         = int64(500)
	defaultMediaUserMaxBytes         = int64(10 << 30)
	defaultMediaOrphanGrace          = 24 * time.Hour
	defaultMediaCleanupPeriod        = 15 * time.Minute
	defaultMediaCleanupBatch         = 50
	defaultPublicBaseURL             = "http://localhost:8080"
	defaultFrontendOrigin            = "http://localhost:4321"
	defaultMAXAPIBaseURL             = "https://platform-api2.max.ru"
	defaultOpenAIAPIBaseURL          = "https://api.openai.com"
	defaultDirectAPIBaseURL          = "https://api.direct.yandex.com/json/v501"
	defaultDirectSandboxAPIBaseURL   = "https://api-sandbox.direct.yandex.com/json/v5"
	directOAuthCallbackRedirectURI   = "https://maxposty.ru/api/v1/advertising/direct/oauth/callback"
	directOAuthVerificationCodeURI   = "https://oauth.yandex.ru/verification_code"
	defaultOpenAIImageModel          = "gpt-image-2"
	defaultOpenAIResearchModel       = "gpt-5.4-mini"
	defaultSchedulerInterval         = 15 * time.Second
	defaultAuthSessionTTL            = 12 * time.Hour
	defaultMaxOwnedTeamWorkspaces    = 5
	defaultAIGlobalConcurrent        = 4
	defaultAIUserConcurrent          = 1
	defaultAIImagePerMinute          = 2
	defaultAIImagePerDay             = 20
	defaultAIResearchPerMinute       = 2
	defaultAIResearchPerDay          = 20
	defaultAILeaseTTL                = 4 * time.Minute
	maxAIConfiguredConcurrent        = 100
	maxAIConfiguredPerMinute         = 10_000
	maxAIConfiguredPerDay            = 1_000_000
	maxAIConfiguredLeaseTTL          = 24 * time.Hour
	maxMediaUserFiles                = int64(100_000)
	maxMediaUserBytes                = int64(1 << 50)
	maxConfiguredOwnedTeamWorkspaces = 1_000
	aiHandlerTimeout                 = 3 * time.Minute
)

type Config struct {
	Host                      string
	Port                      string
	DatabaseURL               string
	MediaDir                  string
	MediaUserMaxFiles         int64
	MediaUserMaxBytes         int64
	MediaOrphanGrace          time.Duration
	MediaCleanupInterval      time.Duration
	MediaCleanupBatch         int
	PublicBaseURL             string
	FrontendOrigin            string
	S3Host                    string
	S3AccessKey               string
	S3SecretKey               string
	S3Bucket                  string
	S3Region                  string
	MAXAPIBaseURL             string
	MAXBotToken               string
	MAXWebhookSecret          string
	MAXCACertFile             string
	OAuthTrustXRealIP         bool
	OAuthRateLimitAtEdge      bool
	AuthBootstrapMode         bool
	YandexClientID            string
	YandexClientSecret        string
	YandexRedirectURI         string
	YandexAllowedUsers        []string
	DirectOAuthClientID       string
	DirectOAuthClientSecret   string
	DirectOAuthRedirectURI    string
	DirectTokenDataKey        []byte
	DirectAPIBaseURL          string
	DirectWritesEnabled       bool
	DirectAutoLaunchEnabled   bool
	DirectSandbox             bool
	ObservabilityAdmins       []string
	AuthSessionTTL            time.Duration
	MaxOwnedTeamWorkspaces    int
	OpenAIAPIKey              string
	OpenAIAPIBaseURL          string
	OpenAIImageModel          string
	OpenAIResearchModel       string
	AIGlobalConcurrent        int
	AIUserConcurrent          int
	AIImagePerMinute          int
	AIImagePerDay             int
	AIResearchPerMinute       int
	AIResearchPerDay          int
	AILeaseTTL                time.Duration
	BillingEnforcementEnabled bool
	BillingLiveEnabled        bool
	YooKassaReceiptsConfirmed bool
	YooKassaShopID            string
	YooKassaSecretKey         string
	YooKassaDataKey           []byte
	YooKassaReturnURL         string
	SchedulerInterval         time.Duration
	SMTPHost                  string
	SMTPPort                  int
	SMTPUsername              string
	SMTPPassword              string
	SMTPFromEmail             string
	SMTPFromName              string
}

func Load() (Config, error) {
	mediaMaxFiles, err := boundedPositiveInt64Env("MEDIA_USER_MAX_FILES", defaultMediaUserMaxFiles, maxMediaUserFiles)
	if err != nil {
		return Config{}, err
	}
	mediaMaxBytes, err := boundedPositiveInt64Env("MEDIA_USER_MAX_BYTES", defaultMediaUserMaxBytes, maxMediaUserBytes)
	if err != nil {
		return Config{}, err
	}
	mediaOrphanGraceText := env("MEDIA_ORPHAN_GRACE_PERIOD", defaultMediaOrphanGrace.String())
	mediaOrphanGrace, err := time.ParseDuration(mediaOrphanGraceText)
	if err != nil || mediaOrphanGrace < time.Hour || mediaOrphanGrace > 30*24*time.Hour {
		return Config{}, fmt.Errorf("MEDIA_ORPHAN_GRACE_PERIOD must be between 1h and 720h: %q", mediaOrphanGraceText)
	}
	mediaCleanupIntervalText := env("MEDIA_CLEANUP_INTERVAL", defaultMediaCleanupPeriod.String())
	mediaCleanupInterval, err := time.ParseDuration(mediaCleanupIntervalText)
	if err != nil || mediaCleanupInterval < time.Minute || mediaCleanupInterval > 24*time.Hour {
		return Config{}, fmt.Errorf("MEDIA_CLEANUP_INTERVAL must be between 1m and 24h: %q", mediaCleanupIntervalText)
	}
	mediaCleanupBatch, err := boundedPositiveIntEnv("MEDIA_CLEANUP_BATCH_SIZE", defaultMediaCleanupBatch, 1_000)
	if err != nil {
		return Config{}, err
	}
	maxOwnedTeamWorkspaces, err := boundedPositiveIntEnv(
		"WORKSPACE_MAX_OWNED_TEAM_WORKSPACES", defaultMaxOwnedTeamWorkspaces, maxConfiguredOwnedTeamWorkspaces,
	)
	if err != nil {
		return Config{}, err
	}
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
	directWritesText := env("DIRECT_WRITES_ENABLED", "false")
	directWritesEnabled, err := strconv.ParseBool(directWritesText)
	if err != nil {
		return Config{}, fmt.Errorf("DIRECT_WRITES_ENABLED must be true or false: %q", directWritesText)
	}
	directAutoLaunchText := env("DIRECT_AUTO_LAUNCH_ENABLED", "false")
	directAutoLaunchEnabled, err := strconv.ParseBool(directAutoLaunchText)
	if err != nil {
		return Config{}, fmt.Errorf("DIRECT_AUTO_LAUNCH_ENABLED must be true or false: %q", directAutoLaunchText)
	}
	directSandboxText := env("DIRECT_SANDBOX", "true")
	directSandbox, err := strconv.ParseBool(directSandboxText)
	if err != nil {
		return Config{}, fmt.Errorf("DIRECT_SANDBOX must be true or false: %q", directSandboxText)
	}
	directAPIBaseURLFallback := defaultDirectAPIBaseURL
	if directSandbox {
		directAPIBaseURLFallback = defaultDirectSandboxAPIBaseURL
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
	billingEnforcementText := env("BILLING_ENFORCEMENT_ENABLED", "false")
	billingEnforcementEnabled, err := strconv.ParseBool(billingEnforcementText)
	if err != nil {
		return Config{}, fmt.Errorf("BILLING_ENFORCEMENT_ENABLED must be true or false: %q", billingEnforcementText)
	}
	billingLiveText := env("BILLING_LIVE_ENABLED", "false")
	billingLiveEnabled, err := strconv.ParseBool(billingLiveText)
	if err != nil {
		return Config{}, fmt.Errorf("BILLING_LIVE_ENABLED must be true or false: %q", billingLiveText)
	}
	receiptsConfirmedText := env("YOOKASSA_RECEIPTS_CONFIRMED", "false")
	receiptsConfirmed, err := strconv.ParseBool(receiptsConfirmedText)
	if err != nil {
		return Config{}, fmt.Errorf("YOOKASSA_RECEIPTS_CONFIRMED must be true or false: %q", receiptsConfirmedText)
	}
	smtpPortText := env("SMTP_PORT", "587")
	smtpPort, err := strconv.Atoi(smtpPortText)
	if err != nil || smtpPort <= 0 || smtpPort > 65535 {
		return Config{}, fmt.Errorf("SMTP_PORT must be an integer between 1 and 65535: %q", smtpPortText)
	}

	cfg := Config{
		Host:                      env("HOST", defaultHost),
		Port:                      env("PORT", defaultPort),
		DatabaseURL:               strings.TrimSpace(os.Getenv("DATABASE_URL")),
		MediaDir:                  env("MEDIA_DIR", defaultMediaDir),
		MediaUserMaxFiles:         mediaMaxFiles,
		MediaUserMaxBytes:         mediaMaxBytes,
		MediaOrphanGrace:          mediaOrphanGrace,
		MediaCleanupInterval:      mediaCleanupInterval,
		MediaCleanupBatch:         mediaCleanupBatch,
		PublicBaseURL:             strings.TrimRight(env("PUBLIC_BASE_URL", defaultPublicBaseURL), "/"),
		FrontendOrigin:            strings.TrimRight(env("FRONTEND_ORIGIN", defaultFrontendOrigin), "/"),
		S3Host:                    strings.TrimSpace(os.Getenv("S3_HOST")),
		S3AccessKey:               strings.TrimSpace(os.Getenv("S3_ACCESS_KEY")),
		S3SecretKey:               strings.TrimSpace(os.Getenv("S3_SECRET_KEY")),
		S3Bucket:                  strings.TrimSpace(os.Getenv("S3_BUCKET")),
		S3Region:                  strings.TrimSpace(os.Getenv("S3_REGION")),
		MAXAPIBaseURL:             env("MAX_API_BASE_URL", defaultMAXAPIBaseURL),
		MAXBotToken:               strings.TrimSpace(os.Getenv("MAX_BOT_TOKEN")),
		MAXWebhookSecret:          strings.TrimSpace(os.Getenv("MAX_WEBHOOK_SECRET")),
		MAXCACertFile:             strings.TrimSpace(os.Getenv("MAX_CA_CERT_FILE")),
		OAuthTrustXRealIP:         trustXRealIP,
		OAuthRateLimitAtEdge:      rateLimitAtEdge,
		AuthBootstrapMode:         authBootstrapMode,
		YandexClientID:            strings.TrimSpace(os.Getenv("YANDEX_CLIENT_ID")),
		YandexClientSecret:        strings.TrimSpace(os.Getenv("YANDEX_CLIENT_SECRET")),
		YandexRedirectURI:         strings.TrimSpace(os.Getenv("YANDEX_REDIRECT_URI")),
		YandexAllowedUsers:        splitNormalizedCSV(os.Getenv("YANDEX_ALLOWED_USERS")),
		DirectOAuthClientID:       strings.TrimSpace(os.Getenv("DIRECT_OAUTH_CLIENT_ID")),
		DirectOAuthClientSecret:   strings.TrimSpace(os.Getenv("DIRECT_OAUTH_CLIENT_SECRET")),
		DirectOAuthRedirectURI:    strings.TrimSpace(os.Getenv("DIRECT_OAUTH_REDIRECT_URI")),
		DirectTokenDataKey:        nil,
		DirectAPIBaseURL:          env("DIRECT_API_BASE_URL", directAPIBaseURLFallback),
		DirectWritesEnabled:       directWritesEnabled,
		DirectAutoLaunchEnabled:   directAutoLaunchEnabled,
		DirectSandbox:             directSandbox,
		ObservabilityAdmins:       splitNormalizedCSV(os.Getenv("OBSERVABILITY_ADMIN_USERS")),
		AuthSessionTTL:            sessionTTL,
		MaxOwnedTeamWorkspaces:    maxOwnedTeamWorkspaces,
		OpenAIAPIKey:              strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		OpenAIAPIBaseURL:          env("OPENAI_API_BASE_URL", defaultOpenAIAPIBaseURL),
		OpenAIImageModel:          env("OPENAI_IMAGE_MODEL", defaultOpenAIImageModel),
		OpenAIResearchModel:       env("OPENAI_RESEARCH_MODEL", defaultOpenAIResearchModel),
		AIGlobalConcurrent:        aiGlobalConcurrent,
		AIUserConcurrent:          aiUserConcurrent,
		AIImagePerMinute:          aiImagePerMinute,
		AIImagePerDay:             aiImagePerDay,
		AIResearchPerMinute:       aiResearchPerMinute,
		AIResearchPerDay:          aiResearchPerDay,
		AILeaseTTL:                aiLeaseTTL,
		BillingEnforcementEnabled: billingEnforcementEnabled,
		BillingLiveEnabled:        billingLiveEnabled,
		YooKassaReceiptsConfirmed: receiptsConfirmed,
		YooKassaShopID:            strings.TrimSpace(os.Getenv("YOOKASSA_SHOP_ID")),
		YooKassaSecretKey:         os.Getenv("YOOKASSA_SECRET_KEY"),
		YooKassaDataKey:           nil,
		YooKassaReturnURL:         strings.TrimSpace(os.Getenv("YOOKASSA_RETURN_URL")),
		SchedulerInterval:         interval,
		SMTPHost:                  strings.TrimSpace(os.Getenv("SMTP_HOST")),
		SMTPPort:                  smtpPort,
		SMTPUsername:              strings.TrimSpace(os.Getenv("SMTP_USERNAME")),
		SMTPPassword:              os.Getenv("SMTP_PASSWORD"),
		SMTPFromEmail:             strings.TrimSpace(os.Getenv("SMTP_FROM_EMAIL")),
		SMTPFromName:              strings.TrimSpace(os.Getenv("SMTP_FROM_NAME")),
	}

	if cfg.Host == "" || cfg.Port == "" || cfg.DatabaseURL == "" || cfg.MediaDir == "" || cfg.PublicBaseURL == "" {
		return Config{}, fmt.Errorf("HOST, PORT, DATABASE_URL, MEDIA_DIR and PUBLIC_BASE_URL must not be empty")
	}
	yooValues := []string{cfg.YooKassaShopID, cfg.YooKassaSecretKey, strings.TrimSpace(os.Getenv("YOOKASSA_DATA_KEY"))}
	yooParts := 0
	for _, value := range yooValues {
		if value != "" {
			yooParts++
		}
	}
	if yooParts != 0 && yooParts != len(yooValues) {
		return Config{}, fmt.Errorf("YOOKASSA_SHOP_ID, YOOKASSA_SECRET_KEY and YOOKASSA_DATA_KEY must be configured together")
	}
	if yooParts == 0 && cfg.YooKassaReturnURL != "" {
		return Config{}, fmt.Errorf("YOOKASSA_RETURN_URL requires YooKassa credentials")
	}
	if yooParts != 0 {
		if !regexp.MustCompile(`^[0-9]{1,64}$`).MatchString(cfg.YooKassaShopID) {
			return Config{}, fmt.Errorf("YOOKASSA_SHOP_ID must contain 1 to 64 digits")
		}
		decodedKey, decodeErr := base64.StdEncoding.DecodeString(yooValues[2])
		if decodeErr != nil || len(decodedKey) != 32 {
			return Config{}, fmt.Errorf("YOOKASSA_DATA_KEY must be standard base64 encoding of exactly 32 random bytes")
		}
		cfg.YooKassaDataKey = decodedKey
		if cfg.YooKassaReturnURL == "" {
			cfg.YooKassaReturnURL = cfg.PublicBaseURL + "/app/?billing=pending#/workspace/settings/plan"
		}
		if err := validateYooKassaReturnURL(cfg.PublicBaseURL, cfg.YooKassaReturnURL); err != nil {
			return Config{}, err
		}
	}
	if cfg.BillingLiveEnabled {
		if !cfg.BillingEnforcementEnabled {
			return Config{}, fmt.Errorf("BILLING_LIVE_ENABLED requires BILLING_ENFORCEMENT_ENABLED=true")
		}
		if !cfg.YooKassaReceiptsConfirmed {
			return Config{}, fmt.Errorf("BILLING_LIVE_ENABLED requires YOOKASSA_RECEIPTS_CONFIRMED=true")
		}
		if !cfg.YooKassaConfigured() {
			return Config{}, fmt.Errorf("BILLING_LIVE_ENABLED requires complete YooKassa credentials")
		}
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
	directDataKey := strings.TrimSpace(os.Getenv("DIRECT_TOKEN_DATA_KEY"))
	directValues := []string{
		cfg.DirectOAuthClientID,
		cfg.DirectOAuthClientSecret,
		cfg.DirectOAuthRedirectURI,
		directDataKey,
	}
	directParts := 0
	for _, value := range directValues {
		if value != "" {
			directParts++
		}
	}
	s3Values := []string{cfg.S3Host, cfg.S3AccessKey, cfg.S3SecretKey}
	s3Parts := 0
	for _, value := range s3Values {
		if value != "" {
			s3Parts++
		}
	}
	if s3Parts != 0 && s3Parts != len(s3Values) {
		return Config{}, fmt.Errorf("S3_HOST, S3_ACCESS_KEY and S3_SECRET_KEY must be configured together")
	}
	if (cfg.S3Bucket != "" || cfg.S3Region != "") && s3Parts == 0 {
		return Config{}, fmt.Errorf("S3_BUCKET and S3_REGION require S3 credentials")
	}
	if cfg.AuthBootstrapMode {
		if oauthParts != 0 || directParts != 0 || cfg.DirectWritesEnabled || cfg.DirectAutoLaunchEnabled ||
			!cfg.DirectSandbox ||
			strings.TrimSuffix(cfg.DirectAPIBaseURL, "/") != defaultDirectSandboxAPIBaseURL ||
			len(cfg.YandexAllowedUsers) != 0 || len(cfg.ObservabilityAdmins) != 0 {
			return Config{}, fmt.Errorf("AUTH_BOOTSTRAP_MODE requires Yandex OAuth credentials and allowlist to be empty")
		}
		if cfg.MAXBotToken != "" || cfg.MAXWebhookSecret != "" || cfg.OpenAIAPIKey != "" || cfg.YooKassaConfigured() {
			return Config{}, fmt.Errorf("AUTH_BOOTSTRAP_MODE requires MAX and OpenAI integrations to be disabled")
		}
		if s3Parts != 0 || cfg.S3Bucket != "" || cfg.S3Region != "" {
			return Config{}, fmt.Errorf("AUTH_BOOTSTRAP_MODE requires S3 storage to be disabled")
		}
		if err := validateBootstrapOrigins(cfg.PublicBaseURL, frontendURL); err != nil {
			return Config{}, err
		}
		return cfg, nil
	}
	if oauthParts != 0 && oauthParts != len(oauthValues) {
		return Config{}, fmt.Errorf("YANDEX_CLIENT_ID, YANDEX_CLIENT_SECRET and YANDEX_REDIRECT_URI must be configured together")
	}
	if directParts != 0 && directParts != len(directValues) {
		return Config{}, fmt.Errorf("DIRECT_OAUTH_CLIENT_ID, DIRECT_OAUTH_CLIENT_SECRET, DIRECT_OAUTH_REDIRECT_URI and DIRECT_TOKEN_DATA_KEY must be configured together")
	}
	if err := validateDirectAPIBaseURL(cfg.DirectAPIBaseURL, cfg.DirectSandbox); err != nil {
		return Config{}, err
	}
	cfg.DirectAPIBaseURL = strings.TrimSuffix(cfg.DirectAPIBaseURL, "/")
	if directParts == len(directValues) {
		decodedKey, decodeErr := base64.StdEncoding.DecodeString(directDataKey)
		if decodeErr != nil || len(decodedKey) != 32 {
			return Config{}, fmt.Errorf("DIRECT_TOKEN_DATA_KEY must be standard base64 encoding of exactly 32 random bytes")
		}
		cfg.DirectTokenDataKey = decodedKey
		if err := validateDirectRedirectURI(cfg.DirectOAuthRedirectURI); err != nil {
			return Config{}, err
		}
	} else if cfg.DirectWritesEnabled || cfg.DirectAutoLaunchEnabled {
		return Config{}, fmt.Errorf("feature flags for Yandex Direct require complete Direct OAuth credentials")
	}
	if cfg.DirectAutoLaunchEnabled && !cfg.DirectWritesEnabled {
		return Config{}, fmt.Errorf("DIRECT_AUTO_LAUNCH_ENABLED requires DIRECT_WRITES_ENABLED=true")
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

func (c Config) DirectConfigured() bool {
	return c.DirectOAuthClientID != "" && c.DirectOAuthClientSecret != "" &&
		c.DirectOAuthRedirectURI != "" && len(c.DirectTokenDataKey) == 32
}

func (c Config) S3Enabled() bool {
	return c.S3Host != "" && c.S3AccessKey != "" && c.S3SecretKey != ""
}

func (c Config) YooKassaConfigured() bool {
	return c.YooKassaShopID != "" && c.YooKassaSecretKey != "" && len(c.YooKassaDataKey) == 32
}

func (c Config) YooKassaEnabled() bool {
	return c.BillingLiveEnabled && c.BillingEnforcementEnabled && c.YooKassaReceiptsConfirmed && c.YooKassaConfigured()
}

func validateYooKassaReturnURL(publicBaseURL, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Port() != "" && parsed.Port() != "443") {
		return fmt.Errorf("YOOKASSA_RETURN_URL must be an absolute HTTPS URL on the default port without credentials")
	}
	publicURL, err := url.Parse(publicBaseURL)
	if err != nil || !strings.EqualFold(parsed.Scheme, publicURL.Scheme) || !strings.EqualFold(parsed.Host, publicURL.Host) {
		return fmt.Errorf("YOOKASSA_RETURN_URL must use the PUBLIC_BASE_URL origin")
	}
	return nil
}

// SMTPConfigured reports whether transactional email delivery is enabled. Host
// and sender address are the minimum required to build an SMTP sender; when
// either is empty welcome emails are disabled and a NoopSender is wired instead.
func (c Config) SMTPConfigured() bool {
	return c.SMTPHost != "" && c.SMTPFromEmail != ""
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

func validateDirectRedirectURI(raw string) error {
	switch strings.TrimSpace(raw) {
	case directOAuthCallbackRedirectURI, directOAuthVerificationCodeURI:
		return nil
	default:
		return fmt.Errorf(
			"DIRECT_OAUTH_REDIRECT_URI must be exactly %s or %s",
			directOAuthCallbackRedirectURI, directOAuthVerificationCodeURI,
		)
	}
}

func validateDirectAPIBaseURL(raw string, sandbox bool) error {
	expected := defaultDirectAPIBaseURL
	if sandbox {
		expected = defaultDirectSandboxAPIBaseURL
	}
	normalized := strings.TrimSuffix(strings.TrimSpace(raw), "/")
	if normalized != expected {
		return fmt.Errorf("DIRECT_API_BASE_URL must be exactly %s when DIRECT_SANDBOX=%t (one trailing slash is allowed)", expected, sandbox)
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

func boundedPositiveInt64Env(name string, fallback, maximum int64) (int64, error) {
	raw := env(name, strconv.FormatInt(fallback, 10))
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 || value > maximum {
		return 0, fmt.Errorf("%s must be an integer between 1 and %d: %q", name, maximum, raw)
	}
	return value, nil
}
