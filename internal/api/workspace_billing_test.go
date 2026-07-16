package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

func TestPublicPlansExposeOnlyFreeWhileWorkspaceBillingIsTenantScoped(t *testing.T) {
	options := testAILimitOptions()
	_, storage, rawHandler := newAIQuotaTestServer(
		t, nil, nil, options, "plans-member", "plans-outsider")
	public := performJSONRequest(rawHandler, http.MethodGet, "/api/v1/plans", "")
	if public.Code != http.StatusOK || !strings.Contains(public.Header().Get("Cache-Control"), "public") {
		t.Fatalf("public plans = %d %s headers=%v", public.Code, public.Body.String(), public.Header())
	}
	var catalog []store.BillingCatalogEntry
	if err := json.Unmarshal(public.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog) != 1 || catalog[0].Plan.Code != "free" ||
		strings.Contains(public.Body.String(), "solo") || strings.Contains(public.Body.String(), "agency") {
		t.Fatalf("public catalog leaked internal plans: %s", public.Body.String())
	}

	workspace, err := storage.CreateWorkspace(t.Context(), "plans-member", store.Workspace{Name: "Plans"})
	if err != nil {
		t.Fatal(err)
	}
	member := withTestSession(t, storage, rawHandler, "plans-member")
	response := performJSONRequest(member, http.MethodGet,
		"/api/v1/workspaces/"+workspace.ID+"/billing", "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("member billing = %d %s headers=%v", response.Code, response.Body.String(), response.Header())
	}
	var billing workspaceBillingResponse
	if err := json.Unmarshal(response.Body.Bytes(), &billing); err != nil {
		t.Fatal(err)
	}
	if billing.WorkspaceID != workspace.ID || billing.Subscription.Plan.Code != "free" ||
		billing.MonthlyEnforcementEnabled {
		t.Fatalf("billing response = %#v", billing)
	}

	outsider := withTestSession(t, storage, rawHandler, "plans-outsider")
	response = performJSONRequest(outsider, http.MethodGet,
		"/api/v1/workspaces/"+workspace.ID+"/billing", "")
	assertProblemCode(t, response, http.StatusNotFound, "not_found")
}

func TestMonthlyImageCreditsObserveModeCountsAcrossLegacyAndWorkspaceRoutes(t *testing.T) {
	image := &quotaImageClient{}
	options := testAILimitOptions()
	options.ImagePerMinute = 10
	server, storage, rawHandler := newAIQuotaTestServer(t, image, nil, options, "credits-observe")
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	server.now = func() time.Time { return now }
	handler := withTestSession(t, storage, rawHandler, "credits-observe")
	workspaceID := personalWorkspaceIDForTest(t, storage, "credits-observe")

	response := performJSONRequest(handler, http.MethodPost, "/api/v1/images/generate",
		`{"prompt":"expensive","quality":"high"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("high image = %d %s", response.Code, response.Body.String())
	}
	response = performJSONRequest(handler, http.MethodPost,
		"/api/v1/workspaces/"+workspaceID+"/images/generate",
		`{"prompt":"cheap","quality":"low"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("low nested image = %d %s", response.Code, response.Body.String())
	}
	if image.callCount() != 2 {
		t.Fatalf("upstream calls=%d, want 2", image.callCount())
	}

	billing := readWorkspaceBillingForTest(t, handler, workspaceID)
	if billing.MonthlyEnforcementEnabled ||
		billingUsageQuantity(t, billing.Usage, store.UsageMetricAIImageCredits) != 37 {
		t.Fatalf("observe billing = %#v", billing)
	}
}

func TestMonthlyImageCreditsEnforcementRejectsBeforeUpstreamWithoutRouteBypass(t *testing.T) {
	image := &quotaImageClient{}
	options := testAILimitOptions()
	options.ImagePerMinute = 10
	options.MonthlyPlanEnforcement = true
	server, storage, rawHandler := newAIQuotaTestServer(t, image, nil, options, "credits-enforce")
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	server.now = func() time.Time { return now }
	handler := withTestSession(t, storage, rawHandler, "credits-enforce")
	workspaceID := personalWorkspaceIDForTest(t, storage, "credits-enforce")
	paths := []string{
		"/api/v1/images/generate",
		"/api/v1/workspaces/" + workspaceID + "/images/generate",
		"/api/v1/images/generate",
	}
	for index, path := range paths {
		response := performJSONRequest(handler, http.MethodPost, path,
			`{"prompt":"medium `+formatInt64(int64(index))+`","quality":"medium"}`)
		if response.Code != http.StatusCreated {
			t.Fatalf("medium image %d = %d %s", index, response.Code, response.Body.String())
		}
	}
	response := performJSONRequest(handler, http.MethodPost,
		"/api/v1/workspaces/"+workspaceID+"/images/generate",
		`{"prompt":"over limit","quality":"low"}`)
	assertProblemCode(t, response, http.StatusTooManyRequests, "ai_rate_limited")
	if !strings.Contains(response.Body.String(), `"reason":"monthly"`) {
		t.Fatalf("monthly rejection body=%s", response.Body.String())
	}
	if image.callCount() != 3 {
		t.Fatalf("monthly rejection reached upstream; calls=%d", image.callCount())
	}
	billing := readWorkspaceBillingForTest(t, handler, workspaceID)
	if !billing.MonthlyEnforcementEnabled ||
		billingUsageQuantity(t, billing.Usage, store.UsageMetricAIImageCredits) != 27 {
		t.Fatalf("enforced billing = %#v", billing)
	}
}

func TestInactiveWorkspacePlanBlocksAIWithoutUsageInObserveAndEnforceModes(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		status  string
		enforce bool
	}{
		{name: "paused-observe", status: "paused"},
		{name: "paused-enforce", status: "paused", enforce: true},
		{name: "canceled-observe", status: "canceled"},
		{name: "canceled-enforce", status: "canceled", enforce: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			image := &quotaImageClient{}
			options := testAILimitOptions()
			options.MonthlyPlanEnforcement = testCase.enforce
			server, storage, rawHandler := newAIQuotaTestServer(
				t, image, nil, options, "inactive-"+testCase.name)
			now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
			server.now = func() time.Time { return now }
			userID := "inactive-" + testCase.name
			handler := withTestSession(t, storage, rawHandler, userID)
			workspaceID := personalWorkspaceIDForTest(t, storage, userID)
			if err := storage.UpdateWorkspaceSubscriptionStatus(
				t.Context(), workspaceID, testCase.status, now); err != nil {
				t.Fatal(err)
			}

			response := performJSONRequest(handler, http.MethodPost, "/api/v1/images/generate",
				`{"prompt":"must not reach provider","quality":"low"}`)
			assertProblemCode(t, response, http.StatusForbidden, "plan_inactive")
			if response.Header().Get("Cache-Control") != "no-store" ||
				!strings.Contains(response.Body.String(), `"status":"`+testCase.status+`"`) {
				t.Fatalf("inactive response headers/body = %#v %s", response.Header(), response.Body.String())
			}
			if image.callCount() != 0 {
				t.Fatalf("inactive request reached upstream %d times", image.callCount())
			}
			billing := readWorkspaceBillingForTest(t, handler, workspaceID)
			if billing.Subscription.Status != testCase.status ||
				billingUsageQuantity(t, billing.Usage, store.UsageMetricAIImageCredits) != 0 {
				t.Fatalf("inactive billing = %#v", billing)
			}
		})
	}
}

func TestResearchAndFormattingHaveSeparateMonthlyMetrics(t *testing.T) {
	research := &fakeResearchClient{
		result:       openairesearch.Result{Topic: "Research"},
		formatResult: openairesearch.FormatResult{Content: "Formatted"},
	}
	options := testAILimitOptions()
	server, storage, rawHandler := newAIQuotaTestServer(t, nil, research, options, "metric-split")
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	server.now = func() time.Time { return now }
	handler := withTestSession(t, storage, rawHandler, "metric-split")
	workspaceID := personalWorkspaceIDForTest(t, storage, "metric-split")

	response := performJSONRequest(handler, http.MethodPost, "/api/v1/posts/format-content",
		`{"content":"Text","format":"markdown"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("format = %d %s", response.Code, response.Body.String())
	}
	response = performJSONRequest(handler, http.MethodPost, "/api/v1/research/generate",
		`{"topic":"Topic","tone":"business","format":"markdown","include_sources":false}`)
	if response.Code != http.StatusOK {
		t.Fatalf("research = %d %s", response.Code, response.Body.String())
	}
	billing := readWorkspaceBillingForTest(t, handler, workspaceID)
	if billingUsageQuantity(t, billing.Usage, store.UsageMetricAIFormatRequests) != 1 ||
		billingUsageQuantity(t, billing.Usage, store.UsageMetricAIResearchRequests) != 1 {
		t.Fatalf("split monthly metrics = %#v", billing.Usage)
	}
}

func TestImageUsageCredits(t *testing.T) {
	for quality, want := range map[string]int64{
		"low": 1, "medium": 9, "": 9, "high": 36, "auto": 36,
	} {
		if got := imageUsageCredits(quality); got != want {
			t.Fatalf("quality %q credits=%d, want %d", quality, got, want)
		}
	}
}

func personalWorkspaceIDForTest(t *testing.T, storage *store.Store, userID string) string {
	t.Helper()
	workspaces, err := storage.ListWorkspaces(t.Context(), userID)
	if err != nil {
		t.Fatal(err)
	}
	for _, access := range workspaces {
		if access.Workspace.IsPersonal {
			return access.Workspace.ID
		}
	}
	t.Fatalf("personal workspace missing for %s", userID)
	return ""
}

func readWorkspaceBillingForTest(t *testing.T, handler http.Handler, workspaceID string) workspaceBillingResponse {
	t.Helper()
	response := performJSONRequest(handler, http.MethodGet,
		"/api/v1/workspaces/"+workspaceID+"/billing", "")
	if response.Code != http.StatusOK {
		t.Fatalf("billing = %d %s", response.Code, response.Body.String())
	}
	var billing workspaceBillingResponse
	if err := json.Unmarshal(response.Body.Bytes(), &billing); err != nil {
		t.Fatal(err)
	}
	return billing
}

func billingUsageQuantity(t *testing.T, usage []store.WorkspaceUsageMetric, metric string) int64 {
	t.Helper()
	for _, item := range usage {
		if item.Metric == metric {
			return item.Quantity
		}
	}
	t.Fatalf("usage metric %s missing from %#v", metric, usage)
	return 0
}
