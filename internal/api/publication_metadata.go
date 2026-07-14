package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) syncMAXPublication(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	postID, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	ctx, cancel := contextWithTimeout(r, time.Minute)
	defer cancel()
	post, err := s.app.SyncMAXPublication(ctx, userID, postID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, post)
}

func (s *Server) pinPost(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	postID, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	ctx, cancel := contextWithTimeout(r, time.Minute)
	defer cancel()
	post, err := s.app.PinPost(ctx, userID, postID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, post)
}

func (s *Server) unpinPost(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	postID, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	ctx, cancel := contextWithTimeout(r, time.Minute)
	defer cancel()
	post, err := s.app.UnpinPost(ctx, userID, postID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, post)
}

func (s *Server) getPostViewHistory(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	postID, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	limit := 500
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed <= 0 || parsed > 1000 {
			s.problem(w, http.StatusBadRequest, "validation_error", "limit must be between 1 and 1000", nil)
			return
		}
		limit = parsed
	}
	var before *time.Time
	if raw := strings.TrimSpace(r.URL.Query().Get("before")); raw != "" {
		parsed, parseErr := time.Parse(time.RFC3339, raw)
		if parseErr != nil {
			s.problem(w, http.StatusBadRequest, "validation_error", "before must be an RFC3339 timestamp", nil)
			return
		}
		parsed = parsed.UTC()
		before = &parsed
	}
	snapshots, err := s.app.Store().ListPostViewSnapshotsForUser(r.Context(), userID, postID, before, limit)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, snapshots)
}
