package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexdirect"
)

type DirectExternalEditingProvider interface {
	GetExternalCampaignDetail(context.Context, string, string, int64) (yandexdirect.ExternalCampaignDetail, error)
	UpdateExternalCampaignName(context.Context, string, string, int64, string) error
	UpdateExternalAd(context.Context, string, string, yandexdirect.ExternalAd, yandexdirect.ExternalAdChanges) error
}
type DirectCopySuggester interface {
	SuggestDirectCopy(context.Context, openairesearch.SuggestDirectCopyRequest) (openairesearch.SuggestDirectCopyResult, error)
}
type DirectExternalDetail struct {
	Connection store.DirectConnection
	Control    store.DirectExternalEditControl
	Provider   yandexdirect.ExternalCampaignDetail
}

func (a *App) DirectExternalDetailsConfigured() bool {
	_, ok := a.direct.(DirectExternalEditingProvider)
	return a.DirectConfigured() && ok
}
func (a *App) DirectExternalEditConfigured() bool {
	return a.DirectExternalDetailsConfigured() && a.DirectWritesEnabled()
}
func (a *App) DirectExternalCopyAIConfigured() bool {
	_, ok := a.research.(DirectCopySuggester)
	return a.DirectExternalDetailsConfigured() && a.research != nil && ok
}

func (a *App) GetDirectExternalDetail(ctx context.Context, actor, workspace, expectedConnection string, campaign int64) (DirectExternalDetail, error) {
	provider, ok := a.direct.(DirectExternalEditingProvider)
	if !a.DirectConfigured() || !ok {
		return DirectExternalDetail{}, ErrDirectNotConfigured
	}
	connection, err := a.store.GetDirectExternalEditContext(ctx, actor, workspace, expectedConnection, campaign, false)
	if err != nil {
		return DirectExternalDetail{}, err
	}
	token, err := a.directAccessToken(ctx, connection)
	if err != nil {
		return DirectExternalDetail{}, err
	}
	detail, err := provider.GetExternalCampaignDetail(ctx, token, connection.ClientLogin, campaign)
	if err != nil {
		return DirectExternalDetail{}, a.directGraphProviderError(ctx, connection, err)
	}
	if detail.Campaign.ID != campaign {
		return DirectExternalDetail{}, ErrDirectProvider
	}
	hash, err := detail.Fingerprint()
	if err != nil {
		return DirectExternalDetail{}, ErrDirectProvider
	}
	control, err := a.store.ObserveDirectExternalEdit(ctx, actor, workspace, connection.ID, campaign, hash, a.now().UTC())
	if err != nil {
		return DirectExternalDetail{}, err
	}
	return DirectExternalDetail{Connection: connection, Control: control, Provider: detail}, nil
}

func externalDetailMatches(detail DirectExternalDetail, fence store.DirectExternalEditFence) error {
	if detail.Control.EditState != "idle" {
		return store.ErrDirectProviderOperationBusy
	}
	hash, err := detail.Provider.Fingerprint()
	if err != nil {
		return ErrDirectProvider
	}
	if fence.ConnectionID != detail.Connection.ID || fence.Version != detail.Control.Version || fence.ObservedHash != hash || fence.ObservedHash != detail.Control.ObservedHash || fence.RevisionID != detail.Control.RevisionID {
		return store.ErrConflict
	}
	return nil
}

func (a *App) EditDirectExternalCampaignName(ctx context.Context, actor, workspace string, campaign int64, fence store.DirectExternalEditFence, name string) (DirectExternalDetail, error) {
	if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 255 {
		return DirectExternalDetail{}, fmt.Errorf("%w: name must contain 1 to 255 characters", store.ErrDirectValidation)
	}
	for _, r := range name {
		if r < ' ' {
			return DirectExternalDetail{}, store.ErrDirectValidation
		}
	}
	return a.editDirectExternal(ctx, actor, workspace, campaign, fence, func(detail yandexdirect.ExternalCampaignDetail) (yandexdirect.ExternalCampaignDetail, error) {
		if detail.Campaign.State == "ARCHIVED" {
			return detail, store.ErrDirectValidation
		}
		detail.Campaign.Name = name
		return detail, nil
	}, func(provider DirectExternalEditingProvider, token, login string, current yandexdirect.ExternalCampaignDetail) error {
		return provider.UpdateExternalCampaignName(ctx, token, login, campaign, name)
	})
}

func (a *App) EditDirectExternalAd(ctx context.Context, actor, workspace string, campaign, adID int64, fence store.DirectExternalEditFence, patch yandexdirect.ExternalAdChanges) (DirectExternalDetail, error) {
	if err := patch.Validate(); err != nil {
		return DirectExternalDetail{}, fmt.Errorf("%w: %w", store.ErrDirectValidation, err)
	}
	return a.editDirectExternal(ctx, actor, workspace, campaign, fence, func(detail yandexdirect.ExternalCampaignDetail) (yandexdirect.ExternalCampaignDetail, error) {
		detail.Ads = append([]yandexdirect.ExternalAd(nil), detail.Ads...)
		for i, ad := range detail.Ads {
			if ad.ID == adID {
				merged, err := patch.Merge(ad)
				if err != nil {
					return detail, fmt.Errorf("%w: %w", store.ErrDirectValidation, err)
				}
				detail.Ads[i] = merged
				return detail, nil
			}
		}
		return detail, store.ErrNotFound
	}, func(provider DirectExternalEditingProvider, token, login string, current yandexdirect.ExternalCampaignDetail) error {
		for _, ad := range current.Ads {
			if ad.ID == adID {
				return provider.UpdateExternalAd(ctx, token, login, ad, patch)
			}
		}
		return store.ErrNotFound
	})
}

func (a *App) editDirectExternal(ctx context.Context, actor, workspace string, campaign int64, fence store.DirectExternalEditFence, merge func(yandexdirect.ExternalCampaignDetail) (yandexdirect.ExternalCampaignDetail, error), write func(DirectExternalEditingProvider, string, string, yandexdirect.ExternalCampaignDetail) error) (DirectExternalDetail, error) {
	if !a.DirectExternalEditConfigured() {
		return DirectExternalDetail{}, ErrDirectWritesDisabled
	}
	// Verify mutation ownership before any upstream request.
	if _, err := a.store.GetDirectExternalEditContext(ctx, actor, workspace, fence.ConnectionID, campaign, true); err != nil {
		return DirectExternalDetail{}, err
	}
	current, err := a.GetDirectExternalDetail(ctx, actor, workspace, fence.ConnectionID, campaign)
	if err != nil {
		return current, err
	}
	if err = externalDetailMatches(current, fence); err != nil {
		return current, err
	}
	desired, err := merge(current.Provider)
	if err != nil {
		return current, err
	}
	desiredHash, err := desired.Fingerprint()
	if err != nil {
		return current, ErrDirectProvider
	}
	if desiredHash == current.Control.ObservedHash {
		return current, nil
	}
	operation, err := a.store.ClaimDirectExternalEdit(ctx, actor, workspace, campaign, fence, desiredHash, a.now().UTC())
	if err != nil {
		return current, err
	}
	started := false
	defer func() {
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		// If a readback already completed the operation, the private CAS returns
		// conflict. The deferred path can never overwrite a newer operation.
		_ = a.store.FinishDirectExternalEdit(finishCtx, workspace, current.Connection.ID, campaign, operation, started)
	}()
	token, err := a.directAccessToken(ctx, current.Connection)
	if err != nil {
		return current, err
	}
	if err = a.store.BeginDirectExternalWrite(ctx, actor, workspace, current.Connection.ID, campaign, operation, a.now().UTC()); err != nil {
		return current, err
	}
	started = true
	err = write(a.direct.(DirectExternalEditingProvider), token, current.Connection.ClientLogin, current.Provider)
	if err != nil {
		var rejected *yandexdirect.PartialMutationError
		if errors.As(err, &rejected) {
			started = false
		}
		return current, a.directGraphProviderError(ctx, current.Connection, err)
	}
	// Readback is necessary even for a successful HTTP update. Never retry the
	// provider mutation, including when a client disconnects during the write.
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	verified, err := a.GetDirectExternalDetail(readCtx, actor, workspace, current.Connection.ID, campaign)
	if err != nil {
		return current, err
	}
	if verified.Control.EditState != "idle" || verified.Control.ObservedHash != desiredHash {
		return verified, store.ErrDirectProviderOperationBusy
	}
	return verified, nil
}

func (a *App) BookmarkDirectExternal(ctx context.Context, actor, workspace, connection string, campaign int64, bookmarked bool) (DirectExternalDetail, error) {
	// Refresh establishes a durable local control row; this never writes Direct.
	if _, err := a.store.GetDirectExternalEditContext(ctx, actor, workspace, connection, campaign, false); err != nil {
		return DirectExternalDetail{}, err
	}
	detail, err := a.GetDirectExternalDetail(ctx, actor, workspace, connection, campaign)
	if err != nil {
		return detail, err
	}
	if err = a.store.BookmarkDirectExternalCampaign(ctx, actor, workspace, detail.Connection.ID, campaign, bookmarked); err != nil {
		return detail, err
	}
	detail.Control.Bookmarked = bookmarked
	return detail, nil
}

func (a *App) SuggestDirectExternalCopyWithBeforeGenerate(ctx context.Context, actor, workspace string, campaign, adID int64, fence store.DirectExternalEditFence, brief string, draft *yandexdirect.ExternalAdChanges, beforeGenerate func() error) (openairesearch.SuggestDirectCopyResult, error) {
	if err := openairesearch.ValidateDirectCopyBrief(brief); err != nil {
		return openairesearch.SuggestDirectCopyResult{}, err
	}
	suggester, ok := a.research.(DirectCopySuggester)
	if !a.DirectExternalCopyAIConfigured() || !ok {
		return openairesearch.SuggestDirectCopyResult{}, ErrResearchNotConfigured
	}
	// This is creative work even when provider writes are administratively off.
	if err := a.store.RequireDirectCreativeRole(ctx, actor, workspace); err != nil {
		return openairesearch.SuggestDirectCopyResult{}, err
	}
	detail, err := a.GetDirectExternalDetail(ctx, actor, workspace, fence.ConnectionID, campaign)
	if err != nil {
		return openairesearch.SuggestDirectCopyResult{}, err
	}
	if err = externalDetailMatches(detail, fence); err != nil {
		return openairesearch.SuggestDirectCopyResult{}, err
	}
	var selected *yandexdirect.ExternalAd
	for _, ad := range detail.Provider.Ads {
		if ad.ID == adID {
			copy := ad
			selected = &copy
			break
		}
	}
	if selected == nil {
		return openairesearch.SuggestDirectCopyResult{}, store.ErrNotFound
	}
	if selected.Type != "TEXT_AD" && selected.Type != "RESPONSIVE_AD" {
		return openairesearch.SuggestDirectCopyResult{}, store.ErrDirectValidation
	}
	if draft != nil {
		merged, mergeErr := draft.Merge(*selected)
		if mergeErr != nil {
			return openairesearch.SuggestDirectCopyResult{}, fmt.Errorf("%w: %w", store.ErrDirectValidation, mergeErr)
		}
		selected = &merged
	}
	request := openairesearch.SuggestDirectCopyRequest{Brief: brief, CampaignName: detail.Provider.Campaign.Name, AdType: selected.Type, Titles: selected.Titles, Texts: selected.Texts, Href: selected.Href}
	if selected.Type == "TEXT_AD" {
		request.Titles = []string{selected.Title}
		if selected.Title2 != nil {
			request.Titles = append(request.Titles, *selected.Title2)
		}
		request.Texts = []string{selected.Text}
	}
	if err = openairesearch.ValidateSuggestDirectCopyRequest(request); err != nil {
		return openairesearch.SuggestDirectCopyResult{}, err
	}
	if beforeGenerate != nil {
		if err = beforeGenerate(); err != nil {
			return openairesearch.SuggestDirectCopyResult{}, err
		}
	}
	return suggester.SuggestDirectCopy(ctx, request)
}
