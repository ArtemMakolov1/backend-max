package api

import (
	"net/http"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/store"
)

type workspaceBillingResponse struct {
	store.WorkspaceBillingState
	MonthlyEnforcementEnabled bool `json:"monthly_enforcement_enabled"`
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
	})
}
