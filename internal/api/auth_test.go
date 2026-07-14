package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexauth"
)

type fakeYandexOAuth struct {
	profile            yandexauth.Profile
	authorizationCalls int
	exchangedCode      string
	exchangedVerifier  string
	userInfoToken      string
}

func (f *fakeYandexOAuth) AuthorizationURL(redirectURI, state, challenge string) string {
	f.authorizationCalls++
	values := url.Values{
		"redirect_uri": {redirectURI}, "state": {state}, "code_challenge": {challenge},
		"code_challenge_method": {"S256"},
	}
	return "https://oauth.yandex.test/authorize?" + values.Encode()
}

func (f *fakeYandexOAuth) ExchangeCode(_ context.Context, code, verifier string) (string, error) {
	f.exchangedCode = code
	f.exchangedVerifier = verifier
	return "provider-access-token", nil
}

func (f *fakeYandexOAuth) UserInfo(_ context.Context, token string) (yandexauth.Profile, error) {
	f.userInfoToken = token
	return f.profile, nil
}

func TestYandexOAuthCreatesServerSessionAndLogoutInvalidatesIt(t *testing.T) {
	handler, provider, server := newYandexAuthTestHandler(t, []string{"42"}, yandexauth.Profile{
		ID: "42", PSUID: "app-scoped-42", ClientID: "client-id", Login: "editor",
		DefaultEmail: "editor@example.ru", DisplayName: "Редактор",
	})

	state, stateCookie := beginYandexAuth(t, handler, "/app/#/calendar")
	callback := httptest.NewRequest(http.MethodGet,
		"/api/v1/auth/yandex/callback?code=confirmation-code&state="+url.QueryEscape(state), nil)
	callback.AddCookie(stateCookie)
	callbackResponse := httptest.NewRecorder()
	handler.ServeHTTP(callbackResponse, callback)
	if callbackResponse.Code != http.StatusSeeOther {
		t.Fatalf("callback status = %d, body = %s", callbackResponse.Code, callbackResponse.Body.String())
	}
	if callbackResponse.Header().Get("Referrer-Policy") != "no-referrer" || callbackResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("callback privacy headers = %#v", callbackResponse.Header())
	}
	if got := callbackResponse.Header().Get("Location"); got != "http://localhost:4321/app/#/calendar" {
		t.Fatalf("callback Location = %q", got)
	}
	if provider.exchangedCode != "confirmation-code" || provider.exchangedVerifier == "" || provider.userInfoToken != "provider-access-token" {
		t.Fatalf("provider exchange = %#v", provider)
	}
	sessionCookie := responseCookie(t, callbackResponse, sessionCookieName)
	if !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteLaxMode || sessionCookie.MaxAge <= 0 {
		t.Fatalf("session cookie = %#v", sessionCookie)
	}

	protected := httptest.NewRequest(http.MethodGet, "/api/v1/posts", nil)
	protected.AddCookie(sessionCookie)
	protectedResponse := httptest.NewRecorder()
	handler.ServeHTTP(protectedResponse, protected)
	if protectedResponse.Code != http.StatusOK {
		t.Fatalf("protected status = %d, body = %s", protectedResponse.Code, protectedResponse.Body.String())
	}

	health := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	health.AddCookie(sessionCookie)
	healthResponse := httptest.NewRecorder()
	handler.ServeHTTP(healthResponse, health)
	var healthBody map[string]any
	if err := json.Unmarshal(healthResponse.Body.Bytes(), &healthBody); err != nil {
		t.Fatal(err)
	}
	user, _ := healthBody["user"].(map[string]any)
	if healthBody["authenticated"] != true || healthBody["auth_method"] != "yandex" || user["display_name"] != "Редактор" {
		t.Fatalf("health auth state = %#v", healthBody)
	}

	server.yandexAllowed["someone-else"] = struct{}{}
	delete(server.yandexAllowed, "42")
	revoked := httptest.NewRequest(http.MethodGet, "/api/v1/posts", nil)
	revoked.AddCookie(sessionCookie)
	revokedResponse := httptest.NewRecorder()
	handler.ServeHTTP(revokedResponse, revoked)
	if revokedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("removed allowlist user status = %d, want 401", revokedResponse.Code)
	}
	server.yandexAllowed["42"] = struct{}{}
	restoredAllowlist := httptest.NewRequest(http.MethodGet, "/api/v1/posts", nil)
	restoredAllowlist.AddCookie(sessionCookie)
	restoredResponse := httptest.NewRecorder()
	handler.ServeHTTP(restoredResponse, restoredAllowlist)
	if restoredResponse.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session after allowlist restore status = %d, want 401", restoredResponse.Code)
	}

	logout := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	logout.Header.Set("Origin", "http://localhost:4321")
	logout.AddCookie(sessionCookie)
	logoutResponse := httptest.NewRecorder()
	handler.ServeHTTP(logoutResponse, logout)
	if logoutResponse.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, body = %s", logoutResponse.Code, logoutResponse.Body.String())
	}
	cleared := responseCookie(t, logoutResponse, sessionCookieName)
	if cleared.MaxAge != -1 {
		t.Fatalf("cleared session cookie = %#v", cleared)
	}

	afterLogout := httptest.NewRequest(http.MethodGet, "/api/v1/posts", nil)
	afterLogout.AddCookie(sessionCookie)
	afterLogoutResponse := httptest.NewRecorder()
	handler.ServeHTTP(afterLogoutResponse, afterLogout)
	if afterLogoutResponse.Code != http.StatusUnauthorized {
		t.Fatalf("after logout status = %d, want 401", afterLogoutResponse.Code)
	}
}

func TestYandexOAuthRejectsUnlistedAccountAndStateReplay(t *testing.T) {
	handler, _, _ := newYandexAuthTestHandler(t, []string{"allowed@example.ru"}, yandexauth.Profile{
		ID: "42", ClientID: "client-id", Login: "intruder", DefaultEmail: "intruder@example.ru",
	})
	state, stateCookie := beginYandexAuth(t, handler, "https://evil.example/stolen")

	callbackURL := "/api/v1/auth/yandex/callback?code=code&state=" + url.QueryEscape(state)
	callback := httptest.NewRequest(http.MethodGet, callbackURL, nil)
	callback.AddCookie(stateCookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, callback)
	if response.Code != http.StatusSeeOther || !strings.Contains(response.Header().Get("Location"), "auth_error=account_not_allowed") {
		t.Fatalf("denied callback = %d %q", response.Code, response.Header().Get("Location"))
	}
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == sessionCookieName && cookie.Value != "" {
			t.Fatalf("denied account received session cookie: %#v", cookie)
		}
	}

	replay := httptest.NewRequest(http.MethodGet, callbackURL, nil)
	replay.AddCookie(stateCookie)
	replayResponse := httptest.NewRecorder()
	handler.ServeHTTP(replayResponse, replay)
	if !strings.Contains(replayResponse.Header().Get("Location"), "auth_error=state_invalid") {
		t.Fatalf("replay Location = %q", replayResponse.Header().Get("Location"))
	}
}

func TestYandexOAuthStartIsRateLimitedBeforeStateCreation(t *testing.T) {
	handler, _, server := newYandexAuthTestHandler(t, []string{"42"}, yandexauth.Profile{
		ID: "42", ClientID: "client-id", Login: "editor",
	})
	fixedNow := time.Date(2026, time.July, 14, 10, 0, 0, 0, time.UTC)
	server.now = func() time.Time { return fixedNow }

	for attempt := 0; attempt < 12; attempt++ {
		request := newYandexStartRequest(t, "", true, true)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("attempt %d status = %d, want 200", attempt+1, response.Code)
		}
	}
	request := newYandexStartRequest(t, "", true, true)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "60" {
		t.Fatalf("rate-limited response = %d Retry-After=%q", response.Code, response.Header().Get("Retry-After"))
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("rate-limited Cache-Control = %q", response.Header().Get("Cache-Control"))
	}

	otherClient := newYandexStartRequest(t, "", true, true)
	otherClient.RemoteAddr = "198.51.100.10:4321"
	otherResponse := httptest.NewRecorder()
	handler.ServeHTTP(otherResponse, otherClient)
	if otherResponse.Code != http.StatusOK {
		t.Fatalf("independent client status = %d, want 200", otherResponse.Code)
	}

	server.trustXRealIP = true
	proxiedClient := newYandexStartRequest(t, "", true, true)
	proxiedClient.Header.Set("X-Real-IP", "203.0.113.25")
	proxiedResponse := httptest.NewRecorder()
	handler.ServeHTTP(proxiedResponse, proxiedClient)
	if proxiedResponse.Code != http.StatusOK {
		t.Fatalf("trusted proxied client status = %d, want 200", proxiedResponse.Code)
	}
}

func TestYandexOAuthStartRequiresPOSTExactOriginAndBothConsents(t *testing.T) {
	handler, provider, _ := newYandexAuthTestHandler(t, []string{"42"}, yandexauth.Profile{
		ID: "42", ClientID: "client-id", Login: "editor",
	})

	oldGET := httptest.NewRequest(http.MethodGet, "/api/v1/auth/yandex/start?terms_accepted=true&personal_data_accepted=true", nil)
	oldGET.Header.Set("Origin", "http://localhost:4321")
	oldGETResponse := httptest.NewRecorder()
	handler.ServeHTTP(oldGETResponse, oldGET)
	if oldGETResponse.Code != http.StatusMethodNotAllowed {
		t.Fatalf("legacy GET status = %d, want 405", oldGETResponse.Code)
	}

	tests := []struct {
		name          string
		origin        string
		termsAccepted bool
		personalData  bool
		wantStatus    int
	}{
		{name: "missing origin", termsAccepted: true, personalData: true, wantStatus: http.StatusForbidden},
		{name: "foreign origin", origin: "https://evil.example", termsAccepted: true, personalData: true, wantStatus: http.StatusForbidden},
		{name: "origin with trailing slash", origin: "http://localhost:4321/", termsAccepted: true, personalData: true, wantStatus: http.StatusForbidden},
		{name: "terms missing", origin: "http://localhost:4321", personalData: true, wantStatus: http.StatusBadRequest},
		{name: "personal data missing", origin: "http://localhost:4321", termsAccepted: true, wantStatus: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := newYandexStartRequest(t, "/app/#/posts", test.termsAccepted, test.personalData)
			if test.origin == "" {
				request.Header.Del("Origin")
			} else {
				request.Header.Set("Origin", test.origin)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.wantStatus, response.Body.String())
			}
			for _, cookie := range response.Result().Cookies() {
				if cookie.Name == stateCookieName && cookie.Value != "" {
					t.Fatalf("rejected request created OAuth state cookie: %#v", cookie)
				}
			}
		})
	}
	if provider.authorizationCalls != 0 {
		t.Fatalf("rejected OAuth starts called provider %d times", provider.authorizationCalls)
	}
}

func beginYandexAuth(t *testing.T, handler http.Handler, returnTo string) (string, *http.Cookie) {
	t.Helper()
	request := newYandexStartRequest(t, returnTo, true, true)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("start status = %d, body = %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("start Cache-Control = %q", response.Header().Get("Cache-Control"))
	}
	var payload struct {
		AuthorizationURL string `json:"authorization_url"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	location, err := url.Parse(payload.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	state := location.Query().Get("state")
	if state == "" || location.Query().Get("code_challenge") == "" || location.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("authorization URL = %s", location)
	}
	return state, responseCookie(t, response, stateCookieName)
}

func newYandexStartRequest(t *testing.T, returnTo string, termsAccepted, personalDataAccepted bool) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"return_to":              returnTo,
		"terms_accepted":         termsAccepted,
		"personal_data_accepted": personalDataAccepted,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/yandex/start", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:4321")
	return request
}

func responseCookie(t *testing.T, response *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("response did not set cookie %q", name)
	return nil
}

func newYandexAuthTestHandler(t *testing.T, allowed []string, profile yandexauth.Profile) (http.Handler, *fakeYandexOAuth, *Server) {
	t.Helper()
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "auth-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application := app.New(storage, mediaStore, nil, nil, nil, logger)
	provider := &fakeYandexOAuth{profile: profile}
	server := New(application, logger, "http://localhost:4321", "webhook-secret", AuthOptions{
		YandexClient: provider, RedirectURI: "http://localhost:8080/api/v1/auth/yandex/callback",
		AllowedUsers: allowed, SessionTTL: time.Hour,
	})
	return server.Handler(), provider, server
}
