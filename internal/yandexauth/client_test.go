package yandexauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
