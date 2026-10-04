package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

type fakeContentDiscoveryClient struct {
	*fakeResearchClient
	discoveryRequests []openairesearch.DiscoverContentRequest
}

func (f *fakeContentDiscoveryClient) DiscoverContent(_ context.Context, request openairesearch.DiscoverContentRequest) (openairesearch.DiscoverContentResult, error) {
	f.mu.Lock()
	f.discoveryRequests = append(f.discoveryRequests, request)
	f.mu.Unlock()
	kind := request.ContentKind
	if kind == "auto" {
		kind = "idea"
	}
	return openairesearch.DiscoverContentResult{Topic: request.Topic, ContentKind: request.ContentKind, Cards: []openairesearch.ContentCard{{
		ID: "stable-source-id", ContentKind: kind, Title: "Идея", Summary: "Подходящий материал", Source: openairesearch.Source{Title: "Источник", URL: "https://example.com/material"},
		Draft: openairesearch.Draft{Title: "Пост", Content: "Короткий пост", Format: request.Format, ImagePrompt: ""},
	}}}, nil
}

func TestContentDiscoveryAPIValidatesBeforeSharedResearchQuota(t *testing.T) {
	fake := &fakeContentDiscoveryClient{fakeResearchClient: &fakeResearchClient{}}
	options := testAILimitOptions()
	options.MonthlyPlanEnforcement, options.ResearchPerMinute = true, 1
	server, storage, raw := newAIQuotaTestServer(t, nil, fake, options, "content-discovery-user", "content-discovery-foreign")
	server.now = func() time.Time { return time.Now().UTC() }
	handler := withTestSession(t, storage, raw, "content-discovery-user")
	workspaceID := personalWorkspaceIDForTest(t, storage, "content-discovery-user")
	path := "/api/v1/workspaces/" + workspaceID + "/research/discover"
	foreignWorkspace := personalWorkspaceIDForTest(t, storage, "content-discovery-foreign")
	foreignChannel, err := storage.CreateChannel(t.Context(), store.Channel{UserID: "content-discovery-foreign", WorkspaceID: foreignWorkspace,
		VerifiedMAXOwnerID: "200", MAXChatID: "-200", Title: "Чужой приватный канал", Description: "Чужой контекст не отправлять", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{}`, `{"topic":"я"}`, `{"topic":"Коты","content_kind":"unknown"}`, `{"topic":"Коты","format":"invalid"}`,
		`{"topic":"Коты","channel_id":-1}`,
	} {
		response := performJSONRequest(handler, http.MethodPost, path, body)
		assertProblemCode(t, response, http.StatusBadRequest, "validation_error")
	}
	response := performJSONRequest(handler, http.MethodPost, path, `{"topic":"Коты","channel_description":"Injected"}`)
	assertProblemCode(t, response, http.StatusBadRequest, "invalid_json")
	response = performJSONRequest(handler, http.MethodPost, path, `{"topic":"Коты","channel_id":999999}`)
	if response.Code != http.StatusNotFound {
		t.Fatalf("foreign/missing channel=%d %s", response.Code, response.Body.String())
	}
	response = performJSONRequest(handler, http.MethodPost, path, fmt.Sprintf(`{"topic":"Коты","channel_id":%d}`, foreignChannel.ID))
	if response.Code != http.StatusNotFound {
		t.Fatalf("foreign channel leaked into discovery=%d %s", response.Code, response.Body.String())
	}
	billing := readWorkspaceBillingForTest(t, handler, workspaceID)
	fake.mu.Lock()
	calls := len(fake.discoveryRequests)
	fake.mu.Unlock()
	if calls != 0 || billingUsageQuantity(t, billing.Usage, store.UsageMetricAIResearchRequests) != 0 {
		t.Fatal("validation/context failure consumed quota or reached provider")
	}
	response = performJSONRequest(handler, http.MethodPost, path, `{"topic":"Забавные коты"}`)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("discovery=%d %s", response.Code, response.Body.String())
	}
	var result openairesearch.DiscoverContentResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Topic != "Забавные коты" || result.ContentKind != "auto" || len(result.Cards) != 1 || result.Cards[0].ContentKind != "idea" || result.Cards[0].Draft.Format != "markdown" {
		t.Fatalf("discovery contract=%#v", result)
	}
	billing = readWorkspaceBillingForTest(t, handler, workspaceID)
	if billingUsageQuantity(t, billing.Usage, store.UsageMetricAIResearchRequests) != 1 {
		t.Fatalf("single discovery did not charge exactly one research request: %#v", billing.Usage)
	}
	// The advanced route must share the same rate limit; discovery is no bypass.
	response = performJSONRequest(handler, http.MethodPost, "/api/v1/workspaces/"+workspaceID+"/research/generate",
		`{"topic":"Забавные коты","tone":"Простой","format":"markdown","include_sources":true}`)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("advanced research bypassed shared minute quota: %d %s", response.Code, response.Body.String())
	}
}

func TestContentDiscoveryAPIRequiresAuthenticationAndConfiguredDiscoverer(t *testing.T) {
	_, storage, raw := newAIQuotaTestServer(t, nil, &fakeResearchClient{}, testAILimitOptions(), "discovery-unconfigured")
	workspaceID := personalWorkspaceIDForTest(t, storage, "discovery-unconfigured")
	path := "/api/v1/workspaces/" + workspaceID + "/research/discover"
	response := httptest.NewRecorder()
	raw.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"topic":"Коты"}`)))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous discovery=%d %s", response.Code, response.Body.String())
	}
	handler := withTestSession(t, storage, raw, "discovery-unconfigured")
	response = performJSONRequest(handler, http.MethodPost, path, `{"topic":"Коты"}`)
	assertProblemCode(t, response, http.StatusServiceUnavailable, "openai_research_not_configured")
	billing := readWorkspaceBillingForTest(t, handler, workspaceID)
	if billingUsageQuantity(t, billing.Usage, store.UsageMetricAIResearchRequests) != 0 {
		t.Fatal("unconfigured provider consumed a research request")
	}
}
