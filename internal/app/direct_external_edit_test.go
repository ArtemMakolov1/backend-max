package app

import (
	"context"
	"errors"
	"testing"

	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexdirect"
)

type externalEditingFake struct {
	*fakeDirectProvider
	detail           yandexdirect.ExternalCampaignDetail
	writes           int
	readErr          error
	writeErr         error
	applyBeforeError bool
	afterWrite       func()
}

func (f *externalEditingFake) GetExternalCampaignDetail(context.Context, string, string, int64) (yandexdirect.ExternalCampaignDetail, error) {
	return f.detail, f.readErr
}
func (f *externalEditingFake) UpdateExternalCampaignName(_ context.Context, _, _ string, _ int64, name string) error {
	f.writes++
	if f.afterWrite != nil {
		defer f.afterWrite()
	}
	if f.writeErr == nil || f.applyBeforeError {
		f.detail.Campaign.Name = name
	}
	return f.writeErr
}
func (f *externalEditingFake) UpdateExternalAd(_ context.Context, _, _ string, ad yandexdirect.ExternalAd, patch yandexdirect.ExternalAdChanges) error {
	f.writes++
	merged, err := patch.Merge(ad)
	if err != nil {
		return err
	}
	if f.writeErr == nil || f.applyBeforeError {
		for i := range f.detail.Ads {
			if f.detail.Ads[i].ID == ad.ID {
				f.detail.Ads[i] = merged
			}
		}
	}
	return f.writeErr
}
func externalEditFixture(t *testing.T) (*App, *externalEditingFake, string, store.Workspace, store.DirectExternalEditFence) {
	t.Helper()
	ctx := context.Background()
	a, _, base, owner, workspace, connection, now := newDirectAppFixture(t, ctx, false)
	href := "https://example.test/"
	summary := yandexdirect.CampaignSummary{ID: 77, Name: "Внешняя кампания", Type: "TEXT_CAMPAIGN", Status: "ACCEPTED", State: "ON", StatusPayment: "ALLOWED", StartDate: now.Format("2006-01-02"), TimeZone: "Europe/Moscow"}
	base.listedCampaigns = []yandexdirect.CampaignSummary{summary}
	f := &externalEditingFake{fakeDirectProvider: base, detail: yandexdirect.ExternalCampaignDetail{Campaign: summary, Ads: []yandexdirect.ExternalAd{{ID: 12, CampaignID: 77, AdGroupID: 13, Type: "TEXT_AD", State: "ON", Title: "Заголовок", Text: "Текущий текст", Href: &href, Titles: []string{}, Texts: []string{}}}}}
	a.direct = f
	if _, err := a.SyncDirectExternalCampaigns(ctx, owner, workspace.ID); err != nil {
		t.Fatal(err)
	}
	detail, err := a.GetDirectExternalDetail(ctx, owner, workspace.ID, connection.ID, 77)
	if err != nil {
		t.Fatal(err)
	}
	return a, f, owner, workspace, store.DirectExternalEditFence{ConnectionID: connection.ID, Version: detail.Control.Version, ObservedHash: detail.Control.ObservedHash, RevisionID: detail.Control.RevisionID}
}
func TestExternalEditingDetectsDriftAndPreservesProviderGraph(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, f, owner, workspace, fence := externalEditFixture(t)
	f.detail.Ads[0].Text = "Изменение из кабинета Директа"
	title := "Новый заголовок"
	if _, err := a.EditDirectExternalAd(ctx, owner, workspace.ID, 77, 12, fence, yandexdirect.ExternalAdChanges{Title: &title}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("provider drift: %v", err)
	}
	if f.writes != 0 || f.resumeCalls != 0 {
		t.Fatal("stale edit wrote or enabled campaign")
	}
	detail, err := a.GetDirectExternalDetail(ctx, owner, workspace.ID, fence.ConnectionID, 77)
	if err != nil {
		t.Fatal(err)
	}
	fence.Version, fence.ObservedHash, fence.RevisionID = detail.Control.Version, detail.Control.ObservedHash, detail.Control.RevisionID
	saved, err := a.EditDirectExternalAd(ctx, owner, workspace.ID, 77, 12, fence, yandexdirect.ExternalAdChanges{Title: &title})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Control.EditState != "idle" || saved.Provider.Ads[0].Title != title || saved.Provider.Ads[0].Text != "Изменение из кабинета Директа" || saved.Provider.Campaign.State != "ON" || f.writes != 1 || f.resumeCalls != 0 {
		t.Fatalf("unsafe edit result %+v", saved)
	}
	if _, err = a.BookmarkDirectExternal(ctx, owner, workspace.ID, fence.ConnectionID, 77, true); err != nil {
		t.Fatal(err)
	}
	if f.writes != 1 {
		t.Fatal("bookmark wrote Direct")
	}
}
func TestExternalAmbiguousWriteIsNeverRetriedAndExactReadbackSettles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, f, owner, workspace, fence := externalEditFixture(t)
	f.writeErr = context.DeadlineExceeded
	if _, err := a.EditDirectExternalCampaignName(ctx, owner, workspace.ID, 77, fence, "Новое название"); err == nil {
		t.Fatal("ambiguous outcome accepted")
	}
	uncertain, err := a.GetDirectExternalDetail(ctx, owner, workspace.ID, fence.ConnectionID, 77)
	if err != nil || uncertain.Control.EditState != "uncertain" {
		t.Fatalf("uncertainty lost: %+v %v", uncertain, err)
	}
	if _, err = a.EditDirectExternalCampaignName(ctx, owner, workspace.ID, 77, fence, "Новое название"); !errors.Is(err, store.ErrDirectProviderOperationBusy) {
		t.Fatalf("blind retry: %v", err)
	}
	if f.writes != 1 {
		t.Fatal("ambiguous write retried")
	}
	f.detail.Campaign.Name = "Новое название"
	settled, err := a.GetDirectExternalDetail(ctx, owner, workspace.ID, fence.ConnectionID, 77)
	if err != nil || settled.Control.EditState != "idle" {
		t.Fatalf("readback didn't settle: %+v %v", settled, err)
	}
}

type directCopyFake struct {
	calls   int
	request openairesearch.SuggestDirectCopyRequest
}

func (f *directCopyFake) Generate(context.Context, openairesearch.Request) (openairesearch.Result, error) {
	panic("wrong AI method")
}
func (f *directCopyFake) SuggestDirectCopy(_ context.Context, r openairesearch.SuggestDirectCopyRequest) (openairesearch.SuggestDirectCopyResult, error) {
	f.calls++
	f.request = r
	return openairesearch.SuggestDirectCopyResult{}, nil
}
func TestExternalCopyValidatesTenantRevisionAndDraftBeforeQuotaAndAI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, f, owner, workspace, fence := externalEditFixture(t)
	ai := &directCopyFake{}
	a.research = ai
	charged := 0
	before := func() error { charged++; return nil }
	brief := "Сделай текущий текст яснее без новых обещаний"
	for _, tc := range []struct {
		actor string
		ad    int64
		brief string
		fence store.DirectExternalEditFence
		draft *yandexdirect.ExternalAdChanges
	}{{owner, 12, "x", fence, nil}, {"outsider", 12, brief, fence, nil}, {owner, 999, brief, fence, nil}, {owner, 12, brief, store.DirectExternalEditFence{ConnectionID: fence.ConnectionID, Version: fence.Version + 1, ObservedHash: fence.ObservedHash, RevisionID: fence.RevisionID}, nil}, {owner, 12, brief, fence, &yandexdirect.ExternalAdChanges{}}} {
		if _, err := a.SuggestDirectExternalCopyWithBeforeGenerate(ctx, tc.actor, workspace.ID, 77, tc.ad, tc.fence, tc.brief, tc.draft, before); err == nil {
			t.Fatal("invalid scope/draft accepted")
		}
	}
	if charged != 0 || ai.calls != 0 || f.writes != 0 {
		t.Fatal("invalid input charged or called AI/Direct")
	}
	if _, err := a.SuggestDirectExternalCopyWithBeforeGenerate(ctx, owner, workspace.ID, 77, 12, fence, brief, nil, before); err != nil {
		t.Fatal(err)
	}
	if charged != 1 || ai.calls != 1 || ai.request.CampaignName != f.detail.Campaign.Name || ai.request.Texts[0] != f.detail.Ads[0].Text || f.writes != 0 {
		t.Fatalf("copy context/charge %+v count%d", ai.request, charged)
	}
}

func TestExternalWriteBoundariesKeepClaimStateWithoutRepeatingProviderMutation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		rejected        bool
		readbackFailure bool
		cancelRequest   bool
		expected        string
	}{
		{name: "definitive item rejection", rejected: true, expected: "idle"},
		{name: "successful write unreadable readback", readbackFailure: true, expected: "uncertain"},
		{name: "canceled ambiguous write", cancelRequest: true, expected: "uncertain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, f, owner, workspace, fence := externalEditFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.rejected {
				f.writeErr = &yandexdirect.PartialMutationError{Operation: "campaigns.update", Results: []yandexdirect.MutationResult{{Errors: []yandexdirect.ProviderIssue{{Code: 1}}}}}
			}
			if tc.readbackFailure {
				f.afterWrite = func() { f.readErr = context.DeadlineExceeded }
			}
			if tc.cancelRequest {
				f.writeErr = context.DeadlineExceeded
				f.afterWrite = cancel
			}
			if _, err := a.EditDirectExternalCampaignName(ctx, owner, workspace.ID, 77, fence, "Новое название"); err == nil {
				t.Fatal("failed/ambiguous outcome accepted")
			}
			f.readErr = nil
			// Read a non-matching snapshot for a successful-but-unconfirmed response,
			// so the inspection cannot settle it merely by proving the exact result.
			if tc.readbackFailure {
				f.detail.Campaign.Name = "Другой текущий результат"
			}
			observed, err := a.GetDirectExternalDetail(context.Background(), owner, workspace.ID, fence.ConnectionID, 77)
			if err != nil || observed.Control.EditState != tc.expected {
				t.Fatalf("claim state %s %+v %v", tc.expected, observed.Control, err)
			}
			if f.writes != 1 {
				t.Fatalf("provider mutation repeated %d", f.writes)
			}
		})
	}
}
