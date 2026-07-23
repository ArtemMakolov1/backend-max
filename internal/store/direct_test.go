package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDirectLaunchClaimIsRecoverableAndPreservesCredential(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connection := connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2042, time.March, 4, 12, 0, 0, 0, time.UTC)
	campaign := createDirectTestCampaign(t, ctx, storage, owner, workspace.ID, now)
	if _, err := storage.GrantDirectAutoLaunchConsent(
		ctx, owner, workspace.ID, campaign.ID, DirectConsentRequest{
			Confirmation: "АВТОЗАПУСК", ExpectedVersion: campaign.Version,
			ExpectedConnectionID: connection.ID, ExpectedAccountID: connection.AccountID,
			ExpectedCampaignName: campaign.Name, ExpectedProviderID: 77_001,
			WeeklyBudgetMinor: campaign.WeeklyBudgetMinor,
			StartsAt:          campaign.StartsAt, EndsAt: campaign.EndsAt, AuthorizedAt: now,
		},
	); !errors.Is(err, ErrDirectConsentMismatch) {
		t.Fatalf("draft auto-launch consent error = %v, want ErrDirectConsentMismatch", err)
	}
	campaign = acceptDirectTestCampaign(t, ctx, storage, owner, workspace.ID, campaign, now)
	consent, err := storage.GrantDirectAutoLaunchConsent(
		ctx, owner, workspace.ID, campaign.ID, DirectConsentRequest{
			Confirmation:         "АВТОЗАПУСК",
			ExpectedVersion:      campaign.Version,
			ExpectedConnectionID: connection.ID,
			ExpectedAccountID:    connection.AccountID,
			ExpectedCampaignName: campaign.Name,
			ExpectedProviderID:   *campaign.ProviderCampaignID,
			ExpectedGraphHash:    campaign.ProviderGraphHash,
			ExpectedRevisionID:   campaign.ProviderRevisionID,
			WeeklyBudgetMinor:    campaign.WeeklyBudgetMinor,
			StartsAt:             campaign.StartsAt,
			EndsAt:               campaign.EndsAt,
			AuthorizedAt:         now,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if campaign.Version != consent.CampaignVersion {
		t.Fatalf("provider lifecycle changed config version: got %d want %d", campaign.Version, consent.CampaignVersion)
	}
	if consent.CampaignName != campaign.Name || consent.ProviderCampaignID == nil ||
		*consent.ProviderCampaignID != *campaign.ProviderCampaignID {
		t.Fatalf("consent did not snapshot the visible provider identity: %#v", consent)
	}
	if !campaign.AutoLaunch.Valid {
		campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
		if err != nil || !campaign.AutoLaunch.Valid {
			t.Fatalf("accepted campaign consent is invalid: %#v, %v", campaign.AutoLaunch, err)
		}
	}
	if campaign.AutoLaunch.CampaignID != campaign.ID ||
		campaign.AutoLaunch.CampaignName != campaign.Name ||
		campaign.AutoLaunch.ProviderCampaignID != "77001" ||
		campaign.AutoLaunch.WarningCode != "" {
		t.Fatalf("public consent summary is incomplete: %#v", campaign.AutoLaunch)
	}

	claimed, err := storage.ClaimDirectAutoCampaignLaunch(
		ctx, workspace.ID, campaign.ID, campaign.Version, *campaign.ProviderCampaignID,
		connection.AccountID, campaign.WeeklyBudgetMinor, campaign.StartsAt, campaign.EndsAt,
		campaign.ProviderGraphHash, campaign.ProviderRevisionID,
		now.Add(time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Campaign.LaunchState != "launching" || claimed.Campaign.LaunchMode != "auto" ||
		claimed.Consent.ConsumedAt == nil {
		t.Fatalf("claim is not durably recoverable: %#v", claimed)
	}
	if err := storage.RevokeDirectConnection(ctx, owner, workspace.ID, now.Add(2*time.Minute)); !errors.Is(err, ErrConflict) {
		t.Fatalf("revoke during reconciliation error = %v, want ErrConflict", err)
	}
	if _, err := storage.ReplaceDirectConnection(ctx, owner, workspace.ID, DirectConnection{
		AccountID: "replacement", CurrencyCode: "RUB", Timezone: "Europe/Moscow",
		TokenCiphertext: "v1.replacement", TokenKeyVersion: 1, CreatedAt: now.Add(2 * time.Minute),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("replace during reconciliation error = %v, want ErrConflict", err)
	}
	if err := storage.MarkDirectCampaignLaunchAttempt(
		ctx, workspace.ID, campaign.ID, *claimed.Campaign.LaunchClaimedAt,
		now.Add(3*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if err := storage.MarkDirectCampaignLaunchReconciling(
		ctx, workspace.ID, campaign.ID, *claimed.Campaign.LaunchClaimedAt,
		"provider_timeout", now.Add(4*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	candidates, err := storage.ClaimDirectLaunchRecoveryCandidates(
		ctx, now.Add(4*time.Minute), 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].WorkspaceID != workspace.ID ||
		candidates[0].CampaignID != campaign.ID {
		t.Fatalf("recovery candidates = %#v", candidates)
	}
	recoveryMaterial, err := storage.GetDirectLaunchRecoveryMaterial(
		ctx, workspace.ID, campaign.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.CompleteDirectCampaignLaunch(
		ctx, workspace.ID, campaign.ID,
		*recoveryMaterial.Campaign.LaunchClaimedAt, now.Add(5*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if campaign.Status != "active" || campaign.LaunchState != "confirmed" ||
		campaign.ProviderStatus != "ACCEPTED" || campaign.ProviderState != "ON" ||
		campaign.LaunchedAt == nil || campaign.Version != consent.CampaignVersion {
		t.Fatalf("completed campaign = %#v", campaign)
	}
	candidates, err = storage.ClaimDirectLaunchRecoveryCandidates(
		ctx, now.Add(6*time.Minute), 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("completed launch remains recoverable: %#v", candidates)
	}
}

func TestDirectManualLaunchIsWorkspaceScopedAndDoesNotRequireAutoConsent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connection := connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2042, time.April, 5, 12, 0, 0, 0, time.UTC)
	campaign := createDirectTestCampaign(t, ctx, storage, owner, workspace.ID, now)
	campaign = acceptDirectTestCampaign(t, ctx, storage, owner, workspace.ID, campaign, now)
	if _, err := storage.ReplaceDirectConnection(
		ctx, owner, workspace.ID, DirectConnection{
			AccountID: "replacement", CurrencyCode: "RUB", Timezone: "Europe/Moscow",
			TokenCiphertext: "v1.replacement", TokenKeyVersion: 1,
			CreatedAt: now.Add(time.Minute),
		},
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("replace with accepted provider campaign = %v, want ErrConflict", err)
	}
	otherWorkspace, err := storage.CreateWorkspace(ctx, owner, Workspace{Name: "Other team"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := storage.GetDirectManualLaunchMaterial(
		ctx, owner, otherWorkspace.ID, campaign.ID,
	); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace material error = %v, want ErrNotFound", err)
	}
	claimed, err := storage.ClaimDirectManualCampaignLaunch(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version, *campaign.ProviderCampaignID,
		connection.AccountID, campaign.WeeklyBudgetMinor, campaign.StartsAt, campaign.EndsAt,
		campaign.ProviderGraphHash, campaign.ProviderRevisionID,
		now.Add(time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Campaign.LaunchMode != "manual" || claimed.Consent.ID != "" {
		t.Fatalf("manual claim unexpectedly depends on auto consent: %#v", claimed)
	}
}

func TestDirectRevokeAllowsActiveCampaignAndReconnectsExplicitly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connection := connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2042, time.April, 7, 12, 0, 0, 0, time.UTC)
	campaign := createDirectTestCampaign(t, ctx, storage, owner, workspace.ID, now)
	campaign = acceptDirectTestCampaign(t, ctx, storage, owner, workspace.ID, campaign, now)
	if _, err := storage.GrantDirectAutoLaunchConsent(
		ctx, owner, workspace.ID, campaign.ID, DirectConsentRequest{
			Confirmation:         "АВТОЗАПУСК",
			ExpectedVersion:      campaign.Version,
			ExpectedConnectionID: connection.ID,
			ExpectedAccountID:    connection.AccountID,
			ExpectedCampaignName: campaign.Name,
			ExpectedProviderID:   *campaign.ProviderCampaignID,
			ExpectedGraphHash:    campaign.ProviderGraphHash,
			ExpectedRevisionID:   campaign.ProviderRevisionID,
			WeeklyBudgetMinor:    campaign.WeeklyBudgetMinor,
			StartsAt:             campaign.StartsAt,
			EndsAt:               campaign.EndsAt,
			AuthorizedAt:         now,
		},
	); err != nil {
		t.Fatal(err)
	}
	campaign, err := storage.SyncDirectCampaignProviderStatus(
		ctx, workspace.ID, campaign.ID, *campaign.ProviderCampaignID,
		"ACCEPTED", "ON", now.Add(time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if campaign.Status != "active" {
		t.Fatalf("provider campaign status = %q, want active", campaign.Status)
	}

	if err := storage.RevokeDirectConnection(
		ctx, owner, workspace.ID, now.Add(2*time.Minute),
	); err != nil {
		t.Fatalf("revoke with a non-ambiguous active campaign: %v", err)
	}
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if campaign.Status != "active" || campaign.AutoLaunch.Valid ||
		campaign.AutoLaunch.InvalidReason != "connection_revoked" {
		t.Fatalf("revoke obscured provider truth or retained consent: %#v", campaign)
	}
	reconnected, err := storage.ReplaceDirectConnection(
		ctx, owner, workspace.ID, DirectConnection{
			AccountID: "replacement", CurrencyCode: "RUB", Timezone: "Europe/Moscow",
			TokenCiphertext: "v1.replacement", TokenKeyVersion: 1,
			CreatedAt: now.Add(3 * time.Minute),
		},
	)
	if err != nil {
		t.Fatalf("reconnect after explicit revoke: %v", err)
	}
	if reconnected.ID == connection.ID || reconnected.AccountID != "replacement" {
		t.Fatalf("reconnected account = %#v", reconnected)
	}
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if campaign.ConnectionID != connection.ID || campaign.Status != "active" {
		t.Fatalf("old provider campaign was silently rebound: %#v", campaign)
	}
}

func TestDirectErrorConnectionCanBeRevokedAndReauthorized(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connection := connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2042, time.April, 8, 12, 0, 0, 0, time.UTC)
	campaign := createDirectTestCampaign(t, ctx, storage, owner, workspace.ID, now)
	campaign = acceptDirectTestCampaign(t, ctx, storage, owner, workspace.ID, campaign, now)
	if _, err := storage.GrantDirectAutoLaunchConsent(
		ctx, owner, workspace.ID, campaign.ID, DirectConsentRequest{
			Confirmation: "АВТОЗАПУСК", ExpectedVersion: campaign.Version,
			ExpectedConnectionID: connection.ID, ExpectedAccountID: connection.AccountID,
			ExpectedCampaignName: campaign.Name, ExpectedProviderID: *campaign.ProviderCampaignID,
			ExpectedGraphHash: campaign.ProviderGraphHash, ExpectedRevisionID: campaign.ProviderRevisionID,
			WeeklyBudgetMinor: campaign.WeeklyBudgetMinor,
			StartsAt:          campaign.StartsAt, EndsAt: campaign.EndsAt, AuthorizedAt: now,
		},
	); err != nil {
		t.Fatal(err)
	}
	if err := storage.MarkDirectConnectionAuthorizationRequired(
		ctx, workspace.ID, connection.ID, now.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	connection, err := storage.GetDirectConnection(ctx, owner, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if connection.Status != "error" || connection.ErrorCode != "authorization_required" {
		t.Fatalf("authorization failure connection = %#v", connection)
	}
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if campaign.AutoLaunch.Valid ||
		campaign.AutoLaunch.InvalidReason != "connection_authorization_required" {
		t.Fatalf("authorization failure retained auto-launch consent: %#v", campaign.AutoLaunch)
	}
	if err := storage.RevokeDirectConnection(ctx, owner, workspace.ID, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("revoke error connection: %v", err)
	}
	reconnected, err := storage.ReplaceDirectConnection(
		ctx, owner, workspace.ID, DirectConnection{
			AccountID: "reauthorized", CurrencyCode: "RUB", Timezone: "Europe/Moscow",
			TokenCiphertext: "v1.reauthorized", TokenKeyVersion: 1,
			CreatedAt: now.Add(3 * time.Minute),
		},
	)
	if err != nil {
		t.Fatalf("reauthorize after error: %v", err)
	}
	if reconnected.Status != "active" || reconnected.AccountID != "reauthorized" {
		t.Fatalf("reauthorized connection = %#v", reconnected)
	}
}

func TestDirectAuthoritativeUnauthorizedLaunchCanBeDisconnected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connection := connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2042, time.April, 9, 12, 0, 0, 0, time.UTC)
	campaign := createDirectTestCampaign(t, ctx, storage, owner, workspace.ID, now)
	campaign = acceptDirectTestCampaign(t, ctx, storage, owner, workspace.ID, campaign, now)
	claimed, err := storage.ClaimDirectManualCampaignLaunch(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version, *campaign.ProviderCampaignID,
		connection.AccountID, campaign.WeeklyBudgetMinor, campaign.StartsAt, campaign.EndsAt,
		campaign.ProviderGraphHash, campaign.ProviderRevisionID,
		now.Add(time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.MarkDirectCampaignLaunchAttempt(
		ctx, workspace.ID, campaign.ID, *claimed.Campaign.LaunchClaimedAt,
		now.Add(2*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if err := storage.AbortDirectCampaignLaunchForAuthorization(
		ctx, workspace.ID, campaign.ID, *claimed.Campaign.LaunchClaimedAt,
		now.Add(3*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if campaign.Status != "accepted" || campaign.LaunchState != "idle" ||
		campaign.LaunchAttemptCount != 0 ||
		campaign.LaunchFailureCode != "authorization_required" {
		t.Fatalf("authoritatively rejected launch was not released: %#v", campaign)
	}
	if err := storage.MarkDirectConnectionAuthorizationRequired(
		ctx, workspace.ID, connection.ID, now.Add(3*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if err := storage.RevokeDirectConnection(
		ctx, owner, workspace.ID, now.Add(4*time.Minute),
	); err != nil {
		t.Fatalf("definitively rejected launch kept revoke blocked: %v", err)
	}
}

func TestDirectAutoLaunchConsentRequiresWritableActiveConnection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connection := connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2042, time.April, 6, 12, 0, 0, 0, time.UTC)
	campaign := createDirectTestCampaign(t, ctx, storage, owner, workspace.ID, now)
	campaign = acceptDirectTestCampaign(t, ctx, storage, owner, workspace.ID, campaign, now)
	request := DirectConsentRequest{
		Confirmation: "АВТОЗАПУСК", ExpectedVersion: campaign.Version,
		ExpectedConnectionID: connection.ID, ExpectedAccountID: connection.AccountID,
		ExpectedCampaignName: campaign.Name, ExpectedProviderID: *campaign.ProviderCampaignID,
		ExpectedGraphHash: campaign.ProviderGraphHash, ExpectedRevisionID: campaign.ProviderRevisionID,
		WeeklyBudgetMinor: campaign.WeeklyBudgetMinor,
		StartsAt:          campaign.StartsAt, EndsAt: campaign.EndsAt, AuthorizedAt: now,
	}
	if _, err := storage.db.ExecContext(ctx, `UPDATE direct_connections
SET read_only=TRUE WHERE workspace_id=$1 AND id=$2`, workspace.ID, connection.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GrantDirectAutoLaunchConsent(
		ctx, owner, workspace.ID, campaign.ID, request,
	); !errors.Is(err, ErrDirectConnectionRequired) {
		t.Fatalf("read-only consent error = %v, want ErrDirectConnectionRequired", err)
	}
	if _, err := storage.db.ExecContext(ctx, `UPDATE direct_connections
SET read_only=FALSE WHERE workspace_id=$1 AND id=$2`, workspace.ID, connection.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GrantDirectAutoLaunchConsent(
		ctx, owner, workspace.ID, campaign.ID, request,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.ExecContext(ctx, `UPDATE direct_connections
SET read_only=TRUE WHERE workspace_id=$1 AND id=$2`, workspace.ID, connection.ID); err != nil {
		t.Fatal(err)
	}
	campaign, err := storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if campaign.AutoLaunch.Valid {
		t.Fatalf("read-only connection left consent valid: %#v", campaign.AutoLaunch)
	}
	candidates, err := storage.ClaimDirectAutoLaunchCandidates(ctx, now.Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("read-only connection produced auto-launch candidates: %#v", candidates)
	}
}

func TestDirectSubmissionFailureWithoutProviderIDIsPersisted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2042, time.May, 6, 12, 0, 0, 0, time.UTC)
	campaign := createDirectTestCampaign(t, ctx, storage, owner, workspace.ID, now)
	if _, _, err := storage.ClaimDirectCampaignSubmission(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version, now.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if err := storage.FailDirectCampaignSubmission(
		ctx, workspace.ID, campaign.ID, "provider_unavailable", now.Add(2*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	campaign, err := storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if campaign.Status != "error" || campaign.ProviderCampaignID != nil ||
		campaign.LaunchFailureCode != "provider_unavailable" {
		t.Fatalf("failed submission = %#v", campaign)
	}
}

func TestPurgeExpiredDirectOAuthStatesIsBoundedAndKeepsLiveState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	now := time.Date(2042, time.June, 7, 12, 0, 0, 0, time.UTC)
	expiredStateHash := strings.Repeat("a", 64)
	liveStateHash := strings.Repeat("b", 64)
	for _, state := range []DirectOAuthState{
		{
			StateHash: expiredStateHash, PKCEVerifier: strings.Repeat("x", 43),
			CreatedAt: now.Add(-2 * time.Minute), ExpiresAt: now.Add(-time.Minute),
		},
		{
			StateHash: liveStateHash, PKCEVerifier: strings.Repeat("y", 43),
			CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		},
	} {
		if err := storage.CreateDirectOAuthState(
			ctx, owner, workspace.ID, state,
		); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := storage.PurgeExpiredDirectOAuthStates(ctx, now, 1)
	if err != nil || removed != 1 {
		t.Fatalf("purge removed=%d err=%v, want one", removed, err)
	}
	if _, err := storage.ConsumeDirectOAuthState(
		ctx, owner, expiredStateHash, now,
	); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired state remained: %v", err)
	}
	if _, err := storage.ConsumeDirectOAuthState(
		ctx, owner, liveStateHash, now,
	); err != nil {
		t.Fatalf("live state was purged: %v", err)
	}
}

func TestPurgeKeepsConsumedDirectOAuthAttemptDuringProviderCompletionWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	now := time.Date(2042, time.June, 7, 15, 0, 0, 0, time.UTC)
	stateHash := strings.Repeat("4", 64)
	if err := storage.CreateDirectOAuthState(ctx, owner, workspace.ID, DirectOAuthState{
		StateHash: stateHash, PKCEVerifier: strings.Repeat("r", 43),
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ConsumeDirectOAuthStateForWorkspace(
		ctx, owner, workspace.ID, stateHash, now.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	removed, err := storage.PurgeExpiredDirectOAuthStates(ctx, now.Add(2*time.Minute), 10)
	if err != nil || removed != 0 {
		t.Fatalf("in-flight purge removed=%d err=%v, want zero", removed, err)
	}
	if _, err := storage.ReplaceDirectConnectionFromOAuthAttempt(
		ctx, owner, workspace.ID, stateHash, DirectConnection{
			AccountID: "retained-account", CurrencyCode: "RUB", Timezone: "Europe/Moscow",
			TokenCiphertext: "v1.retained", TokenKeyVersion: 1, CreatedAt: now.Add(3 * time.Minute),
		},
	); err != nil {
		t.Fatalf("retained completion could not commit: %v", err)
	}
}

func TestDirectOAuthRestartInvalidatesOnlySameActorWorkspaceAndWorkspaceConsumeIsNonDestructive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	otherWorkspace, err := storage.CreateWorkspace(ctx, owner, Workspace{Name: "Other OAuth workspace"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2042, time.June, 8, 12, 0, 0, 0, time.UTC)
	oldHash := strings.Repeat("c", 64)
	newHash := strings.Repeat("d", 64)
	otherHash := strings.Repeat("e", 64)
	for workspaceID, stateHash := range map[string]string{
		workspace.ID: oldHash, otherWorkspace.ID: otherHash,
	} {
		if err := storage.CreateDirectOAuthState(ctx, owner, workspaceID, DirectOAuthState{
			StateHash: stateHash, PKCEVerifier: strings.Repeat("v", 43),
			CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.CreateDirectOAuthState(ctx, owner, workspace.ID, DirectOAuthState{
		StateHash: newHash, PKCEVerifier: strings.Repeat("n", 43),
		CreatedAt: now.Add(time.Minute), ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ConsumeDirectOAuthState(ctx, owner, oldHash, now.Add(2*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("superseded same-workspace state remained usable: %v", err)
	}
	if _, err := storage.ConsumeDirectOAuthStateForWorkspace(
		ctx, owner, otherWorkspace.ID, newHash, now.Add(2*time.Minute),
	); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong-workspace consume error = %v, want ErrNotFound", err)
	}
	if _, err := storage.ConsumeDirectOAuthStateForWorkspace(
		ctx, owner, workspace.ID, newHash, now.Add(2*time.Minute),
	); err != nil {
		t.Fatalf("wrong-workspace attempt consumed the state: %v", err)
	}
	if _, err := storage.ConsumeDirectOAuthStateForWorkspace(
		ctx, owner, otherWorkspace.ID, otherHash, now.Add(2*time.Minute),
	); err != nil {
		t.Fatalf("restart invalidated another workspace state: %v", err)
	}
}

func TestConcurrentDirectOAuthRestartsLeaveExactlyOneActiveAttempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	now := time.Date(2042, time.June, 9, 12, 0, 0, 0, time.UTC)
	hashes := []string{strings.Repeat("f", 64), strings.Repeat("1", 64)}
	start := make(chan struct{})
	errorsByAttempt := make([]error, len(hashes))
	var wait sync.WaitGroup
	for index, stateHash := range hashes {
		index, stateHash := index, stateHash
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errorsByAttempt[index] = storage.CreateDirectOAuthState(
				ctx, owner, workspace.ID, DirectOAuthState{
					StateHash: stateHash, PKCEVerifier: strings.Repeat("p", 43),
					CreatedAt: now, ExpiresAt: now.Add(time.Hour),
				},
			)
		}()
	}
	close(start)
	wait.Wait()
	for index, err := range errorsByAttempt {
		if err != nil {
			t.Fatalf("restart %d: %v", index, err)
		}
	}
	successes, missing := 0, 0
	for _, stateHash := range hashes {
		_, err := storage.ConsumeDirectOAuthStateForWorkspace(
			ctx, owner, workspace.ID, stateHash, now.Add(time.Minute),
		)
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrNotFound):
			missing++
		default:
			t.Fatalf("consume %q: %v", stateHash, err)
		}
	}
	if successes != 1 || missing != 1 {
		t.Fatalf("active attempts: success=%d missing=%d, want exactly one of each", successes, missing)
	}
}

func TestStaleInFlightDirectOAuthCompletionCannotReplaceConnectionAfterRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	now := time.Date(2042, time.June, 10, 12, 0, 0, 0, time.UTC)
	oldHash := strings.Repeat("2", 64)
	newHash := strings.Repeat("3", 64)
	createAttempt := func(stateHash string, createdAt time.Time) {
		t.Helper()
		if err := storage.CreateDirectOAuthState(ctx, owner, workspace.ID, DirectOAuthState{
			StateHash: stateHash, PKCEVerifier: strings.Repeat("q", 43),
			CreatedAt: createdAt, ExpiresAt: createdAt.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	createAttempt(oldHash, now)
	if _, err := storage.ConsumeDirectOAuthStateForWorkspace(
		ctx, owner, workspace.ID, oldHash, now.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}

	providerCallStarted := make(chan struct{})
	releaseProviderCall := make(chan struct{})
	oldCompletionDone := make(chan error, 1)
	go func() {
		close(providerCallStarted)
		<-releaseProviderCall
		_, err := storage.ReplaceDirectConnectionFromOAuthAttempt(
			ctx, owner, workspace.ID, oldHash, DirectConnection{
				AccountID: "stale-account", CurrencyCode: "RUB", Timezone: "Europe/Moscow",
				TokenCiphertext: "v1.stale", TokenKeyVersion: 1, CreatedAt: now.Add(2 * time.Minute),
			},
		)
		oldCompletionDone <- err
	}()
	<-providerCallStarted

	// This models a user restarting OAuth while the old completion is waiting
	// on ExchangeCode/GetAccount outside the database transaction.
	createAttempt(newHash, now.Add(2*time.Minute))
	if _, err := storage.ReplaceDirectConnection(
		ctx, owner, workspace.ID, DirectConnection{
			AccountID: "bypass-account", CurrencyCode: "RUB", Timezone: "Europe/Moscow",
			TokenCiphertext: "v1.bypass", TokenKeyVersion: 1, CreatedAt: now.Add(2 * time.Minute),
		},
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("non-CAS replacement bypassed pending OAuth attempt: %v", err)
	}
	close(releaseProviderCall)
	if err := <-oldCompletionDone; !errors.Is(err, ErrConflict) {
		t.Fatalf("stale completion error = %v, want ErrConflict", err)
	}
	if _, err := storage.GetDirectConnection(ctx, owner, workspace.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale completion wrote a connection: %v", err)
	}

	if _, err := storage.ConsumeDirectOAuthStateForWorkspace(
		ctx, owner, workspace.ID, newHash, now.Add(3*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	connection, err := storage.ReplaceDirectConnectionFromOAuthAttempt(
		ctx, owner, workspace.ID, newHash, DirectConnection{
			AccountID: "latest-account", CurrencyCode: "RUB", Timezone: "Europe/Moscow",
			TokenCiphertext: "v1.latest", TokenKeyVersion: 1, CreatedAt: now.Add(4 * time.Minute),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if connection.AccountID != "latest-account" {
		t.Fatalf("latest connection = %#v", connection)
	}
	if _, err := storage.ConsumeDirectOAuthStateForWorkspace(
		ctx, owner, workspace.ID, newHash, now.Add(5*time.Minute),
	); !errors.Is(err, ErrNotFound) {
		t.Fatalf("successful attempt was not retired: %v", err)
	}
}

func TestWorkspaceArchiveRefusesDirectLaunchReconciliation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, _ := newDirectStoreFixture(t, ctx)
	workspace, err := storage.CreateWorkspace(ctx, owner, Workspace{Name: "Direct team"})
	if err != nil {
		t.Fatal(err)
	}
	connection := connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2042, time.July, 8, 12, 0, 0, 0, time.UTC)
	campaign := createDirectTestCampaign(t, ctx, storage, owner, workspace.ID, now)
	campaign = acceptDirectTestCampaign(t, ctx, storage, owner, workspace.ID, campaign, now)
	_, err = storage.ClaimDirectManualCampaignLaunch(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version, *campaign.ProviderCampaignID,
		connection.AccountID, campaign.WeeklyBudgetMinor, campaign.StartsAt, campaign.EndsAt,
		campaign.ProviderGraphHash, campaign.ProviderRevisionID,
		now.Add(time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	err = storage.DeleteWorkspace(ctx, owner, workspace.ID)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "Yandex Direct") {
		t.Fatalf("archive during launch error = %v, want Direct lifecycle conflict", err)
	}
}

func TestValidateDirectCampaignBudgetMinimum(t *testing.T) {
	t.Parallel()
	base := DirectCampaign{
		Name: "Budget", Objective: "traffic", LandingURL: "https://maxposty.ru/",
		Brief: "Validate official minimum", Regions: []string{"225"}, CurrencyCode: "RUB",
		Titles: []string{"Budget title"}, Texts: []string{"Budget text"},
		Keywords: []string{"budget keyword"}, NegativeKeywords: []string{},
		StartsAt: time.Date(2042, 1, 1, 0, 0, 0, 0, time.UTC),
		EndsAt:   time.Date(2042, 2, 1, 0, 0, 0, 0, time.UTC),
	}
	base.WeeklyBudgetMinor = 29_900
	if err := validateDirectCampaignDraft(&base); err == nil {
		t.Fatal("299 RUB weekly budget was accepted")
	}
	base.WeeklyBudgetMinor = 30_000
	if err := validateDirectCampaignDraft(&base); err != nil {
		t.Fatalf("300 RUB weekly budget was rejected: %v", err)
	}
	base.LandingURL = "https://maxposty.ru/landing#tracking"
	if err := validateDirectCampaignDraft(&base); err == nil {
		t.Fatal("landing URL with a fragment was accepted")
	}
}

func TestDirectSubmissionRejectsInvalidExpectedVersionAsValidation(t *testing.T) {
	t.Parallel()
	var storage Store
	if _, _, err := storage.ClaimDirectCampaignSubmission(
		context.Background(), "actor", "workspace", "campaign", 0, time.Now(),
	); !errors.Is(err, ErrDirectValidation) {
		t.Fatalf("invalid submit version error = %v, want ErrDirectValidation", err)
	}
}

func TestDirectDelayedLaunchTruthIsAtomicAndKeepsCredential(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connection := connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2042, time.September, 10, 12, 0, 0, 0, time.UTC)
	campaign := createDirectTestCampaign(t, ctx, storage, owner, workspace.ID, now)
	campaign = acceptDirectTestCampaign(t, ctx, storage, owner, workspace.ID, campaign, now)
	if _, err := storage.GrantDirectAutoLaunchConsent(
		ctx, owner, workspace.ID, campaign.ID, DirectConsentRequest{
			Confirmation: "АВТОЗАПУСК", ExpectedVersion: campaign.Version,
			ExpectedConnectionID: connection.ID, ExpectedAccountID: connection.AccountID,
			ExpectedCampaignName: campaign.Name, ExpectedProviderID: *campaign.ProviderCampaignID,
			ExpectedGraphHash: campaign.ProviderGraphHash, ExpectedRevisionID: campaign.ProviderRevisionID,
			WeeklyBudgetMinor: campaign.WeeklyBudgetMinor,
			StartsAt:          campaign.StartsAt, EndsAt: campaign.EndsAt, AuthorizedAt: now,
		},
	); err != nil {
		t.Fatal(err)
	}
	claimed, err := storage.ClaimDirectManualCampaignLaunch(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version, *campaign.ProviderCampaignID,
		connection.AccountID, campaign.WeeklyBudgetMinor, campaign.StartsAt, campaign.EndsAt,
		campaign.ProviderGraphHash, campaign.ProviderRevisionID,
		now.Add(time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		at := now.Add(time.Duration(attempt+2) * time.Minute)
		if err := storage.MarkDirectCampaignLaunchAttempt(
			ctx, workspace.ID, campaign.ID, *claimed.Campaign.LaunchClaimedAt, at,
		); err != nil {
			t.Fatal(err)
		}
		if err := storage.MarkDirectCampaignLaunchReconciling(
			ctx, workspace.ID, campaign.ID, *claimed.Campaign.LaunchClaimedAt,
			"provider_timeout", at,
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.FailDirectCampaignLaunch(
		ctx, workspace.ID, campaign.ID, *claimed.Campaign.LaunchClaimedAt,
		"provider_off_after_retries", now.Add(4*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	// A manual failed claim with still-valid auto consent must not churn forever
	// in the automatic launch queue.
	candidates, err := storage.ClaimDirectAutoLaunchCandidates(ctx, now.Add(24*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("failed launch returned to auto queue: %#v", candidates)
	}
	// Failed is an ambiguous spend-capable state. Even well after the former
	// grace window the credential is retained for delayed provider truth.
	if err := storage.RevokeDirectConnection(
		ctx, owner, workspace.ID, now.Add(24*time.Hour),
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("revoke after ambiguous failure = %v, want ErrConflict", err)
	}
	for _, observed := range []struct {
		status string
		state  string
	}{
		{status: "ACCEPTED", state: "SUSPENDED"},
		{status: "REJECTED", state: "OFF"},
	} {
		campaign, err = storage.SyncDirectCampaignProviderStatus(
			ctx, workspace.ID, campaign.ID, *campaign.ProviderCampaignID,
			observed.status, observed.state, now.Add(25*time.Hour),
		)
		if err != nil {
			t.Fatalf("sync failed truth %#v: %v", observed, err)
		}
		if campaign.LaunchState != "failed" || campaign.Status != "accepted" ||
			campaign.ProviderStatus != observed.status || campaign.ProviderState != observed.state {
			t.Fatalf("failed provider truth %#v produced campaign %#v", observed, campaign)
		}
	}
	campaign, err = storage.SyncDirectCampaignProviderStatus(
		ctx, workspace.ID, campaign.ID, *campaign.ProviderCampaignID,
		"ACCEPTED", "ON", now.Add(26*time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	if campaign.Status != "active" || campaign.LaunchState != "confirmed" ||
		campaign.LaunchedAt == nil || campaign.LaunchFailedAt != nil ||
		campaign.LaunchReconcileAfter != nil {
		t.Fatalf("delayed provider success was not promoted atomically: %#v", campaign)
	}
}

func TestDirectProviderONConfirmsReconcilingLaunchAtomically(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connection := connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2042, time.October, 11, 12, 0, 0, 0, time.UTC)
	campaign := createDirectTestCampaign(t, ctx, storage, owner, workspace.ID, now)
	campaign = acceptDirectTestCampaign(t, ctx, storage, owner, workspace.ID, campaign, now)
	claimed, err := storage.ClaimDirectManualCampaignLaunch(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version, *campaign.ProviderCampaignID,
		connection.AccountID, campaign.WeeklyBudgetMinor, campaign.StartsAt, campaign.EndsAt,
		campaign.ProviderGraphHash, campaign.ProviderRevisionID,
		now.Add(time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.MarkDirectCampaignLaunchAttempt(
		ctx, workspace.ID, campaign.ID, *claimed.Campaign.LaunchClaimedAt,
		now.Add(2*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if err := storage.MarkDirectCampaignLaunchReconciling(
		ctx, workspace.ID, campaign.ID, *claimed.Campaign.LaunchClaimedAt,
		"provider_timeout", now.Add(2*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	campaign, err = storage.SyncDirectCampaignProviderStatusForLaunch(
		ctx, workspace.ID, campaign.ID, *campaign.ProviderCampaignID,
		"ACCEPTED", "ON", *claimed.Campaign.LaunchClaimedAt,
		now.Add(3*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if campaign.Status != "active" || campaign.LaunchState != "confirmed" ||
		campaign.LaunchedAt == nil || campaign.LaunchReconcileAfter != nil {
		t.Fatalf("reconciling provider success was not confirmed atomically: %#v", campaign)
	}
}

func TestDirectProviderPollingQueueDoesNotStarveBeyondLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2042, time.August, 9, 12, 0, 0, 0, time.UTC)
	expected := make(map[string]bool)
	for index := 0; index < 5; index++ {
		campaign := createDirectTestCampaign(
			t, ctx, storage, owner, workspace.ID, now.Add(time.Duration(index)*time.Second),
		)
		expected[campaign.ID] = true
		if _, _, err := storage.ClaimDirectCampaignSubmission(
			ctx, owner, workspace.ID, campaign.ID, campaign.Version, now,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := storage.MarkDirectCampaignSubmitted(
			ctx, owner, workspace.ID, campaign.ID, campaign.Version,
			int64(78_000+index), "DRAFT", "OFF", now,
		); err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[string]bool)
	for batchIndex, expectedSize := range []int{2, 2, 1} {
		batch, err := storage.ClaimDirectProviderSyncCandidates(ctx, now, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) != expectedSize {
			t.Fatalf("batch %d size = %d, want %d (%#v)", batchIndex, len(batch), expectedSize, batch)
		}
		for _, candidate := range batch {
			campaignID := candidate.CampaignID
			if candidate.WorkspaceID != workspace.ID || !expected[campaignID] || seen[campaignID] {
				t.Fatalf("unexpected or repeated campaign %#v in batches", candidate)
			}
			seen[campaignID] = true
		}
	}
	if len(seen) != len(expected) {
		t.Fatalf("polled %d/%d campaigns: %#v", len(seen), len(expected), seen)
	}
}

func TestDirectTokenRefreshLeaseAndCiphertextCAS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connection := connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2042, time.November, 12, 13, 0, 0, 0, time.UTC)
	claimed, err := storage.ClaimDirectConnectionTokenRefresh(
		ctx, workspace.ID, connection.ID, connection.TokenCiphertext, now,
	)
	if err != nil || claimed.TokenRefreshClaimedAt == nil {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	if _, err := storage.ClaimDirectConnectionTokenRefresh(
		ctx, workspace.ID, connection.ID, connection.TokenCiphertext,
		now.Add(time.Second),
	); !errors.Is(err, ErrDirectTokenRefreshBusy) {
		t.Fatalf("concurrent claim error = %v, want busy", err)
	}
	if _, err := storage.CompleteDirectConnectionTokenRefresh(
		ctx, workspace.ID, connection.ID, "different-ciphertext",
		*claimed.TokenRefreshClaimedAt, "v2.replacement", now.Add(2*time.Second),
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale CAS error = %v, want conflict", err)
	}
	updated, err := storage.CompleteDirectConnectionTokenRefresh(
		ctx, workspace.ID, connection.ID, connection.TokenCiphertext,
		*claimed.TokenRefreshClaimedAt, "v2.replacement", now.Add(2*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if updated.TokenCiphertext != "v2.replacement" ||
		updated.TokenRefreshClaimedAt != nil ||
		updated.LastVerifiedAt == nil {
		t.Fatalf("completed refresh = %#v", updated)
	}
	if err := storage.ReleaseDirectConnectionTokenRefresh(
		ctx, workspace.ID, connection.ID, connection.TokenCiphertext,
		*claimed.TokenRefreshClaimedAt, now.Add(3*time.Second),
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale release error = %v, want conflict", err)
	}
	reclaimed, err := storage.ClaimDirectConnectionTokenRefresh(
		ctx, workspace.ID, connection.ID, updated.TokenCiphertext,
		now.Add(directTokenRefreshLease+time.Second),
	)
	if err != nil || reclaimed.TokenRefreshClaimedAt == nil {
		t.Fatalf("claim after rotation = %#v, %v", reclaimed, err)
	}
}

func TestDirectAggregateBudgetCapSerializesConcurrentLaunchClaims(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connection := connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2042, time.December, 13, 12, 0, 0, 0, time.UTC)
	nextProviderID := int64(88_000)
	createAccepted := func(budget int64) DirectCampaign {
		nextProviderID++
		campaign, err := storage.CreateDirectCampaign(ctx, owner, workspace.ID, DirectCampaign{
			Name:      fmt.Sprintf("Budget campaign %d", nextProviderID),
			Objective: "traffic", LandingURL: "https://maxposty.ru/",
			Brief: "Verify aggregate budget serialization", Regions: []string{"225"},
			Titles: []string{"Budget campaign"}, Texts: []string{"Budget campaign text"},
			Keywords: []string{"budget campaign"}, NegativeKeywords: []string{},
			WeeklyBudgetMinor: budget, StartsAt: now, EndsAt: now.AddDate(0, 1, 0),
			CreatedAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		return acceptDirectTestCampaign(
			t, ctx, storage, owner, workspace.ID, campaign, now,
		)
	}
	for _, budget := range []int64{1_000_000, 1_000_000, 500_000} {
		campaign := createAccepted(budget)
		claimed, err := storage.ClaimDirectManualCampaignLaunch(
			ctx, owner, workspace.ID, campaign.ID, campaign.Version,
			*campaign.ProviderCampaignID, connection.AccountID,
			campaign.WeeklyBudgetMinor, campaign.StartsAt, campaign.EndsAt,
			campaign.ProviderGraphHash, campaign.ProviderRevisionID, now,
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := storage.CompleteDirectCampaignLaunch(
			ctx, workspace.ID, campaign.ID, *claimed.Campaign.LaunchClaimedAt, now,
		); err != nil {
			t.Fatal(err)
		}
	}
	candidates := []DirectCampaign{createAccepted(400_000), createAccepted(400_000)}
	start := make(chan struct{})
	results := make(chan error, len(candidates))
	var wait sync.WaitGroup
	for _, candidate := range candidates {
		candidate := candidate
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := storage.ClaimDirectManualCampaignLaunch(
				ctx, owner, workspace.ID, candidate.ID, candidate.Version,
				*candidate.ProviderCampaignID, connection.AccountID,
				candidate.WeeklyBudgetMinor, candidate.StartsAt, candidate.EndsAt,
				candidate.ProviderGraphHash, candidate.ProviderRevisionID,
				now.Add(time.Minute),
			)
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	var succeeded, capped int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrDirectBudgetCapExceeded):
			capped++
		default:
			t.Fatalf("unexpected concurrent claim error: %v", err)
		}
	}
	if succeeded != 1 || capped != 1 {
		t.Fatalf("concurrent claims succeeded=%d capped=%d", succeeded, capped)
	}
}

func TestDirectLaunchRecoveryRotatesFenceAndRejectsStaleWorker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connection := connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2043, time.January, 14, 12, 0, 0, 0, time.UTC)
	campaign := createDirectTestCampaign(t, ctx, storage, owner, workspace.ID, now)
	campaign = acceptDirectTestCampaign(t, ctx, storage, owner, workspace.ID, campaign, now)
	claimed, err := storage.ClaimDirectManualCampaignLaunch(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
		*campaign.ProviderCampaignID, connection.AccountID,
		campaign.WeeklyBudgetMinor, campaign.StartsAt, campaign.EndsAt,
		campaign.ProviderGraphHash, campaign.ProviderRevisionID, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	staleGeneration := *claimed.Campaign.LaunchClaimedAt
	if err := storage.MarkDirectCampaignLaunchReconciling(
		ctx, workspace.ID, campaign.ID, staleGeneration,
		"provider_timeout", now,
	); err != nil {
		t.Fatal(err)
	}
	candidates, err := storage.ClaimDirectLaunchRecoveryCandidates(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].CampaignID != campaign.ID {
		t.Fatalf("recovery candidates = %#v", candidates)
	}
	recovery, err := storage.GetDirectLaunchRecoveryMaterial(
		ctx, workspace.ID, campaign.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	freshGeneration := *recovery.Campaign.LaunchClaimedAt
	if !freshGeneration.After(staleGeneration) {
		t.Fatalf(
			"recovery generation %s did not advance past %s",
			freshGeneration, staleGeneration,
		)
	}
	staleCalls := map[string]func() error{
		"attempt": func() error {
			return storage.MarkDirectCampaignLaunchAttempt(
				ctx, workspace.ID, campaign.ID, staleGeneration, now,
			)
		},
		"reconciling": func() error {
			return storage.MarkDirectCampaignLaunchReconciling(
				ctx, workspace.ID, campaign.ID, staleGeneration,
				"stale_worker", now,
			)
		},
		"abort": func() error {
			return storage.AbortDirectCampaignLaunchForAuthorization(
				ctx, workspace.ID, campaign.ID, staleGeneration, now,
			)
		},
		"complete": func() error {
			return storage.CompleteDirectCampaignLaunch(
				ctx, workspace.ID, campaign.ID, staleGeneration, now,
			)
		},
		"sync": func() error {
			_, syncErr := storage.SyncDirectCampaignProviderStatusForLaunch(
				ctx, workspace.ID, campaign.ID, *campaign.ProviderCampaignID,
				"ACCEPTED", "ON", staleGeneration, now,
			)
			return syncErr
		},
		"snapshot_mismatch": func() error {
			return storage.SetDirectCampaignProviderSnapshotMismatchForLaunch(
				ctx, workspace.ID, campaign.ID, true, staleGeneration, now,
			)
		},
	}
	for name, staleCall := range staleCalls {
		if err := staleCall(); err == nil {
			t.Fatalf("stale %s transition succeeded", name)
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := storage.MarkDirectCampaignLaunchAttempt(
			ctx, workspace.ID, campaign.ID, freshGeneration,
			now.Add(time.Duration(attempt+1)*time.Second),
		); err != nil {
			t.Fatalf("fresh attempt %d: %v", attempt+1, err)
		}
	}
	if err := storage.FailDirectCampaignLaunch(
		ctx, workspace.ID, campaign.ID, staleGeneration,
		"stale_failure", now.Add(3*time.Second),
	); err == nil {
		t.Fatal("stale terminal failure succeeded")
	}
	current, err := storage.GetDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if current.LaunchClaimedAt == nil ||
		!current.LaunchClaimedAt.Equal(freshGeneration) ||
		current.LaunchState != "launching" ||
		current.LaunchAttemptCount != 2 {
		t.Fatalf("stale worker changed fresh recovery claim: %#v", current)
	}
}

func TestDirectManualReclaimRotatesFenceAndRejectsPriorGeneration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, owner, workspace := newDirectStoreFixture(t, ctx)
	connection := connectDirectTestAccount(t, ctx, storage, owner, workspace.ID)
	now := time.Date(2043, time.February, 15, 12, 0, 0, 0, time.UTC)
	campaign := createDirectTestCampaign(t, ctx, storage, owner, workspace.ID, now)
	campaign = acceptDirectTestCampaign(t, ctx, storage, owner, workspace.ID, campaign, now)
	first, err := storage.ClaimDirectManualCampaignLaunch(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
		*campaign.ProviderCampaignID, connection.AccountID,
		campaign.WeeklyBudgetMinor, campaign.StartsAt, campaign.EndsAt,
		campaign.ProviderGraphHash, campaign.ProviderRevisionID, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	firstGeneration := *first.Campaign.LaunchClaimedAt
	for attempt := 0; attempt < 2; attempt++ {
		if err := storage.MarkDirectCampaignLaunchAttempt(
			ctx, workspace.ID, campaign.ID, firstGeneration, now,
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.FailDirectCampaignLaunch(
		ctx, workspace.ID, campaign.ID, firstGeneration,
		"provider_off_after_retries", now,
	); err != nil {
		t.Fatal(err)
	}
	second, err := storage.ClaimDirectManualCampaignLaunch(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
		*campaign.ProviderCampaignID, connection.AccountID,
		campaign.WeeklyBudgetMinor, campaign.StartsAt, campaign.EndsAt,
		campaign.ProviderGraphHash, campaign.ProviderRevisionID, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	secondGeneration := *second.Campaign.LaunchClaimedAt
	if !secondGeneration.After(firstGeneration) {
		t.Fatalf(
			"manual reclaim generation %s did not advance past %s",
			secondGeneration, firstGeneration,
		)
	}
	if err := storage.AbortDirectCampaignLaunchForAuthorization(
		ctx, workspace.ID, campaign.ID, firstGeneration, now,
	); err == nil {
		t.Fatal("prior generation aborted manual reclaim")
	}
	if err := storage.CompleteDirectCampaignLaunch(
		ctx, workspace.ID, campaign.ID, firstGeneration, now,
	); err == nil {
		t.Fatal("prior generation completed manual reclaim")
	}
	if err := storage.CompleteDirectCampaignLaunch(
		ctx, workspace.ID, campaign.ID, secondGeneration, now,
	); err != nil {
		t.Fatalf("fresh generation completion: %v", err)
	}
}

func newDirectStoreFixture(
	t *testing.T, ctx context.Context,
) (*Store, string, Workspace) {
	t.Helper()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "direct.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	owner := "direct-owner-" + newStoreID("")
	if err := storage.UpsertUser(ctx, User{ID: owner, DisplayName: owner}); err != nil {
		t.Fatal(err)
	}
	return storage, owner, requirePersonalWorkspace(t, ctx, storage, owner)
}

func connectDirectTestAccount(
	t *testing.T, ctx context.Context, storage *Store, owner, workspaceID string,
) DirectConnection {
	t.Helper()
	connection, err := storage.ReplaceDirectConnection(ctx, owner, workspaceID, DirectConnection{
		AccountID: "client-123", ClientLogin: "client-login", AccountName: "Test Direct",
		CurrencyCode: "RUB", Timezone: "Europe/Moscow", TokenCiphertext: "v1.test-token",
		TokenKeyVersion: 1, CreatedAt: time.Date(2042, time.January, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func createDirectTestCampaign(
	t *testing.T, ctx context.Context, storage *Store, owner, workspaceID string, now time.Time,
) DirectCampaign {
	t.Helper()
	campaign, err := storage.CreateDirectCampaign(ctx, owner, workspaceID, DirectCampaign{
		Name: "Test campaign", Objective: "traffic", LandingURL: "https://maxposty.ru/",
		Brief: "Promote the workspace channel", Regions: []string{"225"},
		Titles:            []string{"Тестовое объявление"},
		Texts:             []string{"Проверяем полную схему объявления"},
		Keywords:          []string{"ведение канала"},
		NegativeKeywords:  []string{"бесплатно"},
		WeeklyBudgetMinor: 30_000, StartsAt: now, EndsAt: now.AddDate(0, 1, 0),
		CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return campaign
}

func acceptDirectTestCampaign(
	t *testing.T, ctx context.Context, storage *Store, owner, workspaceID string,
	campaign DirectCampaign, now time.Time,
) DirectCampaign {
	t.Helper()
	material, err := storage.ClaimDirectCampaignGraphSubmission(
		ctx, owner, workspaceID, campaign.ID, campaign.Version,
		"submit_"+campaign.ID, now.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	var providerCampaignID int64
	if err := storage.db.QueryRowContext(ctx, `SELECT COALESCE(
MAX(provider_campaign_id),77000
)+1 FROM direct_campaigns WHERE workspace_id=$1`, workspaceID).Scan(
		&providerCampaignID,
	); err != nil {
		t.Fatal(err)
	}
	providerAdGroupID, providerAdID := providerCampaignID*10+1, providerCampaignID*10+2
	keywordMappings := make([]DirectKeywordMapping, len(campaign.Keywords))
	for index, keyword := range campaign.Keywords {
		keywordMappings[index] = DirectKeywordMapping{
			Keyword: keyword, ProviderKeywordID: providerCampaignID*100 + int64(index+1),
			Moderation: DirectModerationSnapshot{Status: "DRAFT"},
		}
	}
	advance := func(expectedStage string, update DirectProviderStageUpdate, at time.Time) {
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
		DirectModerationSnapshot{Status: "DRAFT", State: "OFF"},
		DirectModerationSnapshot{Status: "DRAFT"},
		DirectModerationSnapshot{Status: "DRAFT", State: "OFF"},
	)
	graphHash := strings.Repeat("d", 64)
	if graphHash == directGraphHash(observedGraph) {
		t.Fatal("test provider fingerprint unexpectedly equals full JSON hash")
	}
	advance("keywords_created", DirectProviderStageUpdate{
		Stage: "graph_observed", ObservedGraph: observedGraph, GraphHash: graphHash,
	}, now.Add(6*time.Second))
	revision, err := storage.RecordVerifiedDirectCampaignGraph(
		ctx, workspaceID, campaign.ID, DirectVerifiedGraphInput{
			ExpectedOperationID: material.Operation.ID,
			ExpectedStage:       "graph_observed", ExpectedCampaignVersion: campaign.Version,
			ExpectedClaimedAt: material.Operation.ClaimedAt,
			GraphVersion:      DirectGraphFingerprintVersion,
			DesiredGraph:      material.Operation.DesiredGraph, ObservedGraph: observedGraph,
			GraphHash: graphHash, ProviderCampaignID: providerCampaignID,
			ProviderAdGroupID: providerAdGroupID, ProviderAdID: providerAdID,
			ProviderKeywordMappings:   keywordMappings,
			CampaignModeration:        DirectModerationSnapshot{Status: "DRAFT"},
			AdGroupModeration:         DirectModerationSnapshot{Status: "DRAFT"},
			AdModeration:              DirectModerationSnapshot{Status: "DRAFT"},
			AggregateModerationStatus: "MODERATION",
			ObservedAt:                now.Add(7 * time.Second), ActorUserID: owner,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	acceptedMappings := append([]DirectKeywordMapping(nil), keywordMappings...)
	for index := range acceptedMappings {
		acceptedMappings[index].Moderation.Status = "ACCEPTED"
	}
	accepted, err := storage.UpdateDirectCampaignGraphModeration(
		ctx, workspaceID, campaign.ID, DirectGraphModerationUpdate{
			ExpectedGraphHash: graphHash, ExpectedRevisionID: revision.ID,
			Campaign: DirectModerationSnapshot{Status: "ACCEPTED"},
			AdGroup:  DirectModerationSnapshot{Status: "ACCEPTED"},
			Ad:       DirectModerationSnapshot{Status: "ACCEPTED"},
			Keywords: acceptedMappings, ProviderStatus: "ACCEPTED",
			AggregateModerationStatus: "ACCEPTED",
			ProviderState:             "OFF", CheckedAt: now.Add(8 * time.Second),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	material.Operation.Stage = "verified"
	advance("verified", DirectProviderStageUpdate{
		Stage: "moderation_requested",
	}, now.Add(9*time.Second))
	advance("moderation_requested", DirectProviderStageUpdate{
		Stage: "completed", Complete: true,
	}, now.Add(10*time.Second))
	accepted.SubmissionStage = material.Operation.Stage
	return accepted
}
