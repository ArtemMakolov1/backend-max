package api

import (
	"net/http"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/store"
)

type workspaceBillingResponse struct {
	store.WorkspaceBillingState
	MonthlyEnforcementEnabled bool             `json:"monthly_enforcement_enabled"`
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
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, workspaceBillingResponse{
		WorkspaceBillingState:     state,
		MonthlyEnforcementEnabled: s.aiLimiter.options.MonthlyPlanEnforcement,
		ImageCreditCosts:          currentImageCreditCosts(),
	})
}
