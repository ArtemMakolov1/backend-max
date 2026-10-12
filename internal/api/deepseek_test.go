package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/qwenimg"
	"maxpilot/backend/internal/store"
)

func TestDeepSeekHealthAndUnconfiguredSearchDoNotEnableImagesOrConsumeQuota(t *testing.T) {
	calls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer provider.Close()
	client, err := openairesearch.NewDeepSeek(provider.URL, "synthetic-deepseek-key", "deepseek-flash", provider.Client())
	if err != nil {
		t.Fatal(err)
	}
	options := testAILimitOptions()
	options.MonthlyPlanEnforcement, options.ResearchPerMinute = true, 1
	server, storage, raw := newAIQuotaTestServer(t, nil, client, options, "deepseek-only")
	server.now = func() time.Time { return time.Now().UTC() }
	handler := withTestSession(t, storage, raw, "deepseek-only")
	workspaceID := personalWorkspaceIDForTest(t, storage, "deepseek-only")

	healthResponse := performJSONRequest(handler, http.MethodGet, "/api/v1/health", "")
	var health map[string]any
	if healthResponse.Code != http.StatusOK || json.Unmarshal(healthResponse.Body.Bytes(), &health) != nil {
		t.Fatal("could not read provider health")
	}
	if health["text_ai_provider"] != "deepseek" || health["openai_configured"] != false || health["research_configured"] != true || health["content_formatting_configured"] != true || health["content_discovery_configured"] != false || health["content_search_configured"] != false {
		t.Fatalf("DeepSeek-only health misrepresented provider features: %#v", health)
	}
	for i := 0; i < 2; i++ {
		for _, path := range []string{"/api/v1/research/generate", "/api/v1/workspaces/" + workspaceID + "/research/generate"} {
			response := performJSONRequest(handler, http.MethodPost, path,
				`{"topic":"Домашние коты","tone":"Простой","format":"markdown","include_sources":true}`)
			assertProblemCode(t, response, http.StatusServiceUnavailable, "content_search_not_configured")
		}
		response := performJSONRequest(handler, http.MethodPost, "/api/v1/workspaces/"+workspaceID+"/research/discover", `{"topic":"Домашние коты"}`)
		assertProblemCode(t, response, http.StatusServiceUnavailable, "content_search_not_configured")
	}
	billing := readWorkspaceBillingForTest(t, handler, workspaceID)
	if calls != 0 || billingUsageQuantity(t, billing.Usage, store.UsageMetricAIResearchRequests) != 0 {
		t.Fatal("unconfigured source retrieval reached provider or consumed quota")
	}
}

func TestQwenProviderErrorUsesImageAPIFailureContract(t *testing.T) {
	server := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	response := httptest.NewRecorder()
	server.writeError(response, &qwenimg.Error{StatusCode: http.StatusUnauthorized, Code: "provider_error", Message: "Qwen image provider is unavailable"})
	assertProblemCode(t, response, http.StatusBadGateway, "qwen_api_error")
}

func TestHealthAdvertisesQwenImageGenerationSeparatelyFromOpenAI(t *testing.T) {
	images, err := qwenimg.New("https://dashscope-intl.aliyuncs.com", "synthetic-qwen-key", "qwen-image-3.0-pro", &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	text, err := openairesearch.NewDeepSeek("https://api.deepseek.com", "synthetic-deepseek-key", "deepseek-flash", &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	_, storage, raw := newAIQuotaTestServer(t, images, text, testAILimitOptions(), "qwen-health")
	handler := withTestSession(t, storage, raw, "qwen-health")
	response := performJSONRequest(handler, http.MethodGet, "/api/v1/health", "")
	var health map[string]any
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &health) != nil || health["image_generation_configured"] != true || health["image_ai_provider"] != "qwen" || health["text_ai_provider"] != "deepseek" || health["openai_configured"] != false {
		t.Fatalf("Qwen health represented legacy OpenAI capabilities: %#v", health)
	}
}
