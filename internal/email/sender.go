// Package email sends MaxPosty's transactional email. The only message today is
// the welcome email delivered on a user's first sign-in. Delivery is meant to be
// best-effort: callers should treat SendWelcome errors as non-fatal and only log
// them. When SMTP is not configured, wire a NoopSender so sign-in never depends
// on a mail server being present.
package email

import (
	"context"
	"log/slog"
)

// Sender delivers transactional email to MaxPosty users.
type Sender interface {
	SendWelcome(ctx context.Context, recipient WelcomeRecipient) error
}

// WelcomeRecipient identifies the freshly registered user who should receive the
// welcome email. DisplayName may be empty, in which case the greeting is
// rendered without a name.
type WelcomeRecipient struct {
	Email       string
	DisplayName string
}

// NoopSender is used when SMTP is not configured. It performs no delivery and
// reports success so a missing mail server never blocks sign-in. It records a
// debug log to make the disabled state visible in development and tests.
type NoopSender struct {
	logger *slog.Logger
}

// NewNoopSender returns a Sender that silently drops welcome emails. A nil
// logger falls back to slog.Default().
func NewNoopSender(logger *slog.Logger) NoopSender {
	if logger == nil {
		logger = slog.Default()
	}
	return NoopSender{logger: logger}
}

// SendWelcome does nothing and returns nil. The recipient is intentionally not
// logged to avoid recording user email addresses in the disabled path.
func (n NoopSender) SendWelcome(_ context.Context, _ WelcomeRecipient) error {
	if n.logger != nil {
		n.logger.Debug("welcome email skipped: SMTP is not configured")
	}
	return nil
}

var (
	_ Sender = NoopSender{}
	_ Sender = (*SMTPSender)(nil)
)
