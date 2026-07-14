package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexauth"
)

const (
	sessionCookieName = "maxstudio_session"
	stateCookieName   = "maxstudio_oauth_state"
	oauthStateTTL     = 10 * time.Minute
)

type authUser struct {
	ID          string `json:"id"`
	Provider    string `json:"provider"`
	Login       string `json:"login,omitempty"`
	DisplayName string `json:"display_name"`
}

type authPrincipal struct {
	Method    string
	User      *authUser
	ExpiresAt *time.Time
}

type authStatusPayload struct {
	Required         bool            `json:"auth_required"`
	Authenticated    bool            `json:"authenticated"`
	Methods          map[string]bool `json:"auth_methods"`
	Method           string          `json:"auth_method"`
	User             *authUser       `json:"user"`
	SessionExpiresAt *time.Time      `json:"session_expires_at,omitempty"`
}

func (s *Server) authRequired() bool {
	return s.adminAPIKey != "" || s.yandexClient != nil
}

func (s *Server) validAdminKey(r *http.Request) bool {
	if s.adminAPIKey == "" {
		return false
	}
	provided := r.Header.Get("X-Admin-Key")
	return len(provided) == len(s.adminAPIKey) && subtle.ConstantTimeCompare([]byte(provided), []byte(s.adminAPIKey)) == 1
}

func (s *Server) authenticate(r *http.Request) (authPrincipal, bool) {
	if s.validAdminKey(r) {
		return authPrincipal{Method: "admin_key", User: &authUser{
			ID: "admin", Provider: "admin_key", DisplayName: "Администратор",
		}}, true
	}
	if s.yandexClient != nil {
		if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
			tokenHash := sha256Hex(cookie.Value)
			session, err := s.app.Store().GetAuthSession(r.Context(), tokenHash, s.now().UTC())
			if err == nil {
				if _, allowed := s.yandexAllowed[strings.ToLower(strings.TrimSpace(session.AllowlistIdentity))]; !allowed {
					if deleteErr := s.app.Store().DeleteAuthSession(r.Context(), tokenHash); deleteErr != nil {
						s.logger.Warn("revoked auth session cleanup failed", "error", deleteErr)
					}
					return authPrincipal{}, false
				}
				expiresAt := session.ExpiresAt
				return authPrincipal{Method: "yandex", ExpiresAt: &expiresAt, User: &authUser{
					ID: session.YandexUserID, Provider: "yandex", Login: session.Login,
					DisplayName: firstNonEmpty(session.DisplayName, session.Login, "Пользователь Яндекса"),
				}}, true
			}
			if !errors.Is(err, store.ErrNotFound) {
				s.logger.Warn("auth session lookup failed", "error", err)
			}
		}
	}
	if !s.authRequired() {
		return authPrincipal{Method: "none"}, true
	}
	return authPrincipal{}, false
}

func (s *Server) authenticationStatus(r *http.Request) authStatusPayload {
	principal, authenticated := s.authenticate(r)
	return authStatusPayload{
		Required: s.authRequired(), Authenticated: authenticated,
		Methods: map[string]bool{"yandex": s.yandexClient != nil, "admin_key": s.adminAPIKey != ""},
		Method:  principal.Method, User: principal.User, SessionExpiresAt: principal.ExpiresAt,
	}
}

func (s *Server) authSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, s.authenticationStatus(r))
}

func (s *Server) startYandexAuth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if s.yandexClient == nil {
		s.problem(w, http.StatusServiceUnavailable, "yandex_auth_not_configured", "Yandex authentication is not configured", nil)
		return
	}
	if allowed, retryAfter := s.oauthStartLimiter.Allow(s.oauthClientKey(r), s.now().UTC()); !allowed {
		seconds := int64((retryAfter + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		s.problem(w, http.StatusTooManyRequests, "oauth_rate_limited", "Too many sign-in attempts; retry later", nil)
		return
	}
	state, err := randomURLToken(32)
	if err != nil {
		s.writeError(w, err)
		return
	}
	verifier, err := randomURLToken(32)
	if err != nil {
		s.writeError(w, err)
		return
	}
	now := s.now().UTC()
	returnTo := safeReturnTo(r.URL.Query().Get("return_to"))
	if err := s.app.Store().CreateOAuthState(r.Context(), store.OAuthState{
		StateHash: sha256Hex(state), PKCEVerifier: verifier, ReturnTo: returnTo,
		CreatedAt: now, ExpiresAt: now.Add(oauthStateTTL),
	}); err != nil {
		s.writeError(w, err)
		return
	}
	s.setStateCookie(w, state, int(oauthStateTTL.Seconds()))
	challengeBytes := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeBytes[:])
	http.Redirect(w, r, s.yandexClient.AuthorizationURL(s.yandexRedirect, state, challenge), http.StatusFound)
}

func (s *Server) oauthClientKey(r *http.Request) string {
	if s.trustXRealIP {
		if forwarded := strings.TrimSpace(r.Header.Get("X-Real-IP")); forwarded != "" && net.ParseIP(forwarded) != nil {
			return forwarded
		}
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil && net.ParseIP(host) != nil {
		return host
	}
	if direct := strings.TrimSpace(r.RemoteAddr); direct != "" {
		return direct
	}
	return "unknown"
}

func (s *Server) finishYandexAuth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	s.clearStateCookie(w)
	if s.yandexClient == nil {
		s.redirectAuthError(w, r, "oauth_unavailable")
		return
	}
	providedState := r.URL.Query().Get("state")
	stateCookie, err := r.Cookie(stateCookieName)
	if err != nil || !constantTimeEqual(providedState, stateCookie.Value) {
		s.redirectAuthError(w, r, "state_invalid")
		return
	}
	state, err := s.app.Store().ConsumeOAuthState(r.Context(), sha256Hex(providedState), s.now().UTC())
	if err != nil {
		s.redirectAuthError(w, r, "state_invalid")
		return
	}
	if r.URL.Query().Get("error") != "" {
		s.redirectAuthError(w, r, "access_denied")
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		s.redirectAuthError(w, r, "oauth_unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	accessToken, err := s.yandexClient.ExchangeCode(ctx, code, state.PKCEVerifier)
	if err != nil {
		s.logger.Warn("Yandex OAuth code exchange failed", "error", err)
		s.redirectAuthError(w, r, "oauth_unavailable")
		return
	}
	profile, err := s.yandexClient.UserInfo(ctx, accessToken)
	if err != nil {
		s.logger.Warn("Yandex OAuth profile request failed", "error", err)
		s.redirectAuthError(w, r, "oauth_unavailable")
		return
	}
	allowlistIdentity, allowed := s.yandexProfileAllowed(profile)
	if !allowed {
		s.logger.Warn("Yandex OAuth account is not allowlisted", "yandex_user_id", profile.ID)
		s.redirectAuthError(w, r, "account_not_allowed")
		return
	}
	sessionToken, err := randomURLToken(32)
	if err != nil {
		s.writeError(w, err)
		return
	}
	now := s.now().UTC()
	yandexUserID := firstNonEmpty(profile.PSUID, profile.ID)
	displayName := firstNonEmpty(profile.DisplayName, profile.RealName,
		strings.TrimSpace(profile.FirstName+" "+profile.LastName), profile.Login, "Пользователь Яндекса")
	if err := s.app.Store().CreateAuthSession(r.Context(), store.AuthSession{
		TokenHash: sha256Hex(sessionToken), YandexUserID: yandexUserID,
		Login: profile.Login, Email: profile.DefaultEmail, DisplayName: displayName,
		AllowlistIdentity: allowlistIdentity,
		CreatedAt:         now, ExpiresAt: now.Add(s.sessionTTL),
	}); err != nil {
		s.writeError(w, err)
		return
	}
	s.setSessionCookie(w, sessionToken, int(s.sessionTTL.Seconds()))
	http.Redirect(w, r, s.frontendOrigin+state.ReturnTo, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
		if err := s.app.Store().DeleteAuthSession(r.Context(), sha256Hex(cookie.Value)); err != nil {
			s.writeError(w, err)
			return
		}
	}
	s.clearSessionCookie(w)
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) yandexProfileAllowed(profile yandexauth.Profile) (string, bool) {
	candidates := append([]string{profile.ID, profile.PSUID, profile.Login, profile.DefaultEmail}, profile.Emails...)
	for _, candidate := range candidates {
		normalized := strings.ToLower(strings.TrimSpace(candidate))
		if _, ok := s.yandexAllowed[normalized]; ok {
			return normalized, true
		}
	}
	return "", false
}

func (s *Server) redirectAuthError(w http.ResponseWriter, r *http.Request, code string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, s.frontendOrigin+"/app/?auth_error="+url.QueryEscape(code), http.StatusSeeOther)
}

func (s *Server) setStateCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: stateCookieName, Value: value, Path: "/api/v1/auth/yandex/callback",
		MaxAge: maxAge, HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: stateCookieName, Path: "/api/v1/auth/yandex/callback", MaxAge: -1,
		Expires: time.Unix(1, 0), HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Path: "/", MaxAge: -1, Expires: time.Unix(1, 0),
		HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode,
	})
}

func randomURLToken(byteCount int) (string, error) {
	data := make([]byte, byteCount)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func constantTimeEqual(left, right string) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func safeReturnTo(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "/app/"
	}
	if strings.Contains(raw, "\\") || strings.HasPrefix(raw, "//") {
		return "/app/"
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || (parsed.Path != "/app" && !strings.HasPrefix(parsed.Path, "/app/")) {
		return "/app/"
	}
	return parsed.String()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func hasSessionCookie(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	return err == nil && cookie.Value != ""
}

func isUnsafeMethod(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}
