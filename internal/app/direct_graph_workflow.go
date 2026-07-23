package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"

	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexdirect"
)

const (
	directGraphWorkflowLimit   = 24
	directGraphWorkflowTimeout = 8 * time.Minute
)

type directProviderEditBaseline struct {
	Graph            yandexdirect.CampaignGraph
	GraphHash        string
	BaseCampaign     store.DirectCampaign
	BaseRegionIDs    []int64
	DesiredRegionIDs []int64
}

type directProviderEditPrefixState struct {
	CampaignDesired bool
	AdGroupDesired  bool
	AdDesired       bool
	KeywordsDesired bool
}

func (a *App) submitDirectCampaignGraph(
	ctx context.Context, actorUserID, workspaceID, campaignID string,
	expectedVersion int64,
) (store.DirectCampaign, error) {
	if err := a.requireDirectGraphWrites(); err != nil {
		return store.DirectCampaign{}, err
	}
	current, err := a.store.GetDirectCampaign(ctx, actorUserID, workspaceID, campaignID)
	if err != nil {
		return store.DirectCampaign{}, err
	}
	if expectedVersion <= 0 || current.Version != expectedVersion {
		return store.DirectCampaign{}, store.ErrConflict
	}
	marker, err := randomDirectToken(24)
	if err != nil {
		return store.DirectCampaign{}, err
	}
	material, err := a.store.ClaimDirectCampaignGraphSubmission(
		ctx, actorUserID, workspaceID, campaignID, expectedVersion,
		marker, a.now().UTC(),
	)
	if err != nil {
		return store.DirectCampaign{}, err
	}
	if err := a.runDirectGraphOperation(ctx, material); err != nil {
		return store.DirectCampaign{}, err
	}
	return a.store.GetDirectCampaign(ctx, actorUserID, workspaceID, campaignID)
}

// UpdateDirectCampaign keeps local drafts local. Once a provider graph exists,
// the same PATCH becomes a durable provider-side edit that is allowed only for
// an exact, verified, currently OFF revision.
func (a *App) UpdateDirectCampaign(
	ctx context.Context, actorUserID, workspaceID, campaignID string,
	changes store.DirectCampaignChanges, expectedGraphHash, expectedRevisionID string,
) (store.DirectCampaign, error) {
	current, err := a.store.GetDirectCampaign(ctx, actorUserID, workspaceID, campaignID)
	if err != nil {
		return store.DirectCampaign{}, err
	}
	if current.Status == "draft" {
		return a.store.UpdateDirectCampaignDraft(
			ctx, actorUserID, workspaceID, campaignID, changes,
		)
	}
	if err := a.requireDirectGraphWrites(); err != nil {
		return store.DirectCampaign{}, err
	}
	expectedGraphHash = strings.TrimSpace(expectedGraphHash)
	expectedRevisionID = strings.TrimSpace(expectedRevisionID)
	if changes.ExpectedVersion <= 0 || current.Version != changes.ExpectedVersion ||
		current.ProviderGraphHash != expectedGraphHash ||
		current.ProviderRevisionID != expectedRevisionID {
		return store.DirectCampaign{}, store.ErrDirectConsentMismatch
	}

	if current.SubmissionOperationID == "" ||
		directProviderOperationTerminal(current.SubmissionStage) {
		if err = a.preflightDirectProviderEdit(ctx, current, expectedGraphHash); err != nil {
			return store.DirectCampaign{}, err
		}
	}
	marker, err := randomDirectToken(24)
	if err != nil {
		return store.DirectCampaign{}, err
	}
	material, err := a.store.ClaimDirectCampaignProviderEdit(
		ctx, actorUserID, workspaceID, campaignID, changes,
		expectedGraphHash, expectedRevisionID, marker, a.now().UTC(),
	)
	if err != nil {
		return store.DirectCampaign{}, err
	}
	if err := a.runDirectGraphOperation(ctx, material); err != nil {
		return store.DirectCampaign{}, err
	}
	return a.store.GetDirectCampaign(ctx, actorUserID, workspaceID, campaignID)
}

func (a *App) requireDirectGraphWrites() error {
	if !a.DirectWritesEnabled() {
		return ErrDirectWritesDisabled
	}
	if !a.directProviderGraphVerified || a.directGraph == nil {
		return ErrDirectGraphUnsupported
	}
	return nil
}

func (a *App) preflightDirectProviderEdit(
	ctx context.Context, campaign store.DirectCampaign, expectedHash string,
) error {
	if campaign.ProviderCampaignID == nil || campaign.ProviderAdGroupID == nil ||
		campaign.ProviderAdID == nil || campaign.SubmissionOperationMarker == "" {
		return store.ErrDirectGraphUnverified
	}
	connection, err := a.store.GetDirectLifecycleMaterial(
		ctx, campaign.WorkspaceID, campaign.ID,
	)
	if err != nil {
		return err
	}
	token, err := a.directAccessToken(ctx, connection.Connection)
	if err != nil {
		return err
	}
	regions, err := a.resolveDirectRegionIDs(
		ctx, token, connection.Connection, campaign.Regions,
	)
	if err != nil {
		return a.directGraphProviderError(ctx, connection.Connection, err)
	}
	graph, err := a.directGraph.GetCampaignGraph(
		ctx, token, connection.Connection.ClientLogin, *campaign.ProviderCampaignID,
	)
	if err != nil {
		return a.directGraphProviderError(ctx, connection.Connection, err)
	}
	verified, err := verifyDirectProviderGraph(
		graph, campaign, connection.Connection, campaign.SubmissionOperationMarker,
		regions, campaign.ProviderCampaignID, campaign.ProviderAdGroupID,
		campaign.ProviderAdID,
	)
	if err != nil || verified.GraphHash != expectedHash {
		return errors.Join(store.ErrDirectConsentMismatch, err)
	}
	return nil
}

func (a *App) runDirectGraphOperation(
	ctx context.Context, material store.DirectGraphSubmissionMaterial,
) error {
	ctx, cancel := context.WithTimeout(ctx, directGraphWorkflowTimeout)
	defer cancel()

	token, err := a.directAccessToken(ctx, material.Connection)
	if err != nil {
		return err
	}
	var (
		regionIDs    []int64
		editBaseline *directProviderEditBaseline
	)
	if material.Operation.OperationKind == "update" {
		var resolveErr error
		regionIDs, resolveErr = a.resolveDirectRegionIDs(
			ctx, token, material.Connection, material.DesiredCampaign.Regions,
		)
		if resolveErr != nil {
			return a.directGraphProviderError(
				ctx, material.Connection, resolveErr,
			)
		}
		baseline, baselineErr := a.loadDirectProviderEditBaseline(
			ctx, token, material, regionIDs,
		)
		if baselineErr != nil {
			return baselineErr
		}
		editBaseline = &baseline
	} else {
		regionIDs, err = a.resolveDirectRegionIDs(
			ctx, token, material.Connection, material.DesiredCampaign.Regions,
		)
		if err != nil {
			return a.directGraphProviderError(ctx, material.Connection, err)
		}
	}
	for attempts := 0; attempts < directGraphWorkflowLimit; attempts++ {
		switch material.Operation.Stage {
		case "claimed":
			if material.Operation.OperationKind == "submission" {
				material, err = a.directCreateOrReconcileCampaign(
					ctx, token, material,
				)
			} else {
				material, err = a.directUpdateOrReconcileCampaign(
					ctx, token, material, *editBaseline,
				)
			}
		case "campaign_created":
			material, err = a.directCreateOrReconcileAdGroup(
				ctx, token, material, regionIDs,
			)
		case "campaign_updated":
			material, err = a.directUpdateOrReconcileAdGroup(
				ctx, token, material, *editBaseline,
			)
		case "ad_group_created":
			material, err = a.directCreateOrReconcileAd(ctx, token, material)
		case "ad_group_updated":
			material, err = a.directUpdateOrReconcileAd(
				ctx, token, material, *editBaseline,
			)
		case "ad_created":
			material, err = a.directCreateOrReconcileKeywords(ctx, token, material)
		case "ad_updated":
			material, err = a.directUpdateOrReconcileKeywords(
				ctx, token, material, *editBaseline,
			)
		case "keywords_created", "keywords_updated":
			material, err = a.directObserveOperationGraph(
				ctx, token, material, regionIDs,
			)
		case "graph_observed":
			material, err = a.directRecordVerifiedGraph(
				ctx, token, material, regionIDs,
			)
		case "verified":
			material, err = a.directRequestModeration(
				ctx, token, material, regionIDs,
			)
		case "moderation_requested":
			material, err = a.store.AdvanceDirectCampaignGraphSubmission(
				ctx, material.Campaign.WorkspaceID, material.Campaign.ID,
				material.Operation.ID, "moderation_requested",
				store.DirectProviderStageUpdate{
					Stage: "completed", Complete: true,
					ExpectedClaimedAt: material.Operation.ClaimedAt,
				},
				a.now().UTC(),
			)
		case "completed":
			_ = a.syncDirectCampaignLifecycle(
				ctx, material.Campaign.WorkspaceID, material.Campaign.ID,
			)
			return nil
		case "reconciling":
			material, err = a.reconcileDirectGraphOperation(
				ctx, token, material, regionIDs, editBaseline,
			)
		case "failed":
			return store.ErrConflict
		default:
			return fmt.Errorf(
				"%w: unsupported Direct operation stage %q",
				store.ErrDirectValidation, material.Operation.Stage,
			)
		}
		if err != nil {
			return err
		}
	}
	return store.ErrDirectProviderOperationStale
}

func (a *App) loadDirectProviderEditBaseline(
	ctx context.Context, token string,
	material store.DirectGraphSubmissionMaterial,
	desiredRegionIDs []int64,
) (directProviderEditBaseline, error) {
	mismatch := func(field string) (directProviderEditBaseline, error) {
		return directProviderEditBaseline{}, fmt.Errorf(
			"%w: invalid immutable provider edit baseline (%s)",
			errDirectProviderGraphMismatch, field,
		)
	}
	operation := material.Operation
	if operation.OperationKind != "update" ||
		strings.TrimSpace(operation.ExpectedRevisionID) == "" ||
		strings.TrimSpace(operation.ExpectedGraphHash) == "" ||
		operation.ProviderCampaignID == nil ||
		operation.ProviderAdGroupID == nil ||
		operation.ProviderAdID == nil {
		return mismatch("operation")
	}
	revision, err := a.store.GetDirectCampaignRevision(
		ctx, material.Campaign.WorkspaceID, material.Campaign.ID,
		operation.ExpectedRevisionID,
	)
	if err != nil {
		return directProviderEditBaseline{}, err
	}
	if revision.ID != operation.ExpectedRevisionID ||
		revision.WorkspaceID != material.Campaign.WorkspaceID ||
		revision.CampaignID != material.Campaign.ID ||
		revision.ConnectionID != operation.ConnectionID ||
		revision.ConnectionID != material.Connection.ID ||
		revision.CampaignVersion != operation.ExpectedCampaignVersion ||
		revision.GraphVersion != store.DirectGraphFingerprintVersion ||
		revision.GraphHash != operation.ExpectedGraphHash ||
		revision.ProviderCampaignID != *operation.ProviderCampaignID ||
		revision.ProviderAdGroupID != *operation.ProviderAdGroupID ||
		revision.ProviderAdID != *operation.ProviderAdID {
		return mismatch("revision")
	}
	var graph yandexdirect.CampaignGraph
	if err = json.Unmarshal(revision.ObservedGraph, &graph); err != nil {
		return mismatch("observed graph")
	}
	baseCampaign, err := directCampaignFromImmutableDesiredGraph(
		material.Campaign, revision.DesiredGraph,
	)
	if err != nil {
		return mismatch("desired graph")
	}
	baseRegionIDs, err := a.resolveDirectRegionIDs(
		ctx, token, material.Connection, baseCampaign.Regions,
	)
	if err != nil {
		return directProviderEditBaseline{}, a.directGraphProviderError(
			ctx, material.Connection, err,
		)
	}
	hash, err := graph.Fingerprint()
	if err != nil || hash != operation.ExpectedGraphHash {
		return mismatch("graph hash")
	}
	baseMarker, err := directProviderOperationMarker(graph.Campaign.TrackingParams)
	if err != nil {
		return mismatch("operation marker")
	}
	verified, err := verifyDirectProviderGraph(
		graph, baseCampaign, material.Connection, baseMarker,
		baseRegionIDs, operation.ProviderCampaignID, operation.ProviderAdGroupID,
		operation.ProviderAdID,
	)
	if err != nil || verified.GraphHash != operation.ExpectedGraphHash {
		return mismatch("verified graph")
	}
	if !directProviderKeywordMappingsMatch(
		verified.KeywordMappings, revision.ProviderKeywordMappings,
	) || !directProviderKeywordMappingsMatch(
		verified.KeywordMappings, operation.ProviderKeywordMappings,
	) {
		return mismatch("keyword mappings")
	}
	return directProviderEditBaseline{
		Graph:            graph,
		GraphHash:        hash,
		BaseCampaign:     baseCampaign,
		BaseRegionIDs:    append([]int64(nil), baseRegionIDs...),
		DesiredRegionIDs: append([]int64(nil), desiredRegionIDs...),
	}, nil
}

func directCampaignFromImmutableDesiredGraph(
	campaign store.DirectCampaign, raw json.RawMessage,
) (store.DirectCampaign, error) {
	var desired store.DirectCampaignDesiredGraph
	if err := json.Unmarshal(raw, &desired); err != nil {
		return store.DirectCampaign{}, err
	}
	campaign.Name = desired.Name
	campaign.LandingURL = desired.LandingURL
	campaign.Regions = append([]string(nil), desired.Regions...)
	campaign.WeeklyBudgetMinor = desired.WeeklyBudgetMinor
	campaign.CurrencyCode = desired.CurrencyCode
	campaign.StartsAt = desired.StartsAt
	campaign.EndsAt = desired.EndsAt
	campaign.Titles = append([]string(nil), desired.Titles...)
	campaign.Texts = append([]string(nil), desired.Texts...)
	campaign.Keywords = append([]string(nil), desired.Keywords...)
	campaign.NegativeKeywords = append(
		[]string(nil), desired.NegativeKeywords...,
	)
	return campaign, nil
}

func directProviderOperationMarker(trackingParams string) (string, error) {
	values, err := url.ParseQuery(strings.TrimSpace(trackingParams))
	if err != nil || len(values) != 1 {
		return "", errDirectProviderGraphMismatch
	}
	markers, ok := values["mp_op"]
	if !ok || len(markers) != 1 ||
		strings.TrimSpace(markers[0]) == "" ||
		values.Encode() != strings.TrimSpace(trackingParams) {
		return "", errDirectProviderGraphMismatch
	}
	return strings.TrimSpace(markers[0]), nil
}

func directProviderKeywordMappingsMatch(
	left, right []store.DirectKeywordMapping,
) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Keyword != right[index].Keyword ||
			left[index].ProviderKeywordID != right[index].ProviderKeywordID {
			return false
		}
	}
	return true
}

func (a *App) reconcileDirectGraphOperation(
	ctx context.Context, token string, material store.DirectGraphSubmissionMaterial,
	regionIDs []int64, baseline *directProviderEditBaseline,
) (store.DirectGraphSubmissionMaterial, error) {
	campaignID := material.Operation.ProviderCampaignID
	if campaignID == nil {
		id, err := a.directGraph.FindUnifiedCampaignByOperationMarker(
			ctx, token, material.Connection.ClientLogin,
			material.Operation.OperationMarker,
		)
		if err != nil {
			return material, a.directGraphProviderError(ctx, material.Connection, err)
		}
		if id == 0 {
			return material, store.ErrDirectProviderOperationStale
		}
		campaignID = &id
	}
	graph, err := a.directGraph.GetCampaignGraph(
		ctx, token, material.Connection.ClientLogin, *campaignID,
	)
	if err != nil {
		return material, a.directGraphProviderError(ctx, material.Connection, err)
	}
	if !directCampaignNodeMatches(
		graph.Campaign, material.DesiredCampaign, material.Connection,
		material.Operation.OperationMarker,
	) {
		return material, store.ErrDirectProviderOperationStale
	}
	update := store.DirectProviderStageUpdate{ProviderCampaignID: campaignID}
	stage := ""
	if material.Operation.OperationKind == "submission" {
		stage = "campaign_created"
	} else {
		stage = "campaign_updated"
	}
	if len(graph.AdGroups) == 0 {
		return a.advanceDirectGraphStage(ctx, material, stage, update)
	}
	if len(graph.AdGroups) != 1 ||
		!directAdGroupNodeMatches(
			graph.AdGroups[0], material.DesiredCampaign, regionIDs,
			material.Operation.OperationMarker,
		) {
		return material, store.ErrDirectProviderOperationStale
	}
	groupID := graph.AdGroups[0].ID
	update.ProviderAdGroupID = &groupID
	if material.Operation.OperationKind == "submission" {
		stage = "ad_group_created"
	} else {
		stage = "ad_group_updated"
	}
	if len(graph.Ads) == 0 {
		return a.advanceDirectGraphStage(ctx, material, stage, update)
	}
	if len(graph.Ads) != 1 ||
		!directResponsiveAdMatches(
			graph.Ads[0], material.DesiredCampaign, groupID,
		) {
		return material, store.ErrDirectProviderOperationStale
	}
	adID := graph.Ads[0].ID
	update.ProviderAdID = &adID
	if material.Operation.OperationKind == "submission" {
		stage = "ad_created"
	} else {
		stage = "ad_updated"
	}
	mappings, missing, keywordErr := directSubmissionKeywordState(
		graph.Keywords, material.DesiredCampaign.Keywords, *campaignID, groupID,
	)
	if material.Operation.OperationKind == "update" &&
		(keywordErr != nil || len(missing) != 0) {
		if baseline == nil {
			return material, store.ErrDirectGraphUnverified
		}
		updates, deletes, adds, planErr := directKeywordEditPlan(
			graph.Keywords, baseline.BaseCampaign.Keywords,
			material.DesiredCampaign.Keywords, *campaignID, groupID,
		)
		if planErr != nil {
			return material, store.ErrDirectProviderOperationStale
		}
		if len(updates)+len(deletes)+len(adds) != 0 {
			return a.advanceDirectGraphStage(ctx, material, "ad_updated", update)
		}
	}
	if keywordErr != nil {
		return material, store.ErrDirectProviderOperationStale
	}
	if len(missing) != 0 {
		return a.advanceDirectGraphStage(ctx, material, stage, update)
	}
	update.ProviderKeywordMappings = &mappings
	if material.Operation.OperationKind == "submission" {
		stage = "keywords_created"
	} else {
		stage = "keywords_updated"
	}
	verified, verifyErr := verifyDirectProviderGraph(
		graph, material.DesiredCampaign, material.Connection,
		material.Operation.OperationMarker, regionIDs,
		campaignID, &groupID, &adID,
	)
	if verifyErr != nil {
		return material, store.ErrDirectProviderOperationStale
	}
	update.ObservedGraph = verified.ObservedGraph
	update.GraphHash = verified.GraphHash
	if material.Campaign.ProviderGraphHash == verified.GraphHash &&
		material.Campaign.ProviderRevisionID != "" &&
		material.Operation.GraphHash == verified.GraphHash &&
		material.Operation.RevisionID == material.Campaign.ProviderRevisionID {
		switch strings.ToUpper(strings.TrimSpace(graph.Ads[0].Status)) {
		case "MODERATION", "PREACCEPTED", "ACCEPTED", "REJECTED":
			stage = "moderation_requested"
			update.ObservedGraph = nil
			update.GraphHash = ""
		}
	}
	if stage != "moderation_requested" {
		stage = "graph_observed"
	}
	return a.advanceDirectGraphStage(ctx, material, stage, update)
}

func (a *App) directCreateOrReconcileCampaign(
	ctx context.Context, token string, material store.DirectGraphSubmissionMaterial,
) (store.DirectGraphSubmissionMaterial, error) {
	id, err := a.directGraph.FindUnifiedCampaignByOperationMarker(
		ctx, token, material.Connection.ClientLogin, material.Operation.OperationMarker,
	)
	warnings := append([]store.DirectProviderIssue(nil), material.Operation.ProviderWarnings...)
	if err == nil && id == 0 {
		var campaign yandexdirect.Campaign
		campaign, err = a.direct.CreateCampaignDraft(
			ctx, token, material.Connection.ClientLogin, yandexdirect.CampaignDraft{
				Name:              material.DesiredCampaign.Name,
				WeeklyBudgetMinor: material.DesiredCampaign.WeeklyBudgetMinor,
				StartsAt:          material.DesiredCampaign.StartsAt,
				EndsAt:            material.DesiredCampaign.EndsAt,
				TimeZone:          material.Connection.Timezone,
				OperationMarker:   material.Operation.OperationMarker,
			},
		)
		if err == nil {
			id = campaign.ID
			warnings = append(warnings, directProviderIssues(campaign.Warnings)...)
		}
		if err != nil {
			terminal, terminalErr := a.failDirectSubmissionIfProviderAbsent(
				ctx, token, material, err,
			)
			if terminal || terminalErr != nil {
				return material, errors.Join(
					a.directGraphProviderError(
						ctx, material.Connection, err,
					),
					terminalErr,
				)
			}
		}
	}
	if err != nil {
		return material, a.directGraphProviderError(ctx, material.Connection, err)
	}
	if id <= 0 {
		return material, errDirectProviderGraphMismatch
	}
	return a.advanceDirectGraphStage(
		ctx, material, "campaign_created",
		store.DirectProviderStageUpdate{
			ProviderCampaignID: &id, ProviderWarnings: &warnings,
		},
	)
}

func (a *App) directCreateOrReconcileAdGroup(
	ctx context.Context, token string, material store.DirectGraphSubmissionMaterial,
	regionIDs []int64,
) (store.DirectGraphSubmissionMaterial, error) {
	if material.Operation.ProviderCampaignID == nil {
		return material, store.ErrDirectGraphUnverified
	}
	id, err := a.directGraph.FindUnifiedAdGroupByTrackingMarker(
		ctx, token, material.Connection.ClientLogin,
		*material.Operation.ProviderCampaignID, material.Operation.OperationMarker,
	)
	warnings := append([]store.DirectProviderIssue(nil), material.Operation.ProviderWarnings...)
	if err == nil && id == 0 {
		var result yandexdirect.MutationResult
		result, err = a.directGraph.CreateUnifiedAdGroup(
			ctx, token, material.Connection.ClientLogin, yandexdirect.UnifiedAdGroupDraft{
				CampaignID: *material.Operation.ProviderCampaignID,
				Name: yandexdirect.UnifiedAdGroupOperationName(
					material.Operation.OperationMarker,
				),
				RegionIDs: regionIDs, NegativeKeywords: material.DesiredCampaign.NegativeKeywords,
				TrackingMarker: material.Operation.OperationMarker,
			},
		)
		warnings = append(warnings, directProviderIssues(result.Warnings)...)
		if err == nil {
			id = result.ID
		}
	}
	if err != nil {
		return material, a.directGraphProviderError(ctx, material.Connection, err)
	}
	if id <= 0 {
		return material, errDirectProviderGraphMismatch
	}
	return a.advanceDirectGraphStage(
		ctx, material, "ad_group_created",
		store.DirectProviderStageUpdate{
			ProviderAdGroupID: &id, ProviderWarnings: &warnings,
		},
	)
}

func (a *App) directCreateOrReconcileAd(
	ctx context.Context, token string, material store.DirectGraphSubmissionMaterial,
) (store.DirectGraphSubmissionMaterial, error) {
	if material.Operation.ProviderCampaignID == nil ||
		material.Operation.ProviderAdGroupID == nil {
		return material, store.ErrDirectGraphUnverified
	}
	ads, err := a.directGraph.ListResponsiveAds(
		ctx, token, material.Connection.ClientLogin,
		*material.Operation.ProviderCampaignID,
	)
	if err != nil {
		return material, a.directGraphProviderError(ctx, material.Connection, err)
	}
	var id int64
	warnings := append([]store.DirectProviderIssue(nil), material.Operation.ProviderWarnings...)
	switch len(ads) {
	case 0:
		result, addErr := a.directGraph.CreateResponsiveAd(
			ctx, token, material.Connection.ClientLogin, yandexdirect.ResponsiveAdDraft{
				AdGroupID: *material.Operation.ProviderAdGroupID,
				Titles:    material.DesiredCampaign.Titles,
				Texts:     material.DesiredCampaign.Texts,
				Href:      material.DesiredCampaign.LandingURL,
			},
		)
		warnings = append(warnings, directProviderIssues(result.Warnings)...)
		if addErr != nil {
			ads, err = a.directGraph.ListResponsiveAds(
				ctx, token, material.Connection.ClientLogin,
				*material.Operation.ProviderCampaignID,
			)
			if err != nil || len(ads) != 1 ||
				!directResponsiveAdMatches(ads[0], material.DesiredCampaign,
					*material.Operation.ProviderAdGroupID) {
				return material, a.directGraphProviderError(
					ctx, material.Connection, errors.Join(addErr, err),
				)
			}
			id = ads[0].ID
		} else {
			id = result.ID
		}
	case 1:
		if !directResponsiveAdMatches(
			ads[0], material.DesiredCampaign, *material.Operation.ProviderAdGroupID,
		) {
			return material, errDirectProviderGraphMismatch
		}
		id = ads[0].ID
	default:
		return material, errDirectProviderGraphMismatch
	}
	if id <= 0 {
		return material, errDirectProviderGraphMismatch
	}
	return a.advanceDirectGraphStage(
		ctx, material, "ad_created",
		store.DirectProviderStageUpdate{
			ProviderAdID: &id, ProviderWarnings: &warnings,
		},
	)
}

func (a *App) directCreateOrReconcileKeywords(
	ctx context.Context, token string, material store.DirectGraphSubmissionMaterial,
) (store.DirectGraphSubmissionMaterial, error) {
	if material.Operation.ProviderCampaignID == nil ||
		material.Operation.ProviderAdGroupID == nil {
		return material, store.ErrDirectGraphUnverified
	}
	keywords, err := a.directGraph.ListKeywords(
		ctx, token, material.Connection.ClientLogin,
		*material.Operation.ProviderCampaignID,
	)
	if err != nil {
		return material, a.directGraphProviderError(ctx, material.Connection, err)
	}
	mappings, missing, err := directSubmissionKeywordState(
		keywords, material.DesiredCampaign.Keywords,
		*material.Operation.ProviderCampaignID, *material.Operation.ProviderAdGroupID,
	)
	warnings := append([]store.DirectProviderIssue(nil), material.Operation.ProviderWarnings...)
	if err == nil && len(missing) != 0 {
		drafts := make([]yandexdirect.KeywordDraft, 0, len(missing))
		for _, keyword := range missing {
			drafts = append(drafts, yandexdirect.KeywordDraft{
				AdGroupID: *material.Operation.ProviderAdGroupID, Keyword: keyword,
			})
		}
		results, addErr := a.directGraph.AddKeywords(
			ctx, token, material.Connection.ClientLogin, drafts,
		)
		warnings = append(warnings, directMutationWarnings(results)...)
		keywords, err = a.directGraph.ListKeywords(
			ctx, token, material.Connection.ClientLogin,
			*material.Operation.ProviderCampaignID,
		)
		if err == nil {
			mappings, missing, err = directSubmissionKeywordState(
				keywords, material.DesiredCampaign.Keywords,
				*material.Operation.ProviderCampaignID,
				*material.Operation.ProviderAdGroupID,
			)
		}
		if err != nil || len(missing) != 0 {
			return material, a.directGraphProviderError(
				ctx, material.Connection,
				errors.Join(errDirectProviderGraphMismatch, addErr, err),
			)
		}
	}
	if err != nil || len(missing) != 0 {
		return material, errors.Join(errDirectProviderGraphMismatch, err)
	}
	return a.advanceDirectGraphStage(
		ctx, material, "keywords_created",
		store.DirectProviderStageUpdate{
			ProviderKeywordMappings: &mappings, ProviderWarnings: &warnings,
		},
	)
}

func (a *App) directUpdateOrReconcileCampaign(
	ctx context.Context, token string, material store.DirectGraphSubmissionMaterial,
	baseline directProviderEditBaseline,
) (store.DirectGraphSubmissionMaterial, error) {
	if material.Operation.ProviderCampaignID == nil {
		return material, store.ErrDirectGraphUnverified
	}
	graph, err := a.directGraph.GetCampaignGraph(
		ctx, token, material.Connection.ClientLogin,
		*material.Operation.ProviderCampaignID,
	)
	if err != nil {
		return material, a.directGraphProviderError(ctx, material.Connection, err)
	}
	state, err := directVerifyProviderEditPrefix(
		graph, material, baseline, "claimed",
	)
	if err != nil {
		return material, err
	}
	warnings := append([]store.DirectProviderIssue(nil), material.Operation.ProviderWarnings...)
	if !state.CampaignDesired {
		strategy, strategyErr := yandexdirect.SafeUnifiedCampaignBiddingStrategy(
			material.DesiredCampaign.WeeklyBudgetMinor,
		)
		if strategyErr != nil {
			return material, strategyErr
		}
		results, updateErr := a.directGraph.UpdateUnifiedCampaigns(
			ctx, token, material.Connection.ClientLogin,
			[]yandexdirect.UnifiedCampaignUpdate{{
				ID:                *material.Operation.ProviderCampaignID,
				Name:              material.DesiredCampaign.Name,
				WeeklyBudgetMinor: material.DesiredCampaign.WeeklyBudgetMinor,
				StartsAt:          material.DesiredCampaign.StartsAt,
				EndsAt:            material.DesiredCampaign.EndsAt,
				BiddingStrategy:   strategy,
				Settings:          yandexdirect.SafeUnifiedCampaignSettings(),
				TrackingParams: (url.Values{
					"mp_op": []string{material.Operation.OperationMarker},
				}).Encode(),
			}},
		)
		warnings = append(warnings, directMutationWarnings(results)...)
		graph, err = a.directGraph.GetCampaignGraph(
			ctx, token, material.Connection.ClientLogin,
			*material.Operation.ProviderCampaignID,
		)
		if err == nil {
			state, err = directVerifyProviderEditPrefix(
				graph, material, baseline, "claimed",
			)
		}
		if updateErr != nil && err == nil && !state.CampaignDesired {
			terminal, terminalErr := a.failDirectUpdateIfBaselineUnchanged(
				ctx, material, graph, updateErr,
			)
			if terminal || terminalErr != nil {
				return material, errors.Join(
					a.directGraphProviderError(
						ctx, material.Connection, updateErr,
					),
					terminalErr,
				)
			}
		}
		if err != nil || !state.CampaignDesired {
			return material, a.directGraphProviderError(
				ctx, material.Connection,
				errors.Join(errDirectProviderGraphMismatch, updateErr, err),
			)
		}
	}
	return a.advanceDirectGraphStage(
		ctx, material, "campaign_updated",
		store.DirectProviderStageUpdate{ProviderWarnings: &warnings},
	)
}

func (a *App) directUpdateOrReconcileAdGroup(
	ctx context.Context, token string, material store.DirectGraphSubmissionMaterial,
	baseline directProviderEditBaseline,
) (store.DirectGraphSubmissionMaterial, error) {
	if material.Operation.ProviderCampaignID == nil ||
		material.Operation.ProviderAdGroupID == nil {
		return material, store.ErrDirectGraphUnverified
	}
	graph, err := a.directGraph.GetCampaignGraph(
		ctx, token, material.Connection.ClientLogin,
		*material.Operation.ProviderCampaignID,
	)
	if err != nil {
		return material, a.directGraphProviderError(ctx, material.Connection, err)
	}
	state, err := directVerifyProviderEditPrefix(
		graph, material, baseline, "campaign_updated",
	)
	if err != nil {
		return material, err
	}
	warnings := append([]store.DirectProviderIssue(nil), material.Operation.ProviderWarnings...)
	if !state.AdGroupDesired {
		results, updateErr := a.directGraph.UpdateUnifiedAdGroups(
			ctx, token, material.Connection.ClientLogin,
			[]yandexdirect.UnifiedAdGroupUpdate{{
				ID: *material.Operation.ProviderAdGroupID,
				Name: yandexdirect.UnifiedAdGroupOperationName(
					material.Operation.OperationMarker,
				),
				RegionIDs:        baseline.DesiredRegionIDs,
				NegativeKeywords: material.DesiredCampaign.NegativeKeywords,
				TrackingMarker:   material.Operation.OperationMarker,
			}},
		)
		warnings = append(warnings, directMutationWarnings(results)...)
		graph, err = a.directGraph.GetCampaignGraph(
			ctx, token, material.Connection.ClientLogin,
			*material.Operation.ProviderCampaignID,
		)
		if err == nil {
			state, err = directVerifyProviderEditPrefix(
				graph, material, baseline, "campaign_updated",
			)
		}
		if err != nil || !state.AdGroupDesired {
			return material, a.directGraphProviderError(
				ctx, material.Connection,
				errors.Join(errDirectProviderGraphMismatch, updateErr, err),
			)
		}
	}
	return a.advanceDirectGraphStage(
		ctx, material, "ad_group_updated",
		store.DirectProviderStageUpdate{ProviderWarnings: &warnings},
	)
}

func (a *App) directUpdateOrReconcileAd(
	ctx context.Context, token string, material store.DirectGraphSubmissionMaterial,
	baseline directProviderEditBaseline,
) (store.DirectGraphSubmissionMaterial, error) {
	if material.Operation.ProviderCampaignID == nil ||
		material.Operation.ProviderAdGroupID == nil ||
		material.Operation.ProviderAdID == nil {
		return material, store.ErrDirectGraphUnverified
	}
	graph, err := a.directGraph.GetCampaignGraph(
		ctx, token, material.Connection.ClientLogin,
		*material.Operation.ProviderCampaignID,
	)
	if err != nil {
		return material, a.directGraphProviderError(ctx, material.Connection, err)
	}
	state, err := directVerifyProviderEditPrefix(
		graph, material, baseline, "ad_group_updated",
	)
	if err != nil {
		return material, err
	}
	warnings := append([]store.DirectProviderIssue(nil), material.Operation.ProviderWarnings...)
	if !state.AdDesired {
		results, updateErr := a.directGraph.UpdateResponsiveAds(
			ctx, token, material.Connection.ClientLogin,
			[]yandexdirect.ResponsiveAdUpdate{{
				ID:     *material.Operation.ProviderAdID,
				Titles: material.DesiredCampaign.Titles,
				Texts:  material.DesiredCampaign.Texts,
				Href:   material.DesiredCampaign.LandingURL,
			}},
		)
		warnings = append(warnings, directMutationWarnings(results)...)
		graph, err = a.directGraph.GetCampaignGraph(
			ctx, token, material.Connection.ClientLogin,
			*material.Operation.ProviderCampaignID,
		)
		if err == nil {
			state, err = directVerifyProviderEditPrefix(
				graph, material, baseline, "ad_group_updated",
			)
		}
		if err != nil || !state.AdDesired {
			return material, a.directGraphProviderError(
				ctx, material.Connection,
				errors.Join(errDirectProviderGraphMismatch, updateErr, err),
			)
		}
	}
	return a.advanceDirectGraphStage(
		ctx, material, "ad_updated",
		store.DirectProviderStageUpdate{ProviderWarnings: &warnings},
	)
}

func (a *App) directUpdateOrReconcileKeywords(
	ctx context.Context, token string, material store.DirectGraphSubmissionMaterial,
	baseline directProviderEditBaseline,
) (store.DirectGraphSubmissionMaterial, error) {
	if material.Operation.ProviderCampaignID == nil ||
		material.Operation.ProviderAdGroupID == nil {
		return material, store.ErrDirectGraphUnverified
	}
	campaignID := *material.Operation.ProviderCampaignID
	groupID := *material.Operation.ProviderAdGroupID
	graph, err := a.directGraph.GetCampaignGraph(
		ctx, token, material.Connection.ClientLogin, campaignID,
	)
	if err != nil {
		return material, a.directGraphProviderError(ctx, material.Connection, err)
	}
	state, err := directVerifyProviderEditPrefix(
		graph, material, baseline, "ad_updated",
	)
	if err != nil {
		return material, err
	}
	keywords := graph.Keywords
	warnings := append([]store.DirectProviderIssue(nil), material.Operation.ProviderWarnings...)
	if !state.KeywordsDesired {
		updates, deletes, adds, planErr := directKeywordEditPlan(
			keywords, baseline.BaseCampaign.Keywords,
			material.DesiredCampaign.Keywords,
			campaignID, groupID,
		)
		if planErr != nil {
			return material, planErr
		}
		if len(updates) != 0 {
			results, updateErr := a.directGraph.UpdateKeywords(
				ctx, token, material.Connection.ClientLogin, updates,
			)
			warnings = append(warnings, directMutationWarnings(results)...)
			keywords, err = a.directGraph.ListKeywords(
				ctx, token, material.Connection.ClientLogin, campaignID,
			)
			if err != nil {
				return material, a.directGraphProviderError(
					ctx, material.Connection, errors.Join(updateErr, err),
				)
			}
			updates, deletes, adds, planErr = directKeywordEditPlan(
				keywords, baseline.BaseCampaign.Keywords,
				material.DesiredCampaign.Keywords, campaignID, groupID,
			)
			if planErr != nil || len(updates) != 0 {
				return material, errors.Join(
					errDirectProviderGraphMismatch, updateErr, planErr,
				)
			}
		}
		if len(deletes) != 0 {
			results, deleteErr := a.directGraph.DeleteKeywords(
				ctx, token, material.Connection.ClientLogin, deletes,
			)
			warnings = append(warnings, directMutationWarnings(results)...)
			keywords, err = a.directGraph.ListKeywords(
				ctx, token, material.Connection.ClientLogin, campaignID,
			)
			if err != nil {
				return material, a.directGraphProviderError(
					ctx, material.Connection, errors.Join(deleteErr, err),
				)
			}
			updates, deletes, adds, planErr = directKeywordEditPlan(
				keywords, baseline.BaseCampaign.Keywords,
				material.DesiredCampaign.Keywords, campaignID, groupID,
			)
			if planErr != nil || len(updates) != 0 || len(deletes) != 0 {
				return material, errors.Join(
					errDirectProviderGraphMismatch, deleteErr, planErr,
				)
			}
		}
		if len(adds) != 0 {
			drafts := make([]yandexdirect.KeywordDraft, 0, len(adds))
			for _, keyword := range adds {
				drafts = append(drafts, yandexdirect.KeywordDraft{
					AdGroupID: groupID, Keyword: keyword,
				})
			}
			results, addErr := a.directGraph.AddKeywords(
				ctx, token, material.Connection.ClientLogin, drafts,
			)
			warnings = append(warnings, directMutationWarnings(results)...)
			_, err = a.directGraph.ListKeywords(
				ctx, token, material.Connection.ClientLogin, campaignID,
			)
			if err != nil {
				return material, a.directGraphProviderError(
					ctx, material.Connection, errors.Join(addErr, err),
				)
			}
		}
		graph, err = a.directGraph.GetCampaignGraph(
			ctx, token, material.Connection.ClientLogin, campaignID,
		)
		if err == nil {
			state, err = directVerifyProviderEditPrefix(
				graph, material, baseline, "ad_updated",
			)
		}
		if err != nil || !state.KeywordsDesired {
			return material, a.directGraphProviderError(
				ctx, material.Connection,
				errors.Join(errDirectProviderGraphMismatch, err),
			)
		}
		keywords = graph.Keywords
	}
	mappings, missing, err := directSubmissionKeywordState(
		keywords, material.DesiredCampaign.Keywords, campaignID, groupID,
	)
	if err != nil || len(missing) != 0 {
		return material, errors.Join(errDirectProviderGraphMismatch, err)
	}
	return a.advanceDirectGraphStage(
		ctx, material, "keywords_updated",
		store.DirectProviderStageUpdate{
			ProviderKeywordMappings: &mappings, ProviderWarnings: &warnings,
		},
	)
}

func (a *App) directObserveOperationGraph(
	ctx context.Context, token string, material store.DirectGraphSubmissionMaterial,
	regionIDs []int64,
) (store.DirectGraphSubmissionMaterial, error) {
	if material.Operation.ProviderCampaignID == nil ||
		material.Operation.ProviderAdGroupID == nil ||
		material.Operation.ProviderAdID == nil {
		return material, store.ErrDirectGraphUnverified
	}
	graph, err := a.directGraph.GetCampaignGraph(
		ctx, token, material.Connection.ClientLogin,
		*material.Operation.ProviderCampaignID,
	)
	if err != nil {
		return material, a.directGraphProviderError(ctx, material.Connection, err)
	}
	verified, err := verifyDirectProviderGraph(
		graph, material.DesiredCampaign, material.Connection,
		material.Operation.OperationMarker, regionIDs,
		material.Operation.ProviderCampaignID, material.Operation.ProviderAdGroupID,
		material.Operation.ProviderAdID,
	)
	if err != nil {
		return material, err
	}
	if !reflect.DeepEqual(
		verified.KeywordMappings, material.Operation.ProviderKeywordMappings,
	) {
		return material, errDirectProviderGraphMismatch
	}
	mappings := verified.KeywordMappings
	return a.advanceDirectGraphStage(
		ctx, material, "graph_observed",
		store.DirectProviderStageUpdate{
			ObservedGraph: verified.ObservedGraph, GraphHash: verified.GraphHash,
			ProviderKeywordMappings: &mappings,
		},
	)
}

func (a *App) directRecordVerifiedGraph(
	ctx context.Context, token string, material store.DirectGraphSubmissionMaterial,
	regionIDs []int64,
) (store.DirectGraphSubmissionMaterial, error) {
	if material.Operation.ProviderCampaignID == nil ||
		material.Operation.ProviderAdGroupID == nil ||
		material.Operation.ProviderAdID == nil {
		return material, store.ErrDirectGraphUnverified
	}
	graph, err := a.directGraph.GetCampaignGraph(
		ctx, token, material.Connection.ClientLogin,
		*material.Operation.ProviderCampaignID,
	)
	if err != nil {
		return material, a.directGraphProviderError(ctx, material.Connection, err)
	}
	verified, err := verifyDirectProviderGraph(
		graph, material.DesiredCampaign, material.Connection,
		material.Operation.OperationMarker, regionIDs,
		material.Operation.ProviderCampaignID, material.Operation.ProviderAdGroupID,
		material.Operation.ProviderAdID,
	)
	if err != nil || verified.GraphHash != material.Operation.GraphHash {
		return material, errors.Join(store.ErrDirectConsentMismatch, err)
	}
	_, err = a.store.RecordVerifiedDirectCampaignGraph(
		ctx, material.Campaign.WorkspaceID, material.Campaign.ID,
		store.DirectVerifiedGraphInput{
			ExpectedOperationID:              material.Operation.ID,
			ExpectedStage:                    "graph_observed",
			ExpectedCampaignVersion:          material.Operation.ExpectedCampaignVersion,
			ExpectedClaimedAt:                material.Operation.ClaimedAt,
			GraphVersion:                     store.DirectGraphFingerprintVersion,
			DesiredGraph:                     material.Operation.DesiredGraph,
			ObservedGraph:                    verified.ObservedGraph,
			GraphHash:                        verified.GraphHash,
			ProviderCampaignID:               verified.CampaignID,
			ProviderAdGroupID:                verified.AdGroupID,
			ProviderAdID:                     verified.AdID,
			ProviderKeywordMappings:          verified.KeywordMappings,
			ProviderWarnings:                 material.Operation.ProviderWarnings,
			CampaignModeration:               verified.CampaignModeration,
			AdGroupModeration:                verified.AdGroupModeration,
			AdModeration:                     verified.AdModeration,
			AggregateModerationStatus:        verified.ModerationStatus,
			AggregateModerationClarification: verified.Clarification,
			ObservedAt:                       a.now().UTC(),
			ActorUserID:                      material.Operation.ActorUserID,
		},
	)
	if err != nil {
		return material, err
	}
	return a.store.ReloadDirectCampaignGraphSubmission(
		ctx, material.Campaign.WorkspaceID, material.Campaign.ID,
		material.Operation.ID, a.now().UTC(),
	)
}

func (a *App) directRequestModeration(
	ctx context.Context, token string, material store.DirectGraphSubmissionMaterial,
	regionIDs []int64,
) (store.DirectGraphSubmissionMaterial, error) {
	if material.Operation.ProviderCampaignID == nil ||
		material.Operation.ProviderAdID == nil {
		return material, store.ErrDirectGraphUnverified
	}
	graph, err := a.directGraph.GetCampaignGraph(
		ctx, token, material.Connection.ClientLogin,
		*material.Operation.ProviderCampaignID,
	)
	if err != nil {
		return material, a.directGraphProviderError(ctx, material.Connection, err)
	}
	verified, err := verifyDirectProviderGraph(
		graph, material.DesiredCampaign, material.Connection,
		material.Operation.OperationMarker, regionIDs,
		material.Operation.ProviderCampaignID, material.Operation.ProviderAdGroupID,
		material.Operation.ProviderAdID,
	)
	if err != nil || verified.GraphHash != material.Operation.GraphHash {
		return material, errors.Join(store.ErrDirectConsentMismatch, err)
	}
	warnings := append([]store.DirectProviderIssue(nil), material.Operation.ProviderWarnings...)
	status := strings.ToUpper(strings.TrimSpace(graph.Ads[0].Status))
	switch status {
	case "DRAFT":
		results, moderateErr := a.directGraph.ModerateAds(
			ctx, token, material.Connection.ClientLogin,
			[]int64{*material.Operation.ProviderAdID},
		)
		warnings = append(warnings, directMutationWarnings(results)...)
		if moderateErr != nil {
			graph, err = a.directGraph.GetCampaignGraph(
				ctx, token, material.Connection.ClientLogin,
				*material.Operation.ProviderCampaignID,
			)
			if err != nil {
				return material, a.directGraphProviderError(
					ctx, material.Connection, errors.Join(moderateErr, err),
				)
			}
			switch strings.ToUpper(strings.TrimSpace(graph.Ads[0].Status)) {
			case "MODERATION", "PREACCEPTED", "ACCEPTED", "REJECTED":
			default:
				return material, a.directGraphProviderError(
					ctx, material.Connection, moderateErr,
				)
			}
		}
	case "MODERATION", "PREACCEPTED", "ACCEPTED", "REJECTED":
	default:
		return material, errDirectProviderGraphMismatch
	}
	return a.advanceDirectGraphStage(
		ctx, material, "moderation_requested",
		store.DirectProviderStageUpdate{ProviderWarnings: &warnings},
	)
}

func (a *App) launchDirectCampaignVerified(
	ctx context.Context, actorUserID, workspaceID, campaignID string,
	expectedVersion int64, expectedGraphHash, expectedRevisionID string,
) (store.DirectCampaign, error) {
	if err := a.requireDirectGraphWrites(); err != nil {
		return store.DirectCampaign{}, err
	}
	material, err := a.store.GetDirectManualLaunchMaterial(
		ctx, actorUserID, workspaceID, campaignID,
	)
	if err != nil {
		return store.DirectCampaign{}, err
	}
	expectedGraphHash = strings.TrimSpace(expectedGraphHash)
	expectedRevisionID = strings.TrimSpace(expectedRevisionID)
	if expectedVersion <= 0 || material.Campaign.Version != expectedVersion ||
		material.Campaign.ProviderGraphHash != expectedGraphHash ||
		material.Campaign.ProviderRevisionID != expectedRevisionID {
		return store.DirectCampaign{}, store.ErrDirectConsentMismatch
	}
	_, graph, verified, err := a.readVerifiedDirectLaunchGraph(ctx, material, false)
	if err != nil {
		return store.DirectCampaign{}, err
	}
	if directGraphCampaignRunning(graph.Campaign) {
		_, err = a.store.SyncDirectCampaignProviderStatus(
			ctx, workspaceID, campaignID, verified.CampaignID,
			graph.Campaign.Status, graph.Campaign.State, a.now().UTC(),
		)
		if err != nil {
			return store.DirectCampaign{}, err
		}
		return a.store.GetDirectCampaign(ctx, actorUserID, workspaceID, campaignID)
	}
	if verified.ModerationStatus != "ACCEPTED" ||
		!directGraphDeliveryReady(graph) {
		return store.DirectCampaign{}, store.ErrDirectModerationNotReady
	}
	if err := a.persistDirectGraphModeration(
		ctx, material, graph, verified,
	); err != nil {
		return store.DirectCampaign{}, err
	}
	claimed, err := a.store.ClaimDirectManualCampaignLaunch(
		ctx, actorUserID, workspaceID, campaignID, expectedVersion,
		verified.CampaignID, material.Connection.AccountID,
		material.Campaign.WeeklyBudgetMinor, material.Campaign.StartsAt,
		material.Campaign.EndsAt, expectedGraphHash, expectedRevisionID,
		a.now().UTC(),
	)
	if err != nil {
		return store.DirectCampaign{}, err
	}
	if err := a.attemptClaimedDirectLaunch(ctx, claimed); err != nil {
		return store.DirectCampaign{}, err
	}
	return a.store.GetDirectCampaign(ctx, actorUserID, workspaceID, campaignID)
}

func (a *App) launchDirectAutoCampaignVerified(
	ctx context.Context, material store.DirectLaunchMaterial,
) error {
	if err := a.requireDirectGraphWrites(); err != nil {
		return err
	}
	_, graph, verified, err := a.readVerifiedDirectLaunchGraph(ctx, material, true)
	if err != nil {
		_ = a.store.InvalidateDirectAutoLaunchConsent(
			ctx, material.Campaign.WorkspaceID, material.Campaign.ID,
			"provider_graph_mismatch", a.now().UTC(),
		)
		return err
	}
	if directGraphCampaignRunning(graph.Campaign) {
		_, err = a.store.SyncDirectCampaignProviderStatus(
			ctx, material.Campaign.WorkspaceID, material.Campaign.ID,
			verified.CampaignID, graph.Campaign.Status, graph.Campaign.State,
			a.now().UTC(),
		)
		return err
	}
	if verified.ModerationStatus != "ACCEPTED" ||
		!directGraphDeliveryReady(graph) {
		return store.ErrDirectModerationNotReady
	}
	if err := a.persistDirectGraphModeration(
		ctx, material, graph, verified,
	); err != nil {
		return err
	}
	claimed, err := a.store.ClaimDirectAutoCampaignLaunch(
		ctx, material.Campaign.WorkspaceID, material.Campaign.ID,
		material.Campaign.Version, verified.CampaignID,
		material.Connection.AccountID, material.Campaign.WeeklyBudgetMinor,
		material.Campaign.StartsAt, material.Campaign.EndsAt,
		material.Campaign.ProviderGraphHash, material.Campaign.ProviderRevisionID,
		a.now().UTC(),
	)
	if err != nil {
		return err
	}
	return a.attemptClaimedDirectLaunch(ctx, claimed)
}

func (a *App) readVerifiedDirectLaunchGraph(
	ctx context.Context, material store.DirectLaunchMaterial, auto bool,
) (string, yandexdirect.CampaignGraph, directVerifiedProviderGraph, error) {
	if material.Campaign.ProviderCampaignID == nil ||
		material.Campaign.ProviderAdGroupID == nil ||
		material.Campaign.ProviderAdID == nil ||
		material.Campaign.ProviderGraphHash == "" ||
		material.Campaign.ProviderRevisionID == "" ||
		material.Campaign.SubmissionOperationMarker == "" {
		return "", yandexdirect.CampaignGraph{}, directVerifiedProviderGraph{},
			store.ErrDirectGraphUnverified
	}
	token, err := a.directAccessToken(ctx, material.Connection)
	if err != nil {
		return "", yandexdirect.CampaignGraph{}, directVerifiedProviderGraph{}, err
	}
	regionIDs, err := a.resolveDirectRegionIDs(
		ctx, token, material.Connection, material.Campaign.Regions,
	)
	if err != nil {
		return "", yandexdirect.CampaignGraph{}, directVerifiedProviderGraph{},
			a.directGraphProviderError(ctx, material.Connection, err)
	}
	graph, err := a.directGraph.GetCampaignGraph(
		ctx, token, material.Connection.ClientLogin,
		*material.Campaign.ProviderCampaignID,
	)
	if err != nil {
		if auto && directProviderStrategySnapshotError(err) {
			invalidateErr := a.store.InvalidateDirectAutoLaunchConsent(
				ctx, material.Campaign.WorkspaceID, material.Campaign.ID,
				"provider_strategy_changed", a.now().UTC(),
			)
			return "", yandexdirect.CampaignGraph{}, directVerifiedProviderGraph{},
				errors.Join(
					ErrDirectSnapshotMismatch,
					a.directGraphProviderError(ctx, material.Connection, err),
					invalidateErr,
				)
		}
		return "", yandexdirect.CampaignGraph{}, directVerifiedProviderGraph{},
			a.directGraphProviderError(ctx, material.Connection, err)
	}
	verified, err := verifyDirectProviderGraphState(
		graph, material.Campaign, material.Connection,
		material.Campaign.SubmissionOperationMarker, regionIDs,
		material.Campaign.ProviderCampaignID, material.Campaign.ProviderAdGroupID,
		material.Campaign.ProviderAdID, false,
	)
	if err != nil || verified.GraphHash != material.Campaign.ProviderGraphHash {
		if auto {
			_ = a.store.InvalidateDirectAutoLaunchConsent(
				ctx, material.Campaign.WorkspaceID, material.Campaign.ID,
				"provider_graph_mismatch", a.now().UTC(),
			)
		}
		return token, graph, directVerifiedProviderGraph{},
			errors.Join(ErrDirectSnapshotMismatch, err)
	}
	state := strings.ToUpper(strings.TrimSpace(graph.Campaign.State))
	if state != "OFF" && state != "ON" {
		return "", yandexdirect.CampaignGraph{}, directVerifiedProviderGraph{},
			store.ErrDirectCampaignNotAccepted
	}
	return token, graph, verified, nil
}

func (a *App) reconcileDirectCampaignLaunch(
	ctx context.Context, workspaceID, campaignID string, allowProviderRetry bool,
) error {
	if a.directGraph == nil || !a.directProviderGraphVerified {
		return ErrDirectGraphUnsupported
	}
	// Reconciliation always remains available for read-only provider truth,
	// but no caller may turn a retry back on while the write kill-switch is off.
	allowProviderRetry = allowProviderRetry && a.DirectWritesEnabled()
	material, err := a.store.GetDirectLaunchRecoveryMaterial(
		ctx, workspaceID, campaignID,
	)
	if err != nil {
		return err
	}
	if material.Campaign.LaunchClaimedAt == nil {
		return store.ErrDirectLaunchAlreadyClaimed
	}
	launchClaimedAt := *material.Campaign.LaunchClaimedAt
	_, graph, _, err := a.readVerifiedDirectLaunchGraph(ctx, material, false)
	if err != nil {
		if errors.Is(err, ErrDirectSnapshotMismatch) &&
			material.Campaign.ProviderCampaignID != nil &&
			graph.Campaign.ID == *material.Campaign.ProviderCampaignID &&
			directGraphCampaignRunning(graph.Campaign) {
			now := a.now().UTC()
			if _, syncErr := a.store.SyncDirectCampaignProviderStatusForLaunch(
				ctx, workspaceID, campaignID, graph.Campaign.ID,
				graph.Campaign.Status, graph.Campaign.State, launchClaimedAt, now,
			); syncErr != nil {
				return errors.Join(err, syncErr)
			}
			if mismatchErr := a.store.SetDirectCampaignProviderSnapshotMismatchForLaunch(
				ctx, workspaceID, campaignID, true, launchClaimedAt, now,
			); mismatchErr != nil {
				return errors.Join(err, mismatchErr)
			}
			// Provider ON is authoritative for spend. Keep the content drift
			// visible, but never issue another Resume for the changed graph.
			return ErrDirectSnapshotMismatch
		}
		_ = a.store.MarkDirectCampaignLaunchReconciling(
			ctx, workspaceID, campaignID, launchClaimedAt,
			"provider_graph_mismatch", a.now().UTC(),
		)
		return err
	}
	if _, err := a.store.SyncDirectCampaignProviderStatusForLaunch(
		ctx, workspaceID, campaignID, graph.Campaign.ID,
		graph.Campaign.Status, graph.Campaign.State, launchClaimedAt,
		a.now().UTC(),
	); err != nil {
		return err
	}
	if directGraphCampaignRunning(graph.Campaign) {
		return nil
	}
	if strings.ToUpper(strings.TrimSpace(graph.Campaign.State)) != "OFF" {
		return a.store.MarkDirectCampaignLaunchReconciling(
			ctx, workspaceID, campaignID, launchClaimedAt,
			"provider_state_ambiguous", a.now().UTC(),
		)
	}
	if !allowProviderRetry || !a.DirectWritesEnabled() {
		return a.store.MarkDirectCampaignLaunchReconciling(
			ctx, workspaceID, campaignID, launchClaimedAt,
			"provider_retry_disabled", a.now().UTC(),
		)
	}
	if material.Campaign.LaunchAttemptCount >= 2 {
		return a.store.FailDirectCampaignLaunch(
			ctx, workspaceID, campaignID, launchClaimedAt,
			"provider_off_after_retries", a.now().UTC(),
		)
	}
	return a.attemptClaimedDirectLaunch(ctx, material)
}

func (a *App) syncDirectCampaignLifecycle(
	ctx context.Context, workspaceID, campaignID string,
) error {
	if a.directGraph == nil || !a.directProviderGraphVerified {
		return ErrDirectGraphUnsupported
	}
	material, err := a.store.GetDirectLifecycleMaterial(ctx, workspaceID, campaignID)
	if err != nil {
		return err
	}
	if material.Campaign.ProviderCampaignID == nil ||
		material.Campaign.ProviderAdGroupID == nil ||
		material.Campaign.ProviderAdID == nil {
		return store.ErrDirectGraphUnverified
	}
	token, err := a.directAccessToken(ctx, material.Connection)
	if err != nil {
		return err
	}
	regionIDs, err := a.resolveDirectRegionIDs(
		ctx, token, material.Connection, material.Campaign.Regions,
	)
	if err != nil {
		return a.directGraphProviderError(ctx, material.Connection, err)
	}
	graph, err := a.directGraph.GetCampaignGraph(
		ctx, token, material.Connection.ClientLogin,
		*material.Campaign.ProviderCampaignID,
	)
	if err != nil {
		return a.directGraphProviderError(ctx, material.Connection, err)
	}
	verified, err := verifyDirectProviderGraphState(
		graph, material.Campaign, material.Connection,
		material.Campaign.SubmissionOperationMarker, regionIDs,
		material.Campaign.ProviderCampaignID, material.Campaign.ProviderAdGroupID,
		material.Campaign.ProviderAdID, false,
	)
	if err != nil || verified.GraphHash != material.Campaign.ProviderGraphHash {
		_ = a.store.SetDirectCampaignProviderSnapshotMismatch(
			ctx, workspaceID, campaignID, true, a.now().UTC(),
		)
		return errors.Join(ErrDirectSnapshotMismatch, err)
	}
	if err := a.store.SetDirectCampaignProviderSnapshotMismatch(
		ctx, workspaceID, campaignID, false, a.now().UTC(),
	); err != nil {
		return err
	}
	return a.persistDirectGraphModeration(ctx, material, graph, verified)
}

func (a *App) persistDirectGraphModeration(
	ctx context.Context, material store.DirectLaunchMaterial,
	graph yandexdirect.CampaignGraph, verified directVerifiedProviderGraph,
) error {
	_, err := a.store.UpdateDirectCampaignGraphModeration(
		ctx, material.Campaign.WorkspaceID, material.Campaign.ID,
		store.DirectGraphModerationUpdate{
			ExpectedGraphHash:                material.Campaign.ProviderGraphHash,
			ExpectedRevisionID:               material.Campaign.ProviderRevisionID,
			Campaign:                         verified.CampaignModeration,
			AdGroup:                          verified.AdGroupModeration,
			Ad:                               verified.AdModeration,
			Keywords:                         verified.KeywordMappings,
			AggregateModerationStatus:        verified.ModerationStatus,
			AggregateModerationClarification: verified.Clarification,
			ProviderStatus:                   graph.Campaign.Status,
			ProviderState:                    graph.Campaign.State,
			StatusClarification:              verified.Clarification,
			CheckedAt:                        a.now().UTC(),
		},
	)
	return err
}

func (a *App) advanceDirectGraphStage(
	ctx context.Context, material store.DirectGraphSubmissionMaterial, stage string,
	update store.DirectProviderStageUpdate,
) (store.DirectGraphSubmissionMaterial, error) {
	update.Stage = stage
	update.ExpectedClaimedAt = material.Operation.ClaimedAt
	return a.store.AdvanceDirectCampaignGraphSubmission(
		ctx, material.Campaign.WorkspaceID, material.Campaign.ID,
		material.Operation.ID, material.Operation.Stage, update, a.now().UTC(),
	)
}

func (a *App) resolveDirectRegionIDs(
	ctx context.Context, token string, connection store.DirectConnection,
	names []string,
) ([]int64, error) {
	regions, err := a.directGraph.ResolveRegionNames(
		ctx, token, connection.ClientLogin, names,
	)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(regions))
	for _, region := range regions {
		if region.ID <= 0 {
			return nil, errDirectProviderGraphMismatch
		}
		ids = append(ids, region.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

func (a *App) directGraphProviderError(
	ctx context.Context, connection store.DirectConnection, err error,
) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrDirectSnapshotMismatch) ||
		errors.Is(err, store.ErrDirectConsentMismatch) ||
		errors.Is(err, store.ErrDirectGraphUnverified) {
		return err
	}
	connectionErr := a.markDirectConnectionAuthorizationRequired(
		ctx, connection, err, a.now().UTC(),
	)
	return errors.Join(fmt.Errorf("%w: %w", ErrDirectProvider, err), connectionErr)
}

func directProviderOperationTerminal(stage string) bool {
	switch strings.TrimSpace(stage) {
	case "", "completed", "failed":
		return true
	default:
		return false
	}
}

func directResponsiveAdMatches(
	ad yandexdirect.ResponsiveAd, campaign store.DirectCampaign, groupID int64,
) bool {
	href, err := normalizeDirectGraphHref(campaign.LandingURL)
	return err == nil && ad.ID > 0 && ad.AdGroupID == groupID &&
		reflect.DeepEqual(moderatedTextValues(ad.Titles), campaign.Titles) &&
		reflect.DeepEqual(moderatedTextValues(ad.Texts), campaign.Texts) &&
		ad.Href == href
}

func directAdGroupNodeMatches(
	group yandexdirect.UnifiedAdGroup, campaign store.DirectCampaign,
	regionIDs []int64, marker string,
) bool {
	regions := append([]int64(nil), regionIDs...)
	sort.Slice(regions, func(i, j int) bool { return regions[i] < regions[j] })
	return group.ID > 0 &&
		group.Name == yandexdirect.UnifiedAdGroupOperationName(marker) &&
		reflect.DeepEqual(group.RegionIDs, regions) &&
		reflect.DeepEqual(
			sortedCopy(group.NegativeKeywords),
			sortedCopy(campaign.NegativeKeywords),
		) &&
		group.TrackingParams == "" && group.TrackingMarker == "" &&
		group.OfferRetargeting == "NO"
}

func directCampaignNodeMatches(
	provider yandexdirect.GraphCampaign, campaign store.DirectCampaign,
	connection store.DirectConnection, marker string,
) bool {
	if provider.ID <= 0 || provider.Name != strings.TrimSpace(campaign.Name) ||
		provider.Type != "UNIFIED_CAMPAIGN" ||
		provider.WeeklyBudgetMinor != campaign.WeeklyBudgetMinor ||
		!sameDirectDate(provider.StartsAt, campaign.StartsAt) ||
		!sameDirectDate(provider.EndsAt, campaign.EndsAt) ||
		provider.TimeZone != strings.TrimSpace(connection.Timezone) ||
		strings.ToUpper(strings.TrimSpace(provider.State)) != "OFF" ||
		len(provider.NegativeKeywords) != 0 ||
		len(provider.NegativeKeywordSharedSetIDs) != 0 ||
		len(provider.BlockedIPs) != 0 || len(provider.ExcludedSites) != 0 ||
		len(provider.CounterIDs) != 0 || len(provider.PriorityGoals) != 0 ||
		!reflect.DeepEqual(
			provider.TimeTargeting, yandexdirect.SafeUnifiedCampaignTimeTargeting(),
		) ||
		provider.TrackingParams != (url.Values{
			"mp_op": []string{marker},
		}).Encode() ||
		provider.AttributionModel != "AUTO" {
		return false
	}
	return validateDirectSafeCampaignSettings(provider.Settings) == nil &&
		validateDirectSafeBiddingStrategy(
			provider.BiddingStrategy, campaign.WeeklyBudgetMinor,
		) == nil
}

func directVerifyProviderEditPrefix(
	graph yandexdirect.CampaignGraph,
	material store.DirectGraphSubmissionMaterial,
	baseline directProviderEditBaseline,
	stage string,
) (directProviderEditPrefixState, error) {
	mismatch := func(field string) (directProviderEditPrefixState, error) {
		return directProviderEditPrefixState{}, fmt.Errorf(
			"%w: provider edit %s prefix drift (%s)",
			errDirectProviderGraphMismatch, stage, field,
		)
	}
	if material.Operation.OperationKind != "update" ||
		material.Operation.Stage != stage ||
		baseline.GraphHash == "" ||
		baseline.GraphHash != material.Operation.ExpectedGraphHash {
		return mismatch("operation")
	}
	if err := directUpdateTopologySafe(graph, material); err != nil {
		return mismatch("topology")
	}
	campaignBase := directProviderEditNodeMatchesBaseline(
		graph, baseline, "campaign",
	)
	adGroupBase := directProviderEditNodeMatchesBaseline(
		graph, baseline, "ad_group",
	)
	adBase := directProviderEditNodeMatchesBaseline(graph, baseline, "ad")
	keywordsBase := directProviderEditNodeMatchesBaseline(
		graph, baseline, "keywords",
	)
	state := directProviderEditPrefixState{
		CampaignDesired: directCampaignNodeMatches(
			graph.Campaign, material.DesiredCampaign, material.Connection,
			material.Operation.OperationMarker,
		),
		AdGroupDesired: directAdGroupNodeMatches(
			graph.AdGroups[0], material.DesiredCampaign,
			baseline.DesiredRegionIDs, material.Operation.OperationMarker,
		),
		AdDesired: directResponsiveAdMatches(
			graph.Ads[0], material.DesiredCampaign,
			*material.Operation.ProviderAdGroupID,
		),
	}
	_, missing, keywordErr := directSubmissionKeywordState(
		graph.Keywords, material.DesiredCampaign.Keywords,
		*material.Operation.ProviderCampaignID,
		*material.Operation.ProviderAdGroupID,
	)
	state.KeywordsDesired = keywordErr == nil && len(missing) == 0
	_, _, _, transitionErr := directKeywordEditPlan(
		graph.Keywords, baseline.BaseCampaign.Keywords,
		material.DesiredCampaign.Keywords,
		*material.Operation.ProviderCampaignID,
		*material.Operation.ProviderAdGroupID,
	)
	keywordsTransitionValid := transitionErr == nil

	valid := false
	switch stage {
	case "claimed":
		hash, hashErr := graph.Fingerprint()
		fullBase := hashErr == nil &&
			hash == material.Operation.ExpectedGraphHash &&
			campaignBase && adGroupBase && adBase && keywordsBase
		recoveredCampaignWrite := state.CampaignDesired &&
			adGroupBase && adBase && keywordsBase
		valid = fullBase || recoveredCampaignWrite
	case "campaign_updated":
		valid = state.CampaignDesired &&
			(adGroupBase || state.AdGroupDesired) &&
			adBase && keywordsBase
	case "ad_group_updated":
		valid = state.CampaignDesired && state.AdGroupDesired &&
			(adBase || state.AdDesired) && keywordsBase
	case "ad_updated":
		valid = state.CampaignDesired && state.AdGroupDesired &&
			state.AdDesired &&
			(keywordsBase || state.KeywordsDesired || keywordsTransitionValid)
	default:
		return mismatch("unsupported stage")
	}
	if !valid {
		return mismatch("content")
	}
	return state, nil
}

func directProviderEditNodeMatchesBaseline(
	graph yandexdirect.CampaignGraph,
	baseline directProviderEditBaseline,
	node string,
) bool {
	candidate := baseline.Graph
	switch node {
	case "campaign":
		candidate.Campaign = graph.Campaign
	case "ad_group":
		candidate.AdGroups = graph.AdGroups
	case "ad":
		candidate.Ads = graph.Ads
	case "keywords":
		candidate.Keywords = graph.Keywords
	default:
		return false
	}
	hash, err := candidate.Fingerprint()
	return err == nil && hash == baseline.GraphHash
}

func directUpdateTopologySafe(
	graph yandexdirect.CampaignGraph, material store.DirectGraphSubmissionMaterial,
) error {
	if material.Operation.ProviderCampaignID == nil ||
		material.Operation.ProviderAdGroupID == nil ||
		material.Operation.ProviderAdID == nil ||
		graph.Campaign.ID != *material.Operation.ProviderCampaignID ||
		strings.ToUpper(strings.TrimSpace(graph.Campaign.State)) != "OFF" ||
		len(graph.AdGroups) != 1 || len(graph.Ads) != 1 ||
		graph.AdGroups[0].ID != *material.Operation.ProviderAdGroupID ||
		graph.AdGroups[0].CampaignID != *material.Operation.ProviderCampaignID ||
		graph.Ads[0].ID != *material.Operation.ProviderAdID ||
		graph.Ads[0].CampaignID != *material.Operation.ProviderCampaignID ||
		graph.Ads[0].AdGroupID != *material.Operation.ProviderAdGroupID {
		return errDirectProviderGraphMismatch
	}
	return nil
}

func directSubmissionKeywordState(
	values []yandexdirect.Keyword, desired []string, campaignID, groupID int64,
) ([]store.DirectKeywordMapping, []string, error) {
	byKeyword := make(map[string]yandexdirect.Keyword, len(values))
	desiredSet := make(map[string]struct{}, len(desired))
	for _, value := range desired {
		desiredSet[strings.ToLower(strings.TrimSpace(value))] = struct{}{}
	}
	for _, value := range values {
		key := strings.ToLower(strings.TrimSpace(value.Keyword))
		if value.ID <= 0 || value.CampaignID != campaignID ||
			value.AdGroupID != groupID || value.StrategyPriority != "NORMAL" ||
			value.UserParam1 != "" || value.UserParam2 != "" {
			return nil, nil, errDirectProviderGraphMismatch
		}
		if _, wanted := desiredSet[key]; !wanted {
			return nil, nil, errDirectProviderGraphMismatch
		}
		if _, duplicate := byKeyword[key]; duplicate {
			return nil, nil, errDirectProviderGraphMismatch
		}
		byKeyword[key] = value
	}
	mappings := make([]store.DirectKeywordMapping, 0, len(desired))
	missing := make([]string, 0)
	for _, phrase := range desired {
		value, ok := byKeyword[strings.ToLower(strings.TrimSpace(phrase))]
		if !ok {
			missing = append(missing, phrase)
			continue
		}
		if value.Keyword != phrase {
			return nil, nil, errDirectProviderGraphMismatch
		}
		mappings = append(mappings, store.DirectKeywordMapping{
			Keyword: phrase, ProviderKeywordID: value.ID,
			Moderation: directModerationSnapshot(
				value.Status, value.State, value.ServingStatus, "",
			),
		})
	}
	return mappings, missing, nil
}

func directKeywordEditPlan(
	current []yandexdirect.Keyword, previous, desired []string,
	campaignID, groupID int64,
) ([]yandexdirect.KeywordUpdate, []int64, []string, error) {
	desiredByKey := make(map[string]string, len(desired))
	for _, phrase := range desired {
		desiredByKey[strings.ToLower(strings.TrimSpace(phrase))] = phrase
	}
	allowedExisting := make(map[string]struct{})
	for _, phrase := range previous {
		allowedExisting[strings.ToLower(strings.TrimSpace(phrase))] = struct{}{}
	}
	for key := range desiredByKey {
		allowedExisting[key] = struct{}{}
	}
	matched := make(map[string]struct{}, len(desired))
	unmatched := make([]yandexdirect.Keyword, 0)
	for _, keyword := range current {
		if keyword.ID <= 0 || keyword.CampaignID != campaignID ||
			keyword.AdGroupID != groupID || keyword.StrategyPriority != "NORMAL" ||
			keyword.UserParam1 != "" || keyword.UserParam2 != "" {
			return nil, nil, nil, errDirectProviderGraphMismatch
		}
		key := strings.ToLower(strings.TrimSpace(keyword.Keyword))
		if _, allowed := allowedExisting[key]; !allowed {
			return nil, nil, nil, errDirectProviderGraphMismatch
		}
		if desiredPhrase, ok := desiredByKey[key]; ok &&
			desiredPhrase == keyword.Keyword {
			if _, duplicate := matched[key]; duplicate {
				return nil, nil, nil, errDirectProviderGraphMismatch
			}
			matched[key] = struct{}{}
			continue
		}
		unmatched = append(unmatched, keyword)
	}
	missing := make([]string, 0)
	for _, phrase := range desired {
		if _, ok := matched[strings.ToLower(strings.TrimSpace(phrase))]; !ok {
			missing = append(missing, phrase)
		}
	}
	pairs := len(unmatched)
	if len(missing) < pairs {
		pairs = len(missing)
	}
	updates := make([]yandexdirect.KeywordUpdate, 0, pairs)
	for index := 0; index < pairs; index++ {
		updates = append(updates, yandexdirect.KeywordUpdate{
			ID: unmatched[index].ID, Keyword: missing[index],
		})
	}
	deletes := make([]int64, 0, len(unmatched)-pairs)
	for _, keyword := range unmatched[pairs:] {
		deletes = append(deletes, keyword.ID)
	}
	adds := append([]string(nil), missing[pairs:]...)
	return updates, deletes, adds, nil
}

func directMutationWarnings(results []yandexdirect.MutationResult) []store.DirectProviderIssue {
	var warnings []store.DirectProviderIssue
	for _, result := range results {
		warnings = append(warnings, directProviderIssues(result.Warnings)...)
	}
	return warnings
}

func directGraphCampaignRunning(campaign yandexdirect.GraphCampaign) bool {
	return strings.EqualFold(strings.TrimSpace(campaign.State), "ON")
}

func directGraphDeliveryReady(graph yandexdirect.CampaignGraph) bool {
	if strings.ToUpper(strings.TrimSpace(graph.Campaign.Status)) != "ACCEPTED" ||
		len(graph.AdGroups) != 1 || len(graph.Ads) != 1 {
		return false
	}
	eligible := func(value string) bool {
		switch strings.ToUpper(strings.TrimSpace(value)) {
		case "ELIGIBLE", "RARELY_SERVED":
			return true
		default:
			return false
		}
	}
	if !eligible(graph.AdGroups[0].ServingStatus) ||
		strings.ToUpper(strings.TrimSpace(graph.Ads[0].State)) != "ON" {
		return false
	}
	for _, keyword := range graph.Keywords {
		if strings.ToUpper(strings.TrimSpace(keyword.State)) != "ON" ||
			!eligible(keyword.ServingStatus) {
			return false
		}
	}
	return true
}
