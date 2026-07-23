package app

import (
	"testing"
	"time"

	"maxpilot/backend/internal/store"
)

func TestBillingPendingHorizonBoundary(t *testing.T) {
	started := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	attempt := store.BillingPaymentAttempt{CreatedAt: started.Add(-time.Hour), ProviderCreateStartedAt: &started}
	if billingPendingHorizonExceeded(attempt, started.Add(billingProviderPendingHorizon-time.Nanosecond)) {
		t.Fatal("pending payment entered manual review before the horizon")
	}
	if !billingPendingHorizonExceeded(attempt, started.Add(billingProviderPendingHorizon)) {
		t.Fatal("pending payment did not enter manual review at the horizon")
	}
}
