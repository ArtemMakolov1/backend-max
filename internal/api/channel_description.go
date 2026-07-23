package api

import (
	"net/http"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

func (s *Server) suggestChannelDescription(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	channelID, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	var input openairesearch.SuggestChannelDescriptionRequest
	if !s.decodeJSON(w, r, &input) {
		return
	}
	if err := openairesearch.ValidateSuggestChannelDescriptionInput(input); err != nil {
		s.writeError(w, err)
		return
	}
	// Authorize before reserving paid AI quota. The app repeats the scoped
	// lookup while assembling authoritative metadata and post samples.
	if _, err := s.app.Store().GetChannelForUser(r.Context(), userID, channelID); err != nil {
		s.writeError(w, err)
		return
	}
	if !s.app.ChannelDescriptionSuggestionConfigured() {
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
	result, err := s.app.SuggestChannelDescriptionForUser(ctx, userID, channelID, input)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) testWorkspaceChannel(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityChannelsRead)
	if !ok {
		return
	}
	channelID, err := parsePositivePathID(r, "channel_id")
	if err != nil {
		s.writeError(w, err)
		return
	}
	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()
	check, err := s.app.TestChannelForWorkspace(ctx, access.UserID, access.WorkspaceID, channelID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	ready := check.Diagnostics.CanPublish
	message := "Channel is ready for publishing"
	if !ready {
		message = "Channel or bot permissions require attention"
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": ready, "message": message, "channel": check.Channel, "diagnostics": check.Diagnostics,
	})
}

func (s *Server) suggestWorkspaceChannelDescription(w http.ResponseWriter, r *http.Request) {
	workspace, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityAIUse)
	if !ok {
		return
	}
	channelID, err := parsePositivePathID(r, "channel_id")
	if err != nil {
		s.writeError(w, err)
		return
	}
	var input openairesearch.SuggestChannelDescriptionRequest
	if !s.decodeJSON(w, r, &input) {
		return
	}
	if err := openairesearch.ValidateSuggestChannelDescriptionInput(input); err != nil {
		s.writeError(w, err)
		return
	}
	if _, err := s.app.Store().GetChannelForWorkspace(
		r.Context(), access.UserID, access.WorkspaceID, channelID); err != nil {
		s.writeError(w, err)
		return
	}
	if !s.app.ChannelDescriptionSuggestionConfigured() {
		s.writeError(w, app.ErrResearchNotConfigured)
		return
	}
	release, err := s.aiLimiter.acquireForWorkspaceMetric(
		r.Context(), access.UserID, workspace, store.AIOperationResearch,
		store.UsageMetricAIFormatRequests, 1, s.now().UTC())
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer release()
	ctx, cancel := contextWithTimeout(r, AIHandlerTimeout)
	defer cancel()
	result, err := s.app.SuggestChannelDescriptionForWorkspace(
		ctx, access.UserID, access.WorkspaceID, channelID, input)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, result)
}
