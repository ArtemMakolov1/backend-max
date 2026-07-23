package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInFlightRenewalDoesNotPreventCancelOrDetach(t *testing.T) {
	for _, test := range []struct {
		name         string
		manualReview bool
		ambiguous    bool
		detach       bool
	}{
		{name: "pending cancel"},
		{name: "manual review cancel", manualReview: true},
		{name: "pending detach", detach: true},
		{name: "manual review detach", manualReview: true, detach: true},
		{name: "ambiguous cancel", ambiguous: true},
		{name: "ambiguous detach", ambiguous: true, detach: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			storage, err := Open(ctx, filepath.Join(t.TempDir(), "billing-in-flight.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = storage.Close() })
			workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: test.name})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
			seedBillingContract(t, storage, workspace.ID, "solo", now.AddDate(0, -1, 0), now, "sealed-old-method")

			attempt, _, err := storage.PrepareBillingRenewal(ctx, workspace.ID, now)
			if err != nil || attempt == nil {
				t.Fatalf("prepare renewal = %#v, %v", attempt, err)
			}
			providerID := strings.ReplaceAll(test.name, " ", "_")
			if test.ambiguous {
				if _, err := storage.BeginBillingProviderCreate(ctx, attempt.ID, now.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := storage.AttachBillingProviderPayment(ctx, attempt.ID, providerID, "", now.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			if test.manualReview {
				if err := storage.FailBillingProviderCreate(
					ctx, attempt.ID, "provider_create_outcome_unknown", true, now.Add(2*time.Second),
				); err != nil {
					t.Fatal(err)
				}
			}

			if test.detach {
				err = storage.DetachBillingPaymentMethod(ctx, "test-owner", workspace.ID, now.Add(3*time.Second))
			} else {
				var intent BillingCancellationIntent
				intent, err = storage.CreateBillingCancellationIntent(ctx, "test-owner", workspace.ID, now.Add(3*time.Second))
				if err == nil {
					digest := sha256.Sum256([]byte(intent.Token))
					err = storage.ConfirmBillingCancellation(
						ctx, "test-owner", workspace.ID, hex.EncodeToString(digest[:]), now.Add(4*time.Second),
					)
				}
			}
			if err != nil {
				t.Fatalf("disable future renewal: %v", err)
			}
			if test.ambiguous {
				ambiguous, readErr := storage.GetBillingPaymentAttempt(ctx, attempt.ID)
				if readErr != nil || ambiguous.Status != "manual_review" ||
					ambiguous.ErrorCode != "canceled_with_provider_outcome_unknown" {
					t.Fatalf("ambiguous attempt = %#v, %v", ambiguous, readErr)
				}
			}
			assertBillingTombstone(t, storage, workspace.ID, test.detach)

			processed, err := storage.ReconcileBillingPayment(ctx, "payment.succeeded",
				billingTestDedupe(test.name), BillingCanonicalPayment{
					ProviderPaymentID: providerID, Status: "succeeded", Paid: true,
					AmountMinor: attempt.AmountMinor, CurrencyCode: attempt.CurrencyCode,
					PaymentMethodID: "sealed-provider-method", PaymentMethodSaved: true,
					MetadataAttemptID: attempt.ID, MetadataWorkspaceID: workspace.ID,
					OccurredAt: now.Add(5 * time.Second),
				}, now.Add(5*time.Second))
			if err != nil || !processed {
				t.Fatalf("late successful renewal processed=%v err=%v", processed, err)
			}
			assertBillingTombstone(t, storage, workspace.ID, test.detach)
		})
	}
}

func TestPaidStaleRenewalIsQuarantinedWithoutReplacingNewPlan(t *testing.T) {
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "billing-stale-renewal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Stale renewal"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	oldPeriodID := seedBillingContract(
		t, storage, workspace.ID, "solo", now.AddDate(0, -1, 0), now, "sealed-old-method",
	)
	attempt, _, err := storage.PrepareBillingRenewal(ctx, workspace.ID, now)
	if err != nil || attempt == nil {
		t.Fatalf("prepare renewal = %#v, %v", attempt, err)
	}
	if _, err := storage.AttachBillingProviderPayment(ctx, attempt.ID, "stale_provider_payment", "", now); err != nil {
		t.Fatal(err)
	}

	if _, err := storage.db.ExecContext(ctx, `UPDATE billing_subscription_periods
SET status='completed',updated_at=$2 WHERE id=$1`, oldPeriodID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var newPeriodID int64
	if err := storage.db.QueryRowContext(ctx, `INSERT INTO billing_subscription_periods(
workspace_id,plan_code,plan_version,status,period_start,period_end,list_price_minor,
charged_price_minor,currency_code,created_at,updated_at)
VALUES($1,'pro',2,'active',$2,$3,249000,249000,'RUB',$2,$2) RETURNING id`,
		workspace.ID, now.Add(time.Hour), now.AddDate(0, 1, 0)).Scan(&newPeriodID); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.ExecContext(ctx, `UPDATE billing_subscription_contracts SET
status='active',payment_method_id='sealed-new-method',current_period_id=$2,cancel_at_period_end=FALSE,
next_charge_at=$3,grace_until=NULL,version=version+1,updated_at=$4 WHERE workspace_id=$1`,
		workspace.ID, newPeriodID, now.AddDate(0, 1, 0), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.ExecContext(ctx, `UPDATE workspace_subscriptions SET
plan_code='pro',plan_version=2,status='active',updated_at=$2 WHERE workspace_id=$1`,
		workspace.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	processed, err := storage.ReconcileBillingPayment(ctx, "payment.succeeded",
		strings.Repeat("b", 64), BillingCanonicalPayment{
			ProviderPaymentID: "stale_provider_payment", Status: "succeeded", Paid: true,
			AmountMinor: attempt.AmountMinor, CurrencyCode: attempt.CurrencyCode,
			PaymentMethodID: "sealed-old-method", PaymentMethodSaved: true,
			MetadataAttemptID: attempt.ID, MetadataWorkspaceID: workspace.ID,
			OccurredAt: now.Add(2 * time.Hour),
		}, now.Add(2*time.Hour))
	if processed || !errors.Is(err, ErrBillingIntegrity) {
		t.Fatalf("stale renewal processed=%v err=%v, want quarantined integrity failure", processed, err)
	}
	var planCode, contractStatus, paymentMethod, attemptStatus, errorCode, receiptResult string
	var currentPeriodID int64
	if err := storage.db.QueryRowContext(ctx, `SELECT s.plan_code,c.status,c.payment_method_id,
c.current_period_id,a.status,a.error_code,r.result
FROM workspace_subscriptions s
JOIN billing_subscription_contracts c ON c.workspace_id=s.workspace_id
JOIN billing_payment_attempts a ON a.id=$2
JOIN billing_webhook_receipts r ON r.dedupe_key=$3
WHERE s.workspace_id=$1`, workspace.ID, attempt.ID, strings.Repeat("b", 64)).Scan(
		&planCode, &contractStatus, &paymentMethod, &currentPeriodID,
		&attemptStatus, &errorCode, &receiptResult,
	); err != nil {
		t.Fatal(err)
	}
	if planCode != "pro" || contractStatus != "active" || paymentMethod != "sealed-new-method" ||
		currentPeriodID != newPeriodID || attemptStatus != "manual_review" ||
		errorCode != "stale_renewal_paid_manual_refund" || receiptResult != "failed" {
		t.Fatalf("stale reconciliation mutated live contract: plan=%s status=%s method=%s period=%d attempt=%s code=%s receipt=%s",
			planCode, contractStatus, paymentMethod, currentPeriodID, attemptStatus, errorCode, receiptResult)
	}
}

func TestOpenRenewalAdvancesGraceAndDowngradesBeforeLateSuccess(t *testing.T) {
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "billing-open-grace.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Open grace"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	periodEnd := now.Add(time.Hour)
	seedBillingContract(t, storage, workspace.ID, "solo", periodEnd.AddDate(0, -1, 0), periodEnd, "sealed-method")
	if _, err := storage.db.ExecContext(ctx, `UPDATE billing_subscription_contracts
SET next_charge_at=$2 WHERE workspace_id=$1`, workspace.ID, now); err != nil {
		t.Fatal(err)
	}
	attempt, _, err := storage.PrepareBillingRenewal(ctx, workspace.ID, now)
	if err != nil || attempt == nil {
		t.Fatalf("prepare early renewal = %#v, %v", attempt, err)
	}
	if _, err := storage.AttachBillingProviderPayment(ctx, attempt.ID, "open_grace_payment", "", now); err != nil {
		t.Fatal(err)
	}
	if existing, downgraded, err := storage.PrepareBillingRenewal(ctx, workspace.ID, periodEnd); err != nil || existing != nil || downgraded {
		t.Fatalf("advance open renewal lifecycle = %#v, %v, %v", existing, downgraded, err)
	}
	var status string
	var graceUntil time.Time
	if err := storage.db.QueryRowContext(ctx, `SELECT status,grace_until
FROM billing_subscription_contracts WHERE workspace_id=$1`, workspace.ID).Scan(&status, &graceUntil); err != nil {
		t.Fatal(err)
	}
	if status != "past_due" || !graceUntil.After(periodEnd) {
		t.Fatalf("open renewal did not start grace: status=%s grace=%v", status, graceUntil)
	}
	if existing, downgraded, err := storage.PrepareBillingRenewal(
		ctx, workspace.ID, graceUntil.Add(time.Second),
	); err != nil || existing != nil || !downgraded {
		t.Fatalf("grace expiry = %#v, %v, %v", existing, downgraded, err)
	}
	var planCode string
	if err := storage.db.QueryRowContext(ctx, `SELECT plan_code FROM workspace_subscriptions
WHERE workspace_id=$1`, workspace.ID).Scan(&planCode); err != nil {
		t.Fatal(err)
	}
	if planCode != "free" {
		t.Fatalf("expired open renewal kept plan %q, want free", planCode)
	}
	processed, err := storage.ReconcileBillingPayment(ctx, "payment.succeeded", strings.Repeat("c", 64),
		BillingCanonicalPayment{
			ProviderPaymentID: "open_grace_payment", Status: "succeeded", Paid: true,
			AmountMinor: attempt.AmountMinor, CurrencyCode: attempt.CurrencyCode,
			PaymentMethodID: "sealed-method", PaymentMethodSaved: true,
			MetadataAttemptID: attempt.ID, MetadataWorkspaceID: workspace.ID,
			OccurredAt: graceUntil.Add(2 * time.Second),
		}, graceUntil.Add(2*time.Second))
	if processed || !errors.Is(err, ErrBillingIntegrity) {
		t.Fatalf("late post-grace success processed=%v err=%v", processed, err)
	}
}

func TestUnresolvedRenewalFromPreviousCycleBlocksFutureAutomaticDebit(t *testing.T) {
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "billing-cross-cycle-open.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Cross-cycle open renewal"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	oldPeriodID := seedBillingContract(
		t, storage, workspace.ID, "solo", now.AddDate(0, -1, 0), now, "sealed-old-method",
	)
	oldAttempt, _, err := storage.PrepareBillingRenewal(ctx, workspace.ID, now)
	if err != nil || oldAttempt == nil {
		t.Fatalf("prepare old renewal = %#v, %v", oldAttempt, err)
	}
	if _, err := storage.AttachBillingProviderPayment(
		ctx, oldAttempt.ID, "provider_outcome_unknown", "", now.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if err := storage.FailBillingProviderCreate(
		ctx, oldAttempt.ID, "provider_create_outcome_unknown", true, now.Add(2*time.Second),
	); err != nil {
		t.Fatal(err)
	}

	// Simulate a later explicit checkout activating a fresh paid period while
	// the earlier renewal outcome is still unresolved. Explicit checkout is
	// allowed, but it must not re-enable unattended recurring debits.
	newStart := now.Add(time.Hour)
	newEnd := newStart.AddDate(0, 1, 0)
	if _, err := storage.db.ExecContext(ctx, `UPDATE billing_subscription_periods
SET status='completed',updated_at=$2 WHERE id=$1`, oldPeriodID, newStart); err != nil {
		t.Fatal(err)
	}
	var newPeriodID int64
	if err := storage.db.QueryRowContext(ctx, `INSERT INTO billing_subscription_periods(
workspace_id,plan_code,plan_version,status,period_start,period_end,list_price_minor,
charged_price_minor,currency_code,created_at,updated_at)
VALUES($1,'pro',2,'active',$2,$3,249000,249000,'RUB',$2,$2) RETURNING id`,
		workspace.ID, newStart, newEnd).Scan(&newPeriodID); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.ExecContext(ctx, `UPDATE billing_subscription_contracts SET
status='active',payment_method_id='sealed-new-method',current_period_id=$2,cancel_at_period_end=FALSE,
next_charge_at=$3,grace_until=NULL,renewal_attempts=0,version=version+1,updated_at=$4
WHERE workspace_id=$1`, workspace.ID, newPeriodID, newEnd, newStart); err != nil {
		t.Fatal(err)
	}

	attempt, downgraded, err := storage.PrepareBillingRenewal(ctx, workspace.ID, newEnd)
	if err != nil || attempt != nil || downgraded {
		t.Fatalf("future renewal with unresolved prior outcome = %#v, %v, %v", attempt, downgraded, err)
	}
	var renewalCount int
	if err := storage.db.QueryRowContext(ctx, `SELECT count(*) FROM billing_payment_attempts
WHERE workspace_id=$1 AND purpose='renewal'`, workspace.ID).Scan(&renewalCount); err != nil {
		t.Fatal(err)
	}
	if renewalCount != 1 {
		t.Fatalf("created %d renewal attempts with unresolved prior outcome, want 1", renewalCount)
	}
}

func seedBillingContract(
	t *testing.T, storage *Store, workspaceID, planCode string,
	periodStart, periodEnd time.Time, paymentMethod string,
) int64 {
	t.Helper()
	ctx := context.Background()
	price := int64(99000)
	if planCode == "pro" {
		price = 249000
	}
	var periodID int64
	if err := storage.db.QueryRowContext(ctx, `INSERT INTO billing_subscription_periods(
workspace_id,plan_code,plan_version,status,period_start,period_end,list_price_minor,
charged_price_minor,currency_code,created_at,updated_at)
VALUES($1,$2,2,'active',$3,$4,$5,$5,'RUB',$3,$3) RETURNING id`,
		workspaceID, planCode, periodStart, periodEnd, price).Scan(&periodID); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.ExecContext(ctx, `INSERT INTO billing_subscription_contracts(
workspace_id,payer_user_id,status,payment_method_id,current_period_id,cancel_at_period_end,
next_charge_at,created_at,updated_at)
VALUES($1,'test-owner','active',$2,$3,FALSE,$4,$5,$5)`,
		workspaceID, paymentMethod, periodID, periodEnd, periodStart); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.ExecContext(ctx, `UPDATE workspace_subscriptions SET
plan_code=$2,plan_version=2,status='active',started_at=$3,updated_at=$3 WHERE workspace_id=$1`,
		workspaceID, planCode, periodStart); err != nil {
		t.Fatal(err)
	}
	return periodID
}

func assertBillingTombstone(t *testing.T, storage *Store, workspaceID string, detached bool) {
	t.Helper()
	var cancelAtEnd bool
	var paymentMethod string
	if err := storage.db.QueryRowContext(context.Background(), `SELECT cancel_at_period_end,payment_method_id
FROM billing_subscription_contracts WHERE workspace_id=$1`, workspaceID).Scan(&cancelAtEnd, &paymentMethod); err != nil {
		t.Fatal(err)
	}
	if !cancelAtEnd || (detached && paymentMethod != "") || (!detached && paymentMethod == "") {
		t.Fatalf("billing tombstone cancel=%v method=%q detached=%v", cancelAtEnd, paymentMethod, detached)
	}
	if detached {
		var reusableSnapshots int
		if err := storage.db.QueryRowContext(context.Background(), `SELECT count(*)
FROM billing_payment_attempts WHERE workspace_id=$1 AND payment_method_snapshot<>''`, workspaceID).Scan(&reusableSnapshots); err != nil {
			t.Fatal(err)
		}
		if reusableSnapshots != 0 {
			t.Fatalf("detach retained %d reusable payment method snapshots", reusableSnapshots)
		}
	}
}

func billingTestDedupe(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
