package api

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"maxpilot/backend/internal/store"
)

const (
	maxAuthAttemptCookieName = "maxstudio_max_auth_attempt"
	maxAuthAttemptTTL        = 10 * time.Minute
)

type maxAuthPublicAttempt struct {
	RequestID      string    `json:"request_id"`
	Status         string    `json:"status"`
	BotURL         string    `json:"bot_url,omitempty"`
	ComparisonCode string    `json:"comparison_code,omitempty"`
	ExpiresAt      time.Time `json:"expires_at"`
	ReturnTo       string    `json:"return_to,omitempty"`
}

func (s *Server) startMAXAuth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Header.Get("Origin") != s.frontendOrigin {
		s.problem(w, http.StatusForbidden, "origin_required", "An exact frontend Origin is required to start sign-in", nil)
		return
	}
	if !s.app.MAXConfigured() {
		s.problem(w, http.StatusServiceUnavailable, "max_auth_not_configured", "MAX authentication is not configured", nil)
		return
	}
	var input yandexAuthStartRequest
	if !s.decodeJSON(w, r, &input) {
		return
	}
	if !input.TermsAccepted || !input.PersonalDataAccepted {
		s.problem(w, http.StatusBadRequest, "consent_required", "Accept the user agreement and personal data consent before signing in", map[string]string{
			"terms_version": termsVersion, "personal_data_version": personalDataVersion,
		})
		return
	}
	if allowed, retryAfter := s.oauthStartLimiter.Allow(s.oauthClientKey(r), s.now().UTC()); !allowed {
		seconds := int64((retryAfter + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		s.problem(w, http.StatusTooManyRequests, "oauth_rate_limited", "Too many sign-in attempts; retry later", nil)
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
	requestID, err := randomURLToken(24)
	if err != nil {
		s.writeError(w, err)
		return
	}
	browserToken, err := randomURLToken(32)
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
	now := s.now().UTC()
	attempt := store.MAXAuthAttempt{
		ID: requestID, BrowserTokenHash: sha256Hex(browserToken), DeepTokenHash: sha256Hex(deepToken),
		ReturnTo: safeReturnTo(input.ReturnTo), ComparisonCode: comparisonCode, Status: store.MAXAuthAttemptPending,
		TermsVersion: termsVersion, PersonalDataVersion: personalDataVersion, ConsentAt: now,
		CreatedAt: now, ExpiresAt: now.Add(maxAuthAttemptTTL), UpdatedAt: now,
	}
	if err := s.app.Store().CreateMAXAuthAttempt(r.Context(), attempt); err != nil {
		s.writeError(w, err)
		return
	}
	s.setMAXAuthAttemptCookie(w, browserToken, int(maxAuthAttemptTTL.Seconds()))
	botURL := "https://max.ru/" + url.PathEscape(username) + "?start=" + url.QueryEscape("auth_"+deepToken)
	s.writeJSON(w, http.StatusCreated, maxAuthPublicAttempt{
		RequestID: requestID, Status: attempt.Status, BotURL: botURL,
		ComparisonCode: comparisonCode, ExpiresAt: attempt.ExpiresAt, ReturnTo: attempt.ReturnTo,
	})
}

func (s *Server) completeMAXAuth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Header.Get("Origin") != s.frontendOrigin {
		s.problem(w, http.StatusForbidden, "origin_required", "An exact frontend Origin is required to complete sign-in", nil)
		return
	}
	requestID := strings.TrimSpace(chi.URLParam(r, "request_id"))
	cookie, err := r.Cookie(maxAuthAttemptCookieName)
	if err != nil || cookie.Value == "" || requestID == "" {
		s.problem(w, http.StatusNotFound, "max_auth_attempt_not_found", "MAX sign-in attempt was not found", nil)
		return
	}
	now := s.now().UTC()
	attempt, err := s.app.Store().GetMAXAuthAttemptForBrowser(r.Context(), requestID, sha256Hex(cookie.Value), now)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.problem(w, http.StatusNotFound, "max_auth_attempt_not_found", "MAX sign-in attempt was not found", nil)
			return
		}
		s.writeError(w, err)
		return
	}
	switch attempt.Status {
	case store.MAXAuthAttemptPending, store.MAXAuthAttemptAwaitingContact:
		s.writeJSON(w, http.StatusOK, maxAuthPublicAttempt{
			RequestID: attempt.ID, Status: attempt.Status, ComparisonCode: attempt.ComparisonCode,
			ExpiresAt: attempt.ExpiresAt, ReturnTo: attempt.ReturnTo,
		})
		return
	case store.MAXAuthAttemptExpired, store.MAXAuthAttemptCancelled, store.MAXAuthAttemptFailed:
		s.clearMAXAuthAttemptCookie(w)
		s.writeJSON(w, http.StatusOK, maxAuthPublicAttempt{
			RequestID: attempt.ID, Status: attempt.Status, ExpiresAt: attempt.ExpiresAt,
		})
		return
	case store.MAXAuthAttemptAuthenticated:
		s.clearMAXAuthAttemptCookie(w)
		s.problem(w, http.StatusConflict, "max_auth_attempt_used", "MAX sign-in attempt was already completed", nil)
		return
	case store.MAXAuthAttemptVerified:
		// Continue below and atomically exchange the verified attempt for a
		// provider-neutral browser session.
	default:
		s.problem(w, http.StatusConflict, "max_auth_state_invalid", "MAX sign-in attempt is in an invalid state", nil)
		return
	}
	sessionToken, err := randomURLToken(32)
	if err != nil {
		s.writeError(w, err)
		return
	}
	newOwnerToken, err := randomURLToken(24)
	if err != nil {
		s.writeError(w, err)
		return
	}
	prospectiveOwnerID := "max_" + newOwnerToken
	session, err := s.app.Store().CompleteMAXAuthAttempt(r.Context(), requestID, sha256Hex(cookie.Value),
		prospectiveOwnerID, store.AuthSession{
			TokenHash: sha256Hex(sessionToken), Provider: "max",
			CreatedAt: now, ExpiresAt: now.Add(s.sessionTTL),
		}, now)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.problem(w, http.StatusConflict, "max_auth_state_invalid", "MAX sign-in attempt could not be completed", nil)
			return
		}
		s.writeError(w, err)
		return
	}
	s.setSessionCookie(w, sessionToken, int(s.sessionTTL.Seconds()))
	s.clearMAXAuthAttemptCookie(w)
	expiresAt := session.ExpiresAt
	s.writeJSON(w, http.StatusOK, map[string]any{
		"request_id": requestID, "status": store.MAXAuthAttemptAuthenticated, "return_to": attempt.ReturnTo,
		"auth_method": "max", "session_expires_at": expiresAt,
		"is_new_user": session.OwnerID == prospectiveOwnerID,
		"user": authUser{ID: session.OwnerID, Provider: "max", Login: session.Login,
			DisplayName: firstNonEmpty(session.DisplayName, session.Login, "Пользователь MAX"), AvatarURL: session.AvatarURL},
	})
}

func (s *Server) cancelMAXAuth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Header.Get("Origin") != s.frontendOrigin {
		s.problem(w, http.StatusForbidden, "origin_required", "An exact frontend Origin is required to cancel sign-in", nil)
		return
	}
	requestID := strings.TrimSpace(chi.URLParam(r, "request_id"))
	cookie, err := r.Cookie(maxAuthAttemptCookieName)
	if err != nil || cookie.Value == "" || requestID == "" {
		s.problem(w, http.StatusNotFound, "max_auth_attempt_not_found", "MAX sign-in attempt was not found", nil)
		return
	}
	if err := s.app.Store().CancelMAXAuthAttempt(r.Context(), requestID, sha256Hex(cookie.Value), s.now().UTC()); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.writeError(w, err)
		return
	}
	s.clearMAXAuthAttemptCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setMAXAuthAttemptCookie(w http.ResponseWriter, value string, maxAge int) {
	// #nosec G124 -- production uses Secure; loopback development is validated by configuration.
	http.SetCookie(w, &http.Cookie{
		Name: maxAuthAttemptCookieName, Value: value, Path: "/api/v1/auth/max", MaxAge: maxAge,
		HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) clearMAXAuthAttemptCookie(w http.ResponseWriter) {
	// #nosec G124 -- deletion attributes intentionally match the original cookie.
	http.SetCookie(w, &http.Cookie{
		Name: maxAuthAttemptCookieName, Path: "/api/v1/auth/max", MaxAge: -1, Expires: time.Unix(1, 0),
		HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteStrictMode,
	})
}
