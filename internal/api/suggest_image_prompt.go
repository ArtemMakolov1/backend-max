package api

import (
	"net/http"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

func (s *Server) suggestImagePrompt(w http.ResponseWriter, r *http.Request) {
	userID, authErr := authenticatedUserID(r)
	if authErr != nil {
		s.writeError(w, authErr)
		return
	}
	var request openairesearch.SuggestImagePromptRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	if err := openairesearch.ValidateSuggestImagePromptRequest(request); err != nil {
		s.writeError(w, err)
		return
	}
	if !s.app.ImagePromptSuggestionConfigured() {
		s.writeError(w, app.ErrResearchNotConfigured)
		return
	}
	release, err := s.aiLimiter.acquireMetric(
		r.Context(), userID, store.AIOperationResearch,
		store.UsageMetricAIFormatRequests, 1, s.now().UTC())
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer release()

	ctx, cancel := contextWithTimeout(r, AIHandlerTimeout)
	defer cancel()
	result, err := s.app.SuggestImagePrompt(ctx, request)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}
