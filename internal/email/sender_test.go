package email

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestNoopSenderReportsSuccessAndLogsDebug(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sender := NewNoopSender(logger)

	if err := sender.SendWelcome(context.Background(), WelcomeRecipient{Email: "user@example.com", DisplayName: "Иван"}); err != nil {
		t.Fatalf("NoopSender.SendWelcome returned error: %v", err)
	}
	logged := buf.String()
	if !strings.Contains(logged, "SMTP is not configured") {
		t.Errorf("expected a debug log about the disabled sender, got %q", logged)
	}
	if strings.Contains(logged, "user@example.com") {
		t.Errorf("disabled path logged the recipient address: %q", logged)
	}
}

func TestNoopSenderZeroValueDoesNotPanic(t *testing.T) {
	t.Parallel()
	var sender NoopSender
	if err := sender.SendWelcome(context.Background(), WelcomeRecipient{Email: "user@example.com"}); err != nil {
		t.Fatalf("zero-value NoopSender returned error: %v", err)
	}
}
