package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/store"
)

var yooKassaPaymentIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

type workspaceBillingResponse struct {
	store.WorkspaceBillingState
	MonthlyEnforcementEnabled bool             `json:"monthly_enforcement_enabled"`
	CheckoutEnabled           bool             `json:"checkout_enabled"`
	ImageCreditCosts          imageCreditCosts `json:"image_credit_costs"`
}

// imageCreditCosts mirrors the GenerateImageInput quality values so browsers
// can render per-quality prices without hardcoding ledger constants. Values
// are resolved through imageUsageCredits — the same function every image
// charge goes through — so the response can never drift from what is billed.
type imageCreditCosts struct {
	Low    int64 `json:"low"`
	Medium int64 `json:"medium"`
	High   int64 `json:"high"`
}

type billingCheckoutRequest struct {
	PlanCode                string `json:"plan_code"`
	RecurringConsent        bool   `json:"recurring_consent"`
	RecurringConsentVersion string `json:"recurring_consent_version"`
}

type billingIntentRequest struct {
	IntentToken string `json:"intent_token"`
}

type billingDetachPaymentMethodRequest struct {
	DisableAutorenewal bool `json:"disable_autorenewal"`
	Confirmation       bool `json:"confirmation"`
}

type billingCancellationIntentResponse struct {
	IntentToken      string                `json:"intent_token"`
	CurrentPeriodEnd time.Time             `json:"current_period_end"`
	RetentionOffer   billingRetentionOffer `json:"retention_offer"`
}

type billingRetentionOffer struct {
	Eligible              bool      `json:"eligible"`
	DiscountPercent       int       `json:"discount_percent"`
	AppliesToPeriods      int       `json:"applies_to_periods"`
	RegularPriceResumesAt time.Time `json:"regular_price_resumes_at"`
	ExpiresAt             time.Time `json:"expires_at"`
}

func currentImageCreditCosts() imageCreditCosts {
	return imageCreditCosts{
		Low:    imageUsageCredits("low"),
		Medium: imageUsageCredits("medium"),
		High:   imageUsageCredits("high"),
	}
}

// listPublicBillingPlans is intentionally unauthenticated for pricing pages.
// Store-level filtering guarantees that inactive/internal paid plans are not
// serialized even if this handler is reused elsewhere.
func (s *Server) listPublicBillingPlans(w http.ResponseWriter, r *http.Request) {
	plans, err := s.app.Store().ListPublicBillingPlans(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	for index := range plans {
		plans[index].CheckoutEnabled = s.app.BillingLiveEnabled()
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	s.writeJSON(w, http.StatusOK, plans)
}

func (s *Server) getWorkspaceBilling(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityWorkspaceRead)
	if !ok {
		return
	}
	state, err := s.app.Store().GetWorkspaceBillingState(
		r.Context(), access.UserID, access.WorkspaceID, s.now().UTC())
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeWorkspaceBilling(w, http.StatusOK, state, access)
}

func (s *Server) createWorkspaceBillingCheckout(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityWorkspaceUpdate)
	if !ok {
		return
	}
	var request billingCheckoutRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	request.PlanCode = strings.TrimSpace(request.PlanCode)
	if request.PlanCode == "" {
		s.writeError(w, validationError("plan_code is required"))
		return
	}
	if !request.RecurringConsent {
		s.writeError(w, store.ErrBillingConsentRequired)
		return
	}
	checkout, err := s.app.CreateBillingCheckout(
		r.Context(), access.UserID, access.WorkspaceID, request.PlanCode, request.RecurringConsent,
		strings.TrimSpace(request.RecurringConsentVersion))
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusCreated, checkout)
}

func (s *Server) createWorkspaceBillingCancellationIntent(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityWorkspaceUpdate)
	if !ok {
		return
	}
	intent, err := s.app.CreateBillingCancellationIntent(r.Context(), access.UserID, access.WorkspaceID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, billingCancellationIntentResponse{
		IntentToken: intent.Token, CurrentPeriodEnd: intent.CurrentPeriodEnd,
		RetentionOffer: billingRetentionOffer{
			Eligible: intent.Eligible, DiscountPercent: intent.DiscountPercent,
			AppliesToPeriods: 1, RegularPriceResumesAt: intent.RegularPriceResumesAt,
			ExpiresAt: intent.ExpiresAt,
		},
	})
}

func (s *Server) acceptWorkspaceBillingRetentionOffer(w http.ResponseWriter, r *http.Request) {
	s.consumeWorkspaceBillingIntent(w, r, true)
}

func (s *Server) confirmWorkspaceBillingCancellation(w http.ResponseWriter, r *http.Request) {
	s.consumeWorkspaceBillingIntent(w, r, false)
}

func (s *Server) consumeWorkspaceBillingIntent(w http.ResponseWriter, r *http.Request, acceptOffer bool) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityWorkspaceUpdate)
	if !ok {
		return
	}
	var request billingIntentRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	request.IntentToken = strings.TrimSpace(request.IntentToken)
	if len(request.IntentToken) != 64 {
		s.writeError(w, store.ErrBillingIntentInvalid)
		return
	}
	var err error
	if acceptOffer {
		err = s.app.AcceptBillingRetentionOffer(
			r.Context(), access.UserID, access.WorkspaceID, sha256Hex(request.IntentToken))
	} else {
		err = s.app.ConfirmBillingCancellation(
			r.Context(), access.UserID, access.WorkspaceID, sha256Hex(request.IntentToken))
	}
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.respondWorkspaceBillingState(w, r, access)
}

func (s *Server) resumeWorkspaceBilling(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityWorkspaceUpdate)
	if !ok {
		return
	}
	if err := s.app.ResumeBillingSubscription(r.Context(), access.UserID, access.WorkspaceID); err != nil {
		s.writeError(w, err)
		return
	}
	s.respondWorkspaceBillingState(w, r, access)
}

func (s *Server) detachWorkspaceBillingPaymentMethod(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityWorkspaceUpdate)
	if !ok {
		return
	}
	var request billingDetachPaymentMethodRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	if !request.DisableAutorenewal || !request.Confirmation {
		s.writeError(w, validationError("disable_autorenewal and confirmation must both be true"))
		return
	}
	if err := s.app.DetachBillingPaymentMethod(r.Context(), access.UserID, access.WorkspaceID); err != nil {
		s.writeError(w, err)
		return
	}
	s.respondWorkspaceBillingState(w, r, access)
}

func (s *Server) respondWorkspaceBillingState(w http.ResponseWriter, r *http.Request, access app.AccessContext) {
	state, err := s.app.Store().GetWorkspaceBillingState(
		r.Context(), access.UserID, access.WorkspaceID, s.now().UTC())
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeWorkspaceBilling(w, http.StatusOK, state, access)
}

func (s *Server) writeWorkspaceBilling(
	w http.ResponseWriter, status int, state store.WorkspaceBillingState, access app.AccessContext,
) {
	if access.Role != store.WorkspaceRoleOwner {
		state.Actions = store.BillingActions{}
	} else if !s.app.BillingLiveEnabled() {
		state.Actions.CanCheckout = false
		state.Actions.CanResume = false
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, status, workspaceBillingResponse{
		WorkspaceBillingState:     state,
		MonthlyEnforcementEnabled: s.aiLimiter.options.MonthlyPlanEnforcement,
		CheckoutEnabled:           s.app.BillingLiveEnabled(),
		ImageCreditCosts:          currentImageCreditCosts(),
	})
}

func (s *Server) yooKassaWebhook(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	var payload struct {
		Event  string `json:"event"`
		Object struct {
			ID string `json:"id"`
		} `json:"object"`
	}
	if err := decoder.Decode(&payload); err != nil {
		s.problem(w, http.StatusBadRequest, "invalid_json", "Request body is not valid JSON", nil)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		s.problem(w, http.StatusBadRequest, "invalid_json", "Request body must contain one JSON value", nil)
		return
	}
	payload.Event = strings.TrimSpace(payload.Event)
	payload.Object.ID = strings.TrimSpace(payload.Object.ID)
	switch payload.Event {
	case "payment.succeeded", "payment.canceled", "payment.waiting_for_capture":
	default:
		// Acknowledge event kinds this service does not subscribe to. This avoids
		// turning future YooKassa additions into a retry storm.
		s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if payload.Object.ID == "" {
		s.problem(w, http.StatusBadRequest, "validation_error", "YooKassa object.id is required", nil)
		return
	}
	if !yooKassaPaymentIDPattern.MatchString(payload.Object.ID) {
		s.problem(w, http.StatusBadRequest, "validation_error", "YooKassa object.id is invalid", nil)
		return
	}
	if allowed, retryAfter := s.yooWebhookLimiter.Allow(s.oauthClientKey(r), s.now().UTC()); !allowed {
		seconds := int64((retryAfter + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		s.metrics.ObserveSchedulerJob("yookassa_webhook", "rate_limited")
		s.problem(w, http.StatusTooManyRequests, "rate_limited", "Too many YooKassa webhook requests", nil)
		return
	}
	select {
	case s.yooWebhookSlots <- struct{}{}:
		defer func() { <-s.yooWebhookSlots }()
	default:
		w.Header().Set("Retry-After", "1")
		s.metrics.ObserveSchedulerJob("yookassa_webhook", "concurrency_limited")
		s.problem(w, http.StatusTooManyRequests, "rate_limited", "YooKassa webhook capacity is busy", nil)
		return
	}
	if _, err := s.app.ReconcileYooKassaPayment(r.Context(), payload.Event, payload.Object.ID); err != nil {
		if errors.Is(err, store.ErrBillingIntegrity) {
			s.logger.Error("YooKassa webhook failed canonical integrity checks", "error", err)
			s.metrics.ObserveSchedulerJob("yookassa_webhook", "integrity_failed")
			s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
			return
		}
		s.metrics.ObserveSchedulerJob("yookassa_webhook", "error")
		s.writeError(w, err)
		return
	}
	s.metrics.ObserveSchedulerJob("yookassa_webhook", "success")
	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
