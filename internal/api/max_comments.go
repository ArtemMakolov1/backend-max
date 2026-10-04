package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/maxclient"
)

func (s *Server) registerMAXCommentRoutes(r chi.Router) {
	r.Get("/posts/{post_id}/max-comments", s.listMAXComments)
	r.Post("/posts/{post_id}/max-comments/sync", s.syncMAXComments)
	r.Post("/posts/{post_id}/max-comments", s.sendMAXComment)
	r.Put("/posts/{post_id}/max-comments/{comment_mid}", s.editMAXComment)
	r.Delete("/posts/{post_id}/max-comments/{comment_mid}", s.removeMAXComment)
	r.Post("/posts/{post_id}/max-comments/operations/{operation_id}/reconcile", s.reconcileMAXComment)
}

func (s *Server) handleMAXCommentEvent(w http.ResponseWriter, r *http.Request, update maxUpdate) {
	eventAt, valid := maxEventTime(update.Timestamp, s.now().UTC())
	if !valid {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	ctx, cancel := contextWithTimeout(r, 8*time.Second)
	defer cancel()
	var err error
	if update.UpdateType == "comment_removed" {
		chatID, e := webhookChatID(update.ChatID)
		if e != nil {
			s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
			return
		}
		err = s.app.Store().ObserveMAXCommentRemoval(ctx, chatID, update.PostID, update.MessageID, eventAt)
	} else {
		message, e := maxclient.DecodeCommentMessage(update.MessageRaw)
		if e != nil || time.UnixMilli(message.TimestampMillis).After(s.now().UTC().Add(5*time.Minute)) {
			s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
			return
		}
		err = s.app.ObserveMAXCommentMessage(ctx, message, eventAt)
	}
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) listMAXComments(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	_, access, id, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsRead)
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()
	var requestedIDs []string
	if ids := r.URL.Query().Get("client_request_ids"); ids != "" {
		requestedIDs = strings.Split(ids, ",")
	}
	result, err := s.app.GetMAXComments(ctx, access.UserID, access.WorkspaceID, id, r.URL.Query().Get("expected_root_message_id"), requestedIDs...)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}
func (s *Server) syncMAXComments(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	_, access, id, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsRead)
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()
	result, err := s.app.SyncMAXComments(ctx, access.UserID, access.WorkspaceID, id, r.URL.Query().Get("expected_root_message_id"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}
func (s *Server) sendMAXComment(w http.ResponseWriter, r *http.Request) {
	s.mutateMAXComment(w, r, "send", app.CapabilityPostsWrite)
}
func (s *Server) editMAXComment(w http.ResponseWriter, r *http.Request) {
	s.mutateMAXComment(w, r, "edit", app.CapabilityPostsWrite)
}
func (s *Server) removeMAXComment(w http.ResponseWriter, r *http.Request) {
	s.mutateMAXComment(w, r, "delete", app.CapabilityPostsDelete)
}
func (s *Server) mutateMAXComment(w http.ResponseWriter, r *http.Request, kind string, capability app.Capability) {
	w.Header().Set("Cache-Control", "no-store")
	_, access, id, ok := s.requireWorkspacePostCapability(w, r, capability)
	if !ok {
		return
	}
	var request app.MAXCommentMutation
	if !s.decodeJSON(w, r, &request) {
		return
	}
	ctx, cancel := contextWithTimeout(r, 45*time.Second)
	defer cancel()
	result, err := s.app.MutateMAXComment(ctx, access.UserID, access.WorkspaceID, id, kind, chi.URLParam(r, "comment_mid"), request)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}
func (s *Server) reconcileMAXComment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	_, access, id, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	var request struct {
		MessageID             string `json:"message_id"`
		ExpectedRootMessageID string `json:"expected_root_message_id"`
	}
	if !s.decodeJSON(w, r, &request) {
		return
	}
	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()
	result, err := s.app.ReconcileMAXCommentSend(ctx, access.UserID, access.WorkspaceID, id, request.ExpectedRootMessageID, chi.URLParam(r, "operation_id"), request.MessageID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}
