package app

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"maxpilot/backend/internal/yandexdirect"
)

type fakeDirectReportsProvider struct {
	*fakeDirectProvider
	report      yandexdirect.CampaignStatisticsReport
	err         error
	calls       int
	token       string
	clientLogin string
	request     yandexdirect.CampaignStatisticsRequest
}

func (f *fakeDirectReportsProvider) GetCampaignStatistics(
	_ context.Context, token, clientLogin string, request yandexdirect.CampaignStatisticsRequest,
) (yandexdirect.CampaignStatisticsReport, error) {
	f.calls++
	f.token, f.clientLogin, f.request = token, clientLogin, request
	return f.report, f.err
}

func TestGetDirectCampaignStatisticsUsesOwnedConnectionAndAggregatesMicros(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	application, storage, baseProvider, owner, workspace, _, now :=
		newDirectAppFixture(t, ctx, false)
	provider := &fakeDirectReportsProvider{
		fakeDirectProvider: baseProvider,
		report: yandexdirect.CampaignStatisticsReport{
			Status: yandexdirect.ReportStatusReady, RequestID: "statistics-request",
			Units: &yandexdirect.UnitsUsage{Spent: 3, Remaining: 97, DailyLimit: 100},
			Rows: []yandexdirect.CampaignStatisticsRow{
				{Date: "2043-06-01", CampaignID: 98_001, Impressions: 10, Clicks: 2, CostMicros: 5_000, Conversions: 1},
				{Date: "2043-06-01", CampaignID: 98_001, Impressions: 20, Clicks: 3, CostMicros: 5_000, Conversions: 0.5},
				{Date: "2043-06-02", CampaignID: 98_001, Impressions: 7, Clicks: 1, CostMicros: 15_000, Conversions: 2},
			},
		},
	}
	application.direct = provider
	campaign := createDirectAppCampaign(
		t, ctx, application, owner, workspace.ID,
		time.Date(2043, time.June, 1, 0, 0, 0, 0, time.UTC),
	)
	if _, _, err := storage.ClaimDirectCampaignSubmission(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version, *now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.MarkDirectCampaignSubmitted(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
		98_001, "DRAFT", "OFF", *now,
	); err != nil {
		t.Fatal(err)
	}

	result, err := application.GetDirectCampaignStatistics(
		ctx, owner, workspace.ID, campaign.ID,
		time.Date(2043, time.June, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2043, time.June, 2, 0, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls != 1 || provider.token != "access-token" ||
		provider.clientLogin != "direct-login" || provider.request.CampaignID != 98_001 {
		t.Fatalf("provider call = %#v", provider)
	}
	if result.CampaignID != campaign.ID || result.CurrencyCode != "RUB" ||
		result.From != "2043-06-01" || result.To != "2043-06-02" ||
		result.ProviderRequestID != "statistics-request" || result.ProviderUnits == nil ||
		len(result.Daily) != 2 {
		t.Fatalf("statistics = %#v", result)
	}
	if result.Daily[0].Date != "2043-06-01" ||
		result.Daily[0].Impressions != 30 || result.Daily[0].Clicks != 5 ||
		result.Daily[0].CostMinor != 1 || result.Daily[0].Conversions != 1.5 ||
		result.Daily[1].CostMinor != 2 {
		t.Fatalf("daily = %#v", result.Daily)
	}
	if result.Totals.Impressions != 37 || result.Totals.Clicks != 6 ||
		result.Totals.CostMinor != 3 || result.Totals.Conversions != 3.5 ||
		!result.UpdatedAt.Equal(*now) {
		t.Fatalf("totals = %#v updated_at=%s", result.Totals, result.UpdatedAt)
	}
}

func TestGetDirectCampaignStatisticsRejectsLocalDraftBeforeProviderCall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	application, _, baseProvider, owner, workspace, _, now :=
		newDirectAppFixture(t, ctx, false)
	provider := &fakeDirectReportsProvider{fakeDirectProvider: baseProvider}
	application.direct = provider
	campaign := createDirectAppCampaign(t, ctx, application, owner, workspace.ID, *now)
	_, err := application.GetDirectCampaignStatistics(
		ctx, owner, workspace.ID, campaign.ID, *now, now.AddDate(0, 0, 1),
	)
	if !errors.Is(err, ErrDirectStatisticsUnavailable) || provider.calls != 0 {
		t.Fatalf("draft report error=%v calls=%d", err, provider.calls)
	}
}

func TestMicrosToRoundedDirectMinorHalfAwayFromZero(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		micros int64
		want   int64
	}{
		{micros: -15_000, want: -2},
		{micros: -5_000, want: -1},
		{micros: -4_999, want: 0},
		{micros: 0, want: 0},
		{micros: 4_999, want: 0},
		{micros: 5_000, want: 1},
		{micros: 15_000, want: 2},
	} {
		if got := microsToRoundedDirectMinor(test.micros); got != test.want {
			t.Errorf("microsToRoundedDirectMinor(%d) = %d, want %d", test.micros, got, test.want)
		}
	}
}

func TestCheckedDirectReportAddRejectsOverflow(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		left  int64
		right int64
	}{
		{left: math.MaxInt64, right: 1},
		{left: math.MinInt64, right: -1},
	} {
		if _, err := checkedDirectReportAdd(test.left, test.right); err == nil {
			t.Errorf("checkedDirectReportAdd(%d, %d) succeeded", test.left, test.right)
		}
	}
	if got, err := checkedDirectReportAdd(math.MaxInt64-1, 1); err != nil || got != math.MaxInt64 {
		t.Fatalf("bounded add = %d, %v", got, err)
	}
}

func TestAggregateDirectCampaignStatisticsRejectsProviderOverflow(t *testing.T) {
	t.Parallel()
	date := time.Date(2043, time.June, 1, 0, 0, 0, 0, time.UTC)
	_, err := aggregateDirectCampaignStatistics(
		"dcmp_overflow", "RUB", 7001, date, date,
		[]yandexdirect.CampaignStatisticsRow{
			{Date: "2043-06-01", CampaignID: 7001, CostMicros: math.MaxInt64},
			{Date: "2043-06-01", CampaignID: 7001, CostMicros: 1},
		}, date,
	)
	if err == nil {
		t.Fatal("overflowing provider report was accepted")
	}
}

var _ DirectReportsProvider = (*fakeDirectReportsProvider)(nil)
var _ DirectProvider = (*fakeDirectReportsProvider)(nil)
