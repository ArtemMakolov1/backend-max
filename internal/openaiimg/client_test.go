package openaiimg

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGenerateUsesImagesEndpointAndDecodesBase64(t *testing.T) {
	t.Parallel()
	want := []byte("generated-image")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/images/generations" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization = %q", got)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["model"] != "gpt-image-2" || payload["output_format"] != "png" {
			t.Fatalf("unexpected payload: %#v", payload)
		}
		w.Header().Set("x-request-id", "req_123")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(want)}},
		})
	}))
	defer server.Close()

	client, err := New(server.URL, "test-key", "gpt-image-2", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Generate(context.Background(), GenerateRequest{Prompt: "A poster"})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Bytes) != string(want) || result.RequestID != "req_123" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestImageAPIKeyNeverFollowsRedirect(t *testing.T) {
	t.Parallel()
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("redirect target received Authorization %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL+"/stolen")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client, err := New(origin.URL, "shared-openai-key", "gpt-image-2", origin.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Generate(context.Background(), GenerateRequest{Prompt: "A safe illustration"}); err == nil {
		t.Fatal("Generate followed or accepted redirect")
	}
	if targetCalls.Load() != 0 {
		t.Fatalf("redirect target calls = %d, want 0", targetCalls.Load())
	}
}

func TestGenerateReturnsStructuredAPIError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down","code":"rate_limit"}}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "key", "gpt-image-2", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Generate(context.Background(), GenerateRequest{Prompt: "A poster"})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests || apiErr.Code != "rate_limit" {
		t.Fatalf("unexpected error: %#v", err)
	}
}

func TestValidateGPTImage2Size(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		size    string
		wantErr bool
	}{
		{name: "auto", size: "auto"},
		{name: "square", size: "1024x1024"},
		{name: "custom landscape", size: "2048x1152"},
		{name: "4k landscape", size: "3840x2160"},
		{name: "not divisible", size: "1025x1024", wantErr: true},
		{name: "edge too long", size: "4096x2048", wantErr: true},
		{name: "ratio too wide", size: "3072x512", wantErr: true},
		{name: "too few pixels", size: "1024x512", wantErr: true},
		{name: "too many pixels", size: "3840x3840", wantErr: true},
		{name: "malformed", size: "large", wantErr: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateSize("gpt-image-2", test.size)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateSize(%q) error = %v, wantErr = %v", test.size, err, test.wantErr)
			}
		})
	}
}

func TestGeneratePromptLimitCountsUnicodeCharacters(t *testing.T) {
	t.Parallel()
	client, err := New("https://api.openai.com", "key", "gpt-image-2", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	// Cyrillic characters occupy multiple UTF-8 bytes but each counts as one
	// character in the documented API limit.
	_, err = client.Generate(context.Background(), GenerateRequest{Prompt: strings.Repeat("я", 32_001)})
	if err == nil || err.Error() != "image prompt is too long" {
		t.Fatalf("Generate oversized prompt error = %v", err)
	}
}
