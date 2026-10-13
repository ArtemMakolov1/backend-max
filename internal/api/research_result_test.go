package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"maxpilot/backend/internal/contentsearch"
	"maxpilot/backend/internal/openairesearch"
)

func TestResearchInvalidResultIsDistinctFromProviderFailure(t *testing.T) {
	for _, test := range []struct {
		name, providerCode, publicCode, loggedCode string
		status                                     int
	}{
		{"invalid result", "invalid_structured_output", "ai_invalid_result", "invalid_structured_output", 0},
		{"HTTP failure", "invalid_structured_output", "openai_research_error", "invalid_structured_output", 500},
		{"rate limit", "rate_limit", "openai_research_error", "unknown", 429},
		{"transport failure", "provider_transport_error", "openai_research_error", "provider_transport_error", 0},
		{"invalid provider response", "invalid_provider_response", "openai_research_error", "invalid_provider_response", 0},
		{"provider HTTP code", "provider_http_error", "openai_research_error", "provider_http_error", 402},
		{"unknown private code", "private-provider-code", "openai_research_error", "unknown", 0},
	} {
		for _, provider := range []string{"qwen", "deepseek", "openai"} {
			t.Run(provider+"/"+test.name, func(t *testing.T) {
				var logs bytes.Buffer
				server := &Server{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
				response := httptest.NewRecorder()
				server.writeError(response, &openairesearch.Error{Provider: provider, Code: test.providerCode,
					StatusCode: test.status, Message: "private-provider-content discovery_precheck invalid_pair_decisions", RequestID: "private-request-id"})
				assertProblemCode(t, response, http.StatusBadGateway, test.publicCode)
				if strings.Contains(response.Body.String(), "private-") {
					t.Fatal("public error exposed upstream details")
				}
				var logged struct {
					Code             string `json:"code"`
					Message          string `json:"msg"`
					DiscoveryStage   string `json:"discovery_stage"`
					ValidationReason string `json:"validation_reason"`
				}
				if json.Unmarshal(logs.Bytes(), &logged) != nil || logged.Code != test.loggedCode {
					t.Fatal("failure code was not safely classified")
				}
				if provider == "qwen" && logged.Message != "Qwen text request failed" {
					t.Fatal("Qwen failure was logged as another provider")
				}
				if logged.DiscoveryStage != "" || logged.ValidationReason != "" {
					t.Fatal("untyped private provider text acquired discovery diagnostic authority")
				}
			})
		}
	}
}

type researchFailureDiagnosticSearcher struct {
	calls atomic.Int32
}

func (s *researchFailureDiagnosticSearcher) Search(context.Context, contentsearch.Request) (contentsearch.Result, error) {
	s.calls.Add(1)
	return contentsearch.Result{Provider: "fixture", Sources: []contentsearch.Source{
		{Title: "Проверенный источник", URL: "https://example.com/source", Content: "Подробности нового научного события."},
	}}, nil
}

func TestResearchDiscoveryFailureLogsTypedClosedStageWithoutPublicMetadata(t *testing.T) {
	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "private-invalid-provider-payload"},
		}}})
	}))
	defer provider.Close()
	client, err := openairesearch.NewQwen(provider.URL, "local-fixture-key", "qwen3.6-plus", provider.Client())
	if err != nil {
		t.Fatal(err)
	}
	searcher := &researchFailureDiagnosticSearcher{}
	_, err = client.WithContentSearch(searcher).DiscoverContent(t.Context(),
		openairesearch.DiscoverContentRequest{Topic: "Научные события", ContentKind: "idea", Format: "markdown"})
	if err == nil {
		t.Fatal("fixture did not produce a typed discovery validation failure")
	}
	if providerCalls.Load() != 2 || searcher.calls.Load() != 1 {
		t.Fatalf("expected one bounded repair without repeated retrieval, got provider=%d search=%d", providerCalls.Load(), searcher.calls.Load())
	}
	var logs bytes.Buffer
	server := &Server{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	response := httptest.NewRecorder()
	server.writeError(response, err)
	assertProblemCode(t, response, http.StatusBadGateway, "ai_invalid_result")
	if strings.Contains(response.Body.String(), "discovery_stage") || strings.Contains(response.Body.String(), "validation_reason") || strings.Contains(response.Body.String(), "private-") {
		t.Fatal("diagnostic fields or provider data entered the public problem")
	}
	var logged struct {
		Code             string `json:"code"`
		DiscoveryStage   string `json:"discovery_stage"`
		ValidationReason string `json:"validation_reason"`
	}
	if json.Unmarshal(logs.Bytes(), &logged) != nil || logged.Code != "invalid_structured_output" ||
		logged.DiscoveryStage != "discovery_synthesis_retry" || logged.ValidationReason != "discovery_json_invalid" {
		t.Fatal("typed discovery failure did not emit its closed exhausted-repair diagnostic")
	}
	if strings.Contains(logs.String(), "private-invalid-provider-payload") {
		t.Fatal("upstream provider payload escaped the source-owned validation boundary")
	}
}
