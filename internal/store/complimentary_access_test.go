package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func complimentaryStoreFixture(t *testing.T) (*Store, Workspace, time.Time) {
	t.Helper()
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "complimentary.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, id := range []string{"comp-owner", "comp-other", "comp-writer"} {
		if err = s.UpsertUser(t.Context(), User{ID: id, Email: id + "@example.invalid", DisplayName: id}); err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.ExecContext(t.Context(), `INSERT INTO auth_identities(provider,subject,owner_id,created_at,updated_at) VALUES('yandex',$1,$1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, id); err != nil {
			t.Fatal(err)
		}
	}
	w, err := s.CreateWorkspaceLimited(t.Context(), "comp-owner", Workspace{Name: "Owned team"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	return s, w, time.Now().UTC().Truncate(time.Millisecond)
}

func complimentaryOperation(t *testing.T, s *Store, operation string) (bool, bool) {
	t.Helper()
	var id string
	var active, changed bool
	var owned int64
	err := s.db.QueryRowContext(t.Context(), `SELECT * FROM manage_account_complimentary_access($1,$2,$3,$4)`, " COMP-OWNER@EXAMPLE.INVALID ", operation, "github:synthetic-operator", "github-run:synthetic").Scan(&id, &active, &owned, &changed)
	if err != nil || id != "comp-owner" || owned < 2 {
		t.Fatalf("operator %s result: id=%s active=%v owned=%d changed=%v err=%v", operation, id, active, owned, changed, err)
	}
	return active, changed
}

func TestComplimentaryAccessIsInheritedOnlyByOwnedWorkspacesAndKeepsRealUsage(t *testing.T) {
	t.Parallel()
	s, team, now := complimentaryStoreFixture(t)
	if _, err := s.CreateWorkspaceLimited(t.Context(), "comp-owner", Workspace{Name: "Before grant"}, 1); !errors.Is(err, ErrOwnedTeamWorkspaceLimit) {
		t.Fatalf("baseline account cap not enforced: %v", err)
	}
	if active, changed := complimentaryOperation(t, s, "grant"); !active || !changed {
		t.Fatal("grant did not activate")
	}
	if active, changed := complimentaryOperation(t, s, "grant"); !active || changed {
		t.Fatal("same grant was not idempotent")
	}
	future, err := s.CreateWorkspaceLimited(t.Context(), "comp-owner", Workspace{Name: "After grant"}, 1)
	if err != nil {
		t.Fatalf("future owned workspace did not inherit grant: %v", err)
	}
	for _, workspaceID := range []string{team.ID, future.ID} {
		state, err := s.GetWorkspaceBillingState(t.Context(), "comp-owner", workspaceID, now)
		if err != nil || !state.ComplimentaryAccess || state.Subscription.Plan.Code != "free" || state.Contract != nil || state.Actions.CanCheckout || state.Actions.CanResume || !state.Features.AIImages || !state.Features.AIResearch || !state.Features.AIFormat || !state.Features.AIBrandKit || !state.Features.AIChannelDescription {
			t.Fatalf("grant was confused with a payment/feature gate: %+v %v", state, err)
		}
		for _, e := range state.Entitlements {
			if !e.Unlimited || e.HardLimit || e.Limit > 1<<30 {
				t.Fatalf("effective unlimited allowance was faked: %+v", e)
			}
		}
	}
	for _, metric := range []string{UsageMetricAIImageCredits, UsageMetricAIResearchRequests, UsageMetricAIFormatRequests} {
		usage, err := s.ChargeWorkspaceMonthlyUsage(t.Context(), team.ID, metric, 1000, true, now)
		if err != nil || usage.Quantity != 1000 {
			t.Fatalf("monthly %s grant did not preserve ledger: %+v %v", metric, usage, err)
		}
	}
	for i, chat := range []string{"-10001", "-10002"} {
		if _, err = s.CreateChannelForWorkspace(t.Context(), "comp-owner", team.ID, Channel{Title: "Channel", MAXChatID: chat, IsChannel: true, Active: true}); err != nil {
			t.Fatalf("static channel %d cap not removed: %v", i, err)
		}
	}
	if _, err = s.AddWorkspaceMember(t.Context(), "comp-owner", WorkspaceMember{WorkspaceID: team.ID, UserID: "comp-writer", Role: WorkspaceRoleEditor}); err != nil {
		t.Fatalf("static seat cap not removed: %v", err)
	}
	limits := MediaLimits{MaxFiles: 1, MaxBytes: 1}
	for _, key := range []string{strings.Repeat("a", 64) + ".png", strings.Repeat("b", 64) + ".png"} {
		reservation, err := s.ReserveMediaForWorkspace(t.Context(), "comp-writer", team.ID, key, 4, limits, now)
		if err != nil {
			t.Fatalf("configured storage cap not removed: %v", err)
		}
		if err = s.CompleteMediaReservation(t.Context(), reservation, now); err != nil {
			t.Fatal(err)
		}
	}
	media, err := s.GetWorkspaceMediaUsage(t.Context(), team.ID)
	if err != nil || media.AssetCount != 2 || media.TotalBytes != 8 {
		t.Fatalf("media ledger not preserved: %+v %v", media, err)
	}
	other, err := s.CreateWorkspace(t.Context(), "comp-other", Workspace{Name: "Other owner's workspace"})
	if err != nil {
		t.Fatal(err)
	}
	seedBillingContract(t, s, other.ID, "pro", now.Add(-time.Hour), now.Add(24*time.Hour), "synthetic-method")
	if _, err = s.AddWorkspaceMember(t.Context(), "comp-other", WorkspaceMember{WorkspaceID: other.ID, UserID: "comp-owner", Role: WorkspaceRoleEditor}); err != nil {
		t.Fatal(err)
	}
	otherState, err := s.GetWorkspaceBillingState(t.Context(), "comp-owner", other.ID, now)
	if err != nil || otherState.ComplimentaryAccess {
		t.Fatalf("membership inherited another owner's grant: %+v %v", otherState, err)
	}
	for _, e := range otherState.Entitlements {
		if e.Unlimited {
			t.Fatalf("another owner's quota was bypassed: %+v", e)
		}
	}
	_, err = s.ChargeWorkspaceMonthlyUsage(t.Context(), other.ID, UsageMetricAIImageCredits, 1_000_000, true, now)
	var monthly *WorkspaceUsageLimitError
	if !errors.As(err, &monthly) {
		t.Fatalf("another owner's monthly quota disappeared: %v", err)
	}
	var role string
	if err = s.db.QueryRowContext(t.Context(), `SELECT role FROM workspace_members WHERE workspace_id=$1 AND user_id='comp-writer'`, team.ID).Scan(&role); err != nil || role != WorkspaceRoleEditor {
		t.Fatalf("grant changed a member role: %s %v", role, err)
	}
	var payments int
	if err = s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM billing_payment_attempts WHERE workspace_id=ANY($1::text[])`, []string{team.ID, future.ID}).Scan(&payments); err != nil || payments != 0 {
		t.Fatalf("grant manufactured payments: %d %v", payments, err)
	}
}

func TestComplimentaryAccessRevokeAndOwnershipTransferRestoreQuotaBoundaries(t *testing.T) {
	t.Parallel()
	s, team, now := complimentaryStoreFixture(t)
	complimentaryOperation(t, s, "grant")
	for _, chat := range []string{"-20001", "-20002"} {
		if _, err := s.CreateChannelForWorkspace(t.Context(), "comp-owner", team.ID, Channel{Title: "Channel", MAXChatID: chat, IsChannel: true, Active: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddWorkspaceMember(t.Context(), "comp-owner", WorkspaceMember{WorkspaceID: team.ID, UserID: "comp-other", Role: WorkspaceRoleEditor}); err != nil {
		t.Fatal(err)
	}
	transferred, err := s.TransferWorkspaceOwnershipLimited(t.Context(), "comp-owner", team.ID, "comp-other", 1)
	if err != nil || transferred.OwnerUserID != "comp-other" {
		t.Fatalf("ownership transfer: %+v %v", transferred, err)
	}
	state, err := s.GetWorkspaceBillingState(t.Context(), "comp-owner", team.ID, now)
	if err != nil || state.ComplimentaryAccess || state.Features.AIImages {
		t.Fatalf("old owner grant survived transfer: %+v %v", state, err)
	}
	var overage bool
	if err = s.db.QueryRowContext(t.Context(), `SELECT workspace_static_entitlement_overage($1)`, team.ID).Scan(&overage); err != nil || !overage {
		t.Fatalf("transferred static quota bypassed: %v %v", overage, err)
	}
	if _, err = s.CreateChannelForWorkspace(t.Context(), "comp-other", team.ID, Channel{Title: "Blocked", MAXChatID: "-20003", IsChannel: true, Active: true}); err == nil {
		t.Fatal("transferred non-granted owner retained unlimited channels")
	}
	var personal string
	if err = s.db.QueryRowContext(t.Context(), `SELECT id FROM workspaces WHERE owner_user_id='comp-owner' AND is_personal`).Scan(&personal); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{strings.Repeat("c", 64) + ".png", strings.Repeat("d", 64) + ".png"} {
		if _, err = s.ReserveMedia(t.Context(), "comp-owner", key, 4, MediaLimits{MaxFiles: 1, MaxBytes: 1}, now); err != nil {
			t.Fatalf("legacy media grant: %v", err)
		}
	}
	if active, changed := complimentaryOperationAfterTransfer(t, s, "revoke"); active || !changed {
		t.Fatal("revoke failed")
	}
	if _, err = s.ReserveMedia(t.Context(), "comp-owner", strings.Repeat("e", 64)+".png", 1, MediaLimits{MaxFiles: 1, MaxBytes: 1}, now); !errors.Is(err, ErrMediaQuotaExceeded) {
		t.Fatalf("legacy quota not restored: %v", err)
	}
	if _, err = s.ChargeWorkspaceMonthlyUsage(t.Context(), personal, UsageMetricAIImageCredits, 1, true, now); err == nil {
		t.Fatal("revoked Free account retained paid AI")
	}
}

func complimentaryOperationAfterTransfer(t *testing.T, s *Store, operation string) (bool, bool) {
	t.Helper()
	var active, changed bool
	err := s.db.QueryRowContext(t.Context(), `SELECT active,changed FROM manage_account_complimentary_access($1,$2,$3,$4)`, "comp-owner@example.invalid", operation, "github:synthetic-operator", "github-run:synthetic-transfer").Scan(&active, &changed)
	if err != nil {
		t.Fatal(err)
	}
	return active, changed
}

func TestComplimentaryAccessBlocksNewBillingButKeepsExistingContractManagement(t *testing.T) {
	t.Parallel()
	s, team, now := complimentaryStoreFixture(t)
	seedBillingContract(t, s, team.ID, "solo", now.AddDate(0, -1, 0), now, "synthetic-real-method")
	renewal, _, err := s.PrepareBillingRenewal(t.Context(), team.ID, now)
	if err != nil || renewal == nil {
		t.Fatalf("pre-grant renewal: %+v %v", renewal, err)
	}
	var before string
	if err = s.db.QueryRowContext(t.Context(), `SELECT row_to_json(c)::text FROM billing_subscription_contracts c WHERE workspace_id=$1`, team.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	complimentaryOperation(t, s, "grant")
	var after string
	if err = s.db.QueryRowContext(t.Context(), `SELECT row_to_json(c)::text FROM billing_subscription_contracts c WHERE workspace_id=$1`, team.ID).Scan(&after); err != nil || after != before {
		t.Fatalf("grant mutated a real contract: %v", err)
	}
	if _, err = s.BeginBillingProviderCreate(t.Context(), renewal.ID, now.Add(time.Second)); !errors.Is(err, ErrBillingComplimentaryAccess) {
		t.Fatalf("grant-after-prepare allowed provider charge: %v", err)
	}
	prepared, _, err := s.PrepareBillingRenewal(t.Context(), team.ID, now.Add(time.Second))
	if err != nil || prepared != nil {
		t.Fatalf("new renewal under grant: %+v %v", prepared, err)
	}
	due, err := s.ListDueBillingContracts(t.Context(), now.Add(time.Second), 20)
	if err != nil || len(due) != 0 {
		t.Fatalf("granted contract still due: %+v %v", due, err)
	}
	worker, err := s.ListBillingAttemptsForWorker(t.Context(), now.Add(time.Second), 20)
	if err != nil || len(worker) != 0 {
		t.Fatalf("prepared payment under grant claimed by worker: %+v %v", worker, err)
	}
	snapshot := BillingCheckoutSnapshot{PlanCode: "pro", RecurringConsent: true}
	if _, err = s.CreateBillingCheckoutAttempt(t.Context(), "comp-owner", team.ID, snapshot, "https://example.invalid/return", now); !errors.Is(err, ErrBillingComplimentaryAccess) {
		t.Fatalf("direct new checkout not blocked: %v", err)
	}
	if _, err = s.AddWorkspaceMember(t.Context(), "comp-owner", WorkspaceMember{WorkspaceID: team.ID, UserID: "comp-writer", Role: WorkspaceRoleEditor}); err != nil {
		t.Fatal(err)
	}
	intent, err := s.CreateBillingCancellationIntent(t.Context(), "comp-owner", team.ID, now)
	if err != nil {
		t.Fatalf("real cancellation intent disabled by grant: %v", err)
	}
	tokenHash := billingTestDedupe(intent.Token)
	if err = s.AcceptBillingRetentionOffer(t.Context(), "comp-owner", team.ID, tokenHash, now); !errors.Is(err, ErrBillingComplimentaryAccess) {
		t.Fatalf("paid retention reactivation not blocked: %v", err)
	}
	if err = s.ConfirmBillingCancellation(t.Context(), "comp-owner", team.ID, tokenHash, now); err != nil {
		t.Fatalf("real cancellation disabled by grant: %v", err)
	}
	if err = s.ResumeBillingSubscription(t.Context(), "comp-writer", team.ID, now); !errors.Is(err, ErrBillingOwnerRequired) {
		t.Fatalf("non-owner received grant-specific authority: %v", err)
	}
	if err = s.ResumeBillingSubscription(t.Context(), "comp-owner", team.ID, now); !errors.Is(err, ErrBillingComplimentaryAccess) {
		t.Fatalf("direct paid resume not blocked: %v", err)
	}
	state, err := s.GetWorkspaceBillingState(t.Context(), "comp-owner", team.ID, now)
	if err != nil || !state.Actions.CanDetachPaymentMethod || state.Actions.CanResume || state.Actions.CanCheckout || state.Contract == nil || !state.Contract.PaymentMethod.Saved {
		t.Fatalf("existing management was lost: %+v %v", state, err)
	}
	if err = s.DetachBillingPaymentMethod(t.Context(), "comp-owner", team.ID, now); err != nil {
		t.Fatalf("real payment detach disabled by grant: %v", err)
	}
	var submitted sql.NullTime
	if err = s.db.QueryRowContext(t.Context(), `SELECT provider_create_started_at FROM billing_payment_attempts WHERE id=$1`, renewal.ID).Scan(&submitted); err != nil || submitted.Valid {
		t.Fatalf("blocked renewal crossed provider boundary: %v %v", submitted.Valid, err)
	}
}

func TestComplimentaryAccessKeepsSubmittedProviderPaymentReconciliation(t *testing.T) {
	t.Parallel()
	s, team, now := complimentaryStoreFixture(t)
	seedBillingContract(t, s, team.ID, "solo", now.AddDate(0, -1, 0), now, "synthetic-real-method")
	renewal, _, err := s.PrepareBillingRenewal(t.Context(), team.ID, now)
	if err != nil || renewal == nil {
		t.Fatalf("prepare pre-grant renewal: %+v %v", renewal, err)
	}
	if _, err = s.BeginBillingProviderCreate(t.Context(), renewal.ID, now); err != nil {
		t.Fatal(err)
	}
	const paymentID = "synthetic-prior-provider-payment"
	if _, err = s.AttachBillingProviderPayment(t.Context(), renewal.ID, paymentID, "", now); err != nil {
		t.Fatal(err)
	}
	complimentaryOperation(t, s, "grant")
	worker, err := s.ListBillingAttemptsForWorker(t.Context(), now.Add(time.Minute), 20)
	if err != nil || len(worker) != 1 || worker[0].ID != renewal.ID || worker[0].Status != "pending" || worker[0].ProviderPaymentID != paymentID {
		t.Fatalf("accepted payment was lost under grant: %+v %v", worker, err)
	}
	processed, err := s.ReconcileBillingPayment(t.Context(), "payment.succeeded", billingTestDedupe(paymentID), BillingCanonicalPayment{
		ProviderPaymentID: paymentID, Status: "succeeded", Paid: true,
		AmountMinor: renewal.AmountMinor, CurrencyCode: renewal.CurrencyCode,
		PaymentMethodID: "synthetic-real-method", PaymentMethodSaved: true,
		MetadataAttemptID: renewal.ID, MetadataWorkspaceID: team.ID, OccurredAt: now.Add(time.Minute),
	}, now.Add(time.Minute))
	if err != nil || !processed {
		t.Fatalf("prior accepted payment not reconciled: processed=%v err=%v", processed, err)
	}
	finished, err := s.GetBillingPaymentAttempt(t.Context(), renewal.ID)
	if err != nil || finished.Status != "succeeded" || finished.ProviderPaymentID != paymentID {
		t.Fatalf("real payment proof not retained: %+v %v", finished, err)
	}
	state, err := s.GetWorkspaceBillingState(t.Context(), "comp-owner", team.ID, now.Add(time.Minute))
	if err != nil || !state.ComplimentaryAccess || state.Subscription.Plan.Code != "solo" || state.Contract == nil {
		t.Fatalf("real paid contract or independent grant was erased: %+v %v", state, err)
	}
	if _, err = s.BeginBillingProviderCreate(t.Context(), renewal.ID, now.Add(time.Minute)); !errors.Is(err, ErrBillingComplimentaryAccess) {
		t.Fatalf("grant allowed a new provider create after reconciliation: %v", err)
	}
}

func TestComplimentaryAccessSurfacesAmbiguousSubmittedPaymentWithoutRetryOrLosingLateWebhook(t *testing.T) {
	t.Parallel()
	s, team, now := complimentaryStoreFixture(t)
	seedBillingContract(t, s, team.ID, "solo", now.AddDate(0, -1, 0), now, "synthetic-real-method")
	renewal, _, err := s.PrepareBillingRenewal(t.Context(), team.ID, now)
	if err != nil || renewal == nil {
		t.Fatalf("pre-grant renewal: %+v %v", renewal, err)
	}
	if _, err = s.BeginBillingProviderCreate(t.Context(), renewal.ID, now); err != nil {
		t.Fatal(err)
	}
	// The provider may have accepted the one POST before its response timed
	// out. Retain that boundary, but never POST again after the grant.
	if err = s.DeferBillingAttempt(t.Context(), renewal.ID, "provider_create_retry", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	complimentaryOperation(t, s, "grant")
	worker, err := s.ListBillingAttemptsForWorker(t.Context(), now.Add(time.Minute), 20)
	if err != nil || len(worker) != 0 {
		t.Fatalf("ambiguous submitted create retried under grant: %+v %v", worker, err)
	}
	if _, err = s.BeginBillingProviderCreate(t.Context(), renewal.ID, now.Add(time.Minute)); !errors.Is(err, ErrBillingComplimentaryAccess) {
		t.Fatalf("direct retry crossed grant cutoff: %v", err)
	}
	count, err := s.CountManualReviewBillingAttempts(t.Context())
	if err != nil || count != 1 {
		t.Fatalf("grant-suppressed unknown payment invisible to review: count=%d err=%v", count, err)
	}
	unknown, err := s.GetBillingPaymentAttempt(t.Context(), renewal.ID)
	if err != nil || unknown.Status != "prepared" || unknown.ProviderPaymentID != "" || unknown.ProviderCreateStartedAt == nil || unknown.CreateAttempts != 1 {
		t.Fatalf("review observation destroyed the provider boundary: %+v %v", unknown, err)
	}
	const paymentID = "synthetic-late-canonical-payment"
	processed, err := s.ReconcileBillingPayment(t.Context(), "payment.succeeded", billingTestDedupe(paymentID), BillingCanonicalPayment{
		ProviderPaymentID: paymentID, Status: "succeeded", Paid: true,
		AmountMinor: renewal.AmountMinor, CurrencyCode: renewal.CurrencyCode,
		PaymentMethodID: "synthetic-real-method", PaymentMethodSaved: true,
		MetadataAttemptID: renewal.ID, MetadataWorkspaceID: team.ID, OccurredAt: now.Add(time.Minute),
	}, now.Add(time.Minute))
	if err != nil || !processed {
		t.Fatalf("late authoritative webhook not reconciled: processed=%v err=%v", processed, err)
	}
	finished, err := s.GetBillingPaymentAttempt(t.Context(), renewal.ID)
	if err != nil || finished.Status != "succeeded" || finished.ProviderPaymentID != paymentID || finished.CreateAttempts != 1 {
		t.Fatalf("late provider proof or no-retry boundary lost: %+v %v", finished, err)
	}
	count, err = s.CountManualReviewBillingAttempts(t.Context())
	if err != nil || count != 0 {
		t.Fatalf("resolved payment still counted as unknown: %d %v", count, err)
	}
}

func TestComplimentaryAccessKeepsTechnicalAIConcurrencyAndDailyLimits(t *testing.T) {
	t.Parallel()
	s, team, now := complimentaryStoreFixture(t)
	complimentaryOperation(t, s, "grant")
	limits := AILimits{PerMinute: 1, PerDay: 1, MaxConcurrent: 1, LeaseTTL: time.Minute}
	for _, personal := range []bool{true, false} {
		var lease AILease
		var err error
		acquire := func() (AILease, error) {
			if personal {
				return s.AcquireAILeaseWithMonthlyUsage(t.Context(), "comp-owner", AIOperationImage, UsageMetricAIImageCredits, 9, true, limits, now)
			}
			return s.AcquireWorkspaceAILeaseWithMonthlyUsage(t.Context(), team.ID, AIOperationImage, UsageMetricAIImageCredits, 9, true, limits, now)
		}
		lease, err = acquire()
		if err != nil {
			t.Fatalf("granted lease: %v", err)
		}
		if _, err = acquire(); err == nil {
			t.Fatal("grant disabled technical concurrency")
		} else {
			var limited *AILimitError
			if !errors.As(err, &limited) || limited.Reason != AILimitReasonConcurrency {
				t.Fatalf("wrong concurrency outcome: %v", err)
			}
		}
		if personal {
			err = s.ReleaseAILease(t.Context(), "comp-owner", lease.ID)
		} else {
			err = s.ReleaseWorkspaceAILease(t.Context(), team.ID, lease.ID)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err = acquire(); err == nil {
			t.Fatal("grant disabled technical daily limit")
		} else {
			var limited *AILimitError
			if !errors.As(err, &limited) || limited.Reason != AILimitReasonDay {
				t.Fatalf("wrong daily outcome: %v", err)
			}
		}
	}
}

func TestComplimentaryAccessIsIndependentOfInactiveOrExpiredBaseSubscription(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		paid   bool
		status string
	}{
		{name: "paused Free", status: "paused"},
		{name: "canceled Free", status: "canceled"},
		{name: "expired paid", paid: true, status: "canceled"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			s, team, now := complimentaryStoreFixture(t)
			if fixture.paid {
				seedBillingContract(t, s, team.ID, "solo", now.AddDate(0, -2, 0), now.AddDate(0, -1, 0), "synthetic-expired-method")
				if _, err := s.db.ExecContext(t.Context(), `UPDATE billing_subscription_contracts SET status='ended' WHERE workspace_id=$1`, team.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.UpdateWorkspaceSubscriptionStatus(t.Context(), team.ID, fixture.status, now); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ChargeWorkspaceMonthlyUsage(t.Context(), team.ID, UsageMetricAIImageCredits, 1, true, now); err == nil {
				t.Fatal("inactive baseline accepted AI charge")
			}
			complimentaryOperation(t, s, "grant")
			usage, err := s.ChargeWorkspaceMonthlyUsage(t.Context(), team.ID, UsageMetricAIImageCredits, 1000, true, now)
			start, end := workspaceMonthlyUsagePeriod(now)
			if err != nil || usage.Quantity != 1000 || !usage.PeriodStart.Equal(start) || !usage.PeriodEnd.Equal(end) {
				t.Fatalf("grant did not use a real calendar ledger: %+v %v", usage, err)
			}
			state, err := s.GetWorkspaceBillingState(t.Context(), "comp-owner", team.ID, now)
			if err != nil || !state.ComplimentaryAccess || state.Subscription.Status != fixture.status || !state.Features.AIImages {
				t.Fatalf("grant mutated or depended on base subscription: %+v %v", state, err)
			}
			if _, err = s.CreateChannelForWorkspace(t.Context(), "comp-owner", team.ID, Channel{Title: "Allowed under personal grant", MAXChatID: "-777001", IsChannel: true, Active: true}); err != nil {
				t.Fatalf("inactive base subscription blocked granted resources: %v", err)
			}
			complimentaryOperation(t, s, "revoke")
			if _, err = s.ChargeWorkspaceMonthlyUsage(t.Context(), team.ID, UsageMetricAIImageCredits, 1, true, now); err == nil {
				t.Fatal("revoked grant retained inactive-subscription bypass")
			}
		})
	}
}
