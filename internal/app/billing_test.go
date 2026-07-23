package app

import (
	"testing"
	"time"

	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yookassa"
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

func TestCanonicalBillingPaymentCarriesCancellationReason(t *testing.T) {
	application := &App{}
	canonical, err := application.canonicalBillingPayment(yookassa.Payment{
		ID:     "provider-payment",
		Status: "canceled",
		Amount: yookassa.Amount{Value: "990.00", Currency: "RUB"},
		Metadata: map[string]string{
			"attempt_id":   "attempt",
			"workspace_id": "workspace",
		},
		CancellationDetails: &yookassa.CancellationDetails{
			Party: "yoo_kassa", Reason: " permission_revoked ",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if canonical.CancellationReason != "permission_revoked" {
		t.Fatalf("canonical cancellation reason=%q", canonical.CancellationReason)
	}
}
