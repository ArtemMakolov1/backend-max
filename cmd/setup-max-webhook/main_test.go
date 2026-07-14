package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

const validTestWebhookURL = "https://webhook.example.test/api/v1/webhooks/max"

type rewriteURLTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (t rewriteURLTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	rewritten := *request.URL
	rewritten.Scheme = t.target.Scheme
	rewritten.Host = t.target.Host
	clone.URL = &rewritten
	return t.base.RoundTrip(clone)
}

func clientForTestServer(t *testing.T, server *httptest.Server) *http.Client {
	t.Helper()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	base := server.Client()
	return &http.Client{Transport: rewriteURLTransport{base: base.Transport, target: target}}
}

func TestPreflightWebhookMatchesMAXDeliveryContract(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request: %s, content-type %q", r.Method, r.Header.Get("Content-Type"))
		}
		if got := r.Header.Get("X-Max-Bot-Api-Secret"); got != "preflight_secret" {
			t.Errorf("webhook secret = %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != `{"update_type":"maxstudio_preflight","timestamp":1}` {
			t.Errorf("body = %s", body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := preflightWebhook(context.Background(), clientForTestServer(t, server), validTestWebhookURL, "preflight_secret"); err != nil {
		t.Fatalf("preflightWebhook() error = %v", err)
	}
}

func TestPreflightWebhookDoesNotFollowRedirectWithSecret(t *testing.T) {
	t.Parallel()
	var targetCalls atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		if got := r.Header.Get("X-Max-Bot-Api-Secret"); got != "" {
			t.Errorf("redirect target received secret %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	err := preflightWebhook(context.Background(), clientForTestServer(t, origin), validTestWebhookURL, "preflight_secret")
	if err == nil {
		t.Fatal("preflightWebhook() accepted a redirect")
	}
	if targetCalls.Load() != 0 {
		t.Fatalf("redirect target calls = %d, want 0", targetCalls.Load())
	}
}
