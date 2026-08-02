package yandexwordstat

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetTopUsesCloudV2Contract(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/wordstat/topRequests" ||
			r.Header.Get("Authorization") != "Api-Key secret-key" ||
			r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request = %s %s headers=%v", r.Method, r.URL.Path, r.Header)
		}
		var request struct {
			Phrase     string   `json:"phrase"`
			NumPhrases string   `json:"numPhrases"`
			Regions    []string `json:"regions"`
			Devices    []string `json:"devices"`
			FolderID   string   `json:"folderId"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Phrase != "канал max" ||
			request.NumPhrases != "10" || len(request.Regions) != 1 || request.Regions[0] != "225" ||
			len(request.Devices) != 1 || request.Devices[0] != "DEVICE_ALL" ||
			request.FolderID != "folder123" {
			t.Errorf("payload = %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"totalCount":"123","results":[{"phrase":"канал max","count":"100"}],"associations":[{"phrase":"ведение max","count":"20"}]}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "secret-key", "folder123", &http.Client{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.GetTop(t.Context(), TopRequest{
		Phrase: "канал max", Limit: 10, Regions: []string{"225"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 123 || len(result.Results) != 1 ||
		result.Results[0].Count != 100 || len(result.Associations) != 1 ||
		result.Associations[0].Count != 20 {
		t.Fatalf("result = %#v", result)
	}
}

func TestGetTopRetriesAndPreservesQuotaError(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("x-request-id", "req-1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":"RESOURCE_EXHAUSTED","message":"quota"}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "secret-key", "folder123", &http.Client{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetTop(t.Context(), TopRequest{
		Phrase: "канал", Limit: 1, Regions: []string{"225"},
	})
	var providerErr *Error
	if !errors.As(err, &providerErr) || providerErr.StatusCode != http.StatusTooManyRequests ||
		providerErr.Code != "RESOURCE_EXHAUSTED" || providerErr.RequestID != "req-1" ||
		providerErr.RetryAfter != 0 || attempts.Load() != 1 {
		t.Fatalf("error = %#v", err)
	}
}

func TestDecodeProviderErrorPreservesLongRetryAfterWithoutEarlyRetry(t *testing.T) {
	t.Parallel()
	response := &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header)}
	response.Header.Set("Retry-After", "17")
	err := decodeProviderError(
		response, []byte(`{"code":"RESOURCE_EXHAUSTED","message":"quota"}`), "req-1",
	)
	var providerErr *Error
	if !errors.As(err, &providerErr) || providerErr.RetryAfter != 17*time.Second {
		t.Fatalf("error = %#v", err)
	}
	if delay, retry := retryDelay(err, 0); retry || delay != 0 {
		t.Fatalf("retry delay = %s retry=%v", delay, retry)
	}
}

func TestGetTopRetriesTransientServerFailure(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"code":"UNAVAILABLE"}`))
			return
		}
		_, _ = w.Write([]byte(`{"totalCount":"1","results":[],"associations":[]}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "secret-key", "folder123", &http.Client{Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetTop(t.Context(), TopRequest{
		Phrase: "канал", Limit: 1, Regions: []string{"225"},
	}); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts.Load())
	}
}

func TestGetTopRejectsMissingRequiredCounters(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"results":[],"associations":[]}`,
		`{"totalCount":"1","results":[{"phrase":"канал"}],"associations":[]}`,
	} {
		body := body
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			client, err := New(server.URL, "secret-key", "folder123", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.GetTop(t.Context(), TopRequest{
				Phrase: "канал", Limit: 1, Regions: []string{"225"},
			})
			var providerErr *Error
			if !errors.As(err, &providerErr) || providerErr.Code != "invalid_api_response" {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

func TestGetTopKeepsBoundedLongResponsePhrasesAndFiltersUnusableValues(t *testing.T) {
	t.Parallel()
	longAssociation := strings.Repeat("я", 600)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"totalCount": "4",
			"results": []map[string]any{
				{"phrase": "канал max", "count": "4"},
			},
			"associations": []map[string]any{
				{"phrase": longAssociation, "count": "3"},
				{"phrase": "   ", "count": "2"},
				{"phrase": strings.Repeat("я", MaxResponsePhraseRunes+1), "count": "1"},
			},
		})
	}))
	defer server.Close()
	client, err := New(server.URL, "secret-key", "folder123", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.GetTop(t.Context(), TopRequest{
		Phrase: "канал", Limit: 4, Regions: []string{"225"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || len(result.Associations) != 1 ||
		result.Associations[0].Phrase != longAssociation || result.Associations[0].Count != 3 {
		t.Fatalf("result = %#v", result)
	}
}

func TestGetTopNormalizesProviderPhraseWhitespaceAndUnicode(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"totalCount":"1","results":[{"phrase":"  cafe\u0301   MAX  ","count":"1"}],"associations":[]}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "secret-key", "folder123", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.GetTop(t.Context(), TopRequest{
		Phrase: "cafe MAX", Limit: 1, Regions: []string{"225"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].Phrase != "café MAX" {
		t.Fatalf("normalized phrase = %#v", result.Results)
	}
}

func TestGetTopRejectsInvalidInputBeforeNetwork(t *testing.T) {
	t.Parallel()
	client, err := New("http://localhost:1234", "secret-key", "folder123", &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []TopRequest{
		{Phrase: "", Limit: 1, Regions: []string{"225"}},
		{Phrase: "ok", Limit: 51, Regions: []string{"225"}},
		{Phrase: "ok", Limit: 1, Regions: []string{"0225"}},
	} {
		if _, err := client.GetTop(t.Context(), request); err == nil {
			t.Fatalf("invalid request accepted: %#v", request)
		}
	}
}
