package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

func TestDirectAdvertisingRoutesUseDedicatedCapabilities(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	base := "/api/v1/workspaces/" + fixture.workspace.ID + "/advertising/direct"
	for _, userID := range []string{"ws-owner", "ws-editor", "ws-approver", "ws-viewer"} {
		response := performJSONRequest(fixture.handler(t, userID), http.MethodGet, base, "")
		if response.Code != http.StatusOK {
			t.Fatalf("%s GET state = %d %s", userID, response.Code, response.Body.String())
		}
	}
	response := performJSONRequest(
		fixture.handler(t, "ws-editor"), http.MethodPost, base+"/connect/start", "",
	)
	assertProblemCode(t, response, http.StatusForbidden, "workspace_forbidden")
	response = performJSONRequest(
		fixture.handler(t, "ws-owner"), http.MethodPost, base+"/connect/start", "",
	)
	assertProblemCode(t, response, http.StatusServiceUnavailable, "direct_not_configured")
	response = performJSONRequest(
		fixture.handler(t, "ws-viewer"), http.MethodPost, base+"/campaigns",
		`{"name":"No","objective":"traffic","landing_url":"https://maxposty.ru/",`+
			`"brief":"Viewer cannot create","regions":["225"],"weekly_budget_minor":10000,`+
			`"currency_code":"RUB","starts_at":"2044-01-01","ends_at":"2044-02-01"}`,
	)
	assertProblemCode(t, response, http.StatusForbidden, "workspace_forbidden")
}

func TestDirectCampaignValidationIsAClientErrorForCreateAndPatch(t *testing.T) {
	fixture := newWorkspaceAPIFixture(t)
	base := "/api/v1/workspaces/" + fixture.workspace.ID + "/advertising/direct"
	handler := fixture.handler(t, "ws-owner")
	for name, body := range map[string]string{
		"fragment": `{"name":"Campaign","objective":"traffic","landing_url":"https://maxposty.ru/#fragment",` +
			`"brief":"A valid campaign brief","regions":["225"],"weekly_budget_minor":30000,` +
			`"currency_code":"RUB","starts_at":"2044-01-01","ends_at":"2044-02-01"}`,
		"budget": `{"name":"Campaign","objective":"traffic","landing_url":"https://maxposty.ru/",` +
			`"brief":"A valid campaign brief","regions":["225"],"weekly_budget_minor":29999,` +
			`"currency_code":"RUB","starts_at":"2044-01-01","ends_at":"2044-02-01"}`,
		"objective": `{"name":"Campaign","objective":"INVALID OBJECTIVE","landing_url":"https://maxposty.ru/",` +
			`"brief":"A valid campaign brief","regions":["225"],"weekly_budget_minor":30000,` +
			`"currency_code":"RUB","starts_at":"2044-01-01","ends_at":"2044-02-01"}`,
		"currency": `{"name":"Campaign","objective":"traffic","landing_url":"https://maxposty.ru/",` +
			`"brief":"A valid campaign brief","regions":["225"],"weekly_budget_minor":30000,` +
			`"currency_code":"USD","starts_at":"2044-01-01","ends_at":"2044-02-01"}`,
	} {
		t.Run(name, func(t *testing.T) {
			response := performJSONRequest(handler, http.MethodPost, base+"/campaigns", body)
			assertProblemCode(t, response, http.StatusUnprocessableEntity, "direct_validation_error")
		})
	}

	now := time.Date(2043, time.December, 1, 12, 0, 0, 0, time.UTC)
	if _, err := fixture.storage.ReplaceDirectConnection(
		t.Context(), "ws-owner", fixture.workspace.ID, store.DirectConnection{
			AccountID: "validation-account", CurrencyCode: "RUB", Timezone: "Europe/Moscow",
			TokenCiphertext: "v1.validation", TokenKeyVersion: 1, CreatedAt: now,
		},
	); err != nil {
		t.Fatal(err)
	}
	campaign, err := fixture.storage.CreateDirectCampaign(
		t.Context(), "ws-owner", fixture.workspace.ID, store.DirectCampaign{
			Name: "Campaign", Objective: "traffic", LandingURL: "https://maxposty.ru/",
			Brief: "A valid campaign brief", Regions: []string{"225"},
			WeeklyBudgetMinor: 30_000, CurrencyCode: "RUB",
			StartsAt: now, EndsAt: now.AddDate(0, 1, 0), CreatedAt: now,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	response := performJSONRequest(handler, http.MethodPatch,
		base+"/campaigns/"+campaign.ID,
		`{"landing_url":"https://maxposty.ru/#fragment","expected_version":1}`)
	assertProblemCode(t, response, http.StatusUnprocessableEntity, "direct_validation_error")
}

func TestPublicDirectCampaignExposesTruthfulSafeLaunchState(t *testing.T) {
	t.Parallel()
	providerID := int64(9_007_199_254_740_001)
	now := time.Date(2044, time.January, 2, 3, 4, 5, 0, time.UTC)
	response := publicDirectCampaign(store.DirectCampaign{
		ID: "dcmp_safe", ConnectionID: "dcon_safe", ProviderCampaignID: &providerID,
		Name: "Draft in Direct", Objective: "traffic", LandingURL: "https://maxposty.ru/",
		Brief: "A provider-side draft", Regions: []string{"225"},
		WeeklyBudgetMinor: 25_000, CurrencyCode: "RUB",
		StartsAt: now, EndsAt: now.AddDate(0, 1, 0),
		Status: "provider_draft", ProviderStatus: "DRAFT", ProviderState: "OFF",
		LaunchState: "reconciling", LaunchFailureCode: "provider_timeout",
		Version: 1, CreatedAt: now, UpdatedAt: now,
		AutoLaunch: store.DirectAutoLaunchSummary{Enabled: false, Valid: false},
	})
	if response.Status != "provider_draft" || response.LaunchState != "reconciling" ||
		response.ProviderCampaignID == nil || *response.ProviderCampaignID != "9007199254740001" {
		t.Fatalf("public campaign = %#v", response)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"token_ciphertext", "launch_attempt_count", "launch_reconcile_after"} {
		if _, ok := payload[forbidden]; ok {
			t.Fatalf("public campaign leaks internal field %q: %s", forbidden, encoded)
		}
	}
}

func TestPublicDirectConnectionExposesWritableStateWithoutLeakingProviderText(t *testing.T) {
	t.Parallel()
	response := publicDirectConnection(store.DirectConnection{
		ID: "dcon_safe", AccountID: "account-safe", Status: "unexpected provider status",
		ReadOnly: true, ErrorCode: "authorization failed: token=secret",
	})
	if response.Status != "error" || !response.ReadOnly ||
		response.ErrorCode != "connection_error" {
		t.Fatalf("public connection = %#v", response)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "token=secret") {
		t.Fatalf("public connection leaked provider text: %s", encoded)
	}
	response = publicDirectConnection(store.DirectConnection{
		Status: "error", ErrorCode: "authorization_required",
	})
	if response.ErrorCode != "authorization_required" {
		t.Fatalf("safe connection code was lost: %#v", response)
	}
}

func TestCanonicalPositiveDirectIntegerMatchesPublicContract(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"1", "9007199254740001"} {
		if !canonicalPositiveDirectInteger(value) {
			t.Fatalf("canonical provider id %q was rejected", value)
		}
	}
	for _, value := range []string{"", "0", "01", "+1", "-1", "1.0", "1 "} {
		if canonicalPositiveDirectInteger(value) {
			t.Fatalf("non-canonical provider id %q was accepted", value)
		}
	}
}

func TestDirectSuggestionUsesResearchEntitlementAndUsage(t *testing.T) {
	t.Parallel()
	fake := &fakeResearchClient{directResult: openairesearch.SuggestDirectCampaignResult{
		CampaignName: "Campaign draft",
	}}
	options := testAILimitOptions()
	server, storage, rawHandler := newAIQuotaTestServer(
		t, nil, fake, options, "direct-suggest-paid",
	)
	server.now = func() time.Time { return time.Now().UTC() }
	handler := withTestSession(t, storage, rawHandler, "direct-suggest-paid")
	workspaceID := personalWorkspaceIDForTest(t, storage, "direct-suggest-paid")
	response := performJSONRequest(handler, http.MethodPost,
		"/api/v1/workspaces/"+workspaceID+"/advertising/direct/campaigns/suggest",
		validDirectSuggestionBody)
	if response.Code != http.StatusOK {
		t.Fatalf("Direct suggestion = %d %s", response.Code, response.Body.String())
	}
	fake.mu.Lock()
	callCount := len(fake.directRequests)
	fake.mu.Unlock()
	if callCount != 1 {
		t.Fatalf("Direct suggestion upstream calls = %d, want 1", callCount)
	}
	billing := readWorkspaceBillingForTest(t, handler, workspaceID)
	if billingUsageQuantity(t, billing.Usage, store.UsageMetricAIResearchRequests) != 1 ||
		billingUsageQuantity(t, billing.Usage, store.UsageMetricAIFormatRequests) != 0 {
		t.Fatalf("Direct suggestion charged wrong metric: %#v", billing.Usage)
	}
}

func TestDirectSuggestionFreePlanAndMonthlyLimitStopBeforeProvider(t *testing.T) {
	t.Parallel()
	t.Run("free plan", func(t *testing.T) {
		fake := &fakeResearchClient{}
		_, storage, rawHandler := newFreeAIQuotaTestServer(
			t, nil, fake, testAILimitOptions(), "direct-suggest-free",
		)
		handler := withTestSession(t, storage, rawHandler, "direct-suggest-free")
		workspaceID := personalWorkspaceIDForTest(t, storage, "direct-suggest-free")
		response := performJSONRequest(handler, http.MethodPost,
			"/api/v1/workspaces/"+workspaceID+"/advertising/direct/campaigns/suggest",
			validDirectSuggestionBody)
		assertProblemCode(t, response, http.StatusForbidden, "plan_upgrade_required")
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if len(fake.directRequests) != 0 {
			t.Fatalf("free plan reached Direct suggestion provider: %#v", fake.directRequests)
		}
	})

	t.Run("solo monthly research limit", func(t *testing.T) {
		fake := &fakeResearchClient{}
		options := testAILimitOptions()
		options.MonthlyPlanEnforcement = true
		options.ResearchPerMinute = 20
		server, storage, rawHandler := newAIQuotaTestServer(
			t, nil, fake, options, "direct-suggest-limit",
		)
		server.now = func() time.Time { return time.Now().UTC() }
		handler := withTestSession(t, storage, rawHandler, "direct-suggest-limit")
		workspaceID := personalWorkspaceIDForTest(t, storage, "direct-suggest-limit")
		path := "/api/v1/workspaces/" + workspaceID + "/advertising/direct/campaigns/suggest"
		for requestIndex := 0; requestIndex < 8; requestIndex++ {
			response := performJSONRequest(handler, http.MethodPost, path, validDirectSuggestionBody)
			if response.Code != http.StatusOK {
				t.Fatalf("Direct suggestion %d = %d %s",
					requestIndex, response.Code, response.Body.String())
			}
		}
		response := performJSONRequest(handler, http.MethodPost, path, validDirectSuggestionBody)
		assertProblemCode(t, response, http.StatusTooManyRequests, "ai_rate_limited")
		if !strings.Contains(response.Body.String(), `"reason":"monthly"`) {
			t.Fatalf("monthly Direct suggestion response = %s", response.Body.String())
		}
		fake.mu.Lock()
		callCount := len(fake.directRequests)
		fake.mu.Unlock()
		if callCount != 8 {
			t.Fatalf("monthly rejection reached provider: calls=%d", callCount)
		}
		billing := readWorkspaceBillingForTest(t, handler, workspaceID)
		if billingUsageQuantity(t, billing.Usage, store.UsageMetricAIResearchRequests) != 8 ||
			billingUsageQuantity(t, billing.Usage, store.UsageMetricAIFormatRequests) != 0 {
			t.Fatalf("Direct monthly usage = %#v", billing.Usage)
		}
	})
}

func TestDirectProviderErrorMessageDoesNotPromiseLaunchReconciliation(t *testing.T) {
	t.Parallel()
	response := httptest.NewRecorder()
	(&Server{}).writeError(response, app.ErrDirectProvider)
	assertProblemCode(t, response, http.StatusBadGateway, "direct_provider_error")
	body := strings.ToLower(response.Body.String())
	if strings.Contains(body, "запуск") || strings.Contains(body, "сверен") ||
		strings.Contains(body, "reconcil") {
		t.Fatalf("generic provider error promises launch reconciliation: %s", response.Body.String())
	}
}

const validDirectSuggestionBody = `{
	"objective":"traffic",
	"landing_url":"https://maxposty.ru/",
	"brief":"Promote practical channel content to a relevant business audience.",
	"audience":"small business owners",
	"regions":["225"],
	"weekly_budget_minor":30000,
	"currency_code":"RUB",
	"starts_at":"2044-01-01",
	"ends_at":"2044-02-01"
}`
