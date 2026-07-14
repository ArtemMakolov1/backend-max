package maxclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var webhookSecretPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{5,256}$`)

// ConfigureStudioWebhook creates or updates the product webhook for the shared
// MAX bot. End users never call this method: the service operator configures it
// once for the bot used by every tenant.
func (c *Client) ConfigureStudioWebhook(ctx context.Context, rawURL, secret string) error {
	webhookURL, err := validateWebhookURL(rawURL)
	if err != nil {
		return err
	}
	if err := ValidateStudioWebhookConfiguration(rawURL, secret); err != nil {
		return err
	}

	body := struct {
		URL         string   `json:"url"`
		UpdateTypes []string `json:"update_types"`
		// #nosec G117 -- MAX requires the JSON key "secret" for webhook authentication; the value is sent only to the pinned API and is never logged.
		Secret string `json:"secret"`
	}{
		URL: webhookURL.String(),
		UpdateTypes: []string{
			"bot_added",
			"bot_removed",
			"bot_started",
			"message_callback",
		},
		Secret: secret,
	}

	var response operationResponse
	if err := c.doJSON(ctx, http.MethodPost, "/subscriptions", nil, body, &response); err != nil {
		return err
	}
	return response.asError(http.StatusOK)
}

// ValidateStudioWebhookConfiguration performs every local check without
// changing the shared bot subscription. Operators use it before probing the
// public endpoint, so a bad endpoint cannot replace a working subscription.
func ValidateStudioWebhookConfiguration(rawURL, secret string) error {
	if _, err := validateWebhookURL(rawURL); err != nil {
		return err
	}
	if !webhookSecretPattern.MatchString(secret) {
		return errors.New("configure MAX webhook: secret must contain 5 to 256 letters, digits, underscores or hyphens")
	}
	return nil
}

func validateWebhookURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !parsed.IsAbs() || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, errors.New("configure MAX webhook: URL must be an absolute HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("configure MAX webhook: URL must not contain credentials, a query or a fragment")
	}
	// MAX delivers production webhooks only to HTTPS port 443 and requires the
	// port to be omitted from the subscription URL.
	if parsed.Port() != "" {
		return nil, errors.New("configure MAX webhook: URL must use implicit HTTPS port 443")
	}
	return parsed, nil
}
