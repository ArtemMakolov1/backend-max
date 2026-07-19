package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

type fakeResearchClient struct {
	mu               sync.Mutex
	requests         []openairesearch.Request
	result           openairesearch.Result
	err              error
	formatRequests   []openairesearch.FormatRequest
	formatResult     openairesearch.FormatResult
	formatErr        error
	suggestRequests  []openairesearch.SuggestImagePromptRequest
	suggestResult    openairesearch.SuggestImagePromptResult
	suggestErr       error
	brandKitRequests []openairesearch.SuggestBrandKitRequest
	brandKitResult   openairesearch.SuggestBrandKitResult
	brandKitErr      error
}

func (f *fakeResearchClient) Generate(ctx context.Context, request openairesearch.Request) (openairesearch.Result, error) {
	f.mu.Lock()
	f.requests = append(f.requests, request)
	f.mu.Unlock()
	return f.result, f.err
}

func (f *fakeResearchClient) FormatContent(_ context.Context, request openairesearch.FormatRequest) (openairesearch.FormatResult, error) {
	f.mu.Lock()
	f.formatRequests = append(f.formatRequests, request)
	f.mu.Unlock()
	return f.formatResult, f.formatErr
}

func (f *fakeResearchClient) SuggestImagePrompt(_ context.Context, request openairesearch.SuggestImagePromptRequest) (openairesearch.SuggestImagePromptResult, error) {
	f.mu.Lock()
	f.suggestRequests = append(f.suggestRequests, request)
	f.mu.Unlock()
	return f.suggestResult, f.suggestErr
}

func (f *fakeResearchClient) SuggestBrandKit(_ context.Context, request openairesearch.SuggestBrandKitRequest) (openairesearch.SuggestBrandKitResult, error) {
	f.mu.Lock()
	f.brandKitRequests = append(f.brandKitRequests, request)
	f.mu.Unlock()
	return f.brandKitResult, f.brandKitErr
}

func TestResearchGenerateReturnsExactContractUnderYandexSession(t *testing.T) {
	t.Parallel()
	fake := &fakeResearchClient{result: openairesearch.Result{
		Topic:   "ИИ для бизнеса",
		Report:  "Проверенный отчёт [Источник](<https://example.com>)",
		Sources: []openairesearch.Source{{Title: "Источник", URL: "https://example.com"}},
		Draft: openairesearch.Draft{
			Title: "Заголовок", Content: "**Готовый пост**", Format: "markdown", ImagePrompt: "Editorial illustration",
		},
	}}
	handler := newResearchTestHandler(t, fake, "0123456789abcdefghijklmn")
	body := `{"topic":"ИИ для бизнеса","angle":"Практика","audience":"Предприниматели","tone":"Деловой","format":"markdown","include_sources":true}`

	request := httptest.NewRequest(http.MethodPost, "/api/v1/research/generate", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if got := sortedJSONKeys(payload); !reflect.DeepEqual(got, []string{"draft", "report", "sources", "topic"}) {
		t.Fatalf("top-level response keys = %#v", got)
	}
	var draft map[string]json.RawMessage
	if err := json.Unmarshal(payload["draft"], &draft); err != nil {
		t.Fatal(err)
	}
	if got := sortedJSONKeys(draft); !reflect.DeepEqual(got, []string{"content", "format", "image_prompt", "title"}) {
		t.Fatalf("draft response keys = %#v", got)
	}
	var sources []map[string]json.RawMessage
	if err := json.Unmarshal(payload["sources"], &sources); err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || !reflect.DeepEqual(sortedJSONKeys(sources[0]), []string{"title", "url"}) {
		t.Fatalf("sources response = %#v", sources)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 1 || !fake.requests[0].IncludeSources || fake.requests[0].Format != "markdown" {
		t.Fatalf("research requests = %#v", fake.requests)
	}
}

func TestResearchGenerateValidatesBeforeConfiguration(t *testing.T) {
	t.Parallel()
	handler := newResearchTestHandler(t, nil, "")
	tests := []struct {
		name string
		body string
	}{
		{name: "missing topic", body: `{"topic":"","tone":"Деловой","format":"markdown","include_sources":false}`},
		{name: "short topic", body: `{"topic":"ИИ","tone":"Деловой","format":"markdown","include_sources":false}`},
		{name: "missing tone", body: `{"topic":"Тема поста","tone":"","format":"markdown","include_sources":false}`},
		{name: "invalid format", body: `{"topic":"Тема поста","tone":"Деловой","format":"text","include_sources":false}`},
		{name: "unknown field", body: `{"topic":"Тема поста","tone":"Деловой","format":"markdown","include_sources":false,"api_key":"secret"}`},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/research/generate", strings.NewReader(test.body)))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestResearchGenerateReportsUnavailableConfigurationAndUpstreamFailure(t *testing.T) {
	t.Parallel()
	validBody := `{"topic":"Тема поста","tone":"Деловой","format":"html","include_sources":false}`

	t.Run("not configured", func(t *testing.T) {
		handler := newResearchTestHandler(t, nil, "")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/research/generate", strings.NewReader(validBody)))
		assertProblemCode(t, response, http.StatusServiceUnavailable, "openai_research_not_configured")
	})

	t.Run("upstream", func(t *testing.T) {
		fake := &fakeResearchClient{err: &openairesearch.Error{
			StatusCode: http.StatusTooManyRequests, Code: "rate_limit", Message: "slow down", RequestID: "req-123",
		}}
		handler := newResearchTestHandler(t, fake, "")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/research/generate", strings.NewReader(validBody)))
		assertProblemCode(t, response, http.StatusBadGateway, "openai_research_error")
		if strings.Contains(response.Body.String(), "rate_limit") || strings.Contains(response.Body.String(), "req-123") {
			t.Fatalf("upstream problem body = %s", response.Body.String())
		}
	})
}

func TestResearchGenerateMapsDeadlineExceededToGatewayTimeout(t *testing.T) {
	t.Parallel()
	fake := &fakeResearchClient{err: context.DeadlineExceeded}
	handler := newResearchTestHandler(t, fake, "")
	request := httptest.NewRequest(http.MethodPost, "/api/v1/research/generate", strings.NewReader(
		`{"topic":"Тема поста","tone":"Деловой","format":"markdown","include_sources":false}`,
	))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertProblemCode(t, response, http.StatusGatewayTimeout, "upstream_timeout")
}

func TestHealthExposesResearchConfigurationSeparately(t *testing.T) {
	t.Parallel()
	handler := newResearchTestHandler(t, &fakeResearchClient{}, "")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var health map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if health["research_configured"] != true || health["openai_configured"] != true {
		t.Fatalf("health = %#v", health)
	}
}

func newResearchTestHandler(t *testing.T, research app.ResearchClient, _ string) http.Handler {
	t.Helper()
	storage, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "research.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application := app.New(storage, mediaStore, nil, nil, research, logger)
	server := New(application, logger, "http://localhost:4321", "webhook-secret", AuthOptions{YandexClient: &fakeYandexOAuth{}})
	return withTestSession(t, storage, server.Handler(), "research-user")
}

func sortedJSONKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func assertProblemCode(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, status, response.Body.String())
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Code != code {
		t.Fatalf("problem code = %q, want %q; body = %s", payload.Error.Code, code, response.Body.String())
	}
}
