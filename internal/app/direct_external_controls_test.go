package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexdirect"
)

type externalControlsFake struct {
	*externalEditingFake
	nextID int64
}

func (f *externalControlsFake) GetExternalCampaignControls(context.Context, string, string, int64) (yandexdirect.ExternalCampaignControls, error) {
	if f.readErr != nil {
		return yandexdirect.ExternalCampaignControls{}, f.readErr
	}
	return *f.detail.Controls, nil
}
func (f *externalControlsFake) mutate(fn func()) error {
	f.writes++
	if f.writeErr == nil || f.applyBeforeError {
		fn()
	}
	if f.afterWrite != nil {
		f.afterWrite()
	}
	return f.writeErr
}
func (f *externalControlsFake) UpdateExternalBudget(_ context.Context, _, _ string, _ int64, _ yandexdirect.ExternalCampaignControls, key, amount, _ string) error {
	return f.mutate(func() {
		for i := range f.detail.Controls.Budgets {
			if f.detail.Controls.Budgets[i].Key == key {
				f.detail.Controls.Budgets[i].AmountMicros = amount
			}
		}
	})
}
func (f *externalControlsFake) UpdateExternalGroup(_ context.Context, _, _ string, g yandexdirect.ExternalGroup, p yandexdirect.ExternalGroupChanges) error {
	return f.mutate(func() {
		for i := range f.detail.Controls.Groups {
			if f.detail.Controls.Groups[i].ID == g.ID {
				out := &f.detail.Controls.Groups[i]
				if p.Name != nil {
					out.Name = *p.Name
				}
				if p.RegionIDs != nil {
					out.RegionIDs = append([]int64(nil), (*p.RegionIDs)...)
				}
				if p.NegativeKeywords != nil {
					out.NegativeKeywords = append([]string{}, (*p.NegativeKeywords)...)
				}
			}
		}
	})
}
func (f *externalControlsFake) UpdateExternalKeyword(_ context.Context, _, _ string, k yandexdirect.ExternalKeyword, text string) (int64, error) {
	id := k.ID
	if f.nextID > 0 {
		id = f.nextID
	}
	err := f.mutate(func() {
		for i := range f.detail.Controls.Groups {
			for j := range f.detail.Controls.Groups[i].Keywords {
				out := &f.detail.Controls.Groups[i].Keywords[j]
				if out.ID == k.ID {
					out.ID = id
					out.Keyword = text
				}
			}
		}
	})
	return id, err
}
func (f *externalControlsFake) SetExternalKeywordBid(_ context.Context, _, _ string, k yandexdirect.ExternalKeyword, search, network *string) error {
	return f.mutate(func() {
		for i := range f.detail.Controls.Groups {
			for j := range f.detail.Controls.Groups[i].Keywords {
				out := &f.detail.Controls.Groups[i].Keywords[j]
				if out.ID == k.ID {
					if search != nil {
						v := *search
						out.SearchBidMicros = &v
					}
					if network != nil {
						v := *network
						out.NetworkBidMicros = &v
					}
				}
			}
		}
	})
}
func (f *externalControlsFake) SetExternalCampaignState(_ context.Context, _, _ string, _ int64, action string) error {
	return f.mutate(func() {
		c := f.detail.Controls
		c.LifecycleState = "SUSPENDED"
		c.StateActions.CanPause = false
		c.StateActions.CanResume = true
		if action == "resume" {
			c.LifecycleState = "ON"
			c.StateActions.CanPause = true
			c.StateActions.CanResume = false
		}
		f.detail.Campaign.State = c.LifecycleState
	})
}
func (f *externalControlsFake) AddExternalGroup(_ context.Context, _, _ string, _ int64, _ string, p yandexdirect.ExternalGroupChanges) (int64, error) {
	id := f.nextID
	err := f.mutate(func() {
		g := yandexdirect.ExternalGroup{ID: id, Name: *p.Name, Type: "TEXT_AD_GROUP", RegionIDs: append([]int64{}, (*p.RegionIDs)...), NegativeKeywords: []string{}, Keywords: []yandexdirect.ExternalKeyword{}, EditableFields: []string{"name", "region_ids", "negative_keywords"}, CanCreateKeyword: true}
		if p.NegativeKeywords != nil {
			g.NegativeKeywords = append(g.NegativeKeywords, (*p.NegativeKeywords)...)
		}
		f.detail.Controls.Groups = append(f.detail.Controls.Groups, g)
	})
	return id, err
}
func (f *externalControlsFake) AddExternalKeyword(_ context.Context, _, _ string, group int64, text string) (int64, error) {
	id := f.nextID
	err := f.mutate(func() {
		for i := range f.detail.Controls.Groups {
			if f.detail.Controls.Groups[i].ID == group {
				f.detail.Controls.Groups[i].Keywords = append(f.detail.Controls.Groups[i].Keywords, yandexdirect.ExternalKeyword{ID: id, AdGroupID: group, Keyword: text, State: "ON", EditableFields: []string{"keyword", "search_bid_micros"}})
			}
		}
	})
	return id, err
}
func externalControlsFixture(t *testing.T) (*App, *externalControlsFake, string, store.Workspace, store.DirectExternalEditFence) {
	t.Helper()
	a, base, owner, ws, fence := externalEditFixture(t)
	bid := "800000"
	base.detail.Controls = &yandexdirect.ExternalCampaignControls{CampaignType: "TEXT_CAMPAIGN", CurrencyCode: "RUB", LifecycleState: "ON", SearchStrategy: "HIGHEST_POSITION", NetworkStrategy: "WB_MAXIMUM_CLICKS", MinimumBidMicros: "300000", MaximumBidMicros: "25000000000", BidIncrementMicros: "100000", Regions: []yandexdirect.ExternalRegion{{ID: 1, Name: "Москва"}, {ID: 219, Name: "Черноголовка"}}, Budgets: []yandexdirect.ExternalBudget{{Key: "network_weekly", AmountMicros: "700000000", StrategyType: "WB_MAXIMUM_CLICKS", MinimumMicros: "300000000", IncrementMicros: "10000", Editable: true, Branch: "WbMaximumClicks"}}, Groups: []yandexdirect.ExternalGroup{{ID: 13, Name: "Группа", Type: "TEXT_AD_GROUP", RegionIDs: []int64{1}, NegativeKeywords: []string{}, EditableFields: []string{"name", "region_ids", "negative_keywords"}, CanCreateKeyword: true, Keywords: []yandexdirect.ExternalKeyword{{ID: 14, AdGroupID: 13, Keyword: "курс английского", State: "ON", SearchBidMicros: &bid, EditableFields: []string{"keyword", "search_bid_micros"}}}}}, CanCreateGroup: true, StateActions: yandexdirect.ExternalStateActions{CanPause: true}, Spend: yandexdirect.ExternalSpend{Reason: "not_requested"}}
	f := &externalControlsFake{externalEditingFake: base, nextID: 99}
	a.direct = f
	d, err := a.GetDirectExternalDetail(t.Context(), owner, ws.ID, fence.ConnectionID, 77)
	if err != nil {
		t.Fatal(err)
	}
	return a, f, owner, ws, store.DirectExternalEditFence{ConnectionID: fence.ConnectionID, Version: d.Control.Version, ObservedHash: d.Control.ObservedHash, RevisionID: d.Control.RevisionID}
}
func controlRequest(f store.DirectExternalEditFence, kind string) DirectExternalControlMutation {
	return DirectExternalControlMutation{DirectExternalEditFence: f, ClientRequestID: "11111111-1111-4111-8111-111111111111", Kind: kind}
}
func TestExternalControlBudgetReadbackIdempotencyAndPayloadConflict(t *testing.T) {
	t.Parallel()
	a, f, owner, ws, fence := externalControlsFixture(t)
	r := controlRequest(fence, "budget")
	r.BudgetKey = "network_weekly"
	r.AmountMicros = "900000000"
	r.CurrencyCode = "RUB"
	d, err := a.ApplyDirectExternalControl(t.Context(), owner, ws.ID, 77, r)
	if err != nil {
		t.Fatal(err)
	}
	if d.Control.EditState != "idle" || d.Provider.Controls.Budgets[0].AmountMicros != "900000000" || f.writes != 1 || len(d.Operations) != 1 || d.Operations[0].State != "succeeded" {
		t.Fatalf("unverified budget %+v writes%d", d, f.writes)
	}
	if _, err = a.ApplyDirectExternalControl(t.Context(), owner, ws.ID, 77, r); err != nil || f.writes != 1 {
		t.Fatal("idempotent retry changed budget", err)
	}
	r.AmountMicros = "800000000"
	if _, err = a.ApplyDirectExternalControl(t.Context(), owner, ws.ID, 77, r); !errors.Is(err, store.ErrConflict) || f.writes != 1 {
		t.Fatal("UUID payload changed", err)
	}
}
func TestExternalControlValidatesAmountStrategyTargetingAndSpendConsent(t *testing.T) {
	t.Parallel()
	a, f, owner, ws, fence := externalControlsFixture(t)
	cases := []DirectExternalControlMutation{}
	r := controlRequest(fence, "budget")
	r.BudgetKey = "network_weekly"
	r.AmountMicros = "900000001"
	r.CurrencyCode = "RUB"
	cases = append(cases, r)
	r = controlRequest(fence, "bid")
	r.TargetID = 14
	r.CurrencyCode = "RUB"
	amount := "900000"
	r.NetworkBidMicros = &amount
	cases = append(cases, r)
	r = controlRequest(fence, "state")
	r.Action = "resume"
	cases = append(cases, r)
	r = controlRequest(fence, "group")
	r.TargetID = 13
	regions := []int64{99999}
	r.Group.RegionIDs = &regions
	cases = append(cases, r)
	r = controlRequest(fence, "group")
	r.TargetID = 999
	r.Group.Name = &amount
	cases = append(cases, r)
	for _, r := range cases {
		if _, err := a.ApplyDirectExternalControl(t.Context(), owner, ws.ID, 77, r); err == nil {
			t.Fatalf("invalid %s accepted", r.Kind)
		}
	}
	if f.writes != 0 {
		t.Fatal("invalid input reached Direct")
	}
	r = controlRequest(fence, "state")
	r.Action = "pause"
	d, err := a.ApplyDirectExternalControl(t.Context(), owner, ws.ID, 77, r)
	if err != nil || d.Provider.Campaign.State != "SUSPENDED" {
		t.Fatal("pause failed", err)
	}
	r = controlRequest(store.DirectExternalEditFence{ConnectionID: fence.ConnectionID, Version: d.Control.Version, ObservedHash: d.Control.ObservedHash, RevisionID: d.Control.RevisionID}, "state")
	r.ClientRequestID = "22222222-2222-4222-8222-222222222222"
	r.Action = "resume"
	if _, err = a.ApplyDirectExternalControl(t.Context(), owner, ws.ID, 77, r); err == nil || f.writes != 1 {
		t.Fatal("resume lacked explicit consent", err)
	}
	r.ConfirmSpend = true
	d, err = a.ApplyDirectExternalControl(t.Context(), owner, ws.ID, 77, r)
	if err != nil || d.Provider.Campaign.State != "ON" || f.writes != 2 {
		t.Fatal("explicit resume failed", err)
	}
}
func TestExternalControlKeywordReplacementIDAcknowledgedAndNotReplayed(t *testing.T) {
	t.Parallel()
	a, f, owner, ws, fence := externalControlsFixture(t)
	r := controlRequest(fence, "keyword")
	r.TargetID = 14
	r.Keyword = "английский онлайн"
	d, err := a.ApplyDirectExternalControl(t.Context(), owner, ws.ID, 77, r)
	if err != nil {
		t.Fatal(err)
	}
	if d.Control.EditState != "idle" || d.Operations[0].ProviderResultID == nil || *d.Operations[0].ProviderResultID != 99 || d.Provider.Controls.Groups[0].Keywords[0].ID != 99 {
		t.Fatal("replacement ID was lost")
	}
	if _, err = a.ApplyDirectExternalControl(t.Context(), owner, ws.ID, 77, r); err != nil || f.writes != 1 {
		t.Fatal("keyword replacement retried", err)
	}
}
func TestExternalControlCreateACKSurvivesFailedReadbackAndResolvesWithoutWrite(t *testing.T) {
	t.Parallel()
	a, f, owner, ws, fence := externalControlsFixture(t)
	r := controlRequest(fence, "create_keyword")
	r.TargetID = 13
	r.Keyword = "английский онлайн"
	f.afterWrite = func() { f.readErr = context.DeadlineExceeded }
	if _, err := a.ApplyDirectExternalControl(t.Context(), owner, ws.ID, 77, r); !errors.Is(err, store.ErrDirectProviderOperationBusy) {
		t.Fatal("failed readback accepted", err)
	}
	o, err := a.store.GetDirectExternalOperation(t.Context(), owner, ws.ID, fence.ConnectionID, 77, r.ClientRequestID)
	if err != nil || o.ProviderResultID == nil || *o.ProviderResultID != 99 || o.State != "uncertain" {
		t.Fatal("ack lost", err)
	}
	f.readErr = nil
	f.afterWrite = nil
	d, err := a.GetDirectExternalDetail(t.Context(), owner, ws.ID, fence.ConnectionID, 77)
	if err != nil || d.Control.EditState != "idle" || d.Operations[0].State != "succeeded" || f.writes != 1 {
		t.Fatal("readback failed to reconcile", err)
	}
}
func TestExternalControlUnknownCreateNeverReplayedAndExplicitScopedReadReconciles(t *testing.T) {
	t.Parallel()
	a, f, owner, ws, fence := externalControlsFixture(t)
	r := controlRequest(fence, "create_keyword")
	r.TargetID = 13
	r.Keyword = "английский онлайн"
	f.writeErr = context.DeadlineExceeded
	f.applyBeforeError = true
	if _, err := a.ApplyDirectExternalControl(t.Context(), owner, ws.ID, 77, r); err == nil {
		t.Fatal("ambiguous creation accepted")
	}
	d, err := a.GetDirectExternalDetail(t.Context(), owner, ws.ID, fence.ConnectionID, 77)
	if err != nil || d.Control.EditState != "uncertain" || d.Operations[0].ProviderResultID != nil {
		t.Fatal("unknown creation unlocked", err)
	}
	if _, err = a.ApplyDirectExternalControl(t.Context(), owner, ws.ID, 77, r); !errors.Is(err, store.ErrDirectProviderOperationBusy) || f.writes != 1 {
		t.Fatal("unknown creation replayed", err)
	}
	current := store.DirectExternalEditFence{ConnectionID: fence.ConnectionID, Version: d.Control.Version, ObservedHash: d.Control.ObservedHash, RevisionID: d.Control.RevisionID}
	if _, err = a.ReconcileDirectExternalCreation(t.Context(), owner, ws.ID, 77, current, r.ClientRequestID, 14); !errors.Is(err, store.ErrConflict) {
		t.Fatal("unrelated keyword reconciled", err)
	}
	d, err = a.ReconcileDirectExternalCreation(t.Context(), owner, ws.ID, 77, current, r.ClientRequestID, 99)
	if err != nil || d.Control.EditState != "idle" || d.Operations[0].State != "succeeded" || f.writes != 1 {
		t.Fatal("explicit reconciliation failed", err)
	}
}

func TestExternalAcknowledgmentAcceptsProviderDuplicateOnlyInsideOriginalGroup(t *testing.T) {
	detail := yandexdirect.ExternalCampaignDetail{Controls: &yandexdirect.ExternalCampaignControls{Groups: []yandexdirect.ExternalGroup{{ID: 13, Keywords: []yandexdirect.ExternalKeyword{{ID: 99, AdGroupID: 13, Keyword: "курсы английского"}}}}}}
	request := DirectExternalControlMutation{Kind: "keyword", TargetID: 14, ExpectedGroupID: 13, Keyword: "курс английского"}
	if matchesExternalCreation(detail, request, 99) {
		t.Fatal("unknown outcome guessed a grammatical duplicate")
	}
	if !matchesExternalAcknowledgment(detail, request, 99) {
		t.Fatal("known provider duplicate ID rejected")
	}
	detail.Controls.Groups[0].Keywords[0].AdGroupID = 999
	if matchesExternalAcknowledgment(detail, request, 99) {
		t.Fatal("provider acknowledgement crossed group scope")
	}
}
func TestExternalControlRejectedUUIDDoesNotBecomeFalseSuccessOnRetry(t *testing.T) {
	t.Parallel()
	a, f, owner, ws, fence := externalControlsFixture(t)
	r := controlRequest(fence, "budget")
	r.BudgetKey = "network_weekly"
	r.AmountMicros = "900000000"
	r.CurrencyCode = "RUB"
	f.writeErr = &yandexdirect.PartialMutationError{Operation: "campaigns.update", Results: []yandexdirect.MutationResult{{Errors: []yandexdirect.ProviderIssue{{Code: 8000, Message: "rejected"}}}}}
	if _, err := a.ApplyDirectExternalControl(t.Context(), owner, ws.ID, 77, r); err == nil {
		t.Fatal("rejection accepted")
	}
	if _, err := a.ApplyDirectExternalControl(t.Context(), owner, ws.ID, 77, r); !errors.Is(err, store.ErrDirectValidation) || f.writes != 1 {
		t.Fatal("rejected mutation became success or replayed", err)
	}
}

func TestExternalCreationReadbackTreatsGeographyAndNegativePhrasesAsSets(t *testing.T) {
	name := "Новая группа"
	regions := []int64{1, -219}
	phrases := []string{"дешево", "бесплатно"}
	request := DirectExternalControlMutation{Kind: "create_group", Group: yandexdirect.ExternalGroupChanges{Name: &name, RegionIDs: &regions, NegativeKeywords: &phrases}}
	detail := yandexdirect.ExternalCampaignDetail{Controls: &yandexdirect.ExternalCampaignControls{Groups: []yandexdirect.ExternalGroup{{ID: 99, Name: name, RegionIDs: []int64{-219, 1}, NegativeKeywords: []string{"бесплатно", "дешево"}}}}}
	if !matchesExternalCreation(detail, request, 99) {
		t.Fatal("provider set ordering stranded accepted creation")
	}
	detail.Controls.Groups[0].RegionIDs = []int64{999}
	if matchesExternalCreation(detail, request, 99) {
		t.Fatal("different geography accepted as creation readback")
	}
}

func TestExternalControlPreservesExistingUnknownGeographyWithoutAddingUnknownTargets(t *testing.T) {
	name := "Переименованная группа"
	detail := yandexdirect.ExternalCampaignDetail{Controls: &yandexdirect.ExternalCampaignControls{
		Regions:        []yandexdirect.ExternalRegion{{ID: 1, Name: "Москва"}, {ID: 219, Name: "Черноголовка"}},
		CanCreateGroup: true,
		Groups:         []yandexdirect.ExternalGroup{{ID: 13, Name: "Группа", RegionIDs: []int64{1, -99}, EditableFields: []string{"name", "region_ids", "negative_keywords"}}},
	}}
	regions := []int64{1, -99, -219}
	request := DirectExternalControlMutation{Kind: "group", TargetID: 13, Group: yandexdirect.ExternalGroupChanges{Name: &name, RegionIDs: &regions}}
	updated, err := mergeExternalControls(detail, request)
	if err != nil || updated.Controls.Groups[0].Name != name || !equalExternalSet(updated.Controls.Groups[0].RegionIDs, regions) {
		t.Fatal("existing excluded region blocked a valid edit", err)
	}
	if detail.Controls.Groups[0].Name != "Группа" || !equalExternalSet(detail.Controls.Groups[0].RegionIDs, []int64{1, -99}) {
		t.Fatal("validation mutated the provider snapshot")
	}
	for _, regions := range [][]int64{{1, -100}, {1, 99}} {
		request.Group.RegionIDs = &regions
		if _, err := mergeExternalControls(detail, request); err == nil {
			t.Fatal("new unknown region or changed sign accepted", regions)
		}
	}
	regions = []int64{1, -99}
	request.Kind = "create_group"
	request.Group.RegionIDs = &regions
	if _, err := mergeExternalControls(detail, request); err == nil {
		t.Fatal("unknown geography accepted for a new group")
	}
	regions = []int64{0}
	request.Group.RegionIDs = &regions
	if _, err := mergeExternalControls(detail, request); err != nil {
		t.Fatal("worldwide geography rejected", err)
	}
	request.Group.RegionIDs = nil
	request.Kind = "group"
	detail.Controls.Groups[0].NegativeKeywords = make([]string, 300)
	for i := range detail.Controls.Groups[0].NegativeKeywords {
		detail.Controls.Groups[0].NegativeKeywords[i] = "я"
	}
	if _, err := mergeExternalControls(detail, request); err != nil {
		t.Fatal("unchanged provider phrase list blocked name-only edit", err)
	}
}

func TestExternalCreationCannotBeManuallyReconciledWhileProviderWriteIsRunning(t *testing.T) {
	t.Parallel()
	a, f, owner, ws, fence := externalControlsFixture(t)
	r := controlRequest(fence, "create_keyword")
	r.TargetID = 13
	r.Keyword = "английский онлайн"
	hash, payload, err := directExternalMutationHash(r)
	if err != nil {
		t.Fatal(err)
	}
	o, _, err := a.store.ClaimDirectExternalOperation(t.Context(), owner, ws.ID, 77, fence, r.ClientRequestID, r.Kind, hash, hash, payload, a.now())
	if err != nil {
		t.Fatal(err)
	}
	if err = a.store.BeginDirectExternalOperation(t.Context(), owner, ws.ID, fence.ConnectionID, 77, o.OperationID, r.Kind, a.now()); err != nil {
		t.Fatal(err)
	}
	f.detail.Controls.Groups[0].Keywords = append(f.detail.Controls.Groups[0].Keywords, yandexdirect.ExternalKeyword{ID: 99, AdGroupID: 13, Keyword: r.Keyword, EditableFields: []string{"keyword"}})
	d, err := a.GetDirectExternalDetail(t.Context(), owner, ws.ID, fence.ConnectionID, 77)
	if err != nil {
		t.Fatal(err)
	}
	current := store.DirectExternalEditFence{ConnectionID: fence.ConnectionID, Version: d.Control.Version, ObservedHash: d.Control.ObservedHash, RevisionID: d.Control.RevisionID}
	if _, err = a.ReconcileDirectExternalCreation(t.Context(), owner, ws.ID, 77, current, r.ClientRequestID, 99); !errors.Is(err, store.ErrConflict) {
		t.Fatal("running write reconciled early", err)
	}
	oldNow := a.now()
	a.now = func() time.Time { return oldNow.Add(3 * time.Minute) }
	d, err = a.GetDirectExternalDetail(t.Context(), owner, ws.ID, fence.ConnectionID, 77)
	if err != nil || d.Control.EditState != "uncertain" || d.Operations[0].State != "uncertain" {
		t.Fatal("crashed started write lost uncertainty", err)
	}
	if f.writes != 0 {
		t.Fatal("recovery replayed provider write")
	}
}
