package app

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yookassa"
)

const (
	billingMethodCipherPrefix     = "v1."
	billingProviderPendingHorizon = 7 * 24 * time.Hour
	billingWorkerBatchSize        = 4
)

// BillingClient is deliberately limited to the two provider operations used
// by subscriptions, which also makes the orchestration independently testable.
type BillingClient interface {
	CreatePayment(context.Context, string, yookassa.CreatePaymentRequest) (yookassa.Payment, error)
	GetPayment(context.Context, string) (yookassa.Payment, error)
}

type BillingCheckout struct {
	CheckoutID      string `json:"checkout_id"`
	Status          string `json:"status"`
	ConfirmationURL string `json:"confirmation_url,omitempty"`
}

type billingMethodCipher struct {
	aead cipher.AEAD
}

// ConfigureBilling enables checkout and reconciliation. dataKey must be 32
// random bytes supplied outside the database so a database read alone cannot
// be used to perform another merchant-initiated charge.
func (a *App) ConfigureBilling(client BillingClient, returnURL string, dataKey []byte) error {
	if client == nil {
		return errors.New("billing client is required")
	}
	parsed, err := url.Parse(strings.TrimSpace(returnURL))
	if err != nil || !parsed.IsAbs() || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || (parsed.Port() != "" && parsed.Port() != "443") {
		return errors.New("billing return URL must be an absolute HTTPS URL on the default port without credentials")
	}
	if len(dataKey) != 32 {
		return errors.New("billing data key must contain exactly 32 bytes")
	}
	block, err := aes.NewCipher(dataKey)
	if err != nil {
		return fmt.Errorf("initialize billing encryption: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("initialize billing authenticated encryption: %w", err)
	}
	a.billing = client
	a.billingReturnURL = parsed.String()
	a.billingCipher = &billingMethodCipher{aead: aead}
	// Provider configuration enables canonical GET/reconciliation only. New
	// charges remain fail-closed until SetBillingLiveEnabled(true) is called.
	a.billingLiveEnabled = false
	return nil
}

func (a *App) BillingConfigured() bool {
	return a != nil && a.billing != nil && a.billingCipher != nil && a.billingReturnURL != ""
}

func (a *App) SetBillingLiveEnabled(enabled bool) error {
	if enabled && !a.BillingConfigured() {
		return ErrBillingNotConfigured
	}
	a.billingLiveEnabled = enabled
	return nil
}

func (a *App) BillingLiveEnabled() bool {
	return a != nil && a.BillingConfigured() && a.billingLiveEnabled
}

func (a *App) CreateBillingCheckout(
	ctx context.Context, actorUserID, workspaceID, planCode string, recurringConsent bool,
	recurringConsentVersion string,
) (BillingCheckout, error) {
	if !a.BillingLiveEnabled() {
		return BillingCheckout{}, ErrBillingNotConfigured
	}
	attempt, err := a.store.CreateBillingCheckoutAttempt(
		ctx, actorUserID, workspaceID, planCode, recurringConsent, recurringConsentVersion,
		a.billingReturnURL, a.now().UTC())
	if err != nil {
		return BillingCheckout{}, err
	}
	if attempt.Status == "pending" && attempt.ProviderPaymentID != "" && attempt.ConfirmationURL != "" {
		return billingCheckoutFromAttempt(attempt), nil
	}
	updated, err := a.createProviderPayment(ctx, attempt)
	if err != nil {
		return BillingCheckout{}, err
	}
	return billingCheckoutFromAttempt(updated), nil
}

func (a *App) CreateBillingCancellationIntent(
	ctx context.Context, actorUserID, workspaceID string,
) (store.BillingCancellationIntent, error) {
	return a.store.CreateBillingCancellationIntent(ctx, actorUserID, workspaceID, a.now().UTC())
}

func (a *App) AcceptBillingRetentionOffer(
	ctx context.Context, actorUserID, workspaceID, tokenHash string,
) error {
	if !a.BillingLiveEnabled() {
		return ErrBillingNotConfigured
	}
	return a.store.AcceptBillingRetentionOffer(ctx, actorUserID, workspaceID, tokenHash, a.now().UTC())
}

func (a *App) ConfirmBillingCancellation(
	ctx context.Context, actorUserID, workspaceID, tokenHash string,
) error {
	return a.store.ConfirmBillingCancellation(ctx, actorUserID, workspaceID, tokenHash, a.now().UTC())
}

func (a *App) ResumeBillingSubscription(ctx context.Context, actorUserID, workspaceID string) error {
	if !a.BillingLiveEnabled() {
		return ErrBillingNotConfigured
	}
	return a.store.ResumeBillingSubscription(ctx, actorUserID, workspaceID, a.now().UTC())
}

func (a *App) DetachBillingPaymentMethod(ctx context.Context, actorUserID, workspaceID string) error {
	return a.store.DetachBillingPaymentMethod(ctx, actorUserID, workspaceID, a.now().UTC())
}

// ReconcileYooKassaPayment never trusts webhook payment fields. The caller
// supplies only an object ID; this method fetches the canonical object using
// merchant authentication before touching billing state.
func (a *App) ReconcileYooKassaPayment(
	ctx context.Context, eventType, providerPaymentID string,
) (bool, error) {
	if !a.BillingConfigured() {
		return false, ErrBillingNotConfigured
	}
	payment, err := a.billing.GetPayment(ctx, providerPaymentID)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrPaymentProvider, err)
	}
	now := a.now().UTC()
	processed, err := a.reconcileProviderPayment(ctx, eventType, payment, now)
	if errors.Is(err, store.ErrBillingIntegrity) {
		attempt, lookupErr := a.store.GetBillingPaymentAttemptByProviderID(ctx, payment.ID)
		if errors.Is(lookupErr, store.ErrNotFound) && payment.Metadata["attempt_id"] != "" {
			attempt, lookupErr = a.store.GetBillingPaymentAttempt(ctx, payment.Metadata["attempt_id"])
		}
		if lookupErr == nil {
			if markErr := a.store.FailBillingProviderCreate(
				ctx, attempt.ID, "provider_integrity_failed", true, now,
			); markErr != nil && !errors.Is(markErr, store.ErrBillingConflict) {
				return false, errors.Join(err, markErr)
			}
		}
	}
	return processed, err
}

func (a *App) runBillingCycle(ctx context.Context, now time.Time) {
	manualReviewCount, countErr := a.store.CountManualReviewBillingAttempts(ctx)
	if countErr != nil {
		a.metrics.ObserveSchedulerJob("billing_manual_review_scan", "error")
	} else {
		a.observeBillingManualReview(manualReviewCount, now)
	}
	due, err := a.store.ListDueBillingContracts(ctx, now, 20)
	if err != nil {
		a.metrics.ObserveSchedulerJob("billing_due_scan", "error")
		a.logger.Error("scheduler could not list due billing contracts", "error", err)
		return
	}
	a.metrics.ObserveSchedulerJob("billing_due_scan", "success")
	a.metrics.SetSchedulerDue("billing_renewal", len(due))
	for _, contract := range due {
		var lifecycleErr error
		if a.BillingLiveEnabled() {
			_, _, lifecycleErr = a.store.PrepareBillingRenewal(ctx, contract.WorkspaceID, now)
		} else {
			_, lifecycleErr = a.store.ApplyBillingLifecycleWithoutRenewal(ctx, contract.WorkspaceID, now)
		}
		if lifecycleErr != nil && !errors.Is(lifecycleErr, store.ErrNotFound) {
			a.metrics.ObserveSchedulerJob("billing_prepare", "error")
			a.logger.Error("scheduler could not apply billing lifecycle", "error", lifecycleErr)
		} else {
			a.metrics.ObserveSchedulerJob("billing_prepare", "success")
		}
	}
	// Cancellation, grace and downgrade are local commercial state and must keep
	// advancing even if provider credentials are temporarily removed. External
	// reconciliation below is the only part that requires a provider client.
	if !a.BillingConfigured() {
		return
	}

	var attempts []store.BillingPaymentAttempt
	if a.BillingLiveEnabled() {
		attempts, err = a.store.ListBillingAttemptsForWorker(ctx, now, billingWorkerBatchSize)
	} else {
		attempts, err = a.store.ListPendingBillingAttemptsForWorker(ctx, now, billingWorkerBatchSize)
	}
	if err != nil {
		a.metrics.ObserveSchedulerJob("billing_attempt_scan", "error")
		a.logger.Error("scheduler could not list billing attempts", "error", err)
		return
	}
	a.metrics.ObserveSchedulerJob("billing_attempt_scan", "success")
	a.metrics.SetSchedulerDue("billing_attempt", len(attempts))
	for _, attempt := range attempts {
		if err := a.processBillingAttempt(ctx, attempt, now); err != nil {
			a.metrics.ObserveSchedulerJob("billing_attempt", "error")
			a.logger.Error("scheduler could not reconcile billing attempt", "error", err)
		} else {
			a.metrics.ObserveSchedulerJob("billing_attempt", "success")
		}
	}
}

func (a *App) observeBillingManualReview(count int, now time.Time) {
	a.metrics.SetSchedulerDue("billing_manual_review", count)
	a.billingManualReviewMu.Lock()
	defer a.billingManualReviewMu.Unlock()
	shouldLog := count > 0 && (count != a.billingManualReviewLastCount ||
		a.billingManualReviewLastLog.IsZero() || now.Sub(a.billingManualReviewLastLog) >= time.Hour)
	a.billingManualReviewLastCount = count
	if shouldLog {
		a.billingManualReviewLastLog = now
		a.logger.Error("billing payments require manual reconciliation", "count", count)
	}
}

func (a *App) processBillingAttempt(ctx context.Context, attempt store.BillingPaymentAttempt, now time.Time) error {
	if attempt.Status == "prepared" {
		updated, err := a.createProviderPaymentAt(ctx, attempt, now)
		if err != nil {
			return err
		}
		attempt = updated
	}
	if attempt.Status != "pending" || attempt.ProviderPaymentID == "" {
		return nil
	}
	if billingPendingHorizonExceeded(attempt, now) {
		return a.store.FailBillingProviderCreate(
			ctx, attempt.ID, "provider_pending_horizon_exceeded", true, now,
		)
	}
	payment, err := a.billing.GetPayment(ctx, attempt.ProviderPaymentID)
	if err != nil {
		next := billingStatusRetryAt(attempt, now)
		if deferErr := a.store.DeferBillingAttempt(ctx, attempt.ID, "provider_status_unavailable", next); deferErr != nil {
			return errors.Join(fmt.Errorf("%w: %v", ErrPaymentProvider, err), deferErr)
		}
		return fmt.Errorf("%w: %v", ErrPaymentProvider, err)
	}
	_, err = a.reconcileProviderPayment(ctx, "payment.status.checked", payment, now)
	if errors.Is(err, store.ErrBillingIntegrity) {
		if markErr := a.store.FailBillingProviderCreate(
			ctx, attempt.ID, "provider_integrity_failed", true, now,
		); markErr != nil && !errors.Is(markErr, store.ErrBillingConflict) {
			return errors.Join(err, markErr)
		}
	}
	return err
}

func billingPendingHorizonExceeded(attempt store.BillingPaymentAttempt, now time.Time) bool {
	started := attempt.CreatedAt
	if attempt.ProviderCreateStartedAt != nil {
		started = *attempt.ProviderCreateStartedAt
	}
	return !now.UTC().Before(started.UTC().Add(billingProviderPendingHorizon))
}

func (a *App) createProviderPayment(
	ctx context.Context, attempt store.BillingPaymentAttempt,
) (store.BillingPaymentAttempt, error) {
	return a.createProviderPaymentAt(ctx, attempt, a.now().UTC())
}

func (a *App) createProviderPaymentAt(
	ctx context.Context, attempt store.BillingPaymentAttempt, now time.Time,
) (store.BillingPaymentAttempt, error) {
	started, err := a.store.BeginBillingProviderCreate(ctx, attempt.ID, now)
	if err != nil {
		return store.BillingPaymentAttempt{}, err
	}
	attempt = started
	request := yookassa.CreatePaymentRequest{
		Amount:      yookassa.Amount{Value: billingAmount(attempt.AmountMinor), Currency: attempt.CurrencyCode},
		Capture:     true,
		Description: attempt.ProviderDescription,
		Metadata: map[string]string{
			"attempt_id":   attempt.ID,
			"workspace_id": attempt.WorkspaceID,
			"plan_code":    attempt.PlanCode,
			"plan_version": strconv.Itoa(attempt.PlanVersion),
		},
	}
	if attempt.Purpose == "checkout" {
		request.Confirmation = &yookassa.ConfirmationRequest{Type: "redirect", ReturnURL: attempt.ProviderReturnURL}
		request.PaymentMethodData = &yookassa.PaymentMethodData{Type: "bank_card"}
		request.SavePaymentMethod = true
	} else {
		methodID, err := a.billingCipher.open(attempt.WorkspaceID, attempt.PaymentMethodSnapshot)
		if err != nil {
			return store.BillingPaymentAttempt{}, fmt.Errorf("decrypt saved billing payment method: %w", err)
		}
		request.PaymentMethodID = methodID
	}
	payment, err := a.billing.CreatePayment(ctx, attempt.IdempotencyKey, request)
	if err != nil {
		if stateErr := a.recordBillingCreateFailure(ctx, attempt, err, now); stateErr != nil {
			return store.BillingPaymentAttempt{}, errors.Join(fmt.Errorf("%w: %v", ErrPaymentProvider, err), stateErr)
		}
		return store.BillingPaymentAttempt{}, fmt.Errorf("%w: %v", ErrPaymentProvider, err)
	}
	canonical, err := a.canonicalBillingPayment(payment)
	if err != nil || canonical.Test || canonical.MetadataAttemptID != attempt.ID ||
		canonical.MetadataWorkspaceID != attempt.WorkspaceID || canonical.AmountMinor != attempt.AmountMinor ||
		canonical.CurrencyCode != attempt.CurrencyCode {
		stateErr := a.store.QuarantineBillingProviderPayment(
			ctx, attempt.ID, payment.ID, "provider_response_mismatch", now)
		if err == nil {
			err = errors.New("YooKassa create response does not match the local payment attempt")
		}
		if stateErr != nil {
			return store.BillingPaymentAttempt{}, errors.Join(fmt.Errorf("%w: %v", ErrPaymentProvider, err), stateErr)
		}
		return store.BillingPaymentAttempt{}, fmt.Errorf("%w: %v", ErrPaymentProvider, err)
	}
	confirmationURL := ""
	if payment.Confirmation != nil {
		confirmationURL = payment.Confirmation.ConfirmationURL
	}
	if attempt.Purpose == "checkout" && confirmationURL == "" {
		stateErr := a.store.QuarantineBillingProviderPayment(
			ctx, attempt.ID, payment.ID, "provider_confirmation_missing", now)
		providerErr := fmt.Errorf("%w: checkout confirmation URL is missing", ErrPaymentProvider)
		if stateErr != nil {
			return store.BillingPaymentAttempt{}, errors.Join(providerErr, stateErr)
		}
		return store.BillingPaymentAttempt{}, providerErr
	}
	updated, err := a.store.AttachBillingProviderPayment(
		ctx, attempt.ID, payment.ID, confirmationURL, now)
	if err != nil {
		next := billingRetryAt(attempt, now)
		if !next.Before(attempt.CreateDeadline) {
			_ = a.store.FailBillingProviderCreate(ctx, attempt.ID, "provider_attach_outcome_unknown", true, now)
		} else {
			_ = a.store.DeferBillingAttempt(ctx, attempt.ID, "provider_attach_failed", next)
		}
		return store.BillingPaymentAttempt{}, err
	}
	if payment.Status == "succeeded" || payment.Status == "canceled" {
		if _, err := a.reconcileProviderPayment(ctx, "payment."+payment.Status, payment, now); err != nil {
			return store.BillingPaymentAttempt{}, err
		}
		return a.store.GetBillingPaymentAttempt(ctx, attempt.ID)
	}
	return updated, nil
}

func (a *App) recordBillingCreateFailure(
	ctx context.Context, attempt store.BillingPaymentAttempt, providerErr error, now time.Time,
) error {
	var apiErr *yookassa.Error
	if errors.As(providerErr, &apiErr) && apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 &&
		apiErr.StatusCode != 408 && apiErr.StatusCode != 429 {
		// Authentication, merchant capability, receipt/configuration and
		// idempotency failures are integration-wide, not customer declines.
		// Keep the attempt blocked for canonical operator reconciliation.
		return a.store.FailBillingProviderCreate(ctx, attempt.ID, "provider_request_rejected", true, now)
	}
	next := billingRetryAt(attempt, now)
	if !next.Before(attempt.CreateDeadline) {
		return a.store.FailBillingProviderCreate(ctx, attempt.ID, "provider_create_outcome_unknown", true, now)
	}
	return a.store.DeferBillingAttempt(ctx, attempt.ID, "provider_create_retry", next)
}

func billingRetryAt(attempt store.BillingPaymentAttempt, now time.Time) time.Time {
	steps := attempt.CreateAttempts
	if steps < 1 {
		steps = 1
	}
	if steps > 6 {
		steps = 6
	}
	delay := 30 * time.Second * time.Duration(1<<(steps-1))
	if delay > 30*time.Minute {
		delay = 30 * time.Minute
	}
	return now.UTC().Add(delay)
}

func billingStatusRetryAt(attempt store.BillingPaymentAttempt, now time.Time) time.Time {
	steps := attempt.StatusCheckAttempts + 1
	if steps > 10 {
		steps = 10
	}
	delay := 30 * time.Second * time.Duration(1<<(steps-1))
	if delay > 30*time.Minute {
		delay = 30 * time.Minute
	}
	digest := sha256.Sum256([]byte(attempt.ID))
	jitter := time.Duration(digest[0]%21) * delay / 100
	return now.UTC().Add(delay + jitter)
}

func (a *App) reconcileProviderPayment(
	ctx context.Context, eventType string, payment yookassa.Payment, now time.Time,
) (bool, error) {
	canonical, err := a.canonicalBillingPayment(payment)
	if err != nil {
		return false, err
	}
	dedupeSource := eventType + "\x00" + payment.ID + "\x00" + payment.Status + "\x00" + payment.CapturedAt
	if eventType == "payment.status.checked" {
		// Worker polls are separate observations and must each release their newly
		// claimed lease even while the canonical status remains pending.
		dedupeSource += "\x00" + now.UTC().Format(time.RFC3339Nano)
	}
	digest := sha256.Sum256([]byte(dedupeSource))
	return a.store.ReconcileBillingPayment(ctx, eventType, hex.EncodeToString(digest[:]), canonical, now)
}

func (a *App) canonicalBillingPayment(payment yookassa.Payment) (store.BillingCanonicalPayment, error) {
	amountMinor, err := billingAmountMinor(payment.Amount.Value)
	if err != nil {
		return store.BillingCanonicalPayment{}, err
	}
	result := store.BillingCanonicalPayment{
		ProviderPaymentID:   payment.ID,
		Status:              payment.Status,
		Paid:                payment.Paid,
		AmountMinor:         amountMinor,
		CurrencyCode:        payment.Amount.Currency,
		MetadataAttemptID:   payment.Metadata["attempt_id"],
		MetadataWorkspaceID: payment.Metadata["workspace_id"],
		Test:                payment.Test,
	}
	if payment.PaymentMethod != nil {
		result.PaymentMethodSaved = payment.PaymentMethod.Saved
		if payment.PaymentMethod.Saved && payment.PaymentMethod.ID != "" {
			workspaceID := payment.Metadata["workspace_id"]
			if strings.TrimSpace(workspaceID) == "" {
				return store.BillingCanonicalPayment{}, errors.New("YooKassa payment metadata is missing workspace_id")
			}
			result.PaymentMethodID, err = a.billingCipher.seal(workspaceID, payment.PaymentMethod.ID)
			if err != nil {
				return store.BillingCanonicalPayment{}, err
			}
		}
	}
	when := payment.CapturedAt
	if when == "" {
		when = payment.CreatedAt
	}
	if when != "" {
		result.OccurredAt, err = time.Parse(time.RFC3339Nano, when)
		if err != nil {
			return store.BillingCanonicalPayment{}, fmt.Errorf("parse YooKassa payment time: %w", err)
		}
	}
	return result, nil
}

func billingCheckoutFromAttempt(attempt store.BillingPaymentAttempt) BillingCheckout {
	return BillingCheckout{CheckoutID: attempt.ID, Status: attempt.Status, ConfirmationURL: attempt.ConfirmationURL}
}

func billingAmount(minor int64) string {
	return fmt.Sprintf("%d.%02d", minor/100, minor%100)
}

func billingAmountMinor(value string) (int64, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 || len(parts[1]) != 2 {
		return 0, errors.New("invalid YooKassa payment amount")
	}
	rubles, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || rubles < 0 {
		return 0, errors.New("invalid YooKassa payment amount")
	}
	kopecks, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || kopecks < 0 || kopecks > 99 || rubles > (1<<63-1-kopecks)/100 {
		return 0, errors.New("invalid YooKassa payment amount")
	}
	return rubles*100 + kopecks, nil
}

func (c *billingMethodCipher) seal(workspaceID, value string) (string, error) {
	if c == nil || c.aead == nil || strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(value) == "" {
		return "", errors.New("billing payment method encryption is unavailable")
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(value), billingCipherAAD(workspaceID))
	return billingMethodCipherPrefix + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (c *billingMethodCipher) open(workspaceID, value string) (string, error) {
	if c == nil || c.aead == nil || strings.TrimSpace(workspaceID) == "" || !strings.HasPrefix(value, billingMethodCipherPrefix) {
		return "", errors.New("invalid encrypted billing payment method")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, billingMethodCipherPrefix))
	if err != nil || len(payload) < c.aead.NonceSize() {
		return "", errors.New("invalid encrypted billing payment method")
	}
	nonce, ciphertext := payload[:c.aead.NonceSize()], payload[c.aead.NonceSize():]
	plain, err := c.aead.Open(nil, nonce, ciphertext, billingCipherAAD(workspaceID))
	if err != nil || strings.TrimSpace(string(plain)) == "" {
		return "", errors.New("invalid encrypted billing payment method")
	}
	return string(plain), nil
}

func billingCipherAAD(workspaceID string) []byte {
	return []byte(billingMethodCipherPrefix + "\x00" + workspaceID + "\x00yookassa")
}
