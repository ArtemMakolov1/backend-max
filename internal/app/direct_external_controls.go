package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexdirect"
)

type DirectExternalControlsProvider interface {
	GetExternalCampaignControls(context.Context, string, string, int64) (yandexdirect.ExternalCampaignControls, error)
	UpdateExternalBudget(context.Context, string, string, int64, yandexdirect.ExternalCampaignControls, string, string, string) error
	UpdateExternalGroup(context.Context, string, string, yandexdirect.ExternalGroup, yandexdirect.ExternalGroupChanges) error
	UpdateExternalKeyword(context.Context, string, string, yandexdirect.ExternalKeyword, string) (int64, error)
	SetExternalKeywordBid(context.Context, string, string, yandexdirect.ExternalKeyword, *string, *string) error
	SetExternalCampaignState(context.Context, string, string, int64, string) error
	AddExternalGroup(context.Context, string, string, int64, string, yandexdirect.ExternalGroupChanges) (int64, error)
	AddExternalKeyword(context.Context, string, string, int64, string) (int64, error)
}
type DirectExternalControlMutation struct {
	store.DirectExternalEditFence
	ClientRequestID  string                            `json:"client_request_id"`
	Kind             string                            `json:"kind"`
	TargetID         int64                             `json:"target_id,omitempty,string"`
	BudgetKey        string                            `json:"budget_key,omitempty"`
	AmountMicros     string                            `json:"amount_micros,omitempty"`
	CurrencyCode     string                            `json:"currency_code,omitempty"`
	Group            yandexdirect.ExternalGroupChanges `json:"group,omitempty"`
	Keyword          string                            `json:"keyword,omitempty"`
	SearchBidMicros  *string                           `json:"search_bid_micros,omitempty"`
	NetworkBidMicros *string                           `json:"network_bid_micros,omitempty"`
	Action           string                            `json:"action,omitempty"`
	ConfirmSpend     bool                              `json:"confirm_spend,omitempty"`
	ExpectedGroupID  int64                             `json:"expected_group_id,omitempty,string"`
}

func (a *App) DirectExternalControlsConfigured() bool {
	_, ok := a.direct.(DirectExternalControlsProvider)
	return a.DirectExternalDetailsConfigured() && ok
}
func directExternalMutationHash(request DirectExternalControlMutation) (string, json.RawMessage, error) {
	copy := request
	copy.DirectExternalEditFence = store.DirectExternalEditFence{}
	copy.ClientRequestID = ""
	value, err := json.Marshal(copy)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:]), value, nil
}
func directControlsOwnerKind(kind string) bool {
	return kind == "budget" || kind == "bid" || kind == "state"
}

func (a *App) ApplyDirectExternalControl(ctx context.Context, actor, workspace string, campaign int64, request DirectExternalControlMutation) (result DirectExternalDetail, err error) {
	request.ExpectedGroupID = 0
	provider, ok := a.direct.(DirectExternalControlsProvider)
	if !ok || !a.DirectExternalEditConfigured() {
		return result, ErrDirectWritesDisabled
	}
	if !store.ValidDirectExternalRequestID(request.ClientRequestID) {
		return result, store.ErrDirectValidation
	}
	access, err := a.store.ResolveWorkspaceAccess(ctx, actor, workspace)
	if err != nil {
		return result, err
	}
	if directControlsOwnerKind(request.Kind) && access.Member.Role != store.WorkspaceRoleOwner {
		return result, store.ErrNotFound
	}
	if _, err = a.store.GetDirectExternalEditContext(ctx, actor, workspace, request.ConnectionID, campaign, true); err != nil {
		return result, err
	}
	hash, payload, err := directExternalMutationHash(request)
	if err != nil {
		return result, store.ErrDirectValidation
	}
	existing, existingErr := a.store.GetDirectExternalOperation(ctx, actor, workspace, request.ConnectionID, campaign, request.ClientRequestID)
	if existingErr == nil {
		if existing.RequestHash != hash {
			return result, store.ErrConflict
		}
		if existing.State != "succeeded" && existing.State != "rejected" {
			return result, store.ErrDirectProviderOperationBusy
		}
		if existing.State == "rejected" {
			return result, fmt.Errorf("%w: the operation was rejected; refresh and use a new request identifier", store.ErrDirectValidation)
		}
		return a.GetDirectExternalDetail(ctx, actor, workspace, request.ConnectionID, campaign)
	}
	if !errors.Is(existingErr, store.ErrNotFound) {
		return result, existingErr
	}
	current, err := a.GetDirectExternalDetail(ctx, actor, workspace, request.ConnectionID, campaign)
	if err != nil {
		return current, err
	}
	if err = externalDetailMatches(current, request.DirectExternalEditFence); err != nil {
		return current, err
	}
	desired, err := mergeExternalControls(current.Provider, request)
	if err != nil {
		return current, fmt.Errorf("%w: %w", store.ErrDirectValidation, err)
	}
	desiredHash, err := desired.Fingerprint()
	if err != nil {
		return current, ErrDirectProvider
	}
	creating := request.Kind == "create_group" || request.Kind == "create_keyword"
	if request.Kind == "keyword" {
		keyword, findErr := externalKeyword(current.Provider.Controls, request.TargetID)
		if findErr != nil {
			return current, findErr
		}
		request.ExpectedGroupID = keyword.AdGroupID
		// Keep the user request hash stable, while retaining the server-owned
		// original group needed to verify a provider-assigned replacement ID.
		_, payload, err = directExternalMutationHash(request)
		if err != nil {
			return current, store.ErrDirectValidation
		}
	}
	if creating || request.Kind == "keyword" {
		desiredHash = hash
	} else if desiredHash == current.Control.ObservedHash {
		return current, nil
	}
	op, isNew, err := a.store.ClaimDirectExternalOperation(ctx, actor, workspace, campaign, request.DirectExternalEditFence, request.ClientRequestID, request.Kind, hash, desiredHash, payload, a.now().UTC())
	if err != nil {
		return current, err
	}
	if !isNew {
		if op.State == "rejected" {
			return current, store.ErrDirectValidation
		}
		if op.State == "succeeded" {
			return a.GetDirectExternalDetail(ctx, actor, workspace, request.ConnectionID, campaign)
		}
		return current, store.ErrDirectProviderOperationBusy
	}
	state := "rejected"
	defer func() {
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		finishErr := a.store.FinishDirectExternalOperation(finishCtx, workspace, request.ConnectionID, campaign, op.OperationID, state, a.now().UTC())
		if finishErr != nil {
			a.logger.Error("could not finalize external Direct operation", "operation_id", op.OperationID, "error", finishErr)
			if state != "rejected" {
				err = store.ErrDirectProviderOperationBusy
			}
		}
	}()
	token, err := a.directAccessToken(ctx, current.Connection)
	if err != nil {
		return current, err
	}
	if err = a.store.BeginDirectExternalOperation(ctx, actor, workspace, request.ConnectionID, campaign, op.OperationID, request.Kind, a.now().UTC()); err != nil {
		return current, err
	}
	state = "uncertain"
	writeCtx, cancelWrite := context.WithTimeout(ctx, 90*time.Second)
	resultID, writeErr := writeExternalControls(writeCtx, provider, token, current.Connection.ClientLogin, campaign, current.Provider, request)
	cancelWrite()
	if writeErr != nil {
		var rejected *yandexdirect.PartialMutationError
		if errors.As(writeErr, &rejected) {
			state = "rejected"
		}
		return current, a.directGraphProviderError(ctx, current.Connection, writeErr)
	}
	if creating || request.Kind == "keyword" {
		ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		ackErr := a.store.AcknowledgeDirectExternalOperation(ackCtx, op.OperationID, resultID, a.now().UTC())
		cancel()
		if ackErr != nil {
			return current, store.ErrDirectProviderOperationBusy
		}
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	verified, readErr := a.GetDirectExternalDetail(readCtx, actor, workspace, request.ConnectionID, campaign)
	if readErr != nil {
		return current, store.ErrDirectProviderOperationBusy
	}
	if creating || request.Kind == "keyword" {
		if !matchesExternalAcknowledgment(verified.Provider, request, resultID) {
			return verified, store.ErrDirectProviderOperationBusy
		}
	} else {
		observed, _ := verified.Provider.Fingerprint()
		if observed != desiredHash {
			return verified, store.ErrDirectProviderOperationBusy
		}
	}
	state = "succeeded"
	if err = a.store.FinishDirectExternalOperation(readCtx, workspace, request.ConnectionID, campaign, op.OperationID, state, a.now().UTC()); err != nil {
		return verified, store.ErrDirectProviderOperationBusy
	}
	final, viewErr := a.GetDirectExternalDetail(readCtx, actor, workspace, request.ConnectionID, campaign)
	if viewErr != nil {
		return verified, store.ErrDirectProviderOperationBusy
	}
	return final, nil
}

func mergeExternalControls(detail yandexdirect.ExternalCampaignDetail, request DirectExternalControlMutation) (yandexdirect.ExternalCampaignDetail, error) {
	encoded, _ := json.Marshal(detail)
	var desired yandexdirect.ExternalCampaignDetail
	if json.Unmarshal(encoded, &desired) != nil || desired.Controls == nil {
		return detail, errors.New("external controls are unavailable")
	}
	c := desired.Controls
	if detail.Campaign.State == "ARCHIVED" {
		return detail, errors.New("archived campaigns are read-only")
	}
	switch request.Kind {
	case "budget":
		if request.CurrencyCode != c.CurrencyCode {
			return detail, errors.New("currency mismatch")
		}
		found := false
		for i, budget := range c.Budgets {
			if budget.Key != request.BudgetKey {
				continue
			}
			max := ""
			if budget.MaximumMicros != nil {
				max = *budget.MaximumMicros
			}
			if !budget.Editable {
				return detail, errors.New("budget is read-only for this strategy")
			}
			if err := yandexdirect.ValidateExternalAmount(request.AmountMicros, budget.MinimumMicros, max, budget.IncrementMicros); err != nil {
				return detail, err
			}
			c.Budgets[i].AmountMicros = request.AmountMicros
			found = true
		}
		if !found {
			return detail, errors.New("budget slot is unavailable")
		}
	case "group", "create_group":
		if err := request.Group.Validate(); err != nil {
			return detail, err
		}
		var group *yandexdirect.ExternalGroup
		if request.Kind == "group" {
			var err error
			group, err = externalGroup(c, request.TargetID)
			if err != nil {
				return detail, err
			}
		}
		if request.Group.RegionIDs != nil {
			known := map[int64]bool{0: true}
			for _, region := range c.Regions {
				known[region.ID] = true
			}
			preserved := map[int64]bool{}
			if group != nil {
				for _, id := range group.RegionIDs {
					preserved[id] = true
				}
			}
			for _, id := range *request.Group.RegionIDs {
				if preserved[id] {
					continue
				}
				if id < 0 {
					id = -id
				}
				if !known[id] {
					return detail, errors.New("region is unavailable in the provider dictionary")
				}
			}
		}
		if request.Kind == "create_group" {
			if !c.CanCreateGroup || request.Group.Name == nil || request.Group.RegionIDs == nil {
				return detail, errors.New("new group requires name and regions")
			}
			return desired, nil
		}
		if group.ReadOnlyReason != nil {
			return detail, errors.New("group is read-only")
		}
		for field, requested := range map[string]bool{"name": request.Group.Name != nil, "region_ids": request.Group.RegionIDs != nil, "negative_keywords": request.Group.NegativeKeywords != nil} {
			if requested && !slices.Contains(group.EditableFields, field) {
				return detail, errors.New("group field is read-only")
			}
		}
		if request.Group.Name != nil {
			group.Name = *request.Group.Name
		}
		if request.Group.RegionIDs != nil {
			group.RegionIDs = append([]int64(nil), (*request.Group.RegionIDs)...)
			slices.Sort(group.RegionIDs)
		}
		if request.Group.NegativeKeywords != nil {
			group.NegativeKeywords = append([]string{}, (*request.Group.NegativeKeywords)...)
			slices.Sort(group.NegativeKeywords)
		}
	case "keyword", "create_keyword", "bid":
		if request.Kind == "create_keyword" {
			group, err := externalGroup(c, request.TargetID)
			if err != nil {
				return detail, err
			}
			if !group.CanCreateKeyword {
				return detail, errors.New("keyword creation is unavailable for this group")
			}
			if err = yandexdirect.ValidateExternalKeyword(request.Keyword); err != nil {
				return detail, err
			}
			return desired, nil
		}
		keyword, err := externalKeyword(c, request.TargetID)
		if err != nil {
			return detail, err
		}
		if request.Kind == "keyword" {
			if !slices.Contains(keyword.EditableFields, "keyword") {
				return detail, errors.New("keyword is read-only")
			}
			if err = yandexdirect.ValidateExternalKeyword(request.Keyword); err != nil {
				return detail, err
			}
			keyword.Keyword = request.Keyword
			break
		}
		if request.CurrencyCode != c.CurrencyCode {
			return detail, errors.New("currency mismatch")
		}
		if request.SearchBidMicros == nil && request.NetworkBidMicros == nil {
			return detail, errors.New("at least one bid is required")
		}
		for name, amount := range map[string]*string{"search_bid_micros": request.SearchBidMicros, "network_bid_micros": request.NetworkBidMicros} {
			if amount == nil {
				continue
			}
			if !slices.Contains(keyword.EditableFields, name) {
				return detail, errors.New("manual bid is unavailable for this strategy")
			}
			if err = yandexdirect.ValidateExternalAmount(*amount, c.MinimumBidMicros, c.MaximumBidMicros, c.BidIncrementMicros); err != nil {
				return detail, err
			}
			if name == "search_bid_micros" {
				keyword.SearchBidMicros = amount
			} else {
				keyword.NetworkBidMicros = amount
			}
		}
	case "state":
		if request.Action == "pause" && c.StateActions.CanPause {
			c.LifecycleState = "SUSPENDED"
			c.StateActions.CanPause = false
			c.StateActions.CanResume = true
			c.StateActions.ReadOnlyReason = nil
			desired.Campaign.State = "SUSPENDED"
		} else if request.Action == "resume" && c.StateActions.CanResume && request.ConfirmSpend {
			c.LifecycleState = "ON"
			c.StateActions.CanPause = true
			c.StateActions.CanResume = false
			c.StateActions.ReadOnlyReason = nil
			desired.Campaign.State = "ON"
		} else {
			return detail, errors.New("resume requires current paused campaign and explicit spend consent")
		}
	default:
		return detail, errors.New("unsupported operation")
	}
	return desired, nil
}
func externalGroup(c *yandexdirect.ExternalCampaignControls, id int64) (*yandexdirect.ExternalGroup, error) {
	for i := range c.Groups {
		if c.Groups[i].ID == id {
			return &c.Groups[i], nil
		}
	}
	return nil, store.ErrNotFound
}
func externalKeyword(c *yandexdirect.ExternalCampaignControls, id int64) (*yandexdirect.ExternalKeyword, error) {
	for i := range c.Groups {
		for j := range c.Groups[i].Keywords {
			if c.Groups[i].Keywords[j].ID == id {
				return &c.Groups[i].Keywords[j], nil
			}
		}
	}
	return nil, store.ErrNotFound
}
func writeExternalControls(ctx context.Context, provider DirectExternalControlsProvider, token, login string, campaign int64, current yandexdirect.ExternalCampaignDetail, r DirectExternalControlMutation) (int64, error) {
	c := current.Controls
	if c == nil {
		return 0, store.ErrDirectValidation
	}
	switch r.Kind {
	case "budget":
		return campaign, provider.UpdateExternalBudget(ctx, token, login, campaign, *c, r.BudgetKey, r.AmountMicros, r.CurrencyCode)
	case "group":
		g, err := externalGroup(c, r.TargetID)
		if err != nil {
			return 0, err
		}
		return g.ID, provider.UpdateExternalGroup(ctx, token, login, *g, r.Group)
	case "keyword":
		k, err := externalKeyword(c, r.TargetID)
		if err != nil {
			return 0, err
		}
		return provider.UpdateExternalKeyword(ctx, token, login, *k, r.Keyword)
	case "bid":
		k, err := externalKeyword(c, r.TargetID)
		if err != nil {
			return 0, err
		}
		return k.ID, provider.SetExternalKeywordBid(ctx, token, login, *k, r.SearchBidMicros, r.NetworkBidMicros)
	case "state":
		return campaign, provider.SetExternalCampaignState(ctx, token, login, campaign, r.Action)
	case "create_group":
		return provider.AddExternalGroup(ctx, token, login, campaign, current.Campaign.Type, r.Group)
	case "create_keyword":
		return provider.AddExternalKeyword(ctx, token, login, r.TargetID, r.Keyword)
	}
	return 0, store.ErrDirectValidation
}
func matchesExternalCreation(detail yandexdirect.ExternalCampaignDetail, r DirectExternalControlMutation, id int64) bool {
	if detail.Controls == nil || id <= 0 {
		return false
	}
	if r.Kind == "create_group" {
		g, err := externalGroup(detail.Controls, id)
		if err != nil || r.Group.Name == nil || r.Group.RegionIDs == nil || g.Name != *r.Group.Name || !equalExternalSet(g.RegionIDs, *r.Group.RegionIDs) {
			return false
		}
		return r.Group.NegativeKeywords == nil || equalExternalSet(g.NegativeKeywords, *r.Group.NegativeKeywords)
	}
	if r.Kind == "create_keyword" {
		k, err := externalKeyword(detail.Controls, id)
		return err == nil && k.AdGroupID == r.TargetID && k.Keyword == r.Keyword
	}
	if r.Kind == "keyword" {
		k, err := externalKeyword(detail.Controls, id)
		return err == nil && r.ExpectedGroupID > 0 && k.AdGroupID == r.ExpectedGroupID && k.Keyword == r.Keyword
	}
	return false
}

func equalExternalSet[T ~int64 | ~string](a, b []T) bool {
	left, right := slices.Clone(a), slices.Clone(b)
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(left, right)
}

// A successful Keywords.add/update response may identify an existing
// duplicate with a different grammatical form. The provider acknowledgment
// is authoritative for that ID, but never for an ID in another group. An
// unknown outcome without acknowledgment still requires exact field matching
// in matchesExternalCreation before a user can reconcile it explicitly.
func matchesExternalAcknowledgment(detail yandexdirect.ExternalCampaignDetail, r DirectExternalControlMutation, id int64) bool {
	if matchesExternalCreation(detail, r, id) {
		return true
	}
	if detail.Controls == nil || id <= 0 {
		return false
	}
	group := r.TargetID
	if r.Kind == "keyword" {
		group = r.ExpectedGroupID
	} else if r.Kind != "create_keyword" {
		return false
	}
	k, err := externalKeyword(detail.Controls, id)
	return err == nil && group > 0 && k.AdGroupID == group && yandexdirect.ValidateExternalKeyword(k.Keyword) == nil
}

// Reconciliation reads the current authoritative snapshot. An unknown creation
// cannot be replayed; explicit selection binds it to one already-existing ID.
func (a *App) ReconcileDirectExternalCreation(ctx context.Context, actor, workspace string, campaign int64, fence store.DirectExternalEditFence, requestID string, resultID int64) (DirectExternalDetail, error) {
	operation, err := a.store.GetDirectExternalOperation(ctx, actor, workspace, fence.ConnectionID, campaign, requestID)
	if err != nil {
		return DirectExternalDetail{}, err
	}
	if operation.Kind != "create_group" && operation.Kind != "create_keyword" && operation.Kind != "keyword" || operation.State != "uncertain" || !operation.WriteStarted {
		return DirectExternalDetail{}, store.ErrConflict
	}
	current, err := a.GetDirectExternalDetail(ctx, actor, workspace, fence.ConnectionID, campaign)
	if err != nil {
		return current, err
	}
	if current.Connection.ID != fence.ConnectionID || current.Control.Version != fence.Version || current.Control.RevisionID != fence.RevisionID || current.Control.ObservedHash != fence.ObservedHash {
		return current, store.ErrConflict
	}
	if _, err = a.store.GetDirectExternalEditContext(ctx, actor, workspace, fence.ConnectionID, campaign, true); err != nil {
		return current, err
	}
	var request DirectExternalControlMutation
	if json.Unmarshal(operation.Payload, &request) != nil || !matchesExternalCreation(current.Provider, request, resultID) {
		return current, store.ErrConflict
	}
	if operation.ProviderResultID != nil && *operation.ProviderResultID != resultID {
		return current, store.ErrConflict
	}
	if err = a.store.AcknowledgeDirectExternalOperation(ctx, operation.OperationID, resultID, a.now().UTC()); err != nil {
		return current, err
	}
	if err = a.store.FinishDirectExternalOperation(ctx, workspace, fence.ConnectionID, campaign, operation.OperationID, "succeeded", a.now().UTC()); err != nil {
		return current, err
	}
	a.logger.Info("external Direct creation reconciled", "workspace_id", workspace, "campaign_id", strconv.FormatInt(campaign, 10), "operation_id", operation.OperationID)
	return a.GetDirectExternalDetail(ctx, actor, workspace, fence.ConnectionID, campaign)
}

func (a *App) reconcileExternalControlReadback(ctx context.Context, actor, workspace string, campaign int64, connection string, detail yandexdirect.ExternalCampaignDetail) error {
	operations, err := a.store.ListDirectExternalOperations(ctx, actor, workspace, connection, campaign)
	if err != nil {
		return err
	}
	hash, err := detail.Fingerprint()
	if err != nil {
		return err
	}
	for _, o := range operations {
		if o.State != "uncertain" && o.State != "updating" {
			continue
		}
		if !o.WriteStarted {
			if err = a.store.RejectExpiredUnstartedDirectExternalOperation(ctx, workspace, connection, campaign, o.OperationID, a.now().UTC()); err != nil {
				return err
			}
			continue
		}
		if o.State == "updating" && !o.UpdatedAt.Add(2*time.Minute).After(a.now().UTC()) {
			// A crashed worker may never have marked its started write uncertain.
			// Keep the lock, expose recovery, and never authorize a replay.
			if err = a.store.FinishDirectExternalOperation(ctx, workspace, connection, campaign, o.OperationID, "uncertain", a.now().UTC()); err != nil {
				return err
			}
		}
		matched := o.DesiredHash == hash
		if o.ProviderResultID != nil {
			var r DirectExternalControlMutation
			if json.Unmarshal(o.Payload, &r) == nil {
				matched = matched || matchesExternalAcknowledgment(detail, r, *o.ProviderResultID)
			}
		}
		if matched {
			if err = a.store.FinishDirectExternalOperation(ctx, workspace, connection, campaign, o.OperationID, "succeeded", a.now().UTC()); err != nil {
				return err
			}
		}
	}
	return nil
}
