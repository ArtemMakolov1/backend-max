package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexdirect"
)

func TestDirectGraphMismatchMapsToSnapshotConflict(t *testing.T) {
	t.Parallel()
	if !errors.Is(errDirectProviderGraphMismatch, ErrDirectSnapshotMismatch) {
		t.Fatalf("mismatch error does not wrap ErrDirectSnapshotMismatch: %v",
			errDirectProviderGraphMismatch)
	}
}

func TestDirectGraphWorkflowDeadlinePrecedesRecoveryLease(t *testing.T) {
	t.Parallel()
	if directGraphWorkflowTimeout >= store.DirectProviderOperationLease {
		t.Fatalf(
			"workflow timeout %s must be shorter than provider lease %s",
			directGraphWorkflowTimeout, store.DirectProviderOperationLease,
		)
	}
	if margin := store.DirectProviderOperationLease - directGraphWorkflowTimeout; margin < time.Minute {
		t.Fatalf("workflow recovery safety margin = %s, want at least 1m", margin)
	}
}

func TestDirectSafeBiddingStrategyPinsEverySearchPlacementOff(t *testing.T) {
	t.Parallel()
	strategy, err := yandexdirect.SafeUnifiedCampaignBiddingStrategy(30_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateDirectSafeBiddingStrategy(strategy, 30_000); err != nil {
		t.Fatalf("safe strategy rejected: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(strategy, &decoded); err != nil {
		t.Fatal(err)
	}
	search := decoded["Search"].(map[string]any)
	placements := search["PlacementTypes"].(map[string]any)
	placements["SearchResults"] = "YES"
	unsafe, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateDirectSafeBiddingStrategy(unsafe, 30_000); err == nil {
		t.Fatal("search placement drift was accepted")
	}
}

func TestDirectKeywordEditPlanRejectsUnknownExternalPhrase(t *testing.T) {
	t.Parallel()
	current := []yandexdirect.Keyword{{
		ID: 1, CampaignID: 7, AdGroupID: 8, Keyword: "old",
		StrategyPriority: "NORMAL",
	}, {
		ID: 2, CampaignID: 7, AdGroupID: 8, Keyword: "external",
		StrategyPriority: "NORMAL",
	}}
	_, _, _, err := directKeywordEditPlan(
		current, []string{"old"}, []string{"new"}, 7, 8,
	)
	if !errors.Is(err, ErrDirectSnapshotMismatch) {
		t.Fatalf("unknown external phrase error = %v", err)
	}
}

func TestDirectCampaignNodeRejectsProviderOKWithoutDesiredUpdate(t *testing.T) {
	t.Parallel()
	start := time.Date(2044, 1, 1, 0, 0, 0, 0, time.UTC)
	strategy, err := yandexdirect.SafeUnifiedCampaignBiddingStrategy(30_000)
	if err != nil {
		t.Fatal(err)
	}
	connection := store.DirectConnection{Timezone: "Europe/Moscow"}
	desired := store.DirectCampaign{
		Name: "new name", WeeklyBudgetMinor: 30_000,
		StartsAt: start, EndsAt: start.AddDate(0, 1, 0),
	}
	provider := yandexdirect.GraphCampaign{
		ID: 1, Name: "old name", Type: "UNIFIED_CAMPAIGN",
		State: "OFF", WeeklyBudgetMinor: 30_000,
		StartsAt: start, EndsAt: start.AddDate(0, 1, 0),
		TimeZone:        "Europe/Moscow",
		TimeTargeting:   yandexdirect.SafeUnifiedCampaignTimeTargeting(),
		BiddingStrategy: strategy,
		Settings:        yandexdirect.SafeUnifiedCampaignSettings(),
		TrackingParams:  "mp_op=operation_marker", AttributionModel: "AUTO",
	}
	if directCampaignNodeMatches(provider, desired, connection, "operation_marker") {
		t.Fatal("unchanged provider campaign was treated as the desired update")
	}
}

func TestDirectGraphDeliveryReadinessRejectsSuspendedChild(t *testing.T) {
	t.Parallel()
	graph := yandexdirect.CampaignGraph{
		Campaign: yandexdirect.GraphCampaign{Status: "ACCEPTED", State: "OFF"},
		AdGroups: []yandexdirect.UnifiedAdGroup{{
			ID: 1, ServingStatus: "ELIGIBLE",
		}},
		Ads: []yandexdirect.ResponsiveAd{{
			ID: 2, State: "ON", Status: "ACCEPTED",
		}},
		Keywords: []yandexdirect.Keyword{{
			ID: 3, State: "ON", Status: "ACCEPTED",
			ServingStatus: "ELIGIBLE",
		}},
	}
	if !directGraphDeliveryReady(graph) {
		t.Fatal("eligible accepted graph was rejected")
	}
	graph.Keywords[0].State = "OFF"
	if directGraphDeliveryReady(graph) {
		t.Fatal("suspended keyword was accepted for launch")
	}
}

func TestDirectGraphModerationIncludesEveryResponsiveTextPart(t *testing.T) {
	t.Parallel()
	accepted := store.DirectModerationSnapshot{Status: "ACCEPTED"}
	graph := yandexdirect.CampaignGraph{
		Ads: []yandexdirect.ResponsiveAd{{
			Titles: []yandexdirect.ModeratedText{{
				Value: "accepted title", Status: "ACCEPTED",
			}, {
				Value: "rejected title", Status: "REJECTED",
			}},
			Texts: []yandexdirect.ModeratedText{{
				Value: "accepted text", Status: "ACCEPTED",
			}},
		}},
		Keywords: []yandexdirect.Keyword{{Status: "ACCEPTED"}},
	}
	status, _ := directGraphModerationStatus(
		graph, accepted, accepted, accepted,
	)
	if status != "REJECTED" {
		t.Fatalf("complete responsive moderation status = %q, want REJECTED", status)
	}
}

func TestDirectCampaignFromImmutableDesiredGraphIgnoresClaimMaterial(t *testing.T) {
	t.Parallel()
	start := time.Date(2046, 1, 2, 0, 0, 0, 0, time.UTC)
	base := store.DirectCampaignDesiredGraph{
		Name: "Base campaign", LandingURL: "https://maxposty.ru/base",
		Regions: []string{"Moscow"}, WeeklyBudgetMinor: 30_000,
		CurrencyCode: "RUB", StartsAt: start,
		EndsAt: start.AddDate(0, 1, 0), Titles: []string{"Base title"},
		Texts: []string{"Base text"}, Keywords: []string{"base keyword"},
		NegativeKeywords: []string{"negative"},
	}
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	claimed := store.DirectCampaign{
		Name: "Desired campaign", LandingURL: "https://maxposty.ru/desired",
		Regions: []string{"Saint Petersburg"}, Titles: []string{"Desired title"},
		Texts: []string{"Desired text"}, Keywords: []string{"desired keyword"},
	}
	restored, err := directCampaignFromImmutableDesiredGraph(claimed, raw)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Name != base.Name ||
		restored.LandingURL != base.LandingURL ||
		!reflect.DeepEqual(restored.Regions, base.Regions) ||
		!reflect.DeepEqual(restored.Titles, base.Titles) ||
		!reflect.DeepEqual(restored.Texts, base.Texts) ||
		!reflect.DeepEqual(restored.Keywords, base.Keywords) {
		t.Fatalf("immutable base campaign = %#v", restored)
	}
}

func TestLoadDirectProviderEditBaselineUsesImmutableRevision(
	t *testing.T,
) {
	t.Parallel()
	ctx := context.Background()
	application, storage, _, owner, workspace, _, clock :=
		newDirectAppFixture(t, ctx, false)
	campaign := createDirectAppCampaign(
		t, ctx, application, owner, workspace.ID, *clock,
	)
	campaign, err := application.SubmitDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
	)
	if err != nil {
		t.Fatal(err)
	}
	baseName := campaign.Name
	baseTexts := append([]string(nil), campaign.Texts...)
	baseRegions := append([]string(nil), campaign.Regions...)
	desiredName := "Desired name after claim"
	desiredTexts := []string{"Desired text after claim"}
	desiredRegions := []string{"Desired region after claim"}
	material, err := storage.ClaimDirectCampaignProviderEdit(
		ctx, owner, workspace.ID, campaign.ID,
		store.DirectCampaignChanges{
			Name: &desiredName, Texts: &desiredTexts, Regions: &desiredRegions,
			ExpectedVersion: campaign.Version,
		},
		campaign.ProviderGraphHash, campaign.ProviderRevisionID,
		"baseline_load_"+campaign.ID, clock.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce a claim material implementation that exposes desired local
	// fields. Baseline loading must still be anchored to the immutable revision.
	material.Campaign = material.DesiredCampaign
	baseline, err := application.loadDirectProviderEditBaseline(
		ctx, "access-token", material, []int64{225},
	)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.BaseCampaign.Name != baseName ||
		!reflect.DeepEqual(baseline.BaseCampaign.Texts, baseTexts) ||
		!reflect.DeepEqual(baseline.BaseCampaign.Regions, baseRegions) ||
		baseline.BaseCampaign.Name == desiredName ||
		reflect.DeepEqual(baseline.BaseCampaign.Texts, desiredTexts) ||
		reflect.DeepEqual(baseline.BaseCampaign.Regions, desiredRegions) ||
		baseline.GraphHash != campaign.ProviderGraphHash {
		t.Fatalf("loaded immutable baseline = %#v", baseline.BaseCampaign)
	}
}

func TestDirectProviderEditPrefixAllowsOnlyOwnedKeywordTransition(t *testing.T) {
	t.Parallel()
	start := time.Date(2046, 2, 3, 0, 0, 0, 0, time.UTC)
	baseCampaign := store.DirectCampaign{
		Name: "Safe edit", LandingURL: "https://maxposty.ru/",
		Regions: []string{"225"}, Titles: []string{"Safe title"},
		Texts: []string{"Safe text"}, Keywords: []string{"old one", "old two"},
		NegativeKeywords: []string{}, WeeklyBudgetMinor: 30_000,
		StartsAt: start, EndsAt: start.AddDate(0, 1, 0),
	}
	desired := baseCampaign
	desired.Keywords = []string{"new one", "new two"}
	oldMarker, newMarker := "baseline_marker_1234", "edit_marker_12345678"
	strategy, err := yandexdirect.SafeUnifiedCampaignBiddingStrategy(30_000)
	if err != nil {
		t.Fatal(err)
	}
	baseGraph := yandexdirect.CampaignGraph{
		Campaign: yandexdirect.GraphCampaign{
			ID: 11, Name: baseCampaign.Name, Status: "ACCEPTED", State: "OFF",
			Type: "UNIFIED_CAMPAIGN", WeeklyBudgetMinor: 30_000,
			StartsAt: start, EndsAt: start.AddDate(0, 1, 0),
			TimeZone:        "Europe/Moscow",
			TimeTargeting:   yandexdirect.SafeUnifiedCampaignTimeTargeting(),
			BiddingStrategy: strategy,
			Settings:        yandexdirect.SafeUnifiedCampaignSettings(),
			TrackingParams:  "mp_op=" + oldMarker, AttributionModel: "AUTO",
		},
		AdGroups: []yandexdirect.UnifiedAdGroup{{
			ID: 12, CampaignID: 11,
			Name:      yandexdirect.UnifiedAdGroupOperationName(oldMarker),
			RegionIDs: []int64{225}, NegativeKeywords: []string{},
			OfferRetargeting: "NO", Status: "ACCEPTED",
		}},
		Ads: []yandexdirect.ResponsiveAd{{
			ID: 13, CampaignID: 11, AdGroupID: 12,
			Titles: []yandexdirect.ModeratedText{{
				Value: "Safe title", Status: "ACCEPTED",
			}},
			Texts: []yandexdirect.ModeratedText{{
				Value: "Safe text", Status: "ACCEPTED",
			}},
			Href: "https://maxposty.ru/", Status: "ACCEPTED", State: "ON",
		}},
		Keywords: []yandexdirect.Keyword{
			{
				ID: 14, CampaignID: 11, AdGroupID: 12, Keyword: "old one",
				StrategyPriority: "NORMAL", Status: "ACCEPTED", State: "ON",
			},
			{
				ID: 15, CampaignID: 11, AdGroupID: 12, Keyword: "old two",
				StrategyPriority: "NORMAL", Status: "ACCEPTED", State: "ON",
			},
		},
	}
	baseHash, err := baseGraph.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	campaignID, groupID, adID := int64(11), int64(12), int64(13)
	material := store.DirectGraphSubmissionMaterial{
		Campaign: baseCampaign, DesiredCampaign: desired,
		Connection: store.DirectConnection{Timezone: "Europe/Moscow"},
		Operation: store.DirectProviderOperation{
			OperationKind: "update", Stage: "ad_updated",
			OperationMarker: newMarker, ExpectedGraphHash: baseHash,
			ProviderCampaignID: &campaignID, ProviderAdGroupID: &groupID,
			ProviderAdID: &adID,
		},
	}
	baseline := directProviderEditBaseline{
		Graph: baseGraph, GraphHash: baseHash, BaseCampaign: baseCampaign,
		BaseRegionIDs: []int64{225}, DesiredRegionIDs: []int64{225},
	}
	current := baseGraph
	current.Campaign.TrackingParams = "mp_op=" + newMarker
	current.AdGroups = append([]yandexdirect.UnifiedAdGroup(nil), baseGraph.AdGroups...)
	current.AdGroups[0].Name = yandexdirect.UnifiedAdGroupOperationName(newMarker)
	current.Keywords = append([]yandexdirect.Keyword(nil), baseGraph.Keywords...)
	current.Keywords[0].Keyword = "new one"
	if _, err := directVerifyProviderEditPrefix(
		current, material, baseline, "ad_updated",
	); err != nil {
		t.Fatalf("owned partial keyword transition was rejected: %v", err)
	}
	current.Keywords = append(current.Keywords, yandexdirect.Keyword{
		ID: 16, CampaignID: 11, AdGroupID: 12, Keyword: "external drift",
		StrategyPriority: "NORMAL",
	})
	if _, err := directVerifyProviderEditPrefix(
		current, material, baseline, "ad_updated",
	); !errors.Is(err, ErrDirectSnapshotMismatch) {
		t.Fatalf("external keyword drift error = %v", err)
	}
}
