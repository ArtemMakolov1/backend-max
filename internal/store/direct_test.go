package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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
		campaign.AutoLaunch.WarningCode != "provider_creatives_not_snapshotted" {
		t.Fatalf("public consent summary is incomplete: %#v", campaign.AutoLaunch)
	}

	claimed, err := storage.ClaimDirectAutoCampaignLaunch(
		ctx, workspace.ID, campaign.ID, campaign.Version, *campaign.ProviderCampaignID,
		connection.AccountID, campaign.WeeklyBudgetMinor, campaign.StartsAt, campaign.EndsAt,
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
		ctx, workspace.ID, campaign.ID, now.Add(3*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if err := storage.MarkDirectCampaignLaunchReconciling(
		ctx, workspace.ID, campaign.ID, "provider_timeout", now.Add(4*time.Minute),
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
	if err := storage.CompleteDirectCampaignLaunch(
		ctx, workspace.ID, campaign.ID, now.Add(5*time.Minute),
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
			Confirmation: "РђР’РўРћР—РђРџРЈРЎРљ", ExpectedVersion: campaign.Version,
			ExpectedConnectionID: connection.ID, ExpectedAccountID: connection.AccountID,
			ExpectedCampaignName: campaign.Name, ExpectedProviderID: *campaign.ProviderCampaignID,
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
	if _, err := storage.ClaimDirectManualCampaignLaunch(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version, *campaign.ProviderCampaignID,
		connection.AccountID, campaign.WeeklyBudgetMinor, campaign.StartsAt, campaign.EndsAt,
		now.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if err := storage.MarkDirectCampaignLaunchAttempt(
		ctx, workspace.ID, campaign.ID, now.Add(2*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if err := storage.AbortDirectCampaignLaunchForAuthorization(
		ctx, workspace.ID, campaign.ID, now.Add(3*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	campaign, err := storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
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
			CreatedAt: now, ExpiresAt: now.Add(time.Minute),
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
	removed, err := storage.PurgeExpiredDirectOAuthStates(ctx, now.Add(2*time.Minute), 1)
	if err != nil || removed != 1 {
		t.Fatalf("purge removed=%d err=%v, want one", removed, err)
	}
	if _, err := storage.ConsumeDirectOAuthState(
		ctx, owner, expiredStateHash, now.Add(2*time.Minute),
	); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired state remained: %v", err)
	}
	if _, err := storage.ConsumeDirectOAuthState(
		ctx, owner, liveStateHash, now.Add(2*time.Minute),
	); err != nil {
		t.Fatalf("live state was purged: %v", err)
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
	if _, err := storage.ClaimDirectManualCampaignLaunch(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version, *campaign.ProviderCampaignID,
		connection.AccountID, campaign.WeeklyBudgetMinor, campaign.StartsAt, campaign.EndsAt,
		now.Add(time.Minute),
	); err != nil {
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
			WeeklyBudgetMinor: campaign.WeeklyBudgetMinor,
			StartsAt:          campaign.StartsAt, EndsAt: campaign.EndsAt, AuthorizedAt: now,
		},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ClaimDirectManualCampaignLaunch(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version, *campaign.ProviderCampaignID,
		connection.AccountID, campaign.WeeklyBudgetMinor, campaign.StartsAt, campaign.EndsAt,
		now.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		at := now.Add(time.Duration(attempt+2) * time.Minute)
		if err := storage.MarkDirectCampaignLaunchAttempt(
			ctx, workspace.ID, campaign.ID, at,
		); err != nil {
			t.Fatal(err)
		}
		if err := storage.MarkDirectCampaignLaunchReconciling(
			ctx, workspace.ID, campaign.ID, "provider_timeout", at,
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.FailDirectCampaignLaunch(
		ctx, workspace.ID, campaign.ID, "provider_off_after_retries", now.Add(4*time.Minute),
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
	if _, err := storage.ClaimDirectManualCampaignLaunch(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version, *campaign.ProviderCampaignID,
		connection.AccountID, campaign.WeeklyBudgetMinor, campaign.StartsAt, campaign.EndsAt,
		now.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if err := storage.MarkDirectCampaignLaunchAttempt(
		ctx, workspace.ID, campaign.ID, now.Add(2*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if err := storage.MarkDirectCampaignLaunchReconciling(
		ctx, workspace.ID, campaign.ID, "provider_timeout", now.Add(2*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	campaign, err := storage.SyncDirectCampaignProviderStatus(
		ctx, workspace.ID, campaign.ID, *campaign.ProviderCampaignID,
		"ACCEPTED", "ON", now.Add(3*time.Minute),
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
	if _, _, err := storage.ClaimDirectCampaignSubmission(
		ctx, owner, workspaceID, campaign.ID, campaign.Version, now.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	submitted, err := storage.MarkDirectCampaignSubmitted(
		ctx, owner, workspaceID, campaign.ID, campaign.Version,
		77_001, "DRAFT", "OFF", now.Add(2*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := storage.SyncDirectCampaignProviderStatus(
		ctx, workspaceID, campaign.ID, *submitted.ProviderCampaignID,
		"ACCEPTED", "OFF", now.Add(3*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	return accepted
}
