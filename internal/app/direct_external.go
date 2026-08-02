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

const (
	DirectExternalCampaignSyncCooldown = 30 * time.Second
	directExternalCampaignSyncLease    = 5 * time.Minute
)

type DirectExternalCampaignSnapshot struct {
	Items    []store.DirectExternalCampaign
	SyncedAt *time.Time
}

// ListDirectExternalCampaigns returns the latest persisted provider snapshot
// for the currently connected account. It never calls or mutates Yandex.
func (a *App) ListDirectExternalCampaigns(
	ctx context.Context, actorUserID, workspaceID string,
) (DirectExternalCampaignSnapshot, error) {
	return a.directExternalCampaignSnapshot(ctx, actorUserID, workspaceID, "")
}

func (a *App) directExternalCampaignSnapshot(
	ctx context.Context, actorUserID, workspaceID, expectedConnectionID string,
) (DirectExternalCampaignSnapshot, error) {
	snapshot, err := a.store.GetDirectExternalCampaignSnapshot(
		ctx, actorUserID, workspaceID, expectedConnectionID,
	)
	if err != nil {
		return DirectExternalCampaignSnapshot{}, err
	}
	return DirectExternalCampaignSnapshot{
		Items: snapshot.Items, SyncedAt: snapshot.SyncedAt,
	}, nil
}

// SyncDirectExternalCampaigns reads Campaigns.get and atomically replaces the
// external-only snapshot. Existing MaxPosty campaigns and their creation flow
// are left untouched by the store boundary.
func (a *App) SyncDirectExternalCampaigns(
	ctx context.Context, actorUserID, workspaceID string,
) (DirectExternalCampaignSnapshot, error) {
	if !a.DirectConfigured() {
		return DirectExternalCampaignSnapshot{}, ErrDirectNotConfigured
	}
	provider, ok := a.direct.(DirectCampaignListingProvider)
	if !ok {
		return DirectExternalCampaignSnapshot{}, fmt.Errorf(
			"%w: campaign listing is unavailable", ErrDirectProvider,
		)
	}
	connection, err := a.store.GetDirectConnection(ctx, actorUserID, workspaceID)
	if errors.Is(err, store.ErrNotFound) {
		return DirectExternalCampaignSnapshot{}, store.ErrDirectConnectionRequired
	}
	if err != nil {
		return DirectExternalCampaignSnapshot{}, err
	}
	if connection.Status != "active" || connection.RevokedAt != nil {
		return DirectExternalCampaignSnapshot{}, store.ErrDirectConnectionRequired
	}
	value, err, _ := a.directCampaignSync.Do(connection.ID, func() (any, error) {
		current, getErr := a.store.GetDirectConnection(ctx, actorUserID, workspaceID)
		if errors.Is(getErr, store.ErrNotFound) {
			return nil, store.ErrDirectConnectionRequired
		}
		if getErr != nil {
			return nil, getErr
		}
		if current.ID != connection.ID || current.Status != "active" ||
			current.RevokedAt != nil {
			return nil, store.ErrDirectConnectionRequired
		}
		generation, claimErr := a.store.ClaimDirectExternalCampaignSync(
			ctx, actorUserID, workspaceID, current.ID, a.now().UTC(),
			DirectExternalCampaignSyncCooldown, directExternalCampaignSyncLease,
		)
		if claimErr != nil {
			return nil, claimErr
		}
		claimed := true
		defer func() {
			if !claimed {
				return
			}
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if releaseErr := a.store.ReleaseDirectExternalCampaignSync(
				releaseCtx, actorUserID, workspaceID, current.ID, generation,
			); releaseErr != nil && !errors.Is(releaseErr, store.ErrConflict) {
				a.logger.Error("could not release Yandex Direct campaign sync claim",
					"workspace_id", workspaceID, "connection_id", current.ID,
					"error", releaseErr)
			}
		}()

		token, tokenErr := a.directAccessToken(ctx, current)
		if tokenErr != nil {
			return nil, tokenErr
		}
		providerCampaigns, listErr := provider.ListCampaigns(
			ctx, token, current.ClientLogin,
		)
		if listErr != nil {
			return nil, a.directGraphProviderError(ctx, current, listErr)
		}

		items := make([]store.DirectExternalCampaign, 0, len(providerCampaigns))
		for _, campaign := range providerCampaigns {
			item, mapErr := directExternalCampaignFromProvider(campaign)
			if mapErr != nil {
				return nil, fmt.Errorf("%w: invalid campaign list response", ErrDirectProvider)
			}
			items = append(items, item)
		}
		replaceErr := a.store.ReplaceDirectExternalCampaigns(
			ctx, actorUserID, workspaceID, current.ID, generation, items, a.now().UTC(),
		)
		if errors.Is(replaceErr, store.ErrConflict) {
			claimed = false
			return a.directExternalCampaignSnapshot(
				ctx, actorUserID, workspaceID, current.ID,
			)
		}
		if replaceErr != nil {
			return nil, replaceErr
		}
		claimed = false
		return a.directExternalCampaignSnapshot(
			ctx, actorUserID, workspaceID, current.ID,
		)
	})
	if err != nil {
		return DirectExternalCampaignSnapshot{}, err
	}
	snapshot, ok := value.(DirectExternalCampaignSnapshot)
	if !ok {
		return DirectExternalCampaignSnapshot{}, fmt.Errorf(
			"%w: invalid campaign sync result", ErrDirectProvider,
		)
	}
	return snapshot, nil
}

func directExternalCampaignFromProvider(
	campaign yandexdirect.CampaignSummary,
) (store.DirectExternalCampaign, error) {
	startsAt, err := time.Parse(time.DateOnly, strings.TrimSpace(campaign.StartDate))
	if err != nil {
		return store.DirectExternalCampaign{}, err
	}
	var endsAt *time.Time
	if campaign.EndDate != nil {
		value, parseErr := time.Parse(time.DateOnly, strings.TrimSpace(*campaign.EndDate))
		if parseErr != nil {
			return store.DirectExternalCampaign{}, parseErr
		}
		endsAt = &value
	}
	return store.DirectExternalCampaign{
		ProviderCampaignID:    campaign.ID,
		Name:                  campaign.Name,
		CampaignType:          campaign.Type,
		ProviderStatus:        campaign.Status,
		ProviderState:         campaign.State,
		ProviderStatusPayment: campaign.StatusPayment,
		StartsAt:              startsAt,
		EndsAt:                endsAt,
		Timezone:              campaign.TimeZone,
	}, nil
}
