package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"maxpilot/backend/internal/store"
)

const maxIdentityLinkTTL = 10 * time.Minute

type maxIdentityPublicStatus struct {
	Status         string     `json:"status"`
	RequestID      string     `json:"request_id,omitempty"`
	MAXUserID      string     `json:"max_user_id,omitempty"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	ComparisonCode string     `json:"comparison_code,omitempty"`
	RequesterLabel string     `json:"requester_label,omitempty"`
	ErrorCode      string     `json:"error_code,omitempty"`
	LinkedAt       *time.Time `json:"linked_at,omitempty"`
	BotURL         string     `json:"bot_url,omitempty"`
}

func (s *Server) getMAXIdentity(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	now := s.now().UTC()
	attempt, attemptErr := s.app.Store().GetLatestMAXIdentityLinkAttemptForUser(r.Context(), userID, now)
	if attemptErr == nil && (attempt.Status == store.MAXIdentityAttemptPending || attempt.Status == store.MAXIdentityAttemptAwaitingConfirmation) {
		status := publicMAXIdentityAttempt(attempt)
		s.writeJSON(w, http.StatusOK, map[string]any{"identity": status})
		return
	}
	if attemptErr != nil && !errors.Is(attemptErr, store.ErrNotFound) {
		s.writeError(w, attemptErr)
		return
	}
	link, linkErr := s.app.Store().GetMAXIdentityLinkForUser(r.Context(), userID)
	if linkErr == nil {
		linkedAt := link.LinkedAt
		s.writeJSON(w, http.StatusOK, map[string]any{"identity": maxIdentityPublicStatus{
			Status: store.MAXIdentityAttemptLinked, MAXUserID: link.MAXUserID, LinkedAt: &linkedAt,
		}})
		return
	}
	if !errors.Is(linkErr, store.ErrNotFound) {
		s.writeError(w, linkErr)
		return
	}
	if attemptErr == nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"identity": publicMAXIdentityAttempt(attempt)})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"identity": maxIdentityPublicStatus{Status: "unlinked"}})
}

func (s *Server) startMAXIdentity(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if link, linkErr := s.app.Store().GetMAXIdentityLinkForUser(r.Context(), userID); linkErr == nil {
		linkedAt := link.LinkedAt
		s.writeJSON(w, http.StatusOK, map[string]any{"identity": maxIdentityPublicStatus{
			Status: store.MAXIdentityAttemptLinked, MAXUserID: link.MAXUserID, LinkedAt: &linkedAt,
		}})
		return
	} else if !errors.Is(linkErr, store.ErrNotFound) {
		s.writeError(w, linkErr)
		return
	}
	ctx, cancel := contextWithTimeout(r, 8*time.Second)
	bot, err := s.app.TestMAX(ctx)
	cancel()
	if err != nil {
		s.writeError(w, err)
		return
	}
	username := strings.TrimPrefix(strings.TrimSpace(bot.Username), "@")
	if username == "" {
		s.problem(w, http.StatusServiceUnavailable, "max_bot_username_missing", "MAX bot username is missing", nil)
		return
	}
	requestID, err := randomURLToken(32)
	if err != nil {
		s.writeError(w, err)
		return
	}
	deepToken, err := randomURLToken(32)
	if err != nil {
		s.writeError(w, err)
		return
	}
	comparisonCode, err := randomComparisonCode()
	if err != nil {
		s.writeError(w, err)
		return
	}
	user, err := s.app.Store().GetUser(r.Context(), userID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	now := s.now().UTC()
	attempt := store.MAXIdentityLinkAttempt{
		ID: requestID, TokenHash: sha256Hex(deepToken), UserID: userID,
		RequesterLabel: safeRequesterLabel(firstNonEmpty(user.Login, user.Email, user.DisplayName, user.ID)),
		ComparisonCode: comparisonCode, Status: store.MAXIdentityAttemptPending,
		CreatedAt: now, ExpiresAt: now.Add(maxIdentityLinkTTL), UpdatedAt: now,
	}
	if err := s.app.Store().CreateMAXIdentityLinkAttempt(r.Context(), attempt); err != nil {
		if errors.Is(err, store.ErrConflict) {
			if link, linkErr := s.app.Store().GetMAXIdentityLinkForUser(r.Context(), userID); linkErr == nil {
				linkedAt := link.LinkedAt
				s.writeJSON(w, http.StatusOK, map[string]any{"identity": maxIdentityPublicStatus{
					Status: store.MAXIdentityAttemptLinked, MAXUserID: link.MAXUserID, LinkedAt: &linkedAt,
				}})
				return
			}
		}
		s.writeError(w, err)
		return
	}
	status := publicMAXIdentityAttempt(attempt)
	status.BotURL = "https://max.ru/" + url.PathEscape(username) + "?start=" + url.QueryEscape("link_"+deepToken)
	s.writeJSON(w, http.StatusCreated, map[string]any{"identity": status})
}

func publicMAXIdentityAttempt(attempt store.MAXIdentityLinkAttempt) maxIdentityPublicStatus {
	expiresAt := attempt.ExpiresAt
	return maxIdentityPublicStatus{
		Status: attempt.Status, RequestID: attempt.ID, ExpiresAt: &expiresAt,
		ComparisonCode: attempt.ComparisonCode, RequesterLabel: attempt.RequesterLabel, ErrorCode: attempt.ErrorCode,
	}
}
