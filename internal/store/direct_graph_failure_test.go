package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSanitizeDirectGraphFailureCodeDoesNotPersistProviderText(t *testing.T) {
	t.Parallel()
	for input, expected := range map[string]string{
		"6000":                     "provider_validation_6000",
		"provider_validation_5007": "provider_validation_5007",
		"invalid title user@host":  "provider_validation_rejected",
		"":                         "provider_validation_rejected",
	} {
		if actual := sanitizeDirectGraphFailureCode(input); actual != expected {
			t.Fatalf("sanitize %q = %q, want %q", input, actual, expected)
		}
	}
}

func TestDirectGraphTerminalSubmissionFailureRequiresConfirmedAbsence(
	t *testing.T,
) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2045, time.May, 6, 12, 0, 0, 0, time.UTC)
	campaign := createDirectTestCampaign(
		t, ctx, storage, owner, workspace.ID, now,
	)
	material, err := storage.ClaimDirectCampaignGraphSubmission(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
		"terminal_absent_"+campaign.ID, now.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.FailDirectCampaignGraphOperation(
		ctx, workspace.ID, campaign.ID, DirectGraphTerminalFailureInput{
			ExpectedOperationID: material.Operation.ID,
			ExpectedClaimedAt:   material.Operation.ClaimedAt,
			ProviderOutcome:     DirectGraphProviderOutcomeBaselineUnchanged,
			FailureCode:         "6000",
			ConfirmedAt:         now.Add(2 * time.Second),
		},
	); !errors.Is(err, ErrDirectProviderOperationBusy) {
		t.Fatalf("unconfirmed submission failure = %v, want busy", err)
	}
	failed, err := storage.FailDirectCampaignGraphOperation(
		ctx, workspace.ID, campaign.ID, DirectGraphTerminalFailureInput{
			ExpectedOperationID: material.Operation.ID,
			ExpectedClaimedAt:   material.Operation.ClaimedAt,
			ProviderOutcome:     DirectGraphProviderOutcomeAbsent,
			FailureCode:         "provider_validation_6000",
			ConfirmedAt:         now.Add(3 * time.Second),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != "draft" || failed.SubmissionStage != "failed" ||
		failed.SubmissionFailureCode != "provider_validation_6000" ||
		failed.SubmissionFailureClarification !=
			"provider_rejected_before_creation" ||
		failed.ProviderCampaignID != nil ||
		failed.ProviderGraphHash != "" ||
		failed.ProviderRevisionID != "" {
		t.Fatalf("terminal submission failure = %#v", failed)
	}
	var operationStage, failureCode string
	var completed bool
	if err := storage.db.QueryRowContext(ctx, `SELECT
stage,last_provider_error_code,completed_at IS NOT NULL
FROM direct_provider_operations
WHERE workspace_id=$1 AND campaign_id=$2 AND id=$3`,
		workspace.ID, campaign.ID, material.Operation.ID,
	).Scan(&operationStage, &failureCode, &completed); err != nil {
		t.Fatal(err)
	}
	if operationStage != "failed" || failureCode != "provider_validation_6000" ||
		!completed {
		t.Fatalf(
			"failed operation stage=%q code=%q completed=%v",
			operationStage, failureCode, completed,
		)
	}
	replayed, err := storage.ClaimDirectCampaignGraphSubmission(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
		material.Operation.OperationMarker, now.Add(4*time.Second),
	)
	if err != nil {
		t.Fatalf("replay terminal submission failure: %v", err)
	}
	if replayed.Operation.ID != material.Operation.ID ||
		replayed.Operation.Stage != "failed" ||
		replayed.Operation.CompletedAt == nil {
		t.Fatalf("terminal submission replay = %#v", replayed.Operation)
	}
	repairedTitles := []string{"Исправленный заголовок после отказа"}
	repaired, err := storage.UpdateDirectCampaignDraft(
		ctx, owner, workspace.ID, campaign.ID, DirectCampaignChanges{
			Titles: &repairedTitles, ExpectedVersion: campaign.Version,
		},
	)
	if err != nil {
		t.Fatalf("repair rejected draft: %v", err)
	}
	next, err := storage.ClaimDirectCampaignGraphSubmission(
		ctx, owner, workspace.ID, campaign.ID, repaired.Version,
		"terminal_repaired_"+campaign.ID, now.Add(5*time.Second),
	)
	if err != nil {
		t.Fatalf("submit repaired draft: %v", err)
	}
	if next.Operation.ID == material.Operation.ID ||
		next.Operation.Stage != "claimed" {
		t.Fatalf("repaired draft operation = %#v", next.Operation)
	}
}

func TestDirectGraphPartialSubmissionFailureRemainsRecoverable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2045, time.May, 7, 12, 0, 0, 0, time.UTC)
	campaign := createDirectTestCampaign(
		t, ctx, storage, owner, workspace.ID, now,
	)
	material, err := storage.ClaimDirectCampaignGraphSubmission(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
		"partial_graph_"+campaign.ID, now.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	providerCampaignID := int64(7001)
	material, err = storage.AdvanceDirectCampaignGraphSubmission(
		ctx, workspace.ID, campaign.ID, material.Operation.ID, "claimed",
		DirectProviderStageUpdate{
			ExpectedClaimedAt:  material.Operation.ClaimedAt,
			Stage:              "campaign_created",
			ProviderCampaignID: &providerCampaignID,
		}, now.Add(2*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.FailDirectCampaignGraphOperation(
		ctx, workspace.ID, campaign.ID, DirectGraphTerminalFailureInput{
			ExpectedOperationID: material.Operation.ID,
			ExpectedClaimedAt:   material.Operation.ClaimedAt,
			ProviderOutcome:     DirectGraphProviderOutcomeAbsent,
			FailureCode:         "6001",
			ConfirmedAt:         now.Add(3 * time.Second),
		},
	); !errors.Is(err, ErrDirectProviderOperationBusy) {
		t.Fatalf("partial graph terminal failure = %v, want busy", err)
	}
	current, err := storage.GetDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if current.SubmissionStage != "campaign_created" ||
		current.ProviderCampaignID == nil ||
		*current.ProviderCampaignID != providerCampaignID {
		t.Fatalf("partial graph was not left recoverable: %#v", current)
	}
}

func TestDirectGraphTerminalEditFailureRestoresImmutableBaseline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2045, time.May, 8, 12, 0, 0, 0, time.UTC)
	campaign := createDirectTestCampaign(
		t, ctx, storage, owner, workspace.ID, now,
	)
	campaign, revision, baselineMarker := createDirectRejectedGraphBaseline(
		t, ctx, storage, owner, workspace.ID, campaign, now,
	)
	changedTexts := []string{"Provider must reject this exact edit"}
	edit, err := storage.ClaimDirectCampaignProviderEdit(
		ctx, owner, workspace.ID, campaign.ID,
		DirectCampaignChanges{
			Texts: &changedTexts, ExpectedVersion: campaign.Version,
		},
		campaign.ProviderGraphHash, campaign.ProviderRevisionID,
		"rejected_edit_"+campaign.ID, now.Add(20*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if edit.Campaign.ProviderGraphHash != "" ||
		edit.Campaign.ProviderRevisionID != "" {
		t.Fatalf("edit claim did not invalidate graph evidence: %#v", edit.Campaign)
	}
	var storedCampaignID, storedAdGroupID, storedAdID int64
	if err := storage.db.QueryRowContext(ctx, `SELECT
provider_campaign_id,provider_ad_group_id,provider_ad_id
FROM direct_provider_operations
WHERE workspace_id=$1 AND campaign_id=$2 AND id=$3`,
		workspace.ID, campaign.ID, edit.Operation.ID,
	).Scan(&storedCampaignID, &storedAdGroupID, &storedAdID); err != nil {
		t.Fatal(err)
	}
	if storedCampaignID != revision.ProviderCampaignID ||
		storedAdGroupID != revision.ProviderAdGroupID ||
		storedAdID != revision.ProviderAdID {
		t.Fatalf(
			"stored edit baseline IDs = (%d,%d,%d), want (%d,%d,%d)",
			storedCampaignID, storedAdGroupID, storedAdID,
			revision.ProviderCampaignID, revision.ProviderAdGroupID,
			revision.ProviderAdID,
		)
	}
	failed, err := storage.FailDirectCampaignGraphOperation(
		ctx, workspace.ID, campaign.ID, DirectGraphTerminalFailureInput{
			ExpectedOperationID: edit.Operation.ID,
			ExpectedClaimedAt:   edit.Operation.ClaimedAt,
			ProviderOutcome:     DirectGraphProviderOutcomeBaselineUnchanged,
			FailureCode:         "provider_validation_5007",
			ConfirmedAt:         now.Add(21 * time.Second),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if failed.SubmissionStage != "failed" ||
		failed.SubmissionFailureCode != "provider_validation_5007" ||
		failed.SubmissionOperationMarker != baselineMarker ||
		failed.ProviderGraphHash != revision.GraphHash ||
		failed.ProviderRevisionID != revision.ID ||
		failed.GraphVerifiedAt == nil ||
		!failed.GraphVerifiedAt.Equal(revision.ObservedAt) ||
		failed.Status != "rejected" ||
		failed.ModerationStatus != "REJECTED" ||
		failed.CampaignModeration.Status != "REJECTED" ||
		failed.AdGroupModeration.Status != "ACCEPTED" ||
		failed.AdModeration.Status != "ACCEPTED" ||
		len(failed.KeywordModeration) != len(revision.ProviderKeywordMappings) {
		t.Fatalf("restored provider edit baseline = %#v", failed)
	}
	if !sameDirectKeywordProviderIDs(
		failed.ProviderKeywordMappings, revision.ProviderKeywordMappings,
	) {
		t.Fatalf(
			"restored keyword mappings = %#v, want %#v",
			failed.ProviderKeywordMappings, revision.ProviderKeywordMappings,
		)
	}
	replayed, err := storage.ClaimDirectCampaignProviderEdit(
		ctx, owner, workspace.ID, campaign.ID,
		DirectCampaignChanges{
			Texts: &changedTexts, ExpectedVersion: campaign.Version,
		},
		failed.ProviderGraphHash, failed.ProviderRevisionID,
		edit.Operation.OperationMarker, now.Add(22*time.Second),
	)
	if err != nil {
		t.Fatalf("replay rejected provider edit: %v", err)
	}
	if replayed.Operation.ID != edit.Operation.ID ||
		replayed.Operation.Stage != "failed" ||
		replayed.Operation.CompletedAt == nil {
		t.Fatalf("rejected provider edit replay = %#v", replayed.Operation)
	}
	var sameMarkerCount int
	if err := storage.db.QueryRowContext(ctx, `SELECT COUNT(*)
FROM direct_provider_operations
WHERE connection_id=$1 AND operation_marker=$2`,
		campaign.ConnectionID, edit.Operation.OperationMarker,
	).Scan(&sameMarkerCount); err != nil {
		t.Fatal(err)
	}
	if sameMarkerCount != 1 {
		t.Fatalf("rejected edit marker rows = %d, want 1", sameMarkerCount)
	}
	correctedTexts := []string{"Corrected provider edit after rejection"}
	next, err := storage.ClaimDirectCampaignProviderEdit(
		ctx, owner, workspace.ID, campaign.ID,
		DirectCampaignChanges{
			Texts: &correctedTexts, ExpectedVersion: campaign.Version,
		},
		failed.ProviderGraphHash, failed.ProviderRevisionID,
		"corrected_edit_"+campaign.ID, now.Add(23*time.Second),
	)
	if err != nil {
		t.Fatalf("claim corrected provider edit: %v", err)
	}
	if next.Operation.ID == edit.Operation.ID || next.Operation.Stage != "claimed" {
		t.Fatalf("corrected provider edit operation = %#v", next.Operation)
	}
}

func createDirectRejectedGraphBaseline(
	t *testing.T, ctx context.Context, storage *Store, owner, workspaceID string,
	campaign DirectCampaign, now time.Time,
) (DirectCampaign, DirectCampaignRevision, string) {
	t.Helper()
	baselineMarker := "baseline_graph_" + campaign.ID
	material, err := storage.ClaimDirectCampaignGraphSubmission(
		ctx, owner, workspaceID, campaign.ID, campaign.Version,
		baselineMarker, now.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	providerCampaignID, providerAdGroupID, providerAdID :=
		int64(8001), int64(8002), int64(8003)
	keywordMappings := make([]DirectKeywordMapping, len(campaign.Keywords))
	for index, keyword := range campaign.Keywords {
		keywordMappings[index] = DirectKeywordMapping{
			Keyword: keyword, ProviderKeywordID: int64(8100 + index),
			Moderation: DirectModerationSnapshot{Status: "ACCEPTED"},
		}
	}
	advance := func(
		expectedStage string, update DirectProviderStageUpdate, at time.Time,
	) {
		t.Helper()
		update.ExpectedClaimedAt = material.Operation.ClaimedAt
		material, err = storage.AdvanceDirectCampaignGraphSubmission(
			ctx, workspaceID, campaign.ID, material.Operation.ID,
			expectedStage, update, at,
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	advance("claimed", DirectProviderStageUpdate{
		Stage: "campaign_created", ProviderCampaignID: &providerCampaignID,
	}, now.Add(2*time.Second))
	advance("campaign_created", DirectProviderStageUpdate{
		Stage: "ad_group_created", ProviderAdGroupID: &providerAdGroupID,
	}, now.Add(3*time.Second))
	advance("ad_group_created", DirectProviderStageUpdate{
		Stage: "ad_created", ProviderAdID: &providerAdID,
	}, now.Add(4*time.Second))
	advance("ad_created", DirectProviderStageUpdate{
		Stage: "keywords_created", ProviderKeywordMappings: &keywordMappings,
	}, now.Add(5*time.Second))
	observedGraph := directObservedGraphFixture(
		t, providerCampaignID, providerAdGroupID, providerAdID, keywordMappings,
		DirectModerationSnapshot{Status: "REJECTED", State: "OFF"},
		DirectModerationSnapshot{
			Status: "ACCEPTED", ServingStatus: "ELIGIBLE",
		},
		DirectModerationSnapshot{Status: "ACCEPTED", State: "OFF"},
	)
	graphHash := strings.Repeat("e", 64)
	if graphHash == directGraphHash(observedGraph) {
		t.Fatal("test provider fingerprint unexpectedly equals full JSON hash")
	}
	advance("keywords_created", DirectProviderStageUpdate{
		Stage: "graph_observed", ObservedGraph: observedGraph,
		GraphHash: graphHash, ProviderKeywordMappings: &keywordMappings,
	}, now.Add(6*time.Second))
	revision, err := storage.RecordVerifiedDirectCampaignGraph(
		ctx, workspaceID, campaign.ID, DirectVerifiedGraphInput{
			ExpectedOperationID:     material.Operation.ID,
			ExpectedStage:           "graph_observed",
			ExpectedCampaignVersion: campaign.Version,
			ExpectedClaimedAt:       material.Operation.ClaimedAt,
			GraphVersion:            DirectGraphFingerprintVersion,
			DesiredGraph:            material.Operation.DesiredGraph,
			ObservedGraph:           observedGraph, GraphHash: graphHash,
			ProviderCampaignID: providerCampaignID,
			ProviderAdGroupID:  providerAdGroupID, ProviderAdID: providerAdID,
			ProviderKeywordMappings: keywordMappings,
			CampaignModeration:      DirectModerationSnapshot{Status: "REJECTED", State: "OFF"},
			AdGroupModeration: DirectModerationSnapshot{
				Status: "ACCEPTED", ServingStatus: "ELIGIBLE",
			},
			AdModeration:              DirectModerationSnapshot{Status: "ACCEPTED", State: "OFF"},
			AggregateModerationStatus: "REJECTED",
			ObservedAt:                now.Add(7 * time.Second), ActorUserID: owner,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.UpdateDirectCampaignGraphModeration(
		ctx, workspaceID, campaign.ID, DirectGraphModerationUpdate{
			ExpectedGraphHash: graphHash, ExpectedRevisionID: revision.ID,
			Campaign: DirectModerationSnapshot{Status: "REJECTED", State: "OFF"},
			AdGroup: DirectModerationSnapshot{
				Status: "ACCEPTED", ServingStatus: "ELIGIBLE",
			},
			Ad:                        DirectModerationSnapshot{Status: "ACCEPTED", State: "OFF"},
			Keywords:                  keywordMappings,
			AggregateModerationStatus: "REJECTED",
			ProviderStatus:            "REJECTED", ProviderState: "OFF",
			CheckedAt: now.Add(8 * time.Second),
		},
	); err != nil {
		t.Fatal(err)
	}
	material, err = storage.ReloadDirectCampaignGraphSubmission(
		ctx, workspaceID, campaign.ID, material.Operation.ID,
		now.Add(9*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	advance("verified", DirectProviderStageUpdate{
		Stage: "moderation_requested",
	}, now.Add(10*time.Second))
	advance("moderation_requested", DirectProviderStageUpdate{
		Stage: "completed", Complete: true,
	}, now.Add(11*time.Second))
	campaign, err = storage.GetDirectCampaign(
		ctx, owner, workspaceID, campaign.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	return campaign, revision, baselineMarker
}

func TestDirectGraphTerminalFailureFenceRejectsStaleOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2045, time.May, 9, 12, 0, 0, 0, time.UTC)
	campaign := createDirectTestCampaign(
		t, ctx, storage, owner, workspace.ID, now,
	)
	material, err := storage.ClaimDirectCampaignGraphSubmission(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
		"terminal_fence_"+campaign.ID, now.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	recoveryAt := material.Operation.LeaseExpiresAt.Add(time.Second)
	if _, err := storage.ClaimDirectCampaignGraphRecoveryCandidates(
		ctx, recoveryAt, 10,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.FailDirectCampaignGraphOperation(
		ctx, workspace.ID, campaign.ID, DirectGraphTerminalFailureInput{
			ExpectedOperationID: material.Operation.ID,
			ExpectedClaimedAt:   material.Operation.ClaimedAt,
			ProviderOutcome:     DirectGraphProviderOutcomeAbsent,
			FailureCode:         "6000",
			ConfirmedAt:         recoveryAt.Add(time.Second),
		},
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale terminal failure = %v, want conflict", err)
	}
}

func TestDirectGraphRevisionEvidenceRejectsIncompleteSnapshot(t *testing.T) {
	t.Parallel()
	revision := DirectCampaignRevision{
		ObservedGraph: json.RawMessage(`{"campaign":{"status":"ACCEPTED","state":"OFF"}}`),
	}
	if _, err := directGraphRevisionEvidence(
		revision,
	); !errors.Is(err, ErrDirectGraphUnverified) {
		t.Fatalf("incomplete revision evidence = %v, want graph unverified", err)
	}
}
