package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexdirect"
)

type fakeAPIDirectReportsProvider struct {
	*fakeDirectOAuthProvider
	report  yandexdirect.CampaignStatisticsReport
	calls   int
	token   string
	login   string
	request yandexdirect.CampaignStatisticsRequest
}

func (f *fakeAPIDirectReportsProvider) GetCampaignStatistics(
	_ context.Context, token, login string, request yandexdirect.CampaignStatisticsRequest,
) (yandexdirect.CampaignStatisticsReport, error) {
	f.calls++
	f.token, f.login, f.request = token, login, request
	return f.report, nil
}

func TestDirectCampaignStatisticsRouteUsesAdsReadAndStableContract(t *testing.T) {
	t.Parallel()
	fixture := newWorkspaceAPIFixture(t)
	const providerCampaignID int64 = 9_007_199_254_740_001
	provider := &fakeAPIDirectReportsProvider{
		fakeDirectOAuthProvider: &fakeDirectOAuthProvider{
			flow: yandexdirect.OAuthFlowVerificationCode,
		},
		report: yandexdirect.CampaignStatisticsReport{
			Status: yandexdirect.ReportStatusReady,
			Rows: []yandexdirect.CampaignStatisticsRow{
				{Date: "2043-06-01", CampaignID: providerCampaignID, Impressions: 11, Clicks: 2, CostMicros: 15_000, Conversions: 1},
				{Date: "2043-06-02", CampaignID: providerCampaignID, Impressions: 20, Clicks: 4, CostMicros: 24_999, Conversions: 2},
			},
		},
	}
	if err := fixture.app.ConfigureDirect(
		provider, []byte("0123456789abcdef0123456789abcdef"),
	); err != nil {
		t.Fatal(err)
	}
	ownerHandler := fixture.handler(t, "ws-owner")
	base := "/api/v1/workspaces/" + fixture.workspace.ID + "/advertising/direct"
	start := performJSONRequest(ownerHandler, http.MethodPost, base+"/connect/start", "")
	if start.Code != http.StatusOK {
		t.Fatalf("connect start = %d %s", start.Code, start.Body.String())
	}
	var started struct {
		Connection struct {
			State string `json:"state"`
		} `json:"connection"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &started); err != nil || started.Connection.State == "" {
		t.Fatalf("connect start payload = %#v, %v", started, err)
	}
	completeBody := fmt.Sprintf(
		`{"code":"A1b2C3d4E5f6G7h8","state":%q}`, started.Connection.State,
	)
	complete := performJSONRequest(
		ownerHandler, http.MethodPost, base+"/connect/complete", completeBody,
	)
	if complete.Code != http.StatusOK {
		t.Fatalf("connect complete = %d %s", complete.Code, complete.Body.String())
	}
	now := time.Date(2043, time.June, 1, 12, 0, 0, 0, time.UTC)
	campaign, err := fixture.storage.CreateDirectCampaign(
		t.Context(), "ws-owner", fixture.workspace.ID, store.DirectCampaign{
			Name: "Statistics campaign", Objective: "traffic",
			LandingURL: "https://maxposty.ru/", Brief: "Statistics route test",
			Regions: []string{"225"}, Titles: []string{"Safe title"},
			Texts: []string{"Safe text"}, Keywords: []string{"safe keyword"},
			WeeklyBudgetMinor: 30_000, CurrencyCode: "RUB",
			StartsAt: now, EndsAt: now.AddDate(0, 1, 0), CreatedAt: now,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.storage.ClaimDirectCampaignSubmission(
		t.Context(), "ws-owner", fixture.workspace.ID, campaign.ID, campaign.Version, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.storage.MarkDirectCampaignSubmitted(
		t.Context(), "ws-owner", fixture.workspace.ID, campaign.ID, campaign.Version,
		providerCampaignID, "DRAFT", "OFF", now,
	); err != nil {
		t.Fatal(err)
	}

	path := base + "/campaigns/" + campaign.ID +
		"/statistics?from=2043-06-01&to=2043-06-02"
	response := performJSONRequest(fixture.handler(t, "ws-viewer"), http.MethodGet, path, "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("statistics = %d headers=%v body=%s",
			response.Code, response.Header(), response.Body.String())
	}
	var payload struct {
		Statistics app.DirectCampaignStatistics `json:"statistics"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	statistics := payload.Statistics
	if statistics.CampaignID != campaign.ID || statistics.CurrencyCode != "RUB" ||
		statistics.From != "2043-06-01" || statistics.To != "2043-06-02" ||
		statistics.Totals.Impressions != 31 || statistics.Totals.Clicks != 6 ||
		statistics.Totals.CostMinor != 4 || statistics.Totals.Conversions != 3 ||
		len(statistics.Daily) != 2 || statistics.Daily[0].CostMinor != 2 ||
		statistics.Daily[1].CostMinor != 2 || statistics.UpdatedAt.IsZero() {
		t.Fatalf("statistics payload = %#v", statistics)
	}
	if provider.calls != 1 || provider.token != "provider-access-token" ||
		provider.login != "owner-login" || provider.request.CampaignID != providerCampaignID {
		t.Fatalf("provider call = %#v", provider)
	}
	if response.Body.String() == "" ||
		json.Valid(response.Body.Bytes()) == false {
		t.Fatalf("invalid statistics JSON: %s", response.Body.String())
	}

	invalid := performJSONRequest(
		fixture.handler(t, "ws-viewer"), http.MethodGet,
		base+"/campaigns/"+campaign.ID+"/statistics?from=2043-06-02&to=2043-06-01", "",
	)
	assertProblemCode(t, invalid, http.StatusUnprocessableEntity, "direct_statistics_range_invalid")
	if invalid.Header().Get("Cache-Control") != "no-store" || provider.calls != 1 {
		t.Fatalf("invalid range headers=%v provider calls=%d", invalid.Header(), provider.calls)
	}
	outside := performJSONRequest(
		fixture.handler(t, "ws-outsider"), http.MethodGet, path, "",
	)
	assertProblemCode(t, outside, http.StatusNotFound, "not_found")
}

var _ app.DirectReportsProvider = (*fakeAPIDirectReportsProvider)(nil)
