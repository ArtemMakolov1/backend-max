package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
					StatusCode: test.status, Message: "private-provider-content", RequestID: "private-request-id"})
				assertProblemCode(t, response, http.StatusBadGateway, test.publicCode)
				if strings.Contains(response.Body.String(), "private-") {
					t.Fatal("public error exposed upstream details")
				}
				var logged struct {
					Code    string `json:"code"`
					Message string `json:"msg"`
				}
				if json.Unmarshal(logs.Bytes(), &logged) != nil || logged.Code != test.loggedCode {
					t.Fatal("failure code was not safely classified")
				}
				if provider == "qwen" && logged.Message != "Qwen text request failed" {
					t.Fatal("Qwen failure was logged as another provider")
				}
			})
		}
	}
}
