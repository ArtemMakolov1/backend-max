package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"maxpilot/backend/internal/store"
)

// activatePaidWorkspaceForTest moves a workspace through the same public
// checkout and canonical-payment reconciliation flow used by production. This
// keeps application fixtures honest when they exercise resources unavailable
// on Free, without reaching into Store internals or bypassing billing guards.
func activatePaidWorkspaceForTest(t *testing.T, storage *store.Store, ownerID, workspaceID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	attempt, err := storage.CreateBillingCheckoutAttempt(
		ctx,
		ownerID,
		workspaceID,
		"pro",
		true,
		store.BillingRecurringConsentVersion,
		"https://maxposty.ru/app/?billing=pending#/workspace/settings/plan",
		now,
	)
	if err != nil {
		t.Fatalf("create paid-plan checkout fixture: %v", err)
	}
	providerPaymentID := "test-payment-" + attempt.ID
	paidAt := now.Add(time.Second)
	if _, err := storage.AttachBillingProviderPayment(
		ctx, attempt.ID, providerPaymentID, "https://yookassa.test/confirmation/"+attempt.ID, paidAt,
	); err != nil {
		t.Fatalf("attach paid-plan provider fixture: %v", err)
	}
	dedupeKey := sha256.Sum256([]byte("paid-workspace-fixture:" + attempt.ID))
	processed, err := storage.ReconcileBillingPayment(
		ctx,
		"payment.succeeded",
		hex.EncodeToString(dedupeKey[:]),
		store.BillingCanonicalPayment{
			ProviderPaymentID:   providerPaymentID,
			Status:              "succeeded",
			Paid:                true,
			AmountMinor:         attempt.AmountMinor,
			CurrencyCode:        attempt.CurrencyCode,
			PaymentMethodID:     "test-method-" + attempt.ID,
			PaymentMethodSaved:  true,
			MetadataAttemptID:   attempt.ID,
			MetadataWorkspaceID: workspaceID,
			OccurredAt:          paidAt,
		},
		paidAt,
	)
	if err != nil || !processed {
		t.Fatalf("activate paid-plan fixture: processed=%v err=%v", processed, err)
	}
}
