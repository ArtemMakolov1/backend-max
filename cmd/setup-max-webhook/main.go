package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"maxpilot/backend/internal/maxclient"
)

const defaultMAXAPIBaseURL = "https://platform-api2.max.ru"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	token := strings.TrimSpace(os.Getenv("MAX_BOT_TOKEN"))
	webhookURL := strings.TrimSpace(os.Getenv("MAX_WEBHOOK_URL"))
	secret := strings.TrimSpace(os.Getenv("MAX_WEBHOOK_SECRET"))
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("MAX_API_BASE_URL")), "/")
	if baseURL == "" {
		baseURL = defaultMAXAPIBaseURL
	}
	if token == "" || webhookURL == "" || secret == "" {
		logger.Error("MAX_BOT_TOKEN, MAX_WEBHOOK_URL and MAX_WEBHOOK_SECRET are required")
		os.Exit(1)
	}
	if baseURL != defaultMAXAPIBaseURL {
		logger.Error("MAX_API_BASE_URL must be the official HTTPS MAX API origin", "expected", defaultMAXAPIBaseURL)
		os.Exit(1)
	}

	httpClient, err := newMAXHTTPClient(strings.TrimSpace(os.Getenv("MAX_CA_CERT_FILE")))
	if err != nil {
		logger.Error("could not configure MAX TLS", "error", err)
		os.Exit(1)
	}
	client, err := maxclient.New(baseURL, token, httpClient)
	if err != nil {
		logger.Error("could not initialize MAX client", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := maxclient.ValidateStudioWebhookConfiguration(webhookURL, secret); err != nil {
		logger.Error("invalid MAX webhook configuration", "error", err)
		os.Exit(1)
	}
	if err := preflightWebhook(ctx, httpClient, webhookURL, secret); err != nil {
		logger.Error("MAX webhook public endpoint failed preflight; existing subscription was not changed", "error", err)
		os.Exit(1)
	}
	if err := client.ConfigureStudioWebhook(ctx, webhookURL, secret); err != nil {
		logger.Error("could not configure MAX webhook", "error", err)
		os.Exit(1)
	}
	logger.Info("MAX webhook configured", "url", webhookURL)
}

func preflightWebhook(ctx context.Context, client *http.Client, webhookURL, secret string) error {
	if client == nil {
		return errors.New("webhook preflight HTTP client is required")
	}
	if err := maxclient.ValidateStudioWebhookConfiguration(webhookURL, secret); err != nil {
		return err
	}
	body := []byte(`{"update_type":"maxstudio_preflight","timestamp":1}`)
	// #nosec G704 -- ValidateStudioWebhookConfiguration above restricts this operator-owned target to absolute HTTPS on implicit port 443, without credentials, query or fragment.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create webhook preflight request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Max-Bot-Api-Secret", secret)

	preflightClient := *client
	preflightClient.Timeout = 15 * time.Second
	preflightClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	// #nosec G704 -- the operator-owned URL passed ValidateStudioWebhookConfiguration: absolute HTTPS, no credentials/query/fragment, implicit port 443; redirects are disabled.
	response, err := preflightClient.Do(req)
	if err != nil {
		return fmt.Errorf("call public webhook endpoint: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("public webhook endpoint returned HTTP %d, MAX requires 200", response.StatusCode)
	}
	return nil
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
		// #nosec G703 -- this is an operator-owned local environment path, never HTTP input; the contents are subsequently parsed as PEM certificates.
		pemBytes, err := os.ReadFile(caCertFile)
		if err != nil {
			return nil, fmt.Errorf("read MAX_CA_CERT_FILE: %w", err)
		}
		if ok := roots.AppendCertsFromPEM(pemBytes); !ok {
			return nil, errors.New("MAX_CA_CERT_FILE does not contain a valid PEM certificate")
		}
		tlsConfig.RootCAs = roots
	}
	clone.TLSClientConfig = tlsConfig
	return &http.Client{Transport: clone, Timeout: 30 * time.Second}, nil
}
