package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"maxpilot/backend/internal/openairesearch"
)

type configuredContentSearchResearch struct{ *fakeResearchClient }

func (*configuredContentSearchResearch) ContentSearchConfigured() bool { return true }

func TestHealthExposesServerSideContentSearchWithoutProviderCredentials(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "external"}[configured], func(t *testing.T) {
			var handler http.Handler
			if configured {
				handler = newResearchTestHandler(t, &configuredContentSearchResearch{fakeResearchClient: &fakeResearchClient{}}, "")
			} else {
				handler = newResearchTestHandler(t, &fakeResearchClient{}, "")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
			var health map[string]any
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &health) != nil || health["content_search_configured"] != configured {
				t.Fatalf("wrong content search configuration: %d %s", response.Code, response.Body.String())
			}
			if strings.Contains(strings.ToLower(response.Body.String()), "api_key") {
				t.Fatal("health exposed a provider credential field")
			}
		})
	}
}

func TestContentSearchFailureReturnsHelpfulProblemWithoutRemoteDetails(t *testing.T) {
	fake := &fakeResearchClient{err: &openairesearch.Error{Code: "content_search_failed", Message: "private-upstream-body with credentials", RequestID: "private-request-id"}}
	handler := newResearchTestHandler(t, fake, "")
	response := performJSONRequest(handler, http.MethodPost, "/api/v1/research/generate", `{"topic":"Тема поста","tone":"Простой","format":"markdown","include_sources":false}`)
	assertProblemCode(t, response, http.StatusBadGateway, "content_search_error")
	if strings.Contains(response.Body.String(), "private-upstream") || strings.Contains(response.Body.String(), "private-request-id") {
		t.Fatal("provider failure leaked remote details")
	}
}
