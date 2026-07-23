package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	DirectGraphProviderOutcomeAbsent            = "provider_absent"
	DirectGraphProviderOutcomeBaselineUnchanged = "baseline_unchanged"
)

type DirectGraphTerminalFailureInput struct {
	ExpectedOperationID string
	ExpectedClaimedAt   time.Time
	ProviderOutcome     string
	FailureCode         string
	ConfirmedAt         time.Time
}

// FailDirectCampaignGraphOperation records only authoritative, non-ambiguous
// provider rejections. A submission can fail terminally only while no provider
// object exists. An edit can fail terminally only after the caller has read the
// provider graph and confirmed the immutable expected revision is unchanged.
// Every partial or ambiguous outcome remains leased/recoverable instead.
func (s *Store) FailDirectCampaignGraphOperation(
	ctx context.Context, workspaceID, campaignID string,
	input DirectGraphTerminalFailureInput,
) (DirectCampaign, error) {
	input.ExpectedOperationID = strings.TrimSpace(input.ExpectedOperationID)
	input.ExpectedClaimedAt = input.ExpectedClaimedAt.UTC().Truncate(time.Microsecond)
	input.ProviderOutcome = strings.TrimSpace(input.ProviderOutcome)
	input.FailureCode = sanitizeDirectGraphFailureCode(input.FailureCode)
	input.ConfirmedAt = directProviderOperationReadTime(input.ConfirmedAt)
	if input.ExpectedOperationID == "" || input.ExpectedClaimedAt.IsZero() ||
		input.FailureCode == "" {
		return DirectCampaign{}, fmt.Errorf(
			"%w: incomplete terminal provider failure", ErrDirectValidation,
		)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DirectCampaign{}, err
	}
	defer func() { _ = tx.Rollback() }()
	campaign, err := scanDirectCampaign(tx.QueryRowContext(ctx, `SELECT `+directCampaignColumns+`
FROM direct_campaigns
WHERE workspace_id=$1 AND id=$2
FOR UPDATE`, workspaceID, campaignID))
	if err != nil {
		return DirectCampaign{}, err
	}
	operation, err := scanDirectProviderOperation(tx.QueryRowContext(ctx,
		`SELECT `+directProviderOperationColumns+`
FROM direct_provider_operations
WHERE workspace_id=$1 AND campaign_id=$2 AND id=$3
FOR UPDATE`, workspaceID, campaignID, input.ExpectedOperationID))
	if err != nil {
		return DirectCampaign{}, err
	}
	if campaign.SubmissionOperationID != operation.ID ||
		!operation.ClaimedAt.Equal(input.ExpectedClaimedAt) ||
		operation.CompletedAt != nil ||
		operation.Stage == "completed" || operation.Stage == "failed" {
		return DirectCampaign{}, ErrConflict
	}
	if operation.LeaseExpiresAt.Before(input.ConfirmedAt) {
		return DirectCampaign{}, ErrDirectProviderOperationStale
	}

	clarification := ""
	var status string
	baselineMarker := campaign.SubmissionOperationMarker
	switch operation.OperationKind {
	case "submission":
		if input.ProviderOutcome != DirectGraphProviderOutcomeAbsent ||
			operation.ProviderCampaignID != nil ||
			operation.ProviderAdGroupID != nil ||
			operation.ProviderAdID != nil ||
			len(operation.ProviderKeywordMappings) != 0 ||
			campaign.ProviderCampaignID != nil ||
			campaign.ProviderAdGroupID != nil ||
			campaign.ProviderAdID != nil ||
			len(campaign.ProviderKeywordMappings) != 0 {
			return DirectCampaign{}, ErrDirectProviderOperationBusy
		}
		// The workflow is terminal, not the user's local draft. Keep the
		// durable failed operation/evidence, but let the user correct the
		// rejected draft and submit it again with a new version and marker.
		status = "draft"
		clarification = "provider_rejected_before_creation"
	case "update":
		if input.ProviderOutcome != DirectGraphProviderOutcomeBaselineUnchanged {
			return DirectCampaign{}, ErrDirectProviderOperationBusy
		}
		revision, marker, restoreErr := directGraphFailureBaseline(
			ctx, tx, operation,
		)
		if restoreErr != nil {
			return DirectCampaign{}, restoreErr
		}
		evidence, evidenceErr := directGraphRevisionEvidence(revision)
		if evidenceErr != nil {
			return DirectCampaign{}, evidenceErr
		}
		mappingsJSON, _ := json.Marshal(revision.ProviderKeywordMappings)
		keywordIDsJSON, _ := json.Marshal(
			directProviderKeywordIDs(revision.ProviderKeywordMappings),
		)
		warningsJSON, _ := json.Marshal(revision.ProviderWarnings)
		campaignModerationJSON, _ := json.Marshal(evidence.Campaign)
		adGroupModerationJSON, _ := json.Marshal(evidence.AdGroup)
		adModerationJSON, _ := json.Marshal(evidence.Ad)
		keywordModerationJSON, _ := json.Marshal(
			normalizeDirectKeywordMappingsModeration(
				revision.ProviderKeywordMappings,
			),
		)
		status = directLocalStatusForModeration(
			revision.ModerationStatus, campaign.Status,
		)
		if revision.ModerationStatus == "ACCEPTED" &&
			evidence.ProviderStatus != "ACCEPTED" {
			status = "moderation"
		}
		result, updateErr := tx.ExecContext(ctx, `UPDATE direct_campaigns
SET provider_campaign_id=$1,provider_ad_group_id=$2,provider_ad_id=$3,
    provider_keyword_ids=$4,provider_keyword_mappings=$5,provider_warnings=$6,
    provider_graph_hash=$7,provider_revision_id=$8,graph_verified_at=$9,
    moderation_status=$10,moderation_clarification=$11,
    campaign_moderation=$12,ad_group_moderation=$13,ad_moderation=$14,
    keyword_moderation=$15,provider_status=$16,provider_state=$17,
    status=$18,submission_operation_marker=$19,updated_at=$20
WHERE workspace_id=$21 AND id=$22 AND submission_operation_id=$23`,
			revision.ProviderCampaignID, revision.ProviderAdGroupID,
			revision.ProviderAdID, string(keywordIDsJSON), string(mappingsJSON),
			string(warningsJSON), revision.GraphHash, revision.ID,
			revision.ObservedAt, revision.ModerationStatus,
			revision.ModerationClarification, string(campaignModerationJSON),
			string(adGroupModerationJSON), string(adModerationJSON),
			string(keywordModerationJSON), evidence.ProviderStatus,
			evidence.ProviderState, status, marker, input.ConfirmedAt,
			workspaceID, campaignID, operation.ID)
		if updateErr != nil {
			return DirectCampaign{}, updateErr
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return DirectCampaign{}, ErrConflict
		}
		baselineMarker = marker
		clarification = "provider_rejected_without_changes"
	default:
		return DirectCampaign{}, ErrConflict
	}

	operation.Stage = "failed"
	operation.LastProviderErrorCode = input.FailureCode
	operation.LastProviderClarification = clarification
	operation.LeaseExpiresAt = input.ConfirmedAt
	operation.CompletedAt = directTimePointer(input.ConfirmedAt)
	operation.UpdatedAt = input.ConfirmedAt
	result, err := tx.ExecContext(ctx, `UPDATE direct_provider_operations
SET stage='failed',last_provider_error_code=$1,
    last_provider_clarification=$2,lease_expires_at=$3,completed_at=$3,
    updated_at=$3
WHERE workspace_id=$4 AND campaign_id=$5 AND id=$6
  AND claimed_at=$7 AND completed_at IS NULL
  AND stage NOT IN ('completed','failed')`,
		operation.LastProviderErrorCode, operation.LastProviderClarification,
		input.ConfirmedAt, workspaceID, campaignID, operation.ID,
		input.ExpectedClaimedAt)
	if err != nil {
		return DirectCampaign{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return DirectCampaign{}, ErrConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE direct_campaigns
SET status=$1,submission_stage='failed',submission_operation_marker=$2,
    submission_lease_expires_at=$3,submission_failure_code=$4,
    submission_failure_clarification=$5,updated_at=$3
WHERE workspace_id=$6 AND id=$7 AND submission_operation_id=$8`,
		status, baselineMarker, input.ConfirmedAt, operation.LastProviderErrorCode,
		operation.LastProviderClarification, workspaceID, campaignID, operation.ID)
	if err != nil {
		return DirectCampaign{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return DirectCampaign{}, ErrConflict
	}
	if err := insertDirectProviderOperationJournal(
		ctx, tx, operation, input.ConfirmedAt,
	); err != nil {
		return DirectCampaign{}, err
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID,
		ActorUserID: operation.ActorUserID,
		Action:      "direct.campaign.graph_operation_failed",
		EntityType:  "direct_campaign",
		EntityID:    campaignID,
		Metadata: mustJSON(map[string]any{
			"operation_id":     operation.ID,
			"operation_kind":   operation.OperationKind,
			"provider_outcome": input.ProviderOutcome,
			"failure_code":     input.FailureCode,
		}),
		CreatedAt: input.ConfirmedAt,
	}); err != nil {
		return DirectCampaign{}, err
	}
	if err := tx.Commit(); err != nil {
		return DirectCampaign{}, err
	}
	return s.getDirectCampaignForWorker(ctx, workspaceID, campaignID)
}

type directGraphRestoredEvidence struct {
	Campaign       DirectModerationSnapshot
	AdGroup        DirectModerationSnapshot
	Ad             DirectModerationSnapshot
	ProviderStatus string
	ProviderState  string
}

func directGraphFailureBaseline(
	ctx context.Context, tx *sql.Tx, operation DirectProviderOperation,
) (DirectCampaignRevision, string, error) {
	if !directGraphHashPattern.MatchString(operation.ExpectedGraphHash) ||
		!directRevisionIDPattern.MatchString(operation.ExpectedRevisionID) {
		return DirectCampaignRevision{}, "", ErrDirectGraphUnverified
	}
	revision, err := scanDirectCampaignRevision(tx.QueryRowContext(ctx,
		`SELECT `+directCampaignRevisionColumns+`
FROM direct_campaign_revisions
WHERE workspace_id=$1 AND campaign_id=$2 AND id=$3
FOR SHARE`, operation.WorkspaceID, operation.CampaignID,
		operation.ExpectedRevisionID))
	if err != nil {
		return DirectCampaignRevision{}, "", err
	}
	if revision.GraphHash != operation.ExpectedGraphHash ||
		revision.CampaignVersion != operation.ExpectedCampaignVersion ||
		operation.ProviderCampaignID == nil ||
		*operation.ProviderCampaignID != revision.ProviderCampaignID ||
		operation.ProviderAdGroupID == nil ||
		*operation.ProviderAdGroupID != revision.ProviderAdGroupID ||
		operation.ProviderAdID == nil ||
		*operation.ProviderAdID != revision.ProviderAdID ||
		!sameDirectKeywordProviderIDs(
			operation.ProviderKeywordMappings,
			revision.ProviderKeywordMappings,
		) {
		return DirectCampaignRevision{}, "", ErrDirectConsentMismatch
	}
	if err := validateDirectObservedGraphStructure(
		revision.ObservedGraph, revision.ProviderCampaignID,
		revision.ProviderAdGroupID, revision.ProviderAdID,
		revision.ProviderKeywordMappings,
	); err != nil {
		return DirectCampaignRevision{}, "", err
	}
	var marker string
	err = tx.QueryRowContext(ctx, `SELECT operation_marker
FROM direct_provider_operations
WHERE workspace_id=$1 AND campaign_id=$2 AND revision_id=$3
  AND id<>$4 AND stage='completed' AND completed_at IS NOT NULL
ORDER BY completed_at DESC,id DESC
LIMIT 1
FOR SHARE`, operation.WorkspaceID, operation.CampaignID, revision.ID,
		operation.ID).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		return DirectCampaignRevision{}, "", ErrDirectGraphUnverified
	}
	if err != nil {
		return DirectCampaignRevision{}, "", err
	}
	marker = strings.TrimSpace(marker)
	if !directProviderOperationMarkerPattern.MatchString(marker) {
		return DirectCampaignRevision{}, "", ErrDirectGraphUnverified
	}
	return revision, marker, nil
}

func directGraphRevisionEvidence(
	revision DirectCampaignRevision,
) (directGraphRestoredEvidence, error) {
	var observed struct {
		Campaign struct {
			Status string `json:"status"`
			State  string `json:"state"`
		} `json:"campaign"`
		AdGroups []struct {
			Status        string `json:"status"`
			ServingStatus string `json:"serving_status"`
		} `json:"ad_groups"`
		Ads []struct {
			Status              string `json:"status"`
			State               string `json:"state"`
			StatusClarification string `json:"status_clarification"`
		} `json:"ads"`
	}
	if err := json.Unmarshal(revision.ObservedGraph, &observed); err != nil ||
		len(observed.AdGroups) != 1 || len(observed.Ads) != 1 {
		return directGraphRestoredEvidence{}, ErrDirectGraphUnverified
	}
	providerStatus := normalizeDirectProviderStatus(observed.Campaign.Status)
	providerState := strings.ToUpper(strings.TrimSpace(observed.Campaign.State))
	if providerStatus == "" || (providerState != "OFF" && providerState != "ON") {
		return directGraphRestoredEvidence{}, ErrDirectGraphUnverified
	}
	return directGraphRestoredEvidence{
		Campaign: normalizeDirectModerationSnapshot(DirectModerationSnapshot{
			Status: observed.Campaign.Status,
			State:  observed.Campaign.State,
		}),
		AdGroup: normalizeDirectModerationSnapshot(DirectModerationSnapshot{
			Status:        observed.AdGroups[0].Status,
			ServingStatus: observed.AdGroups[0].ServingStatus,
		}),
		Ad: normalizeDirectModerationSnapshot(DirectModerationSnapshot{
			Status:              observed.Ads[0].Status,
			State:               observed.Ads[0].State,
			StatusClarification: observed.Ads[0].StatusClarification,
		}),
		ProviderStatus: providerStatus,
		ProviderState:  providerState,
	}, nil
}

func sanitizeDirectGraphFailureCode(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	digits := strings.TrimPrefix(value, "provider_validation_")
	if len(digits) == 0 || len(digits) > 10 {
		return "provider_validation_rejected"
	}
	for _, char := range digits {
		if char < '0' || char > '9' {
			return "provider_validation_rejected"
		}
	}
	return "provider_validation_" + digits
}
