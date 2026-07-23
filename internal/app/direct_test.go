package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexdirect"
)

type fakeDirectProvider struct {
	account     yandexdirect.Account
	campaign    yandexdirect.Campaign
	oauthFlow   yandexdirect.OAuthFlow
	resumeCalls int
	resume      func(*fakeDirectProvider) error
	getErr      error
	getContext  func(context.Context)
}

func (f *fakeDirectProvider) OAuthFlow() yandexdirect.OAuthFlow {
	if f.oauthFlow == "" {
		return yandexdirect.OAuthFlowCallback
	}
	return f.oauthFlow
}
func (f *fakeDirectProvider) AuthorizationURL(string, string) string { return "https://oauth.test/" }
func (f *fakeDirectProvider) ExchangeCode(context.Context, string, string) (string, error) {
	return "token", nil
}
func (f *fakeDirectProvider) GetAccount(context.Context, string, string) (yandexdirect.Account, error) {
	return f.account, nil
}
func (f *fakeDirectProvider) CreateCampaignDraft(
	_ context.Context, _, _ string, draft yandexdirect.CampaignDraft,
) (yandexdirect.Campaign, error) {
	f.campaign.Name = draft.Name
	f.campaign.WeeklyBudgetMinor = draft.WeeklyBudgetMinor
	f.campaign.StartsAt = draft.StartsAt
	f.campaign.EndsAt = draft.EndsAt
	if f.campaign.ID == 0 {
		f.campaign.ID = 98_001
	}
	f.campaign.Status = "DRAFT"
	f.campaign.State = "OFF"
	return f.campaign, nil
}
func (f *fakeDirectProvider) GetCampaign(
	ctx context.Context, _ string, _ string, _ int64,
) (yandexdirect.Campaign, error) {
	if f.getContext != nil {
		f.getContext(ctx)
	}
	if f.getErr != nil {
		return yandexdirect.Campaign{}, f.getErr
	}
	return f.campaign, nil
}
func (f *fakeDirectProvider) ResumeCampaign(context.Context, string, string, int64) error {
	f.resumeCalls++
	if f.resume != nil {
		return f.resume(f)
	}
	f.campaign.State = "ON"
	return nil
}
func (f *fakeDirectProvider) Sandbox() bool { return true }

func TestDirectOAuthInputValidationIsFlowSpecific(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"1234567", "0000000"} {
		if !validDirectOAuthCode(yandexdirect.OAuthFlowVerificationCode, code) {
			t.Fatalf("verification code %q was rejected", code)
		}
	}
	for _, code := range []string{
		"", "123456", "12345678", "123456a", " 1234567", "1234567 ", "１２３４５６７",
	} {
		if validDirectOAuthCode(yandexdirect.OAuthFlowVerificationCode, code) {
			t.Fatalf("invalid verification code %q was accepted", code)
		}
	}
	if !validDirectOAuthCode(yandexdirect.OAuthFlowCallback, "callback-code.with_symbols~") ||
		validDirectOAuthCode(yandexdirect.OAuthFlowCallback, "callback code") {
		t.Fatal("callback code validation is not bounded printable ASCII")
	}
	state, err := randomDirectToken(32)
	if err != nil {
		t.Fatal(err)
	}
	if !validDirectOAuthState(state) || validDirectOAuthState(state+"x") {
		t.Fatal("OAuth state validation did not require canonical 32-byte base64url")
	}
}

func TestDirectAutoLaunchAmbiguousSuccessIsReconciledWithoutSecondWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	application, storage, provider, owner, workspace, connection, clock :=
		newDirectAppFixture(t, ctx, true)
	campaign := createDirectAppCampaign(t, ctx, application, owner, workspace.ID, *clock)
	if _, err := application.SubmitDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
	); err != nil {
		t.Fatal(err)
	}
	provider.campaign.Status = "ACCEPTED"
	provider.campaign.State = "OFF"
	application.RunDirectAutoLaunchOnce(ctx, 10)
	campaign, err := storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil || campaign.Status != "accepted" || campaign.ProviderCampaignID == nil {
		t.Fatalf("accepted campaign before consent = %#v, %v", campaign, err)
	}
	if _, err := application.GrantDirectAutoLaunchConsent(
		ctx, owner, workspace.ID, campaign.ID, store.DirectConsentRequest{
			Confirmation:         "АВТОЗАПУСК",
			ExpectedVersion:      campaign.Version,
			ExpectedConnectionID: connection.ID,
			ExpectedAccountID:    connection.AccountID,
			ExpectedCampaignName: campaign.Name,
			ExpectedProviderID:   *campaign.ProviderCampaignID,
			WeeklyBudgetMinor:    campaign.WeeklyBudgetMinor,
			StartsAt:             campaign.StartsAt,
			EndsAt:               campaign.EndsAt,
		},
	); err != nil {
		t.Fatal(err)
	}
	// Direct applied Resume but the HTTP result was ambiguous. The local state
	// must stay reconciling and the worker must poll before doing anything else.
	provider.resume = func(provider *fakeDirectProvider) error {
		provider.campaign.State = "ON"
		return context.DeadlineExceeded
	}
	// Provider polling and the launch queue use separate leases, so one worker
	// cycle can observe acceptance and then persist the ambiguous launch.
	application.RunDirectAutoLaunchOnce(ctx, 10)
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if provider.resumeCalls != 1 || campaign.LaunchState != "reconciling" ||
		campaign.Status != "accepted" {
		t.Fatalf("ambiguous launch: calls=%d campaign=%#v", provider.resumeCalls, campaign)
	}

	// The emergency write kill-switch must not disable read-only recovery.
	if err := application.SetDirectFeatureFlags(false, false); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(time.Minute)
	application.RunDirectAutoLaunchOnce(ctx, 10)
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if provider.resumeCalls != 1 || campaign.Status != "active" ||
		campaign.LaunchState != "confirmed" || campaign.ProviderState != "ON" {
		t.Fatalf("reconciled launch: calls=%d campaign=%#v", provider.resumeCalls, campaign)
	}
}

func TestDirectReconciliationConfirmsProviderONAndRecordsSnapshotDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	application, storage, provider, owner, workspace, connection, clock :=
		newDirectAppFixture(t, ctx, true)
	campaign := createDirectAppCampaign(t, ctx, application, owner, workspace.ID, *clock)
	if _, err := application.SubmitDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
	); err != nil {
		t.Fatal(err)
	}
	provider.campaign.Status = "ACCEPTED"
	provider.campaign.State = "OFF"
	application.RunDirectAutoLaunchOnce(ctx, 10)
	campaign, err := storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.GrantDirectAutoLaunchConsent(
		ctx, owner, workspace.ID, campaign.ID, store.DirectConsentRequest{
			Confirmation:         "АВТОЗАПУСК",
			ExpectedVersion:      campaign.Version,
			ExpectedConnectionID: connection.ID,
			ExpectedAccountID:    connection.AccountID,
			ExpectedCampaignName: campaign.Name,
			ExpectedProviderID:   *campaign.ProviderCampaignID,
			WeeklyBudgetMinor:    campaign.WeeklyBudgetMinor,
			StartsAt:             campaign.StartsAt,
			EndsAt:               campaign.EndsAt,
		},
	); err != nil {
		t.Fatal(err)
	}
	provider.resume = func(provider *fakeDirectProvider) error {
		provider.campaign.State = "ON"
		return context.DeadlineExceeded
	}
	application.RunDirectAutoLaunchOnce(ctx, 10)
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil || campaign.LaunchState != "reconciling" {
		t.Fatalf("ambiguous launch was not retained: %#v, %v", campaign, err)
	}

	provider.campaign.Name = "Changed directly in Yandex after Resume"
	if err := application.SetDirectFeatureFlags(false, false); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(time.Minute)
	application.RunDirectAutoLaunchOnce(ctx, 10)
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if provider.resumeCalls != 1 || campaign.Status != "active" ||
		campaign.LaunchState != "confirmed" || campaign.ProviderState != "ON" ||
		campaign.LaunchFailureCode != "provider_snapshot_mismatch" {
		t.Fatalf("provider ON truth or snapshot warning was lost: calls=%d campaign=%#v",
			provider.resumeCalls, campaign)
	}
}

func TestDirectAutomationIsIsolatedFromCoreScheduler(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	application, storage, provider, owner, workspace, connection, clock :=
		newDirectAppFixture(t, ctx, true)
	campaign := createDirectAppCampaign(t, ctx, application, owner, workspace.ID, *clock)
	campaign, err := application.SubmitDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
	)
	if err != nil {
		t.Fatal(err)
	}
	provider.campaign.Status = "ACCEPTED"
	provider.campaign.State = "OFF"
	campaign, err = storage.SyncDirectCampaignProviderStatus(
		ctx, workspace.ID, campaign.ID, *campaign.ProviderCampaignID,
		provider.campaign.Status, provider.campaign.State, *clock,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.GrantDirectAutoLaunchConsent(
		ctx, owner, workspace.ID, campaign.ID, store.DirectConsentRequest{
			Confirmation:         "АВТОЗАПУСК",
			ExpectedVersion:      campaign.Version,
			ExpectedConnectionID: connection.ID,
			ExpectedAccountID:    connection.AccountID,
			ExpectedCampaignName: campaign.Name,
			ExpectedProviderID:   *campaign.ProviderCampaignID,
			WeeklyBudgetMinor:    campaign.WeeklyBudgetMinor,
			StartsAt:             campaign.StartsAt,
			EndsAt:               campaign.EndsAt,
		},
	); err != nil {
		t.Fatal(err)
	}
	application.runSchedulerCycle(ctx, *clock)
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if provider.resumeCalls != 0 || campaign.Status != "accepted" {
		t.Fatalf("core scheduler executed Direct work: calls=%d campaign=%#v",
			provider.resumeCalls, campaign)
	}

	application.runDirectAutomationCycle(ctx)
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if provider.resumeCalls != 1 || campaign.Status != "active" ||
		campaign.LaunchState != "confirmed" {
		t.Fatalf("Direct worker did not execute its cycle: calls=%d campaign=%#v",
			provider.resumeCalls, campaign)
	}
}

func TestDirectAutomationCycleBoundsProviderContext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	application, _, provider, owner, workspace, _, clock :=
		newDirectAppFixture(t, ctx, false)
	campaign := createDirectAppCampaign(t, ctx, application, owner, workspace.ID, *clock)
	if _, err := application.SubmitDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
	); err != nil {
		t.Fatal(err)
	}
	provider.campaign.Status = "ACCEPTED"
	provider.campaign.State = "OFF"
	var observedDeadline time.Time
	provider.getContext = func(providerCtx context.Context) {
		observedDeadline, _ = providerCtx.Deadline()
	}

	startedAt := time.Now()
	application.runDirectAutomationCycle(ctx)
	if observedDeadline.IsZero() {
		t.Fatal("Direct provider call did not receive a deadline")
	}
	if maximum := startedAt.Add(directAutomationCycleTimeout + time.Second); observedDeadline.After(maximum) {
		t.Fatalf("Direct cycle deadline = %v, want no later than %v", observedDeadline, maximum)
	}
}

func TestDirectAutoLaunchInvalidatesConsentOnProviderStrategyDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	application, storage, provider, owner, workspace, connection, clock :=
		newDirectAppFixture(t, ctx, true)
	campaign := createDirectAppCampaign(t, ctx, application, owner, workspace.ID, *clock)
	if _, err := application.SubmitDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
	); err != nil {
		t.Fatal(err)
	}
	provider.campaign.Status = "ACCEPTED"
	provider.campaign.State = "OFF"
	application.RunDirectAutoLaunchOnce(ctx, 10)
	campaign, err := storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.GrantDirectAutoLaunchConsent(
		ctx, owner, workspace.ID, campaign.ID, store.DirectConsentRequest{
			Confirmation: "АВТОЗАПУСК", ExpectedVersion: campaign.Version,
			ExpectedConnectionID: connection.ID, ExpectedAccountID: connection.AccountID,
			ExpectedCampaignName: campaign.Name, ExpectedProviderID: *campaign.ProviderCampaignID,
			WeeklyBudgetMinor: campaign.WeeklyBudgetMinor,
			StartsAt:          campaign.StartsAt, EndsAt: campaign.EndsAt,
		},
	); err != nil {
		t.Fatal(err)
	}
	provider.getErr = &yandexdirect.Error{Code: "campaign_budget_unavailable"}
	application.RunDirectAutoLaunchOnce(ctx, 10)
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if provider.resumeCalls != 0 || campaign.AutoLaunch.Valid ||
		campaign.AutoLaunch.InvalidReason != "provider_strategy_changed" {
		t.Fatalf("strategy drift did not invalidate auto consent: calls=%d campaign=%#v",
			provider.resumeCalls, campaign)
	}
}

func TestDirectAutoLaunchInvalidatesConsentOnProviderSnapshotDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	application, storage, provider, owner, workspace, connection, clock :=
		newDirectAppFixture(t, ctx, true)
	campaign := createDirectAppCampaign(t, ctx, application, owner, workspace.ID, *clock)
	if _, err := application.SubmitDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
	); err != nil {
		t.Fatal(err)
	}
	provider.campaign.Status = "ACCEPTED"
	provider.campaign.State = "OFF"
	application.RunDirectAutoLaunchOnce(ctx, 10)
	campaign, err := storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.GrantDirectAutoLaunchConsent(
		ctx, owner, workspace.ID, campaign.ID, store.DirectConsentRequest{
			Confirmation:         "АВТОЗАПУСК",
			ExpectedVersion:      campaign.Version,
			ExpectedConnectionID: connection.ID,
			ExpectedAccountID:    connection.AccountID,
			ExpectedCampaignName: campaign.Name,
			ExpectedProviderID:   *campaign.ProviderCampaignID,
			WeeklyBudgetMinor:    campaign.WeeklyBudgetMinor,
			StartsAt:             campaign.StartsAt,
			EndsAt:               campaign.EndsAt,
		},
	); err != nil {
		t.Fatal(err)
	}

	provider.campaign.Name = "Changed directly in Yandex"
	application.RunDirectAutoLaunchOnce(ctx, 10)
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if provider.resumeCalls != 0 || campaign.LaunchFailureCode != "provider_snapshot_mismatch" ||
		campaign.AutoLaunch.Valid ||
		campaign.AutoLaunch.InvalidReason != "provider_snapshot_changed" {
		t.Fatalf("provider snapshot drift was not made explicit: calls=%d campaign=%#v",
			provider.resumeCalls, campaign)
	}
}

func TestDirectManualLaunchNeedsNeitherAutoFlagNorConsentAndIsWorkspaceScoped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	application, storage, provider, owner, workspace, _, clock :=
		newDirectAppFixture(t, ctx, false)
	campaign := createDirectAppCampaign(t, ctx, application, owner, workspace.ID, *clock)
	campaign, err := application.SubmitDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
	)
	if err != nil {
		t.Fatal(err)
	}
	if campaign.Status != "provider_draft" {
		t.Fatalf("Campaigns.add result status = %q, want provider_draft", campaign.Status)
	}
	provider.campaign.Status = "ACCEPTED"
	provider.campaign.State = "OFF"
	otherWorkspace, err := storage.CreateWorkspace(ctx, owner, store.Workspace{Name: "Other team"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.LaunchDirectCampaign(
		ctx, owner, otherWorkspace.ID, campaign.ID, campaign.Version,
	); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-workspace launch error = %v, want ErrNotFound", err)
	}
	campaign, err = application.LaunchDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
	)
	if err != nil {
		t.Fatal(err)
	}
	if provider.resumeCalls != 1 || campaign.Status != "active" ||
		campaign.LaunchMode != "manual" {
		t.Fatalf("manual launch: calls=%d campaign=%#v", provider.resumeCalls, campaign)
	}
}

func TestDirectUnauthorizedResumeMarksConnectionErrorAndReleasesClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	application, storage, provider, owner, workspace, _, clock :=
		newDirectAppFixture(t, ctx, false)
	campaign := createDirectAppCampaign(t, ctx, application, owner, workspace.ID, *clock)
	campaign, err := application.SubmitDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
	)
	if err != nil {
		t.Fatal(err)
	}
	provider.campaign.Status = "ACCEPTED"
	provider.campaign.State = "OFF"
	provider.resume = func(*fakeDirectProvider) error {
		return &yandexdirect.Error{
			StatusCode: http.StatusUnauthorized,
			Code:       "authorization_required",
		}
	}
	if _, err := application.LaunchDirectCampaign(
		ctx, owner, workspace.ID, campaign.ID, campaign.Version,
	); !errors.Is(err, ErrDirectProvider) {
		t.Fatalf("unauthorized launch error = %v, want ErrDirectProvider", err)
	}
	campaign, err = storage.GetDirectCampaign(ctx, owner, workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if provider.resumeCalls != 1 || campaign.Status != "accepted" ||
		campaign.LaunchState != "idle" ||
		campaign.LaunchFailureCode != "authorization_required" {
		t.Fatalf("unauthorized Resume left an ambiguous launch: calls=%d campaign=%#v",
			provider.resumeCalls, campaign)
	}
	connection, err := storage.GetDirectConnection(ctx, owner, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if connection.Status != "error" || connection.ErrorCode != "authorization_required" {
		t.Fatalf("expired connection = %#v", connection)
	}
	if err := application.RevokeDirectConnection(ctx, owner, workspace.ID); err != nil {
		t.Fatalf("explicit disconnect after authoritative 401: %v", err)
	}
}

func TestDirectProviderAuthorizationErrorRequiresAuthoritativeProviderSignal(t *testing.T) {
	t.Parallel()
	if !directProviderAuthorizationError(fmt.Errorf(
		"wrapped: %w", &yandexdirect.Error{StatusCode: http.StatusUnauthorized},
	)) {
		t.Fatal("wrapped HTTP 401 was not recognized")
	}
	if !directProviderAuthorizationError(&yandexdirect.Error{APIErrorCode: 53}) {
		t.Fatal("Yandex Direct invalid-token error 53 was not recognized")
	}
	if directProviderAuthorizationError(
		&yandexdirect.Error{StatusCode: http.StatusForbidden, Code: "authorization_required"},
	) {
		t.Fatal("non-401 provider error was treated as an expired credential")
	}
	if directProviderAuthorizationError(context.DeadlineExceeded) {
		t.Fatal("ambiguous transport error was treated as an expired credential")
	}
}

func newDirectAppFixture(
	t *testing.T, ctx context.Context, autoLaunch bool,
) (*App, *store.Store, *fakeDirectProvider, string, store.Workspace, store.DirectConnection, *time.Time) {
	t.Helper()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "direct-app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	owner := "direct-app-owner"
	if err := storage.UpsertUser(ctx, store.User{ID: owner, DisplayName: owner}); err != nil {
		t.Fatal(err)
	}
	var workspace store.Workspace
	workspaces, err := storage.ListWorkspaces(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	for _, access := range workspaces {
		if access.Workspace.IsPersonal {
			workspace = access.Workspace
			break
		}
	}
	if workspace.ID == "" {
		t.Fatal("personal workspace is missing")
	}
	provider := &fakeDirectProvider{
		account: yandexdirect.Account{
			ID: "account-123", Login: "direct-login", DisplayName: "Direct account",
			CurrencyCode: "RUB", Timezone: "Europe/Moscow",
		},
		campaign: yandexdirect.Campaign{ID: 98_001},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application := New(storage, nil, nil, nil, nil, logger)
	if err := application.ConfigureDirect(provider, []byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	if err := application.SetDirectFeatureFlags(true, autoLaunch); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2043, time.June, 7, 12, 0, 0, 0, time.UTC)
	application.now = func() time.Time { return now }
	ciphertext, err := application.directCipher.seal(
		workspace.ID, provider.account.ID, provider.account.Login, "access-token",
	)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := storage.ReplaceDirectConnection(ctx, owner, workspace.ID, store.DirectConnection{
		AccountID: provider.account.ID, ClientLogin: provider.account.Login,
		AccountName: provider.account.DisplayName, CurrencyCode: provider.account.CurrencyCode,
		Timezone: provider.account.Timezone, TokenCiphertext: ciphertext,
		TokenKeyVersion: 1, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return application, storage, provider, owner, workspace, connection, &now
}

func createDirectAppCampaign(
	t *testing.T, ctx context.Context, application *App, owner, workspaceID string, now time.Time,
) store.DirectCampaign {
	t.Helper()
	campaign, err := application.CreateDirectCampaign(ctx, owner, workspaceID, store.DirectCampaign{
		Name: "Provider truth campaign", Objective: "traffic",
		LandingURL: "https://maxposty.ru/", Brief: "Promote the channel",
		Regions: []string{"225"}, WeeklyBudgetMinor: 30_000,
		StartsAt: now, EndsAt: now.AddDate(0, 1, 0), CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return campaign
}
