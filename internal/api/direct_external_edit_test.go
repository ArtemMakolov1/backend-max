package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexdirect"
)

type externalAPIDirectFake struct {
	*fakeDirectOAuthProvider
	detail yandexdirect.ExternalCampaignDetail
	reads  int
	writes int
}

func (f *externalAPIDirectFake) ListCampaigns(context.Context, string, string) ([]yandexdirect.CampaignSummary, error) {
	return []yandexdirect.CampaignSummary{f.detail.Campaign}, nil
}
func (f *externalAPIDirectFake) GetExternalCampaignDetail(context.Context, string, string, int64) (yandexdirect.ExternalCampaignDetail, error) {
	f.reads++
	return f.detail, nil
}
func (f *externalAPIDirectFake) UpdateExternalCampaignName(context.Context, string, string, int64, string) error {
	f.writes++
	return nil
}
func (f *externalAPIDirectFake) UpdateExternalAd(context.Context, string, string, yandexdirect.ExternalAd, yandexdirect.ExternalAdChanges) error {
	f.writes++
	return nil
}

type externalAPICopyFake struct {
	*fakeResearchClient
	copyCalls int
}

func (f *externalAPICopyFake) SuggestDirectCopy(context.Context, openairesearch.SuggestDirectCopyRequest) (openairesearch.SuggestDirectCopyResult, error) {
	f.copyCalls++
	return openairesearch.SuggestDirectCopyResult{Variants: []openairesearch.DirectCopyVariant{{Title: "Первый", Text: "Первый текст"}, {Title: "Второй", Text: "Второй текст"}, {Title: "Третий", Text: "Третий текст"}}, Rationale: []string{}, RiskWarnings: []string{}}, nil
}

func TestExternalCopyAPIChecksAuthRevisionAndUnknownSpendFieldsBeforePaidQuota(t *testing.T) {
	fake := &externalAPICopyFake{fakeResearchClient: &fakeResearchClient{}}
	limits := testAILimitOptions()
	limits.MonthlyPlanEnforcement = true
	limits.ResearchPerMinute = 1
	server, storage, raw := newAIQuotaTestServer(t, nil, fake, limits, "external-copy-owner", "external-copy-outsider")
	owner := "external-copy-owner"
	workspace := personalWorkspaceIDForTest(t, storage, owner)
	base := "/api/v1/workspaces/" + workspace + "/advertising/direct"
	href := "https://example.test/"
	provider := &externalAPIDirectFake{fakeDirectOAuthProvider: &fakeDirectOAuthProvider{flow: yandexdirect.OAuthFlowVerificationCode}, detail: yandexdirect.ExternalCampaignDetail{Campaign: yandexdirect.CampaignSummary{ID: 77, Name: "Кампания", Type: "TEXT_CAMPAIGN", Status: "ACCEPTED", State: "ON", StatusPayment: "ALLOWED", StartDate: "2025-01-01", TimeZone: "Europe/Moscow"}, Ads: []yandexdirect.ExternalAd{{ID: 12, CampaignID: 77, AdGroupID: 13, Type: "TEXT_AD", State: "ON", Title: "Текущий заголовок", Text: "Текущий текст", Href: &href, Titles: []string{}, Texts: []string{}}}}}
	if err := server.app.ConfigureDirect(provider, []byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	handler := withTestSession(t, storage, raw, owner)
	start := performJSONRequest(handler, http.MethodPost, base+"/connect/start", "")
	if start.Code != http.StatusOK {
		t.Fatalf("start %d %s", start.Code, start.Body.String())
	}
	completion := performJSONRequest(handler, http.MethodPost, base+"/connect/complete", fmt.Sprintf(`{"code":"A1b2C3d4E5f6G7h8","state":%q}`, provider.state))
	if completion.Code != http.StatusOK {
		t.Fatalf("complete %d %s", completion.Code, completion.Body.String())
	}
	sync := performJSONRequest(handler, http.MethodPost, base+"/campaigns/sync", "")
	if sync.Code != http.StatusOK {
		t.Fatalf("sync %d %s", sync.Code, sync.Body.String())
	}
	connection, err := storage.GetDirectConnection(t.Context(), owner, workspace)
	if err != nil {
		t.Fatal(err)
	}
	get := performJSONRequest(handler, http.MethodGet, base+"/campaigns/external/77?connection_id="+connection.ID, "")
	if get.Code != http.StatusOK {
		t.Fatalf("detail %d %s", get.Code, get.Body.String())
	}
	var envelope struct {
		Campaign directExternalDetailResponse `json:"campaign"`
	}
	if err = json.Unmarshal(get.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Campaign.Ads[0].Title2 != nil || envelope.Campaign.ConnectionID != connection.ID || envelope.Campaign.EditableFields == nil || envelope.Campaign.Ads[0].EditableFields == nil {
		t.Fatal("nullable or readonly detail contract missing")
	}
	if !regexp.MustCompile(`^dxr_[0-9a-f]{32}$`).MatchString(envelope.Campaign.RevisionID) {
		t.Fatalf("revision incompatible with browser contract: %q", envelope.Campaign.RevisionID)
	}
	fence := store.DirectExternalEditFence{ConnectionID: connection.ID, Version: envelope.Campaign.Version, ObservedHash: envelope.Campaign.ObservedHash, RevisionID: envelope.Campaign.RevisionID}
	request := map[string]any{"expected_connection_id": fence.ConnectionID, "expected_version": fence.Version, "expected_observed_hash": fence.ObservedHash, "expected_revision_id": fence.RevisionID, "brief": "Сделай текущее объявление яснее"}
	body := func() string { encoded, _ := json.Marshal(request); return string(encoded) }
	path := base + "/campaigns/external/77/ads/12/suggest"
	anonymous := httptest.NewRecorder()
	raw.ServeHTTP(anonymous, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body())))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatal("anonymous copy accepted")
	}
	outsider := withTestSession(t, storage, raw, "external-copy-outsider")
	if response := performJSONRequest(outsider, http.MethodPost, path, body()); response.Code != http.StatusNotFound {
		t.Fatalf("foreign creative access %d", response.Code)
	}
	request["brief"] = "x"
	assertProblemCode(t, performJSONRequest(handler, http.MethodPost, path, body()), http.StatusBadRequest, "validation_error")
	request["brief"] = "Сделай текущее объявление яснее"
	request["expected_version"] = fence.Version + 1
	response := performJSONRequest(handler, http.MethodPost, path, body())
	if response.Code != http.StatusConflict {
		t.Fatalf("stale fence %d %s", response.Code, response.Body.String())
	}
	request["expected_version"] = fence.Version
	request["weekly_budget_minor"] = 30000
	assertProblemCode(t, performJSONRequest(handler, http.MethodPost, path, body()), http.StatusBadRequest, "invalid_json")
	delete(request, "weekly_budget_minor")
	request["current_draft"] = map[string]any{"href": "http://example.test/"}
	response = performJSONRequest(handler, http.MethodPost, path, body())
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid draft %d %s", response.Code, response.Body.String())
	}
	delete(request, "current_draft")
	billing := readWorkspaceBillingForTest(t, handler, workspace)
	if fake.copyCalls != 0 || billingUsageQuantity(t, billing.Usage, store.UsageMetricAIResearchRequests) != 0 || provider.writes != 0 {
		t.Fatal("invalid request consumed quota or wrote Direct")
	}
	response = performJSONRequest(handler, http.MethodPost, path, body())
	if response.Code != http.StatusOK {
		t.Fatalf("copy %d %s", response.Code, response.Body.String())
	}
	billing = readWorkspaceBillingForTest(t, handler, workspace)
	if fake.copyCalls != 1 || billingUsageQuantity(t, billing.Usage, store.UsageMetricAIResearchRequests) != 1 || provider.writes != 0 {
		t.Fatal("copy must charge once and never write Direct")
	}
	response = performJSONRequest(handler, http.MethodPost, path, body())
	if response.Code != http.StatusTooManyRequests || fake.copyCalls != 1 {
		t.Fatal("copy bypassed shared AI rate limit")
	}
	// Budget fields and a fake nullable helper flag are never accepted by PATCH.
	for _, bad := range []string{`{"weekly_budget_minor":40000}`, `{"title2_set":true}`} {
		response = performJSONRequest(handler, http.MethodPatch, base+"/campaigns/external/77/ads/12", bad)
		assertProblemCode(t, response, http.StatusBadRequest, "invalid_json")
	}
}
func TestExternalAdPatchDistinguishesOmittedAndNullableSecondTitle(t *testing.T) {
	for _, tc := range []struct {
		body  string
		set   bool
		value *string
	}{{`{"title":"Новый"}`, false, nil}, {`{"title2":null}`, true, nil}, {`{"title2":"Второй"}`, true, func() *string { v := "Второй"; return &v }()}} {
		var request externalAdPatchRequest
		if err := json.Unmarshal([]byte(tc.body), &request); err != nil {
			t.Fatal(err)
		}
		changes, err := request.changes()
		if err != nil || changes.Title2Set != tc.set {
			t.Fatalf("nullable patch %s %+v %v", tc.body, changes, err)
		}
		if (tc.value == nil) != (changes.Title2 == nil) || (tc.value != nil && *tc.value != *changes.Title2) {
			t.Fatal("nullable title changed")
		}
	}
}
