package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"maxpilot/backend/internal/legal"
)

const (
	billingCheckoutTTL             = 30 * time.Minute
	billingRetentionTTL            = 30 * time.Minute
	billingRenewalRetry            = 24 * time.Hour
	billingRenewalGrace            = 3 * 24 * time.Hour
	billingMaxRenewalTrials        = 3
	billingRetentionBPS            = 5000
	billingProviderCreateWindow    = 23 * time.Hour
	billingWorkerLease             = 2 * time.Minute
	BillingRecurringConsentVersion = "yookassa-recurring-v2"
	BillingRecurringTermsVersion   = legal.CurrentTermsVersion
	BillingRecurringTermsURL       = "https://maxposty.ru/terms/"
)

var (
	ErrBillingOwnerRequired            = errors.New("workspace owner is required for billing")
	ErrBillingPlanUnavailable          = errors.New("billing plan is unavailable")
	ErrBillingConflict                 = errors.New("billing state conflict")
	ErrBillingIntentInvalid            = errors.New("billing cancellation intent is invalid")
	ErrBillingConsentRequired          = errors.New("recurring payment consent is required")
	ErrBillingCheckoutSnapshotMismatch = errors.New("billing checkout snapshot does not match the current catalog")
	ErrBillingLegalConsentRequired     = errors.New("current legal document consent is required for billing")
	ErrBillingIntegrity                = errors.New("billing provider integrity check failed")
)

type BillingCheckoutSnapshot struct {
	PlanCode                     string
	PlanVersion                  int
	MonthlyPriceMinor            int64
	CurrencyCode                 string
	RecurringConsent             bool
	RecurringConsentVersion      string
	RecurringConsentTermsVersion string
}

type BillingPaymentAttempt struct {
	ID                      string
	WorkspaceID             string
	RequestedByUserID       string
	PeriodID                *int64
	Purpose                 string
	AttemptNumber           int
	IdempotencyKey          string
	ProviderPaymentID       string
	PlanCode                string
	PlanVersion             int
	AmountMinor             int64
	CurrencyCode            string
	Status                  string
	ConfirmationURL         string
	RequestedPeriodStart    *time.Time
	RequestedPeriodEnd      *time.Time
	ErrorCode               string
	ProviderDescription     string
	ProviderReturnURL       string
	PaymentMethodSnapshot   string
	DiscountBasisPoints     int
	ProviderCreateStartedAt *time.Time
	CreateDeadline          time.Time
	NextAttemptAt           time.Time
	CreateAttempts          int
	StatusCheckAttempts     int
	WorkerLeaseToken        string
	WorkerLeaseUntil        *time.Time
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

type BillingCancellationIntent struct {
	Token                 string
	WorkspaceID           string
	CurrentPeriodEnd      time.Time
	RegularPriceResumesAt time.Time
	DiscountPercent       int
	Eligible              bool
	ExpiresAt             time.Time
}

type BillingCanonicalPayment struct {
	ProviderPaymentID   string
	Status              string
	Paid                bool
	AmountMinor         int64
	CurrencyCode        string
	PaymentMethodID     string
	PaymentMethodSaved  bool
	MetadataAttemptID   string
	MetadataWorkspaceID string
	OccurredAt          time.Time
	Test                bool
	CancellationReason  string
}

type BillingDueContract struct {
	WorkspaceID       string
	CurrentPeriodEnd  time.Time
	CancelAtPeriodEnd bool
}

func (s *Store) GetAvailableBillingPlan(ctx context.Context, planCode string) (BillingCatalogEntry, error) {
	planCode = strings.TrimSpace(planCode)
	if planCode == "" {
		return BillingCatalogEntry{}, ErrBillingPlanUnavailable
	}
	var entry BillingCatalogEntry
	err := s.db.QueryRowContext(ctx, `SELECT `+billingPlanColumns+`
FROM billing_plan_versions p
WHERE p.plan_code=$1 AND p.public=TRUE AND p.available=TRUE`, planCode).Scan(
		&entry.Plan.Code, &entry.Plan.Version, &entry.Plan.CatalogVersion, &entry.Plan.Name,
		&entry.Plan.Description, &entry.Plan.CurrencyCode, &entry.Plan.MonthlyPriceMinor,
		&entry.Plan.BillingInterval, &entry.Plan.Public, &entry.Plan.Available,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return BillingCatalogEntry{}, ErrBillingPlanUnavailable
	}
	if err != nil {
		return BillingCatalogEntry{}, fmt.Errorf("read available billing plan: %w", err)
	}
	entry.Entitlements, err = readBillingEntitlements(ctx, s.db, entry.Plan.Code, entry.Plan.Version)
	if err != nil {
		return BillingCatalogEntry{}, err
	}
	return entry, nil
}

func (s *Store) CreateBillingCheckoutAttempt(
	ctx context.Context, actorUserID, workspaceID string, snapshot BillingCheckoutSnapshot,
	providerReturnURL string, now time.Time,
) (BillingPaymentAttempt, error) {
	providerReturnURL = strings.TrimSpace(providerReturnURL)
	if actorUserID == "" || workspaceID == "" || providerReturnURL == "" || now.IsZero() {
		return BillingPaymentAttempt{}, errors.New("billing actor, workspace and time are required")
	}
	planCode := strings.TrimSpace(snapshot.PlanCode)
	if planCode == "free" || planCode == "" {
		return BillingPaymentAttempt{}, ErrBillingPlanUnavailable
	}
	if !snapshot.RecurringConsent {
		return BillingPaymentAttempt{}, ErrBillingConsentRequired
	}
	snapshot.CurrencyCode = strings.TrimSpace(snapshot.CurrencyCode)
	snapshot.RecurringConsentVersion = strings.TrimSpace(snapshot.RecurringConsentVersion)
	snapshot.RecurringConsentTermsVersion = strings.TrimSpace(snapshot.RecurringConsentTermsVersion)
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BillingPaymentAttempt{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		"maxstudio:billing:"+workspaceID); err != nil {
		return BillingPaymentAttempt{}, err
	}
	if err := requireBillingOwner(ctx, tx, actorUserID, workspaceID); err != nil {
		return BillingPaymentAttempt{}, err
	}
	var currentTermsAccepted, currentPersonalDataAccepted bool
	if err := tx.QueryRowContext(ctx, `SELECT
EXISTS(SELECT 1 FROM user_consents WHERE owner_id=$1 AND document='terms' AND version=$2),
EXISTS(SELECT 1 FROM user_consents WHERE owner_id=$1 AND document='personal_data' AND version=$3)`,
		actorUserID, legal.CurrentTermsVersion, legal.CurrentPersonalDataVersion).
		Scan(&currentTermsAccepted, &currentPersonalDataAccepted); err != nil {
		return BillingPaymentAttempt{}, err
	}
	if !currentTermsAccepted || !currentPersonalDataAccepted {
		return BillingPaymentAttempt{}, ErrBillingLegalConsentRequired
	}
	var contractStatus string
	err = tx.QueryRowContext(ctx, `SELECT status FROM billing_subscription_contracts WHERE workspace_id=$1`, workspaceID).
		Scan(&contractStatus)
	if err == nil && contractStatus != "ended" {
		return BillingPaymentAttempt{}, ErrBillingConflict
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return BillingPaymentAttempt{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE billing_payment_attempts
SET status='failed',error_code='checkout_expired',updated_at=$2
WHERE workspace_id=$1 AND purpose='checkout' AND status='prepared'
  AND provider_payment_id IS NULL AND provider_create_started_at IS NULL AND created_at<=$3`,
		workspaceID, now, now.Add(-billingCheckoutTTL)); err != nil {
		return BillingPaymentAttempt{}, err
	}
	var plan BillingPlan
	err = tx.QueryRowContext(ctx, `SELECT `+billingPlanColumns+`
FROM billing_plan_versions p
WHERE p.plan_code=$1 AND p.public=TRUE AND p.available=TRUE`, planCode).Scan(
		&plan.Code, &plan.Version, &plan.CatalogVersion, &plan.Name, &plan.Description,
		&plan.CurrencyCode, &plan.MonthlyPriceMinor, &plan.BillingInterval, &plan.Public, &plan.Available)
	if errors.Is(err, sql.ErrNoRows) || plan.MonthlyPriceMinor <= 0 {
		return BillingPaymentAttempt{}, ErrBillingPlanUnavailable
	}
	if err != nil {
		return BillingPaymentAttempt{}, err
	}
	consent := billingRecurringConsent(plan)
	if snapshot.PlanVersion != plan.Version ||
		snapshot.MonthlyPriceMinor != plan.MonthlyPriceMinor ||
		snapshot.CurrencyCode != plan.CurrencyCode ||
		snapshot.RecurringConsentVersion != consent.Version ||
		snapshot.RecurringConsentTermsVersion != consent.TermsVersion {
		return BillingPaymentAttempt{}, ErrBillingCheckoutSnapshotMismatch
	}
	openAttempt, openErr := scanBillingPaymentAttempt(tx.QueryRowContext(ctx,
		billingAttemptSelect+` WHERE a.workspace_id=$1 AND a.purpose='checkout'
AND a.status IN ('prepared','pending','manual_review') FOR UPDATE`, workspaceID))
	if openErr == nil {
		if openAttempt.RequestedByUserID != actorUserID {
			return BillingPaymentAttempt{}, ErrBillingConflict
		}
		var acceptedConsentVersion, acceptedTermsVersion, acceptedCurrencyCode string
		var acceptedPlanVersion int
		var acceptedMonthlyPriceMinor int64
		err = tx.QueryRowContext(ctx, `SELECT consent_version,terms_version,plan_version,
monthly_price_minor,currency_code FROM billing_recurring_consents WHERE payment_attempt_id=$1`,
			openAttempt.ID).Scan(&acceptedConsentVersion, &acceptedTermsVersion, &acceptedPlanVersion,
			&acceptedMonthlyPriceMinor, &acceptedCurrencyCode)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return BillingPaymentAttempt{}, err
		}
		if errors.Is(err, sql.ErrNoRows) ||
			openAttempt.PlanCode != planCode ||
			openAttempt.PlanVersion != snapshot.PlanVersion ||
			openAttempt.AmountMinor != snapshot.MonthlyPriceMinor ||
			openAttempt.CurrencyCode != snapshot.CurrencyCode ||
			acceptedConsentVersion != snapshot.RecurringConsentVersion ||
			acceptedTermsVersion != snapshot.RecurringConsentTermsVersion ||
			acceptedPlanVersion != snapshot.PlanVersion ||
			acceptedMonthlyPriceMinor != snapshot.MonthlyPriceMinor ||
			acceptedCurrencyCode != snapshot.CurrencyCode {
			return BillingPaymentAttempt{}, ErrBillingCheckoutSnapshotMismatch
		}
		if err := tx.Commit(); err != nil {
			return BillingPaymentAttempt{}, err
		}
		return openAttempt, nil
	}
	if !errors.Is(openErr, ErrNotFound) {
		return BillingPaymentAttempt{}, openErr
	}
	attempt, err := newBillingPaymentAttempt(actorUserID, workspaceID, "checkout", plan, 1, nil, nil, 0, now)
	if err != nil {
		return BillingPaymentAttempt{}, err
	}
	attempt.ProviderReturnURL = providerReturnURL
	if err := insertBillingPaymentAttempt(ctx, tx, attempt); err != nil {
		if isUniqueViolation(err) {
			return BillingPaymentAttempt{}, ErrBillingConflict
		}
		return BillingPaymentAttempt{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO billing_recurring_consents(
payment_attempt_id,workspace_id,actor_user_id,consent_version,consent_text,terms_version,terms_url,
plan_code,plan_version,monthly_price_minor,currency_code,accepted_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, attempt.ID, workspaceID, actorUserID,
		consent.Version, consent.Text, consent.TermsVersion, consent.TermsURL,
		plan.Code, plan.Version, plan.MonthlyPriceMinor, plan.CurrencyCode, now); err != nil {
		return BillingPaymentAttempt{}, fmt.Errorf("record recurring payment consent: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return BillingPaymentAttempt{}, err
	}
	return attempt, nil
}

func (s *Store) AttachBillingProviderPayment(
	ctx context.Context, attemptID, providerPaymentID, confirmationURL string, now time.Time,
) (BillingPaymentAttempt, error) {
	if attemptID == "" || providerPaymentID == "" || now.IsZero() {
		return BillingPaymentAttempt{}, errors.New("billing attempt, provider payment and time are required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE billing_payment_attempts
SET provider_payment_id=$2,confirmation_url=$3,status='pending',error_code='',
    payment_method_snapshot='',status_check_attempts=0,next_attempt_at=$5,
    worker_lease_token='',worker_lease_until=NULL,updated_at=$4
WHERE id=$1 AND status IN ('prepared','pending')
  AND (provider_payment_id IS NULL OR provider_payment_id=$2)`,
		attemptID, providerPaymentID, confirmationURL, now.UTC(), now.UTC().Add(30*time.Second))
	if err != nil {
		return BillingPaymentAttempt{}, fmt.Errorf("attach YooKassa payment: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		current, readErr := s.GetBillingPaymentAttempt(ctx, attemptID)
		if readErr == nil && current.ProviderPaymentID == providerPaymentID &&
			(current.Status == "pending" || current.Status == "succeeded" || current.Status == "canceled") {
			return current, nil
		}
		return BillingPaymentAttempt{}, ErrBillingConflict
	}
	return s.GetBillingPaymentAttempt(ctx, attemptID)
}

// QuarantineBillingProviderPayment preserves the provider ID for operator
// reconciliation without treating a mismatched create response as a normal
// pending payment or exposing its confirmation URL.
func (s *Store) QuarantineBillingProviderPayment(
	ctx context.Context, attemptID, providerPaymentID, errorCode string, now time.Time,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var workspaceID string
	if err := tx.QueryRowContext(ctx, `SELECT workspace_id FROM billing_payment_attempts WHERE id=$1`, attemptID).
		Scan(&workspaceID); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		"maxstudio:billing:"+workspaceID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE billing_payment_attempts SET
provider_payment_id=$2,status='manual_review',
error_code=CASE WHEN status='manual_review' AND error_code NOT IN ('','provider_integrity_failed','provider_create_outcome_unknown')
                THEN error_code ELSE $3 END,
next_attempt_at=$4,payment_method_snapshot='',
worker_lease_token='',worker_lease_until=NULL,updated_at=$4
WHERE id=$1 AND status IN ('prepared','manual_review')
  AND (provider_payment_id IS NULL OR provider_payment_id=$2)`,
		attemptID, providerPaymentID, errorCode, now.UTC())
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrBillingConflict
	}
	return tx.Commit()
}

func (s *Store) GetBillingPaymentAttempt(ctx context.Context, attemptID string) (BillingPaymentAttempt, error) {
	return scanBillingPaymentAttempt(s.db.QueryRowContext(ctx, billingAttemptSelect+` WHERE a.id=$1`, attemptID))
}

func (s *Store) GetBillingPaymentAttemptByProviderID(ctx context.Context, providerID string) (BillingPaymentAttempt, error) {
	return scanBillingPaymentAttempt(s.db.QueryRowContext(ctx, billingAttemptSelect+` WHERE a.provider_payment_id=$1`, providerID))
}

func (s *Store) ReconcileBillingPayment(
	ctx context.Context, eventType, dedupeKey string, payment BillingCanonicalPayment, now time.Time,
) (bool, error) {
	if eventType == "" || dedupeKey == "" || payment.ProviderPaymentID == "" || now.IsZero() {
		return false, errors.New("billing reconciliation fields are required")
	}
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var priorResult string
	if err := tx.QueryRowContext(ctx, `SELECT result FROM billing_webhook_receipts WHERE dedupe_key=$1`,
		dedupeKey).Scan(&priorResult); err == nil {
		if priorResult == "failed" {
			return false, ErrBillingIntegrity
		}
		return false, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if payment.Test {
		if err := insertBillingReceipt(ctx, tx, dedupeKey, eventType, payment.ProviderPaymentID, "ignored", now); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	// Discover the tenant without locking an attempt, then take the shared
	// workspace advisory lock before any billing row lock. This lock order is
	// also used by cancellation, detach, renewal creation and ownership changes.
	var discoveredAttemptID, discoveredWorkspaceID string
	err = tx.QueryRowContext(ctx, `SELECT id,workspace_id FROM billing_payment_attempts
WHERE provider_payment_id=$1 OR ($2<>'' AND id=$2)
ORDER BY (provider_payment_id=$1) DESC LIMIT 1`,
		payment.ProviderPaymentID, payment.MetadataAttemptID).Scan(&discoveredAttemptID, &discoveredWorkspaceID)
	if errors.Is(err, sql.ErrNoRows) {
		if err := insertBillingReceipt(ctx, tx, dedupeKey, eventType, payment.ProviderPaymentID, "ignored", now); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		"maxstudio:billing:"+discoveredWorkspaceID); err != nil {
		return false, err
	}
	attempt, err := scanBillingPaymentAttempt(tx.QueryRowContext(ctx,
		billingAttemptSelect+` WHERE a.provider_payment_id=$1 FOR UPDATE`, payment.ProviderPaymentID))
	if errors.Is(err, ErrNotFound) && payment.MetadataAttemptID != "" {
		attempt, err = scanBillingPaymentAttempt(tx.QueryRowContext(ctx,
			billingAttemptSelect+` WHERE a.id=$1 FOR UPDATE`, payment.MetadataAttemptID))
		if err == nil && attempt.ProviderPaymentID != "" && attempt.ProviderPaymentID != payment.ProviderPaymentID {
			return commitBillingIntegrityFailure(ctx, tx, dedupeKey, eventType, payment.ProviderPaymentID,
				"attempt is attached to another provider payment", now)
		}
		if err == nil {
			if attempt.ID != discoveredAttemptID || attempt.WorkspaceID != discoveredWorkspaceID ||
				payment.MetadataAttemptID != attempt.ID || payment.MetadataWorkspaceID != attempt.WorkspaceID ||
				payment.AmountMinor != attempt.AmountMinor || payment.CurrencyCode != attempt.CurrencyCode {
				return commitBillingIntegrityFailure(ctx, tx, dedupeKey, eventType, payment.ProviderPaymentID,
					"unbound canonical payment does not match local attempt", now)
			}
			if _, updateErr := tx.ExecContext(ctx, `UPDATE billing_payment_attempts
SET provider_payment_id=$2,status=CASE WHEN status='prepared' THEN 'pending' ELSE status END,
payment_method_snapshot='',updated_at=$3
WHERE id=$1`, attempt.ID, payment.ProviderPaymentID, now); updateErr != nil {
				return false, updateErr
			}
			attempt.ProviderPaymentID = payment.ProviderPaymentID
		}
	}
	if errors.Is(err, ErrNotFound) {
		if err := insertBillingReceipt(ctx, tx, dedupeKey, eventType, payment.ProviderPaymentID, "ignored", now); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	if attempt.ID != discoveredAttemptID || attempt.WorkspaceID != discoveredWorkspaceID {
		return commitBillingIntegrityFailure(ctx, tx, dedupeKey, eventType, payment.ProviderPaymentID,
			"payment attempt changed during reconciliation", now)
	}
	if payment.MetadataAttemptID != attempt.ID || payment.MetadataWorkspaceID != attempt.WorkspaceID ||
		payment.AmountMinor != attempt.AmountMinor ||
		payment.CurrencyCode != attempt.CurrencyCode {
		return commitBillingIntegrityFailure(ctx, tx, dedupeKey, eventType, payment.ProviderPaymentID,
			"canonical YooKassa payment does not match local attempt", now)
	}
	processed := false
	switch payment.Status {
	case "succeeded":
		if !payment.Paid {
			return commitBillingIntegrityFailure(ctx, tx, dedupeKey, eventType, payment.ProviderPaymentID,
				"succeeded payment is not marked paid", now)
		}
		if attempt.Status != "succeeded" {
			if attempt.Purpose == "renewal" {
				current, currentErr := billingRenewalStillCurrentTx(ctx, tx, attempt)
				if currentErr != nil {
					return false, currentErr
				}
				if !current {
					return commitStalePaidRenewal(
						ctx, tx, attempt, dedupeKey, eventType, payment.ProviderPaymentID, now,
					)
				}
			}
			if err := activateBillingAttempt(ctx, tx, attempt, payment, now); err != nil {
				return false, err
			}
			processed = true
		}
	case "canceled":
		if attempt.Status != "canceled" && attempt.Status != "succeeded" {
			if err := cancelBillingAttempt(ctx, tx, attempt, payment.CancellationReason, now); err != nil {
				return false, err
			}
			processed = true
		}
	case "pending", "waiting_for_capture":
		// Release the worker lease and poll the canonical object again later.
		if _, err := tx.ExecContext(ctx, `UPDATE billing_payment_attempts SET
next_attempt_at=$2,status_check_attempts=0,worker_lease_token='',worker_lease_until=NULL,updated_at=$3
WHERE id=$1 AND status='pending'`, attempt.ID, now.Add(5*time.Minute), now); err != nil {
			return false, err
		}
	default:
		return false, fmt.Errorf("%w: unsupported canonical payment status", ErrBillingConflict)
	}
	if err := insertBillingReceipt(ctx, tx, dedupeKey, eventType, payment.ProviderPaymentID, "processed", now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return processed, nil
}

func (s *Store) CreateBillingCancellationIntent(
	ctx context.Context, actorUserID, workspaceID string, now time.Time,
) (BillingCancellationIntent, error) {
	if now.IsZero() {
		return BillingCancellationIntent{}, errors.New("billing cancellation time is required")
	}
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BillingCancellationIntent{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		"maxstudio:billing:"+workspaceID); err != nil {
		return BillingCancellationIntent{}, err
	}
	if err := requireBillingOwner(ctx, tx, actorUserID, workspaceID); err != nil {
		return BillingCancellationIntent{}, err
	}
	var periodID int64
	var periodEnd time.Time
	var retentionUsed, cancelAtEnd bool
	err = tx.QueryRowContext(ctx, `SELECT p.id,p.period_end,c.retention_offer_used,c.cancel_at_period_end
FROM billing_subscription_contracts c
JOIN billing_subscription_periods p ON p.id=c.current_period_id
WHERE c.workspace_id=$1 AND c.status IN ('active','past_due') FOR UPDATE OF c`, workspaceID).
		Scan(&periodID, &periodEnd, &retentionUsed, &cancelAtEnd)
	if errors.Is(err, sql.ErrNoRows) || cancelAtEnd {
		return BillingCancellationIntent{}, ErrBillingConflict
	}
	if err != nil {
		return BillingCancellationIntent{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE billing_retention_offers
SET status='expired',consumed_at=$2
WHERE workspace_id=$1 AND status='pending'`, workspaceID, now); err != nil {
		return BillingCancellationIntent{}, err
	}
	token, hash, err := randomBillingToken()
	if err != nil {
		return BillingCancellationIntent{}, err
	}
	expiresAt := now.Add(billingRetentionTTL)
	if _, err := tx.ExecContext(ctx, `INSERT INTO billing_retention_offers(
token_hash,workspace_id,requested_by_user_id,subscription_period_id,status,discount_basis_points,created_at,expires_at)
VALUES($1,$2,$3,$4,'pending',$5,$6,$7)`, hash, workspaceID, actorUserID, periodID,
		billingRetentionBPS, now, expiresAt); err != nil {
		return BillingCancellationIntent{}, err
	}
	if err := tx.Commit(); err != nil {
		return BillingCancellationIntent{}, err
	}
	return BillingCancellationIntent{
		Token: token, WorkspaceID: workspaceID, CurrentPeriodEnd: periodEnd.UTC(),
		RegularPriceResumesAt: addBillingMonth(periodEnd.UTC()),
		DiscountPercent:       50, Eligible: !retentionUsed, ExpiresAt: expiresAt,
	}, nil
}

func (s *Store) AcceptBillingRetentionOffer(
	ctx context.Context, actorUserID, workspaceID, tokenHash string, now time.Time,
) error {
	return s.consumeBillingIntent(ctx, actorUserID, workspaceID, tokenHash, "accepted", now)
}

func (s *Store) ConfirmBillingCancellation(
	ctx context.Context, actorUserID, workspaceID, tokenHash string, now time.Time,
) error {
	return s.consumeBillingIntent(ctx, actorUserID, workspaceID, tokenHash, "cancel_confirmed", now)
}

func (s *Store) ResumeBillingSubscription(ctx context.Context, actorUserID, workspaceID string, now time.Time) error {
	if now.IsZero() {
		return errors.New("billing resume time is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		"maxstudio:billing:"+workspaceID); err != nil {
		return err
	}
	if err := requireBillingOwner(ctx, tx, actorUserID, workspaceID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE billing_subscription_contracts
SET cancel_at_period_end=FALSE,version=version+1,updated_at=$2
WHERE workspace_id=$1 AND status IN ('active','past_due') AND cancel_at_period_end=TRUE
  AND payment_method_id<>''`, workspaceID, now.UTC())
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrBillingConflict
	}
	return tx.Commit()
}

// DetachBillingPaymentMethod irreversibly forgets the locally saved recurring
// payment reference while preserving access through the already paid period.
// Repeating the call is a no-op, and audit metadata never contains the ID.
func (s *Store) DetachBillingPaymentMethod(
	ctx context.Context, actorUserID, workspaceID string, now time.Time,
) error {
	if now.IsZero() {
		return errors.New("billing payment method detach time is required")
	}
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		"maxstudio:billing:"+workspaceID); err != nil {
		return err
	}
	if err := requireBillingOwner(ctx, tx, actorUserID, workspaceID); err != nil {
		return err
	}
	var paymentMethodID, status string
	var cancelAtEnd bool
	var periodEnd time.Time
	err = tx.QueryRowContext(ctx, `SELECT c.payment_method_id,c.status,c.cancel_at_period_end,p.period_end
FROM billing_subscription_contracts c
JOIN billing_subscription_periods p ON p.id=c.current_period_id
WHERE c.workspace_id=$1 AND c.status IN ('active','past_due') FOR UPDATE OF c`, workspaceID).
		Scan(&paymentMethodID, &status, &cancelAtEnd, &periodEnd)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrBillingConflict
	}
	if err != nil {
		return err
	}
	if err := rejectUnboundSubmittedBillingRenewal(ctx, tx, workspaceID); err != nil {
		return err
	}
	if paymentMethodID == "" && cancelAtEnd {
		return tx.Commit()
	}
	if err := suppressUnsubmittedBillingRenewals(ctx, tx, workspaceID, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE billing_retention_offers
SET status='expired',consumed_at=$2 WHERE workspace_id=$1 AND status='pending'`, workspaceID, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE billing_subscription_contracts SET
payment_method_id='',cancel_at_period_end=TRUE,next_charge_at=$2,
next_period_discount_basis_points=0,version=version+1,updated_at=$3
WHERE workspace_id=$1`, workspaceID, periodEnd.UTC(), now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE billing_payment_attempts SET
payment_method_snapshot='',updated_at=$2 WHERE workspace_id=$1 AND purpose='renewal'`, workspaceID, now); err != nil {
		return err
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID,
		Action: "billing.payment_method_detached", EntityType: "workspace",
		EntityID: workspaceID, Metadata: []byte(`{}`), CreatedAt: now,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListDueBillingContracts(ctx context.Context, now time.Time, limit int) ([]BillingDueContract, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT c.workspace_id,p.period_end,c.cancel_at_period_end
FROM billing_subscription_contracts c
JOIN billing_subscription_periods p ON p.id=c.current_period_id
JOIN workspaces w ON w.id=c.workspace_id AND w.archived_at IS NULL
WHERE c.status IN ('active','past_due') AND c.next_charge_at<=$1
ORDER BY c.next_charge_at,c.workspace_id LIMIT $2`, now.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]BillingDueContract, 0)
	for rows.Next() {
		var item BillingDueContract
		if err := rows.Scan(&item.WorkspaceID, &item.CurrentPeriodEnd, &item.CancelAtPeriodEnd); err != nil {
			return nil, err
		}
		item.CurrentPeriodEnd = item.CurrentPeriodEnd.UTC()
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) CountManualReviewBillingAttempts(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM billing_payment_attempts
WHERE status='manual_review'`).Scan(&count)
	return count, err
}

// ApplyBillingLifecycleWithoutRenewal keeps the live kill switch safe: it can
// expire canceled/grace-ended access and start a bounded grace window, but it
// never creates a provider POST attempt.
func (s *Store) ApplyBillingLifecycleWithoutRenewal(
	ctx context.Context, workspaceID string, now time.Time,
) (bool, error) {
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		"maxstudio:billing:"+workspaceID); err != nil {
		return false, err
	}
	var status string
	var cancelAtEnd bool
	var attempts int
	var periodID int64
	var periodEnd time.Time
	var grace sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT c.status,c.cancel_at_period_end,c.renewal_attempts,
p.id,p.period_end,c.grace_until FROM billing_subscription_contracts c
JOIN billing_subscription_periods p ON p.id=c.current_period_id AND p.workspace_id=c.workspace_id
WHERE c.workspace_id=$1 AND c.status IN ('active','past_due') FOR UPDATE OF c`, workspaceID).
		Scan(&status, &cancelAtEnd, &attempts, &periodID, &periodEnd, &grace); errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	} else if err != nil {
		return false, err
	}
	if (cancelAtEnd && !now.Before(periodEnd)) ||
		(grace.Valid && !now.Before(grace.Time)) ||
		(status == "past_due" && attempts >= billingMaxRenewalTrials) {
		if err := downgradeBillingWorkspaceTx(ctx, tx, workspaceID, periodID, now); err != nil {
			return false, err
		}
		return true, tx.Commit()
	}
	if !now.Before(periodEnd) && (status == "active" || !grace.Valid) {
		if _, err := tx.ExecContext(ctx, `UPDATE billing_subscription_contracts SET
status='past_due',grace_until=COALESCE(grace_until,$2),next_charge_at=$3,
version=version+1,updated_at=$4 WHERE workspace_id=$1`,
			workspaceID, now.Add(billingRenewalGrace), now.Add(time.Hour), now); err != nil {
			return false, err
		}
	}
	return false, tx.Commit()
}

func (s *Store) PrepareBillingRenewal(
	ctx context.Context, workspaceID string, now time.Time,
) (*BillingPaymentAttempt, bool, error) {
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		"maxstudio:billing:"+workspaceID); err != nil {
		return nil, false, err
	}
	var contract BillingContract
	var periodID int64
	var periodStart, periodEnd time.Time
	var nextCharge, grace sql.NullTime
	var plan BillingPlan
	err = tx.QueryRowContext(ctx, `SELECT c.payer_user_id,c.status,c.payment_method_id,c.cancel_at_period_end,
c.next_charge_at,c.grace_until,c.retention_offer_used,c.next_period_discount_basis_points,
c.renewal_attempts,c.version,p.id,p.period_start,p.period_end,
v.plan_code,v.version,v.catalog_version::text,v.name,v.description,v.currency_code,
v.monthly_price_minor,v.billing_interval,v.public,v.available
FROM billing_subscription_contracts c
JOIN billing_subscription_periods p ON p.id=c.current_period_id
JOIN billing_plan_versions v ON v.plan_code=p.plan_code AND v.version=p.plan_version
WHERE c.workspace_id=$1 AND c.status IN ('active','past_due') FOR UPDATE OF c`, workspaceID).Scan(
		&contract.PayerUserID, &contract.Status, &contract.PaymentMethodID, &contract.CancelAtPeriodEnd,
		&nextCharge, &grace, &contract.RetentionOfferUsed, &contract.NextPeriodDiscountPercent,
		&contract.RenewalAttempts, &contract.Version, &periodID, &periodStart, &periodEnd,
		&plan.Code, &plan.Version, &plan.CatalogVersion, &plan.Name, &plan.Description,
		&plan.CurrencyCode, &plan.MonthlyPriceMinor, &plan.BillingInterval, &plan.Public, &plan.Available)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, err
	}
	if nextCharge.Valid && nextCharge.Time.After(now) {
		return nil, false, nil
	}
	if contract.CancelAtPeriodEnd && !now.Before(periodEnd) {
		if err := downgradeBillingWorkspaceTx(ctx, tx, workspaceID, periodID, now); err != nil {
			return nil, false, err
		}
		return nil, true, tx.Commit()
	}
	if (contract.RenewalAttempts >= billingMaxRenewalTrials && contract.Status == "past_due") ||
		(grace.Valid && !now.Before(grace.Time)) {
		if err := downgradeBillingWorkspaceTx(ctx, tx, workspaceID, periodID, now); err != nil {
			return nil, false, err
		}
		return nil, true, tx.Commit()
	}
	if !now.Before(periodEnd) && (contract.Status == "active" || !grace.Valid) {
		graceUntil := now.Add(billingRenewalGrace)
		if _, err := tx.ExecContext(ctx, `UPDATE billing_subscription_contracts SET
status='past_due',grace_until=COALESCE(grace_until,$2),version=version+1,updated_at=$3
WHERE workspace_id=$1`, workspaceID, graceUntil, now); err != nil {
			return nil, false, err
		}
		contract.Status = "past_due"
		if !grace.Valid {
			grace = sql.NullTime{Time: graceUntil, Valid: true}
		}
	}
	requestedStart := periodEnd.UTC()
	requestedEnd := addBillingMonth(requestedStart)
	openAttempt, err := hasOpenBillingRenewal(ctx, tx, workspaceID)
	if err != nil {
		return nil, false, err
	}
	if openAttempt {
		next := now.Add(time.Hour)
		if grace.Valid && grace.Time.Before(next) {
			next = grace.Time
		}
		if _, err := tx.ExecContext(ctx, `UPDATE billing_subscription_contracts SET
next_charge_at=$2,version=version+1,updated_at=$3 WHERE workspace_id=$1`, workspaceID, next, now); err != nil {
			return nil, false, err
		}
		return nil, false, tx.Commit()
	}
	amount := plan.MonthlyPriceMinor
	discountBPS := contract.NextPeriodDiscountPercent
	if discountBPS > 0 {
		amount = amount * int64(10000-discountBPS) / 10000
	}
	attempt, err := newBillingPaymentAttempt(contract.PayerUserID, workspaceID, "renewal", plan,
		contract.RenewalAttempts+1, &requestedStart, &requestedEnd, discountBPS, now)
	if err != nil {
		return nil, false, err
	}
	attempt.AmountMinor = amount
	attempt.PaymentMethodSnapshot = contract.PaymentMethodID
	if err := insertBillingPaymentAttempt(ctx, tx, attempt); err != nil {
		if isUniqueViolation(err) {
			return nil, false, ErrBillingConflict
		}
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE billing_subscription_contracts
	SET next_charge_at=$2,
	    version=version+1,updated_at=$3 WHERE workspace_id=$1`,
		workspaceID, now.Add(5*time.Minute), now); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return &attempt, false, nil
}

func (s *Store) ListBillingAttemptsForWorker(ctx context.Context, now time.Time, limit int) ([]BillingPaymentAttempt, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	now = now.UTC()
	leaseToken, err := randomBillingHex(16)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `WITH candidates AS (
  SELECT id FROM billing_payment_attempts
  WHERE next_attempt_at<=$1
    AND (worker_lease_until IS NULL OR worker_lease_until<$1)
    AND (
	      status='prepared' OR
      (status='pending' AND provider_payment_id IS NOT NULL)
    )
  ORDER BY next_attempt_at,created_at,id
  FOR UPDATE SKIP LOCKED LIMIT $2
), claimed AS (
  UPDATE billing_payment_attempts a SET worker_lease_token=$3,worker_lease_until=$4,updated_at=$1
  FROM candidates c WHERE a.id=c.id RETURNING a.id
)
`+billingAttemptSelect+` JOIN claimed c ON c.id=a.id ORDER BY a.created_at,a.id`,
		now, limit, leaseToken, now.Add(billingWorkerLease))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]BillingPaymentAttempt, 0)
	for rows.Next() {
		attempt, err := scanBillingPaymentAttempt(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, attempt)
	}
	return result, rows.Err()
}

func (s *Store) ListPendingBillingAttemptsForWorker(
	ctx context.Context, now time.Time, limit int,
) ([]BillingPaymentAttempt, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	now = now.UTC()
	leaseToken, err := randomBillingHex(16)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `WITH candidates AS (
  SELECT id FROM billing_payment_attempts
  WHERE next_attempt_at<=$1 AND (worker_lease_until IS NULL OR worker_lease_until<$1)
    AND status='pending' AND provider_payment_id IS NOT NULL
  ORDER BY next_attempt_at,created_at,id FOR UPDATE SKIP LOCKED LIMIT $2
), claimed AS (
  UPDATE billing_payment_attempts a SET worker_lease_token=$3,worker_lease_until=$4,updated_at=$1
  FROM candidates c WHERE a.id=c.id RETURNING a.id
)
`+billingAttemptSelect+` JOIN claimed c ON c.id=a.id ORDER BY a.created_at,a.id`,
		now, limit, leaseToken, now.Add(billingWorkerLease))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]BillingPaymentAttempt, 0)
	for rows.Next() {
		attempt, scanErr := scanBillingPaymentAttempt(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, attempt)
	}
	return result, rows.Err()
}

// BeginBillingProviderCreate creates a durable boundary before the external
// POST. Cancellation and detach use the same workspace advisory lock, so a
// debit is either clearly before or clearly after their cutoff.
func (s *Store) BeginBillingProviderCreate(
	ctx context.Context, attemptID string, now time.Time,
) (BillingPaymentAttempt, error) {
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BillingPaymentAttempt{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var workspaceID string
	if err := tx.QueryRowContext(ctx, `SELECT workspace_id FROM billing_payment_attempts WHERE id=$1`, attemptID).
		Scan(&workspaceID); errors.Is(err, sql.ErrNoRows) {
		return BillingPaymentAttempt{}, ErrNotFound
	} else if err != nil {
		return BillingPaymentAttempt{}, err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		"maxstudio:billing:"+workspaceID); err != nil {
		return BillingPaymentAttempt{}, err
	}
	attempt, err := scanBillingPaymentAttempt(tx.QueryRowContext(ctx,
		billingAttemptSelect+` WHERE a.id=$1 FOR UPDATE`, attemptID))
	if err != nil {
		return BillingPaymentAttempt{}, err
	}
	if attempt.Status != "prepared" {
		return BillingPaymentAttempt{}, ErrBillingConflict
	}
	if !now.Before(attempt.CreateDeadline) {
		if err := markBillingAttemptManualReviewTx(
			ctx, tx, attempt, "provider_create_deadline_exceeded", now,
		); err != nil {
			return BillingPaymentAttempt{}, err
		}
		if err := tx.Commit(); err != nil {
			return BillingPaymentAttempt{}, err
		}
		return BillingPaymentAttempt{}, ErrBillingConflict
	}
	if attempt.WorkspaceID != workspaceID {
		return BillingPaymentAttempt{}, ErrBillingConflict
	}
	if attempt.Purpose == "renewal" {
		var allowed bool
		if err := tx.QueryRowContext(ctx, `SELECT status IN ('active','past_due')
AND cancel_at_period_end=FALSE AND payment_method_id<>''
FROM billing_subscription_contracts WHERE workspace_id=$1 FOR UPDATE`, attempt.WorkspaceID).Scan(&allowed); err != nil {
			return BillingPaymentAttempt{}, err
		}
		if !allowed {
			return BillingPaymentAttempt{}, ErrBillingConflict
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE billing_payment_attempts SET
provider_create_started_at=COALESCE(provider_create_started_at,$2),create_attempts=create_attempts+1,
updated_at=$2 WHERE id=$1 AND status='prepared'`, attempt.ID, now); err != nil {
		return BillingPaymentAttempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return BillingPaymentAttempt{}, err
	}
	return s.GetBillingPaymentAttempt(ctx, attempt.ID)
}

func (s *Store) DeferBillingAttempt(
	ctx context.Context, attemptID, errorCode string, next time.Time,
) error {
	result, err := s.db.ExecContext(ctx, `UPDATE billing_payment_attempts SET
error_code=$2,next_attempt_at=$3,
status_check_attempts=status_check_attempts+CASE WHEN status='pending' THEN 1 ELSE 0 END,
worker_lease_token='',worker_lease_until=NULL,updated_at=CURRENT_TIMESTAMP
WHERE id=$1 AND status IN ('prepared','pending')`, attemptID, errorCode, next.UTC())
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrBillingConflict
	}
	return nil
}

func (s *Store) FailBillingProviderCreate(
	ctx context.Context, attemptID, errorCode string, manualReview bool, now time.Time,
) error {
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var workspaceID string
	if err := tx.QueryRowContext(ctx, `SELECT workspace_id FROM billing_payment_attempts WHERE id=$1`, attemptID).
		Scan(&workspaceID); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		"maxstudio:billing:"+workspaceID); err != nil {
		return err
	}
	attempt, err := scanBillingPaymentAttempt(tx.QueryRowContext(ctx,
		billingAttemptSelect+` WHERE a.id=$1 FOR UPDATE`, attemptID))
	if err != nil {
		return err
	}
	if (!manualReview && attempt.Status != "prepared") ||
		(manualReview && attempt.Status != "prepared" && attempt.Status != "pending") {
		return ErrBillingConflict
	}
	if manualReview {
		if err := markBillingAttemptManualReviewTx(ctx, tx, attempt, errorCode, now); err != nil {
			return err
		}
	} else {
		result, err := tx.ExecContext(ctx, `UPDATE billing_payment_attempts SET
status='failed',error_code=$2,payment_method_snapshot='',worker_lease_token='',worker_lease_until=NULL,updated_at=$3
WHERE id=$1 AND status='prepared'`, attempt.ID, errorCode, now)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return ErrBillingConflict
		}
		if attempt.Purpose == "renewal" {
			if err := markBillingContractPastDueTx(ctx, tx, attempt, now); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func markBillingAttemptManualReviewTx(
	ctx context.Context, tx *sql.Tx, attempt BillingPaymentAttempt, errorCode string, now time.Time,
) error {
	if strings.TrimSpace(errorCode) == "" {
		errorCode = "provider_create_outcome_unknown"
	}
	result, err := tx.ExecContext(ctx, `UPDATE billing_payment_attempts SET
status='manual_review',error_code=$2,next_attempt_at=$3,
payment_method_snapshot='',worker_lease_token='',worker_lease_until=NULL,updated_at=$3
WHERE id=$1 AND status IN ('prepared','pending')`, attempt.ID, errorCode, now)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrBillingConflict
	}
	if attempt.Purpose == "renewal" {
		_, err = tx.ExecContext(ctx, `UPDATE billing_subscription_contracts SET
next_charge_at=$2,version=version+1,updated_at=$3
WHERE workspace_id=$1 AND status IN ('active','past_due')`,
			attempt.WorkspaceID, now.Add(billingRenewalRetry), now)
	}
	return err
}

func markBillingContractPastDueTx(
	ctx context.Context, tx *sql.Tx, attempt BillingPaymentAttempt, now time.Time,
) error {
	if attempt.Purpose != "renewal" {
		return nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE billing_subscription_contracts SET
status='past_due',renewal_attempts=GREATEST(renewal_attempts,$2),next_charge_at=$3,
grace_until=COALESCE(grace_until,$4),version=version+1,updated_at=$5
WHERE workspace_id=$1 AND status IN ('active','past_due')`, attempt.WorkspaceID,
		attempt.AttemptNumber, now.Add(billingRenewalRetry), now.Add(billingRenewalGrace), now)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrBillingConflict
	}
	return nil
}

func hasOpenBillingRenewal(ctx context.Context, tx *sql.Tx, workspaceID string) (bool, error) {
	var open bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM billing_payment_attempts
WHERE workspace_id=$1 AND purpose='renewal' AND status IN ('prepared','pending','manual_review'))`,
		workspaceID).Scan(&open)
	return open, err
}

// rejectUnboundSubmittedBillingRenewal prevents cancellation or detach from
// claiming success while an external POST may already have created a payment
// that has not yet been durably attached to its local attempt. Once the
// provider ID is attached, reconciliation can safely classify a late result.
func rejectUnboundSubmittedBillingRenewal(ctx context.Context, tx *sql.Tx, workspaceID string) error {
	var unresolved bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM billing_payment_attempts
WHERE workspace_id=$1 AND purpose='renewal'
  AND status IN ('prepared','pending','manual_review')
  AND provider_create_started_at IS NOT NULL
  AND provider_payment_id IS NULL)`, workspaceID).Scan(&unresolved); err != nil {
		return err
	}
	if unresolved {
		return ErrBillingConflict
	}
	return nil
}

// suppressUnsubmittedBillingRenewals establishes the exact cancellation
// cutoff. Only attempts for which no provider POST has begun are suppressed.
// A submitted attempt without an attached provider ID is rejected by
// rejectUnboundSubmittedBillingRenewal before this function is called.
func suppressUnsubmittedBillingRenewals(
	ctx context.Context, tx *sql.Tx, workspaceID string, now time.Time,
) error {
	if _, err := tx.ExecContext(ctx, `UPDATE billing_payment_attempts SET
status='manual_review',error_code='canceled_with_provider_outcome_unknown',
payment_method_snapshot='',worker_lease_token='',worker_lease_until=NULL,updated_at=$2
WHERE workspace_id=$1 AND purpose='renewal' AND status='prepared'
  AND (provider_create_started_at IS NOT NULL OR provider_payment_id IS NOT NULL)`, workspaceID, now); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE billing_payment_attempts SET
status='failed',error_code='canceled_before_submission',payment_method_snapshot='',
worker_lease_token='',worker_lease_until=NULL,updated_at=$2
WHERE workspace_id=$1 AND purpose='renewal' AND status='prepared'
  AND provider_payment_id IS NULL AND provider_create_started_at IS NULL`, workspaceID, now)
	return err
}

// BillingPaymentMethodForAttempt returns the saved provider method only for a
// renewal that still belongs to the workspace's live contract. It is kept out
// of BillingPaymentAttempt so secrets cannot accidentally enter API payloads.
func (s *Store) BillingPaymentMethodForAttempt(ctx context.Context, attemptID string) (string, error) {
	var paymentMethodID string
	err := s.db.QueryRowContext(ctx, `SELECT c.payment_method_id
FROM billing_payment_attempts a
JOIN billing_subscription_contracts c ON c.workspace_id=a.workspace_id
WHERE a.id=$1 AND a.purpose='renewal' AND a.status IN ('prepared','pending')
  AND c.status IN ('active','past_due') AND c.cancel_at_period_end=FALSE`, attemptID).Scan(&paymentMethodID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read saved billing payment method: %w", err)
	}
	if strings.TrimSpace(paymentMethodID) == "" {
		return "", ErrBillingConflict
	}
	return paymentMethodID, nil
}

func (s *Store) consumeBillingIntent(
	ctx context.Context, actorUserID, workspaceID, tokenHash, targetStatus string, now time.Time,
) error {
	if actorUserID == "" || workspaceID == "" || len(tokenHash) != 64 || now.IsZero() {
		return ErrBillingIntentInvalid
	}
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		"maxstudio:billing:"+workspaceID); err != nil {
		return err
	}
	if err := requireBillingOwner(ctx, tx, actorUserID, workspaceID); err != nil {
		return err
	}
	var retentionUsed bool
	var offerActor, status, paymentMethodID string
	var expiresAt time.Time
	err = tx.QueryRowContext(ctx, `SELECT o.requested_by_user_id,o.status,o.expires_at,c.retention_offer_used,c.payment_method_id
FROM billing_retention_offers o
JOIN billing_subscription_contracts c ON c.workspace_id=o.workspace_id
WHERE o.token_hash=$1 AND o.workspace_id=$2 AND c.status IN ('active','past_due')
  AND o.subscription_period_id=c.current_period_id
		FOR UPDATE OF o,c`, tokenHash, workspaceID).Scan(
		&offerActor, &status, &expiresAt, &retentionUsed, &paymentMethodID)
	if errors.Is(err, sql.ErrNoRows) || offerActor != actorUserID || status != "pending" || !now.Before(expiresAt) {
		return ErrBillingIntentInvalid
	}
	if err != nil {
		return err
	}
	switch targetStatus {
	case "accepted":
		if retentionUsed || paymentMethodID == "" {
			return ErrBillingConflict
		}
		if open, checkErr := hasOpenBillingRenewal(ctx, tx, workspaceID); checkErr != nil {
			return checkErr
		} else if open {
			return ErrBillingConflict
		}
		if _, err := tx.ExecContext(ctx, `UPDATE billing_subscription_contracts
SET retention_offer_used=TRUE,next_period_discount_basis_points=$2,
cancel_at_period_end=FALSE,version=version+1,updated_at=$3 WHERE workspace_id=$1`,
			workspaceID, billingRetentionBPS, now); err != nil {
			return err
		}
	case "cancel_confirmed":
		if err := rejectUnboundSubmittedBillingRenewal(ctx, tx, workspaceID); err != nil {
			return err
		}
		if err := suppressUnsubmittedBillingRenewals(ctx, tx, workspaceID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE billing_subscription_contracts
SET cancel_at_period_end=TRUE,version=version+1,updated_at=$2 WHERE workspace_id=$1`,
			workspaceID, now); err != nil {
			return err
		}
	default:
		return ErrBillingIntentInvalid
	}
	if _, err := tx.ExecContext(ctx, `UPDATE billing_retention_offers
SET status=$2,consumed_at=$3 WHERE token_hash=$1`, tokenHash, targetStatus, now); err != nil {
		return err
	}
	return tx.Commit()
}

func requireBillingOwner(ctx context.Context, q workspaceQueryer, actorUserID, workspaceID string) error {
	access, err := resolveWorkspaceAccess(ctx, q, actorUserID, workspaceID)
	if err != nil {
		return err
	}
	if access.Member.Role != WorkspaceRoleOwner {
		return ErrBillingOwnerRequired
	}
	return nil
}

func activateBillingAttempt(
	ctx context.Context, tx *sql.Tx, attempt BillingPaymentAttempt, payment BillingCanonicalPayment, now time.Time,
) error {
	periodStart := payment.OccurredAt.UTC()
	if periodStart.IsZero() {
		periodStart = now
	}
	periodEnd := addBillingMonth(periodStart)
	discountBPS := 0
	if attempt.Purpose == "renewal" {
		if attempt.RequestedPeriodStart == nil || attempt.RequestedPeriodEnd == nil {
			return ErrBillingConflict
		}
		periodStart, periodEnd = *attempt.RequestedPeriodStart, *attempt.RequestedPeriodEnd
		discountBPS = attempt.DiscountBasisPoints
	}
	if _, err := tx.ExecContext(ctx, `UPDATE billing_subscription_periods
SET status='completed',updated_at=$2 WHERE workspace_id=$1 AND status='active'`, attempt.WorkspaceID, now); err != nil {
		return err
	}
	var periodID int64
	err := tx.QueryRowContext(ctx, `INSERT INTO billing_subscription_periods(
workspace_id,plan_code,plan_version,status,period_start,period_end,list_price_minor,
charged_price_minor,currency_code,discount_basis_points,created_at,updated_at)
VALUES($1,$2,$3,'active',$4,$5,(SELECT monthly_price_minor FROM billing_plan_versions
WHERE plan_code=$2 AND version=$3),$6,$7,$8,$9,$9)
RETURNING id`, attempt.WorkspaceID, attempt.PlanCode, attempt.PlanVersion, periodStart, periodEnd,
		attempt.AmountMinor, attempt.CurrencyCode, discountBPS, now).Scan(&periodID)
	if err != nil {
		return err
	}
	if attempt.Purpose == "checkout" {
		cancelAtEnd := !payment.PaymentMethodSaved || strings.TrimSpace(payment.PaymentMethodID) == ""
		_, err = tx.ExecContext(ctx, `INSERT INTO billing_subscription_contracts(
workspace_id,payer_user_id,status,payment_method_id,current_period_id,cancel_at_period_end,
next_charge_at,retention_offer_used,next_period_discount_basis_points,renewal_attempts,created_at,updated_at)
VALUES($1,$2,'active',$3,$4,$5,$6,FALSE,0,0,$7,$7)
ON CONFLICT(workspace_id) DO UPDATE SET payer_user_id=EXCLUDED.payer_user_id,status='active',
payment_method_id=EXCLUDED.payment_method_id,current_period_id=EXCLUDED.current_period_id,
cancel_at_period_end=EXCLUDED.cancel_at_period_end,next_charge_at=EXCLUDED.next_charge_at,grace_until=NULL,
next_period_discount_basis_points=0,renewal_attempts=0,version=billing_subscription_contracts.version+1,
updated_at=EXCLUDED.updated_at`, attempt.WorkspaceID, attempt.RequestedByUserID,
			payment.PaymentMethodID, periodID, cancelAtEnd, periodEnd, now)
	} else {
		cancelAtEnd := !payment.PaymentMethodSaved || strings.TrimSpace(payment.PaymentMethodID) == ""
		_, err = tx.ExecContext(ctx, `UPDATE billing_subscription_contracts SET
status='active',
payment_method_id=CASE WHEN cancel_at_period_end=TRUE AND payment_method_id='' THEN '' ELSE $2 END,
current_period_id=$3,next_charge_at=$4,grace_until=NULL,
next_period_discount_basis_points=0,renewal_attempts=0,
cancel_at_period_end=cancel_at_period_end OR $5,
version=version+1,updated_at=$6 WHERE workspace_id=$1`, attempt.WorkspaceID,
			payment.PaymentMethodID, periodID, periodEnd, cancelAtEnd, now)
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workspace_subscriptions
SET plan_code=$2,plan_version=$3,status='active',started_at=$4,updated_at=$5
WHERE workspace_id=$1`, attempt.WorkspaceID, attempt.PlanCode, attempt.PlanVersion, periodStart, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE billing_payment_attempts
	SET status='succeeded',period_id=$2,updated_at=$3,error_code='',payment_method_snapshot='',
	worker_lease_token='',worker_lease_until=NULL
	WHERE id=$1`, attempt.ID, periodID, now); err != nil {
		return err
	}
	return nil
}

func billingRenewalStillCurrentTx(
	ctx context.Context, tx *sql.Tx, attempt BillingPaymentAttempt,
) (bool, error) {
	if attempt.Purpose != "renewal" || attempt.RequestedPeriodStart == nil {
		return false, ErrBillingConflict
	}
	var contractStatus, paymentMethodID, periodStatus, planCode string
	var cancelAtPeriodEnd bool
	var planVersion int
	var periodEnd time.Time
	err := tx.QueryRowContext(ctx, `SELECT c.status,c.cancel_at_period_end,c.payment_method_id,
p.status,p.plan_code,p.plan_version,p.period_end
FROM billing_subscription_contracts c
JOIN billing_subscription_periods p ON p.id=c.current_period_id AND p.workspace_id=c.workspace_id
WHERE c.workspace_id=$1 FOR UPDATE OF c,p`, attempt.WorkspaceID).Scan(
		&contractStatus, &cancelAtPeriodEnd, &paymentMethodID,
		&periodStatus, &planCode, &planVersion, &periodEnd)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return (contractStatus == "active" || contractStatus == "past_due") &&
		!cancelAtPeriodEnd && paymentMethodID != "" &&
		periodStatus == "active" && planCode == attempt.PlanCode && planVersion == attempt.PlanVersion &&
		periodEnd.UTC().Equal(attempt.RequestedPeriodStart.UTC()), nil
}

func commitStalePaidRenewal(
	ctx context.Context, tx *sql.Tx, attempt BillingPaymentAttempt,
	key, eventType, objectID string, now time.Time,
) (bool, error) {
	result, err := tx.ExecContext(ctx, `UPDATE billing_payment_attempts SET
status='manual_review',error_code='stale_renewal_paid_manual_refund',next_attempt_at=$2,
payment_method_snapshot='',worker_lease_token='',worker_lease_until=NULL,updated_at=$2
WHERE id=$1 AND status<>'succeeded'`, attempt.ID, now)
	if err != nil {
		return false, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return false, ErrBillingConflict
	}
	if err := insertBillingReceipt(ctx, tx, key, eventType, objectID, "failed", now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return false, fmt.Errorf("%w: paid stale renewal requires manual refund", ErrBillingIntegrity)
}

func cancelBillingAttempt(
	ctx context.Context, tx *sql.Tx, attempt BillingPaymentAttempt, cancellationReason string, now time.Time,
) error {
	current := true
	if attempt.Purpose == "renewal" {
		var err error
		current, err = billingRenewalStillCurrentTx(ctx, tx, attempt)
		if err != nil {
			return err
		}
	}
	cancellationReason = strings.TrimSpace(cancellationReason)
	errorCode := "provider_canceled"
	disablePaymentMethod := billingCancellationDisablesPaymentMethod(cancellationReason)
	if disablePaymentMethod {
		errorCode += "_" + cancellationReason
	}
	if _, err := tx.ExecContext(ctx, `UPDATE billing_payment_attempts
SET status='canceled',error_code=$2,payment_method_snapshot='',
worker_lease_token='',worker_lease_until=NULL,updated_at=$3 WHERE id=$1`,
		attempt.ID, errorCode, now); err != nil {
		return err
	}
	if attempt.Purpose != "renewal" || !current {
		return nil
	}
	var grace sql.NullTime
	var periodEnd time.Time
	if err := tx.QueryRowContext(ctx, `SELECT c.grace_until,p.period_end
FROM billing_subscription_contracts c
JOIN billing_subscription_periods p ON p.id=c.current_period_id AND p.workspace_id=c.workspace_id
WHERE c.workspace_id=$1 FOR UPDATE OF c,p`, attempt.WorkspaceID).Scan(&grace, &periodEnd); err != nil {
		return err
	}
	if disablePaymentMethod {
		if _, err := tx.ExecContext(ctx, `UPDATE billing_payment_attempts SET
payment_method_snapshot='',updated_at=$2 WHERE workspace_id=$1 AND purpose='renewal'`,
			attempt.WorkspaceID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE billing_retention_offers SET
status='expired',consumed_at=$2 WHERE workspace_id=$1 AND status='pending'`,
			attempt.WorkspaceID, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE billing_subscription_contracts SET
payment_method_id='',cancel_at_period_end=TRUE,next_charge_at=$2,grace_until=NULL,
next_period_discount_basis_points=0,renewal_attempts=GREATEST(renewal_attempts,$3),
version=version+1,updated_at=$4 WHERE workspace_id=$1`, attempt.WorkspaceID,
			periodEnd.UTC(), attempt.AttemptNumber, now)
		return err
	}
	graceUntil := now.Add(billingRenewalGrace)
	if grace.Valid {
		graceUntil = grace.Time.UTC()
	}
	_, err := tx.ExecContext(ctx, `UPDATE billing_subscription_contracts
SET status='past_due',renewal_attempts=$2,next_charge_at=$3,grace_until=$4,
version=version+1,updated_at=$5 WHERE workspace_id=$1`, attempt.WorkspaceID,
		attempt.AttemptNumber, now.Add(billingRenewalRetry), graceUntil, now)
	return err
}

func billingCancellationDisablesPaymentMethod(reason string) bool {
	switch strings.TrimSpace(reason) {
	case "permission_revoked", "card_expired", "payment_method_restricted":
		return true
	default:
		return false
	}
}

func downgradeBillingWorkspaceTx(ctx context.Context, tx *sql.Tx, workspaceID string, periodID int64, now time.Time) error {
	var publishing bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM posts WHERE workspace_id=$1 AND status='publishing')`, workspaceID).Scan(&publishing); err != nil {
		return err
	}
	if publishing {
		return ErrBillingConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE billing_subscription_periods
SET status='completed',updated_at=$2 WHERE id=$1 AND status='active'`, periodID, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE billing_payment_attempts SET
status=CASE
  WHEN status='prepared' AND provider_create_started_at IS NULL AND provider_payment_id IS NULL THEN 'failed'
  WHEN status='prepared' THEN 'manual_review'
  ELSE status END,
error_code=CASE
  WHEN status='prepared' AND provider_create_started_at IS NULL AND provider_payment_id IS NULL
    THEN 'contract_ended_before_submission'
  WHEN status='prepared' THEN 'contract_ended_provider_outcome_unknown'
  ELSE error_code END,
payment_method_snapshot='',worker_lease_token='',worker_lease_until=NULL,updated_at=$2
WHERE workspace_id=$1 AND purpose='renewal'`, workspaceID, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE billing_subscription_contracts SET
status='ended',payment_method_id='',cancel_at_period_end=FALSE,next_charge_at=NULL,grace_until=NULL,
next_period_discount_basis_points=0,version=version+1,updated_at=$2 WHERE workspace_id=$1`, workspaceID, now); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE workspace_subscriptions
SET plan_code='free',plan_version=2,status='active',started_at=$2,updated_at=$2 WHERE workspace_id=$1`,
		workspaceID, now)
	return err
}

func newBillingPaymentAttempt(
	actorUserID, workspaceID, purpose string, plan BillingPlan, attemptNumber int,
	periodStart, periodEnd *time.Time, discountBPS int, now time.Time,
) (BillingPaymentAttempt, error) {
	id, err := randomBillingHex(16)
	if err != nil {
		return BillingPaymentAttempt{}, err
	}
	key, err := randomBillingUUID()
	if err != nil {
		return BillingPaymentAttempt{}, err
	}
	amount := plan.MonthlyPriceMinor * int64(10000-discountBPS) / 10000
	return BillingPaymentAttempt{
		ID: id, WorkspaceID: workspaceID, RequestedByUserID: actorUserID,
		Purpose: purpose, AttemptNumber: attemptNumber, IdempotencyKey: key,
		PlanCode: plan.Code, PlanVersion: plan.Version, AmountMinor: amount,
		CurrencyCode: plan.CurrencyCode, Status: "prepared",
		ProviderDescription: "MaxPosty: тариф " + plan.Code + " на 1 месяц",
		DiscountBasisPoints: discountBPS, CreateDeadline: now.Add(billingProviderCreateWindow),
		NextAttemptAt:        now,
		RequestedPeriodStart: periodStart, RequestedPeriodEnd: periodEnd,
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

func insertBillingPaymentAttempt(ctx context.Context, tx *sql.Tx, attempt BillingPaymentAttempt) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO billing_payment_attempts(
id,workspace_id,requested_by_user_id,purpose,attempt_number,idempotency_key,
plan_code,plan_version,amount_minor,currency_code,status,requested_period_start,
requested_period_end,provider_description,provider_return_url,payment_method_snapshot,
discount_basis_points,create_deadline,next_attempt_at,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$20)`,
		attempt.ID, attempt.WorkspaceID, attempt.RequestedByUserID, attempt.Purpose,
		attempt.AttemptNumber, attempt.IdempotencyKey, attempt.PlanCode, attempt.PlanVersion,
		attempt.AmountMinor, attempt.CurrencyCode, attempt.Status, attempt.RequestedPeriodStart,
		attempt.RequestedPeriodEnd, attempt.ProviderDescription, attempt.ProviderReturnURL,
		attempt.PaymentMethodSnapshot, attempt.DiscountBasisPoints, attempt.CreateDeadline,
		attempt.NextAttemptAt, attempt.CreatedAt)
	return err
}

const billingAttemptSelect = `SELECT a.id,a.workspace_id,a.requested_by_user_id,a.period_id,a.purpose,
a.attempt_number,a.idempotency_key,COALESCE(a.provider_payment_id,''),a.plan_code,a.plan_version,
a.amount_minor,a.currency_code,a.status,a.confirmation_url,a.requested_period_start,
a.requested_period_end,a.error_code,a.provider_description,a.provider_return_url,a.payment_method_snapshot,
a.discount_basis_points,a.provider_create_started_at,
a.create_deadline,a.next_attempt_at,a.create_attempts,a.status_check_attempts,a.worker_lease_token,a.worker_lease_until,
a.created_at,a.updated_at FROM billing_payment_attempts a`

type billingAttemptScanner interface {
	Scan(...any) error
}

func scanBillingPaymentAttempt(scanner billingAttemptScanner) (BillingPaymentAttempt, error) {
	var attempt BillingPaymentAttempt
	var periodID sql.NullInt64
	var requestedStart, requestedEnd, providerStarted, workerLeaseUntil sql.NullTime
	err := scanner.Scan(&attempt.ID, &attempt.WorkspaceID, &attempt.RequestedByUserID, &periodID,
		&attempt.Purpose, &attempt.AttemptNumber, &attempt.IdempotencyKey, &attempt.ProviderPaymentID,
		&attempt.PlanCode, &attempt.PlanVersion, &attempt.AmountMinor, &attempt.CurrencyCode,
		&attempt.Status, &attempt.ConfirmationURL, &requestedStart, &requestedEnd,
		&attempt.ErrorCode, &attempt.ProviderDescription, &attempt.ProviderReturnURL,
		&attempt.PaymentMethodSnapshot, &attempt.DiscountBasisPoints, &providerStarted,
		&attempt.CreateDeadline, &attempt.NextAttemptAt, &attempt.CreateAttempts, &attempt.StatusCheckAttempts,
		&attempt.WorkerLeaseToken, &workerLeaseUntil, &attempt.CreatedAt, &attempt.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return BillingPaymentAttempt{}, ErrNotFound
	}
	if err != nil {
		return BillingPaymentAttempt{}, err
	}
	if periodID.Valid {
		attempt.PeriodID = &periodID.Int64
	}
	attempt.RequestedPeriodStart = nullTimePointer(requestedStart)
	attempt.RequestedPeriodEnd = nullTimePointer(requestedEnd)
	attempt.ProviderCreateStartedAt = nullTimePointer(providerStarted)
	attempt.WorkerLeaseUntil = nullTimePointer(workerLeaseUntil)
	attempt.CreateDeadline = attempt.CreateDeadline.UTC()
	attempt.NextAttemptAt = attempt.NextAttemptAt.UTC()
	attempt.CreatedAt = attempt.CreatedAt.UTC()
	attempt.UpdatedAt = attempt.UpdatedAt.UTC()
	return attempt, nil
}

func insertBillingReceipt(
	ctx context.Context, tx *sql.Tx, key, eventType, objectID, result string, now time.Time,
) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO billing_webhook_receipts(
dedupe_key,event_type,object_id,result,received_at,processed_at)
VALUES($1,$2,$3,$4,$5,$5)`, key, eventType, objectID, result, now)
	return err
}

func commitBillingIntegrityFailure(
	ctx context.Context, tx *sql.Tx, key, eventType, objectID, reason string, now time.Time,
) (bool, error) {
	if err := insertBillingReceipt(ctx, tx, key, eventType, objectID, "failed", now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return false, fmt.Errorf("%w: %s", ErrBillingIntegrity, reason)
}

func randomBillingToken() (string, string, error) {
	token, err := randomBillingHex(32)
	if err != nil {
		return "", "", err
	}
	digest := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(digest[:]), nil
}

func randomBillingHex(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func randomBillingUUID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "SQLSTATE 23505") || strings.Contains(strings.ToLower(err.Error()), "unique")
}

// addBillingMonth advances a monthly anniversary while clamping the day to
// the target month's final day (Jan 31 -> Feb 28/29, never March).
func addBillingMonth(value time.Time) time.Time {
	value = value.UTC()
	targetMonth := value.Month() + 1
	targetYear := value.Year()
	if targetMonth > time.December {
		targetMonth = time.January
		targetYear++
	}
	lastDay := time.Date(targetYear, targetMonth+1, 0, 0, 0, 0, 0, time.UTC).Day()
	day := value.Day()
	if day > lastDay {
		day = lastDay
	}
	return time.Date(targetYear, targetMonth, day, value.Hour(), value.Minute(), value.Second(), value.Nanosecond(), time.UTC)
}
