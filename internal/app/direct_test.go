package app

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexdirect"
)

type fakeDirectProvider struct {
	account         yandexdirect.Account
	campaign        yandexdirect.Campaign
	graphSupported  bool
	operationMarker string
	group           yandexdirect.UnifiedAdGroup
	ad              yandexdirect.ResponsiveAd
	keywords        []yandexdirect.Keyword
	nextGraphID     int64
	oauthFlow       yandexdirect.OAuthFlow
	resumeCalls     int
	resume          func(*fakeDirectProvider) error
	getErr          error
	getContext      func(context.Context)
	refreshCalls    int
	refreshErr      error
	refreshResult   yandexdirect.OAuthToken
}

func (f *fakeDirectProvider) OAuthFlow() yandexdirect.OAuthFlow {
	if f.oauthFlow == "" {
		return yandexdirect.OAuthFlowCallback
	}
	return f.oauthFlow
}
func (f *fakeDirectProvider) AuthorizationURL(string, string) string { return "https://oauth.test/" }
func (f *fakeDirectProvider) ExchangeCode(
	context.Context, string, string,
) (yandexdirect.OAuthToken, error) {
	return yandexdirect.OAuthToken{
		AccessToken: "token", RefreshToken: "refresh-token",
		ExpiresInSeconds: int64((24 * time.Hour) / time.Second),
	}, nil
}
func (f *fakeDirectProvider) RefreshToken(
	context.Context, string,
) (yandexdirect.OAuthToken, error) {
	f.refreshCalls++
	if f.refreshErr != nil {
		return yandexdirect.OAuthToken{}, f.refreshErr
	}
	if f.refreshResult.AccessToken != "" {
		return f.refreshResult, nil
	}
	return yandexdirect.OAuthToken{
		AccessToken: "refreshed-token", RefreshToken: "rotated-refresh-token",
		ExpiresInSeconds: int64((24 * time.Hour) / time.Second),
	}, nil
}
func (f *fakeDirectProvider) GetAccount(context.Context, string, string) (yandexdirect.Account, error) {
	return f.account, nil
}
func (f *fakeDirectProvider) CreateCampaignDraft(
	_ context.Context, _, _ string, draft yandexdirect.CampaignDraft,
) (yandexdirect.Campaign, error) {
	f.campaign.Name = draft.Name
	f.campaign.WeeklyBudgetMinor = draft.WeeklyBudgetMinor
	f.campaign.StartsAt = draft.StartsAt
	f.campaign.EndsAt = draft.EndsAt
	if f.campaign.ID == 0 {
		f.campaign.ID = 98_001
	}
	f.campaign.Status = "DRAFT"
	f.campaign.State = "OFF"
	f.campaign.TimeZone = draft.TimeZone
	f.campaign.TrackingParams = "mp_op=" + draft.OperationMarker
	f.operationMarker = draft.OperationMarker
	if f.nextGraphID <= f.campaign.ID {
		f.nextGraphID = f.campaign.ID + 1
	}
	return f.campaign, nil
}
func (f *fakeDirectProvider) GetCampaign(
	ctx context.Context, _ string, _ string, _ int64,
) (yandexdirect.Campaign, error) {
	if f.getContext != nil {
		f.getContext(ctx)
	}
	if f.getErr != nil {
		return yandexdirect.Campaign{}, f.getErr
	}
	return f.campaign, nil
}
func (f *fakeDirectProvider) ResumeCampaign(context.Context, string, string, int64) error {
	f.resumeCalls++
	if f.resume != nil {
		return f.resume(f)
	}
	f.campaign.State = "ON"
	return nil
}
func (f *fakeDirectProvider) Sandbox() bool { return true }

func (f *fakeDirectProvider) SupportsUnifiedGraph() bool {
	return f.graphSupported
}

func (f *fakeDirectProvider) ResolveRegionNames(
	_ context.Context, _, _ string, names []string,
) ([]yandexdirect.GeoRegion, error) {
	regions := make([]yandexdirect.GeoRegion, len(names))
	for index, name := range names {
		id := int64(225 + index)
		if name == "225" {
			id = 225
		}
		regions[index] = yandexdirect.GeoRegion{ID: id, Name: name}
	}
	return regions, nil
}

func (f *fakeDirectProvider) CreateUnifiedAdGroup(
	_ context.Context, _, _ string, draft yandexdirect.UnifiedAdGroupDraft,
) (yandexdirect.MutationResult, error) {
	id := f.nextID()
	f.group = yandexdirect.UnifiedAdGroup{
		ID: id, CampaignID: draft.CampaignID, Name: draft.Name,
		RegionIDs:        append([]int64(nil), draft.RegionIDs...),
		NegativeKeywords: append([]string(nil), draft.NegativeKeywords...),
		OfferRetargeting: "NO", Status: "DRAFT",
	}
	return yandexdirect.MutationResult{ID: id}, nil
}

func (f *fakeDirectProvider) ListUnifiedAdGroups(
	_ context.Context, _, _ string, campaignID int64,
) ([]yandexdirect.UnifiedAdGroup, error) {
	if f.group.ID == 0 || f.group.CampaignID != campaignID {
		return []yandexdirect.UnifiedAdGroup{}, nil
	}
	group, _, _ := f.graphChildren()
	return []yandexdirect.UnifiedAdGroup{group}, nil
}

func (f *fakeDirectProvider) UpdateUnifiedAdGroups(
	_ context.Context, _, _ string, values []yandexdirect.UnifiedAdGroupUpdate,
) ([]yandexdirect.MutationResult, error) {
	results := make([]yandexdirect.MutationResult, len(values))
	for index, value := range values {
		if value.ID == f.group.ID {
			f.group.Name = value.Name
			f.group.RegionIDs = append([]int64(nil), value.RegionIDs...)
			f.group.NegativeKeywords = append(
				[]string(nil), value.NegativeKeywords...,
			)
		}
		results[index].ID = value.ID
	}
	return results, nil
}

func (f *fakeDirectProvider) CreateResponsiveAd(
	_ context.Context, _, _ string, draft yandexdirect.ResponsiveAdDraft,
) (yandexdirect.MutationResult, error) {
	id := f.nextID()
	f.ad = yandexdirect.ResponsiveAd{
		ID: id, CampaignID: f.campaign.ID, AdGroupID: draft.AdGroupID,
		Titles: fakeModeratedTexts(draft.Titles, "DRAFT"),
		Texts:  fakeModeratedTexts(draft.Texts, "DRAFT"),
		Href:   draft.Href, Status: "DRAFT", State: "OFF",
	}
	return yandexdirect.MutationResult{ID: id}, nil
}

func (f *fakeDirectProvider) ListResponsiveAds(
	_ context.Context, _, _ string, campaignID int64,
) ([]yandexdirect.ResponsiveAd, error) {
	if f.ad.ID == 0 || f.ad.CampaignID != campaignID {
		return []yandexdirect.ResponsiveAd{}, nil
	}
	_, ad, _ := f.graphChildren()
	return []yandexdirect.ResponsiveAd{ad}, nil
}

func (f *fakeDirectProvider) UpdateResponsiveAds(
	_ context.Context, _, _ string, values []yandexdirect.ResponsiveAdUpdate,
) ([]yandexdirect.MutationResult, error) {
	results := make([]yandexdirect.MutationResult, len(values))
	for index, value := range values {
		if value.ID == f.ad.ID {
			f.ad.Titles = fakeModeratedTexts(value.Titles, "DRAFT")
			f.ad.Texts = fakeModeratedTexts(value.Texts, "DRAFT")
			f.ad.Href = value.Href
		}
		results[index].ID = value.ID
	}
	return results, nil
}

func (f *fakeDirectProvider) AddKeywords(
	_ context.Context, _, _ string, drafts []yandexdirect.KeywordDraft,
) ([]yandexdirect.MutationResult, error) {
	results := make([]yandexdirect.MutationResult, len(drafts))
	for index, draft := range drafts {
		id := f.nextID()
		f.keywords = append(f.keywords, yandexdirect.Keyword{
			ID: id, CampaignID: f.campaign.ID, AdGroupID: draft.AdGroupID,
			Keyword: draft.Keyword, StrategyPriority: "NORMAL",
			Status: "DRAFT", State: "OFF",
		})
		results[index].ID = id
	}
	return results, nil
}

func (f *fakeDirectProvider) UpdateKeywords(
	_ context.Context, _, _ string, values []yandexdirect.KeywordUpdate,
) ([]yandexdirect.MutationResult, error) {
	results := make([]yandexdirect.MutationResult, len(values))
	for index, value := range values {
		for keywordIndex := range f.keywords {
			if f.keywords[keywordIndex].ID == value.ID {
				f.keywords[keywordIndex].Keyword = value.Keyword
			}
		}
		results[index].ID = value.ID
	}
	return results, nil
}

func (f *fakeDirectProvider) DeleteKeywords(
	_ context.Context, _, _ string, ids []int64,
) ([]yandexdirect.MutationResult, error) {
	remove := make(map[int64]struct{}, len(ids))
	results := make([]yandexdirect.MutationResult, len(ids))
	for index, id := range ids {
		remove[id] = struct{}{}
		results[index].ID = id
	}
	kept := f.keywords[:0]
	for _, keyword := range f.keywords {
		if _, ok := remove[keyword.ID]; !ok {
			kept = append(kept, keyword)
		}
	}
	f.keywords = kept
	return results, nil
}

func (f *fakeDirectProvider) ListKeywords(
	_ context.Context, _, _ string, campaignID int64,
) ([]yandexdirect.Keyword, error) {
	if f.campaign.ID != campaignID {
		return []yandexdirect.Keyword{}, nil
	}
	_, _, keywords := f.graphChildren()
	return keywords, nil
}

func (f *fakeDirectProvider) ModerateAds(
	_ context.Context, _, _ string, ids []int64,
) ([]yandexdirect.MutationResult, error) {
	results := make([]yandexdirect.MutationResult, len(ids))
	for index, id := range ids {
		if id == f.ad.ID {
			f.ad.Status = "MODERATION"
			for titleIndex := range f.ad.Titles {
				f.ad.Titles[titleIndex].Status = "MODERATION"
			}
			for textIndex := range f.ad.Texts {
				f.ad.Texts[textIndex].Status = "MODERATION"
			}
		}
		results[index].ID = id
	}
	return results, nil
}

func (f *fakeDirectProvider) FindUnifiedCampaignByOperationMarker(
	_ context.Context, _, _, marker string,
) (int64, error) {
	if f.campaign.ID != 0 && f.operationMarker == marker {
		return f.campaign.ID, nil
	}
	return 0, nil
}

func (f *fakeDirectProvider) FindUnifiedAdGroupByTrackingMarker(
	_ context.Context, _, _ string, campaignID int64, marker string,
) (int64, error) {
	if f.group.ID != 0 && f.group.CampaignID == campaignID &&
		f.group.Name == yandexdirect.UnifiedAdGroupOperationName(marker) {
		return f.group.ID, nil
	}
	return 0, nil
}

func (f *fakeDirectProvider) EnsureNoBidModifiers(
	context.Context, string, string, int64,
) error {
	return nil
}

func (f *fakeDirectProvider) EnsureNoAudienceTargets(
	context.Context, string, string, int64,
) error {
	return nil
}

func (f *fakeDirectProvider) UpdateUnifiedCampaigns(
	_ context.Context, _, _ string, values []yandexdirect.UnifiedCampaignUpdate,
) ([]yandexdirect.MutationResult, error) {
	results := make([]yandexdirect.MutationResult, len(values))
	for index, value := range values {
		if value.ID == f.campaign.ID {
			f.campaign.Name = value.Name
			f.campaign.WeeklyBudgetMinor = value.WeeklyBudgetMinor
			f.campaign.StartsAt = value.StartsAt
			f.campaign.EndsAt = value.EndsAt
			f.campaign.TrackingParams = value.TrackingParams
		}
		results[index].ID = value.ID
	}
	return results, nil
}

func (f *fakeDirectProvider) GetCampaignGraph(
	ctx context.Context, _, _ string, campaignID int64,
) (yandexdirect.CampaignGraph, error) {
	if f.getContext != nil {
		f.getContext(ctx)
	}
	if f.getErr != nil {
		return yandexdirect.CampaignGraph{}, f.getErr
	}
	if f.campaign.ID != campaignID {
		return yandexdirect.CampaignGraph{}, &yandexdirect.Error{Code: "not_found"}
	}
	strategy, err := yandexdirect.SafeUnifiedCampaignBiddingStrategy(
		f.campaign.WeeklyBudgetMinor,
	)
	if err != nil {
		return yandexdirect.CampaignGraph{}, err
	}
	group, ad, keywords := f.graphChildren()
	graph := yandexdirect.CampaignGraph{
		Campaign: yandexdirect.GraphCampaign{
			ID: f.campaign.ID, Name: f.campaign.Name,
			Status: f.campaign.Status, State: f.campaign.State,
			Type:              "UNIFIED_CAMPAIGN",
			WeeklyBudgetMinor: f.campaign.WeeklyBudgetMinor,
			StartsAt:          f.campaign.StartsAt, EndsAt: f.campaign.EndsAt,
			TimeZone:         f.campaign.TimeZone,
			TimeTargeting:    yandexdirect.SafeUnifiedCampaignTimeTargeting(),
			BiddingStrategy:  strategy,
			Settings:         yandexdirect.SafeUnifiedCampaignSettings(),
			TrackingParams:   f.campaign.TrackingParams,
			AttributionModel: "AUTO",
		},
	}
	if group.ID != 0 {
		graph.AdGroups = []yandexdirect.UnifiedAdGroup{group}
	}
	if ad.ID != 0 {
		graph.Ads = []yandexdirect.ResponsiveAd{ad}
	}
	graph.Keywords = keywords
	return graph, nil
}

func (f *fakeDirectProvider) nextID() int64 {
	if f.nextGraphID <= f.campaign.ID {
		f.nextGraphID = f.campaign.ID + 1
	}
	id := f.nextGraphID
	f.nextGraphID++
	return id
}

func (f *fakeDirectProvider) graphChildren() (
	yandexdirect.UnifiedAdGroup, yandexdirect.ResponsiveAd, []yandexdirect.Keyword,
) {
	group := f.group
	ad := f.ad
	ad.Titles = append([]yandexdirect.ModeratedText(nil), f.ad.Titles...)
	ad.Texts = append([]yandexdirect.ModeratedText(nil), f.ad.Texts...)
	keywords := append([]yandexdirect.Keyword(nil), f.keywords...)
	status := strings.ToUpper(strings.TrimSpace(f.campaign.Status))
	if status == "ACCEPTED" {
		group.Status, group.ServingStatus = "ACCEPTED", "ELIGIBLE"
		ad.Status, ad.State = "ACCEPTED", "ON"
		for index := range ad.Titles {
			ad.Titles[index].Status = "ACCEPTED"
		}
		for index := range ad.Texts {
			ad.Texts[index].Status = "ACCEPTED"
		}
		for index := range keywords {
			keywords[index].Status = "ACCEPTED"
			keywords[index].State = "ON"
			keywords[index].ServingStatus = "ELIGIBLE"
		}
	}
	return group, ad, keywords
}

func fakeModeratedTexts(
	values []string, status string,
) []yandexdirect.ModeratedText {
	result := make([]yandexdirect.ModeratedText, len(values))
	for index, value := range values {
		result[index] = yandexdirect.ModeratedText{Value: value, Status: status}
	}
	return result
}

func TestDirectOAuthInputValidationIsFlowSpecific(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"1234567", "0000000"} {
		if !validDirectOAuthCode(yandexdirect.OAuthFlowVerificationCode, code) {
			t.Fatalf("verification code %q was rejected", code)
		}
	}
	for _, code := range []string{
		"", "123456", "12345678", "123456a", " 1234567", "1234567 ", "１２３４５６７",
	} {
		if validDirectOAuthCode(yandexdirect.OAuthFlowVerificationCode, code) {
			t.Fatalf("invalid verification code %q was accepted", code)
		}
	}
	if !validDirectOAuthCode(yandexdirect.OAuthFlowCallback, "callback-code.with_symbols~") ||
		validDirectOAuthCode(yandexdirect.OAuthFlowCallback, "callback code") {
		t.Fatal("callback code validation is not bounded printable ASCII")
	}
	state, err := randomDirectToken(32)
	if err != nil {
		t.Fatal(err)
	}
	if !validDirectOAuthState(state) || validDirectOAuthState(state+"x") {
		t.Fatal("OAuth state validation did not require canonical 32-byte base64url")
	}
}

func TestDirectAutoLaunchStaysFailClosedWithoutVerifiedProviderGraph(t *testing.T) {
	t.Parallel()
	application := New(
		nil, nil, nil, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := application.ConfigureDirect(
		&fakeDirectProvider{}, []byte("0123456789abcdef0123456789abcdef"),
	); err != nil {
		t.Fatal(err)
	}
	if err := application.SetDirectFeatureFlags(
		true, true,
	); !errors.Is(err, ErrDirectGraphUnsupported) {
		t.Fatalf("feature flags error = %v, want graph unsupported", err)
	}
	if application.DirectWritesEnabled() || application.DirectAutoLaunchEnabled() {
		t.Fatalf(
			"writes=%v auto=%v, want both disabled without graph verification",
			application.DirectWritesEnabled(), application.DirectAutoLaunchEnabled(),
		)
	}
}

func TestDirectAutoLaunchAmbiguousSuccessIsReconciledWithoutSecondWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	application, storage, provider, owner, workspace, connection, clock :=
		newDirectAppFixture(t, ctx, true)
	campaign := createDirectAppCampaign(t, ctx, application, owner, workspace.ID, *clock)
	if _, err := application.SubmitDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
	); err != nil {
		t.Fatal(err)
	}
	provider.campaign.Status = "ACCEPTED"
	provider.campaign.State = "OFF"
	application.RunDirectAutoLaunchOnce(ctx, 10)
	campaign, err := storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil || campaign.Status != "accepted" || campaign.ProviderCampaignID == nil {
		t.Fatalf("accepted campaign before consent = %#v, %v", campaign, err)
	}
	if _, err := application.GrantDirectAutoLaunchConsent(
		ctx, owner, workspace.ID, campaign.ID, store.DirectConsentRequest{
			Confirmation:         "АВТОЗАПУСК",
			ExpectedVersion:      campaign.Version,
			ExpectedConnectionID: connection.ID,
			ExpectedAccountID:    connection.AccountID,
			ExpectedCampaignName: campaign.Name,
			ExpectedProviderID:   *campaign.ProviderCampaignID,
			ExpectedGraphHash:    campaign.ProviderGraphHash,
			ExpectedRevisionID:   campaign.ProviderRevisionID,
			WeeklyBudgetMinor:    campaign.WeeklyBudgetMinor,
			StartsAt:             campaign.StartsAt,
			EndsAt:               campaign.EndsAt,
		},
	); err != nil {
		t.Fatal(err)
	}
	// Direct applied Resume but the HTTP result was ambiguous. The local state
	// must stay reconciling and the worker must poll before doing anything else.
	provider.resume = func(provider *fakeDirectProvider) error {
		provider.campaign.State = "ON"
		return context.DeadlineExceeded
	}
	// Provider polling and the launch queue use separate leases, so one worker
	// cycle can observe acceptance and then persist the ambiguous launch.
	application.RunDirectAutoLaunchOnce(ctx, 10)
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if provider.resumeCalls != 1 || campaign.LaunchState != "reconciling" ||
		campaign.Status != "accepted" {
		t.Fatalf("ambiguous launch: calls=%d campaign=%#v", provider.resumeCalls, campaign)
	}

	// The emergency write kill-switch must not disable read-only recovery.
	if err := application.SetDirectFeatureFlags(false, false); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(time.Minute)
	application.RunDirectAutoLaunchOnce(ctx, 10)
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if provider.resumeCalls != 1 || campaign.Status != "active" ||
		campaign.LaunchState != "confirmed" || campaign.ProviderState != "ON" {
		t.Fatalf("reconciled launch: calls=%d campaign=%#v", provider.resumeCalls, campaign)
	}
}

func TestDirectReconciliationConfirmsProviderONAndRecordsSnapshotDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	application, storage, provider, owner, workspace, connection, clock :=
		newDirectAppFixture(t, ctx, true)
	campaign := createDirectAppCampaign(t, ctx, application, owner, workspace.ID, *clock)
	if _, err := application.SubmitDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
	); err != nil {
		t.Fatal(err)
	}
	provider.campaign.Status = "ACCEPTED"
	provider.campaign.State = "OFF"
	application.RunDirectAutoLaunchOnce(ctx, 10)
	campaign, err := storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.GrantDirectAutoLaunchConsent(
		ctx, owner, workspace.ID, campaign.ID, store.DirectConsentRequest{
			Confirmation:         "АВТОЗАПУСК",
			ExpectedVersion:      campaign.Version,
			ExpectedConnectionID: connection.ID,
			ExpectedAccountID:    connection.AccountID,
			ExpectedCampaignName: campaign.Name,
			ExpectedProviderID:   *campaign.ProviderCampaignID,
			ExpectedGraphHash:    campaign.ProviderGraphHash,
			ExpectedRevisionID:   campaign.ProviderRevisionID,
			WeeklyBudgetMinor:    campaign.WeeklyBudgetMinor,
			StartsAt:             campaign.StartsAt,
			EndsAt:               campaign.EndsAt,
		},
	); err != nil {
		t.Fatal(err)
	}
	provider.resume = func(provider *fakeDirectProvider) error {
		provider.campaign.State = "ON"
		return context.DeadlineExceeded
	}
	application.RunDirectAutoLaunchOnce(ctx, 10)
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil || campaign.LaunchState != "reconciling" {
		t.Fatalf("ambiguous launch was not retained: %#v, %v", campaign, err)
	}

	provider.campaign.Name = "Changed directly in Yandex after Resume"
	if err := application.SetDirectFeatureFlags(false, false); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(time.Minute)
	application.RunDirectAutoLaunchOnce(ctx, 10)
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if provider.resumeCalls != 1 || campaign.Status != "active" ||
		campaign.LaunchState != "confirmed" || campaign.ProviderState != "ON" ||
		campaign.LaunchFailureCode != "provider_snapshot_mismatch" {
		t.Fatalf("provider ON truth or snapshot warning was lost: calls=%d campaign=%#v",
			provider.resumeCalls, campaign)
	}
}

func TestDirectAutomationIsIsolatedFromCoreScheduler(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	application, storage, provider, owner, workspace, connection, clock :=
		newDirectAppFixture(t, ctx, true)
	campaign := createDirectAppCampaign(t, ctx, application, owner, workspace.ID, *clock)
	campaign, err := application.SubmitDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
	)
	if err != nil {
		t.Fatal(err)
	}
	provider.campaign.Status = "ACCEPTED"
	provider.campaign.State = "OFF"
	if err = application.syncDirectCampaignLifecycle(
		ctx, workspace.ID, campaign.ID,
	); err != nil {
		t.Fatal(err)
	}
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.GrantDirectAutoLaunchConsent(
		ctx, owner, workspace.ID, campaign.ID, store.DirectConsentRequest{
			Confirmation:         "АВТОЗАПУСК",
			ExpectedVersion:      campaign.Version,
			ExpectedConnectionID: connection.ID,
			ExpectedAccountID:    connection.AccountID,
			ExpectedCampaignName: campaign.Name,
			ExpectedProviderID:   *campaign.ProviderCampaignID,
			ExpectedGraphHash:    campaign.ProviderGraphHash,
			ExpectedRevisionID:   campaign.ProviderRevisionID,
			WeeklyBudgetMinor:    campaign.WeeklyBudgetMinor,
			StartsAt:             campaign.StartsAt,
			EndsAt:               campaign.EndsAt,
		},
	); err != nil {
		t.Fatal(err)
	}
	application.runSchedulerCycle(ctx, *clock)
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if provider.resumeCalls != 0 || campaign.Status != "accepted" {
		t.Fatalf("core scheduler executed Direct work: calls=%d campaign=%#v",
			provider.resumeCalls, campaign)
	}

	application.runDirectAutomationCycle(ctx)
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if provider.resumeCalls != 1 || campaign.Status != "active" ||
		campaign.LaunchState != "confirmed" {
		t.Fatalf("Direct worker did not execute its cycle: calls=%d campaign=%#v",
			provider.resumeCalls, campaign)
	}
}

func TestDirectAutomationCycleBoundsProviderContext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	application, _, provider, owner, workspace, _, clock :=
		newDirectAppFixture(t, ctx, false)
	campaign := createDirectAppCampaign(t, ctx, application, owner, workspace.ID, *clock)
	if _, err := application.SubmitDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
	); err != nil {
		t.Fatal(err)
	}
	provider.campaign.Status = "ACCEPTED"
	provider.campaign.State = "OFF"
	var observedDeadline time.Time
	provider.getContext = func(providerCtx context.Context) {
		observedDeadline, _ = providerCtx.Deadline()
	}

	startedAt := time.Now()
	application.runDirectAutomationCycle(ctx)
	if observedDeadline.IsZero() {
		t.Fatal("Direct provider call did not receive a deadline")
	}
	if maximum := startedAt.Add(directAutomationCycleTimeout + time.Second); observedDeadline.After(maximum) {
		t.Fatalf("Direct cycle deadline = %v, want no later than %v", observedDeadline, maximum)
	}
}

func TestDirectAutoLaunchInvalidatesConsentOnProviderStrategyDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	application, storage, provider, owner, workspace, connection, clock :=
		newDirectAppFixture(t, ctx, true)
	campaign := createDirectAppCampaign(t, ctx, application, owner, workspace.ID, *clock)
	if _, err := application.SubmitDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
	); err != nil {
		t.Fatal(err)
	}
	provider.campaign.Status = "ACCEPTED"
	provider.campaign.State = "OFF"
	application.RunDirectAutoLaunchOnce(ctx, 10)
	campaign, err := storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.GrantDirectAutoLaunchConsent(
		ctx, owner, workspace.ID, campaign.ID, store.DirectConsentRequest{
			Confirmation: "АВТОЗАПУСК", ExpectedVersion: campaign.Version,
			ExpectedConnectionID: connection.ID, ExpectedAccountID: connection.AccountID,
			ExpectedCampaignName: campaign.Name, ExpectedProviderID: *campaign.ProviderCampaignID,
			ExpectedGraphHash:  campaign.ProviderGraphHash,
			ExpectedRevisionID: campaign.ProviderRevisionID,
			WeeklyBudgetMinor:  campaign.WeeklyBudgetMinor,
			StartsAt:           campaign.StartsAt, EndsAt: campaign.EndsAt,
		},
	); err != nil {
		t.Fatal(err)
	}
	provider.getErr = &yandexdirect.Error{Code: "campaign_budget_unavailable"}
	application.RunDirectAutoLaunchOnce(ctx, 10)
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if provider.resumeCalls != 0 || campaign.AutoLaunch.Valid ||
		campaign.AutoLaunch.InvalidReason != "provider_strategy_changed" {
		t.Fatalf("strategy drift did not invalidate auto consent: calls=%d campaign=%#v",
			provider.resumeCalls, campaign)
	}
}

func TestDirectAutoLaunchInvalidatesConsentOnProviderSnapshotDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	application, storage, provider, owner, workspace, connection, clock :=
		newDirectAppFixture(t, ctx, true)
	campaign := createDirectAppCampaign(t, ctx, application, owner, workspace.ID, *clock)
	if _, err := application.SubmitDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
	); err != nil {
		t.Fatal(err)
	}
	provider.campaign.Status = "ACCEPTED"
	provider.campaign.State = "OFF"
	application.RunDirectAutoLaunchOnce(ctx, 10)
	campaign, err := storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.GrantDirectAutoLaunchConsent(
		ctx, owner, workspace.ID, campaign.ID, store.DirectConsentRequest{
			Confirmation:         "АВТОЗАПУСК",
			ExpectedVersion:      campaign.Version,
			ExpectedConnectionID: connection.ID,
			ExpectedAccountID:    connection.AccountID,
			ExpectedCampaignName: campaign.Name,
			ExpectedProviderID:   *campaign.ProviderCampaignID,
			ExpectedGraphHash:    campaign.ProviderGraphHash,
			ExpectedRevisionID:   campaign.ProviderRevisionID,
			WeeklyBudgetMinor:    campaign.WeeklyBudgetMinor,
			StartsAt:             campaign.StartsAt,
			EndsAt:               campaign.EndsAt,
		},
	); err != nil {
		t.Fatal(err)
	}

	provider.campaign.Name = "Changed directly in Yandex"
	application.RunDirectAutoLaunchOnce(ctx, 10)
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if provider.resumeCalls != 0 || campaign.LaunchFailureCode != "provider_snapshot_mismatch" ||
		campaign.AutoLaunch.Valid ||
		campaign.AutoLaunch.InvalidReason != "provider_snapshot_changed" {
		t.Fatalf("provider snapshot drift was not made explicit: calls=%d campaign=%#v",
			provider.resumeCalls, campaign)
	}
}

func TestDirectManualLaunchNeedsNeitherAutoFlagNorConsentAndIsWorkspaceScoped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	application, storage, provider, owner, workspace, _, clock :=
		newDirectAppFixture(t, ctx, false)
	campaign := createDirectAppCampaign(t, ctx, application, owner, workspace.ID, *clock)
	campaign, err := application.SubmitDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
	)
	if err != nil {
		t.Fatal(err)
	}
	if campaign.Status != "moderation" {
		t.Fatalf("Campaigns.add result status = %q, want moderation", campaign.Status)
	}
	provider.campaign.Status = "ACCEPTED"
	provider.campaign.State = "OFF"
	otherWorkspace, err := storage.CreateWorkspace(ctx, owner, store.Workspace{Name: "Other team"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.LaunchDirectCampaign(
		ctx, owner, otherWorkspace.ID, campaign.ID, campaign.Version,
		campaign.ProviderGraphHash, campaign.ProviderRevisionID,
	); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-workspace launch error = %v, want ErrNotFound", err)
	}
	campaign, err = application.LaunchDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
		campaign.ProviderGraphHash, campaign.ProviderRevisionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if provider.resumeCalls != 1 || campaign.Status != "active" ||
		campaign.LaunchMode != "manual" {
		t.Fatalf("manual launch: calls=%d campaign=%#v", provider.resumeCalls, campaign)
	}
}

func TestDirectUnauthorizedResumeMarksConnectionErrorAndReleasesClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	application, storage, provider, owner, workspace, _, clock :=
		newDirectAppFixture(t, ctx, false)
	campaign := createDirectAppCampaign(t, ctx, application, owner, workspace.ID, *clock)
	campaign, err := application.SubmitDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
	)
	if err != nil {
		t.Fatal(err)
	}
	provider.campaign.Status = "ACCEPTED"
	provider.campaign.State = "OFF"
	provider.resume = func(*fakeDirectProvider) error {
		return &yandexdirect.Error{
			StatusCode: http.StatusUnauthorized,
			Code:       "authorization_required",
		}
	}
	if _, err := application.LaunchDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
		campaign.ProviderGraphHash, campaign.ProviderRevisionID,
	); !errors.Is(err, ErrDirectProvider) {
		t.Fatalf("unauthorized launch error = %v, want ErrDirectProvider", err)
	}
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if provider.resumeCalls != 1 || campaign.Status != "accepted" ||
		campaign.LaunchState != "idle" ||
		campaign.LaunchFailureCode != "authorization_required" {
		t.Fatalf("unauthorized Resume left an ambiguous launch: calls=%d campaign=%#v",
			provider.resumeCalls, campaign)
	}
	connection, err := storage.GetDirectConnection(ctx, owner, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if connection.Status != "error" || connection.ErrorCode != "authorization_required" {
		t.Fatalf("expired connection = %#v", connection)
	}
	if err := application.RevokeDirectConnection(ctx, owner, workspace.ID); err != nil {
		t.Fatalf("explicit disconnect after authoritative 401: %v", err)
	}
}

func TestDirectProviderAuthorizationErrorRequiresAuthoritativeProviderSignal(t *testing.T) {
	t.Parallel()
	if !directProviderAuthorizationError(fmt.Errorf(
		"wrapped: %w", &yandexdirect.Error{StatusCode: http.StatusUnauthorized},
	)) {
		t.Fatal("wrapped HTTP 401 was not recognized")
	}
	if !directProviderAuthorizationError(&yandexdirect.Error{APIErrorCode: 53}) {
		t.Fatal("Yandex Direct invalid-token error 53 was not recognized")
	}
	if directProviderAuthorizationError(
		&yandexdirect.Error{StatusCode: http.StatusForbidden, Code: "authorization_required"},
	) {
		t.Fatal("non-401 provider error was treated as an expired credential")
	}
	if directProviderAuthorizationError(context.DeadlineExceeded) {
		t.Fatal("ambiguous transport error was treated as an expired credential")
	}
}

func TestDirectTokenBundleEncryptionSupportsRotatingAndLegacyTokens(t *testing.T) {
	t.Parallel()
	block, err := aes.NewCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	tokenCipher := &directTokenCipher{aead: aead}
	now := time.Date(2043, time.January, 2, 3, 4, 5, 0, time.UTC)
	bundle, err := newDirectTokenBundle(now, yandexdirect.OAuthToken{
		AccessToken: "access-secret", RefreshToken: "refresh-secret",
		ExpiresInSeconds: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := tokenCipher.sealBundle(
		"workspace", "account", "login", bundle,
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ciphertext, "access-secret") ||
		strings.Contains(ciphertext, "refresh-secret") {
		t.Fatal("encrypted token bundle exposed plaintext")
	}
	opened, err := tokenCipher.openBundle(
		"workspace", "account", "login", ciphertext,
	)
	if err != nil {
		t.Fatal(err)
	}
	if opened.AccessToken != bundle.AccessToken ||
		opened.RefreshToken != bundle.RefreshToken ||
		!opened.ExpiresAt.Equal(bundle.ExpiresAt) || opened.Legacy {
		t.Fatalf("opened bundle = %#v", opened)
	}
	if _, err := tokenCipher.openBundle(
		"workspace", "different-account", "login", ciphertext,
	); err == nil {
		t.Fatal("token bundle opened with different authenticated metadata")
	}
	legacyCiphertext, err := tokenCipher.seal(
		"workspace", "account", "login", "legacy-access",
	)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := tokenCipher.openBundle(
		"workspace", "account", "login", legacyCiphertext,
	)
	if err != nil || !legacy.Legacy || legacy.AccessToken != "legacy-access" ||
		legacy.RefreshToken != "" {
		t.Fatalf("legacy bundle = %#v, %v", legacy, err)
	}
}

func TestDirectTokenRefreshScheduleIsBoundedAndEarly(t *testing.T) {
	t.Parallel()
	now := time.Date(2043, time.February, 3, 4, 5, 6, 0, time.UTC)
	short, err := newDirectTokenBundle(now, yandexdirect.OAuthToken{
		AccessToken: "access", RefreshToken: "refresh", ExpiresInSeconds: 600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(5 * time.Minute); !short.RefreshAfter.Equal(want) {
		t.Fatalf("short refresh_after = %v, want %v", short.RefreshAfter, want)
	}
	long, err := newDirectTokenBundle(now, yandexdirect.OAuthToken{
		AccessToken: "access", RefreshToken: "refresh",
		ExpiresInSeconds: math.MaxInt64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(directTokenRefreshInterval); !long.RefreshAfter.Equal(want) {
		t.Fatalf("long refresh_after = %v, want %v", long.RefreshAfter, want)
	}
	if want := now.Add(directTokenMaximumLifetime); !long.ExpiresAt.Equal(want) {
		t.Fatalf("bounded expires_at = %v, want %v", long.ExpiresAt, want)
	}
}

func TestDirectAccessTokenRefreshRotatesBundleAndTransientFailureFallsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	application, storage, provider, _, workspace, connection, clock :=
		newDirectAppFixture(t, ctx, false)
	*clock = clock.Add(24*time.Hour - 4*time.Minute)
	provider.refreshErr = context.DeadlineExceeded
	token, err := application.directAccessToken(ctx, connection)
	if err != nil || token != "access-token" {
		t.Fatalf("transient fallback token = %q, err = %v", token, err)
	}
	current, err := storage.GetDirectConnectionTokenMaterial(
		ctx, workspace.ID, connection.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if current.TokenRefreshClaimedAt != nil {
		t.Fatalf("transient refresh retained claim: %#v", current.TokenRefreshClaimedAt)
	}
	provider.refreshErr = nil
	token, err = application.directAccessToken(ctx, connection)
	if err != nil || token != "refreshed-token" {
		t.Fatalf("rotated token = %q, err = %v", token, err)
	}
	current, err = storage.GetDirectConnectionTokenMaterial(ctx, workspace.ID, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := application.directCipher.openBundle(
		current.WorkspaceID, current.AccountID, current.ClientLogin,
		current.TokenCiphertext,
	)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.AccessToken != "refreshed-token" ||
		rotated.RefreshToken != "rotated-refresh-token" ||
		provider.refreshCalls != 2 {
		t.Fatalf("rotated bundle = %#v, calls = %d", rotated, provider.refreshCalls)
	}
}

func TestDirectRefreshInvalidGrantRequiresReauthorization(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	application, storage, provider, _, workspace, connection, clock :=
		newDirectAppFixture(t, ctx, false)
	*clock = clock.Add(24*time.Hour - 4*time.Minute)
	provider.refreshErr = &yandexdirect.Error{
		StatusCode: http.StatusBadRequest, Code: "invalid_grant",
	}
	if _, err := application.directAccessToken(
		ctx, connection,
	); !errors.Is(err, store.ErrDirectConnectionRequired) {
		t.Fatalf("invalid_grant error = %v", err)
	}
	current, err := storage.GetDirectConnectionTokenMaterial(
		ctx, workspace.ID, connection.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "error" ||
		current.ErrorCode != "authorization_required" ||
		current.TokenRefreshClaimedAt != nil ||
		current.TokenCiphertext != connection.TokenCiphertext {
		t.Fatalf("connection after invalid_grant = %#v", current)
	}
}

func newDirectAppFixture(
	t *testing.T, ctx context.Context, autoLaunch bool,
) (*App, *store.Store, *fakeDirectProvider, string, store.Workspace, store.DirectConnection, *time.Time) {
	t.Helper()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "direct-app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	owner := "direct-app-owner"
	if err := storage.UpsertUser(ctx, store.User{ID: owner, DisplayName: owner}); err != nil {
		t.Fatal(err)
	}
	var workspace store.Workspace
	workspaces, err := storage.ListWorkspaces(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	for _, access := range workspaces {
		if access.Workspace.IsPersonal {
			workspace = access.Workspace
			break
		}
	}
	if workspace.ID == "" {
		t.Fatal("personal workspace is missing")
	}
	provider := &fakeDirectProvider{
		graphSupported: true,
		account: yandexdirect.Account{
			ID: "account-123", Login: "direct-login", DisplayName: "Direct account",
			CurrencyCode: "RUB", Timezone: "Europe/Moscow",
		},
		campaign: yandexdirect.Campaign{ID: 98_001},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application := New(storage, nil, nil, nil, nil, logger)
	if err := application.ConfigureDirect(provider, []byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	if err := application.SetDirectFeatureFlags(true, autoLaunch); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2043, time.June, 7, 12, 0, 0, 0, time.UTC)
	application.now = func() time.Time { return now }
	bundle, err := newDirectTokenBundle(now, yandexdirect.OAuthToken{
		AccessToken: "access-token", RefreshToken: "refresh-token",
		ExpiresInSeconds: int64((24 * time.Hour) / time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := application.directCipher.sealBundle(
		workspace.ID, provider.account.ID, provider.account.Login, bundle,
	)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := storage.ReplaceDirectConnection(ctx, owner, workspace.ID, store.DirectConnection{
		AccountID: provider.account.ID, ClientLogin: provider.account.Login,
		AccountName: provider.account.DisplayName, CurrencyCode: provider.account.CurrencyCode,
		Timezone: provider.account.Timezone, TokenCiphertext: ciphertext,
		TokenKeyVersion: 1, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return application, storage, provider, owner, workspace, connection, &now
}

func createDirectAppCampaign(
	t *testing.T, ctx context.Context, application *App, owner, workspaceID string, now time.Time,
) store.DirectCampaign {
	t.Helper()
	campaign, err := application.CreateDirectCampaign(ctx, owner, workspaceID, store.DirectCampaign{
		Name: "Provider truth campaign", Objective: "traffic",
		LandingURL: "https://maxposty.ru/", Brief: "Promote the channel",
		Regions:           []string{"225"},
		Titles:            []string{"Ведение канала MAX без рутины"},
		Texts:             []string{"Планируйте публикации и развивайте канал с MaxPosty"},
		Keywords:          []string{"ведение канала max"},
		NegativeKeywords:  []string{"бесплатно"},
		WeeklyBudgetMinor: 30_000,
		StartsAt:          now, EndsAt: now.AddDate(0, 1, 0), CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return campaign
}
