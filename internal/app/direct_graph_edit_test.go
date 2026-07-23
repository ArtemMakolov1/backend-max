package app

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexdirect"
)

type directProviderEditFake struct {
	DirectGraphProvider
	graph               yandexdirect.CampaignGraph
	campaignUpdateCalls int
	adGroupUpdateCalls  int
	adUpdateCalls       int
}

func (f *directProviderEditFake) GetCampaignGraph(
	context.Context, string, string, int64,
) (yandexdirect.CampaignGraph, error) {
	return f.graph, nil
}

func (f *directProviderEditFake) UpdateUnifiedCampaigns(
	context.Context, string, string, []yandexdirect.UnifiedCampaignUpdate,
) ([]yandexdirect.MutationResult, error) {
	f.campaignUpdateCalls++
	return nil, nil
}

func (f *directProviderEditFake) UpdateUnifiedAdGroups(
	context.Context, string, string, []yandexdirect.UnifiedAdGroupUpdate,
) ([]yandexdirect.MutationResult, error) {
	f.adGroupUpdateCalls++
	return nil, nil
}

func (f *directProviderEditFake) UpdateResponsiveAds(
	context.Context, string, string, []yandexdirect.ResponsiveAdUpdate,
) ([]yandexdirect.MutationResult, error) {
	f.adUpdateCalls++
	return nil, nil
}

type directProviderEditFixture struct {
	material store.DirectGraphSubmissionMaterial
	baseline directProviderEditBaseline
	base     yandexdirect.CampaignGraph
}

func newDirectProviderEditFixture(t *testing.T) directProviderEditFixture {
	t.Helper()
	const (
		campaignID = int64(101)
		adGroupID  = int64(201)
		adID       = int64(301)
		baseMarker = "base_marker"
		editMarker = "edit_marker"
	)
	startsAt := time.Date(2045, 1, 10, 0, 0, 0, 0, time.UTC)
	endsAt := startsAt.AddDate(0, 1, 0)
	strategy, err := yandexdirect.SafeUnifiedCampaignBiddingStrategy(30_000)
	if err != nil {
		t.Fatal(err)
	}
	base := yandexdirect.CampaignGraph{
		Campaign: yandexdirect.GraphCampaign{
			ID: campaignID, Name: "Базовая кампания",
			Status: "DRAFT", State: "OFF", Type: "UNIFIED_CAMPAIGN",
			WeeklyBudgetMinor: 30_000, StartsAt: startsAt, EndsAt: endsAt,
			TimeZone:                    "Europe/Moscow",
			NegativeKeywords:            []string{},
			NegativeKeywordSharedSetIDs: []int64{},
			BlockedIPs:                  []string{},
			ExcludedSites:               []string{},
			TimeTargeting:               yandexdirect.SafeUnifiedCampaignTimeTargeting(),
			BiddingStrategy:             strategy,
			Settings:                    yandexdirect.SafeUnifiedCampaignSettings(),
			CounterIDs:                  []int64{},
			PriorityGoals:               []yandexdirect.GraphPriorityGoal{},
			TrackingParams: (url.Values{
				"mp_op": []string{baseMarker},
			}).Encode(),
			AttributionModel: "AUTO",
		},
		AdGroups: []yandexdirect.UnifiedAdGroup{{
			ID: adGroupID, CampaignID: campaignID,
			Name:             yandexdirect.UnifiedAdGroupOperationName(baseMarker),
			RegionIDs:        []int64{213},
			NegativeKeywords: []string{"бесплатно"},
			OfferRetargeting: "NO", Status: "ACCEPTED",
			ServingStatus: "ELIGIBLE",
		}},
		Ads: []yandexdirect.ResponsiveAd{{
			ID: adID, CampaignID: campaignID, AdGroupID: adGroupID,
			Titles: []yandexdirect.ModeratedText{{
				Value: "Старая реклама", Status: "ACCEPTED",
			}},
			Texts: []yandexdirect.ModeratedText{{
				Value: "Старое описание", Status: "ACCEPTED",
			}},
			Href: "https://maxposty.ru/old", Status: "ACCEPTED", State: "OFF",
		}},
		Keywords: []yandexdirect.Keyword{{
			ID: 401, CampaignID: campaignID, AdGroupID: adGroupID,
			Keyword: "старый запрос", StrategyPriority: "NORMAL",
			Status: "ACCEPTED", State: "OFF", ServingStatus: "ELIGIBLE",
		}, {
			ID: 402, CampaignID: campaignID, AdGroupID: adGroupID,
			Keyword: "ведение канала", StrategyPriority: "NORMAL",
			Status: "ACCEPTED", State: "OFF", ServingStatus: "ELIGIBLE",
		}},
	}
	hash, err := base.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	campaign := store.DirectCampaign{
		ID: "campaign_1", WorkspaceID: "workspace_1",
		ConnectionID: "connection_1", ProviderCampaignID: editInt64(campaignID),
		ProviderAdGroupID: editInt64(adGroupID), ProviderAdID: editInt64(adID),
		Name: "Базовая кампания", LandingURL: "https://maxposty.ru/old",
		Regions: []string{"Москва"}, Titles: []string{"Старая реклама"},
		Texts:             []string{"Старое описание"},
		Keywords:          []string{"старый запрос", "ведение канала"},
		NegativeKeywords:  []string{"бесплатно"},
		WeeklyBudgetMinor: 30_000, CurrencyCode: "RUB",
		StartsAt: startsAt, EndsAt: endsAt, Version: 7, ProviderState: "OFF",
	}
	desired := campaign
	desired.Name = "Новая кампания"
	desired.LandingURL = "https://maxposty.ru/new"
	desired.Regions = []string{"Санкт-Петербург"}
	desired.Titles = []string{"Новая реклама"}
	desired.Texts = []string{"Новое описание"}
	desired.Keywords = []string{"новый запрос", "ведение сообщества"}
	desired.NegativeKeywords = []string{"дорого"}
	desired.WeeklyBudgetMinor = 35_000
	mappings := []store.DirectKeywordMapping{{
		Keyword: "старый запрос", ProviderKeywordID: 401,
	}, {
		Keyword: "ведение канала", ProviderKeywordID: 402,
	}}
	material := store.DirectGraphSubmissionMaterial{
		Campaign: campaign, DesiredCampaign: desired,
		Connection: store.DirectConnection{
			ID: "connection_1", Timezone: "Europe/Moscow",
		},
		Operation: store.DirectProviderOperation{
			ID: "operation_1", ConnectionID: "connection_1",
			OperationKind: "update", OperationMarker: editMarker,
			ExpectedCampaignVersion: 7, ExpectedGraphHash: hash,
			ExpectedRevisionID: "revision_1", Stage: "claimed",
			ProviderCampaignID: editInt64(campaignID),
			ProviderAdGroupID:  editInt64(adGroupID),
			ProviderAdID:       editInt64(adID), ProviderKeywordMappings: mappings,
		},
	}
	return directProviderEditFixture{
		material: material,
		baseline: directProviderEditBaseline{
			Graph: base, GraphHash: hash, BaseCampaign: campaign,
			BaseRegionIDs: []int64{213}, DesiredRegionIDs: []int64{2},
		},
		base: base,
	}
}

func editInt64(value int64) *int64 {
	return &value
}

func TestDirectProviderEditRejectsGroupDriftBeforeCampaignMutation(t *testing.T) {
	t.Parallel()
	fixture := newDirectProviderEditFixture(t)
	graph := fixture.base
	graph.AdGroups = append([]yandexdirect.UnifiedAdGroup(nil), graph.AdGroups...)
	graph.AdGroups[0].Name = "Внешнее изменение"
	provider := &directProviderEditFake{graph: graph}
	app := &App{directGraph: provider}

	_, err := app.directUpdateOrReconcileCampaign(
		context.Background(), "token", fixture.material, fixture.baseline,
	)
	if !errors.Is(err, ErrDirectSnapshotMismatch) {
		t.Fatalf("group drift error = %v, want snapshot mismatch", err)
	}
	if provider.campaignUpdateCalls != 0 {
		t.Fatalf("campaign provider mutations = %d, want 0", provider.campaignUpdateCalls)
	}
}

func TestDirectProviderEditRejectsAdDriftBeforeAdGroupMutation(t *testing.T) {
	t.Parallel()
	fixture := newDirectProviderEditFixture(t)
	fixture.material.Operation.Stage = "campaign_updated"
	graph := directProviderEditDesiredCampaign(fixture)
	graph.Ads = append([]yandexdirect.ResponsiveAd(nil), graph.Ads...)
	graph.Ads[0].Href = "https://external.example/drift"
	provider := &directProviderEditFake{graph: graph}
	app := &App{directGraph: provider}

	_, err := app.directUpdateOrReconcileAdGroup(
		context.Background(), "token", fixture.material, fixture.baseline,
	)
	if !errors.Is(err, ErrDirectSnapshotMismatch) {
		t.Fatalf("ad drift error = %v, want snapshot mismatch", err)
	}
	if provider.adGroupUpdateCalls != 0 {
		t.Fatalf("ad-group provider mutations = %d, want 0", provider.adGroupUpdateCalls)
	}
}

func TestDirectProviderEditRejectsKeywordDriftBeforeAdMutation(t *testing.T) {
	t.Parallel()
	fixture := newDirectProviderEditFixture(t)
	fixture.material.Operation.Stage = "ad_group_updated"
	graph := directProviderEditDesiredAdGroup(
		directProviderEditDesiredCampaign(fixture), fixture,
	)
	graph.Keywords = append([]yandexdirect.Keyword(nil), graph.Keywords...)
	graph.Keywords[0].Keyword = "внешний запрос"
	provider := &directProviderEditFake{graph: graph}
	app := &App{directGraph: provider}

	_, err := app.directUpdateOrReconcileAd(
		context.Background(), "token", fixture.material, fixture.baseline,
	)
	if !errors.Is(err, ErrDirectSnapshotMismatch) {
		t.Fatalf("keyword drift error = %v, want snapshot mismatch", err)
	}
	if provider.adUpdateCalls != 0 {
		t.Fatalf("ad provider mutations = %d, want 0", provider.adUpdateCalls)
	}
}

func TestDirectProviderEditRecoversProviderWriteBeforeDatabaseAdvance(t *testing.T) {
	t.Parallel()
	fixture := newDirectProviderEditFixture(t)
	campaignDesired := directProviderEditDesiredCampaign(fixture)
	groupDesired := directProviderEditDesiredAdGroup(campaignDesired, fixture)
	adDesired := directProviderEditDesiredAd(groupDesired, fixture)
	keywordsDesired := directProviderEditDesiredKeywords(adDesired, fixture)
	tests := []struct {
		stage string
		graph yandexdirect.CampaignGraph
		want  func(directProviderEditPrefixState) bool
	}{
		{"claimed", campaignDesired, func(s directProviderEditPrefixState) bool {
			return s.CampaignDesired
		}},
		{"campaign_updated", groupDesired, func(s directProviderEditPrefixState) bool {
			return s.AdGroupDesired
		}},
		{"ad_group_updated", adDesired, func(s directProviderEditPrefixState) bool {
			return s.AdDesired
		}},
		{"ad_updated", keywordsDesired, func(s directProviderEditPrefixState) bool {
			return s.KeywordsDesired
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.stage, func(t *testing.T) {
			material := fixture.material
			material.Operation.Stage = test.stage
			state, err := directVerifyProviderEditPrefix(
				test.graph, material, fixture.baseline, test.stage,
			)
			if err != nil {
				t.Fatalf("recovered %s write rejected: %v", test.stage, err)
			}
			if !test.want(state) {
				t.Fatalf("recovered %s write was not recognized", test.stage)
			}
		})
	}
}

func directProviderEditDesiredCampaign(
	fixture directProviderEditFixture,
) yandexdirect.CampaignGraph {
	graph := fixture.base
	strategy, _ := yandexdirect.SafeUnifiedCampaignBiddingStrategy(
		fixture.material.DesiredCampaign.WeeklyBudgetMinor,
	)
	graph.Campaign.Name = fixture.material.DesiredCampaign.Name
	graph.Campaign.WeeklyBudgetMinor = fixture.material.DesiredCampaign.WeeklyBudgetMinor
	graph.Campaign.BiddingStrategy = strategy
	graph.Campaign.TrackingParams = (url.Values{
		"mp_op": []string{fixture.material.Operation.OperationMarker},
	}).Encode()
	return graph
}

func directProviderEditDesiredAdGroup(
	graph yandexdirect.CampaignGraph, fixture directProviderEditFixture,
) yandexdirect.CampaignGraph {
	graph.AdGroups = append([]yandexdirect.UnifiedAdGroup(nil), graph.AdGroups...)
	graph.AdGroups[0].Name = yandexdirect.UnifiedAdGroupOperationName(
		fixture.material.Operation.OperationMarker,
	)
	graph.AdGroups[0].RegionIDs = append(
		[]int64(nil), fixture.baseline.DesiredRegionIDs...,
	)
	graph.AdGroups[0].NegativeKeywords = append(
		[]string(nil), fixture.material.DesiredCampaign.NegativeKeywords...,
	)
	return graph
}

func directProviderEditDesiredAd(
	graph yandexdirect.CampaignGraph, fixture directProviderEditFixture,
) yandexdirect.CampaignGraph {
	graph.Ads = append([]yandexdirect.ResponsiveAd(nil), graph.Ads...)
	graph.Ads[0].Titles = []yandexdirect.ModeratedText{{
		Value: fixture.material.DesiredCampaign.Titles[0],
	}}
	graph.Ads[0].Texts = []yandexdirect.ModeratedText{{
		Value: fixture.material.DesiredCampaign.Texts[0],
	}}
	graph.Ads[0].Href = fixture.material.DesiredCampaign.LandingURL
	return graph
}

func directProviderEditDesiredKeywords(
	graph yandexdirect.CampaignGraph, fixture directProviderEditFixture,
) yandexdirect.CampaignGraph {
	graph.Keywords = append([]yandexdirect.Keyword(nil), graph.Keywords...)
	for index := range graph.Keywords {
		graph.Keywords[index].Keyword = fixture.material.DesiredCampaign.Keywords[index]
	}
	return graph
}
