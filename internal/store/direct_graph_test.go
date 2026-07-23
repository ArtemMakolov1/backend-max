package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"maxpilot/backend/internal/yandexdirect"

	"golang.org/x/text/unicode/norm"
)

func directObservedGraphFixture(
	t *testing.T, providerCampaignID, providerAdGroupID, providerAdID int64,
	mappings []DirectKeywordMapping, campaign, adGroup,
	ad DirectModerationSnapshot,
) json.RawMessage {
	t.Helper()
	keywords := make([]map[string]any, len(mappings))
	for index, mapping := range mappings {
		keywords[index] = map[string]any{
			"id":          mapping.ProviderKeywordID,
			"campaign_id": providerCampaignID,
			"ad_group_id": providerAdGroupID,
			"keyword":     mapping.Keyword,
			"status":      mapping.Moderation.Status,
			"state":       mapping.Moderation.State,
		}
	}
	observed, err := canonicalDirectJSONObject(mustJSON(map[string]any{
		"campaign": map[string]any{
			"id": providerCampaignID, "status": campaign.Status,
			"state": campaign.State,
		},
		"ad_groups": []map[string]any{{
			"id": providerAdGroupID, "campaign_id": providerCampaignID,
			"status": adGroup.Status, "serving_status": adGroup.ServingStatus,
		}},
		"ads": []map[string]any{{
			"id": providerAdID, "campaign_id": providerCampaignID,
			"ad_group_id": providerAdGroupID, "status": ad.Status,
			"state": ad.State, "status_clarification": ad.StatusClarification,
		}},
		"keywords": keywords,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return observed
}

func directYandexProviderGraphFixture(
	t *testing.T, providerCampaignID, providerAdGroupID, providerAdID int64,
	mappings []DirectKeywordMapping,
) (json.RawMessage, string) {
	t.Helper()
	keywords := make([]yandexdirect.Keyword, len(mappings))
	for index, mapping := range mappings {
		keywords[index] = yandexdirect.Keyword{
			ID: mapping.ProviderKeywordID, CampaignID: providerCampaignID,
			AdGroupID: providerAdGroupID, Keyword: mapping.Keyword,
			StrategyPriority: "NORMAL", Status: mapping.Moderation.Status,
			State: "OFF",
		}
	}
	graph := yandexdirect.CampaignGraph{
		Campaign: yandexdirect.GraphCampaign{
			ID: providerCampaignID, Name: "Campaign graph fixture",
			Status: "DRAFT", State: "OFF", Type: "UNIFIED_CAMPAIGN",
			WeeklyBudgetMinor: 30_000,
			StartsAt: time.Date(
				2044, time.January, 2, 0, 0, 0, 0, time.UTC,
			),
			EndsAt: time.Date(
				2044, time.February, 2, 0, 0, 0, 0, time.UTC,
			),
			TimeZone: "Europe/Moscow",
			BiddingStrategy: json.RawMessage(
				`{"Network":{"WbMaximumClicks":{"WeeklySpendLimit":300000000},"BiddingStrategyType":"WB_MAXIMUM_CLICKS"},"Search":{"BiddingStrategyType":"SERVING_OFF"}}`,
			),
			AttributionModel: "AUTO",
		},
		AdGroups: []yandexdirect.UnifiedAdGroup{{
			ID: providerAdGroupID, CampaignID: providerCampaignID,
			Name: "Campaign graph group", RegionIDs: []int64{225},
			OfferRetargeting: "NO", Status: "DRAFT",
		}},
		Ads: []yandexdirect.ResponsiveAd{{
			ID: providerAdID, CampaignID: providerCampaignID,
			AdGroupID: providerAdGroupID,
			Titles: []yandexdirect.ModeratedText{{
				Value: "Заголовок", Status: "DRAFT",
			}},
			Texts: []yandexdirect.ModeratedText{{
				Value: "Текст объявления", Status: "DRAFT",
			}},
			Href: "https://example.com", Status: "DRAFT", State: "OFF",
		}},
		Keywords: keywords,
	}
	fingerprint, err := graph.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(graph)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := canonicalDirectJSONObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	if directGraphHash(observed) == fingerprint {
		t.Fatal("provider fingerprint unexpectedly equals full graph JSON hash")
	}
	return observed, fingerprint
}

func TestDirectGraphConcurrentClaimsFenceSecondProviderWorker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2042, time.February, 3, 12, 0, 0, 0, time.UTC)
	campaign := createDirectTestCampaign(
		t, ctx, storage, owner, workspace.ID, now,
	)

	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, marker := range []string{"concurrent_first", "concurrent_second"} {
		marker := marker
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, err := storage.ClaimDirectCampaignGraphSubmission(
				ctx, owner, workspace.ID, campaign.ID, campaign.Version,
				marker, now.Add(time.Second),
			)
			results <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	var succeeded, busy int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrDirectProviderOperationBusy):
			busy++
		default:
			t.Fatalf("concurrent claim returned %v", err)
		}
	}
	if succeeded != 1 || busy != 1 {
		t.Fatalf(
			"concurrent claims: success=%d busy=%d, want one of each",
			succeeded, busy,
		)
	}

	var operationID string
	var leaseBefore time.Time
	if err := storage.db.QueryRowContext(ctx, `SELECT o.id,o.lease_expires_at
FROM direct_campaigns c
JOIN direct_provider_operations o
  ON o.workspace_id=c.workspace_id
 AND o.campaign_id=c.id
 AND o.id=c.submission_operation_id
WHERE c.workspace_id=$1 AND c.id=$2`,
		workspace.ID, campaign.ID,
	).Scan(&operationID, &leaseBefore); err != nil {
		t.Fatal(err)
	}
	material, err := storage.GetDirectCampaignGraphSubmissionMaterial(
		ctx, workspace.ID, campaign.ID, operationID, now.Add(time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if material.Operation.ID != operationID {
		t.Fatalf("material operation = %q, want %q", material.Operation.ID, operationID)
	}
	var leaseAfter time.Time
	if err := storage.db.QueryRowContext(ctx, `SELECT lease_expires_at
FROM direct_provider_operations
WHERE workspace_id=$1 AND campaign_id=$2 AND id=$3`,
		workspace.ID, campaign.ID, operationID,
	).Scan(&leaseAfter); err != nil {
		t.Fatal(err)
	}
	if !leaseAfter.Equal(leaseBefore) {
		t.Fatalf(
			"read-only material load renewed lease from %s to %s",
			leaseBefore, leaseAfter,
		)
	}
}

func TestDirectGraphExpiredExactClaimReclaimsWithNewFence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2042, time.February, 4, 12, 0, 0, 0, time.UTC)
	campaign := createDirectTestCampaign(
		t, ctx, storage, owner, workspace.ID, now,
	)
	first, err := storage.ClaimDirectCampaignGraphSubmission(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
		"expired_exact_first", now.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	reclaimAt := first.Operation.LeaseExpiresAt.Add(time.Second)
	reclaimed, err := storage.ClaimDirectCampaignGraphSubmission(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
		"expired_exact_retry", reclaimAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.Operation.ID != first.Operation.ID ||
		reclaimed.Operation.OperationMarker != first.Operation.OperationMarker ||
		!reclaimed.Operation.ClaimedAt.Equal(reclaimAt) ||
		reclaimed.Operation.ClaimedAt.Equal(first.Operation.ClaimedAt) {
		t.Fatalf("reclaimed material = %#v; first = %#v", reclaimed, first)
	}
	var claimSnapshots int
	if err := storage.db.QueryRowContext(ctx, `SELECT count(*)
FROM direct_provider_operation_journal
WHERE operation_id=$1 AND stage='claimed'`,
		reclaimed.Operation.ID,
	).Scan(&claimSnapshots); err != nil {
		t.Fatal(err)
	}
	if claimSnapshots != 2 {
		t.Fatalf("claimed journal snapshots = %d, want 2", claimSnapshots)
	}
	providerCampaignID := int64(4101)
	if _, err := storage.AdvanceDirectCampaignGraphSubmission(
		ctx, workspace.ID, campaign.ID, first.Operation.ID, "claimed",
		DirectProviderStageUpdate{
			ExpectedClaimedAt:  first.Operation.ClaimedAt,
			Stage:              "campaign_created",
			ProviderCampaignID: &providerCampaignID,
		},
		reclaimAt.Add(time.Second),
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale HTTP owner advance = %v, want conflict", err)
	}
}

func TestDirectGraphRecoveryLeasesRepeatedReconciliation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connection := connectDirectTestAccount(
		t, ctx, storage, owner, workspace.ID,
	)
	now := time.Date(2043, time.March, 4, 12, 0, 0, 0, time.UTC)
	campaign := createDirectTestCampaign(
		t, ctx, storage, owner, workspace.ID, now,
	)
	material, err := storage.ClaimDirectCampaignGraphSubmission(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
		"recover_"+campaign.ID, now.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	providerCampaignID, providerAdGroupID := int64(501), int64(502)
	material, err = storage.AdvanceDirectCampaignGraphSubmission(
		ctx, workspace.ID, campaign.ID, material.Operation.ID, "claimed",
		DirectProviderStageUpdate{
			ExpectedClaimedAt: material.Operation.ClaimedAt,
			Stage:             "campaign_created", ProviderCampaignID: &providerCampaignID,
		}, now.Add(2*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	material, err = storage.AdvanceDirectCampaignGraphSubmission(
		ctx, workspace.ID, campaign.ID, material.Operation.ID,
		"campaign_created", DirectProviderStageUpdate{
			ExpectedClaimedAt: material.Operation.ClaimedAt,
			Stage:             "reconciling",
		},
		now.Add(3*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	material, err = storage.AdvanceDirectCampaignGraphSubmission(
		ctx, workspace.ID, campaign.ID, material.Operation.ID, "reconciling",
		DirectProviderStageUpdate{
			ExpectedClaimedAt: material.Operation.ClaimedAt,
			Stage:             "ad_group_created", ProviderAdGroupID: &providerAdGroupID,
		}, now.Add(4*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	material, err = storage.AdvanceDirectCampaignGraphSubmission(
		ctx, workspace.ID, campaign.ID, material.Operation.ID,
		"ad_group_created", DirectProviderStageUpdate{
			ExpectedClaimedAt: material.Operation.ClaimedAt,
			Stage:             "reconciling",
		},
		now.Add(5*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	var reconciliationSnapshots int
	if err := storage.db.QueryRowContext(ctx, `SELECT count(*)
FROM direct_provider_operation_journal
WHERE operation_id=$1 AND stage='reconciling'`,
		material.Operation.ID).Scan(&reconciliationSnapshots); err != nil {
		t.Fatal(err)
	}
	if reconciliationSnapshots != 2 {
		t.Fatalf(
			"reconciliation snapshots = %d, want 2",
			reconciliationSnapshots,
		)
	}
	recoveryAt := now.Add(5*time.Second + directProviderOperationLease + time.Second)
	candidates, err := storage.ClaimDirectCampaignGraphRecoveryCandidates(
		ctx, recoveryAt, 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 ||
		candidates[0].WorkspaceID != workspace.ID ||
		candidates[0].CampaignID != campaign.ID ||
		candidates[0].OperationID != material.Operation.ID ||
		candidates[0].Stage != "reconciling" {
		t.Fatalf("recovery candidates = %#v", candidates)
	}
	if err := storage.db.QueryRowContext(ctx, `SELECT count(*)
FROM direct_provider_operation_journal
WHERE operation_id=$1 AND stage='reconciling'`,
		material.Operation.ID,
	).Scan(&reconciliationSnapshots); err != nil {
		t.Fatal(err)
	}
	if reconciliationSnapshots != 3 {
		t.Fatalf(
			"reconciliation snapshots after recovery = %d, want 3",
			reconciliationSnapshots,
		)
	}
	if _, err := storage.AdvanceDirectCampaignGraphSubmission(
		ctx, workspace.ID, campaign.ID, material.Operation.ID, "reconciling",
		DirectProviderStageUpdate{
			ExpectedClaimedAt: material.Operation.ClaimedAt,
			Stage:             "reconciling",
		},
		recoveryAt.Add(time.Second),
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale recovery owner advance = %v, want conflict", err)
	}
	if err := storage.RevokeDirectConnection(
		ctx, owner, workspace.ID, recoveryAt,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("revoke during graph recovery = %v, want conflict", err)
	}
	reloaded, err := storage.ReloadDirectCampaignGraphSubmission(
		ctx, workspace.ID, campaign.ID, candidates[0].OperationID,
		recoveryAt.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Connection.ID != connection.ID ||
		reloaded.Operation.Stage != "reconciling" ||
		reloaded.Operation.ClaimedAt.Equal(material.Operation.ClaimedAt) ||
		!reloaded.Operation.LeaseExpiresAt.After(recoveryAt) {
		t.Fatalf("reloaded recovery material = %#v", reloaded)
	}
}

func TestDirectGraphRecoveryFencesStaleVerifiedRevision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2043, time.March, 5, 12, 0, 0, 0, time.UTC)
	campaign := createDirectTestCampaign(
		t, ctx, storage, owner, workspace.ID, now,
	)
	material, err := storage.ClaimDirectCampaignGraphSubmission(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
		"verify_fence_"+campaign.ID, now.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	providerCampaignID, providerAdGroupID, providerAdID :=
		int64(5101), int64(5102), int64(5103)
	keywordMappings := make([]DirectKeywordMapping, len(campaign.Keywords))
	for index, keyword := range campaign.Keywords {
		keywordMappings[index] = DirectKeywordMapping{
			Keyword: keyword, ProviderKeywordID: int64(5200 + index),
			Moderation: DirectModerationSnapshot{Status: "DRAFT"},
		}
	}
	advance := func(
		expectedStage string, update DirectProviderStageUpdate, at time.Time,
	) {
		t.Helper()
		update.ExpectedClaimedAt = material.Operation.ClaimedAt
		material, err = storage.AdvanceDirectCampaignGraphSubmission(
			ctx, workspace.ID, campaign.ID, material.Operation.ID,
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
	observedGraph, graphHash := directYandexProviderGraphFixture(
		t, providerCampaignID, providerAdGroupID, providerAdID, keywordMappings,
	)
	advance("keywords_created", DirectProviderStageUpdate{
		Stage: "graph_observed", ObservedGraph: observedGraph,
		GraphHash: graphHash, ProviderKeywordMappings: &keywordMappings,
	}, now.Add(6*time.Second))
	staleClaimedAt := material.Operation.ClaimedAt
	recoveryAt := material.Operation.LeaseExpiresAt.Add(time.Second)
	candidates, err := storage.ClaimDirectCampaignGraphRecoveryCandidates(
		ctx, recoveryAt, 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].OperationID != material.Operation.ID {
		t.Fatalf("recovery candidates = %#v", candidates)
	}
	reclaimed, err := storage.GetDirectCampaignGraphSubmissionMaterial(
		ctx, workspace.ID, campaign.ID, material.Operation.ID, recoveryAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	input := DirectVerifiedGraphInput{
		ExpectedOperationID:     material.Operation.ID,
		ExpectedStage:           "graph_observed",
		ExpectedCampaignVersion: campaign.Version,
		ExpectedClaimedAt:       staleClaimedAt,
		GraphVersion:            DirectGraphFingerprintVersion,
		DesiredGraph:            material.Operation.DesiredGraph,
		ObservedGraph:           observedGraph, GraphHash: graphHash,
		ProviderCampaignID: providerCampaignID,
		ProviderAdGroupID:  providerAdGroupID, ProviderAdID: providerAdID,
		ProviderKeywordMappings:   keywordMappings,
		CampaignModeration:        DirectModerationSnapshot{Status: "DRAFT"},
		AdGroupModeration:         DirectModerationSnapshot{Status: "DRAFT"},
		AdModeration:              DirectModerationSnapshot{Status: "DRAFT"},
		AggregateModerationStatus: "MODERATION",
		ObservedAt:                recoveryAt.Add(time.Second), ActorUserID: owner,
	}
	if _, err := storage.RecordVerifiedDirectCampaignGraph(
		ctx, workspace.ID, campaign.ID, input,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale verified graph record = %v, want conflict", err)
	}
	input.ExpectedClaimedAt = reclaimed.Operation.ClaimedAt
	input.ObservedAt = recoveryAt.Add(2 * time.Second)
	if _, err := storage.RecordVerifiedDirectCampaignGraph(
		ctx, workspace.ID, campaign.ID, input,
	); err != nil {
		t.Fatalf("reclaimed verified graph record: %v", err)
	}
}

func TestDirectProviderEditInvalidatesGraphAndConsentAtClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connection := connectDirectTestAccount(
		t, ctx, storage, owner, workspace.ID,
	)
	now := time.Date(2044, time.April, 5, 12, 0, 0, 0, time.UTC)
	campaign := createDirectTestCampaign(
		t, ctx, storage, owner, workspace.ID, now,
	)
	campaign = acceptDirectTestCampaign(
		t, ctx, storage, owner, workspace.ID, campaign, now,
	)
	oldGraphHash, oldRevisionID := campaign.ProviderGraphHash, campaign.ProviderRevisionID
	if _, err := storage.GrantDirectAutoLaunchConsent(
		ctx, owner, workspace.ID, campaign.ID, DirectConsentRequest{
			Confirmation: "АВТОЗАПУСК", ExpectedVersion: campaign.Version,
			ExpectedConnectionID: connection.ID,
			ExpectedAccountID:    connection.AccountID,
			ExpectedCampaignName: campaign.Name,
			ExpectedProviderID:   *campaign.ProviderCampaignID,
			ExpectedGraphHash:    oldGraphHash, ExpectedRevisionID: oldRevisionID,
			WeeklyBudgetMinor: campaign.WeeklyBudgetMinor,
			StartsAt:          campaign.StartsAt, EndsAt: campaign.EndsAt,
			AuthorizedAt: now.Add(11 * time.Second),
		},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.ExecContext(ctx, `UPDATE direct_campaign_revisions
SET moderation_status='REJECTED' WHERE id=$1`, oldRevisionID); err == nil {
		t.Fatal("immutable Direct graph revision was updated")
	}
	texts := []string{"Новая проверенная версия объявления"}
	changes := DirectCampaignChanges{
		Texts: &texts, ExpectedVersion: campaign.Version,
	}
	marker := "provider_edit_" + campaign.ID
	material, err := storage.ClaimDirectCampaignProviderEdit(
		ctx, owner, workspace.ID, campaign.ID, changes,
		oldGraphHash, oldRevisionID, marker, now.Add(12*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if material.Operation.ExpectedGraphHash != oldGraphHash ||
		material.Operation.ExpectedRevisionID != oldRevisionID ||
		material.Campaign.ProviderGraphHash != "" ||
		material.Campaign.ProviderRevisionID != "" ||
		material.Campaign.GraphVerifiedAt != nil {
		t.Fatalf("provider edit claim retained unsafe graph evidence: %#v", material)
	}
	current, err := storage.GetDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if current.AutoLaunch.Valid ||
		current.AutoLaunch.InvalidReason != "provider_edit_claimed" {
		t.Fatalf("provider edit retained auto-launch consent: %#v", current.AutoLaunch)
	}
	retried, err := storage.ClaimDirectCampaignProviderEdit(
		ctx, owner, workspace.ID, campaign.ID, changes,
		oldGraphHash, oldRevisionID, marker, now.Add(13*time.Second),
	)
	if err != nil || retried.Operation.ID != material.Operation.ID {
		t.Fatalf("idempotent provider edit retry = %#v, %v", retried, err)
	}
	if _, err := storage.ClaimDirectCampaignProviderEdit(
		ctx, owner, workspace.ID, campaign.ID, changes,
		oldGraphHash, oldRevisionID, "different_"+campaign.ID,
		now.Add(14*time.Second),
	); !errors.Is(err, ErrDirectProviderOperationBusy) {
		t.Fatalf("parallel provider edit error = %v, want busy", err)
	}
}

func TestDirectResponsiveAdTextLimitsUseUnicodeRunes(t *testing.T) {
	t.Parallel()
	campaign := validDirectGraphTestCampaign()
	campaign.Titles = []string{
		strings.Repeat("я", 22) + " " + strings.Repeat("ю", 22) + " " +
			strings.Repeat("э", 10),
	}
	campaign.Texts = []string{
		strings.Repeat("я", 23) + " " + strings.Repeat("ю", 23) + " " +
			strings.Repeat("э", 23) + " " + strings.Repeat("а", 9),
	}
	if err := validateDirectCampaignDraft(&campaign); err != nil {
		t.Fatalf("valid Unicode limits were rejected: %v", err)
	}

	tooLongTitle := validDirectGraphTestCampaign()
	tooLongTitle.Titles = []string{
		strings.Repeat("я", directMaxTitleRunes+1),
	}
	if err := validateDirectCampaignDraft(&tooLongTitle); err == nil {
		t.Fatal("title above the Unicode rune limit was accepted")
	}

	tooLongWord := validDirectGraphTestCampaign()
	tooLongWord.Texts = []string{
		strings.Repeat("я", directMaxTextWordRunes+1),
	}
	if err := validateDirectCampaignDraft(&tooLongWord); err == nil {
		t.Fatal("text word above the Unicode rune limit was accepted")
	}
}

func TestDirectGraphDraftRejectsNormalizedAndCaseFoldedDuplicates(t *testing.T) {
	t.Parallel()
	campaign := validDirectGraphTestCampaign()
	composed := "Caf\u00e9"
	decomposed := norm.NFD.String(composed)
	campaign.Titles = []string{composed, decomposed}
	if err := validateDirectCampaignDraft(&campaign); err == nil {
		t.Fatal("canonically equivalent titles were accepted")
	}

	campaign = validDirectGraphTestCampaign()
	campaign.Keywords = []string{"Ведение канала", "ведение канала"}
	if err := validateDirectCampaignDraft(&campaign); err == nil {
		t.Fatal("case-folded duplicate keywords were accepted")
	}

	campaign = validDirectGraphTestCampaign()
	campaign.Titles = []string{"Ａгентство", "Aгентство"}
	if err := validateDirectCampaignDraft(&campaign); err == nil {
		t.Fatal("NFKC-equivalent titles were accepted")
	}

	campaign = validDirectGraphTestCampaign()
	campaign.Regions = []string{"①", "1"}
	if err := validateDirectCampaignDraft(&campaign); err == nil {
		t.Fatal("NFKC-equivalent regions were accepted")
	}
}

func TestDirectProviderOperationRejectsPastStartOrEndDate(t *testing.T) {
	t.Parallel()
	now := time.Date(2042, time.January, 9, 21, 30, 0, 0, time.UTC)
	campaign := validDirectGraphTestCampaign()
	providerToday := time.Date(
		2042, time.January, 10, 0, 0, 0, 0, time.UTC,
	)
	campaign.StartsAt = providerToday
	campaign.EndsAt = providerToday.AddDate(0, 1, 0)
	if err := validateDirectProviderOperationDates(campaign, now); err != nil {
		t.Fatalf("Moscow today's provider dates were rejected: %v", err)
	}
	campaign.StartsAt = providerToday.AddDate(0, 0, -1)
	if err := validateDirectProviderOperationDates(
		campaign, now,
	); !errors.Is(err, ErrDirectValidation) {
		t.Fatalf("past start date error = %v, want validation", err)
	}
	campaign.StartsAt = providerToday
	campaign.EndsAt = providerToday.AddDate(0, 0, -1)
	if err := validateDirectProviderOperationDates(
		campaign, now,
	); !errors.Is(err, ErrDirectValidation) {
		t.Fatalf("past end date error = %v, want validation", err)
	}
}

func TestDirectProviderEditAllowsOnlyUnchangedLocalMetadata(t *testing.T) {
	t.Parallel()
	campaign := validDirectGraphTestCampaign()
	campaign.Objective = "  traffic "
	campaign.Brief = "  " + campaign.Brief + " "
	objective := "\ttraffic\n"
	brief := strings.TrimSpace(campaign.Brief)
	if !directProviderEditLocalMetadataUnchanged(
		campaign, DirectCampaignChanges{Objective: &objective, Brief: &brief},
	) {
		t.Fatal("trim-space equivalent objective and brief were rejected")
	}
	changed := brief + " changed"
	if directProviderEditLocalMetadataUnchanged(
		campaign, DirectCampaignChanges{Brief: &changed},
	) {
		t.Fatal("changed local-only brief was accepted for provider edit")
	}
}

func TestDirectKeywordMappingsAreExactAndFailClosed(t *testing.T) {
	t.Parallel()
	keywords := []string{"первый запрос", "второй запрос"}
	valid := []DirectKeywordMapping{
		{Keyword: keywords[0], ProviderKeywordID: 101},
		{Keyword: keywords[1], ProviderKeywordID: 102},
	}
	if err := validateDirectKeywordMappings(valid, keywords, true); err != nil {
		t.Fatalf("exact keyword mappings were rejected: %v", err)
	}
	reordered := []DirectKeywordMapping{valid[1], valid[0]}
	if err := validateDirectKeywordMappings(
		reordered, keywords, true,
	); !errors.Is(err, ErrDirectConsentMismatch) {
		t.Fatalf("reordered mappings error = %v, want consent mismatch", err)
	}
	duplicateID := append([]DirectKeywordMapping(nil), valid...)
	duplicateID[1].ProviderKeywordID = duplicateID[0].ProviderKeywordID
	if err := validateDirectKeywordMappings(
		duplicateID, keywords, true,
	); !errors.Is(err, ErrDirectValidation) {
		t.Fatalf("duplicate provider id error = %v, want validation", err)
	}
}

func TestDirectObservedGraphHashIsCanonical(t *testing.T) {
	t.Parallel()
	left, err := canonicalDirectJSONObject(
		[]byte(`{"z":[3,2,1],"a":{"b":true,"a":1}}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	right, err := canonicalDirectJSONObject(
		[]byte(`{ "a": { "a": 1, "b": true }, "z": [3,2,1] }`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(left, right) || directGraphHash(left) != directGraphHash(right) {
		t.Fatalf("canonical graph/hash differ: %s != %s", left, right)
	}
	if _, err := canonicalDirectJSONObject([]byte(`[{"a":1}]`)); err == nil {
		t.Fatal("non-object observed graph was accepted")
	}
}

func TestDirectObservedGraphStructureAcceptsStatusIndependentFingerprint(
	t *testing.T,
) {
	t.Parallel()
	mappings := []DirectKeywordMapping{{
		Keyword: "ведение канала", ProviderKeywordID: 104,
		Moderation: DirectModerationSnapshot{Status: "DRAFT"},
	}}
	draft := directObservedGraphFixture(
		t, 101, 102, 103, mappings,
		DirectModerationSnapshot{Status: "DRAFT", State: "OFF"},
		DirectModerationSnapshot{Status: "DRAFT"},
		DirectModerationSnapshot{Status: "DRAFT", State: "OFF"},
	)
	mappings[0].Moderation.Status = "ACCEPTED"
	accepted := directObservedGraphFixture(
		t, 101, 102, 103, mappings,
		DirectModerationSnapshot{Status: "ACCEPTED", State: "ON"},
		DirectModerationSnapshot{Status: "ACCEPTED", ServingStatus: "ELIGIBLE"},
		DirectModerationSnapshot{Status: "ACCEPTED", State: "ON"},
	)
	providerFingerprint := strings.Repeat("a", 64)
	if !directGraphHashPattern.MatchString(providerFingerprint) {
		t.Fatal("valid provider fingerprint was rejected")
	}
	for _, observed := range []json.RawMessage{draft, accepted} {
		if err := validateDirectObservedGraphStructure(
			observed, 101, 102, 103, mappings,
		); err != nil {
			t.Fatalf("status-only graph change was rejected: %v", err)
		}
		if directGraphHash(observed) == providerFingerprint {
			t.Fatal("fixture does not prove fingerprint independence")
		}
	}
	if directGraphHash(draft) == directGraphHash(accepted) {
		t.Fatal("status change did not alter full observed JSON hash")
	}
}

func TestDirectProviderStageTransitionsRequireRevisionBoundary(t *testing.T) {
	t.Parallel()
	submission := [][2]string{
		{"claimed", "campaign_created"},
		{"campaign_created", "ad_group_created"},
		{"ad_group_created", "ad_created"},
		{"ad_created", "keywords_created"},
		{"keywords_created", "graph_observed"},
		{"verified", "moderation_requested"},
		{"moderation_requested", "completed"},
	}
	for _, transition := range submission {
		if !validDirectProviderStageTransition(
			"submission", transition[0], transition[1],
		) {
			t.Fatalf("valid transition %q -> %q was rejected", transition[0], transition[1])
		}
	}
	if validDirectProviderStageTransition(
		"submission", "graph_observed", "verified",
	) {
		t.Fatal("graph_observed bypassed immutable revision creation")
	}
	for _, transition := range [][2]string{
		{"campaign_created", "reconciling"},
		{"reconciling", "ad_group_created"},
		{"ad_group_created", "reconciling"},
		{"reconciling", "ad_created"},
	} {
		if !validDirectProviderStageTransition(
			"submission", transition[0], transition[1],
		) {
			t.Fatalf(
				"repeated reconciliation transition %q -> %q was rejected",
				transition[0], transition[1],
			)
		}
	}
	operation := DirectProviderOperation{}
	if err := mergeDirectProviderStageUpdate(
		&operation, DirectProviderStageUpdate{Stage: "verified"},
	); !errors.Is(err, ErrDirectValidation) {
		t.Fatalf("Advance verified error = %v, want validation", err)
	}
}

func TestDirectProviderStageUpdateKeepsEmptyJSONArrays(t *testing.T) {
	t.Parallel()
	var mappings []DirectKeywordMapping
	var warnings []DirectProviderIssue
	providerCampaignID := int64(1)
	operation := DirectProviderOperation{}
	if err := mergeDirectProviderStageUpdate(
		&operation, DirectProviderStageUpdate{
			Stage:                   "campaign_created",
			ProviderCampaignID:      &providerCampaignID,
			ProviderKeywordMappings: &mappings,
			ProviderWarnings:        &warnings,
		},
	); err != nil {
		t.Fatal(err)
	}
	if operation.ProviderKeywordMappings == nil || operation.ProviderWarnings == nil {
		t.Fatalf("empty provider arrays became nil: %#v", operation)
	}
	mappingsJSON, err := json.Marshal(operation.ProviderKeywordMappings)
	if err != nil {
		t.Fatal(err)
	}
	warningsJSON, err := json.Marshal(operation.ProviderWarnings)
	if err != nil {
		t.Fatal(err)
	}
	if string(mappingsJSON) != "[]" || string(warningsJSON) != "[]" {
		t.Fatalf(
			"empty provider arrays = mappings:%s warnings:%s, want []",
			mappingsJSON, warningsJSON,
		)
	}
}

func TestDirectVerifiedGraphReplayRequiresExactImmutableRevision(t *testing.T) {
	t.Parallel()
	desired, err := canonicalDirectJSONObject([]byte(`{"name":"campaign"}`))
	if err != nil {
		t.Fatal(err)
	}
	observed, err := canonicalDirectJSONObject([]byte(`{"campaign_id":101}`))
	if err != nil {
		t.Fatal(err)
	}
	hash := directGraphHash(observed)
	mappings := []DirectKeywordMapping{{
		Keyword: "ведение канала", ProviderKeywordID: 104,
		Moderation: DirectModerationSnapshot{Status: "DRAFT"},
	}}
	campaignID, adGroupID, adID := int64(101), int64(102), int64(103)
	input := DirectVerifiedGraphInput{
		ExpectedOperationID:     "dpop_" + strings.Repeat("a", 32),
		ExpectedCampaignVersion: 7, GraphVersion: DirectGraphFingerprintVersion,
		DesiredGraph: desired, ObservedGraph: observed, GraphHash: hash,
		ProviderCampaignID: campaignID, ProviderAdGroupID: adGroupID,
		ProviderAdID: adID, ProviderKeywordMappings: mappings,
		CampaignModeration:        DirectModerationSnapshot{Status: "DRAFT"},
		AdGroupModeration:         DirectModerationSnapshot{Status: "DRAFT"},
		AdModeration:              DirectModerationSnapshot{Status: "DRAFT"},
		AggregateModerationStatus: "MODERATION",
		ActorUserID:               "owner",
	}
	operation := DirectProviderOperation{
		ID: input.ExpectedOperationID, OperationKind: "submission",
		ExpectedCampaignVersion: input.ExpectedCampaignVersion,
		ActorUserID:             input.ActorUserID, DesiredGraph: desired,
		ObservedGraph: observed, GraphHash: hash,
		RevisionID:         "drev_" + strings.Repeat("b", 32),
		ProviderCampaignID: &campaignID, ProviderAdGroupID: &adGroupID,
		ProviderAdID: &adID,
	}
	revision := DirectCampaignRevision{
		ID: operation.RevisionID, CampaignVersion: input.ExpectedCampaignVersion,
		GraphVersion: input.GraphVersion, DesiredGraph: desired,
		ObservedGraph: observed, GraphHash: hash,
		ProviderCampaignID: campaignID, ProviderAdGroupID: adGroupID,
		ProviderAdID: adID, ProviderKeywordMappings: mappings,
		ProviderWarnings: []DirectProviderIssue{},
		ModerationStatus: "MODERATION", ActorUserID: input.ActorUserID,
	}
	if !directVerifiedGraphReplayMatches(
		operation, revision, input, desired, observed,
	) {
		t.Fatal("exact verified graph replay was rejected")
	}
	input.ProviderAdID++
	if directVerifiedGraphReplayMatches(
		operation, revision, input, desired, observed,
	) {
		t.Fatal("mismatched verified graph replay was accepted")
	}
}

func TestDirectProviderEditEvidenceDoesNotRequireAcceptedModeration(t *testing.T) {
	t.Parallel()
	campaign := readyDirectGraphTestCampaign()
	campaign.Status = "rejected"
	campaign.ProviderStatus = "REJECTED"
	campaign.ModerationStatus = "REJECTED"
	campaign.CampaignModeration.Status = "REJECTED"
	campaign.AdGroupModeration.Status = "REJECTED"
	campaign.AdModeration.Status = "REJECTED"
	for index := range campaign.KeywordModeration {
		campaign.KeywordModeration[index].Moderation.Status = "REJECTED"
	}
	if err := directCampaignGraphEvidenceReady(campaign); err != nil {
		t.Fatalf("rejected verified graph cannot be edited: %v", err)
	}
	if err := directCampaignGraphLaunchReady(
		campaign,
	); !errors.Is(err, ErrDirectModerationNotReady) {
		t.Fatalf("rejected graph launch error = %v, want moderation not ready", err)
	}
}

func TestDirectLaunchReadinessRequiresEveryAcceptedChild(t *testing.T) {
	t.Parallel()
	campaign := readyDirectGraphTestCampaign()
	if err := directCampaignGraphLaunchReady(campaign); err != nil {
		t.Fatalf("complete accepted graph was rejected: %v", err)
	}
	campaign.KeywordModeration[0].Moderation.Status = "MODERATION"
	if err := directCampaignGraphLaunchReady(
		campaign,
	); !errors.Is(err, ErrDirectModerationNotReady) {
		t.Fatalf("partially moderated graph error = %v, want moderation not ready", err)
	}
}

func TestDirectAuthoritativeModerationIncludesResponsiveTextChildren(t *testing.T) {
	t.Parallel()
	status, clarification, err := authoritativeDirectModeration(
		"REJECTED", "title rejected", "ACCEPTED", "",
	)
	if err != nil || status != "REJECTED" || clarification != "title rejected" {
		t.Fatalf(
			"authoritative moderation = %q, %q, %v",
			status, clarification, err,
		)
	}
	if _, _, err := authoritativeDirectModeration(
		"ACCEPTED", "", "MODERATION", "",
	); !errors.Is(err, ErrDirectValidation) {
		t.Fatalf("premature accepted aggregate error = %v", err)
	}
	if _, _, err := authoritativeDirectModeration(
		"MODERATION", "", "REJECTED", "",
	); !errors.Is(err, ErrDirectValidation) {
		t.Fatalf("hidden rejection aggregate error = %v", err)
	}
}

func validDirectGraphTestCampaign() DirectCampaign {
	return DirectCampaign{
		Name: "Кампания", Objective: "traffic",
		LandingURL: "https://maxposty.ru/", Brief: "Тест полного графа",
		Regions: []string{"225"}, Titles: []string{"Заголовок"},
		Texts: []string{"Текст объявления"}, Keywords: []string{"ведение канала"},
		NegativeKeywords: []string{}, WeeklyBudgetMinor: 30_000,
		CurrencyCode: "RUB",
		StartsAt:     time.Date(2042, time.January, 1, 0, 0, 0, 0, time.UTC),
		EndsAt:       time.Date(2042, time.February, 1, 0, 0, 0, 0, time.UTC),
	}
}

func readyDirectGraphTestCampaign() DirectCampaign {
	campaign := validDirectGraphTestCampaign()
	campaignID, adGroupID, adID, keywordID := int64(101), int64(102), int64(103), int64(104)
	verifiedAt := time.Date(2042, time.January, 2, 0, 0, 0, 0, time.UTC)
	campaign.Status = "accepted"
	campaign.ProviderStatus = "ACCEPTED"
	campaign.ProviderState = "OFF"
	campaign.ProviderCampaignID = &campaignID
	campaign.ProviderAdGroupID = &adGroupID
	campaign.ProviderAdID = &adID
	campaign.ProviderKeywordIDs = []int64{keywordID}
	campaign.ProviderKeywordMappings = []DirectKeywordMapping{{
		Keyword: campaign.Keywords[0], ProviderKeywordID: keywordID,
		Moderation: DirectModerationSnapshot{Status: "ACCEPTED"},
	}}
	campaign.ProviderGraphHash = strings.Repeat("a", 64)
	campaign.ProviderRevisionID = "drev_" + strings.Repeat("b", 32)
	campaign.GraphVerifiedAt = &verifiedAt
	campaign.ModerationStatus = "ACCEPTED"
	campaign.CampaignModeration = DirectModerationSnapshot{Status: "ACCEPTED"}
	campaign.AdGroupModeration = DirectModerationSnapshot{Status: "ACCEPTED"}
	campaign.AdModeration = DirectModerationSnapshot{Status: "ACCEPTED"}
	campaign.KeywordModeration = []DirectKeywordMapping{{
		Keyword: campaign.Keywords[0], ProviderKeywordID: keywordID,
		Moderation: DirectModerationSnapshot{Status: "ACCEPTED"},
	}}
	return campaign
}
