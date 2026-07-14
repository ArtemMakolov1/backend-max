package api

import (
	"net/http"
	"time"

	"maxpilot/backend/internal/openairesearch"
)

func (s *Server) generateResearch(w http.ResponseWriter, r *http.Request) {
	var request openairesearch.Request
	if !s.decodeJSON(w, r, &request) {
		return
	}
	if err := openairesearch.ValidateRequest(request); err != nil {
		s.writeError(w, err)
		return
	}

	// The workflow performs two sequential Responses API calls. Keep their
	// shared deadline below the HTTP server write timeout so failures can still
	// be returned as JSON instead of leaving a partially written response.
	ctx, cancel := contextWithTimeout(r, 3*time.Minute)
	defer cancel()
	result, err := s.app.GenerateResearch(ctx, request)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}
