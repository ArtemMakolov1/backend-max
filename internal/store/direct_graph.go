package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	DirectGraphFingerprintVersion = "yandex-direct-campaign-graph-v1"

	// DirectProviderOperationLease is longer than the app workflow deadline.
	// An expired worker cannot start another provider request, and recovery
	// still waits through the remaining safety margin before acquiring a new
	// fencing generation.
	DirectProviderOperationLease = 10 * time.Minute
	directProviderOperationLease = DirectProviderOperationLease
	directMaxTitles              = 7
	directMaxTexts               = 3
	directMaxKeywords            = 50
	directMaxNegativeKeywords    = 50
	directMaxTitleRunes          = 56
	directMaxTitleWordRunes      = 22
	directMaxTextRunes           = 81
	directMaxTextWordRunes       = 23
	directMaxKeywordRunes        = 4096
	directMaxKeywordWords        = 7
	directMaxKeywordWordRunes    = 35
)

var directProviderOperationMarkerPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
var directGraphHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var directRevisionIDPattern = regexp.MustCompile(`^drev_[0-9a-f]{32}$`)
var directMoscowLocation = time.FixedZone("Europe/Moscow", 3*60*60)

const directProviderOperationColumns = `id,workspace_id,campaign_id,connection_id,
actor_user_id,operation_kind,operation_marker,expected_campaign_version,
expected_graph_hash,COALESCE(expected_revision_id,''),stage,desired_graph,
observed_graph,provider_campaign_id,provider_ad_group_id,provider_ad_id,
provider_keyword_mappings,provider_warnings,graph_hash,COALESCE(revision_id,''),
last_provider_error_code,last_provider_clarification,claimed_at,lease_expires_at,
completed_at,created_at,updated_at`

const directCampaignRevisionColumns = `id,workspace_id,campaign_id,connection_id,
revision_number,campaign_version,graph_version,desired_graph,observed_graph,graph_hash,
provider_campaign_id,provider_ad_group_id,provider_ad_id,provider_keyword_mappings,
provider_warnings,moderation_status,moderation_clarification,actor_user_id,observed_at,
created_at`

type DirectProviderIssue struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
	Details string `json:"details,omitempty"`
}

type DirectModerationSnapshot struct {
	Status              string `json:"status,omitempty"`
	State               string `json:"state,omitempty"`
	ServingStatus       string `json:"serving_status,omitempty"`
	StatusClarification string `json:"status_clarification,omitempty"`
}

type DirectKeywordMapping struct {
	Keyword           string                   `json:"keyword"`
	ProviderKeywordID int64                    `json:"provider_keyword_id"`
	Moderation        DirectModerationSnapshot `json:"moderation"`
}

type DirectCampaignDesiredGraph struct {
	Name              string    `json:"name"`
	LandingURL        string    `json:"landing_url"`
	Regions           []string  `json:"regions"`
	WeeklyBudgetMinor int64     `json:"weekly_budget_minor"`
	CurrencyCode      string    `json:"currency_code"`
	StartsAt          time.Time `json:"starts_at"`
	EndsAt            time.Time `json:"ends_at"`
	Titles            []string  `json:"titles"`
	Texts             []string  `json:"texts"`
	Keywords          []string  `json:"keywords"`
	NegativeKeywords  []string  `json:"negative_keywords"`
}

type DirectProviderOperation struct {
	ID                        string                 `json:"id"`
	WorkspaceID               string                 `json:"workspace_id"`
	CampaignID                string                 `json:"campaign_id"`
	ConnectionID              string                 `json:"connection_id"`
	ActorUserID               string                 `json:"actor_user_id"`
	OperationKind             string                 `json:"operation_kind"`
	OperationMarker           string                 `json:"operation_marker"`
	ExpectedCampaignVersion   int64                  `json:"expected_campaign_version"`
	ExpectedGraphHash         string                 `json:"expected_graph_hash,omitempty"`
	ExpectedRevisionID        string                 `json:"expected_revision_id,omitempty"`
	Stage                     string                 `json:"stage"`
	DesiredGraph              json.RawMessage        `json:"desired_graph"`
	ObservedGraph             json.RawMessage        `json:"observed_graph,omitempty"`
	ProviderCampaignID        *int64                 `json:"provider_campaign_id,omitempty"`
	ProviderAdGroupID         *int64                 `json:"provider_ad_group_id,omitempty"`
	ProviderAdID              *int64                 `json:"provider_ad_id,omitempty"`
	ProviderKeywordMappings   []DirectKeywordMapping `json:"provider_keyword_mappings"`
	ProviderWarnings          []DirectProviderIssue  `json:"provider_warnings"`
	GraphHash                 string                 `json:"graph_hash,omitempty"`
	RevisionID                string                 `json:"revision_id,omitempty"`
	LastProviderErrorCode     string                 `json:"last_provider_error_code,omitempty"`
	LastProviderClarification string                 `json:"last_provider_clarification,omitempty"`
	ClaimedAt                 time.Time              `json:"claimed_at"`
	LeaseExpiresAt            time.Time              `json:"lease_expires_at"`
	CompletedAt               *time.Time             `json:"completed_at,omitempty"`
	CreatedAt                 time.Time              `json:"created_at"`
	UpdatedAt                 time.Time              `json:"updated_at"`
}

type DirectGraphSubmissionMaterial struct {
	Campaign        DirectCampaign
	DesiredCampaign DirectCampaign
	Connection      DirectConnection
	Operation       DirectProviderOperation
	TokenCiphertext string `json:"-"`
	TokenKeyVersion int    `json:"-"`
}

type DirectGraphRecoveryCandidate struct {
	WorkspaceID   string `json:"workspace_id"`
	CampaignID    string `json:"campaign_id"`
	OperationID   string `json:"operation_id"`
	OperationKind string `json:"operation_kind"`
	Stage         string `json:"stage"`
}

type DirectProviderStageUpdate struct {
	ExpectedClaimedAt         time.Time
	Stage                     string
	ProviderCampaignID        *int64
	ProviderAdGroupID         *int64
	ProviderAdID              *int64
	ProviderKeywordMappings   *[]DirectKeywordMapping
	ProviderWarnings          *[]DirectProviderIssue
	ObservedGraph             json.RawMessage
	GraphHash                 string
	RevisionID                string
	LastProviderErrorCode     string
	LastProviderClarification string
	Complete                  bool
}

type DirectVerifiedGraphInput struct {
	ExpectedOperationID              string
	ExpectedStage                    string
	ExpectedCampaignVersion          int64
	ExpectedClaimedAt                time.Time
	GraphVersion                     string
	DesiredGraph                     json.RawMessage
	ObservedGraph                    json.RawMessage
	GraphHash                        string
	ProviderCampaignID               int64
	ProviderAdGroupID                int64
	ProviderAdID                     int64
	ProviderKeywordMappings          []DirectKeywordMapping
	ProviderWarnings                 []DirectProviderIssue
	CampaignModeration               DirectModerationSnapshot
	AdGroupModeration                DirectModerationSnapshot
	AdModeration                     DirectModerationSnapshot
	AggregateModerationStatus        string
	AggregateModerationClarification string
	ObservedAt                       time.Time
	ActorUserID                      string
}

type DirectCampaignRevision struct {
	ID                      string                 `json:"id"`
	WorkspaceID             string                 `json:"workspace_id"`
	CampaignID              string                 `json:"campaign_id"`
	ConnectionID            string                 `json:"connection_id"`
	RevisionNumber          int64                  `json:"revision_number"`
	CampaignVersion         int64                  `json:"campaign_version"`
	GraphVersion            string                 `json:"graph_version"`
	DesiredGraph            json.RawMessage        `json:"desired_graph"`
	ObservedGraph           json.RawMessage        `json:"observed_graph"`
	GraphHash               string                 `json:"graph_hash"`
	ProviderCampaignID      int64                  `json:"provider_campaign_id"`
	ProviderAdGroupID       int64                  `json:"provider_ad_group_id"`
	ProviderAdID            int64                  `json:"provider_ad_id"`
	ProviderKeywordMappings []DirectKeywordMapping `json:"provider_keyword_mappings"`
	ProviderWarnings        []DirectProviderIssue  `json:"provider_warnings"`
	ModerationStatus        string                 `json:"moderation_status"`
	ModerationClarification string                 `json:"moderation_clarification,omitempty"`
	ActorUserID             string                 `json:"actor_user_id"`
	ObservedAt              time.Time              `json:"observed_at"`
	CreatedAt               time.Time              `json:"created_at"`
}

type DirectGraphModerationUpdate struct {
	ExpectedGraphHash                string
	ExpectedRevisionID               string
	Campaign                         DirectModerationSnapshot
	AdGroup                          DirectModerationSnapshot
	Ad                               DirectModerationSnapshot
	Keywords                         []DirectKeywordMapping
	AggregateModerationStatus        string
	AggregateModerationClarification string
	ProviderStatus                   string
	ProviderState                    string
	StatusClarification              string
	CheckedAt                        time.Time
}

func (s *Store) ClaimDirectCampaignGraphSubmission(
	ctx context.Context, actorUserID, workspaceID, campaignID string,
	expectedVersion int64, operationMarker string, now time.Time,
) (DirectGraphSubmissionMaterial, error) {
	return s.claimDirectCampaignGraphOperation(
		ctx, actorUserID, workspaceID, campaignID, expectedVersion,
		operationMarker, "submission", DirectCampaignChanges{}, "", "", now,
	)
}

func (s *Store) ClaimDirectCampaignProviderEdit(
	ctx context.Context, actorUserID, workspaceID, campaignID string,
	changes DirectCampaignChanges, expectedGraphHash, expectedRevisionID,
	operationMarker string, now time.Time,
) (DirectGraphSubmissionMaterial, error) {
	if !directGraphHashPattern.MatchString(strings.TrimSpace(expectedGraphHash)) ||
		strings.TrimSpace(expectedRevisionID) == "" {
		return DirectGraphSubmissionMaterial{}, ErrDirectGraphUnverified
	}
	return s.claimDirectCampaignGraphOperation(
		ctx, actorUserID, workspaceID, campaignID, changes.ExpectedVersion,
		operationMarker, "update", changes, strings.TrimSpace(expectedGraphHash),
		strings.TrimSpace(expectedRevisionID), now,
	)
}

func (s *Store) claimDirectCampaignGraphOperation(
	ctx context.Context, actorUserID, workspaceID, campaignID string,
	expectedVersion int64, operationMarker, operationKind string,
	changes DirectCampaignChanges, expectedGraphHash, expectedRevisionID string,
	now time.Time,
) (DirectGraphSubmissionMaterial, error) {
	if expectedVersion <= 0 ||
		!directProviderOperationMarkerPattern.MatchString(strings.TrimSpace(operationMarker)) ||
		(operationKind != "submission" && operationKind != "update") {
		return DirectGraphSubmissionMaterial{}, fmt.Errorf(
			"%w: invalid provider operation claim", ErrDirectValidation,
		)
	}
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.Truncate(time.Microsecond)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DirectGraphSubmissionMaterial{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(
		ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor,
	); err != nil {
		return DirectGraphSubmissionMaterial{}, err
	}
	campaign, err := scanDirectCampaign(tx.QueryRowContext(ctx, `SELECT `+directCampaignColumns+`
FROM direct_campaigns WHERE workspace_id=$1 AND id=$2 FOR UPDATE`,
		workspaceID, campaignID))
	if err != nil {
		return DirectGraphSubmissionMaterial{}, err
	}
	operationMarker = strings.TrimSpace(operationMarker)
	if campaign.SubmissionOperationID != "" {
		operation, operationErr := scanDirectProviderOperation(
			tx.QueryRowContext(ctx, `SELECT `+directProviderOperationColumns+`
FROM direct_provider_operations
WHERE workspace_id=$1 AND campaign_id=$2 AND id=$3 FOR UPDATE`,
				workspaceID, campaignID, campaign.SubmissionOperationID),
		)
		if operationErr != nil {
			return DirectGraphSubmissionMaterial{}, operationErr
		}
		// A live lease is exclusive. In particular, an HTTP retry using the
		// same marker must not receive material while the first worker can
		// still issue provider writes. Only an expired, exactly matching
		// operation may be reclaimed below.
		if operation.CompletedAt == nil && operation.LeaseExpiresAt.After(now) {
			return DirectGraphSubmissionMaterial{}, ErrDirectProviderOperationBusy
		}
		requestDesired, requestDesiredErr := directRequestedGraphCampaign(
			campaign, operationKind, changes,
		)
		if requestDesiredErr != nil {
			return DirectGraphSubmissionMaterial{}, requestDesiredErr
		}
		requestDesiredJSON, requestDesiredErr := marshalDirectDesiredGraph(
			requestDesired,
		)
		if requestDesiredErr != nil {
			return DirectGraphSubmissionMaterial{}, requestDesiredErr
		}
		requestMatches := operation.OperationKind == operationKind &&
			operation.ExpectedCampaignVersion == expectedVersion &&
			operation.ActorUserID == actorUserID &&
			operation.ExpectedGraphHash == expectedGraphHash &&
			operation.ExpectedRevisionID == expectedRevisionID &&
			bytes.Equal(operation.DesiredGraph, requestDesiredJSON)
		// The campaign marker describes the currently verified provider graph.
		// A failed provider edit restores that baseline marker, while the
		// operation keeps the request marker needed for idempotent HTTP retry.
		sameMarker := operation.OperationMarker == operationMarker
		staleOwnedOperation := operation.CompletedAt == nil &&
			!operation.LeaseExpiresAt.After(now) && requestMatches
		if sameMarker && !requestMatches {
			return DirectGraphSubmissionMaterial{}, ErrConflict
		}
		if !sameMarker && !staleOwnedOperation {
			operation = DirectProviderOperation{}
		} else if operation.OperationKind != operationKind ||
			operation.ExpectedCampaignVersion != expectedVersion ||
			operation.ActorUserID != actorUserID ||
			operation.ExpectedGraphHash != expectedGraphHash ||
			operation.ExpectedRevisionID != expectedRevisionID {
			return DirectGraphSubmissionMaterial{}, ErrConflict
		} else {
			connection, connectionErr := scanDirectConnection(tx.QueryRowContext(ctx,
				`SELECT `+directConnectionColumns+`
FROM direct_connections
WHERE workspace_id=$1 AND id=$2 AND status='active' AND read_only=FALSE
  AND revoked_at IS NULL FOR SHARE`, workspaceID, operation.ConnectionID))
			if connectionErr != nil {
				return DirectGraphSubmissionMaterial{}, ErrDirectConnectionRequired
			}
			if operation.CompletedAt == nil {
				operation.ClaimedAt = now
				operation.LeaseExpiresAt = now.Add(directProviderOperationLease)
				operation.UpdatedAt = now
				if _, updateErr := tx.ExecContext(ctx, `UPDATE direct_provider_operations
SET claimed_at=$1,lease_expires_at=$2,updated_at=$1
WHERE workspace_id=$3 AND campaign_id=$4 AND id=$5
  AND completed_at IS NULL`, now, operation.LeaseExpiresAt, workspaceID,
					campaignID, operation.ID); updateErr != nil {
					return DirectGraphSubmissionMaterial{}, updateErr
				}
				if _, updateErr := tx.ExecContext(ctx, `UPDATE direct_campaigns
SET submission_claimed_at=$1,submission_lease_expires_at=$2,updated_at=$1
WHERE workspace_id=$3 AND id=$4 AND submission_operation_id=$5`,
					now, operation.LeaseExpiresAt, workspaceID, campaignID,
					operation.ID); updateErr != nil {
					return DirectGraphSubmissionMaterial{}, updateErr
				}
				campaign.SubmissionClaimedAt = directTimePointer(now)
				campaign.SubmissionLeaseExpiresAt = directTimePointer(
					operation.LeaseExpiresAt,
				)
				campaign.UpdatedAt = now
				if journalErr := insertDirectProviderOperationJournal(
					ctx, tx, operation, now,
				); journalErr != nil {
					return DirectGraphSubmissionMaterial{}, journalErr
				}
			}
			desired, desiredErr := directDesiredCampaignFromJSON(
				campaign, operation.DesiredGraph,
			)
			if desiredErr != nil {
				return DirectGraphSubmissionMaterial{}, desiredErr
			}
			if err := tx.Commit(); err != nil {
				return DirectGraphSubmissionMaterial{}, err
			}
			return DirectGraphSubmissionMaterial{
				Campaign: campaign, DesiredCampaign: desired,
				Connection: connection, Operation: operation,
				TokenCiphertext: connection.TokenCiphertext,
				TokenKeyVersion: connection.TokenKeyVersion,
			}, nil
		}
	}
	if campaign.Version != expectedVersion {
		return DirectGraphSubmissionMaterial{}, ErrConflict
	}
	if campaign.SubmissionOperationID != "" &&
		campaign.SubmissionStage != "completed" &&
		campaign.SubmissionStage != "failed" {
		return DirectGraphSubmissionMaterial{}, ErrDirectProviderOperationBusy
	}
	desired := campaign
	switch operationKind {
	case "submission":
		if campaign.Status != "draft" || campaign.ProviderCampaignID != nil {
			return DirectGraphSubmissionMaterial{}, ErrDirectCampaignNotDraft
		}
	case "update":
		if err := directCampaignGraphEvidenceReady(campaign); err != nil {
			return DirectGraphSubmissionMaterial{}, err
		}
		if campaign.ProviderGraphHash != expectedGraphHash ||
			campaign.ProviderRevisionID != expectedRevisionID {
			return DirectGraphSubmissionMaterial{}, ErrDirectConsentMismatch
		}
		if !strings.EqualFold(campaign.ProviderState, "OFF") ||
			campaign.Status == "active" || campaign.LaunchState != "idle" ||
			campaign.LaunchedAt != nil {
			return DirectGraphSubmissionMaterial{}, ErrDirectLaunchAlreadyClaimed
		}
		if !directProviderEditLocalMetadataUnchanged(campaign, changes) {
			return DirectGraphSubmissionMaterial{}, fmt.Errorf(
				"%w: provider edit cannot change local-only metadata",
				ErrDirectValidation,
			)
		}
		applyDirectCampaignChanges(&desired, changes)
		if err := validateDirectCampaignDraft(&desired); err != nil {
			return DirectGraphSubmissionMaterial{}, fmt.Errorf(
				"%w: %w", ErrDirectValidation, err,
			)
		}
	}
	if err := validateDirectProviderOperationDates(desired, now); err != nil {
		return DirectGraphSubmissionMaterial{}, err
	}
	connection, err := scanDirectConnection(tx.QueryRowContext(ctx,
		`SELECT `+directConnectionColumns+`
FROM direct_connections
WHERE workspace_id=$1 AND id=$2 AND status='active' AND read_only=FALSE
  AND revoked_at IS NULL FOR SHARE`, workspaceID, campaign.ConnectionID))
	if err != nil {
		return DirectGraphSubmissionMaterial{}, ErrDirectConnectionRequired
	}
	desiredJSON, err := marshalDirectDesiredGraph(desired)
	if err != nil {
		return DirectGraphSubmissionMaterial{}, err
	}
	operation := DirectProviderOperation{
		ID: newStoreID("dpop_"), WorkspaceID: workspaceID, CampaignID: campaignID,
		ConnectionID: campaign.ConnectionID, ActorUserID: actorUserID,
		OperationKind: operationKind, OperationMarker: operationMarker,
		ExpectedCampaignVersion: expectedVersion, Stage: "claimed",
		DesiredGraph: desiredJSON, ProviderKeywordMappings: []DirectKeywordMapping{},
		ProviderWarnings: []DirectProviderIssue{}, ClaimedAt: now,
		LeaseExpiresAt: now.Add(directProviderOperationLease),
		CreatedAt:      now, UpdatedAt: now,
	}
	if operationKind == "update" {
		operation.ExpectedGraphHash = expectedGraphHash
		operation.ExpectedRevisionID = expectedRevisionID
		operation.ProviderCampaignID = cloneInt64Pointer(campaign.ProviderCampaignID)
		operation.ProviderAdGroupID = cloneInt64Pointer(campaign.ProviderAdGroupID)
		operation.ProviderAdID = cloneInt64Pointer(campaign.ProviderAdID)
		operation.ProviderKeywordMappings = append(
			[]DirectKeywordMapping{}, campaign.ProviderKeywordMappings...,
		)
		operation.ProviderWarnings = append(
			[]DirectProviderIssue{}, campaign.ProviderWarnings...,
		)
	}
	if operation.ProviderKeywordMappings == nil {
		operation.ProviderKeywordMappings = []DirectKeywordMapping{}
	}
	if operation.ProviderWarnings == nil {
		operation.ProviderWarnings = []DirectProviderIssue{}
	}
	initialMappingsJSON, _ := json.Marshal(operation.ProviderKeywordMappings)
	initialWarningsJSON, _ := json.Marshal(operation.ProviderWarnings)
	_, err = tx.ExecContext(ctx, `INSERT INTO direct_provider_operations(
id,workspace_id,campaign_id,connection_id,actor_user_id,operation_kind,
operation_marker,expected_campaign_version,expected_graph_hash,
expected_revision_id,stage,desired_graph,provider_keyword_mappings,
provider_warnings,provider_campaign_id,provider_ad_group_id,provider_ad_id,
claimed_at,lease_expires_at,
created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$20)`,
		operation.ID, workspaceID, campaignID, campaign.ConnectionID, actorUserID,
		operationKind, operation.OperationMarker, expectedVersion,
		operation.ExpectedGraphHash, operation.ExpectedRevisionID, operation.Stage,
		string(desiredJSON), string(initialMappingsJSON), string(initialWarningsJSON),
		operation.ProviderCampaignID, operation.ProviderAdGroupID,
		operation.ProviderAdID, now, operation.LeaseExpiresAt, now)
	if err != nil {
		return DirectGraphSubmissionMaterial{}, mapWorkspaceWriteError(
			"claim Direct graph operation", err,
		)
	}
	status := campaign.Status
	if operationKind == "submission" {
		status = "creating"
	}
	_, err = tx.ExecContext(ctx, `UPDATE direct_campaigns
SET status=$1,submission_operation_id=$2,submission_stage='claimed',
    submission_operation_marker=$3,submission_claimed_at=$4,
    submission_lease_expires_at=$5,submission_failure_code='',
    submission_failure_clarification='',updated_at=$4
WHERE workspace_id=$6 AND id=$7 AND version=$8`,
		status, operation.ID, operation.OperationMarker, now, operation.LeaseExpiresAt,
		workspaceID, campaignID, expectedVersion)
	if err != nil {
		return DirectGraphSubmissionMaterial{}, err
	}
	if operationKind == "update" {
		if _, err := tx.ExecContext(ctx, `UPDATE direct_auto_launch_consents_v2
SET invalidated_at=$1,invalid_reason='provider_edit_claimed'
WHERE workspace_id=$2 AND campaign_id=$3
  AND revoked_at IS NULL AND invalidated_at IS NULL AND consumed_at IS NULL`,
			now, workspaceID, campaignID); err != nil {
			return DirectGraphSubmissionMaterial{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE direct_campaigns
SET provider_graph_hash='',provider_revision_id=NULL,graph_verified_at=NULL,
    provider_warnings='[]'::jsonb,moderation_status='',
    moderation_clarification='',campaign_moderation='{}'::jsonb,
    ad_group_moderation='{}'::jsonb,ad_moderation='{}'::jsonb,
    keyword_moderation='[]'::jsonb,updated_at=$1
WHERE workspace_id=$2 AND id=$3 AND submission_operation_id=$4`,
			now, workspaceID, campaignID, operation.ID); err != nil {
			return DirectGraphSubmissionMaterial{}, err
		}
		campaign.ProviderGraphHash = ""
		campaign.ProviderRevisionID = ""
		campaign.GraphVerifiedAt = nil
		campaign.ProviderWarnings = []DirectProviderIssue{}
		campaign.ModerationStatus = ""
		campaign.ModerationClarification = ""
		campaign.CampaignModeration = DirectModerationSnapshot{}
		campaign.AdGroupModeration = DirectModerationSnapshot{}
		campaign.AdModeration = DirectModerationSnapshot{}
		campaign.KeywordModeration = []DirectKeywordMapping{}
	}
	if err := insertDirectProviderOperationJournal(
		ctx, tx, operation, now,
	); err != nil {
		return DirectGraphSubmissionMaterial{}, err
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID,
		Action:     "direct.campaign.graph_operation_claimed",
		EntityType: "direct_campaign", EntityID: campaignID,
		Metadata: mustJSON(map[string]any{
			"operation_id": operation.ID, "operation_kind": operationKind,
			"operation_marker": operation.OperationMarker,
			"expected_version": expectedVersion,
		}), CreatedAt: now,
	}); err != nil {
		return DirectGraphSubmissionMaterial{}, err
	}
	if err := tx.Commit(); err != nil {
		return DirectGraphSubmissionMaterial{}, err
	}
	campaign.Status = status
	campaign.SubmissionOperationID = operation.ID
	campaign.SubmissionStage = operation.Stage
	campaign.SubmissionOperationMarker = operation.OperationMarker
	campaign.SubmissionClaimedAt = directTimePointer(now)
	campaign.SubmissionLeaseExpiresAt = directTimePointer(operation.LeaseExpiresAt)
	campaign.UpdatedAt = now
	return DirectGraphSubmissionMaterial{
		Campaign: campaign, DesiredCampaign: desired, Connection: connection,
		Operation: operation, TokenCiphertext: connection.TokenCiphertext,
		TokenKeyVersion: connection.TokenKeyVersion,
	}, nil
}

func (s *Store) GetDirectCampaignGraphSubmissionMaterial(
	ctx context.Context, workspaceID, campaignID, operationID string, now time.Time,
) (DirectGraphSubmissionMaterial, error) {
	now = directProviderOperationReadTime(now)
	return s.loadDirectCampaignGraphSubmissionMaterial(
		ctx, workspaceID, campaignID, operationID, now,
	)
}

// ReloadDirectCampaignGraphSubmission is retained for owned workflow
// continuations. It is deliberately read-only: lease ownership is acquired
// only by ClaimDirectCampaignGraphSubmission,
// ClaimDirectCampaignProviderEdit, or
// ClaimDirectCampaignGraphRecoveryCandidates.
func (s *Store) ReloadDirectCampaignGraphSubmission(
	ctx context.Context, workspaceID, campaignID, operationID string, now time.Time,
) (DirectGraphSubmissionMaterial, error) {
	now = directProviderOperationReadTime(now)
	return s.loadDirectCampaignGraphSubmissionMaterial(
		ctx, workspaceID, campaignID, operationID, now,
	)
}

func (s *Store) loadDirectCampaignGraphSubmissionMaterial(
	ctx context.Context, workspaceID, campaignID, operationID string, now time.Time,
) (DirectGraphSubmissionMaterial, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DirectGraphSubmissionMaterial{}, err
	}
	defer func() { _ = tx.Rollback() }()
	campaign, err := scanDirectCampaign(tx.QueryRowContext(ctx, `SELECT `+directCampaignColumns+`
FROM direct_campaigns WHERE workspace_id=$1 AND id=$2 FOR SHARE`,
		workspaceID, campaignID))
	if err != nil {
		return DirectGraphSubmissionMaterial{}, err
	}
	operation, err := scanDirectProviderOperation(tx.QueryRowContext(ctx,
		`SELECT `+directProviderOperationColumns+`
FROM direct_provider_operations
WHERE workspace_id=$1 AND campaign_id=$2 AND id=$3 FOR SHARE`,
		workspaceID, campaignID, operationID))
	if err != nil {
		return DirectGraphSubmissionMaterial{}, err
	}
	if campaign.SubmissionOperationID != operation.ID ||
		operation.Stage == "completed" || operation.Stage == "failed" {
		return DirectGraphSubmissionMaterial{}, ErrConflict
	}
	if operation.LeaseExpiresAt.Before(now) {
		return DirectGraphSubmissionMaterial{}, ErrDirectProviderOperationStale
	}
	connection, err := scanDirectConnection(tx.QueryRowContext(ctx,
		`SELECT `+directConnectionColumns+`
FROM direct_connections
WHERE workspace_id=$1 AND id=$2 AND status='active' AND read_only=FALSE
  AND revoked_at IS NULL FOR SHARE`, workspaceID, operation.ConnectionID))
	if err != nil {
		return DirectGraphSubmissionMaterial{}, ErrDirectConnectionRequired
	}
	desired, err := directDesiredCampaignFromJSON(campaign, operation.DesiredGraph)
	if err != nil {
		return DirectGraphSubmissionMaterial{}, err
	}
	if err := tx.Commit(); err != nil {
		return DirectGraphSubmissionMaterial{}, err
	}
	return DirectGraphSubmissionMaterial{
		Campaign: campaign, DesiredCampaign: desired, Connection: connection,
		Operation: operation, TokenCiphertext: connection.TokenCiphertext,
		TokenKeyVersion: connection.TokenKeyVersion,
	}, nil
}

func directProviderOperationReadTime(now time.Time) time.Time {
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return now.Truncate(time.Microsecond)
}

func (s *Store) ClaimDirectCampaignGraphRecoveryCandidates(
	ctx context.Context, now time.Time, limit int,
) ([]DirectGraphRecoveryCandidate, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.Truncate(time.Microsecond)
	leaseExpiresAt := now.Add(directProviderOperationLease)
	rows, err := s.db.QueryContext(ctx, `WITH candidates AS (
    SELECT o.id
    FROM direct_provider_operations o
    JOIN direct_campaigns c
      ON c.workspace_id=o.workspace_id
     AND c.id=o.campaign_id
     AND c.submission_operation_id=o.id
     AND c.submission_stage=o.stage
    JOIN workspaces w ON w.id=o.workspace_id AND w.archived_at IS NULL
    WHERE o.completed_at IS NULL
      AND o.stage NOT IN ('completed','failed')
      AND o.lease_expires_at <= $1
    ORDER BY o.lease_expires_at,o.id
    FOR UPDATE OF c,o SKIP LOCKED
    LIMIT $2
), leased AS (
    UPDATE direct_provider_operations o
    SET claimed_at=$1,lease_expires_at=$3,updated_at=$1
    FROM candidates x
    WHERE o.id=x.id
      AND o.completed_at IS NULL
      AND o.stage NOT IN ('completed','failed')
      AND o.lease_expires_at <= $1
    RETURNING o.*
), refreshed AS (
    UPDATE direct_campaigns c
    SET submission_claimed_at=$1,submission_lease_expires_at=$3,updated_at=$1
    FROM leased l
    WHERE c.workspace_id=l.workspace_id
      AND c.id=l.campaign_id
      AND c.submission_operation_id=l.id
      AND c.submission_stage=l.stage
    RETURNING c.workspace_id,c.id,c.submission_operation_id
), journaled AS (
    INSERT INTO direct_provider_operation_journal(
        workspace_id,campaign_id,operation_id,stage,snapshot,recorded_at
    )
    SELECT l.workspace_id,l.campaign_id,l.id,l.stage,to_jsonb(l),$1
    FROM leased l
    RETURNING workspace_id,campaign_id,operation_id
)
SELECT l.workspace_id,l.campaign_id,l.id,l.operation_kind,l.stage
FROM leased l
JOIN refreshed r
  ON r.workspace_id=l.workspace_id
 AND r.id=l.campaign_id
 AND r.submission_operation_id=l.id
JOIN journaled j
  ON j.workspace_id=l.workspace_id
 AND j.campaign_id=l.campaign_id
 AND j.operation_id=l.id
ORDER BY l.workspace_id,l.campaign_id,l.id`,
		now, limit, leaseExpiresAt)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	candidates := make([]DirectGraphRecoveryCandidate, 0)
	for rows.Next() {
		var candidate DirectGraphRecoveryCandidate
		if err := rows.Scan(
			&candidate.WorkspaceID, &candidate.CampaignID,
			&candidate.OperationID, &candidate.OperationKind, &candidate.Stage,
		); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return candidates, nil
}

func (s *Store) AdvanceDirectCampaignGraphSubmission(
	ctx context.Context, workspaceID, campaignID, operationID, expectedStage string,
	update DirectProviderStageUpdate, now time.Time,
) (DirectGraphSubmissionMaterial, error) {
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.Truncate(time.Microsecond)
	update.Stage = strings.TrimSpace(update.Stage)
	expectedStage = strings.TrimSpace(expectedStage)
	update.ExpectedClaimedAt = update.ExpectedClaimedAt.UTC().Truncate(time.Microsecond)
	if update.Stage == "" || expectedStage == "" ||
		update.ExpectedClaimedAt.IsZero() {
		return DirectGraphSubmissionMaterial{}, fmt.Errorf(
			"%w: provider stage and fencing claim are required",
			ErrDirectValidation,
		)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DirectGraphSubmissionMaterial{}, err
	}
	defer func() { _ = tx.Rollback() }()
	campaign, err := scanDirectCampaign(tx.QueryRowContext(ctx, `SELECT `+directCampaignColumns+`
FROM direct_campaigns WHERE workspace_id=$1 AND id=$2 FOR UPDATE`,
		workspaceID, campaignID))
	if err != nil {
		return DirectGraphSubmissionMaterial{}, err
	}
	operation, err := scanDirectProviderOperation(tx.QueryRowContext(ctx,
		`SELECT `+directProviderOperationColumns+`
FROM direct_provider_operations
WHERE workspace_id=$1 AND campaign_id=$2 AND id=$3 FOR UPDATE`,
		workspaceID, campaignID, operationID))
	if err != nil {
		return DirectGraphSubmissionMaterial{}, err
	}
	if campaign.SubmissionOperationID != operation.ID ||
		!operation.ClaimedAt.Equal(update.ExpectedClaimedAt) {
		return DirectGraphSubmissionMaterial{}, ErrConflict
	}
	replay := operation.Stage == update.Stage
	if !replay && (operation.Stage != expectedStage || operation.CompletedAt != nil) {
		return DirectGraphSubmissionMaterial{}, ErrConflict
	}
	if replay && expectedStage != update.Stage &&
		!validDirectProviderStageTransition(
			operation.OperationKind, expectedStage, update.Stage,
		) {
		return DirectGraphSubmissionMaterial{}, ErrConflict
	}
	if operation.CompletedAt == nil && operation.LeaseExpiresAt.Before(now) {
		return DirectGraphSubmissionMaterial{}, ErrDirectProviderOperationStale
	}
	if !replay && !validDirectProviderStageTransition(
		operation.OperationKind, operation.Stage, update.Stage,
	) {
		return DirectGraphSubmissionMaterial{}, fmt.Errorf(
			"%w: invalid provider stage transition %s to %s",
			ErrDirectValidation, operation.Stage, update.Stage,
		)
	}
	previousOperation := operation
	if err := mergeDirectProviderStageUpdate(&operation, update); err != nil {
		return DirectGraphSubmissionMaterial{}, err
	}
	if replay && !sameDirectProviderOperationResult(previousOperation, operation) {
		return DirectGraphSubmissionMaterial{}, ErrConflict
	}
	desired, err := directDesiredCampaignFromJSON(campaign, operation.DesiredGraph)
	if err != nil {
		return DirectGraphSubmissionMaterial{}, err
	}
	switch update.Stage {
	case "keywords_created", "keywords_updated", "graph_observed":
		if err := validateDirectKeywordMappings(
			operation.ProviderKeywordMappings, desired.Keywords, true,
		); err != nil {
			return DirectGraphSubmissionMaterial{}, err
		}
	}
	operation.Stage = update.Stage
	operation.UpdatedAt = now
	if replay && previousOperation.CompletedAt != nil {
		operation.CompletedAt = previousOperation.CompletedAt
		operation.LeaseExpiresAt = previousOperation.LeaseExpiresAt
	} else {
		operation.LeaseExpiresAt = now.Add(directProviderOperationLease)
		if update.Complete || update.Stage == "completed" || update.Stage == "failed" {
			operation.CompletedAt = directTimePointer(now)
			operation.LeaseExpiresAt = now
		}
	}
	if operation.ProviderKeywordMappings == nil {
		operation.ProviderKeywordMappings = []DirectKeywordMapping{}
	}
	if operation.ProviderWarnings == nil {
		operation.ProviderWarnings = []DirectProviderIssue{}
	}
	mappingsJSON, _ := json.Marshal(operation.ProviderKeywordMappings)
	warningsJSON, _ := json.Marshal(operation.ProviderWarnings)
	var observed any
	if len(operation.ObservedGraph) > 0 {
		observed = string(operation.ObservedGraph)
	}
	databaseExpectedStage := expectedStage
	if replay {
		databaseExpectedStage = operation.Stage
	}
	result, err := tx.ExecContext(ctx, `UPDATE direct_provider_operations
SET stage=$1,observed_graph=$2,provider_campaign_id=$3,provider_ad_group_id=$4,
    provider_ad_id=$5,provider_keyword_mappings=$6,provider_warnings=$7,
    graph_hash=$8,revision_id=NULLIF($9,''),last_provider_error_code=$10,
    last_provider_clarification=$11,lease_expires_at=$12,completed_at=$13,
    updated_at=$14
WHERE workspace_id=$15 AND campaign_id=$16 AND id=$17
  AND stage=$18
  AND claimed_at=$19
  AND (lease_expires_at >= $14 OR completed_at IS NOT NULL)`,
		operation.Stage, observed, operation.ProviderCampaignID,
		operation.ProviderAdGroupID, operation.ProviderAdID, string(mappingsJSON),
		string(warningsJSON), operation.GraphHash, operation.RevisionID,
		operation.LastProviderErrorCode, operation.LastProviderClarification,
		operation.LeaseExpiresAt, operation.CompletedAt, now, workspaceID,
		campaignID, operation.ID, databaseExpectedStage, update.ExpectedClaimedAt)
	if err != nil {
		return DirectGraphSubmissionMaterial{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return DirectGraphSubmissionMaterial{}, ErrConflict
	}
	providerKeywordIDs := directProviderKeywordIDs(operation.ProviderKeywordMappings)
	providerKeywordIDsJSON, _ := json.Marshal(providerKeywordIDs)
	status := campaign.Status
	if operation.OperationKind == "submission" {
		switch operation.Stage {
		case "failed":
			status = "error"
		case "moderation_requested":
			if campaign.ModerationStatus != "ACCEPTED" &&
				campaign.ModerationStatus != "REJECTED" {
				status = "moderation"
			}
		case "campaign_created", "ad_group_created", "ad_created",
			"keywords_created", "graph_observed":
			status = "provider_draft"
		case "verified", "completed":
			status = directLocalStatusForModeration(
				campaign.ModerationStatus, campaign.Status,
			)
		}
		_, err = tx.ExecContext(ctx, `UPDATE direct_campaigns
SET provider_campaign_id=$1,provider_ad_group_id=$2,provider_ad_id=$3,
    provider_keyword_ids=$4,provider_keyword_mappings=$5,provider_warnings=$6,
    status=$7,submission_stage=$8,submission_lease_expires_at=$9,
    submission_failure_code=$10,submission_failure_clarification=$11,
    updated_at=$12
WHERE workspace_id=$13 AND id=$14 AND submission_operation_id=$15`,
			operation.ProviderCampaignID, operation.ProviderAdGroupID,
			operation.ProviderAdID, string(providerKeywordIDsJSON),
			string(mappingsJSON), string(warningsJSON), status, operation.Stage,
			operation.LeaseExpiresAt, operation.LastProviderErrorCode,
			operation.LastProviderClarification, now, workspaceID, campaignID,
			operation.ID)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE direct_campaigns
SET provider_warnings=$1,submission_stage=$2,submission_lease_expires_at=$3,
    submission_failure_code=$4,submission_failure_clarification=$5,updated_at=$6
WHERE workspace_id=$7 AND id=$8 AND submission_operation_id=$9`,
			string(warningsJSON), operation.Stage, operation.LeaseExpiresAt,
			operation.LastProviderErrorCode, operation.LastProviderClarification,
			now, workspaceID, campaignID, operation.ID)
	}
	if err != nil {
		return DirectGraphSubmissionMaterial{}, err
	}
	if !replay {
		if err := insertDirectProviderOperationJournal(
			ctx, tx, operation, now,
		); err != nil {
			return DirectGraphSubmissionMaterial{}, err
		}
	}
	connection, err := scanDirectConnection(tx.QueryRowContext(ctx,
		`SELECT `+directConnectionColumns+`
FROM direct_connections
WHERE workspace_id=$1 AND id=$2 AND status='active' AND read_only=FALSE
  AND revoked_at IS NULL FOR SHARE`, workspaceID, operation.ConnectionID))
	if err != nil {
		return DirectGraphSubmissionMaterial{}, ErrDirectConnectionRequired
	}
	if err := tx.Commit(); err != nil {
		return DirectGraphSubmissionMaterial{}, err
	}
	campaign.Status = status
	campaign.SubmissionStage = operation.Stage
	campaign.SubmissionLeaseExpiresAt = directTimePointer(operation.LeaseExpiresAt)
	campaign.SubmissionFailureCode = operation.LastProviderErrorCode
	campaign.SubmissionFailureClarification = operation.LastProviderClarification
	campaign.ProviderWarnings = append([]DirectProviderIssue(nil), operation.ProviderWarnings...)
	if operation.OperationKind == "submission" {
		campaign.ProviderCampaignID = cloneInt64Pointer(operation.ProviderCampaignID)
		campaign.ProviderAdGroupID = cloneInt64Pointer(operation.ProviderAdGroupID)
		campaign.ProviderAdID = cloneInt64Pointer(operation.ProviderAdID)
		campaign.ProviderKeywordMappings = append(
			[]DirectKeywordMapping(nil), operation.ProviderKeywordMappings...,
		)
		campaign.ProviderKeywordIDs = providerKeywordIDs
	}
	campaign.UpdatedAt = now
	return DirectGraphSubmissionMaterial{
		Campaign: campaign, DesiredCampaign: desired, Connection: connection,
		Operation: operation, TokenCiphertext: connection.TokenCiphertext,
		TokenKeyVersion: connection.TokenKeyVersion,
	}, nil
}

func (s *Store) RecordVerifiedDirectCampaignGraph(
	ctx context.Context, workspaceID, campaignID string, input DirectVerifiedGraphInput,
) (DirectCampaignRevision, error) {
	input.ExpectedOperationID = strings.TrimSpace(input.ExpectedOperationID)
	input.ExpectedStage = strings.TrimSpace(input.ExpectedStage)
	input.GraphVersion = strings.TrimSpace(input.GraphVersion)
	input.GraphHash = strings.TrimSpace(input.GraphHash)
	input.ActorUserID = strings.TrimSpace(input.ActorUserID)
	input.ExpectedClaimedAt = input.ExpectedClaimedAt.UTC().Truncate(time.Microsecond)
	input.ObservedAt = input.ObservedAt.UTC()
	if input.ObservedAt.IsZero() {
		input.ObservedAt = time.Now().UTC()
	}
	if input.ExpectedOperationID == "" || input.ExpectedStage == "" ||
		input.ExpectedCampaignVersion <= 0 || input.ExpectedClaimedAt.IsZero() ||
		input.GraphVersion != DirectGraphFingerprintVersion ||
		!directGraphHashPattern.MatchString(input.GraphHash) ||
		input.ProviderCampaignID <= 0 || input.ProviderAdGroupID <= 0 ||
		input.ProviderAdID <= 0 || input.ActorUserID == "" {
		return DirectCampaignRevision{}, fmt.Errorf(
			"%w: incomplete verified graph", ErrDirectValidation,
		)
	}
	if input.ProviderWarnings == nil {
		input.ProviderWarnings = []DirectProviderIssue{}
	}
	desiredGraph, err := canonicalDirectJSONObject(input.DesiredGraph)
	if err != nil {
		return DirectCampaignRevision{}, fmt.Errorf(
			"%w: invalid desired graph", ErrDirectValidation,
		)
	}
	observedGraph, err := canonicalDirectJSONObject(input.ObservedGraph)
	if err != nil {
		return DirectCampaignRevision{}, fmt.Errorf(
			"%w: invalid observed graph", ErrDirectValidation,
		)
	}
	if err := validateDirectObservedGraphStructure(
		observedGraph, input.ProviderCampaignID, input.ProviderAdGroupID,
		input.ProviderAdID, input.ProviderKeywordMappings,
	); err != nil {
		return DirectCampaignRevision{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DirectCampaignRevision{}, err
	}
	defer func() { _ = tx.Rollback() }()
	campaign, err := scanDirectCampaign(tx.QueryRowContext(ctx, `SELECT `+directCampaignColumns+`
FROM direct_campaigns WHERE workspace_id=$1 AND id=$2 FOR UPDATE`,
		workspaceID, campaignID))
	if err != nil {
		return DirectCampaignRevision{}, err
	}
	operation, err := scanDirectProviderOperation(tx.QueryRowContext(ctx,
		`SELECT `+directProviderOperationColumns+`
FROM direct_provider_operations
WHERE workspace_id=$1 AND campaign_id=$2 AND id=$3 FOR UPDATE`,
		workspaceID, campaignID, input.ExpectedOperationID))
	if err != nil {
		return DirectCampaignRevision{}, err
	}
	if campaign.SubmissionOperationID != operation.ID ||
		!operation.ClaimedAt.Equal(input.ExpectedClaimedAt) {
		return DirectCampaignRevision{}, ErrConflict
	}
	if campaign.SubmissionOperationID == operation.ID &&
		operation.RevisionID != "" {
		revision, revisionErr := scanDirectCampaignRevision(
			tx.QueryRowContext(ctx, `SELECT `+directCampaignRevisionColumns+`
FROM direct_campaign_revisions
WHERE workspace_id=$1 AND campaign_id=$2 AND id=$3 FOR SHARE`,
				workspaceID, campaignID, operation.RevisionID),
		)
		if revisionErr != nil {
			return DirectCampaignRevision{}, revisionErr
		}
		if !directVerifiedGraphReplayMatches(
			operation, revision, input, desiredGraph, observedGraph,
		) {
			return DirectCampaignRevision{}, ErrDirectConsentMismatch
		}
		if err := tx.Commit(); err != nil {
			return DirectCampaignRevision{}, err
		}
		return revision, nil
	}
	if campaign.SubmissionOperationID != operation.ID ||
		operation.ExpectedCampaignVersion != input.ExpectedCampaignVersion ||
		campaign.Version != input.ExpectedCampaignVersion ||
		operation.Stage != input.ExpectedStage ||
		operation.CompletedAt != nil ||
		operation.LeaseExpiresAt.Before(input.ObservedAt) ||
		operation.ActorUserID != input.ActorUserID {
		return DirectCampaignRevision{}, ErrConflict
	}
	operationDesired, err := canonicalDirectJSONObject(operation.DesiredGraph)
	if err != nil || !bytes.Equal(operationDesired, desiredGraph) {
		return DirectCampaignRevision{}, ErrDirectConsentMismatch
	}
	desiredCampaign, err := directDesiredCampaignFromJSON(campaign, desiredGraph)
	if err != nil {
		return DirectCampaignRevision{}, err
	}
	if err := validateDirectKeywordMappings(
		input.ProviderKeywordMappings, desiredCampaign.Keywords, true,
	); err != nil {
		return DirectCampaignRevision{}, err
	}
	if err := validateDirectProviderIssues(input.ProviderWarnings); err != nil {
		return DirectCampaignRevision{}, err
	}
	keywordIDs := directProviderKeywordIDs(input.ProviderKeywordMappings)
	mappingsJSON, _ := json.Marshal(input.ProviderKeywordMappings)
	keywordIDsJSON, _ := json.Marshal(keywordIDs)
	warningsJSON, _ := json.Marshal(input.ProviderWarnings)
	campaignModeration := normalizeDirectModerationSnapshot(input.CampaignModeration)
	adGroupModeration := normalizeDirectModerationSnapshot(input.AdGroupModeration)
	adModeration := normalizeDirectModerationSnapshot(input.AdModeration)
	keywordModeration := normalizeDirectKeywordMappingsModeration(
		input.ProviderKeywordMappings,
	)
	moderationStatus, moderationClarification := aggregateDirectModeration(
		campaignModeration, adGroupModeration, adModeration, keywordModeration,
	)
	moderationStatus, moderationClarification, err =
		authoritativeDirectModeration(
			input.AggregateModerationStatus,
			input.AggregateModerationClarification,
			moderationStatus, moderationClarification,
		)
	if err != nil {
		return DirectCampaignRevision{}, err
	}
	campaignModerationJSON, _ := json.Marshal(campaignModeration)
	adGroupModerationJSON, _ := json.Marshal(adGroupModeration)
	adModerationJSON, _ := json.Marshal(adModeration)
	keywordModerationJSON, _ := json.Marshal(keywordModeration)
	campaignVersion := campaign.Version
	if operation.OperationKind == "update" {
		titlesJSON, textsJSON, keywordsJSON, negativeKeywordsJSON, marshalErr :=
			marshalDirectCampaignDesiredLists(desiredCampaign)
		if marshalErr != nil {
			return DirectCampaignRevision{}, marshalErr
		}
		regionsJSON, marshalErr := json.Marshal(desiredCampaign.Regions)
		if marshalErr != nil {
			return DirectCampaignRevision{}, marshalErr
		}
		result, updateErr := tx.ExecContext(ctx, `UPDATE direct_campaigns
SET name=$1,landing_url=$2,regions=$3,titles=$4,texts=$5,keywords=$6,
    negative_keywords=$7,weekly_budget_minor=$8,starts_at=$9,ends_at=$10,
    provider_campaign_id=$11,provider_ad_group_id=$12,provider_ad_id=$13,
    provider_keyword_ids=$14,provider_keyword_mappings=$15,version=version+1,
    updated_at=$16
WHERE workspace_id=$17 AND id=$18 AND version=$19
  AND submission_operation_id=$20`,
			desiredCampaign.Name, desiredCampaign.LandingURL, string(regionsJSON),
			titlesJSON, textsJSON, keywordsJSON, negativeKeywordsJSON,
			desiredCampaign.WeeklyBudgetMinor, dateOnly(desiredCampaign.StartsAt),
			dateOnly(desiredCampaign.EndsAt), input.ProviderCampaignID,
			input.ProviderAdGroupID, input.ProviderAdID, string(keywordIDsJSON),
			string(mappingsJSON), input.ObservedAt, workspaceID, campaignID,
			input.ExpectedCampaignVersion, operation.ID)
		if updateErr != nil {
			return DirectCampaignRevision{}, updateErr
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return DirectCampaignRevision{}, ErrConflict
		}
		campaignVersion++
	} else {
		result, updateErr := tx.ExecContext(ctx, `UPDATE direct_campaigns
SET provider_campaign_id=$1,provider_ad_group_id=$2,provider_ad_id=$3,
    provider_keyword_ids=$4,provider_keyword_mappings=$5,provider_warnings=$6,
    updated_at=$7
WHERE workspace_id=$8 AND id=$9 AND version=$10
  AND submission_operation_id=$11`,
			input.ProviderCampaignID, input.ProviderAdGroupID, input.ProviderAdID,
			string(keywordIDsJSON), string(mappingsJSON), string(warningsJSON),
			input.ObservedAt, workspaceID, campaignID,
			input.ExpectedCampaignVersion, operation.ID)
		if updateErr != nil {
			return DirectCampaignRevision{}, updateErr
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return DirectCampaignRevision{}, ErrConflict
		}
	}
	var revisionNumber int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision_number),0)+1
FROM direct_campaign_revisions
WHERE workspace_id=$1 AND campaign_id=$2`, workspaceID, campaignID).Scan(
		&revisionNumber,
	); err != nil {
		return DirectCampaignRevision{}, err
	}
	revision := DirectCampaignRevision{
		ID: newStoreID("drev_"), WorkspaceID: workspaceID, CampaignID: campaignID,
		ConnectionID: campaign.ConnectionID, RevisionNumber: revisionNumber,
		CampaignVersion: campaignVersion, GraphVersion: input.GraphVersion,
		DesiredGraph: desiredGraph, ObservedGraph: observedGraph,
		GraphHash: input.GraphHash, ProviderCampaignID: input.ProviderCampaignID,
		ProviderAdGroupID: input.ProviderAdGroupID, ProviderAdID: input.ProviderAdID,
		ProviderKeywordMappings: append(
			[]DirectKeywordMapping(nil), input.ProviderKeywordMappings...,
		),
		ProviderWarnings:        append([]DirectProviderIssue(nil), input.ProviderWarnings...),
		ModerationStatus:        moderationStatus,
		ModerationClarification: moderationClarification,
		ActorUserID:             input.ActorUserID, ObservedAt: input.ObservedAt,
		CreatedAt: input.ObservedAt,
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO direct_campaign_revisions(
id,workspace_id,campaign_id,connection_id,revision_number,campaign_version,
graph_version,desired_graph,observed_graph,graph_hash,provider_campaign_id,
provider_ad_group_id,provider_ad_id,provider_keyword_mappings,provider_warnings,
moderation_status,moderation_clarification,actor_user_id,observed_at,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$19)`,
		revision.ID, workspaceID, campaignID, campaign.ConnectionID,
		revision.RevisionNumber, revision.CampaignVersion, revision.GraphVersion,
		string(desiredGraph), string(observedGraph), revision.GraphHash,
		revision.ProviderCampaignID, revision.ProviderAdGroupID,
		revision.ProviderAdID, string(mappingsJSON), string(warningsJSON),
		revision.ModerationStatus, revision.ModerationClarification,
		revision.ActorUserID, revision.ObservedAt)
	if err != nil {
		return DirectCampaignRevision{}, err
	}
	status := directLocalStatusForModeration(moderationStatus, campaign.Status)
	campaignResult, err := tx.ExecContext(ctx, `UPDATE direct_campaigns
SET provider_graph_hash=$1,provider_revision_id=$2,graph_verified_at=$3,
    provider_warnings=$4,moderation_status=$5,moderation_clarification=$6,
    campaign_moderation=$7,ad_group_moderation=$8,ad_moderation=$9,
    keyword_moderation=$10,submission_stage='verified',
    submission_lease_expires_at=$11,submission_failure_code='',
    submission_failure_clarification='',status=$12,updated_at=$3
WHERE workspace_id=$13 AND id=$14 AND submission_operation_id=$15`,
		revision.GraphHash, revision.ID, revision.ObservedAt, string(warningsJSON),
		moderationStatus, moderationClarification, string(campaignModerationJSON),
		string(adGroupModerationJSON), string(adModerationJSON),
		string(keywordModerationJSON),
		revision.ObservedAt.Add(directProviderOperationLease), status,
		workspaceID, campaignID, operation.ID)
	if err != nil {
		return DirectCampaignRevision{}, err
	}
	if affected, _ := campaignResult.RowsAffected(); affected != 1 {
		return DirectCampaignRevision{}, ErrConflict
	}
	operation.Stage = "verified"
	operation.ObservedGraph = observedGraph
	operation.ProviderCampaignID = int64Pointer(input.ProviderCampaignID)
	operation.ProviderAdGroupID = int64Pointer(input.ProviderAdGroupID)
	operation.ProviderAdID = int64Pointer(input.ProviderAdID)
	operation.ProviderKeywordMappings = append(
		[]DirectKeywordMapping(nil), input.ProviderKeywordMappings...,
	)
	operation.ProviderWarnings = append(
		[]DirectProviderIssue(nil), input.ProviderWarnings...,
	)
	operation.GraphHash = revision.GraphHash
	operation.RevisionID = revision.ID
	operation.LeaseExpiresAt = revision.ObservedAt.Add(directProviderOperationLease)
	operation.UpdatedAt = revision.ObservedAt
	operationResult, err := tx.ExecContext(ctx, `UPDATE direct_provider_operations
SET stage='verified',observed_graph=$1,provider_campaign_id=$2,
    provider_ad_group_id=$3,provider_ad_id=$4,provider_keyword_mappings=$5,
    provider_warnings=$6,graph_hash=$7,revision_id=$8,lease_expires_at=$9,
    last_provider_error_code='',last_provider_clarification='',updated_at=$10
WHERE id=$11 AND workspace_id=$12 AND campaign_id=$13 AND stage=$14
  AND claimed_at=$15`,
		string(observedGraph), input.ProviderCampaignID, input.ProviderAdGroupID,
		input.ProviderAdID, string(mappingsJSON), string(warningsJSON),
		revision.GraphHash, revision.ID, operation.LeaseExpiresAt,
		revision.ObservedAt, operation.ID, workspaceID, campaignID,
		input.ExpectedStage, input.ExpectedClaimedAt)
	if err != nil {
		return DirectCampaignRevision{}, err
	}
	if affected, _ := operationResult.RowsAffected(); affected != 1 {
		return DirectCampaignRevision{}, ErrConflict
	}
	if err := insertDirectProviderOperationJournal(
		ctx, tx, operation, revision.ObservedAt,
	); err != nil {
		return DirectCampaignRevision{}, err
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: input.ActorUserID,
		Action:     "direct.campaign.graph_verified",
		EntityType: "direct_campaign", EntityID: campaignID,
		Metadata: mustJSON(map[string]any{
			"operation_id": operation.ID, "revision_id": revision.ID,
			"graph_hash":           revision.GraphHash,
			"provider_campaign_id": revision.ProviderCampaignID,
			"provider_ad_group_id": revision.ProviderAdGroupID,
			"provider_ad_id":       revision.ProviderAdID,
			"keyword_count":        len(revision.ProviderKeywordMappings),
		}), CreatedAt: revision.ObservedAt,
	}); err != nil {
		return DirectCampaignRevision{}, err
	}
	if err := tx.Commit(); err != nil {
		return DirectCampaignRevision{}, err
	}
	return revision, nil
}

func (s *Store) UpdateDirectCampaignGraphModeration(
	ctx context.Context, workspaceID, campaignID string,
	update DirectGraphModerationUpdate,
) (DirectCampaign, error) {
	update.ExpectedGraphHash = strings.TrimSpace(update.ExpectedGraphHash)
	update.ExpectedRevisionID = strings.TrimSpace(update.ExpectedRevisionID)
	update.CheckedAt = update.CheckedAt.UTC()
	if update.CheckedAt.IsZero() {
		update.CheckedAt = time.Now().UTC()
	}
	if !directGraphHashPattern.MatchString(update.ExpectedGraphHash) ||
		update.ExpectedRevisionID == "" {
		return DirectCampaign{}, ErrDirectGraphUnverified
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DirectCampaign{}, err
	}
	defer func() { _ = tx.Rollback() }()
	campaign, err := scanDirectCampaign(tx.QueryRowContext(ctx, `SELECT `+directCampaignColumns+`
FROM direct_campaigns WHERE workspace_id=$1 AND id=$2 FOR UPDATE`,
		workspaceID, campaignID))
	if err != nil {
		return DirectCampaign{}, err
	}
	if campaign.GraphVerifiedAt == nil ||
		campaign.ProviderGraphHash != update.ExpectedGraphHash ||
		campaign.ProviderRevisionID != update.ExpectedRevisionID {
		return DirectCampaign{}, ErrDirectConsentMismatch
	}
	if err := validateDirectKeywordMappings(
		update.Keywords, campaign.Keywords, true,
	); err != nil {
		return DirectCampaign{}, err
	}
	if !sameDirectKeywordProviderIDs(
		update.Keywords, campaign.ProviderKeywordMappings,
	) {
		return DirectCampaign{}, ErrDirectConsentMismatch
	}
	campaignModeration := normalizeDirectModerationSnapshot(update.Campaign)
	adGroupModeration := normalizeDirectModerationSnapshot(update.AdGroup)
	adModeration := normalizeDirectModerationSnapshot(update.Ad)
	keywordModeration := normalizeDirectKeywordMappingsModeration(update.Keywords)
	moderationStatus, moderationClarification := aggregateDirectModeration(
		campaignModeration, adGroupModeration, adModeration, keywordModeration,
	)
	moderationStatus, moderationClarification, err =
		authoritativeDirectModeration(
			update.AggregateModerationStatus,
			update.AggregateModerationClarification,
			moderationStatus, moderationClarification,
		)
	if err != nil {
		return DirectCampaign{}, err
	}
	if clarification := directGraphText(update.StatusClarification); clarification != "" && moderationClarification == "" {
		moderationClarification = clarification
	}
	rawProviderStatus := strings.TrimSpace(update.ProviderStatus)
	providerStatus := normalizeDirectProviderStatus(rawProviderStatus)
	if rawProviderStatus != "" && providerStatus == "" {
		return DirectCampaign{}, fmt.Errorf(
			"%w: invalid provider campaign status", ErrDirectValidation,
		)
	}
	if providerStatus == "" {
		providerStatus = campaign.ProviderStatus
	}
	providerState := strings.ToUpper(strings.TrimSpace(update.ProviderState))
	if utf8.RuneCountInString(providerState) > 64 {
		return DirectCampaign{}, fmt.Errorf(
			"%w: invalid provider campaign state", ErrDirectValidation,
		)
	}
	if providerState == "" {
		providerState = campaign.ProviderState
	}
	status := directLocalStatusForModeration(moderationStatus, campaign.Status)
	if moderationStatus == "ACCEPTED" && providerStatus != "ACCEPTED" {
		status = "moderation"
	}
	campaignModerationJSON, _ := json.Marshal(campaignModeration)
	adGroupModerationJSON, _ := json.Marshal(adGroupModeration)
	adModerationJSON, _ := json.Marshal(adModeration)
	keywordModerationJSON, _ := json.Marshal(keywordModeration)
	result, err := tx.ExecContext(ctx, `UPDATE direct_campaigns
SET moderation_status=$1,moderation_clarification=$2,campaign_moderation=$3,
    ad_group_moderation=$4,ad_moderation=$5,keyword_moderation=$6,
    provider_status=$7,provider_state=$8,status=$9,provider_next_check_at=$10,
    updated_at=$10
WHERE workspace_id=$11 AND id=$12 AND provider_graph_hash=$13
  AND provider_revision_id=$14 AND graph_verified_at IS NOT NULL`,
		moderationStatus, moderationClarification, string(campaignModerationJSON),
		string(adGroupModerationJSON), string(adModerationJSON),
		string(keywordModerationJSON), providerStatus, providerState, status,
		update.CheckedAt, workspaceID, campaignID, update.ExpectedGraphHash,
		update.ExpectedRevisionID)
	if err != nil {
		return DirectCampaign{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return DirectCampaign{}, ErrConflict
	}
	if moderationStatus != "ACCEPTED" {
		if _, err := tx.ExecContext(ctx, `UPDATE direct_auto_launch_consents_v2
SET invalidated_at=$1,invalid_reason='moderation_not_accepted'
WHERE workspace_id=$2 AND campaign_id=$3
  AND revoked_at IS NULL AND invalidated_at IS NULL AND consumed_at IS NULL`,
			update.CheckedAt, workspaceID, campaignID); err != nil {
			return DirectCampaign{}, err
		}
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, Action: "direct.campaign.moderation_observed",
		EntityType: "direct_campaign", EntityID: campaignID,
		Metadata: mustJSON(map[string]any{
			"graph_hash":        update.ExpectedGraphHash,
			"revision_id":       update.ExpectedRevisionID,
			"moderation_status": moderationStatus,
			"provider_status":   providerStatus, "provider_state": providerState,
		}), CreatedAt: update.CheckedAt,
	}); err != nil {
		return DirectCampaign{}, err
	}
	if err := tx.Commit(); err != nil {
		return DirectCampaign{}, err
	}
	return s.getDirectCampaignForWorker(ctx, workspaceID, campaignID)
}

func (s *Store) GetDirectCampaignRevision(
	ctx context.Context, workspaceID, campaignID, revisionID string,
) (DirectCampaignRevision, error) {
	return scanDirectCampaignRevision(s.db.QueryRowContext(ctx,
		`SELECT `+directCampaignRevisionColumns+`
FROM direct_campaign_revisions
WHERE workspace_id=$1 AND campaign_id=$2 AND id=$3`,
		workspaceID, campaignID, revisionID))
}

func validateDirectCampaignGraphDraft(campaign *DirectCampaign) error {
	var err error
	campaign.Titles, err = normalizeDirectAdTextValues(
		campaign.Titles, 1, directMaxTitles,
		directMaxTitleRunes, directMaxTitleWordRunes, "title",
	)
	if err != nil {
		return err
	}
	campaign.Texts, err = normalizeDirectAdTextValues(
		campaign.Texts, 1, directMaxTexts,
		directMaxTextRunes, directMaxTextWordRunes, "text",
	)
	if err != nil {
		return err
	}
	campaign.Keywords, err = normalizeDirectKeywordValues(
		campaign.Keywords, 1, directMaxKeywords, false,
	)
	if err != nil {
		return err
	}
	campaign.NegativeKeywords, err = normalizeDirectKeywordValues(
		campaign.NegativeKeywords, 0, directMaxNegativeKeywords, true,
	)
	return err
}

func validateDirectProviderOperationDates(
	campaign DirectCampaign, now time.Time,
) error {
	providerNow := now.In(directMoscowLocation)
	today := time.Date(
		providerNow.Year(), providerNow.Month(), providerNow.Day(),
		0, 0, 0, 0, time.UTC,
	)
	if campaign.StartsAt.Before(today) || campaign.EndsAt.Before(today) {
		return fmt.Errorf(
			"%w: starts_at and ends_at must not be in the past",
			ErrDirectValidation,
		)
	}
	return nil
}

func normalizeDirectAdTextValues(
	values []string, minItems, maxItems, maxRunes, maxWordRunes int, field string,
) ([]string, error) {
	if len(values) < minItems || len(values) > maxItems {
		return nil, fmt.Errorf("%s must contain %d to %d items", field, minItems, maxItems)
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = directGraphText(value)
		if value == "" || utf8.RuneCountInString(value) > maxRunes {
			return nil, fmt.Errorf("%s must contain 1 to %d characters", field, maxRunes)
		}
		for _, word := range strings.Fields(value) {
			if utf8.RuneCountInString(word) > maxWordRunes {
				return nil, fmt.Errorf("%s word must not exceed %d characters", field, maxWordRunes)
			}
		}
		key := directGraphDuplicateKey(value)
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf(
				"%s must not contain duplicate values", field,
			)
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func normalizeDirectKeywordValues(
	values []string, minItems, maxItems int, negative bool,
) ([]string, error) {
	field := "keywords"
	if negative {
		field = "negative_keywords"
	}
	if len(values) < minItems || len(values) > maxItems {
		return nil, fmt.Errorf("%s must contain %d to %d items", field, minItems, maxItems)
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	totalRunes := 0
	for _, value := range values {
		value = directGraphText(value)
		if value == "" || utf8.RuneCountInString(value) > directMaxKeywordRunes ||
			negative && strings.HasPrefix(value, "-") {
			return nil, fmt.Errorf("%s contains an invalid phrase", field)
		}
		words := strings.Fields(value)
		if len(words) == 0 || len(words) > directMaxKeywordWords {
			return nil, fmt.Errorf("%s phrase must contain 1 to %d words", field, directMaxKeywordWords)
		}
		positiveWords := 0
		for _, word := range words {
			core := strings.Trim(word, `-+!()[]"`)
			if core == "" || utf8.RuneCountInString(core) > directMaxKeywordWordRunes {
				return nil, fmt.Errorf(
					"%s word must contain 1 to %d characters",
					field, directMaxKeywordWordRunes,
				)
			}
			if !strings.HasPrefix(word, "-") {
				positiveWords++
			}
		}
		if positiveWords == 0 {
			return nil, fmt.Errorf("%s phrase must contain a positive word", field)
		}
		totalRunes += utf8.RuneCountInString(value)
		if negative && totalRunes > directMaxKeywordRunes {
			return nil, fmt.Errorf("%s exceeds %d characters in total", field, directMaxKeywordRunes)
		}
		key := directGraphDuplicateKey(value)
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf(
				"%s must not contain duplicate phrases", field,
			)
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func directGraphText(value string) string {
	return strings.Join(strings.Fields(norm.NFC.String(strings.TrimSpace(value))), " ")
}

func directGraphDuplicateKey(value string) string {
	return cases.Fold().String(norm.NFKC.String(value))
}

func marshalDirectCampaignDesiredLists(
	campaign DirectCampaign,
) (titles, texts, keywords, negativeKeywords string, err error) {
	values := []any{
		campaign.Titles, campaign.Texts, campaign.Keywords, campaign.NegativeKeywords,
	}
	encoded := make([]string, len(values))
	for index, value := range values {
		payload, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			return "", "", "", "", marshalErr
		}
		encoded[index] = string(payload)
	}
	return encoded[0], encoded[1], encoded[2], encoded[3], nil
}

func applyDirectCampaignChanges(campaign *DirectCampaign, changes DirectCampaignChanges) {
	if changes.Name != nil {
		campaign.Name = *changes.Name
	}
	if changes.Objective != nil {
		campaign.Objective = *changes.Objective
	}
	if changes.LandingURL != nil {
		campaign.LandingURL = *changes.LandingURL
	}
	if changes.Brief != nil {
		campaign.Brief = *changes.Brief
	}
	if changes.Regions != nil {
		campaign.Regions = append([]string(nil), (*changes.Regions)...)
	}
	if changes.Titles != nil {
		campaign.Titles = append([]string(nil), (*changes.Titles)...)
	}
	if changes.Texts != nil {
		campaign.Texts = append([]string(nil), (*changes.Texts)...)
	}
	if changes.Keywords != nil {
		campaign.Keywords = append([]string(nil), (*changes.Keywords)...)
	}
	if changes.NegativeKeywords != nil {
		campaign.NegativeKeywords = append(
			[]string(nil), (*changes.NegativeKeywords)...,
		)
	}
	if changes.WeeklyBudgetMinor != nil {
		campaign.WeeklyBudgetMinor = *changes.WeeklyBudgetMinor
	}
	if changes.StartsAt != nil {
		campaign.StartsAt = *changes.StartsAt
	}
	if changes.EndsAt != nil {
		campaign.EndsAt = *changes.EndsAt
	}
}

func directRequestedGraphCampaign(
	campaign DirectCampaign, operationKind string, changes DirectCampaignChanges,
) (DirectCampaign, error) {
	if operationKind == "update" {
		if !directProviderEditLocalMetadataUnchanged(campaign, changes) {
			return DirectCampaign{}, fmt.Errorf(
				"%w: provider edit cannot change local-only metadata",
				ErrDirectValidation,
			)
		}
		applyDirectCampaignChanges(&campaign, changes)
	}
	if err := validateDirectCampaignDraft(&campaign); err != nil {
		return DirectCampaign{}, fmt.Errorf(
			"%w: %w", ErrDirectValidation, err,
		)
	}
	return campaign, nil
}

func directProviderEditLocalMetadataUnchanged(
	campaign DirectCampaign, changes DirectCampaignChanges,
) bool {
	return (changes.Objective == nil ||
		strings.TrimSpace(*changes.Objective) == strings.TrimSpace(campaign.Objective)) &&
		(changes.Brief == nil ||
			strings.TrimSpace(*changes.Brief) == strings.TrimSpace(campaign.Brief))
}

func directDesiredGraphFromCampaign(campaign DirectCampaign) DirectCampaignDesiredGraph {
	return DirectCampaignDesiredGraph{
		Name:              campaign.Name,
		LandingURL:        campaign.LandingURL,
		Regions:           append([]string(nil), campaign.Regions...),
		WeeklyBudgetMinor: campaign.WeeklyBudgetMinor,
		CurrencyCode:      campaign.CurrencyCode,
		StartsAt:          dateOnly(campaign.StartsAt),
		EndsAt:            dateOnly(campaign.EndsAt),
		Titles:            append([]string(nil), campaign.Titles...),
		Texts:             append([]string(nil), campaign.Texts...),
		Keywords:          append([]string(nil), campaign.Keywords...),
		NegativeKeywords:  append([]string(nil), campaign.NegativeKeywords...),
	}
}

func marshalDirectDesiredGraph(campaign DirectCampaign) (json.RawMessage, error) {
	desired := directDesiredGraphFromCampaign(campaign)
	payload, err := json.Marshal(desired)
	if err != nil {
		return nil, err
	}
	return canonicalDirectJSONObject(payload)
}

func directDesiredCampaignFromJSON(
	base DirectCampaign, payload json.RawMessage,
) (DirectCampaign, error) {
	canonical, err := canonicalDirectJSONObject(payload)
	if err != nil {
		return DirectCampaign{}, fmt.Errorf(
			"%w: invalid desired provider graph", ErrDirectValidation,
		)
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	var desired DirectCampaignDesiredGraph
	if err := decoder.Decode(&desired); err != nil {
		return DirectCampaign{}, fmt.Errorf(
			"%w: decode desired provider graph: %w", ErrDirectValidation, err,
		)
	}
	base.Name = desired.Name
	base.LandingURL = desired.LandingURL
	base.Regions = append([]string(nil), desired.Regions...)
	base.WeeklyBudgetMinor = desired.WeeklyBudgetMinor
	base.CurrencyCode = desired.CurrencyCode
	base.StartsAt = desired.StartsAt
	base.EndsAt = desired.EndsAt
	base.Titles = append([]string(nil), desired.Titles...)
	base.Texts = append([]string(nil), desired.Texts...)
	base.Keywords = append([]string(nil), desired.Keywords...)
	base.NegativeKeywords = append([]string(nil), desired.NegativeKeywords...)
	if err := validateDirectCampaignDraft(&base); err != nil {
		return DirectCampaign{}, fmt.Errorf("%w: %w", ErrDirectValidation, err)
	}
	encoded, err := marshalDirectDesiredGraph(base)
	if err != nil || !bytes.Equal(encoded, canonical) {
		return DirectCampaign{}, fmt.Errorf(
			"%w: desired provider graph is not canonical", ErrDirectValidation,
		)
	}
	return base, nil
}

func canonicalDirectJSONObject(payload json.RawMessage) (json.RawMessage, error) {
	if len(payload) == 0 {
		return nil, errors.New("JSON object is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		if err == nil {
			err = errors.New("JSON object is required")
		}
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, errors.New("multiple JSON values are not allowed")
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	canonical, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	return canonical, nil
}

func directGraphHash(payload json.RawMessage) string {
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("%x", sum[:])
}

// validateDirectObservedGraphStructure validates the provider graph topology
// without attempting to reproduce the provider-owned content fingerprint.
// Operational fields such as moderation status are intentionally ignored:
// they may change while the immutable content fingerprint remains the same.
func validateDirectObservedGraphStructure(
	observedGraph json.RawMessage, providerCampaignID, providerAdGroupID,
	providerAdID int64, mappings []DirectKeywordMapping,
) error {
	var observed struct {
		Campaign struct {
			ID int64 `json:"id"`
		} `json:"campaign"`
		AdGroups []struct {
			ID         int64 `json:"id"`
			CampaignID int64 `json:"campaign_id"`
		} `json:"ad_groups"`
		Ads []struct {
			ID         int64 `json:"id"`
			CampaignID int64 `json:"campaign_id"`
			AdGroupID  int64 `json:"ad_group_id"`
		} `json:"ads"`
		Keywords []struct {
			ID         int64  `json:"id"`
			CampaignID int64  `json:"campaign_id"`
			AdGroupID  int64  `json:"ad_group_id"`
			Keyword    string `json:"keyword"`
		} `json:"keywords"`
	}
	if err := json.Unmarshal(observedGraph, &observed); err != nil ||
		observed.Campaign.ID <= 0 ||
		len(observed.AdGroups) != 1 || len(observed.Ads) != 1 ||
		len(observed.Keywords) != len(mappings) {
		return ErrDirectGraphUnverified
	}
	if observed.Campaign.ID != providerCampaignID ||
		observed.AdGroups[0].ID != providerAdGroupID ||
		observed.AdGroups[0].CampaignID != providerCampaignID ||
		observed.Ads[0].ID != providerAdID ||
		observed.Ads[0].CampaignID != providerCampaignID ||
		observed.Ads[0].AdGroupID != providerAdGroupID {
		return ErrDirectConsentMismatch
	}
	expectedKeywords := make(map[int64]string, len(mappings))
	for _, mapping := range mappings {
		keyword := directGraphText(mapping.Keyword)
		if mapping.ProviderKeywordID <= 0 || keyword == "" ||
			keyword != mapping.Keyword {
			return ErrDirectGraphUnverified
		}
		if _, duplicate := expectedKeywords[mapping.ProviderKeywordID]; duplicate {
			return ErrDirectGraphUnverified
		}
		expectedKeywords[mapping.ProviderKeywordID] = keyword
	}
	seenKeywords := make(map[int64]struct{}, len(observed.Keywords))
	for _, keyword := range observed.Keywords {
		if keyword.ID <= 0 || keyword.CampaignID != providerCampaignID ||
			keyword.AdGroupID != providerAdGroupID ||
			directGraphText(keyword.Keyword) != keyword.Keyword {
			return ErrDirectGraphUnverified
		}
		expected, exists := expectedKeywords[keyword.ID]
		if !exists || expected != keyword.Keyword {
			return ErrDirectConsentMismatch
		}
		if _, duplicate := seenKeywords[keyword.ID]; duplicate {
			return ErrDirectGraphUnverified
		}
		seenKeywords[keyword.ID] = struct{}{}
	}
	return nil
}

func validateDirectKeywordMappings(
	mappings []DirectKeywordMapping, keywords []string, exact bool,
) error {
	if exact && len(mappings) != len(keywords) {
		return fmt.Errorf(
			"%w: provider keyword mapping count does not match desired keywords",
			ErrDirectValidation,
		)
	}
	if len(mappings) > directMaxKeywords {
		return fmt.Errorf(
			"%w: provider keyword mapping count exceeds %d",
			ErrDirectValidation, directMaxKeywords,
		)
	}
	seenIDs := make(map[int64]struct{}, len(mappings))
	for index, mapping := range mappings {
		keyword := directGraphText(mapping.Keyword)
		if keyword == "" || keyword != mapping.Keyword ||
			mapping.ProviderKeywordID <= 0 {
			return fmt.Errorf(
				"%w: invalid provider keyword mapping at index %d",
				ErrDirectValidation, index,
			)
		}
		if exact && keyword != keywords[index] {
			return fmt.Errorf(
				"%w: provider keyword mapping does not match desired graph",
				ErrDirectConsentMismatch,
			)
		}
		if _, duplicate := seenIDs[mapping.ProviderKeywordID]; duplicate {
			return fmt.Errorf(
				"%w: duplicate provider keyword id", ErrDirectValidation,
			)
		}
		seenIDs[mapping.ProviderKeywordID] = struct{}{}
	}
	return nil
}

func validateDirectProviderIssues(issues []DirectProviderIssue) error {
	if len(issues) > 1000 {
		return fmt.Errorf(
			"%w: too many provider warnings", ErrDirectValidation,
		)
	}
	for _, issue := range issues {
		if issue.Code <= 0 ||
			utf8.RuneCountInString(strings.TrimSpace(issue.Message)) > 2000 ||
			utf8.RuneCountInString(strings.TrimSpace(issue.Details)) > 4000 {
			return fmt.Errorf(
				"%w: invalid provider warning", ErrDirectValidation,
			)
		}
	}
	return nil
}

func normalizeDirectModerationSnapshot(
	snapshot DirectModerationSnapshot,
) DirectModerationSnapshot {
	switch strings.ToUpper(strings.TrimSpace(snapshot.Status)) {
	case "DRAFT", "MODERATION", "ACCEPTED", "REJECTED":
		snapshot.Status = strings.ToUpper(strings.TrimSpace(snapshot.Status))
	default:
		snapshot.Status = "UNKNOWN"
	}
	snapshot.State = truncateDirectGraphText(
		strings.ToUpper(strings.TrimSpace(snapshot.State)), 128,
	)
	snapshot.ServingStatus = truncateDirectGraphText(
		strings.ToUpper(strings.TrimSpace(snapshot.ServingStatus)), 128,
	)
	snapshot.StatusClarification = truncateDirectGraphText(
		snapshot.StatusClarification, 2000,
	)
	return snapshot
}

func normalizeDirectKeywordMappingsModeration(
	mappings []DirectKeywordMapping,
) []DirectKeywordMapping {
	result := make([]DirectKeywordMapping, len(mappings))
	for index, mapping := range mappings {
		result[index] = DirectKeywordMapping{
			Keyword: mapping.Keyword, ProviderKeywordID: mapping.ProviderKeywordID,
			Moderation: normalizeDirectModerationSnapshot(mapping.Moderation),
		}
	}
	return result
}

func aggregateDirectModeration(
	campaign, adGroup, ad DirectModerationSnapshot,
	keywords []DirectKeywordMapping,
) (string, string) {
	snapshots := []DirectModerationSnapshot{campaign, adGroup, ad}
	for _, keyword := range keywords {
		snapshots = append(snapshots, keyword.Moderation)
	}
	allAccepted := len(keywords) > 0
	hasModeration := false
	hasDraft := false
	hasRejected := false
	clarifications := make([]string, 0, len(snapshots))
	seenClarifications := make(map[string]struct{}, len(snapshots))
	for _, snapshot := range snapshots {
		snapshot = normalizeDirectModerationSnapshot(snapshot)
		switch snapshot.Status {
		case "ACCEPTED":
		case "REJECTED":
			hasRejected = true
			allAccepted = false
		case "MODERATION":
			hasModeration = true
			allAccepted = false
		case "DRAFT":
			hasDraft = true
			allAccepted = false
		default:
			allAccepted = false
		}
		if snapshot.StatusClarification != "" {
			if _, exists := seenClarifications[snapshot.StatusClarification]; !exists {
				seenClarifications[snapshot.StatusClarification] = struct{}{}
				clarifications = append(
					clarifications, snapshot.StatusClarification,
				)
			}
		}
	}
	status := "UNKNOWN"
	switch {
	case hasRejected:
		status = "REJECTED"
	case allAccepted:
		status = "ACCEPTED"
	case hasModeration:
		status = "MODERATION"
	case hasDraft:
		status = "DRAFT"
	}
	return status, truncateRunes(strings.Join(clarifications, "; "), 2000)
}

// authoritativeDirectModeration preserves the aggregate computed from the
// complete provider graph. The persisted child snapshots intentionally stay
// compact, so responsive-ad title/text moderation can make the authoritative
// result stricter than the aggregate reconstructed from campaign/group/ad and
// keyword rows alone.
func authoritativeDirectModeration(
	authoritativeStatus, authoritativeClarification,
	fallbackStatus, fallbackClarification string,
) (string, string, error) {
	status := strings.ToUpper(strings.TrimSpace(authoritativeStatus))
	switch status {
	case "ACCEPTED", "REJECTED", "MODERATION", "UNKNOWN":
	default:
		return "", "", fmt.Errorf(
			"%w: invalid aggregate moderation status", ErrDirectValidation,
		)
	}
	// The complete graph may only make the compact aggregate more
	// conservative. It may never hide a rejection or claim acceptance while
	// one of the persisted child objects is still unresolved.
	switch {
	case fallbackStatus == "REJECTED" && status != "REJECTED":
		return "", "", fmt.Errorf(
			"%w: aggregate moderation hides rejection", ErrDirectValidation,
		)
	case status == "ACCEPTED" && fallbackStatus != "ACCEPTED":
		return "", "", fmt.Errorf(
			"%w: aggregate moderation is prematurely accepted", ErrDirectValidation,
		)
	}
	clarification := truncateDirectGraphText(
		authoritativeClarification, 2000,
	)
	if clarification == "" {
		clarification = truncateDirectGraphText(fallbackClarification, 2000)
	}
	return status, clarification, nil
}

func directLocalStatusForModeration(moderationStatus, currentStatus string) string {
	switch moderationStatus {
	case "ACCEPTED":
		switch currentStatus {
		case "active", "suspended", "completed":
			return currentStatus
		default:
			return "accepted"
		}
	case "REJECTED":
		return "rejected"
	case "DRAFT":
		return "provider_draft"
	default:
		return "moderation"
	}
}

func sameDirectKeywordProviderIDs(
	left, right []DirectKeywordMapping,
) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if directGraphText(left[index].Keyword) !=
			directGraphText(right[index].Keyword) ||
			left[index].ProviderKeywordID != right[index].ProviderKeywordID {
			return false
		}
	}
	return true
}

func directCampaignGraphEvidenceReady(campaign DirectCampaign) error {
	if campaign.GraphVerifiedAt == nil ||
		!directGraphHashPattern.MatchString(campaign.ProviderGraphHash) ||
		!directRevisionIDPattern.MatchString(campaign.ProviderRevisionID) ||
		campaign.ProviderCampaignID == nil || *campaign.ProviderCampaignID <= 0 ||
		campaign.ProviderAdGroupID == nil || *campaign.ProviderAdGroupID <= 0 ||
		campaign.ProviderAdID == nil || *campaign.ProviderAdID <= 0 {
		return ErrDirectGraphUnverified
	}
	if err := validateDirectKeywordMappings(
		campaign.ProviderKeywordMappings, campaign.Keywords, true,
	); err != nil {
		return ErrDirectGraphUnverified
	}
	keywordIDs := directProviderKeywordIDs(campaign.ProviderKeywordMappings)
	if len(keywordIDs) != len(campaign.ProviderKeywordIDs) {
		return ErrDirectGraphUnverified
	}
	for index := range keywordIDs {
		if keywordIDs[index] != campaign.ProviderKeywordIDs[index] {
			return ErrDirectGraphUnverified
		}
	}
	return nil
}

func directCampaignGraphLaunchReady(campaign DirectCampaign) error {
	if err := directCampaignGraphEvidenceReady(campaign); err != nil {
		return err
	}
	if campaign.Status != "accepted" || campaign.ProviderStatus != "ACCEPTED" ||
		campaign.ModerationStatus != "ACCEPTED" ||
		normalizeDirectModerationSnapshot(campaign.CampaignModeration).Status != "ACCEPTED" ||
		normalizeDirectModerationSnapshot(campaign.AdGroupModeration).Status != "ACCEPTED" ||
		normalizeDirectModerationSnapshot(campaign.AdModeration).Status != "ACCEPTED" ||
		len(campaign.KeywordModeration) != len(campaign.Keywords) {
		return ErrDirectModerationNotReady
	}
	for index, keyword := range campaign.KeywordModeration {
		if keyword.ProviderKeywordID != campaign.ProviderKeywordIDs[index] ||
			directGraphText(keyword.Keyword) != campaign.Keywords[index] ||
			normalizeDirectModerationSnapshot(keyword.Moderation).Status != "ACCEPTED" {
			return ErrDirectModerationNotReady
		}
	}
	return nil
}

func truncateDirectGraphText(value string, maxRunes int) string {
	return truncateRunes(directGraphText(value), maxRunes)
}

func truncateRunes(value string, maxRunes int) string {
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes])
}

func directProviderKeywordIDs(mappings []DirectKeywordMapping) []int64 {
	result := make([]int64, len(mappings))
	for index, mapping := range mappings {
		result[index] = mapping.ProviderKeywordID
	}
	return result
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func directTimePointer(value time.Time) *time.Time {
	value = value.UTC()
	return &value
}

func validDirectProviderStageTransition(kind, from, to string) bool {
	if from == "" || to == "" || from == to ||
		from == "completed" || from == "failed" {
		return false
	}
	if to == "failed" || to == "reconciling" {
		return true
	}
	if from == "reconciling" {
		switch kind {
		case "submission":
			return to == "campaign_created" || to == "ad_group_created" ||
				to == "ad_created" || to == "keywords_created" ||
				to == "graph_observed" || to == "moderation_requested" ||
				to == "completed"
		case "update":
			return to == "campaign_updated" || to == "ad_group_updated" ||
				to == "ad_updated" || to == "keywords_updated" ||
				to == "graph_observed" || to == "moderation_requested" ||
				to == "completed"
		}
		return false
	}
	var transitions map[string]string
	switch kind {
	case "submission":
		transitions = map[string]string{
			"claimed":              "campaign_created",
			"campaign_created":     "ad_group_created",
			"ad_group_created":     "ad_created",
			"ad_created":           "keywords_created",
			"keywords_created":     "graph_observed",
			"verified":             "moderation_requested",
			"moderation_requested": "completed",
		}
	case "update":
		transitions = map[string]string{
			"claimed":              "campaign_updated",
			"campaign_updated":     "ad_group_updated",
			"ad_group_updated":     "ad_updated",
			"ad_updated":           "keywords_updated",
			"keywords_updated":     "graph_observed",
			"verified":             "moderation_requested",
			"moderation_requested": "completed",
		}
	default:
		return false
	}
	return transitions[from] == to
}

func mergeDirectProviderStageUpdate(
	operation *DirectProviderOperation, update DirectProviderStageUpdate,
) error {
	if update.Stage == "verified" {
		return fmt.Errorf(
			"%w: verified stage requires an immutable graph revision",
			ErrDirectValidation,
		)
	}
	for field, value := range map[string]*int64{
		"provider_campaign_id": update.ProviderCampaignID,
		"provider_ad_group_id": update.ProviderAdGroupID,
		"provider_ad_id":       update.ProviderAdID,
	} {
		if value != nil && *value <= 0 {
			return fmt.Errorf(
				"%w: %s must be positive", ErrDirectValidation, field,
			)
		}
	}
	if update.ProviderCampaignID != nil {
		operation.ProviderCampaignID = cloneInt64Pointer(update.ProviderCampaignID)
	}
	if update.ProviderAdGroupID != nil {
		operation.ProviderAdGroupID = cloneInt64Pointer(update.ProviderAdGroupID)
	}
	if update.ProviderAdID != nil {
		operation.ProviderAdID = cloneInt64Pointer(update.ProviderAdID)
	}
	if update.ProviderKeywordMappings != nil {
		if err := validateDirectKeywordMappings(
			*update.ProviderKeywordMappings, nil, false,
		); err != nil {
			return err
		}
		operation.ProviderKeywordMappings = append(
			[]DirectKeywordMapping{}, (*update.ProviderKeywordMappings)...,
		)
	}
	if update.ProviderWarnings != nil {
		if err := validateDirectProviderIssues(*update.ProviderWarnings); err != nil {
			return err
		}
		operation.ProviderWarnings = append(
			[]DirectProviderIssue{}, (*update.ProviderWarnings)...,
		)
	}
	if len(update.ObservedGraph) > 0 {
		observed, err := canonicalDirectJSONObject(update.ObservedGraph)
		if err != nil {
			return fmt.Errorf(
				"%w: invalid observed graph", ErrDirectValidation,
			)
		}
		operation.ObservedGraph = observed
	}
	if update.GraphHash != "" {
		update.GraphHash = strings.TrimSpace(update.GraphHash)
		if !directGraphHashPattern.MatchString(update.GraphHash) {
			return fmt.Errorf("%w: invalid graph hash", ErrDirectValidation)
		}
		operation.GraphHash = update.GraphHash
	}
	if update.RevisionID != "" {
		update.RevisionID = strings.TrimSpace(update.RevisionID)
		if !directRevisionIDPattern.MatchString(update.RevisionID) {
			return fmt.Errorf("%w: invalid graph revision id", ErrDirectValidation)
		}
		operation.RevisionID = update.RevisionID
	}
	operation.LastProviderErrorCode = strings.TrimSpace(
		update.LastProviderErrorCode,
	)
	operation.LastProviderClarification = truncateDirectGraphText(
		update.LastProviderClarification, 2000,
	)
	if len(operation.LastProviderErrorCode) > 128 {
		return fmt.Errorf("%w: provider error code is too long", ErrDirectValidation)
	}
	if update.Complete && update.Stage != "completed" && update.Stage != "failed" {
		return fmt.Errorf(
			"%w: only a terminal provider stage may complete an operation",
			ErrDirectValidation,
		)
	}
	switch update.Stage {
	case "campaign_created", "campaign_updated":
		if operation.ProviderCampaignID == nil {
			return fmt.Errorf(
				"%w: provider campaign id is required", ErrDirectValidation,
			)
		}
	case "ad_group_created", "ad_group_updated":
		if operation.ProviderCampaignID == nil ||
			operation.ProviderAdGroupID == nil {
			return fmt.Errorf(
				"%w: provider campaign and ad group ids are required",
				ErrDirectValidation,
			)
		}
	case "ad_created", "ad_updated":
		if operation.ProviderCampaignID == nil ||
			operation.ProviderAdGroupID == nil || operation.ProviderAdID == nil {
			return fmt.Errorf(
				"%w: complete provider creative ids are required",
				ErrDirectValidation,
			)
		}
	case "keywords_created", "keywords_updated":
		if operation.ProviderCampaignID == nil ||
			operation.ProviderAdGroupID == nil || operation.ProviderAdID == nil ||
			len(operation.ProviderKeywordMappings) == 0 {
			return fmt.Errorf(
				"%w: provider keyword mappings are required",
				ErrDirectValidation,
			)
		}
	case "graph_observed":
		if len(operation.ObservedGraph) == 0 ||
			!directGraphHashPattern.MatchString(operation.GraphHash) ||
			operation.ProviderCampaignID == nil ||
			operation.ProviderAdGroupID == nil ||
			operation.ProviderAdID == nil {
			return fmt.Errorf(
				"%w: exact observed graph and hash are required",
				ErrDirectValidation,
			)
		}
		if err := validateDirectObservedGraphStructure(
			operation.ObservedGraph, *operation.ProviderCampaignID,
			*operation.ProviderAdGroupID, *operation.ProviderAdID,
			operation.ProviderKeywordMappings,
		); err != nil {
			return err
		}
	case "failed":
		if operation.LastProviderErrorCode == "" {
			return fmt.Errorf(
				"%w: failed provider stage requires an error code",
				ErrDirectValidation,
			)
		}
	}
	return nil
}

func sameDirectProviderOperationResult(
	left, right DirectProviderOperation,
) bool {
	type result struct {
		ProviderCampaignID        *int64                 `json:"provider_campaign_id"`
		ProviderAdGroupID         *int64                 `json:"provider_ad_group_id"`
		ProviderAdID              *int64                 `json:"provider_ad_id"`
		ProviderKeywordMappings   []DirectKeywordMapping `json:"provider_keyword_mappings"`
		ProviderWarnings          []DirectProviderIssue  `json:"provider_warnings"`
		ObservedGraph             json.RawMessage        `json:"observed_graph"`
		GraphHash                 string                 `json:"graph_hash"`
		RevisionID                string                 `json:"revision_id"`
		LastProviderErrorCode     string                 `json:"last_provider_error_code"`
		LastProviderClarification string                 `json:"last_provider_clarification"`
	}
	leftJSON, leftErr := json.Marshal(result{
		left.ProviderCampaignID, left.ProviderAdGroupID, left.ProviderAdID,
		left.ProviderKeywordMappings, left.ProviderWarnings, left.ObservedGraph,
		left.GraphHash, left.RevisionID, left.LastProviderErrorCode,
		left.LastProviderClarification,
	})
	rightJSON, rightErr := json.Marshal(result{
		right.ProviderCampaignID, right.ProviderAdGroupID, right.ProviderAdID,
		right.ProviderKeywordMappings, right.ProviderWarnings, right.ObservedGraph,
		right.GraphHash, right.RevisionID, right.LastProviderErrorCode,
		right.LastProviderClarification,
	})
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func directVerifiedGraphReplayMatches(
	operation DirectProviderOperation, revision DirectCampaignRevision,
	input DirectVerifiedGraphInput, desiredGraph, observedGraph json.RawMessage,
) bool {
	expectedCampaignVersion := input.ExpectedCampaignVersion
	if operation.OperationKind == "update" {
		expectedCampaignVersion++
	}
	mappingsJSON, mappingsErr := json.Marshal(input.ProviderKeywordMappings)
	revisionMappingsJSON, revisionMappingsErr := json.Marshal(
		revision.ProviderKeywordMappings,
	)
	warningsJSON, warningsErr := json.Marshal(input.ProviderWarnings)
	if input.ProviderWarnings == nil {
		warningsJSON, warningsErr = json.Marshal([]DirectProviderIssue{})
	}
	revisionWarningsJSON, revisionWarningsErr := json.Marshal(
		revision.ProviderWarnings,
	)
	campaignModeration := normalizeDirectModerationSnapshot(
		input.CampaignModeration,
	)
	adGroupModeration := normalizeDirectModerationSnapshot(
		input.AdGroupModeration,
	)
	adModeration := normalizeDirectModerationSnapshot(input.AdModeration)
	keywordModeration := normalizeDirectKeywordMappingsModeration(
		input.ProviderKeywordMappings,
	)
	moderationStatus, moderationClarification := aggregateDirectModeration(
		campaignModeration, adGroupModeration, adModeration, keywordModeration,
	)
	moderationStatus, moderationClarification, moderationErr :=
		authoritativeDirectModeration(
			input.AggregateModerationStatus,
			input.AggregateModerationClarification,
			moderationStatus, moderationClarification,
		)
	return mappingsErr == nil && revisionMappingsErr == nil &&
		warningsErr == nil && revisionWarningsErr == nil &&
		moderationErr == nil &&
		operation.ID == input.ExpectedOperationID &&
		operation.ExpectedCampaignVersion == input.ExpectedCampaignVersion &&
		operation.ActorUserID == input.ActorUserID &&
		operation.GraphHash == input.GraphHash &&
		operation.RevisionID == revision.ID &&
		operation.ProviderCampaignID != nil &&
		*operation.ProviderCampaignID == input.ProviderCampaignID &&
		operation.ProviderAdGroupID != nil &&
		*operation.ProviderAdGroupID == input.ProviderAdGroupID &&
		operation.ProviderAdID != nil &&
		*operation.ProviderAdID == input.ProviderAdID &&
		bytes.Equal(operation.DesiredGraph, desiredGraph) &&
		bytes.Equal(operation.ObservedGraph, observedGraph) &&
		revision.CampaignVersion == expectedCampaignVersion &&
		revision.GraphVersion == input.GraphVersion &&
		revision.GraphHash == input.GraphHash &&
		revision.ProviderCampaignID == input.ProviderCampaignID &&
		revision.ProviderAdGroupID == input.ProviderAdGroupID &&
		revision.ProviderAdID == input.ProviderAdID &&
		bytes.Equal(revision.DesiredGraph, desiredGraph) &&
		bytes.Equal(revision.ObservedGraph, observedGraph) &&
		bytes.Equal(mappingsJSON, revisionMappingsJSON) &&
		bytes.Equal(warningsJSON, revisionWarningsJSON) &&
		revision.ModerationStatus == moderationStatus &&
		revision.ModerationClarification == moderationClarification &&
		revision.ActorUserID == input.ActorUserID
}

func insertDirectProviderOperationJournal(
	ctx context.Context, tx *sql.Tx, operation DirectProviderOperation,
	recordedAt time.Time,
) error {
	snapshot, err := json.Marshal(operation)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO direct_provider_operation_journal(
workspace_id,campaign_id,operation_id,stage,snapshot,recorded_at)
VALUES($1,$2,$3,$4,$5,$6)`,
		operation.WorkspaceID, operation.CampaignID, operation.ID,
		operation.Stage, string(snapshot), recordedAt.UTC())
	return err
}

func scanDirectProviderOperation(row scanner) (DirectProviderOperation, error) {
	var operation DirectProviderOperation
	var providerCampaignID, providerAdGroupID, providerAdID sql.NullInt64
	var desiredGraphJSON, observedGraphJSON []byte
	var mappingsJSON, warningsJSON []byte
	var completedAt sql.NullTime
	err := row.Scan(
		&operation.ID, &operation.WorkspaceID, &operation.CampaignID,
		&operation.ConnectionID, &operation.ActorUserID, &operation.OperationKind,
		&operation.OperationMarker, &operation.ExpectedCampaignVersion,
		&operation.ExpectedGraphHash, &operation.ExpectedRevisionID,
		&operation.Stage, &desiredGraphJSON, &observedGraphJSON,
		&providerCampaignID, &providerAdGroupID, &providerAdID, &mappingsJSON,
		&warningsJSON, &operation.GraphHash, &operation.RevisionID,
		&operation.LastProviderErrorCode,
		&operation.LastProviderClarification, &operation.ClaimedAt,
		&operation.LeaseExpiresAt, &completedAt, &operation.CreatedAt,
		&operation.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return DirectProviderOperation{}, ErrNotFound
	}
	if err != nil {
		return DirectProviderOperation{}, err
	}
	operation.DesiredGraph, err = canonicalDirectJSONObject(desiredGraphJSON)
	if err != nil {
		return DirectProviderOperation{}, fmt.Errorf(
			"decode Direct provider operation desired graph: %w", err,
		)
	}
	if len(observedGraphJSON) > 0 {
		operation.ObservedGraph, err = canonicalDirectJSONObject(observedGraphJSON)
		if err != nil {
			return DirectProviderOperation{}, fmt.Errorf(
				"decode Direct provider operation observed graph: %w", err,
			)
		}
	}
	if err := json.Unmarshal(mappingsJSON, &operation.ProviderKeywordMappings); err != nil {
		return DirectProviderOperation{}, fmt.Errorf(
			"decode Direct provider keyword mappings: %w", err,
		)
	}
	if err := json.Unmarshal(warningsJSON, &operation.ProviderWarnings); err != nil {
		return DirectProviderOperation{}, fmt.Errorf(
			"decode Direct provider warnings: %w", err,
		)
	}
	if providerCampaignID.Valid {
		operation.ProviderCampaignID = int64Pointer(providerCampaignID.Int64)
	}
	if providerAdGroupID.Valid {
		operation.ProviderAdGroupID = int64Pointer(providerAdGroupID.Int64)
	}
	if providerAdID.Valid {
		operation.ProviderAdID = int64Pointer(providerAdID.Int64)
	}
	operation.CompletedAt = parseNullableTime(completedAt)
	operation.ClaimedAt = operation.ClaimedAt.UTC()
	operation.LeaseExpiresAt = operation.LeaseExpiresAt.UTC()
	operation.CreatedAt = operation.CreatedAt.UTC()
	operation.UpdatedAt = operation.UpdatedAt.UTC()
	return operation, nil
}

func scanDirectCampaignRevision(row scanner) (DirectCampaignRevision, error) {
	var revision DirectCampaignRevision
	var desiredGraphJSON, observedGraphJSON, mappingsJSON, warningsJSON []byte
	err := row.Scan(
		&revision.ID, &revision.WorkspaceID, &revision.CampaignID,
		&revision.ConnectionID, &revision.RevisionNumber,
		&revision.CampaignVersion, &revision.GraphVersion, &desiredGraphJSON,
		&observedGraphJSON, &revision.GraphHash, &revision.ProviderCampaignID,
		&revision.ProviderAdGroupID, &revision.ProviderAdID, &mappingsJSON,
		&warningsJSON, &revision.ModerationStatus,
		&revision.ModerationClarification, &revision.ActorUserID,
		&revision.ObservedAt, &revision.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return DirectCampaignRevision{}, ErrNotFound
	}
	if err != nil {
		return DirectCampaignRevision{}, err
	}
	revision.DesiredGraph, err = canonicalDirectJSONObject(desiredGraphJSON)
	if err != nil {
		return DirectCampaignRevision{}, fmt.Errorf(
			"decode Direct campaign revision desired graph: %w", err,
		)
	}
	revision.ObservedGraph, err = canonicalDirectJSONObject(observedGraphJSON)
	if err != nil {
		return DirectCampaignRevision{}, fmt.Errorf(
			"decode Direct campaign revision observed graph: %w", err,
		)
	}
	if err := json.Unmarshal(mappingsJSON, &revision.ProviderKeywordMappings); err != nil {
		return DirectCampaignRevision{}, fmt.Errorf(
			"decode Direct campaign revision keyword mappings: %w", err,
		)
	}
	if err := json.Unmarshal(warningsJSON, &revision.ProviderWarnings); err != nil {
		return DirectCampaignRevision{}, fmt.Errorf(
			"decode Direct campaign revision warnings: %w", err,
		)
	}
	revision.ObservedAt = revision.ObservedAt.UTC()
	revision.CreatedAt = revision.CreatedAt.UTC()
	return revision, nil
}

func (s *Store) getDirectCampaignForWorker(
	ctx context.Context, workspaceID, campaignID string,
) (DirectCampaign, error) {
	return scanDirectCampaign(s.db.QueryRowContext(
		ctx, `SELECT `+directCampaignColumns+`
FROM direct_campaigns WHERE workspace_id=$1 AND id=$2`,
		workspaceID, campaignID,
	))
}
