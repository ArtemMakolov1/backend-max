package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"maxpilot/backend/internal/api"
	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/config"
	"maxpilot/backend/internal/email"
	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/observability"
	"maxpilot/backend/internal/openaiimg"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexauth"
	"maxpilot/backend/internal/yandexdirect"
	"maxpilot/backend/internal/yookassa"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	metrics := observability.New()
	metrics.SetSchedulerInterval(cfg.SchedulerInterval)
	storage, err := store.OpenRuntimeWithTracer(rootCtx, cfg.DatabaseURL, metrics)
	if err != nil {
		logger.Error("could not open database", "error", err)
		os.Exit(1)
	}
	defer func() {
		if closeErr := storage.Close(); closeErr != nil {
			logger.Error("could not close database", "error", closeErr)
		}
	}()
	if err := metrics.RegisterDBPoolStats(storage.DBStats); err != nil {
		logger.Error("could not register database pool metrics", "error", err)
		os.Exit(1)
	}
	if err := metrics.RegisterProductAnalytics(storage.ProductAnalyticsSnapshot); err != nil {
		logger.Error("could not register product analytics metrics", "error", err)
		os.Exit(1)
	}

	var mediaStore *media.Store
	mediaDriver := "local"
	if cfg.S3Enabled() {
		mediaDriver = "s3"
		mediaStore, err = media.NewS3(rootCtx, media.S3Config{
			Endpoint: cfg.S3Host, Region: cfg.S3Region,
			KeyID: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey, Bucket: cfg.S3Bucket,
		}, cfg.PublicBaseURL)
	} else {
		mediaStore, err = media.New(cfg.MediaDir, cfg.PublicBaseURL)
	}
	if err != nil {
		logger.Error("could not initialize media store", "error", err)
		os.Exit(1)
	}
	logger.Info("media store initialized", "driver", mediaDriver)

	var maxAPI app.MAXClient
	if cfg.MAXBotToken != "" {
		maxHTTPClient, err := newMAXHTTPClient(cfg.MAXCACertFile)
		if err != nil {
			logger.Error("could not configure MAX TLS", "error", err)
			os.Exit(1)
		}
		client, err := maxclient.New(cfg.MAXAPIBaseURL, cfg.MAXBotToken, maxHTTPClient)
		if err != nil {
			logger.Error("could not initialize MAX client", "error", err)
			os.Exit(1)
		}
		maxAPI = client
	}

	var openAI app.ImageClient
	var research app.ResearchClient
	if cfg.OpenAIAPIKey != "" {
		client, err := openaiimg.New(cfg.OpenAIAPIBaseURL, cfg.OpenAIAPIKey, cfg.OpenAIImageModel, &http.Client{Timeout: 3 * time.Minute})
		if err != nil {
			logger.Error("could not initialize OpenAI client", "error", err)
			os.Exit(1)
		}
		openAI = client
		researchClient, err := openairesearch.New(cfg.OpenAIAPIBaseURL, cfg.OpenAIAPIKey, cfg.OpenAIResearchModel, &http.Client{Timeout: 90 * time.Second})
		if err != nil {
			logger.Error("could not initialize OpenAI research client", "error", err)
			os.Exit(1)
		}
		research = researchClient
	}

	application := app.NewWithMetrics(storage, mediaStore, maxAPI, openAI, research, logger, metrics)
	if cfg.DirectConfigured() {
		directClient, err := yandexdirect.New(
			cfg.DirectAPIBaseURL,
			cfg.DirectOAuthClientID,
			cfg.DirectOAuthClientSecret,
			cfg.DirectOAuthRedirectURI,
			&http.Client{Timeout: 20 * time.Second},
		)
		if err != nil {
			logger.Error("could not initialize Yandex Direct client", "error", err)
			os.Exit(1)
		}
		if err := application.ConfigureDirect(directClient, cfg.DirectTokenDataKey); err != nil {
			logger.Error("could not configure Yandex Direct integration", "error", err)
			os.Exit(1)
		}
		if err := application.SetDirectFeatureFlags(
			cfg.DirectWritesEnabled, cfg.DirectAutoLaunchEnabled,
		); err != nil {
			logger.Error("could not apply Yandex Direct feature flags", "error", err)
			os.Exit(1)
		}
		logger.Info("Yandex Direct integration configured",
			"sandbox", cfg.DirectSandbox,
			"writes_enabled", cfg.DirectWritesEnabled,
			"auto_launch_enabled", cfg.DirectAutoLaunchEnabled)
	}
	if cfg.YooKassaConfigured() {
		yooClient, err := yookassa.New(cfg.YooKassaShopID, cfg.YooKassaSecretKey, &http.Client{Timeout: 15 * time.Second})
		if err != nil {
			logger.Error("could not initialize YooKassa client", "error", err)
			os.Exit(1)
		}
		if err := application.ConfigureBilling(yooClient, cfg.YooKassaReturnURL, cfg.YooKassaDataKey); err != nil {
			logger.Error("could not configure billing", "error", err)
			os.Exit(1)
		}
		if err := application.SetBillingLiveEnabled(cfg.YooKassaEnabled()); err != nil {
			logger.Error("could not apply billing live gate", "error", err)
			os.Exit(1)
		}
	}
	if err := application.ConfigureMediaPolicy(app.MediaPolicy{
		MaxFiles: cfg.MediaUserMaxFiles, MaxBytes: cfg.MediaUserMaxBytes,
		OrphanGrace: cfg.MediaOrphanGrace, CleanupInterval: cfg.MediaCleanupInterval,
		CleanupBatch: cfg.MediaCleanupBatch,
	}); err != nil {
		logger.Error("could not configure media policy", "error", err)
		os.Exit(1)
	}
	var welcomeSender email.Sender = email.NewNoopSender(logger)
	if cfg.SMTPConfigured() {
		sender, err := email.NewSMTPSender(email.Config{
			Host: cfg.SMTPHost, Port: cfg.SMTPPort, Username: cfg.SMTPUsername, Password: cfg.SMTPPassword,
			FromEmail: cfg.SMTPFromEmail, FromName: cfg.SMTPFromName,
			AppURL: cfg.PublicBaseURL + "/app/#/posts", SiteURL: cfg.FrontendOrigin,
		})
		if err != nil {
			logger.Error("could not initialize welcome email sender", "error", err)
			os.Exit(1)
		}
		welcomeSender = sender
		logger.Info("welcome emails enabled", "smtp_host", cfg.SMTPHost, "smtp_port", cfg.SMTPPort)
	} else {
		logger.Info("welcome emails disabled: SMTP not configured")
	}

	var yandexOAuth api.YandexOAuthClient
	if cfg.YandexAuthEnabled() {
		client, err := yandexauth.New(cfg.YandexClientID, cfg.YandexClientSecret, nil)
		if err != nil {
			logger.Error("could not initialize Yandex OAuth", "error", err)
			os.Exit(1)
		}
		yandexOAuth = client
	}
	apiServer := api.New(application, logger, cfg.FrontendOrigin, cfg.MAXWebhookSecret, api.AuthOptions{
		YandexClient: yandexOAuth, RedirectURI: cfg.YandexRedirectURI,
		AllowedUsers: cfg.YandexAllowedUsers, SessionTTL: cfg.AuthSessionTTL,
		ObservabilityAdmins: cfg.ObservabilityAdmins,
		SecureCookies:       strings.HasPrefix(strings.ToLower(cfg.YandexRedirectURI), "https://"),
		TrustXRealIP:        cfg.OAuthTrustXRealIP, RateLimitAtEdge: cfg.OAuthRateLimitAtEdge,
		MaxOwnedTeamWorkspaces: cfg.MaxOwnedTeamWorkspaces,
		WelcomeSender:          welcomeSender,
		AILimits: &api.AILimitOptions{
			GlobalMaxConcurrent:    cfg.AIGlobalConcurrent,
			UserMaxConcurrent:      cfg.AIUserConcurrent,
			ImagePerMinute:         cfg.AIImagePerMinute,
			ImagePerDay:            cfg.AIImagePerDay,
			ResearchPerMinute:      cfg.AIResearchPerMinute,
			ResearchPerDay:         cfg.AIResearchPerDay,
			LeaseTTL:               cfg.AILeaseTTL,
			MonthlyPlanEnforcement: cfg.BillingEnforcementEnabled,
		},
		Metrics: metrics,
	})
	httpServer := &http.Server{
		Addr:              net.JoinHostPort(cfg.Host, cfg.Port),
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		WriteTimeout:      4 * time.Minute,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	schedulerDone := make(chan struct{})
	go func() {
		defer close(schedulerDone)
		application.RunScheduler(rootCtx, cfg.SchedulerInterval)
	}()
	directAutomationDone := make(chan struct{})
	go func() {
		defer close(directAutomationDone)
		application.RunDirectAutomation(rootCtx, cfg.SchedulerInterval)
	}()

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("server started", "address", httpServer.Addr, "frontend_origin", cfg.FrontendOrigin,
			"yandex_auth", cfg.YandexAuthEnabled())
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case <-rootCtx.Done():
		logger.Info("shutdown requested")
	case err := <-serverErr:
		if err != nil {
			logger.Error("HTTP server failed", "error", err)
		}
	}
	// Restore default signal handling so a second SIGINT/SIGTERM terminates
	// the process immediately instead of waiting for graceful shutdown.
	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
	// An in-flight scheduled publication keeps running on a context detached
	// from the stop signal, so a routine deploy does not turn a half-sent post
	// into a terminal failure or a duplicate. Wait for the scheduler goroutine
	// to drain; the bound follows the 3-minute per-publication budget.
	select {
	case <-schedulerDone:
	case <-time.After(3*time.Minute + 30*time.Second):
		logger.Error("scheduler did not stop before the shutdown deadline")
	}
	select {
	case <-directAutomationDone:
	case <-time.After(5 * time.Second):
		logger.Error("Yandex Direct automation did not stop before the shutdown deadline")
	}
	logger.Info("server stopped")
}

func newMAXHTTPClient(caCertFile string) (*http.Client, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("default HTTP transport has an unexpected type")
	}
	clone := transport.Clone()
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if caCertFile != "" {
		roots, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("load system CA pool: %w", err)
		}
		pemBytes, err := os.ReadFile(caCertFile)
		if err != nil {
			return nil, fmt.Errorf("read MAX_CA_CERT_FILE: %w", err)
		}
		if ok := roots.AppendCertsFromPEM(pemBytes); !ok {
			return nil, errors.New("MAX_CA_CERT_FILE does not contain a valid PEM certificate")
		}
		tlsConfig.RootCAs = roots
	}
	// TLS verification remains enabled. MAX_CA_CERT_FILE only extends the
	// system trust store for the platform-api2.max.ru certificate chain.
	clone.TLSClientConfig = tlsConfig
	return &http.Client{Transport: clone, Timeout: 75 * time.Second}, nil
}
