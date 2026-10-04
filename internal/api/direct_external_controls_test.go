package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexdirect"
)

type externalAPIControlsFake struct {
	*externalAPIDirectFake
	app.DirectGraphProvider
	controls yandexdirect.ExternalCampaignControls
}

func (f *externalAPIControlsFake) SupportsUnifiedGraph() bool { return true }

func (f *externalAPIControlsFake) GetExternalCampaignControls(context.Context, string, string, int64) (yandexdirect.ExternalCampaignControls, error) {
	return f.controls, nil
}
func (f *externalAPIControlsFake) UpdateExternalBudget(_ context.Context, _, _ string, _ int64, _ yandexdirect.ExternalCampaignControls, key, amount, _ string) error {
	f.writes++
	for i := range f.controls.Budgets {
		if f.controls.Budgets[i].Key == key {
			f.controls.Budgets[i].AmountMicros = amount
		}
	}
	return nil
}
func (f *externalAPIControlsFake) UpdateExternalGroup(context.Context, string, string, yandexdirect.ExternalGroup, yandexdirect.ExternalGroupChanges) error {
	panic("unexpected group mutation")
}
func (f *externalAPIControlsFake) UpdateExternalKeyword(context.Context, string, string, yandexdirect.ExternalKeyword, string) (int64, error) {
	panic("unexpected keyword mutation")
}
func (f *externalAPIControlsFake) SetExternalKeywordBid(context.Context, string, string, yandexdirect.ExternalKeyword, *string, *string) error {
	panic("unexpected bid mutation")
}
func (f *externalAPIControlsFake) SetExternalCampaignState(context.Context, string, string, int64, string) error {
	panic("unexpected activation")
}
func (f *externalAPIControlsFake) AddExternalGroup(context.Context, string, string, int64, string, yandexdirect.ExternalGroupChanges) (int64, error) {
	panic("unexpected creation")
}
func (f *externalAPIControlsFake) AddExternalKeyword(context.Context, string, string, int64, string) (int64, error) {
	panic("unexpected creation")
}
func TestExternalControlAPIRequiresTenantFenceAndExactStringAmount(t *testing.T) {
	limits := testAILimitOptions()
	server, storage, raw := newAIQuotaTestServer(t, nil, nil, limits, "controls-api-owner", "controls-api-outsider")
	owner := "controls-api-owner"
	ws := personalWorkspaceIDForTest(t, storage, owner)
	base := "/api/v1/workspaces/" + ws + "/advertising/direct"
	href := "https://example.test/"
	summary := yandexdirect.CampaignSummary{ID: 77, Name: "Кампания", Type: "TEXT_CAMPAIGN", Status: "ACCEPTED", State: "ON", StatusPayment: "ALLOWED", StartDate: "2025-01-01", TimeZone: "Europe/Moscow"}
	provider := &externalAPIControlsFake{externalAPIDirectFake: &externalAPIDirectFake{fakeDirectOAuthProvider: &fakeDirectOAuthProvider{flow: yandexdirect.OAuthFlowVerificationCode}, detail: yandexdirect.ExternalCampaignDetail{Campaign: summary, Ads: []yandexdirect.ExternalAd{{ID: 12, CampaignID: 77, AdGroupID: 13, Type: "TEXT_AD", State: "ON", Title: "Заголовок", Text: "Текст", Href: &href, Titles: []string{}, Texts: []string{}}}}}, controls: yandexdirect.ExternalCampaignControls{CampaignType: "TEXT_CAMPAIGN", CurrencyCode: "RUB", LifecycleState: "ON", SearchStrategy: "HIGHEST_POSITION", NetworkStrategy: "WB_MAXIMUM_CLICKS", Regions: []yandexdirect.ExternalRegion{}, Budgets: []yandexdirect.ExternalBudget{{Key: "network_weekly", AmountMicros: "700000000", MinimumMicros: "300000000", IncrementMicros: "10000", Editable: true, StrategyType: "WB_MAXIMUM_CLICKS", Branch: "WbMaximumClicks"}}, Groups: []yandexdirect.ExternalGroup{}, StateActions: yandexdirect.ExternalStateActions{CanPause: true}, Spend: yandexdirect.ExternalSpend{Reason: "not_requested"}, MinimumBidMicros: "300000", MaximumBidMicros: "25000000000", BidIncrementMicros: "100000"}}
	if err := server.app.ConfigureDirect(provider, []byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	if err := server.app.SetDirectFeatureFlags(true, false); err != nil {
		t.Fatal(err)
	}
	handler := withTestSession(t, storage, raw, owner)
	if r := performJSONRequest(handler, http.MethodPost, base+"/connect/start", ""); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	if r := performJSONRequest(handler, http.MethodPost, base+"/connect/complete", fmt.Sprintf(`{"code":"A1b2C3d4E5f6G7h8","state":%q}`, provider.state)); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	if r := performJSONRequest(handler, http.MethodPost, base+"/campaigns/sync", ""); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	connection, err := storage.GetDirectConnection(t.Context(), owner, ws)
	if err != nil {
		t.Fatal(err)
	}
	get := performJSONRequest(handler, http.MethodGet, base+"/campaigns/external/77?connection_id="+connection.ID, "")
	if get.Code != 200 {
		t.Fatal(get.Body.String())
	}
	var d struct {
		Campaign directExternalDetailResponse `json:"campaign"`
	}
	if err = json.Unmarshal(get.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Campaign.Controls == nil || !d.Campaign.Controls.Budgets[0].Editable || d.Campaign.Controls.Spend.AmountMicros != nil {
		t.Fatal("controls or truthful spend missing")
	}
	request := map[string]any{"expected_connection_id": connection.ID, "expected_version": d.Campaign.Version, "expected_observed_hash": d.Campaign.ObservedHash, "expected_revision_id": d.Campaign.RevisionID, "client_request_id": "11111111-1111-4111-8111-111111111111", "budget_key": "network_weekly", "amount_micros": "900000000", "currency_code": "RUB"}
	body := func() string { v, _ := json.Marshal(request); return string(v) }
	path := base + "/campaigns/external/77/budget"
	anonymous := httptest.NewRecorder()
	raw.ServeHTTP(anonymous, httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body())))
	if anonymous.Code != 401 {
		t.Fatal("anonymous control accepted")
	}
	outsider := withTestSession(t, storage, raw, "controls-api-outsider")
	if r := performJSONRequest(outsider, http.MethodPatch, path, body()); r.Code != 404 {
		t.Fatalf("foreign control accepted %d", r.Code)
	}
	request["amount_micros"] = 900000000
	if r := performJSONRequest(handler, http.MethodPatch, path, body()); r.Code != 400 {
		t.Fatal("numeric micros accepted")
	}
	request["amount_micros"] = "900000000"
	request["name"] = "ignored"
	if r := performJSONRequest(handler, http.MethodPatch, path, body()); r.Code != http.StatusUnprocessableEntity {
		t.Fatal("unrelated field ignored")
	}
	delete(request, "name")
	request["expected_version"] = d.Campaign.Version + 1
	if r := performJSONRequest(handler, http.MethodPatch, path, body()); r.Code != 409 {
		t.Fatal("stale revision wrote")
	}
	request["expected_version"] = d.Campaign.Version
	if provider.writes != 0 {
		t.Fatal("invalid request wrote Direct")
	}
	saved := performJSONRequest(handler, http.MethodPatch, path, body())
	if saved.Code != 200 || saved.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("budget save %d %s", saved.Code, saved.Body.String())
	}
	if !strings.Contains(saved.Body.String(), `"amount_micros":"900000000"`) || strings.Contains(saved.Body.String(), "request_payload") || strings.Contains(saved.Body.String(), "access-token") {
		t.Fatal("contract rounded amount or leaked private data")
	}
	if r := performJSONRequest(handler, http.MethodPatch, path, body()); r.Code != 200 || provider.writes != 1 {
		t.Fatal("HTTP retry replayed budget")
	}
}
func TestExternalControlFieldsDoNotIgnoreUnrelatedInputs(t *testing.T) {
	f := store.DirectExternalEditFence{ConnectionID: "connection", Version: 1, ObservedHash: strings.Repeat("a", 64), RevisionID: "dxr_" + strings.Repeat("b", 32)}
	amount := "900000"
	base := externalControlRequest{DirectExternalEditFence: f, ClientRequestID: "11111111-1111-4111-8111-111111111111", CurrencyCode: "RUB", SearchBidMicros: &amount}
	if !externalControlFieldsValid(base, "bid") {
		t.Fatal("valid bid rejected")
	}
	base.Action = "resume"
	if externalControlFieldsValid(base, "bid") {
		t.Fatal("unrelated launch hidden in bid")
	}
	base.Action = ""
	base.CurrencyCode = ""
	if externalControlFieldsValid(base, "bid") {
		t.Fatal("bid currency omitted")
	}
}
