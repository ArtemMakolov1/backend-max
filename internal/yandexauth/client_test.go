package yandexauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAuthorizationURLUsesStateAndPKCES256(t *testing.T) {
	client, err := New("client-id", "client-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := client.AuthorizationURL("https://studio.example/api/v1/auth/yandex/callback", "state-value", "challenge-value")
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	for key, want := range map[string]string{
		"response_type": "code", "client_id": "client-id", "state": "state-value",
		"code_challenge": "challenge-value", "code_challenge_method": "S256",
		"scope": "login:info login:email", "optional_scope": "login:avatar",
	} {
		if got := query.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestExchangeAndUserInfoStayServerSide(t *testing.T) {
	var tokenRequestSeen bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenRequestSeen = true
			clientID, secret, ok := r.BasicAuth()
			if !ok || clientID != "client-id" || secret != "client-secret" {
				t.Fatalf("Basic auth = %q/%q/%v", clientID, secret, ok)
			}
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "code" || r.Form.Get("code_verifier") != "verifier" {
				t.Fatalf("token form = %#v", r.Form)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-secret", "token_type": "bearer"})
		case "/info":
			if got := r.Header.Get("Authorization"); got != "OAuth access-secret" {
				t.Fatalf("Authorization = %q", got)
			}
			_ = json.NewEncoder(w).Encode(Profile{ID: "42", ClientID: "client-id", Login: "editor", DefaultEmail: "editor@example.ru"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := New("client-id", "client-secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	client.tokenURL = server.URL + "/token"
	client.userInfoURL = server.URL + "/info"
	token, err := client.ExchangeCode(context.Background(), "code", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := client.UserInfo(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if !tokenRequestSeen || profile.ID != "42" || profile.DefaultEmail != "editor@example.ru" {
		t.Fatalf("unexpected profile: %#v", profile)
	}
}

func TestUserInfoRejectsTokenForAnotherClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"42","client_id":"another-client"}`))
	}))
	defer server.Close()
	client, _ := New("client-id", "client-secret", server.Client())
	client.userInfoURL = server.URL
	_, err := client.UserInfo(context.Background(), "token")
	if err == nil || !strings.Contains(err.Error(), "user info") {
		t.Fatalf("UserInfo() error = %v", err)
	}
}

func TestOAuthSecretsNeverFollowRedirects(t *testing.T) {
	t.Parallel()
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		if authorization := r.Header.Get("Authorization"); authorization != "" {
			t.Errorf("redirect target received Authorization %q", authorization)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL+"/secret")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client, err := New("client-id", "client-secret", origin.Client())
	if err != nil {
		t.Fatal(err)
	}
	client.tokenURL = origin.URL + "/token"
	client.userInfoURL = origin.URL + "/info"
	if _, err := client.ExchangeCode(context.Background(), "code", "verifier"); err == nil {
		t.Fatal("ExchangeCode followed or accepted redirect")
	}
	if _, err := client.UserInfo(context.Background(), "access-secret"); err == nil {
		t.Fatal("UserInfo followed or accepted redirect")
	}
	if targetCalls.Load() != 0 {
		t.Fatalf("redirect target calls = %d, want 0", targetCalls.Load())
	}
}

func TestAvatarURLAllowsOfficialNestedIDsAndRejectsUnsafeValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		profile Profile
		want    string
	}{
		{name: "simple", profile: Profile{DefaultAvatarID: "avatar_42-1"}, want: "https://avatars.yandex.net/get-yapic/avatar_42-1/islands-200"},
		{name: "official nested id", profile: Profile{DefaultAvatarID: "1824/2a0000018f-avatar"}, want: "https://avatars.yandex.net/get-yapic/1824/2a0000018f-avatar/islands-200"},
		{name: "empty flag", profile: Profile{DefaultAvatarID: "1824/avatar", IsAvatarEmpty: true}},
		{name: "missing id", profile: Profile{}},
		{name: "path traversal", profile: Profile{DefaultAvatarID: "1824/../secret"}},
		{name: "double dot", profile: Profile{DefaultAvatarID: "avatar..secret"}},
		{name: "empty segment", profile: Profile{DefaultAvatarID: "1824//avatar"}},
		{name: "dot segment", profile: Profile{DefaultAvatarID: "1824/./avatar"}},
		{name: "query", profile: Profile{DefaultAvatarID: "1824/avatar?size=999"}},
		{name: "fragment", profile: Profile{DefaultAvatarID: "1824/avatar#fragment"}},
		{name: "backslash", profile: Profile{DefaultAvatarID: `1824\avatar`}},
		{name: "unicode", profile: Profile{DefaultAvatarID: "1824/аватар"}},
		{name: "surrounding whitespace", profile: Profile{DefaultAvatarID: " 1824/avatar "}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := AvatarURL(test.profile); got != test.want {
				t.Fatalf("AvatarURL() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestProfileDecodesAvatarEmptyFlag(t *testing.T) {
	t.Parallel()
	var profile Profile
	if err := json.Unmarshal([]byte(`{"id":"42","default_avatar_id":"1824/avatar","is_avatar_empty":true}`), &profile); err != nil {
		t.Fatal(err)
	}
	if !profile.IsAvatarEmpty || profile.DefaultAvatarID != "1824/avatar" || AvatarURL(profile) != "" {
		t.Fatalf("profile = %#v", profile)
	}
}
