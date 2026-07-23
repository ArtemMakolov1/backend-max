package app

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexdirect"
)

const (
	directTokenCipherPrefix = "v1."
	directOAuthStateTTL     = 10 * time.Minute
)

var (
	ErrDirectNotConfigured    = errors.New("integration with Yandex Direct is not configured")
	ErrDirectWritesDisabled   = errors.New("writes to Yandex Direct are disabled")
	ErrDirectAutoLaunchOff    = errors.New("auto-launch for Yandex Direct is disabled")
	ErrDirectProvider         = errors.New("provider request to Yandex Direct failed")
	ErrDirectSnapshotMismatch = errors.New("provider campaign in Yandex Direct does not match the authorized snapshot")
	ErrDirectOAuthInvalid     = errors.New("invalid Yandex Direct OAuth completion")
	ErrDirectOAuthFlow        = errors.New("Yandex Direct OAuth completion flow does not match the configured redirect")
)

type DirectProvider interface {
	OAuthFlow() yandexdirect.OAuthFlow
	AuthorizationURL(state, codeChallenge string) string
	ExchangeCode(context.Context, string, string) (string, error)
	GetAccount(context.Context, string, string) (yandexdirect.Account, error)
	CreateCampaignDraft(context.Context, string, string, yandexdirect.CampaignDraft) (yandexdirect.Campaign, error)
	GetCampaign(context.Context, string, string, int64) (yandexdirect.Campaign, error)
	ResumeCampaign(context.Context, string, string, int64) error
	Sandbox() bool
}

type DirectCampaignSuggester interface {
	SuggestDirectCampaign(
		context.Context, openairesearch.SuggestDirectCampaignRequest,
	) (openairesearch.SuggestDirectCampaignResult, error)
}

type directTokenCipher struct {
	aead cipher.AEAD
}

type DirectOAuthStart struct {
	AuthorizationURL string                 `json:"authorization_url"`
	ExpiresAt        time.Time              `json:"expires_at"`
	Flow             yandexdirect.OAuthFlow `json:"flow"`
	State            string                 `json:"state,omitempty"`
}

type DirectOAuthCompletion struct {
	WorkspaceID string
	ReturnTo    string
	Connection  store.DirectConnection
}

type DirectIntegrationStatus struct {
	Configured        bool
	WritesEnabled     bool
	AutoLaunchEnabled bool
	Sandbox           bool
	Connected         bool
	Connection        *store.DirectConnection
}

func (a *App) ConfigureDirect(provider DirectProvider, dataKey []byte) error {
	if provider == nil {
		return errors.New("provider for Yandex Direct is required")
	}
	if len(dataKey) != 32 {
		return errors.New("token data key for Yandex Direct must contain exactly 32 bytes")
	}
	if flow := provider.OAuthFlow(); flow != yandexdirect.OAuthFlowCallback &&
		flow != yandexdirect.OAuthFlowVerificationCode {
		return errors.New("provider for Yandex Direct returned an unsupported OAuth flow")
	}
	block, err := aes.NewCipher(dataKey)
	if err != nil {
		return fmt.Errorf("initialize Yandex Direct encryption: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("initialize Yandex Direct authenticated encryption: %w", err)
	}
	a.direct = provider
	a.directCipher = &directTokenCipher{aead: aead}
	a.directSandbox = provider.Sandbox()
	// Provider credentials alone never enable a spend-capable write.
	a.directWritesEnabled = false
	a.directAutoLaunchEnabled = false
	return nil
}

func (a *App) SetDirectFeatureFlags(writesEnabled, autoLaunchEnabled bool) error {
	if (writesEnabled || autoLaunchEnabled) && !a.DirectConfigured() {
		return ErrDirectNotConfigured
	}
	if autoLaunchEnabled && !writesEnabled {
		return errors.New("auto-launch for Yandex Direct requires writes to be enabled")
	}
	a.directWritesEnabled = writesEnabled
	a.directAutoLaunchEnabled = autoLaunchEnabled
	return nil
}

func (a *App) DirectConfigured() bool {
	return a != nil && a.direct != nil && a.directCipher != nil
}

func (a *App) DirectWritesEnabled() bool {
	return a != nil && a.DirectConfigured() && a.directWritesEnabled
}

func (a *App) DirectAutoLaunchEnabled() bool {
	return a != nil && a.DirectWritesEnabled() && a.directAutoLaunchEnabled
}

func (a *App) DirectCampaignSuggestionConfigured() bool {
	if a == nil || a.research == nil {
		return false
	}
	_, ok := a.research.(DirectCampaignSuggester)
	return ok
}

func (a *App) GetDirectIntegrationStatus(
	ctx context.Context, actorUserID, workspaceID string,
) (DirectIntegrationStatus, error) {
	status := DirectIntegrationStatus{
		Configured: a.DirectConfigured(), WritesEnabled: a.DirectWritesEnabled(),
		AutoLaunchEnabled: a.DirectAutoLaunchEnabled(), Sandbox: a.directSandbox,
	}
	connection, err := a.store.GetDirectConnection(ctx, actorUserID, workspaceID)
	if errors.Is(err, store.ErrNotFound) {
		return status, nil
	}
	if err != nil {
		return DirectIntegrationStatus{}, err
	}
	connection.TokenCiphertext = ""
	connection.TokenKeyVersion = 0
	status.Connected = connection.Status == "active" || connection.Status == "error"
	status.Connection = &connection
	return status, nil
}

func (a *App) StartDirectOAuth(
	ctx context.Context, actorUserID, workspaceID, sessionBinding, clientLogin, returnTo string,
) (DirectOAuthStart, error) {
	if !a.DirectConfigured() {
		return DirectOAuthStart{}, ErrDirectNotConfigured
	}
	flow := a.direct.OAuthFlow()
	if !validDirectOAuthSessionBinding(sessionBinding) ||
		(flow != yandexdirect.OAuthFlowCallback && flow != yandexdirect.OAuthFlowVerificationCode) {
		return DirectOAuthStart{}, ErrDirectOAuthInvalid
	}
	state, err := randomDirectToken(32)
	if err != nil {
		return DirectOAuthStart{}, err
	}
	verifier, err := randomDirectToken(32)
	if err != nil {
		return DirectOAuthStart{}, err
	}
	now := a.now().UTC()
	expiresAt := now.Add(directOAuthStateTTL)
	returnTo = safeDirectReturnTo(returnTo)
	if err := a.store.CreateDirectOAuthState(ctx, actorUserID, workspaceID, store.DirectOAuthState{
		StateHash: directOAuthStateHash(flow, sessionBinding, state), PKCEVerifier: verifier,
		ClientLogin: strings.TrimSpace(clientLogin), ReturnTo: returnTo,
		CreatedAt: now, ExpiresAt: expiresAt,
	}); err != nil {
		return DirectOAuthStart{}, err
	}
	challengeBytes := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeBytes[:])
	result := DirectOAuthStart{
		AuthorizationURL: a.direct.AuthorizationURL(state, challenge),
		ExpiresAt:        expiresAt, Flow: flow,
	}
	if flow == yandexdirect.OAuthFlowVerificationCode {
		result.State = state
	}
	return result, nil
}

func (a *App) CompleteDirectOAuthCallback(
	ctx context.Context, actorUserID, sessionBinding, state, code string,
) (DirectOAuthCompletion, error) {
	return a.completeDirectOAuth(
		ctx, actorUserID, "", sessionBinding, yandexdirect.OAuthFlowCallback, state, code,
	)
}

func (a *App) CompleteDirectOAuthVerification(
	ctx context.Context, actorUserID, workspaceID, sessionBinding, state, code string,
) (DirectOAuthCompletion, error) {
	return a.completeDirectOAuth(
		ctx, actorUserID, workspaceID, sessionBinding,
		yandexdirect.OAuthFlowVerificationCode, state, code,
	)
}

func (a *App) completeDirectOAuth(
	ctx context.Context, actorUserID, workspaceID, sessionBinding string,
	flow yandexdirect.OAuthFlow, state, code string,
) (DirectOAuthCompletion, error) {
	if !a.DirectConfigured() {
		return DirectOAuthCompletion{}, ErrDirectNotConfigured
	}
	if a.direct.OAuthFlow() != flow {
		return DirectOAuthCompletion{}, ErrDirectOAuthFlow
	}
	if flow == yandexdirect.OAuthFlowVerificationCode {
		if state != strings.TrimSpace(state) || code != strings.TrimSpace(code) {
			return DirectOAuthCompletion{}, ErrDirectOAuthInvalid
		}
	} else {
		state, code = strings.TrimSpace(state), strings.TrimSpace(code)
	}
	if !validDirectOAuthSessionBinding(sessionBinding) ||
		!validDirectOAuthState(state) || !validDirectOAuthCode(flow, code) {
		return DirectOAuthCompletion{}, ErrDirectOAuthInvalid
	}
	stateHash := directOAuthStateHash(flow, sessionBinding, state)
	var (
		stored store.DirectOAuthState
		err    error
	)
	if workspaceID == "" {
		stored, err = a.store.ConsumeDirectOAuthState(
			ctx, actorUserID, stateHash, a.now().UTC(),
		)
	} else {
		stored, err = a.store.ConsumeDirectOAuthStateForWorkspace(
			ctx, actorUserID, workspaceID, stateHash, a.now().UTC(),
		)
	}
	if err != nil {
		return DirectOAuthCompletion{}, err
	}
	token, err := a.direct.ExchangeCode(ctx, code, stored.PKCEVerifier)
	if err != nil {
		return DirectOAuthCompletion{}, fmt.Errorf("%w: %w", ErrDirectProvider, err)
	}
	account, err := a.direct.GetAccount(ctx, token, stored.ClientLogin)
	if err != nil {
		return DirectOAuthCompletion{}, fmt.Errorf("%w: %w", ErrDirectProvider, err)
	}
	clientLogin := strings.TrimSpace(stored.ClientLogin)
	if clientLogin == "" {
		clientLogin = strings.TrimSpace(account.Login)
	}
	ciphertext, err := a.directCipher.seal(
		stored.WorkspaceID, account.ID, clientLogin, token,
	)
	if err != nil {
		return DirectOAuthCompletion{}, err
	}
	connection, err := a.store.ReplaceDirectConnectionFromOAuthAttempt(
		ctx, actorUserID, stored.WorkspaceID, stored.StateHash, store.DirectConnection{
			AccountID: account.ID, ClientLogin: clientLogin, AccountName: account.DisplayName,
			CurrencyCode: account.CurrencyCode, Timezone: account.Timezone, ReadOnly: account.ReadOnly,
			TokenCiphertext: ciphertext, TokenKeyVersion: 1, CreatedAt: a.now().UTC(),
		})
	if err != nil {
		return DirectOAuthCompletion{}, err
	}
	return DirectOAuthCompletion{
		WorkspaceID: stored.WorkspaceID, ReturnTo: stored.ReturnTo, Connection: connection,
	}, nil
}

func (a *App) RevokeDirectConnection(
	ctx context.Context, actorUserID, workspaceID string,
) error {
	return a.store.RevokeDirectConnection(ctx, actorUserID, workspaceID, a.now().UTC())
}

func (a *App) CreateDirectCampaign(
	ctx context.Context, actorUserID, workspaceID string, campaign store.DirectCampaign,
) (store.DirectCampaign, error) {
	return a.store.CreateDirectCampaign(ctx, actorUserID, workspaceID, campaign)
}

func (a *App) UpdateDirectCampaignDraft(
	ctx context.Context, actorUserID, workspaceID, campaignID string,
	changes store.DirectCampaignChanges,
) (store.DirectCampaign, error) {
	return a.store.UpdateDirectCampaignDraft(ctx, actorUserID, workspaceID, campaignID, changes)
}

func (a *App) SubmitDirectCampaign(
	ctx context.Context, actorUserID, workspaceID, campaignID string, expectedVersion int64,
) (store.DirectCampaign, error) {
	if !a.DirectWritesEnabled() {
		return store.DirectCampaign{}, ErrDirectWritesDisabled
	}
	campaign, connection, err := a.store.ClaimDirectCampaignSubmission(
		ctx, actorUserID, workspaceID, campaignID, expectedVersion, a.now().UTC(),
	)
	if err != nil {
		return store.DirectCampaign{}, err
	}
	token, err := a.directCipher.open(
		connection.WorkspaceID, connection.AccountID, connection.ClientLogin, connection.TokenCiphertext,
	)
	if err != nil {
		_ = a.store.FailDirectCampaignSubmission(ctx, workspaceID, campaignID, "token_decryption_failed", a.now().UTC())
		return store.DirectCampaign{}, err
	}
	providerCampaign, err := a.direct.CreateCampaignDraft(ctx, token, connection.ClientLogin, yandexdirect.CampaignDraft{
		Name: campaign.Name, WeeklyBudgetMinor: campaign.WeeklyBudgetMinor,
		StartsAt: campaign.StartsAt, EndsAt: campaign.EndsAt,
	})
	if err != nil {
		now := a.now().UTC()
		connectionErr := a.markDirectConnectionAuthorizationRequired(
			ctx, connection, err, now,
		)
		failureErr := a.store.FailDirectCampaignSubmission(
			ctx, workspaceID, campaignID, directProviderErrorCode(err), now,
		)
		return store.DirectCampaign{}, errors.Join(
			fmt.Errorf("%w: %w", ErrDirectProvider, err), connectionErr, failureErr,
		)
	}
	submitted, err := a.store.MarkDirectCampaignSubmitted(
		ctx, actorUserID, workspaceID, campaignID, expectedVersion, providerCampaign.ID,
		providerCampaign.Status, providerCampaign.State, a.now().UTC(),
	)
	if err != nil {
		// Fail closed: the creating claim remains and this provider campaign is
		// never created again automatically. An operator must reconcile its ID.
		a.logger.Error("Yandex Direct campaign created but local submission finalization failed",
			"workspace_id", workspaceID, "campaign_id", campaignID,
			"provider_campaign_id", providerCampaign.ID, "error", err)
		return store.DirectCampaign{}, err
	}
	return submitted, nil
}

func (a *App) GrantDirectAutoLaunchConsent(
	ctx context.Context, actorUserID, workspaceID, campaignID string,
	request store.DirectConsentRequest,
) (store.DirectCampaign, error) {
	if !a.DirectAutoLaunchEnabled() {
		return store.DirectCampaign{}, ErrDirectAutoLaunchOff
	}
	request.AuthorizedAt = a.now().UTC()
	if _, err := a.store.GrantDirectAutoLaunchConsent(
		ctx, actorUserID, workspaceID, campaignID, request,
	); err != nil {
		return store.DirectCampaign{}, err
	}
	return a.store.GetDirectCampaign(ctx, actorUserID, workspaceID, campaignID)
}

func (a *App) RevokeDirectAutoLaunchConsent(
	ctx context.Context, actorUserID, workspaceID, campaignID string,
) (store.DirectCampaign, error) {
	if err := a.store.RevokeDirectAutoLaunchConsent(
		ctx, actorUserID, workspaceID, campaignID, a.now().UTC(),
	); err != nil {
		return store.DirectCampaign{}, err
	}
	return a.store.GetDirectCampaign(ctx, actorUserID, workspaceID, campaignID)
}

func (a *App) LaunchDirectCampaign(
	ctx context.Context, actorUserID, workspaceID, campaignID string, expectedVersion int64,
) (store.DirectCampaign, error) {
	if !a.DirectWritesEnabled() {
		return store.DirectCampaign{}, ErrDirectWritesDisabled
	}
	material, err := a.store.GetDirectManualLaunchMaterial(
		ctx, actorUserID, workspaceID, campaignID,
	)
	if err != nil {
		return store.DirectCampaign{}, err
	}
	if expectedVersion <= 0 || material.Campaign.Version != expectedVersion {
		return store.DirectCampaign{}, store.ErrConflict
	}
	if err := a.launchDirectCampaign(
		ctx, material, false, func(providerCampaign yandexdirect.Campaign) (store.DirectLaunchMaterial, error) {
			return a.store.ClaimDirectManualCampaignLaunch(
				ctx, actorUserID, workspaceID, campaignID, expectedVersion,
				providerCampaign.ID, material.Connection.AccountID,
				providerCampaign.WeeklyBudgetMinor, providerCampaign.StartsAt,
				providerCampaign.EndsAt, a.now().UTC(),
			)
		},
	); err != nil {
		return store.DirectCampaign{}, err
	}
	return a.store.GetDirectCampaign(ctx, actorUserID, workspaceID, campaignID)
}

func (a *App) launchDirectAutoCampaign(
	ctx context.Context, campaignID string,
) error {
	material, err := a.store.GetDirectAutoLaunchMaterial(ctx, campaignID)
	if err != nil {
		return err
	}
	return a.launchDirectCampaign(
		ctx, material, true, func(providerCampaign yandexdirect.Campaign) (store.DirectLaunchMaterial, error) {
			return a.store.ClaimDirectAutoCampaignLaunch(
				ctx, material.Campaign.WorkspaceID, campaignID, material.Campaign.Version,
				providerCampaign.ID, material.Connection.AccountID,
				providerCampaign.WeeklyBudgetMinor, providerCampaign.StartsAt,
				providerCampaign.EndsAt, a.now().UTC(),
			)
		},
	)
}

func (a *App) launchDirectCampaign(
	ctx context.Context, material store.DirectLaunchMaterial, autoLaunch bool,
	claim func(yandexdirect.Campaign) (store.DirectLaunchMaterial, error),
) error {
	if material.Campaign.ProviderCampaignID == nil {
		return store.ErrDirectCampaignNotAccepted
	}
	token, err := a.directCipher.open(
		material.Connection.WorkspaceID, material.Connection.AccountID,
		material.Connection.ClientLogin, material.TokenCiphertext,
	)
	if err != nil {
		return err
	}
	providerCampaign, err := a.direct.GetCampaign(
		ctx, token, material.Connection.ClientLogin, *material.Campaign.ProviderCampaignID,
	)
	if err != nil {
		now := a.now().UTC()
		connectionErr := a.markDirectConnectionAuthorizationRequired(
			ctx, material.Connection, err, now,
		)
		if autoLaunch && directProviderStrategySnapshotError(err) {
			if invalidateErr := a.store.InvalidateDirectAutoLaunchConsent(
				ctx, material.Campaign.WorkspaceID, material.Campaign.ID,
				"provider_strategy_changed", now,
			); invalidateErr != nil {
				return errors.Join(ErrDirectSnapshotMismatch, connectionErr, invalidateErr)
			}
			return errors.Join(ErrDirectSnapshotMismatch, connectionErr)
		}
		return errors.Join(fmt.Errorf("%w: %w", ErrDirectProvider, err), connectionErr)
	}
	snapshotMatches := directProviderSnapshotMatches(providerCampaign, material.Campaign)
	if _, err := a.store.SyncDirectCampaignProviderStatus(
		ctx, material.Campaign.WorkspaceID, material.Campaign.ID, providerCampaign.ID,
		providerCampaign.Status, providerCampaign.State, a.now().UTC(),
	); err != nil {
		return err
	}
	if err := a.store.SetDirectCampaignProviderSnapshotMismatch(
		ctx, material.Campaign.WorkspaceID, material.Campaign.ID,
		!snapshotMatches, a.now().UTC(),
	); err != nil {
		return err
	}
	if directProviderCampaignRunning(providerCampaign) {
		// The user may have launched the campaign directly in Yandex. Reflect
		// provider truth and never send a duplicate Resume.
		if !snapshotMatches {
			return ErrDirectSnapshotMismatch
		}
		return nil
	}
	if providerCampaign.Status != "ACCEPTED" {
		return store.ErrDirectCampaignNotAccepted
	}
	if !snapshotMatches {
		// SetDirectCampaignProviderSnapshotMismatch already invalidated any
		// outstanding consent in the same transaction as the clarification.
		return ErrDirectSnapshotMismatch
	}
	if !directProviderCampaignDefinitelyOff(providerCampaign) {
		if autoLaunch {
			reason := "provider_state_" +
				strings.ToLower(strings.TrimSpace(providerCampaign.State))
			if strings.HasSuffix(reason, "_") {
				reason = "provider_state_unknown"
			}
			if err := a.store.InvalidateDirectAutoLaunchConsent(
				ctx, material.Campaign.WorkspaceID, material.Campaign.ID,
				reason, a.now().UTC(),
			); err != nil {
				return err
			}
		}
		return store.ErrDirectCampaignNotAccepted
	}
	claimed, err := claim(providerCampaign)
	if err != nil {
		return err
	}
	return a.attemptClaimedDirectLaunch(ctx, token, claimed)
}

func (a *App) attemptClaimedDirectLaunch(
	ctx context.Context, token string, material store.DirectLaunchMaterial,
) error {
	workspaceID := material.Campaign.WorkspaceID
	campaignID := material.Campaign.ID
	if material.Campaign.ProviderCampaignID == nil {
		return store.ErrDirectCampaignNotAccepted
	}
	if err := a.store.MarkDirectCampaignLaunchAttempt(
		ctx, workspaceID, campaignID, a.now().UTC(),
	); err != nil {
		return err
	}
	err := a.direct.ResumeCampaign(
		ctx, token, material.Connection.ClientLogin, *material.Campaign.ProviderCampaignID,
	)
	if err != nil {
		now := a.now().UTC()
		if directProviderAuthorizationError(err) {
			// An explicit HTTP 401 or Direct API error 53 is an
			// authoritative rejection of this Resume request, unlike a
			// timeout whose provider outcome is ambiguous. Release the launch
			// claim so the expired credential cannot dead-lock an otherwise
			// quiescent campaign.
			connectionErr := a.markDirectConnectionAuthorizationRequired(
				ctx, material.Connection, err, now,
			)
			abortErr := a.store.AbortDirectCampaignLaunchForAuthorization(
				ctx, workspaceID, campaignID, now,
			)
			return errors.Join(
				fmt.Errorf("%w: %w", ErrDirectProvider, err), connectionErr, abortErr,
			)
		}
		reconcileErr := a.store.MarkDirectCampaignLaunchReconciling(
			ctx, workspaceID, campaignID, directProviderErrorCode(err), now,
		)
		if reconcileErr != nil {
			return errors.Join(fmt.Errorf("%w: %w", ErrDirectProvider, err), reconcileErr)
		}
		return fmt.Errorf("%w: %w", ErrDirectProvider, err)
	}
	// If this write fails after Direct accepted Resume, the durable launch claim
	// remains. The reconciliation worker will observe provider State=ON and
	// finish the local transition without issuing another write.
	return a.store.CompleteDirectCampaignLaunch(
		ctx, workspaceID, campaignID, a.now().UTC(),
	)
}

func (a *App) reconcileDirectCampaignLaunch(
	ctx context.Context, workspaceID, campaignID string, allowProviderRetry bool,
) error {
	material, err := a.store.GetDirectLaunchRecoveryMaterial(ctx, workspaceID, campaignID)
	if err != nil {
		return err
	}
	if material.Campaign.ProviderCampaignID == nil {
		return a.store.MarkDirectCampaignLaunchReconciling(
			ctx, workspaceID, campaignID, "provider_campaign_missing", a.now().UTC(),
		)
	}
	token, err := a.directCipher.open(
		material.Connection.WorkspaceID, material.Connection.AccountID,
		material.Connection.ClientLogin, material.TokenCiphertext,
	)
	if err != nil {
		return err
	}
	providerCampaign, err := a.direct.GetCampaign(
		ctx, token, material.Connection.ClientLogin, *material.Campaign.ProviderCampaignID,
	)
	if err != nil {
		now := a.now().UTC()
		connectionErr := a.markDirectConnectionAuthorizationRequired(
			ctx, material.Connection, err, now,
		)
		markErr := a.store.MarkDirectCampaignLaunchReconciling(
			ctx, workspaceID, campaignID, directProviderErrorCode(err), now,
		)
		return errors.Join(
			fmt.Errorf("%w: %w", ErrDirectProvider, err), connectionErr, markErr,
		)
	}
	if providerCampaign.ID != *material.Campaign.ProviderCampaignID {
		return a.store.MarkDirectCampaignLaunchReconciling(
			ctx, workspaceID, campaignID, "provider_campaign_mismatch", a.now().UTC(),
		)
	}
	snapshotMatches := directProviderSnapshotMatches(providerCampaign, material.Campaign)
	if _, err := a.store.SyncDirectCampaignProviderStatus(
		ctx, workspaceID, campaignID, providerCampaign.ID,
		providerCampaign.Status, providerCampaign.State, a.now().UTC(),
	); err != nil {
		return err
	}
	if err := a.store.SetDirectCampaignProviderSnapshotMismatch(
		ctx, workspaceID, campaignID, !snapshotMatches, a.now().UTC(),
	); err != nil {
		return err
	}
	if !snapshotMatches {
		// Provider ON is authoritative and SyncDirectCampaignProviderStatus has
		// already promoted the launch atomically. Keep the mismatch visible,
		// but never issue another Resume for a changed campaign.
		return ErrDirectSnapshotMismatch
	}
	if providerCampaign.Status != "ACCEPTED" {
		reason := "provider_status_mismatch"
		if directProviderCampaignRunning(providerCampaign) {
			reason = "provider_running_status_mismatch"
		}
		return a.store.MarkDirectCampaignLaunchReconciling(
			ctx, workspaceID, campaignID, reason, a.now().UTC(),
		)
	}
	if directProviderCampaignRunning(providerCampaign) {
		// The provider truth sync above completed launching/reconciling/failed
		// states atomically when Direct reported ACCEPTED + ON.
		return nil
	}
	if !directProviderCampaignDefinitelyOff(providerCampaign) {
		return a.store.MarkDirectCampaignLaunchReconciling(
			ctx, workspaceID, campaignID, "provider_state_ambiguous", a.now().UTC(),
		)
	}
	if !allowProviderRetry {
		return a.store.MarkDirectCampaignLaunchReconciling(
			ctx, workspaceID, campaignID, "provider_retry_disabled", a.now().UTC(),
		)
	}
	if material.Campaign.LaunchAttemptCount >= 2 {
		return a.store.FailDirectCampaignLaunch(
			ctx, workspaceID, campaignID, "provider_off_after_retries", a.now().UTC(),
		)
	}
	// A bounded retry is allowed only after an authoritative provider read
	// confirms that the campaign is still OFF. Further ambiguity is polled but
	// never produces another provider write.
	return a.attemptClaimedDirectLaunch(ctx, token, material)
}

func (a *App) RunDirectAutoLaunchOnce(ctx context.Context, limit int) {
	if !a.DirectConfigured() {
		return
	}
	if _, err := a.store.PurgeExpiredDirectOAuthStates(ctx, a.now().UTC(), 100); err != nil {
		a.logger.Error("could not purge expired Yandex Direct OAuth states", "error", err)
	}
	recoveryCandidates, err := a.store.ClaimDirectLaunchRecoveryCandidates(
		ctx, a.now().UTC(), limit,
	)
	if err != nil {
		a.logger.Error("could not list Yandex Direct launch reconciliation candidates", "error", err)
		return
	}
	for _, candidate := range recoveryCandidates {
		if err := a.reconcileDirectCampaignLaunch(
			ctx, candidate.WorkspaceID, candidate.CampaignID, a.DirectWritesEnabled(),
		); err != nil && !errors.Is(err, store.ErrDirectLaunchRetryExhausted) {
			a.logger.Error("Yandex Direct launch reconciliation failed",
				"workspace_id", candidate.WorkspaceID,
				"campaign_id", candidate.CampaignID, "error", err)
		}
	}
	lifecycleCandidates, err := a.store.ClaimDirectProviderSyncCandidates(
		ctx, a.now().UTC(), limit,
	)
	if err != nil {
		a.logger.Error("could not list Yandex Direct lifecycle candidates", "error", err)
		return
	}
	for _, candidate := range lifecycleCandidates {
		if err := a.syncDirectCampaignLifecycle(
			ctx, candidate.WorkspaceID, candidate.CampaignID,
		); err != nil {
			a.logger.Error("Yandex Direct lifecycle sync failed",
				"workspace_id", candidate.WorkspaceID,
				"campaign_id", candidate.CampaignID, "error", err)
		}
	}
	if !a.DirectAutoLaunchEnabled() {
		return
	}
	campaignIDs, err := a.store.ClaimDirectAutoLaunchCandidates(ctx, a.now().UTC(), limit)
	if err != nil {
		a.logger.Error("could not list Yandex Direct auto-launch candidates", "error", err)
		return
	}
	for _, campaignID := range campaignIDs {
		if err := a.launchDirectAutoCampaign(ctx, campaignID); err != nil &&
			!errors.Is(err, store.ErrDirectCampaignNotAccepted) &&
			!errors.Is(err, store.ErrDirectLaunchAlreadyClaimed) {
			a.logger.Error("Yandex Direct auto-launch failed", "campaign_id", campaignID, "error", err)
		}
	}
}

func (a *App) syncDirectCampaignLifecycle(
	ctx context.Context, workspaceID, campaignID string,
) error {
	material, err := a.store.GetDirectLifecycleMaterial(ctx, workspaceID, campaignID)
	if err != nil {
		return err
	}
	if material.Campaign.ProviderCampaignID == nil {
		return store.ErrNotFound
	}
	token, err := a.directCipher.open(
		material.Connection.WorkspaceID, material.Connection.AccountID,
		material.Connection.ClientLogin, material.TokenCiphertext,
	)
	if err != nil {
		return err
	}
	providerCampaign, err := a.direct.GetCampaign(
		ctx, token, material.Connection.ClientLogin, *material.Campaign.ProviderCampaignID,
	)
	if err != nil {
		connectionErr := a.markDirectConnectionAuthorizationRequired(
			ctx, material.Connection, err, a.now().UTC(),
		)
		return errors.Join(fmt.Errorf("%w: %w", ErrDirectProvider, err), connectionErr)
	}
	if providerCampaign.ID != *material.Campaign.ProviderCampaignID {
		return ErrDirectSnapshotMismatch
	}
	snapshotMismatch := !directProviderSnapshotMatches(providerCampaign, material.Campaign)
	if _, err = a.store.SyncDirectCampaignProviderStatus(
		ctx, workspaceID, campaignID, providerCampaign.ID,
		providerCampaign.Status, providerCampaign.State, a.now().UTC(),
	); err != nil {
		return err
	}
	return a.store.SetDirectCampaignProviderSnapshotMismatch(
		ctx, workspaceID, campaignID, snapshotMismatch, a.now().UTC(),
	)
}

func directProviderSnapshotMatches(
	provider yandexdirect.Campaign, campaign store.DirectCampaign,
) bool {
	return campaign.ProviderCampaignID != nil &&
		provider.ID == *campaign.ProviderCampaignID &&
		strings.TrimSpace(provider.Name) == strings.TrimSpace(campaign.Name) &&
		provider.WeeklyBudgetMinor == campaign.WeeklyBudgetMinor &&
		sameDirectDate(provider.StartsAt, campaign.StartsAt) &&
		sameDirectDate(provider.EndsAt, campaign.EndsAt)
}

func directProviderCampaignRunning(campaign yandexdirect.Campaign) bool {
	return strings.EqualFold(strings.TrimSpace(campaign.State), "ON")
}

func directProviderCampaignDefinitelyOff(campaign yandexdirect.Campaign) bool {
	state := strings.ToUpper(strings.TrimSpace(campaign.State))
	return state == "OFF"
}

func directProviderStrategySnapshotError(err error) bool {
	var providerErr *yandexdirect.Error
	if !errors.As(err, &providerErr) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(providerErr.Code)) {
	case "campaign_budget_unavailable", "campaign_budget_invalid":
		return true
	default:
		return false
	}
}

func directProviderAuthorizationError(err error) bool {
	var providerErr *yandexdirect.Error
	if !errors.As(err, &providerErr) {
		return false
	}
	return providerErr.StatusCode == http.StatusUnauthorized ||
		providerErr.APIErrorCode == 53 ||
		strings.TrimSpace(providerErr.Code) == "53"
}

func (a *App) markDirectConnectionAuthorizationRequired(
	ctx context.Context, connection store.DirectConnection, providerErr error, now time.Time,
) error {
	if !directProviderAuthorizationError(providerErr) {
		return nil
	}
	return a.store.MarkDirectConnectionAuthorizationRequired(
		ctx, connection.WorkspaceID, connection.ID, now.UTC(),
	)
}

func (a *App) SuggestDirectCampaign(
	ctx context.Context, actorUserID, workspaceID string,
	request openairesearch.SuggestDirectCampaignRequest,
) (openairesearch.SuggestDirectCampaignResult, error) {
	suggester, ok := a.research.(DirectCampaignSuggester)
	if !ok || a.research == nil {
		return openairesearch.SuggestDirectCampaignResult{}, ErrResearchNotConfigured
	}
	// These fields are always server-authoritative. A caller cannot smuggle
	// content from another tenant into the prompt through the public request.
	request.ChannelTitle = ""
	request.ChannelDescription = ""
	request.RecentPosts = nil
	channels, err := a.store.ListChannelsForWorkspace(ctx, actorUserID, workspaceID)
	if err != nil {
		return openairesearch.SuggestDirectCampaignResult{}, err
	}
	for _, channel := range channels {
		if !sameDirectURL(channel.PublicLink, request.LandingURL) {
			continue
		}
		request.ChannelTitle = channel.Title
		request.ChannelDescription = channel.Description
		posts, listErr := a.store.ListRecentPublishedPostContentsForWorkspace(
			ctx, actorUserID, workspaceID, channel.ID,
		)
		if listErr != nil {
			return openairesearch.SuggestDirectCampaignResult{}, listErr
		}
		for _, post := range posts {
			if content := strings.TrimSpace(post); content != "" {
				request.RecentPosts = append(request.RecentPosts, content)
			}
		}
		break
	}
	if err := openairesearch.ValidateSuggestDirectCampaignRequest(request); err != nil {
		return openairesearch.SuggestDirectCampaignResult{}, err
	}
	return suggester.SuggestDirectCampaign(ctx, request)
}

func (c *directTokenCipher) seal(workspaceID, accountID, clientLogin, token string) (string, error) {
	if c == nil || c.aead == nil || strings.TrimSpace(token) == "" {
		return "", errors.New("token encryption for Yandex Direct is unavailable")
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(token), directTokenAAD(workspaceID, accountID, clientLogin))
	return directTokenCipherPrefix + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (c *directTokenCipher) open(workspaceID, accountID, clientLogin, value string) (string, error) {
	if c == nil || c.aead == nil || !strings.HasPrefix(value, directTokenCipherPrefix) {
		return "", errors.New("invalid encrypted Yandex Direct token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, directTokenCipherPrefix))
	if err != nil || len(payload) <= c.aead.NonceSize() {
		return "", errors.New("invalid encrypted Yandex Direct token")
	}
	nonce, ciphertext := payload[:c.aead.NonceSize()], payload[c.aead.NonceSize():]
	plain, err := c.aead.Open(nil, nonce, ciphertext, directTokenAAD(workspaceID, accountID, clientLogin))
	if err != nil {
		return "", errors.New("invalid encrypted Yandex Direct token")
	}
	return string(plain), nil
}

func directTokenAAD(workspaceID, accountID, clientLogin string) []byte {
	return []byte(directTokenCipherPrefix + "\x00" + workspaceID + "\x00" +
		accountID + "\x00" + strings.ToLower(strings.TrimSpace(clientLogin)))
}

func randomDirectToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func directOAuthStateHash(
	flow yandexdirect.OAuthFlow, sessionBinding, state string,
) string {
	sum := sha256.Sum256([]byte(
		string(flow) + "\x00" + strings.ToLower(sessionBinding) + "\x00" + state,
	))
	return hex.EncodeToString(sum[:])
}

func validDirectOAuthSessionBinding(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func validDirectOAuthState(value string) bool {
	if len(value) != base64.RawURLEncoding.EncodedLen(32) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func validDirectOAuthCode(flow yandexdirect.OAuthFlow, value string) bool {
	if flow == yandexdirect.OAuthFlowVerificationCode {
		if len(value) != 7 {
			return false
		}
		for index := 0; index < len(value); index++ {
			if value[index] < '0' || value[index] > '9' {
				return false
			}
		}
		return true
	}
	if len(value) == 0 || len(value) > 2048 {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < 0x21 || value[index] > 0x7e {
			return false
		}
	}
	return true
}

func safeDirectReturnTo(value string) string {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "/app/") || strings.HasPrefix(value, "//") ||
		strings.ContainsAny(value, "\r\n") {
		return "/app/#/advertising"
	}
	return value
}

func directProviderErrorCode(err error) string {
	var providerErr *yandexdirect.Error
	if errors.As(err, &providerErr) {
		code := strings.ToLower(strings.TrimSpace(providerErr.Code))
		if code != "" && len(code) <= 128 {
			return code
		}
	}
	return "provider_request_failed"
}

func sameDirectDate(left, right time.Time) bool {
	left, right = left.UTC(), right.UTC()
	return left.Year() == right.Year() && left.YearDay() == right.YearDay()
}

func sameDirectURL(left, right string) bool {
	parse := func(raw string) string {
		parsed, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return ""
		}
		parsed.Scheme = strings.ToLower(parsed.Scheme)
		parsed.Host = strings.ToLower(parsed.Host)
		parsed.Fragment = ""
		parsed.RawQuery = ""
		parsed.Path = strings.TrimRight(parsed.EscapedPath(), "/")
		return parsed.String()
	}
	normalizedLeft, normalizedRight := parse(left), parse(right)
	return normalizedLeft != "" && normalizedLeft == normalizedRight
}
