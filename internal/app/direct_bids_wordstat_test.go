package app

import (
	"context"
	"errors"
	"testing"

	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexwordstat"
)

func TestDirectBidCeilingUsesVerifiedGraphEdit(t *testing.T) {
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
	before, err := application.GetDirectCampaignBids(
		ctx, owner, workspace.ID, campaign.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if before.Strategy != "WB_MAXIMUM_CLICKS" || before.BidCeilingMinor != nil ||
		before.MinimumBidMinor != 30 || before.MaximumBidMinor != 2_500_000 {
		t.Fatalf("initial bids = %#v", before)
	}
	updated, err := application.UpdateDirectCampaignBidCeiling(
		ctx, owner, workspace.ID, campaign.ID, DirectBidCeilingChange{
			BidCeilingMinor: int64Pointer(1_000), ExpectedVersion: before.Version,
			ExpectedGraphHash: before.GraphHash, ExpectedRevisionID: before.RevisionID,
			Confirmation: "ИЗМЕНИТЬ СТАВКУ",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if updated.BidCeilingMinor == nil || *updated.BidCeilingMinor != 1_000 ||
		updated.Version != before.Version+1 || updated.GraphHash == before.GraphHash ||
		updated.RevisionID == before.RevisionID || provider.campaign.BidCeilingMinor != 1_000 {
		t.Fatalf("updated bids = %#v provider=%#v", updated, provider.campaign)
	}
	persisted, err := storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil || persisted.BidCeilingMinor != 1_000 ||
		persisted.ProviderGraphHash != updated.GraphHash {
		t.Fatalf("persisted campaign = %#v, %v", persisted, err)
	}
	cleared, err := application.UpdateDirectCampaignBidCeiling(
		ctx, owner, workspace.ID, campaign.ID, DirectBidCeilingChange{
			BidCeilingMinor: nil, ExpectedVersion: updated.Version,
			ExpectedGraphHash: updated.GraphHash, ExpectedRevisionID: updated.RevisionID,
			Confirmation: "ИЗМЕНИТЬ СТАВКУ",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.BidCeilingMinor != nil || provider.campaign.BidCeilingMinor != 0 {
		t.Fatalf("cleared bids = %#v provider=%#v", cleared, provider.campaign)
	}
	_, err = application.UpdateDirectCampaignBidCeiling(
		ctx, owner, workspace.ID, campaign.ID, DirectBidCeilingChange{
			BidCeilingMinor: int64Pointer(2_000), ExpectedVersion: cleared.Version,
			ExpectedGraphHash: cleared.GraphHash, ExpectedRevisionID: cleared.RevisionID,
			Confirmation: "изменить ставку",
		},
	)
	if !errors.Is(err, store.ErrDirectValidation) {
		t.Fatalf("confirmation error = %v", err)
	}
}

func TestDirectBidCeilingRejectsStaleAndUnsafeValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	application, _, _, owner, workspace, _, clock := newDirectAppFixture(t, ctx, false)
	campaign := createDirectAppCampaign(t, ctx, application, owner, workspace.ID, *clock)
	campaign, err := application.SubmitDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
	)
	if err != nil {
		t.Fatal(err)
	}
	bids, err := application.GetDirectCampaignBids(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = application.UpdateDirectCampaignBidCeiling(
		ctx, owner, workspace.ID, campaign.ID, DirectBidCeilingChange{
			BidCeilingMinor: int64Pointer(bids.MaximumBidMinor + 1),
			ExpectedVersion: bids.Version, ExpectedGraphHash: bids.GraphHash,
			ExpectedRevisionID: bids.RevisionID, Confirmation: "ИЗМЕНИТЬ СТАВКУ",
		},
	)
	if !errors.Is(err, store.ErrDirectValidation) {
		t.Fatalf("unsafe ceiling error = %v", err)
	}
	_, err = application.UpdateDirectCampaignBidCeiling(
		ctx, owner, workspace.ID, campaign.ID, DirectBidCeilingChange{
			BidCeilingMinor: int64Pointer(1_000), ExpectedVersion: bids.Version,
			ExpectedGraphHash: "stale", ExpectedRevisionID: bids.RevisionID,
			Confirmation: "ИЗМЕНИТЬ СТАВКУ",
		},
	)
	if !errors.Is(err, store.ErrDirectConsentMismatch) {
		t.Fatalf("stale snapshot error = %v", err)
	}
}

type fakeWordstatProvider struct {
	request yandexwordstat.TopRequest
	result  yandexwordstat.TopResult
}

func (f *fakeWordstatProvider) GetTop(
	_ context.Context, request yandexwordstat.TopRequest,
) (yandexwordstat.TopResult, error) {
	f.request = request
	return f.result, nil
}

func TestWordstatSuggestionsResolveCampaignRegionsAndDedupe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	application, _, _, owner, workspace, _, clock := newDirectAppFixture(t, ctx, false)
	campaign := createDirectAppCampaign(t, ctx, application, owner, workspace.ID, *clock)
	wordstat := &fakeWordstatProvider{result: yandexwordstat.TopResult{
		TotalCount: 100,
		Results: []yandexwordstat.Phrase{
			{Phrase: "Канал MAX", Count: 80},
			{Phrase: "ведение канала", Count: 40},
		},
		Associations: []yandexwordstat.Phrase{
			{Phrase: "канал max", Count: 70},
			{Phrase: "продвижение max", Count: 20},
		},
	}}
	if err := application.ConfigureWordstat(wordstat); err != nil {
		t.Fatal(err)
	}
	result, err := application.SuggestDirectCampaignKeywords(
		ctx, owner, workspace.ID, campaign.ID, "  канал MAX ", 3,
	)
	if err != nil {
		t.Fatal(err)
	}
	if wordstat.request.Phrase != "канал MAX" || wordstat.request.Limit != 3 ||
		len(wordstat.request.Regions) != 1 || wordstat.request.Regions[0] != "225" {
		t.Fatalf("provider request = %#v", wordstat.request)
	}
	if len(result.Items) != 3 || result.Items[0].Source != "included" ||
		result.Items[1].Source != "included" || result.Items[2].Source != "association" ||
		result.Items[2].Phrase != "продвижение max" {
		t.Fatalf("suggestions = %#v", result)
	}
}

func int64Pointer(value int64) *int64 { return &value }
