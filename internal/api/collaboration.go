package api

import (
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/store"
)

type reviewDecisionRequest struct {
	RevisionID int64  `json:"revision_id"`
	Decision   string `json:"decision,omitempty"`
	Comment    string `json:"comment,omitempty"`
}

type createCommentRequest struct {
	RevisionID *int64 `json:"revision_id,omitempty"`
	ParentID   *int64 `json:"parent_id,omitempty"`
	Body       string `json:"body"`
}

type resolveCommentRequest struct {
	Resolved bool `json:"resolved"`
}

type markNotificationsReadRequest struct {
	WorkspaceID string `json:"workspace_id,omitempty"`
}

func (s *Server) listPostRevisions(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsRead)
	if !ok {
		return
	}
	revisions, err := s.app.Store().ListPostRevisions(r.Context(), access.UserID, access.WorkspaceID, postID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, revisions)
}

func (s *Server) listPostReviews(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsRead)
	if !ok {
		return
	}
	reviews, err := s.app.Store().ListPostReviews(r.Context(), access.UserID, access.WorkspaceID, postID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, reviews)
}

func (s *Server) submitPostReview(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityReviewSubmit)
	if !ok {
		return
	}
	revision, err := s.app.Store().SubmitPostForReview(r.Context(), access.UserID, access.WorkspaceID, postID, s.now().UTC())
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"review_status": store.ReviewStatusInReview,
		"revision":      revision,
	})
}

func (s *Server) decidePostReview(w http.ResponseWriter, r *http.Request) {
	s.decidePostReviewWithDecision(w, r, "")
}

func (s *Server) approvePostReview(w http.ResponseWriter, r *http.Request) {
	s.decidePostReviewWithDecision(w, r, store.ReviewDecisionApproved)
}

func (s *Server) requestPostChanges(w http.ResponseWriter, r *http.Request) {
	s.decidePostReviewWithDecision(w, r, store.ReviewDecisionChangesRequested)
}

func (s *Server) decidePostReviewWithDecision(w http.ResponseWriter, r *http.Request, forcedDecision string) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityReviewDecide)
	if !ok {
		return
	}
	var request reviewDecisionRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	decision := forcedDecision
	if decision == "" {
		decision = strings.TrimSpace(request.Decision)
	}
	if request.RevisionID <= 0 || (decision != store.ReviewDecisionApproved && decision != store.ReviewDecisionChangesRequested) {
		s.problem(w, http.StatusBadRequest, "validation_error", "A current revision_id and valid decision are required", nil)
		return
	}
	if utf8.RuneCountInString(strings.TrimSpace(request.Comment)) > 4000 {
		s.problem(w, http.StatusBadRequest, "validation_error", "Review comment must not exceed 4000 characters", nil)
		return
	}
	review, err := s.app.Store().DecidePostReview(r.Context(), access.UserID, access.WorkspaceID,
		postID, request.RevisionID, decision, request.Comment, s.now().UTC())
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, review)
}

func (s *Server) listPostComments(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityCommentsRead)
	if !ok {
		return
	}
	comments, err := s.app.Store().ListPostComments(r.Context(), access.UserID, access.WorkspaceID, postID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, comments)
}

func (s *Server) createPostComment(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityCommentsWrite)
	if !ok {
		return
	}
	var request createCommentRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	request.Body = strings.TrimSpace(request.Body)
	if request.Body == "" || utf8.RuneCountInString(request.Body) > 4000 ||
		(request.RevisionID != nil && *request.RevisionID <= 0) || (request.ParentID != nil && *request.ParentID <= 0) {
		s.problem(w, http.StatusBadRequest, "validation_error", "Comment body and references are invalid", nil)
		return
	}
	comment, err := s.app.Store().CreatePostComment(r.Context(), access.UserID, store.PostComment{
		WorkspaceID: access.WorkspaceID, PostID: postID, RevisionID: request.RevisionID,
		ParentID: request.ParentID, Body: request.Body, CreatedAt: s.now().UTC(),
	})
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, comment)
}

func (s *Server) deletePostComment(w http.ResponseWriter, r *http.Request) {
	_, access, _, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityCommentsWrite)
	if !ok {
		return
	}
	commentID, err := parsePositivePathID(r, "comment_id")
	if err != nil {
		s.writeError(w, err)
		return
	}
	if err := s.app.Store().DeletePostComment(r.Context(), access.UserID, access.WorkspaceID, commentID, s.now().UTC()); err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) resolvePostComment(w http.ResponseWriter, r *http.Request) {
	_, access, _, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityCommentsResolve)
	if !ok {
		return
	}
	commentID, err := parsePositivePathID(r, "comment_id")
	if err != nil {
		s.writeError(w, err)
		return
	}
	var request resolveCommentRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	comment, err := s.app.Store().ResolvePostComment(
		r.Context(), access.UserID, access.WorkspaceID, commentID, request.Resolved, s.now().UTC())
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, comment)
}

func (s *Server) listWorkspaceAudit(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityAuditRead)
	if !ok {
		return
	}
	limit, before, valid := s.parseListCursor(w, r)
	if !valid {
		return
	}
	events, err := s.app.Store().ListAuditEvents(r.Context(), access.UserID, access.WorkspaceID, limit, before)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, events)
}

func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	workspaceID := strings.TrimSpace(r.URL.Query().Get("workspace_id"))
	if workspaceID != "" {
		if _, access, resolveErr := s.app.ResolveWorkspaceAccess(r.Context(), userID, workspaceID); resolveErr != nil {
			s.writeError(w, resolveErr)
			return
		} else if !access.Can(app.CapabilityNotificationsRead) {
			s.problem(w, http.StatusForbidden, "workspace_forbidden", "Notifications are not available", nil)
			return
		}
	}
	limit, before, valid := s.parseListCursor(w, r)
	if !valid {
		return
	}
	unreadOnly := r.URL.Query().Get("unread_only") == "true"
	notifications, err := s.app.Store().ListNotifications(r.Context(), userID, workspaceID, unreadOnly, limit, before)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, notifications)
}

func (s *Server) markNotificationRead(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	notificationID, err := parsePositivePathID(r, "notification_id")
	if err != nil {
		s.writeError(w, err)
		return
	}
	notification, err := s.app.Store().MarkNotificationRead(r.Context(), userID, notificationID, s.now().UTC())
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, notification)
}

func (s *Server) markAllNotificationsRead(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	var request markNotificationsReadRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	request.WorkspaceID = strings.TrimSpace(request.WorkspaceID)
	if request.WorkspaceID != "" {
		if _, access, resolveErr := s.app.ResolveWorkspaceAccess(r.Context(), userID, request.WorkspaceID); resolveErr != nil {
			s.writeError(w, resolveErr)
			return
		} else if !access.Can(app.CapabilityNotificationsManage) {
			s.problem(w, http.StatusForbidden, "workspace_forbidden", "Notifications cannot be changed", nil)
			return
		}
	}
	count, err := s.app.Store().MarkAllNotificationsRead(r.Context(), userID, request.WorkspaceID, s.now().UTC())
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]int64{"updated_count": count})
}

func (s *Server) requireWorkspacePostCapability(
	w http.ResponseWriter, r *http.Request, capability app.Capability,
) (store.Workspace, app.AccessContext, int64, bool) {
	workspace, access, ok := s.requireWorkspaceCapability(w, r, capability)
	if !ok {
		return store.Workspace{}, app.AccessContext{}, 0, false
	}
	postID, err := parsePositivePathID(r, "post_id")
	if err != nil {
		s.writeError(w, err)
		return store.Workspace{}, app.AccessContext{}, 0, false
	}
	if _, err := s.app.Store().GetPostForWorkspace(r.Context(), access.UserID, access.WorkspaceID, postID); err != nil {
		s.writeError(w, err)
		return store.Workspace{}, app.AccessContext{}, 0, false
	}
	return workspace, access, postID, true
}

func (s *Server) parseListCursor(w http.ResponseWriter, r *http.Request) (int, int64, bool) {
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			s.problem(w, http.StatusBadRequest, "validation_error", "limit must be between 1 and 100", nil)
			return 0, 0, false
		}
		limit = value
	}
	var before int64
	if raw := strings.TrimSpace(r.URL.Query().Get("before_id")); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			s.problem(w, http.StatusBadRequest, "validation_error", "before_id must be positive", nil)
			return 0, 0, false
		}
		before = value
	}
	return limit, before, true
}
