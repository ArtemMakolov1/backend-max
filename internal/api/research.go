package api

import (
	"net/http"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

func (s *Server) generateResearch(w http.ResponseWriter, r *http.Request) {
	userID, authErr := authenticatedUserID(r)
	if authErr != nil {
		s.writeError(w, authErr)
		return
	}
	var request openairesearch.Request
	if !s.decodeJSON(w, r, &request) {
		return
	}
	if err := openairesearch.ValidateRequest(request); err != nil {
		s.writeError(w, err)
		return
	}
	if !s.app.ResearchConfigured() {
		s.writeError(w, app.ErrResearchNotConfigured)
		return
	}
	release, err := s.aiLimiter.acquire(r.Context(), userID, store.AIOperationResearch, s.now().UTC())
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer release()

	// The workflow performs two sequential Responses API calls. Keep their
	// shared deadline below the HTTP server write timeout so failures can still
	// be returned as JSON instead of leaving a partially written response.
	ctx, cancel := contextWithTimeout(r, AIHandlerTimeout)
	defer cancel()
	result, err := s.app.GenerateResearch(ctx, request)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}
