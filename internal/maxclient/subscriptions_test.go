package maxclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

type countingTransport struct{ calls atomic.Int32 }

func (t *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return nil, errors.New("unexpected outbound request")
}

func TestConfigureStudioWebhookUsesRequiredEventsAndSecret(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/subscriptions" || r.URL.RawQuery != "" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		if got := r.Header.Get("Authorization"); got != "bot-token" {
			t.Errorf("Authorization = %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Errorf("Content-Type = %q", got)
			}

			var body struct {
				URL         string   `json:"url"`
				UpdateTypes []string `json:"update_types"`
				// #nosec G117 -- test-only decoding of the mandatory MAX webhook contract field.
				Secret string `json:"secret"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if body.URL != "https://api.example.ru/api/v1/webhooks/max" || body.Secret != "safe_secret-123" {
				t.Errorf("request body = %#v", body)
			}
			wantEvents := []string{"bot_added", "bot_removed", "bot_started", "message_created", "message_callback"}
			if !reflect.DeepEqual(body.UpdateTypes, wantEvents) {
				t.Errorf("update_types = %#v, want %#v", body.UpdateTypes, wantEvents)
			}
			_, _ = io.WriteString(w, `{"success":true}`)
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"subscriptions":[{"url":"https://api.example.ru/api/v1/webhooks/max","time":1,"update_types":["bot_added","bot_removed","bot_started","message_created","message_callback"]}]}`)
		default:
			t.Errorf("unexpected request method: %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "bot-token", server.Client())
	if err := client.ConfigureStudioWebhook(context.Background(), "https://api.example.ru/api/v1/webhooks/max", "safe_secret-123"); err != nil {
		t.Fatalf("ConfigureStudioWebhook() error = %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("MAX API calls = %d, want 2", got)
	}
}

func TestConfigureStudioWebhookRejectsUnverifiedEventSet(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			_, _ = io.WriteString(w, `{"success":true}`)
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"subscriptions":[{"url":"https://api.example.ru/api/v1/webhooks/max","time":1,"update_types":["bot_added","bot_removed","bot_started","message_callback"]}]}`)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "bot-token", server.Client())
	err := client.ConfigureStudioWebhook(context.Background(), "https://api.example.ru/api/v1/webhooks/max", "safe_secret-123")
	if err == nil || !strings.Contains(err.Error(), `required update type "message_created" is missing`) {
		t.Fatalf("ConfigureStudioWebhook() error = %v", err)
	}
}

func TestConfigureStudioWebhookRejectsMissingUpdatedEndpoint(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = io.WriteString(w, `{"success":true}`)
			return
		}
		_, _ = io.WriteString(w, `{"subscriptions":[]}`)
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "bot-token", server.Client())
	err := client.ConfigureStudioWebhook(context.Background(), "https://api.example.ru/api/v1/webhooks/max", "safe_secret-123")
	if err == nil || !strings.Contains(err.Error(), "updated endpoint was not returned") {
		t.Fatalf("ConfigureStudioWebhook() error = %v", err)
	}
}

func TestConfigureStudioWebhookRejectsUnsafeConfiguration(t *testing.T) {
	t.Parallel()
	transport := &countingTransport{}
	client := mustClient(t, "https://platform-api2.max.ru", "bot-token", &http.Client{Transport: transport})

	for _, rawURL := range []string{
		"",
		"http://api.example.ru/api/v1/webhooks/max",
		"https://user:pass@api.example.ru/api/v1/webhooks/max",
		"https://api.example.ru:8443/api/v1/webhooks/max",
		"https://api.example.ru/api/v1/webhooks/max?secret=leak",
		"https://api.example.ru/api/v1/webhooks/max#fragment",
	} {
		if err := client.ConfigureStudioWebhook(context.Background(), rawURL, "safe_secret-123"); err == nil {
			t.Errorf("ConfigureStudioWebhook(%q) accepted unsafe URL", rawURL)
		}
	}
	for _, secret := range []string{"", "four", "contains space", "кириллица", strings.Repeat("a", 257)} {
		if err := client.ConfigureStudioWebhook(context.Background(), "https://api.example.ru/api/v1/webhooks/max", secret); err == nil {
			t.Errorf("ConfigureStudioWebhook accepted invalid secret %q", secret)
		}
	}
	if transport.calls.Load() != 0 {
		t.Fatalf("unsafe configuration caused %d outbound calls, want 0", transport.calls.Load())
	}
}

func TestConfigureStudioWebhookReturnsOperationFailure(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("unexpected request after failed update: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":false,"message":"subscription rejected"}`)
	}))
	defer server.Close()

	client := mustClient(t, server.URL, "bot-token", server.Client())
	err := client.ConfigureStudioWebhook(context.Background(), "https://api.example.ru/api/v1/webhooks/max", "safe_secret-123")
	if err == nil || !strings.Contains(err.Error(), "subscription rejected") {
		t.Fatalf("ConfigureStudioWebhook() error = %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("MAX API calls = %d, want 1", got)
	}
}
