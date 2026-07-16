package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"maxpilot/backend/internal/app"
)

func (s *Server) updateWorkspacePublishedPost(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsPublish)
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(r, 3*time.Minute)
	defer cancel()
	post, err := s.app.UpdatePublishedPostForWorkspace(ctx, access.UserID, access.WorkspaceID, postID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, scopeWorkspacePostMedia(post, access.WorkspaceID))
}

func (s *Server) syncWorkspaceMAXPublication(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsPublish)
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(r, time.Minute)
	defer cancel()
	post, err := s.app.SyncMAXPublicationForWorkspace(ctx, access.UserID, access.WorkspaceID, postID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, scopeWorkspacePostMedia(post, access.WorkspaceID))
}

func (s *Server) pinWorkspacePost(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsPublish)
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(r, time.Minute)
	defer cancel()
	post, err := s.app.PinPostForWorkspace(ctx, access.UserID, access.WorkspaceID, postID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, scopeWorkspacePostMedia(post, access.WorkspaceID))
}

func (s *Server) unpinWorkspacePost(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsPublish)
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(r, time.Minute)
	defer cancel()
	post, err := s.app.UnpinPostForWorkspace(ctx, access.UserID, access.WorkspaceID, postID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, scopeWorkspacePostMedia(post, access.WorkspaceID))
}

func (s *Server) deleteWorkspacePublication(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsPublish)
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(r, time.Minute)
	defer cancel()
	post, err := s.app.DeletePublicationForWorkspace(ctx, access.UserID, access.WorkspaceID, postID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, scopeWorkspacePostMedia(post, access.WorkspaceID))
}

func (s *Server) getWorkspacePostViewHistory(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsRead)
	if !ok {
		return
	}
	limit := 500
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > 1000 {
			s.problem(w, http.StatusBadRequest, "validation_error", "limit must be between 1 and 1000", nil)
			return
		}
		limit = parsed
	}
	var before *time.Time
	if raw := strings.TrimSpace(r.URL.Query().Get("before")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			s.problem(w, http.StatusBadRequest, "validation_error", "before must be an RFC3339 timestamp", nil)
			return
		}
		parsed = parsed.UTC()
		before = &parsed
	}
	snapshots, err := s.app.Store().ListPostViewSnapshotsForWorkspace(
		r.Context(), access.UserID, access.WorkspaceID, postID, before, limit)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, snapshots)
}
