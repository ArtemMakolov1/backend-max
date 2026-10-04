package api

import (
	"errors"
	"net/http"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/store"
)

func (s *Server) createWorkspaceContentDiscoveryDraft(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	var request app.CreateDiscoveryDraftRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	if err := app.ValidateCreateDiscoveryDraftRequest(request); err != nil {
		s.writeError(w, validationError(err.Error()))
		return
	}
	release, acquired := s.mediaUploads.tryAcquire(access.UserID)
	if !acquired {
		s.writeError(w, errMediaUploadRateLimited)
		return
	}
	defer release()
	ctx, cancel := contextWithTimeout(r, 90*time.Second)
	defer cancel()
	result, err := s.app.CreateContentDiscoveryDraftForWorkspace(ctx, access.UserID, access.WorkspaceID, request)
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		switch {
		case errors.Is(err, store.ErrDiscoveryDraftBusy):
			s.problem(w, http.StatusConflict, "content_discovery_draft_in_progress", "Черновик создаётся. Проверьте результат с тем же идентификатором запроса.", nil)
		case errors.Is(err, store.ErrDiscoveryRequestConflict):
			s.problem(w, http.StatusConflict, "content_discovery_request_conflict", "Идентификатор уже связан с другим запросом создания черновика.", nil)
		case errors.Is(err, store.ErrDiscoveryCandidateExpired):
			s.problem(w, http.StatusGone, "content_discovery_candidate_expired", "Материал устарел. Выполните новый поиск.", nil)
		default:
			s.writeError(w, err)
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, result)
}
