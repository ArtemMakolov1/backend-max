package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"maxpilot/backend/internal/legal"
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
	user, err := storage.GetUser(ctx, ownerID)
	if err != nil {
		t.Fatalf("read paid-plan fixture user: %v", err)
	}
	consentSessionHash := sha256.Sum256([]byte("paid-workspace-consent:" + ownerID))
	if _, err := storage.CreateAuthenticatedSession(ctx, user, []store.Consent{
		{Document: "terms", Version: legal.CurrentTermsVersion, AcceptedAt: now, Source: "test"},
		{Document: "personal_data", Version: legal.CurrentPersonalDataVersion, AcceptedAt: now, Source: "test"},
	}, store.AuthSession{
		TokenHash: hex.EncodeToString(consentSessionHash[:]), OwnerID: ownerID,
		Provider: "yandex", ProviderSubject: ownerID, Login: user.Login, Email: user.Email,
		DisplayName: user.DisplayName, AvatarURL: user.AvatarURL,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("record paid-plan fixture consent: %v", err)
	}
	attempt, err := storage.CreateBillingCheckoutAttempt(
		ctx,
		ownerID,
		workspaceID,
		store.BillingCheckoutSnapshot{
			PlanCode: "pro", PlanVersion: 2, MonthlyPriceMinor: 249000, CurrencyCode: "RUB",
			RecurringConsent: true, RecurringConsentVersion: store.BillingRecurringConsentVersion,
			RecurringConsentTermsVersion: store.BillingRecurringTermsVersion,
		},
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
