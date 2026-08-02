package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestDirectExternalCampaignSnapshotReplacement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connection := connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	if _, err := storage.db.ExecContext(ctx, `UPDATE direct_connections
SET read_only=TRUE WHERE workspace_id=$1 AND id=$2`, workspace.ID, connection.ID); err != nil {
		t.Fatal(err)
	}

	firstSync := time.Date(2042, time.June, 1, 12, 0, 0, 0, time.UTC)
	firstEnd := time.Date(2042, time.July, 9, 18, 30, 0, 0, time.UTC)
	firstItems := []DirectExternalCampaign{
		{
			ProviderCampaignID:    101,
			Name:                  "  First campaign  ",
			CampaignType:          "text_campaign",
			ProviderStatus:        "accepted",
			ProviderState:         "on",
			ProviderStatusPayment: "allowed",
			StartsAt:              time.Date(2042, time.June, 8, 15, 0, 0, 0, time.UTC),
			Timezone:              " Europe/Moscow ",
		},
		{
			ProviderCampaignID:    102,
			Name:                  "Second campaign",
			CampaignType:          "UNIFIED_CAMPAIGN",
			ProviderStatus:        "DRAFT",
			ProviderState:         "OFF",
			ProviderStatusPayment: "DISALLOWED",
			StartsAt:              time.Date(2042, time.July, 2, 9, 0, 0, 0, time.UTC),
			EndsAt:                &firstEnd,
			Timezone:              "Europe/Moscow",
		},
	}
	firstGeneration := claimDirectExternalTestSync(
		t, ctx, storage, owner, workspace.ID, connection.ID, firstSync,
	)
	if err := storage.ReplaceDirectExternalCampaigns(
		ctx, owner, workspace.ID, connection.ID, firstGeneration, firstItems, firstSync,
	); err != nil {
		t.Fatal(err)
	}
	firstSnapshot, err := storage.GetDirectExternalCampaignSnapshot(
		ctx, owner, workspace.ID, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if firstSnapshot.ConnectionID != connection.ID || firstSnapshot.SyncedAt == nil ||
		!firstSnapshot.SyncedAt.Equal(firstSync) || len(firstSnapshot.Items) != 2 {
		t.Fatalf("atomic external campaign snapshot = %#v", firstSnapshot)
	}

	listed, err := storage.ListDirectExternalCampaigns(
		ctx, owner, workspace.ID, connection.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].ProviderCampaignID != 102 || listed[1].ProviderCampaignID != 101 {
		t.Fatalf("external campaigns = %#v", listed)
	}
	first := listed[1]
	if first.WorkspaceID != workspace.ID || first.ConnectionID != connection.ID ||
		first.Name != "First campaign" || first.CampaignType != "TEXT_CAMPAIGN" ||
		first.ProviderStatus != "ACCEPTED" || first.ProviderState != "ON" ||
		first.ProviderStatusPayment != "ALLOWED" || first.Timezone != "Europe/Moscow" ||
		first.EndsAt != nil || !first.StartsAt.Equal(time.Date(2042, time.June, 8, 0, 0, 0, 0, time.UTC)) ||
		!first.SyncedAt.Equal(firstSync) {
		t.Fatalf("normalized external campaign = %#v", first)
	}
	second := listed[0]
	if second.EndsAt == nil || !second.EndsAt.Equal(time.Date(2042, time.July, 9, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("nullable ends_at was not preserved: %#v", second.EndsAt)
	}

	invalidSync := firstSync.Add(time.Minute)
	invalidGeneration := claimDirectExternalTestSync(
		t, ctx, storage, owner, workspace.ID, connection.ID, invalidSync,
	)
	duplicate := append([]DirectExternalCampaign(nil), firstItems[0], firstItems[0])
	if err := storage.ReplaceDirectExternalCampaigns(
		ctx, owner, workspace.ID, connection.ID, invalidGeneration, duplicate, invalidSync,
	); !errors.Is(err, ErrDirectValidation) {
		t.Fatalf("duplicate snapshot error = %v, want ErrDirectValidation", err)
	}
	if err := storage.ReleaseDirectExternalCampaignSync(
		ctx, owner, workspace.ID, connection.ID, invalidGeneration,
	); err != nil {
		t.Fatalf("release invalid snapshot claim: %v", err)
	}
	listed, err = storage.ListDirectExternalCampaigns(ctx, owner, workspace.ID, connection.ID)
	if err != nil || len(listed) != 2 {
		t.Fatalf("invalid replacement changed snapshot: %#v, %v", listed, err)
	}

	secondSync := firstSync.Add(2 * time.Minute)
	secondGeneration := claimDirectExternalTestSync(
		t, ctx, storage, owner, workspace.ID, connection.ID, secondSync,
	)
	if err := storage.ReplaceDirectExternalCampaigns(
		ctx, owner, workspace.ID, connection.ID, secondGeneration,
		[]DirectExternalCampaign{externalCampaignTestItem(103, "Replacement", secondSync)},
		secondSync,
	); err != nil {
		t.Fatal(err)
	}
	listed, err = storage.ListDirectExternalCampaigns(ctx, owner, workspace.ID, connection.ID)
	if err != nil || len(listed) != 1 || listed[0].ProviderCampaignID != 103 {
		t.Fatalf("replacement snapshot = %#v, %v", listed, err)
	}
	if err := storage.ReplaceDirectExternalCampaigns(
		ctx, owner, workspace.ID, connection.ID, firstGeneration, nil, secondSync.Add(time.Minute),
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale snapshot error = %v, want ErrConflict", err)
	}
	listed, err = storage.ListDirectExternalCampaigns(ctx, owner, workspace.ID, connection.ID)
	if err != nil || len(listed) != 1 || listed[0].ProviderCampaignID != 103 {
		t.Fatalf("stale generation changed snapshot: %#v, %v", listed, err)
	}
	connection, err = storage.GetDirectConnection(ctx, owner, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if connection.ExternalCampaignsSyncedAt == nil ||
		!connection.ExternalCampaignsSyncedAt.Equal(secondSync) {
		t.Fatalf("connection external campaigns synced_at = %#v", connection.ExternalCampaignsSyncedAt)
	}

	outsider := "direct-outsider-" + newStoreID("")
	if err := storage.UpsertUser(ctx, User{ID: outsider, DisplayName: outsider}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ListDirectExternalCampaigns(
		ctx, outsider, workspace.ID, connection.ID,
	); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider list error = %v, want ErrNotFound", err)
	}
	if err := storage.ReplaceDirectExternalCampaigns(
		ctx, outsider, workspace.ID, connection.ID, secondGeneration, nil, secondSync.Add(time.Minute),
	); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider replace error = %v, want ErrNotFound", err)
	}

	events, err := storage.ListAuditEvents(ctx, owner, workspace.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	externalSyncEvents := 0
	for _, event := range events {
		if event.Action != "direct.campaigns.external_synced" {
			continue
		}
		externalSyncEvents++
		if event.EntityType != "direct_connection" || event.EntityID != connection.ID {
			t.Fatalf("external sync audit scope = %#v", event)
		}
		var metadata map[string]any
		if err := json.Unmarshal(event.Metadata, &metadata); err != nil {
			t.Fatal(err)
		}
		if _, ok := metadata["stored_count"]; !ok {
			t.Fatalf("external sync audit metadata = %#v", metadata)
		}
	}
	if externalSyncEvents != 2 {
		t.Fatalf("external sync audit events = %d, want 2", externalSyncEvents)
	}
}

func TestDirectExternalCampaignSyncClaimLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connection := connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2042, time.July, 1, 12, 0, 0, 0, time.UTC)
	cooldown := time.Minute
	lease := 2 * time.Minute

	firstGeneration, err := storage.ClaimDirectExternalCampaignSync(
		ctx, owner, workspace.ID, connection.ID, now, cooldown, lease,
	)
	if err != nil || firstGeneration != 1 {
		t.Fatalf("first generation = %d, %v", firstGeneration, err)
	}
	if _, err := storage.ClaimDirectExternalCampaignSync(
		ctx, owner, workspace.ID, connection.ID, now.Add(time.Second), cooldown, lease,
	); !errors.Is(err, ErrDirectProviderOperationBusy) {
		t.Fatalf("concurrent claim error = %v, want busy", err)
	}
	if err := storage.ReleaseDirectExternalCampaignSync(
		ctx, owner, workspace.ID, connection.ID, firstGeneration,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ClaimDirectExternalCampaignSync(
		ctx, owner, workspace.ID, connection.ID, now.Add(30*time.Second), cooldown, lease,
	); !errors.Is(err, ErrDirectExternalSyncCooldown) {
		t.Fatalf("claim after failed release error = %v, want cooldown", err)
	}

	secondStartedAt := now.Add(cooldown)
	secondGeneration, err := storage.ClaimDirectExternalCampaignSync(
		ctx, owner, workspace.ID, connection.ID, secondStartedAt, cooldown, lease,
	)
	if err != nil || secondGeneration != firstGeneration+1 {
		t.Fatalf("second generation = %d, %v", secondGeneration, err)
	}
	if _, err := storage.ClaimDirectExternalCampaignSync(
		ctx, owner, workspace.ID, connection.ID, secondStartedAt.Add(lease-time.Second), cooldown, lease,
	); !errors.Is(err, ErrDirectProviderOperationBusy) {
		t.Fatalf("claim within lease error = %v, want busy", err)
	}

	thirdStartedAt := secondStartedAt.Add(lease)
	thirdGeneration, err := storage.ClaimDirectExternalCampaignSync(
		ctx, owner, workspace.ID, connection.ID, thirdStartedAt, cooldown, lease,
	)
	if err != nil || thirdGeneration != secondGeneration+1 {
		t.Fatalf("lease recovery generation = %d, %v", thirdGeneration, err)
	}
	if err := storage.ReleaseDirectExternalCampaignSync(
		ctx, owner, workspace.ID, connection.ID, secondGeneration,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale release error = %v, want ErrConflict", err)
	}

	completedAt := thirdStartedAt.Add(time.Second)
	if err := storage.ReplaceDirectExternalCampaigns(
		ctx, owner, workspace.ID, connection.ID, thirdGeneration,
		[]DirectExternalCampaign{externalCampaignTestItem(303, "Fresh generation", completedAt)},
		completedAt,
	); err != nil {
		t.Fatal(err)
	}
	if err := storage.ReplaceDirectExternalCampaigns(
		ctx, owner, workspace.ID, connection.ID, secondGeneration,
		[]DirectExternalCampaign{externalCampaignTestItem(202, "Stale generation", completedAt)},
		completedAt.Add(time.Second),
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale completion error = %v, want ErrConflict", err)
	}
	if err := storage.ReleaseDirectExternalCampaignSync(
		ctx, owner, workspace.ID, connection.ID, thirdGeneration,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("completed claim release error = %v, want ErrConflict", err)
	}
	listed, err := storage.ListDirectExternalCampaigns(ctx, owner, workspace.ID, connection.ID)
	if err != nil || len(listed) != 1 || listed[0].ProviderCampaignID != 303 {
		t.Fatalf("snapshot after stale completion = %#v, %v", listed, err)
	}
	if _, err := storage.ClaimDirectExternalCampaignSync(
		ctx, owner, workspace.ID, connection.ID, thirdStartedAt.Add(30*time.Second), cooldown, lease,
	); !errors.Is(err, ErrDirectExternalSyncCooldown) {
		t.Fatalf("claim after success error = %v, want cooldown", err)
	}
}

func TestDirectExternalCampaignSyncConstraintRejectsNullGeneration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connection := connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)

	_, err := storage.db.ExecContext(ctx, `UPDATE direct_connections
SET external_campaigns_sync_started_at=$1,
    external_campaigns_sync_generation=NULL
WHERE workspace_id=$2 AND id=$3`, time.Now().UTC(), workspace.ID, connection.ID)
	if err == nil {
		t.Fatal("sync state constraint accepted started sync with NULL generation")
	}
}

func TestDirectExternalCampaignSnapshotsArePurgedOnReconnectAndRevoke(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	oldConnection := connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2042, time.July, 3, 12, 0, 0, 0, time.UTC)
	oldGeneration := claimDirectExternalTestSync(
		t, ctx, storage, owner, workspace.ID, oldConnection.ID, now,
	)
	if err := storage.ReplaceDirectExternalCampaigns(
		ctx, owner, workspace.ID, oldConnection.ID, oldGeneration,
		[]DirectExternalCampaign{externalCampaignTestItem(401, "Old account", now)},
		now,
	); err != nil {
		t.Fatal(err)
	}

	newConnection := connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	assertDirectExternalTestRowCount(t, ctx, storage, workspace.ID, oldConnection.ID, 0)
	newStartedAt := now.Add(time.Minute)
	newGeneration := claimDirectExternalTestSync(
		t, ctx, storage, owner, workspace.ID, newConnection.ID, newStartedAt,
	)
	if err := storage.ReplaceDirectExternalCampaigns(
		ctx, owner, workspace.ID, newConnection.ID, newGeneration,
		[]DirectExternalCampaign{externalCampaignTestItem(402, "Current account", newStartedAt)},
		newStartedAt,
	); err != nil {
		t.Fatal(err)
	}
	if err := storage.RevokeDirectConnection(
		ctx, owner, workspace.ID, newStartedAt.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	assertDirectExternalTestRowCount(t, ctx, storage, workspace.ID, newConnection.ID, 0)
}

func TestDirectExternalCampaignSyncClaimIsExclusive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connection := connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2042, time.July, 2, 12, 0, 0, 0, time.UTC)
	type claimResult struct {
		generation int64
		err        error
	}
	start := make(chan struct{})
	results := make(chan claimResult, 2)
	for range 2 {
		go func() {
			<-start
			generation, err := storage.ClaimDirectExternalCampaignSync(
				ctx, owner, workspace.ID, connection.ID, now, 0, time.Minute,
			)
			results <- claimResult{generation: generation, err: err}
		}()
	}
	close(start)
	successes, busy := 0, 0
	var claimedGeneration int64
	for range 2 {
		result := <-results
		switch {
		case result.err == nil:
			successes++
			claimedGeneration = result.generation
		case errors.Is(result.err, ErrDirectProviderOperationBusy):
			busy++
		default:
			t.Fatalf("concurrent claim error = %v", result.err)
		}
	}
	if successes != 1 || busy != 1 || claimedGeneration != 1 {
		t.Fatalf(
			"concurrent claims: successes=%d busy=%d generation=%d",
			successes, busy, claimedGeneration,
		)
	}
}

func TestDirectExternalCampaignsExcludeManagedAcrossConnections(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	oldConnection := connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2042, time.August, 1, 12, 0, 0, 0, time.UTC)
	managedBeforeReconnect := createDirectTestCampaign(t, ctx, storage, owner, workspace.ID, now)
	managedAfterSync := createDirectTestCampaign(t, ctx, storage, owner, workspace.ID, now.Add(time.Second))

	oldGeneration := claimDirectExternalTestSync(
		t, ctx, storage, owner, workspace.ID, oldConnection.ID, now,
	)
	if err := storage.ReplaceDirectExternalCampaigns(
		ctx, owner, workspace.ID, oldConnection.ID, oldGeneration,
		[]DirectExternalCampaign{
			externalCampaignTestItem(501, "Managed", now),
			externalCampaignTestItem(502, "Managed later", now),
		},
		now,
	); err != nil {
		t.Fatal(err)
	}
	setDirectTestProviderCampaignID(
		t, ctx, storage, workspace.ID, managedBeforeReconnect.ID, 501,
	)
	listed, err := storage.ListDirectExternalCampaigns(
		ctx, owner, workspace.ID, oldConnection.ID,
	)
	if err != nil || len(listed) != 1 || listed[0].ProviderCampaignID != 502 {
		t.Fatalf("same-connection managed filter = %#v, %v", listed, err)
	}

	revokedAt := now.Add(time.Minute)
	if _, err := storage.db.ExecContext(ctx, `UPDATE direct_connections
SET status='revoked',token_ciphertext='',token_refresh_claimed_at=NULL,
    revoked_at=$1,updated_at=$1,error_code=''
WHERE workspace_id=$2 AND id=$3`, revokedAt, workspace.ID, oldConnection.ID); err != nil {
		t.Fatal(err)
	}
	listed, err = storage.ListDirectExternalCampaigns(ctx, owner, workspace.ID, oldConnection.ID)
	if !errors.Is(err, ErrDirectConnectionRequired) || listed != nil {
		t.Fatalf("revoked connection snapshot result: %#v, %v", listed, err)
	}

	newConnection := connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	secondSync := now.Add(2 * time.Minute)
	newGeneration := claimDirectExternalTestSync(
		t, ctx, storage, owner, workspace.ID, newConnection.ID, secondSync,
	)
	if err := storage.ReplaceDirectExternalCampaigns(
		ctx, owner, workspace.ID, newConnection.ID, newGeneration,
		[]DirectExternalCampaign{
			externalCampaignTestItem(501, "Managed through old connection", secondSync),
			externalCampaignTestItem(502, "Will become managed", secondSync),
			externalCampaignTestItem(503, "External", secondSync),
		},
		secondSync,
	); err != nil {
		t.Fatal(err)
	}
	listed, err = storage.ListDirectExternalCampaigns(ctx, owner, workspace.ID, newConnection.ID)
	if err != nil || len(listed) != 2 ||
		listed[0].ProviderCampaignID != 503 || listed[1].ProviderCampaignID != 502 {
		t.Fatalf("cross-connection managed filter = %#v, %v", listed, err)
	}
	var excludedStored int
	if err := storage.db.QueryRowContext(ctx, `SELECT COUNT(*)
FROM direct_external_campaigns
WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=501`,
		workspace.ID, newConnection.ID).Scan(&excludedStored); err != nil {
		t.Fatal(err)
	}
	if excludedStored != 0 {
		t.Fatalf("managed provider ID was stored for new connection: %d", excludedStored)
	}

	// The local association can appear after the snapshot and can still point
	// at the former connection. List must filter it dynamically workspace-wide.
	setDirectTestProviderCampaignID(t, ctx, storage, workspace.ID, managedAfterSync.ID, 502)
	listed, err = storage.ListDirectExternalCampaigns(ctx, owner, workspace.ID, newConnection.ID)
	if err != nil || len(listed) != 1 || listed[0].ProviderCampaignID != 503 {
		t.Fatalf("dynamic cross-connection managed filter = %#v, %v", listed, err)
	}
	var dynamicallyFilteredStored int
	if err := storage.db.QueryRowContext(ctx, `SELECT COUNT(*)
FROM direct_external_campaigns
WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=502`,
		workspace.ID, newConnection.ID).Scan(&dynamicallyFilteredStored); err != nil {
		t.Fatal(err)
	}
	if dynamicallyFilteredStored != 1 {
		t.Fatalf("dynamic filter unexpectedly rewrote snapshot: %d", dynamicallyFilteredStored)
	}
}

func externalCampaignTestItem(
	providerCampaignID int64, name string, startsAt time.Time,
) DirectExternalCampaign {
	return DirectExternalCampaign{
		ProviderCampaignID:    providerCampaignID,
		Name:                  name,
		CampaignType:          "TEXT_CAMPAIGN",
		ProviderStatus:        "ACCEPTED",
		ProviderState:         "ON",
		ProviderStatusPayment: "ALLOWED",
		StartsAt:              startsAt,
		Timezone:              "Europe/Moscow",
	}
}

func claimDirectExternalTestSync(
	t *testing.T,
	ctx context.Context,
	storage *Store,
	actorUserID, workspaceID, connectionID string,
	now time.Time,
) int64 {
	t.Helper()
	generation, err := storage.ClaimDirectExternalCampaignSync(
		ctx, actorUserID, workspaceID, connectionID, now, 0, time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	return generation
}

func setDirectTestProviderCampaignID(
	t *testing.T,
	ctx context.Context,
	storage *Store,
	workspaceID, campaignID string,
	providerCampaignID int64,
) {
	t.Helper()
	if _, err := storage.db.ExecContext(ctx, `UPDATE direct_campaigns
SET provider_campaign_id=$1,status='provider_draft',provider_status='DRAFT',provider_state='OFF'
WHERE workspace_id=$2 AND id=$3`, providerCampaignID, workspaceID, campaignID); err != nil {
		t.Fatal(err)
	}
}

func assertDirectExternalTestRowCount(
	t *testing.T,
	ctx context.Context,
	storage *Store,
	workspaceID, connectionID string,
	want int,
) {
	t.Helper()
	var count int
	if err := storage.db.QueryRowContext(ctx, `SELECT COUNT(*)
FROM direct_external_campaigns
WHERE workspace_id=$1 AND connection_id=$2`, workspaceID, connectionID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("external campaign row count = %d, want %d", count, want)
	}
}
