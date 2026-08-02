package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexdirect"
)

var ErrDirectBidsUnavailable = errors.New("bid management for Yandex Direct is unavailable")

// DirectBiddingProvider is separate from the core OAuth provider contract so
// tests and sandbox/read-only providers remain fail-closed until they expose
// the current v501 campaign strategy and currency dictionary.
type DirectBiddingProvider interface {
	GetCampaignBidConfiguration(
		context.Context, string, string, int64,
	) (yandexdirect.CampaignBidConfiguration, error)
	GetCurrencyBidLimits(
		context.Context, string, string, string,
	) (yandexdirect.CurrencyBidLimits, error)
}

type DirectCampaignBids struct {
	CampaignID        string
	CurrencyCode      string
	Strategy          string
	WeeklyBudgetMinor int64
	BidCeilingMinor   *int64
	MinimumBidMinor   int64
	MaximumBidMinor   int64
	Version           int64
	GraphHash         string
	RevisionID        string
	UpdatedAt         time.Time
}

type DirectBidCeilingChange struct {
	// BidCeilingMinor nil removes the optional ceiling.
	BidCeilingMinor    *int64
	ExpectedVersion    int64
	ExpectedGraphHash  string
	ExpectedRevisionID string
	Confirmation       string
}

func (a *App) GetDirectCampaignBids(
	ctx context.Context, actorUserID, workspaceID, campaignID string,
) (DirectCampaignBids, error) {
	campaign, connection, provider, token, err := a.directBidMaterial(
		ctx, actorUserID, workspaceID, campaignID,
	)
	if err != nil {
		return DirectCampaignBids{}, err
	}
	providerConfig, err := provider.GetCampaignBidConfiguration(
		ctx, token, connection.ClientLogin, *campaign.ProviderCampaignID,
	)
	if err != nil {
		return DirectCampaignBids{}, a.directGraphProviderError(ctx, connection, err)
	}
	limits, err := provider.GetCurrencyBidLimits(
		ctx, token, connection.ClientLogin, campaign.CurrencyCode,
	)
	if err != nil {
		return DirectCampaignBids{}, a.directGraphProviderError(ctx, connection, err)
	}
	if providerConfig.CampaignID != *campaign.ProviderCampaignID ||
		providerConfig.WeeklyBudgetMinor != campaign.WeeklyBudgetMinor ||
		providerConfig.BidCeilingMinor != campaign.BidCeilingMinor ||
		limits.CurrencyCode != campaign.CurrencyCode {
		return DirectCampaignBids{}, fmt.Errorf(
			"%w: bid configuration does not match the verified campaign graph",
			ErrDirectSnapshotMismatch,
		)
	}
	return directCampaignBids(campaign, limits), nil
}

func (a *App) UpdateDirectCampaignBidCeiling(
	ctx context.Context, actorUserID, workspaceID, campaignID string,
	change DirectBidCeilingChange,
) (DirectCampaignBids, error) {
	if strings.TrimSpace(change.Confirmation) != "ИЗМЕНИТЬ СТАВКУ" {
		return DirectCampaignBids{}, fmt.Errorf(
			"%w: exact bid change confirmation is required", store.ErrDirectValidation,
		)
	}
	current, err := a.GetDirectCampaignBids(ctx, actorUserID, workspaceID, campaignID)
	if err != nil {
		return DirectCampaignBids{}, err
	}
	if change.ExpectedVersion <= 0 || current.Version != change.ExpectedVersion ||
		current.GraphHash != strings.TrimSpace(change.ExpectedGraphHash) ||
		current.RevisionID != strings.TrimSpace(change.ExpectedRevisionID) {
		return DirectCampaignBids{}, store.ErrDirectConsentMismatch
	}
	value := int64(0)
	if change.BidCeilingMinor != nil {
		value = *change.BidCeilingMinor
		if value < current.MinimumBidMinor || value > current.MaximumBidMinor {
			return DirectCampaignBids{}, fmt.Errorf(
				"%w: bid_ceiling_minor is outside the allowed range",
				store.ErrDirectValidation,
			)
		}
	}
	if (current.BidCeilingMinor == nil && value == 0) ||
		(current.BidCeilingMinor != nil && *current.BidCeilingMinor == value) {
		return current, nil
	}
	updated, err := a.UpdateDirectCampaign(
		ctx, actorUserID, workspaceID, campaignID,
		store.DirectCampaignChanges{
			BidCeilingMinor: &value, ExpectedVersion: change.ExpectedVersion,
		},
		change.ExpectedGraphHash, change.ExpectedRevisionID,
	)
	if err != nil {
		return DirectCampaignBids{}, err
	}
	// UpdateDirectCampaign completes only after the provider graph has been
	// reconciled and a new immutable revision has been persisted.
	result := directCampaignBids(updated, yandexdirect.CurrencyBidLimits{
		CurrencyCode:    current.CurrencyCode,
		MinimumBidMinor: current.MinimumBidMinor,
		MaximumBidMinor: current.MaximumBidMinor,
	})
	return result, nil
}

func (a *App) directBidMaterial(
	ctx context.Context, actorUserID, workspaceID, campaignID string,
) (store.DirectCampaign, store.DirectConnection, DirectBiddingProvider, string, error) {
	if !a.DirectConfigured() {
		return store.DirectCampaign{}, store.DirectConnection{}, nil, "", ErrDirectNotConfigured
	}
	provider, ok := a.direct.(DirectBiddingProvider)
	if !ok {
		return store.DirectCampaign{}, store.DirectConnection{}, nil, "", ErrDirectBidsUnavailable
	}
	campaign, err := a.store.GetDirectCampaign(ctx, actorUserID, workspaceID, campaignID)
	if err != nil {
		return store.DirectCampaign{}, store.DirectConnection{}, nil, "", err
	}
	if campaign.ProviderCampaignID == nil || campaign.GraphVerifiedAt == nil ||
		strings.TrimSpace(campaign.ProviderGraphHash) == "" ||
		strings.TrimSpace(campaign.ProviderRevisionID) == "" {
		return store.DirectCampaign{}, store.DirectConnection{}, nil, "", store.ErrDirectGraphUnverified
	}
	connection, err := a.store.GetDirectConnection(ctx, actorUserID, workspaceID)
	if err != nil || connection.ID != campaign.ConnectionID || connection.Status != "active" ||
		connection.RevokedAt != nil {
		if err == nil {
			err = store.ErrDirectConnectionRequired
		}
		return store.DirectCampaign{}, store.DirectConnection{}, nil, "", err
	}
	token, err := a.directAccessToken(ctx, connection)
	if err != nil {
		return store.DirectCampaign{}, store.DirectConnection{}, nil, "", err
	}
	return campaign, connection, provider, token, nil
}

func directCampaignBids(
	campaign store.DirectCampaign, limits yandexdirect.CurrencyBidLimits,
) DirectCampaignBids {
	var ceiling *int64
	if campaign.BidCeilingMinor != 0 {
		value := campaign.BidCeilingMinor
		ceiling = &value
	}
	return DirectCampaignBids{
		CampaignID: campaign.ID, CurrencyCode: campaign.CurrencyCode,
		Strategy: "WB_MAXIMUM_CLICKS", WeeklyBudgetMinor: campaign.WeeklyBudgetMinor,
		BidCeilingMinor: ceiling, MinimumBidMinor: limits.MinimumBidMinor,
		MaximumBidMinor: limits.MaximumBidMinor, Version: campaign.Version,
		GraphHash: campaign.ProviderGraphHash, RevisionID: campaign.ProviderRevisionID,
		UpdatedAt: campaign.UpdatedAt,
	}
}
