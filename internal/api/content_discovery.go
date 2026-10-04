package api

import (
	"net/http"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

type discoverWorkspaceContentRequest struct {
	openairesearch.DiscoverContentRequest
	ChannelID *int64 `json:"channel_id,omitempty"`
}

func (s *Server) discoverWorkspaceContent(w http.ResponseWriter, r *http.Request) {
	workspace, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityAIUse)
	if !ok {
		return
	}
	var request discoverWorkspaceContentRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	if err := openairesearch.ValidateDiscoverContentInput(request.DiscoverContentRequest); err != nil {
		s.writeError(w, err)
		return
	}
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	// This short path uses one Responses call, bounded below the server deadline.
	ctx, cancel := contextWithTimeout(r, 90*time.Second)
	defer cancel()
	result, err := s.app.DiscoverContentForWorkspaceWithBeforeGenerate(
		ctx, access.UserID, access.WorkspaceID, request.ChannelID, request.DiscoverContentRequest,
		func() error {
			var err error
			release, err = s.aiLimiter.acquireForWorkspaceMetric(ctx, access.UserID, workspace,
				store.AIOperationResearch, store.UsageMetricAIResearchRequests, 1, s.now().UTC())
			return err
		})
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, result)
}
