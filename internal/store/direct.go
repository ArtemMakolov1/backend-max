package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const DirectAutoLaunchConsentVersion = "yandex-direct-auto-launch-v2"

const (
	// DirectMaxCampaignWeeklyBudgetMinor is 10,000 RUB. This is a hard
	// product safety ceiling, independent of the provider's much larger
	// monetary range.
	DirectMaxCampaignWeeklyBudgetMinor int64 = 1_000_000
	// DirectMaxWorkspaceWeeklyBudgetMinor caps the sum of MaxPosty-managed
	// campaigns that are active or have a durable launch claim at 30,000 RUB.
	DirectMaxWorkspaceWeeklyBudgetMinor int64 = 3_000_000
)

const directLaunchRecoveryLease = 2 * time.Minute
const directProviderPollLease = time.Minute
const directOAuthCompletionRetention = 5 * time.Minute
const directTokenRefreshLease = 2 * time.Minute

var directIdentifierPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var directOAuthStateHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

var (
	ErrDirectConnectionRequired     = errors.New("direct connection is required")
	ErrDirectCampaignNotDraft       = errors.New("direct campaign is not a draft")
	ErrDirectCampaignNotAccepted    = errors.New("direct campaign is not accepted")
	ErrDirectConsentRequired        = errors.New("active direct auto-launch consent is required")
	ErrDirectConsentMismatch        = errors.New("direct auto-launch consent does not match the campaign")
	ErrDirectLaunchAlreadyClaimed   = errors.New("direct launch was already claimed")
	ErrDirectLaunchRetryExhausted   = errors.New("direct launch retry is exhausted; reconciliation is still required")
	ErrDirectBudgetCapExceeded      = errors.New("direct budget safety cap is exceeded")
	ErrDirectTokenRefreshBusy       = errors.New("direct token refresh is already in progress")
	ErrDirectGraphUnverified        = errors.New("direct provider graph is not verified")
	ErrDirectModerationNotReady     = errors.New("direct provider graph moderation is not accepted")
	ErrDirectProviderOperationBusy  = errors.New("direct provider operation is already in progress")
	ErrDirectProviderOperationStale = errors.New("direct provider operation lease is stale")
	ErrDirectValidation             = errors.New("invalid Yandex Direct campaign")
)

const directConnectionColumns = `id,workspace_id,account_id,client_login,account_name,currency_code,
timezone,read_only,token_ciphertext,token_key_version,status,connected_by,last_verified_at,error_code,
created_at,updated_at,revoked_at,token_refresh_claimed_at`

const directCampaignColumns = `id,workspace_id,connection_id,provider_campaign_id,name,
objective,landing_url,brief,regions,weekly_budget_minor,currency_code,starts_at,ends_at,
status,provider_status,provider_state,provider_next_check_at,auto_launch_next_attempt_at,
version,created_by,submitted_at,launch_claimed_at,
launch_state,launch_mode,launch_attempt_count,launch_reconcile_after,launched_at,
launch_failed_at,launch_failure_code,created_at,updated_at,
titles,texts,keywords,negative_keywords,provider_ad_group_id,provider_ad_id,
provider_keyword_ids,provider_keyword_mappings,provider_warnings,
COALESCE(submission_operation_id,''),submission_stage,submission_operation_marker,
submission_claimed_at,submission_lease_expires_at,submission_failure_code,
submission_failure_clarification,provider_graph_hash,
COALESCE(provider_revision_id,''),graph_verified_at,moderation_status,
moderation_clarification,campaign_moderation,ad_group_moderation,ad_moderation,
keyword_moderation`

const directConsentColumns = `id,workspace_id,campaign_id,connection_id,actor_user_id,
consent_version,confirmation,campaign_version,account_id,provider_campaign_id,
campaign_name,weekly_budget_minor,currency_code,starts_at,ends_at,
expected_graph_hash,COALESCE(expected_revision_id,''),authorized_at,revoked_at,
invalidated_at,invalid_reason,consumed_at`

type DirectOAuthState struct {
	StateHash    string
	WorkspaceID  string
	ActorUserID  string
	PKCEVerifier string
	ClientLogin  string
	ReturnTo     string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	ConsumedAt   *time.Time
}

type DirectConnection struct {
	ID                    string     `json:"id"`
	WorkspaceID           string     `json:"workspace_id"`
	AccountID             string     `json:"account_id"`
	ClientLogin           string     `json:"client_login,omitempty"`
	AccountName           string     `json:"account_name,omitempty"`
	CurrencyCode          string     `json:"currency_code"`
	Timezone              string     `json:"timezone"`
	ReadOnly              bool       `json:"read_only"`
	TokenCiphertext       string     `json:"-"`
	TokenKeyVersion       int        `json:"-"`
	Status                string     `json:"status"`
	ConnectedBy           string     `json:"connected_by"`
	LastVerifiedAt        *time.Time `json:"last_verified_at,omitempty"`
	ErrorCode             string     `json:"error_code,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
	RevokedAt             *time.Time `json:"revoked_at,omitempty"`
	TokenRefreshClaimedAt *time.Time `json:"-"`
}

type DirectCampaign struct {
	ID                             string                   `json:"id"`
	WorkspaceID                    string                   `json:"workspace_id"`
	ConnectionID                   string                   `json:"connection_id"`
	ProviderCampaignID             *int64                   `json:"provider_campaign_id,omitempty"`
	Name                           string                   `json:"name"`
	Objective                      string                   `json:"objective"`
	LandingURL                     string                   `json:"landing_url"`
	Brief                          string                   `json:"brief"`
	Regions                        []string                 `json:"regions"`
	Titles                         []string                 `json:"titles"`
	Texts                          []string                 `json:"texts"`
	Keywords                       []string                 `json:"keywords"`
	NegativeKeywords               []string                 `json:"negative_keywords"`
	WeeklyBudgetMinor              int64                    `json:"weekly_budget_minor"`
	CurrencyCode                   string                   `json:"currency_code"`
	StartsAt                       time.Time                `json:"starts_at"`
	EndsAt                         time.Time                `json:"ends_at"`
	Status                         string                   `json:"status"`
	ProviderStatus                 string                   `json:"provider_status,omitempty"`
	ProviderState                  string                   `json:"provider_state,omitempty"`
	ProviderNextCheckAt            time.Time                `json:"-"`
	AutoLaunchNextAttemptAt        time.Time                `json:"-"`
	Version                        int64                    `json:"version"`
	CreatedBy                      string                   `json:"created_by"`
	SubmittedAt                    *time.Time               `json:"submitted_at,omitempty"`
	LaunchClaimedAt                *time.Time               `json:"-"`
	LaunchState                    string                   `json:"-"`
	LaunchMode                     string                   `json:"-"`
	LaunchAttemptCount             int                      `json:"-"`
	LaunchReconcileAfter           *time.Time               `json:"-"`
	LaunchedAt                     *time.Time               `json:"launched_at,omitempty"`
	LaunchFailedAt                 *time.Time               `json:"-"`
	LaunchFailureCode              string                   `json:"launch_failure_code,omitempty"`
	ProviderAdGroupID              *int64                   `json:"provider_ad_group_id,omitempty"`
	ProviderAdID                   *int64                   `json:"provider_ad_id,omitempty"`
	ProviderKeywordIDs             []int64                  `json:"provider_keyword_ids"`
	ProviderKeywordMappings        []DirectKeywordMapping   `json:"provider_keyword_mappings"`
	ProviderWarnings               []DirectProviderIssue    `json:"provider_warnings"`
	SubmissionOperationID          string                   `json:"submission_operation_id,omitempty"`
	SubmissionStage                string                   `json:"submission_stage"`
	SubmissionOperationMarker      string                   `json:"submission_operation_marker,omitempty"`
	SubmissionClaimedAt            *time.Time               `json:"-"`
	SubmissionLeaseExpiresAt       *time.Time               `json:"-"`
	SubmissionFailureCode          string                   `json:"submission_failure_code,omitempty"`
	SubmissionFailureClarification string                   `json:"submission_failure_clarification,omitempty"`
	ProviderGraphHash              string                   `json:"provider_graph_hash,omitempty"`
	ProviderRevisionID             string                   `json:"provider_revision_id,omitempty"`
	GraphVerifiedAt                *time.Time               `json:"graph_verified_at,omitempty"`
	ModerationStatus               string                   `json:"moderation_status,omitempty"`
	ModerationClarification        string                   `json:"moderation_clarification,omitempty"`
	CampaignModeration             DirectModerationSnapshot `json:"campaign_moderation"`
	AdGroupModeration              DirectModerationSnapshot `json:"ad_group_moderation"`
	AdModeration                   DirectModerationSnapshot `json:"ad_moderation"`
	KeywordModeration              []DirectKeywordMapping   `json:"keyword_moderation"`
	CreatedAt                      time.Time                `json:"created_at"`
	UpdatedAt                      time.Time                `json:"updated_at"`
	AutoLaunch                     DirectAutoLaunchSummary  `json:"auto_launch"`
}

type DirectCampaignChanges struct {
	Name              *string
	Objective         *string
	LandingURL        *string
	Brief             *string
	Regions           *[]string
	Titles            *[]string
	Texts             *[]string
	Keywords          *[]string
	NegativeKeywords  *[]string
	WeeklyBudgetMinor *int64
	StartsAt          *time.Time
	EndsAt            *time.Time
	ExpectedVersion   int64
}

type DirectAutoLaunchConsent struct {
	ID                 string     `json:"id"`
	WorkspaceID        string     `json:"workspace_id"`
	CampaignID         string     `json:"campaign_id"`
	ConnectionID       string     `json:"connection_id"`
	ActorUserID        string     `json:"actor_user_id"`
	ConsentVersion     string     `json:"consent_version"`
	Confirmation       string     `json:"-"`
	CampaignVersion    int64      `json:"campaign_version"`
	AccountID          string     `json:"account_id"`
	ProviderCampaignID *int64     `json:"provider_campaign_id,omitempty"`
	CampaignName       string     `json:"campaign_name"`
	WeeklyBudgetMinor  int64      `json:"weekly_budget_minor"`
	CurrencyCode       string     `json:"currency_code"`
	StartsAt           time.Time  `json:"starts_at"`
	EndsAt             time.Time  `json:"ends_at"`
	ExpectedGraphHash  string     `json:"expected_graph_hash,omitempty"`
	ExpectedRevisionID string     `json:"expected_revision_id,omitempty"`
	AuthorizedAt       time.Time  `json:"authorized_at"`
	RevokedAt          *time.Time `json:"revoked_at,omitempty"`
	InvalidatedAt      *time.Time `json:"invalidated_at,omitempty"`
	InvalidReason      string     `json:"invalid_reason,omitempty"`
	ConsumedAt         *time.Time `json:"consumed_at,omitempty"`
}

type DirectAutoLaunchSummary struct {
	Enabled            bool       `json:"enabled"`
	AuthorizedAt       *time.Time `json:"authorized_at,omitempty"`
	Valid              bool       `json:"valid"`
	InvalidReason      string     `json:"invalid_reason,omitempty"`
	CampaignID         string     `json:"campaign_id,omitempty"`
	CampaignName       string     `json:"campaign_name,omitempty"`
	ProviderCampaignID string     `json:"provider_campaign_id,omitempty"`
	WarningCode        string     `json:"warning_code,omitempty"`
}

type DirectConsentRequest struct {
	Confirmation         string
	ExpectedVersion      int64
	ExpectedConnectionID string
	ExpectedAccountID    string
	ExpectedCampaignName string
	ExpectedProviderID   int64
	ExpectedGraphHash    string
	ExpectedRevisionID   string
	WeeklyBudgetMinor    int64
	StartsAt             time.Time
	EndsAt               time.Time
	AuthorizedAt         time.Time
}

type DirectLaunchMaterial struct {
	Campaign        DirectCampaign
	Connection      DirectConnection
	Consent         DirectAutoLaunchConsent
	TokenCiphertext string `json:"-"`
	TokenKeyVersion int    `json:"-"`
}

type DirectLaunchRecoveryCandidate struct {
	WorkspaceID string
	CampaignID  string
}

func (s *Store) CreateDirectOAuthState(
	ctx context.Context, actorUserID, workspaceID string, state DirectOAuthState,
) error {
	if state.StateHash == "" || state.PKCEVerifier == "" || state.ExpiresAt.IsZero() {
		return errors.New("direct OAuth state, verifier and expiry are required")
	}
	if state.CreatedAt.IsZero() {
		state.CreatedAt = time.Now().UTC()
	}
	state.ActorUserID, state.WorkspaceID = actorUserID, workspaceID
	state.ClientLogin = strings.TrimSpace(state.ClientLogin)
	if state.ReturnTo == "" {
		state.ReturnTo = "/app/#/advertising"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner); err != nil {
		return err
	}
	// Serialize restarts for this workspace. Without the row lock, two empty
	// DELETE snapshots could both insert an active state and leave an older tab
	// able to win after the newer authorization.
	var lockedWorkspaceID string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID,
	).Scan(&lockedWorkspaceID); err != nil {
		return mapWorkspaceWriteError("lock workspace for Direct OAuth", err)
	}
	// A restart is authoritative for this actor and workspace. Removing the
	// previous active attempt in the same transaction prevents an older tab
	// from replacing the connection after a newer authorization was started.
	if _, err := tx.ExecContext(ctx, `DELETE FROM direct_oauth_states
WHERE actor_user_id=$1 AND workspace_id=$2 AND expires_at>$3 AND consumed_at IS NULL`,
		actorUserID, workspaceID, state.CreatedAt.UTC()); err != nil {
		return err
	}
	var activeStates int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM direct_oauth_states
WHERE actor_user_id=$1 AND expires_at>$2 AND consumed_at IS NULL`,
		actorUserID, state.CreatedAt.UTC()).Scan(&activeStates); err != nil {
		return err
	}
	if activeStates >= 8 {
		return ErrConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO direct_oauth_states(
state_hash,workspace_id,actor_user_id,pkce_verifier,client_login,return_to,created_at,expires_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, state.StateHash, workspaceID, actorUserID,
		state.PKCEVerifier, state.ClientLogin, state.ReturnTo, state.CreatedAt.UTC(), state.ExpiresAt.UTC())
	if err != nil {
		return mapWorkspaceWriteError("create Direct OAuth state", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO direct_oauth_latest_attempts(
workspace_id,state_hash,actor_user_id,created_at)
VALUES($1,$2,$3,$4)
ON CONFLICT(workspace_id) DO UPDATE SET
state_hash=EXCLUDED.state_hash,actor_user_id=EXCLUDED.actor_user_id,created_at=EXCLUDED.created_at`,
		workspaceID, state.StateHash, actorUserID, state.CreatedAt.UTC()); err != nil {
		return mapWorkspaceWriteError("record latest Direct OAuth attempt", err)
	}
	return tx.Commit()
}

func (s *Store) ConsumeDirectOAuthState(
	ctx context.Context, actorUserID, stateHash string, now time.Time,
) (DirectOAuthState, error) {
	return s.consumeDirectOAuthState(ctx, actorUserID, "", stateHash, now)
}

func (s *Store) ConsumeDirectOAuthStateForWorkspace(
	ctx context.Context, actorUserID, workspaceID, stateHash string, now time.Time,
) (DirectOAuthState, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return DirectOAuthState{}, ErrNotFound
	}
	return s.consumeDirectOAuthState(ctx, actorUserID, workspaceID, stateHash, now)
}

func (s *Store) consumeDirectOAuthState(
	ctx context.Context, actorUserID, workspaceID, stateHash string, now time.Time,
) (DirectOAuthState, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DirectOAuthState{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var state DirectOAuthState
	query := `SELECT state_hash,workspace_id,actor_user_id,pkce_verifier,
client_login,return_to,created_at,expires_at,consumed_at
FROM direct_oauth_states WHERE state_hash=$1 AND actor_user_id=$2`
	args := []any{stateHash, actorUserID}
	if workspaceID != "" {
		query += ` AND workspace_id=$3`
		args = append(args, workspaceID)
	}
	query += ` FOR UPDATE`
	err = tx.QueryRowContext(ctx, query, args...).Scan(
		&state.StateHash, &state.WorkspaceID, &state.ActorUserID,
		&state.PKCEVerifier, &state.ClientLogin, &state.ReturnTo, &state.CreatedAt,
		&state.ExpiresAt, &state.ConsumedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DirectOAuthState{}, ErrNotFound
	}
	if err != nil {
		return DirectOAuthState{}, err
	}
	if state.ConsumedAt != nil || !state.ExpiresAt.After(now.UTC()) {
		return DirectOAuthState{}, ErrConflict
	}
	if err := requireWorkspaceRole(ctx, tx, actorUserID, state.WorkspaceID, WorkspaceRoleOwner); err != nil {
		return DirectOAuthState{}, err
	}
	var latest bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM direct_oauth_latest_attempts
WHERE workspace_id=$1 AND state_hash=$2 AND actor_user_id=$3
)`, state.WorkspaceID, stateHash, actorUserID).Scan(&latest); err != nil {
		return DirectOAuthState{}, err
	}
	if !latest {
		return DirectOAuthState{}, ErrConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE direct_oauth_states
SET consumed_at=$1
WHERE state_hash=$2 AND actor_user_id=$3 AND consumed_at IS NULL`,
		now.UTC(), stateHash, actorUserID)
	if err != nil {
		return DirectOAuthState{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return DirectOAuthState{}, ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return DirectOAuthState{}, err
	}
	normalizeDirectOAuthState(&state)
	consumed := now.UTC()
	state.ConsumedAt = &consumed
	return state, nil
}

func (s *Store) PurgeExpiredDirectOAuthStates(
	ctx context.Context, now time.Time, limit int,
) (int64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	result, err := s.db.ExecContext(ctx, `WITH expired AS (
    SELECT state_hash
    FROM direct_oauth_states
    WHERE (consumed_at IS NULL AND expires_at <= $1)
       OR (consumed_at IS NOT NULL AND consumed_at <= $2)
    ORDER BY COALESCE(consumed_at,expires_at),state_hash
    FOR UPDATE SKIP LOCKED
    LIMIT $3
)
DELETE FROM direct_oauth_states s
USING expired e
WHERE s.state_hash=e.state_hash`,
		now.UTC(), now.UTC().Add(-directOAuthCompletionRetention), limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) ReplaceDirectConnection(
	ctx context.Context, actorUserID, workspaceID string, connection DirectConnection,
) (DirectConnection, error) {
	return s.replaceDirectConnection(ctx, actorUserID, workspaceID, "", connection)
}

func (s *Store) ReplaceDirectConnectionFromOAuthAttempt(
	ctx context.Context, actorUserID, workspaceID, expectedStateHash string,
	connection DirectConnection,
) (DirectConnection, error) {
	expectedStateHash = strings.TrimSpace(expectedStateHash)
	if !directOAuthStateHashPattern.MatchString(expectedStateHash) {
		return DirectConnection{}, ErrConflict
	}
	return s.replaceDirectConnection(
		ctx, actorUserID, workspaceID, expectedStateHash, connection,
	)
}

func (s *Store) replaceDirectConnection(
	ctx context.Context, actorUserID, workspaceID, expectedStateHash string,
	connection DirectConnection,
) (DirectConnection, error) {
	connection.AccountID = strings.TrimSpace(connection.AccountID)
	connection.ClientLogin = strings.TrimSpace(connection.ClientLogin)
	connection.AccountName = strings.TrimSpace(connection.AccountName)
	connection.CurrencyCode = strings.ToUpper(strings.TrimSpace(connection.CurrencyCode))
	connection.Timezone = strings.TrimSpace(connection.Timezone)
	if connection.AccountID == "" || connection.CurrencyCode == "" || connection.TokenCiphertext == "" {
		return DirectConnection{}, errors.New("direct account, currency and encrypted token are required")
	}
	if connection.Timezone == "" {
		connection.Timezone = "Europe/Moscow"
	}
	if connection.ID == "" {
		connection.ID = newStoreID("dcon_")
	}
	now := connection.CreatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if connection.TokenKeyVersion <= 0 {
		connection.TokenKeyVersion = 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DirectConnection{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner); err != nil {
		return DirectConnection{}, err
	}
	var lockedWorkspaceID string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID,
	).Scan(&lockedWorkspaceID); err != nil {
		return DirectConnection{}, mapWorkspaceWriteError("lock workspace for Direct connection", err)
	}
	if expectedStateHash != "" {
		var currentAttemptStateHash string
		err := tx.QueryRowContext(ctx, `SELECT a.state_hash
FROM direct_oauth_latest_attempts a
JOIN direct_oauth_states s ON s.state_hash=a.state_hash
WHERE a.workspace_id=$1 AND a.actor_user_id=$2 AND a.state_hash=$3
  AND s.workspace_id=a.workspace_id AND s.actor_user_id=a.actor_user_id
  AND s.consumed_at IS NOT NULL
FOR UPDATE`, workspaceID, actorUserID, expectedStateHash).Scan(&currentAttemptStateHash)
		if errors.Is(err, sql.ErrNoRows) {
			return DirectConnection{}, ErrConflict
		}
		if err != nil {
			return DirectConnection{}, err
		}
	} else {
		var oauthAttemptPending bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM direct_oauth_latest_attempts WHERE workspace_id=$1
)`, workspaceID).Scan(&oauthAttemptPending); err != nil {
			return DirectConnection{}, err
		}
		if oauthAttemptPending {
			return DirectConnection{}, ErrConflict
		}
	}
	var currentConnectionID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM direct_connections
WHERE workspace_id=$1 AND revoked_at IS NULL FOR UPDATE`, workspaceID).Scan(&currentConnectionID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return DirectConnection{}, err
	}
	var launchInFlight bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM direct_campaigns
WHERE workspace_id=$1 AND connection_id=$2
  AND (
    launch_state IN ('launching','reconciling')
    OR launch_state='failed'
    OR (
      provider_campaign_id IS NOT NULL
      AND status IN ('provider_draft','moderation','accepted','rejected','active','suspended')
    )
  )
) OR EXISTS(
SELECT 1 FROM direct_provider_operations
WHERE workspace_id=$1 AND connection_id=$2
  AND completed_at IS NULL AND stage NOT IN ('completed','failed')
)`, workspaceID, currentConnectionID).Scan(&launchInFlight); err != nil {
		return DirectConnection{}, err
	}
	if launchInFlight {
		// Replacing the account would erase the only credential capable of
		// reconciling an ambiguous provider write.
		return DirectConnection{}, ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE direct_connections
SET status='revoked',token_ciphertext='',token_refresh_claimed_at=NULL,
    revoked_at=$1,updated_at=$1,error_code=''
WHERE workspace_id=$2 AND revoked_at IS NULL`, now, workspaceID); err != nil {
		return DirectConnection{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE direct_auto_launch_consents_v2
SET invalidated_at=$1,invalid_reason='connection_replaced'
WHERE workspace_id=$2 AND revoked_at IS NULL AND invalidated_at IS NULL AND consumed_at IS NULL`,
		now, workspaceID); err != nil {
		return DirectConnection{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO direct_connections(
id,workspace_id,account_id,client_login,account_name,currency_code,timezone,read_only,token_ciphertext,
token_key_version,status,connected_by,last_verified_at,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'active',$11,$12,$12,$12)`,
		connection.ID, workspaceID, connection.AccountID, connection.ClientLogin,
		connection.AccountName, connection.CurrencyCode, connection.Timezone,
		connection.ReadOnly, connection.TokenCiphertext, connection.TokenKeyVersion, actorUserID, now)
	if err != nil {
		return DirectConnection{}, mapWorkspaceWriteError("connect Yandex Direct", err)
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "direct.connection.connected",
		EntityType: "direct_connection", EntityID: connection.ID,
		Metadata: mustJSON(map[string]any{
			"account_id": connection.AccountID, "client_login": connection.ClientLogin,
			"currency_code": connection.CurrencyCode,
		}), CreatedAt: now,
	}); err != nil {
		return DirectConnection{}, err
	}
	if expectedStateHash != "" {
		result, err := tx.ExecContext(ctx, `DELETE FROM direct_oauth_states
WHERE state_hash=$1 AND workspace_id=$2 AND actor_user_id=$3 AND consumed_at IS NOT NULL`,
			expectedStateHash, workspaceID, actorUserID)
		if err != nil {
			return DirectConnection{}, err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return DirectConnection{}, ErrConflict
		}
	}
	if err := tx.Commit(); err != nil {
		return DirectConnection{}, err
	}
	return s.GetDirectConnection(ctx, actorUserID, workspaceID)
}

func (s *Store) GetDirectConnection(
	ctx context.Context, actorUserID, workspaceID string,
) (DirectConnection, error) {
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID); err != nil {
		return DirectConnection{}, err
	}
	return scanDirectConnection(s.db.QueryRowContext(ctx, `SELECT `+directConnectionColumns+`
FROM direct_connections WHERE workspace_id=$1 AND revoked_at IS NULL
ORDER BY created_at DESC LIMIT 1`, workspaceID))
}

func (s *Store) getDirectConnectionForWorker(
	ctx context.Context, workspaceID, connectionID string,
) (DirectConnection, error) {
	return scanDirectConnection(s.db.QueryRowContext(ctx, `SELECT `+directConnectionColumns+`
FROM direct_connections WHERE workspace_id=$1 AND id=$2 AND revoked_at IS NULL`,
		workspaceID, connectionID))
}

// GetDirectConnectionTokenMaterial is worker-scoped token material. Public API
// responses must continue to use GetDirectConnection, whose callers strip the
// ciphertext before serialization.
func (s *Store) GetDirectConnectionTokenMaterial(
	ctx context.Context, workspaceID, connectionID string,
) (DirectConnection, error) {
	return s.getDirectConnectionForWorker(ctx, workspaceID, connectionID)
}

func (s *Store) ClaimDirectConnectionTokenRefresh(
	ctx context.Context, workspaceID, connectionID, expectedCiphertext string, now time.Time,
) (DirectConnection, error) {
	if strings.TrimSpace(expectedCiphertext) == "" {
		return DirectConnection{}, ErrConflict
	}
	now = now.UTC()
	connection, err := scanDirectConnection(s.db.QueryRowContext(ctx, `UPDATE direct_connections
SET token_refresh_claimed_at=$1,updated_at=$1
WHERE workspace_id=$2 AND id=$3 AND status='active' AND revoked_at IS NULL
  AND token_ciphertext=$4
  AND (token_refresh_claimed_at IS NULL OR token_refresh_claimed_at <= $5)
RETURNING `+directConnectionColumns,
		now, workspaceID, connectionID, expectedCiphertext, now.Add(-directTokenRefreshLease)))
	if err == nil {
		return connection, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return DirectConnection{}, err
	}
	current, currentErr := s.getDirectConnectionForWorker(ctx, workspaceID, connectionID)
	if currentErr != nil {
		return DirectConnection{}, currentErr
	}
	if current.TokenCiphertext == expectedCiphertext &&
		current.TokenRefreshClaimedAt != nil &&
		current.TokenRefreshClaimedAt.After(now.Add(-directTokenRefreshLease)) {
		return DirectConnection{}, ErrDirectTokenRefreshBusy
	}
	return DirectConnection{}, ErrConflict
}

func (s *Store) CompleteDirectConnectionTokenRefresh(
	ctx context.Context, workspaceID, connectionID, expectedCiphertext string,
	claimedAt time.Time, replacementCiphertext string, now time.Time,
) (DirectConnection, error) {
	if expectedCiphertext == "" || replacementCiphertext == "" || claimedAt.IsZero() {
		return DirectConnection{}, ErrConflict
	}
	connection, err := scanDirectConnection(s.db.QueryRowContext(ctx, `UPDATE direct_connections
SET token_ciphertext=$1,token_refresh_claimed_at=NULL,last_verified_at=$2,
    status='active',error_code='',updated_at=$2
WHERE workspace_id=$3 AND id=$4 AND status='active' AND revoked_at IS NULL
  AND token_ciphertext=$5 AND token_refresh_claimed_at=$6
RETURNING `+directConnectionColumns,
		replacementCiphertext, now.UTC(), workspaceID, connectionID,
		expectedCiphertext, claimedAt.UTC()))
	if errors.Is(err, ErrNotFound) {
		return DirectConnection{}, ErrConflict
	}
	return connection, err
}

func (s *Store) ReleaseDirectConnectionTokenRefresh(
	ctx context.Context, workspaceID, connectionID, expectedCiphertext string,
	claimedAt, now time.Time,
) error {
	result, err := s.db.ExecContext(ctx, `UPDATE direct_connections
SET token_refresh_claimed_at=NULL,updated_at=$1
WHERE workspace_id=$2 AND id=$3 AND status='active' AND revoked_at IS NULL
  AND token_ciphertext=$4 AND token_refresh_claimed_at=$5`,
		now.UTC(), workspaceID, connectionID, expectedCiphertext, claimedAt.UTC())
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) MarkDirectConnectionRefreshAuthorizationRequired(
	ctx context.Context, workspaceID, connectionID, expectedCiphertext string,
	claimedAt, now time.Time,
) (bool, error) {
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE direct_connections
SET status='error',error_code='authorization_required',
    token_refresh_claimed_at=NULL,updated_at=$1
WHERE workspace_id=$2 AND id=$3 AND status='active' AND revoked_at IS NULL
  AND token_ciphertext=$4 AND token_refresh_claimed_at=$5`,
		now, workspaceID, connectionID, expectedCiphertext, claimedAt.UTC())
	if err != nil {
		return false, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE direct_auto_launch_consents_v2
SET invalidated_at=$1,invalid_reason='connection_authorization_required'
WHERE workspace_id=$2 AND connection_id=$3
  AND revoked_at IS NULL AND invalidated_at IS NULL AND consumed_at IS NULL`,
		now, workspaceID, connectionID); err != nil {
		return false, err
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, Action: "direct.connection.authorization_required",
		EntityType: "direct_connection", EntityID: connectionID,
		Metadata:  mustJSON(map[string]any{"error_code": "authorization_required"}),
		CreatedAt: now,
	}); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) MarkDirectConnectionAuthorizationRequired(
	ctx context.Context, workspaceID, connectionID string, now time.Time,
) error {
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE direct_connections
SET status='error',error_code='authorization_required',
    token_refresh_claimed_at=NULL,updated_at=$1
WHERE workspace_id=$2 AND id=$3 AND status IN ('active','error') AND revoked_at IS NULL`,
		now, workspaceID, connectionID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE direct_auto_launch_consents_v2
SET invalidated_at=$1,invalid_reason='connection_authorization_required'
WHERE workspace_id=$2 AND connection_id=$3
  AND revoked_at IS NULL AND invalidated_at IS NULL AND consumed_at IS NULL`,
		now, workspaceID, connectionID); err != nil {
		return err
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, Action: "direct.connection.authorization_required",
		EntityType: "direct_connection", EntityID: connectionID,
		Metadata:  mustJSON(map[string]any{"error_code": "authorization_required"}),
		CreatedAt: now,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RevokeDirectConnection(
	ctx context.Context, actorUserID, workspaceID string, now time.Time,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner); err != nil {
		return err
	}
	var connectionID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM direct_connections
WHERE workspace_id=$1 AND revoked_at IS NULL FOR UPDATE`, workspaceID).Scan(&connectionID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var launchInFlight bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM direct_campaigns
WHERE workspace_id=$1 AND connection_id=$2
  AND launch_state IN ('launching','reconciling','failed')
) OR EXISTS(
SELECT 1 FROM direct_provider_operations
WHERE workspace_id=$1 AND connection_id=$2
  AND completed_at IS NULL AND stage NOT IN ('completed','failed')
)`, workspaceID, connectionID).Scan(&launchInFlight); err != nil {
		return err
	}
	if launchInFlight {
		// Keep the encrypted credential available until reconciliation proves
		// whether the provider accepted the spend-capable operation.
		return ErrConflict
	}
	now = now.UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE direct_connections
SET status='revoked',token_ciphertext='',token_refresh_claimed_at=NULL,
    revoked_at=$1,updated_at=$1,error_code=''
WHERE workspace_id=$2 AND id=$3`, now, workspaceID, connectionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE direct_auto_launch_consents_v2
SET invalidated_at=$1,invalid_reason='connection_revoked'
WHERE workspace_id=$2 AND revoked_at IS NULL AND invalidated_at IS NULL AND consumed_at IS NULL`,
		now, workspaceID); err != nil {
		return err
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "direct.connection.revoked",
		EntityType: "direct_connection", EntityID: connectionID,
		Metadata: json.RawMessage(`{}`), CreatedAt: now,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateDirectCampaign(
	ctx context.Context, actorUserID, workspaceID string, campaign DirectCampaign,
) (DirectCampaign, error) {
	if err := validateDirectCampaignDraft(&campaign); err != nil {
		return DirectCampaign{}, fmt.Errorf("%w: %w", ErrDirectValidation, err)
	}
	if campaign.ID == "" {
		campaign.ID = newStoreID("dcmp_")
	}
	now := campaign.CreatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DirectCampaign{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return DirectCampaign{}, err
	}
	var connection DirectConnection
	connection, err = scanDirectConnection(tx.QueryRowContext(ctx, `SELECT `+directConnectionColumns+`
FROM direct_connections WHERE workspace_id=$1 AND revoked_at IS NULL FOR SHARE`, workspaceID))
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
		return DirectCampaign{}, ErrDirectConnectionRequired
	}
	if err != nil {
		return DirectCampaign{}, err
	}
	if campaign.ConnectionID != "" && campaign.ConnectionID != connection.ID {
		return DirectCampaign{}, ErrDirectConnectionRequired
	}
	if campaign.CurrencyCode == "" {
		campaign.CurrencyCode = connection.CurrencyCode
	}
	if campaign.CurrencyCode != connection.CurrencyCode {
		return DirectCampaign{}, fmt.Errorf("%w: campaign currency must match the Direct account", ErrDirectValidation)
	}
	regionsJSON, err := json.Marshal(campaign.Regions)
	if err != nil {
		return DirectCampaign{}, err
	}
	titlesJSON, textsJSON, keywordsJSON, negativeKeywordsJSON, err :=
		marshalDirectCampaignDesiredLists(campaign)
	if err != nil {
		return DirectCampaign{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO direct_campaigns(
	id,workspace_id,connection_id,name,objective,landing_url,brief,regions,
	titles,texts,keywords,negative_keywords,weekly_budget_minor,currency_code,
	starts_at,ends_at,status,provider_status,provider_state,version,created_by,
	created_at,updated_at)
	VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,
	'draft','','',1,$17,$18,$18)`,
		campaign.ID, workspaceID, connection.ID, campaign.Name, campaign.Objective,
		campaign.LandingURL, campaign.Brief, string(regionsJSON), titlesJSON, textsJSON,
		keywordsJSON, negativeKeywordsJSON, campaign.WeeklyBudgetMinor,
		campaign.CurrencyCode, dateOnly(campaign.StartsAt), dateOnly(campaign.EndsAt),
		actorUserID, now)
	if err != nil {
		return DirectCampaign{}, mapWorkspaceWriteError("create Direct campaign", err)
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "direct.campaign.created",
		EntityType: "direct_campaign", EntityID: campaign.ID,
		Metadata: directCampaignAuditMetadata(campaign), CreatedAt: now,
	}); err != nil {
		return DirectCampaign{}, err
	}
	if err := tx.Commit(); err != nil {
		return DirectCampaign{}, err
	}
	return s.GetDirectCampaign(ctx, actorUserID, workspaceID, campaign.ID)
}

func (s *Store) ListDirectCampaigns(
	ctx context.Context, actorUserID, workspaceID string,
) ([]DirectCampaign, error) {
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+directCampaignColumns+`
FROM direct_campaigns WHERE workspace_id=$1 ORDER BY updated_at DESC,id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]DirectCampaign, 0)
	for rows.Next() {
		campaign, scanErr := scanDirectCampaign(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		campaign.AutoLaunch, scanErr = s.getDirectAutoLaunchSummary(ctx, workspaceID, campaign)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, campaign)
	}
	return result, rows.Err()
}

func (s *Store) GetDirectCampaign(
	ctx context.Context, actorUserID, workspaceID, campaignID string,
) (DirectCampaign, error) {
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID); err != nil {
		return DirectCampaign{}, err
	}
	campaign, err := scanDirectCampaign(s.db.QueryRowContext(ctx, `SELECT `+directCampaignColumns+`
FROM direct_campaigns WHERE workspace_id=$1 AND id=$2`, workspaceID, campaignID))
	if err != nil {
		return DirectCampaign{}, err
	}
	campaign.AutoLaunch, err = s.getDirectAutoLaunchSummary(ctx, workspaceID, campaign)
	return campaign, err
}

func (s *Store) UpdateDirectCampaignDraft(
	ctx context.Context, actorUserID, workspaceID, campaignID string, changes DirectCampaignChanges,
) (DirectCampaign, error) {
	if changes.ExpectedVersion <= 0 {
		return DirectCampaign{}, fmt.Errorf("%w: expected_version must be positive", ErrDirectValidation)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DirectCampaign{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return DirectCampaign{}, err
	}
	campaign, err := scanDirectCampaign(tx.QueryRowContext(ctx, `SELECT `+directCampaignColumns+`
FROM direct_campaigns WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspaceID, campaignID))
	if err != nil {
		return DirectCampaign{}, err
	}
	if campaign.Status != "draft" {
		return DirectCampaign{}, ErrDirectCampaignNotDraft
	}
	if campaign.Version != changes.ExpectedVersion {
		return DirectCampaign{}, ErrConflict
	}
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
		campaign.NegativeKeywords = append([]string(nil), (*changes.NegativeKeywords)...)
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
	if err := validateDirectCampaignDraft(&campaign); err != nil {
		return DirectCampaign{}, fmt.Errorf("%w: %w", ErrDirectValidation, err)
	}
	now := time.Now().UTC()
	regionsJSON, err := json.Marshal(campaign.Regions)
	if err != nil {
		return DirectCampaign{}, err
	}
	titlesJSON, textsJSON, keywordsJSON, negativeKeywordsJSON, err :=
		marshalDirectCampaignDesiredLists(campaign)
	if err != nil {
		return DirectCampaign{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE direct_campaigns
SET name=$1,objective=$2,landing_url=$3,brief=$4,regions=$5,titles=$6,
    texts=$7,keywords=$8,negative_keywords=$9,weekly_budget_minor=$10,
    starts_at=$11,ends_at=$12,version=version+1,updated_at=$13
WHERE workspace_id=$14 AND id=$15 AND version=$16 AND status='draft'`,
		campaign.Name, campaign.Objective, campaign.LandingURL, campaign.Brief, string(regionsJSON),
		titlesJSON, textsJSON, keywordsJSON, negativeKeywordsJSON,
		campaign.WeeklyBudgetMinor, dateOnly(campaign.StartsAt), dateOnly(campaign.EndsAt),
		now, workspaceID, campaignID, changes.ExpectedVersion)
	if err != nil {
		return DirectCampaign{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return DirectCampaign{}, ErrConflict
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "direct.campaign.updated",
		EntityType: "direct_campaign", EntityID: campaignID,
		Metadata: directCampaignAuditMetadata(campaign), CreatedAt: now,
	}); err != nil {
		return DirectCampaign{}, err
	}
	if err := tx.Commit(); err != nil {
		return DirectCampaign{}, err
	}
	return s.GetDirectCampaign(ctx, actorUserID, workspaceID, campaignID)
}

func (s *Store) MarkDirectCampaignSubmitted(
	ctx context.Context, actorUserID, workspaceID, campaignID string, expectedVersion int64,
	providerCampaignID int64, providerStatus, providerState string, now time.Time,
) (DirectCampaign, error) {
	if expectedVersion <= 0 || providerCampaignID <= 0 {
		return DirectCampaign{}, errors.New("expected version and provider campaign id are required")
	}
	providerStatus = normalizeDirectProviderStatus(providerStatus)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DirectCampaign{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleApprover); err != nil {
		return DirectCampaign{}, err
	}
	status := directCampaignStatusFromProvider(providerStatus)
	result, err := tx.ExecContext(ctx, `UPDATE direct_campaigns
SET provider_campaign_id=$1,status=$2,provider_status=$3,provider_state=$4,
submitted_at=$5,updated_at=$5
WHERE workspace_id=$6 AND id=$7 AND status='creating' AND version=$8`,
		providerCampaignID, status, providerStatus, strings.TrimSpace(providerState), now.UTC(),
		workspaceID, campaignID, expectedVersion)
	if err != nil {
		return DirectCampaign{}, mapWorkspaceWriteError("submit Direct campaign", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		var status string
		var version int64
		if scanErr := tx.QueryRowContext(ctx, `SELECT status,version FROM direct_campaigns
WHERE workspace_id=$1 AND id=$2`, workspaceID, campaignID).Scan(&status, &version); errors.Is(scanErr, sql.ErrNoRows) {
			return DirectCampaign{}, ErrNotFound
		}
		if status != "creating" {
			return DirectCampaign{}, ErrDirectCampaignNotDraft
		}
		return DirectCampaign{}, ErrConflict
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "direct.campaign.submitted",
		EntityType: "direct_campaign", EntityID: campaignID,
		Metadata:  mustJSON(map[string]any{"provider_campaign_id": providerCampaignID}),
		CreatedAt: now.UTC(),
	}); err != nil {
		return DirectCampaign{}, err
	}
	if err := tx.Commit(); err != nil {
		return DirectCampaign{}, err
	}
	return s.GetDirectCampaign(ctx, actorUserID, workspaceID, campaignID)
}

func (s *Store) ClaimDirectCampaignSubmission(
	ctx context.Context, actorUserID, workspaceID, campaignID string,
	expectedVersion int64, now time.Time,
) (DirectCampaign, DirectConnection, error) {
	if expectedVersion <= 0 {
		return DirectCampaign{}, DirectConnection{},
			fmt.Errorf("%w: expected_version must be positive", ErrDirectValidation)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DirectCampaign{}, DirectConnection{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleApprover); err != nil {
		return DirectCampaign{}, DirectConnection{}, err
	}
	campaign, err := scanDirectCampaign(tx.QueryRowContext(ctx, `SELECT `+directCampaignColumns+`
FROM direct_campaigns WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspaceID, campaignID))
	if err != nil {
		return DirectCampaign{}, DirectConnection{}, err
	}
	if campaign.Status != "draft" {
		return DirectCampaign{}, DirectConnection{}, ErrDirectCampaignNotDraft
	}
	if campaign.Version != expectedVersion {
		return DirectCampaign{}, DirectConnection{}, ErrConflict
	}
	connection, err := scanDirectConnection(tx.QueryRowContext(ctx, `SELECT `+directConnectionColumns+`
FROM direct_connections WHERE workspace_id=$1 AND id=$2 AND status='active' AND revoked_at IS NULL FOR SHARE`,
		workspaceID, campaign.ConnectionID))
	if err != nil {
		return DirectCampaign{}, DirectConnection{}, ErrDirectConnectionRequired
	}
	if connection.ReadOnly {
		return DirectCampaign{}, DirectConnection{}, ErrDirectConnectionRequired
	}
	now = now.UTC()
	result, err := tx.ExecContext(ctx, `UPDATE direct_campaigns
SET status='creating',submitted_at=$1,updated_at=$1
WHERE workspace_id=$2 AND id=$3 AND status='draft' AND version=$4`,
		now, workspaceID, campaignID, expectedVersion)
	if err != nil {
		return DirectCampaign{}, DirectConnection{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return DirectCampaign{}, DirectConnection{}, ErrConflict
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "direct.campaign.submission_claimed",
		EntityType: "direct_campaign", EntityID: campaignID,
		Metadata: mustJSON(map[string]any{"campaign_version": expectedVersion}), CreatedAt: now,
	}); err != nil {
		return DirectCampaign{}, DirectConnection{}, err
	}
	if err := tx.Commit(); err != nil {
		return DirectCampaign{}, DirectConnection{}, err
	}
	campaign.Status = "creating"
	campaign.SubmittedAt = &now
	campaign.UpdatedAt = now
	return campaign, connection, nil
}

func (s *Store) FailDirectCampaignSubmission(
	ctx context.Context, workspaceID, campaignID, failureCode string, now time.Time,
) error {
	failureCode = strings.TrimSpace(failureCode)
	if len(failureCode) > 128 {
		failureCode = failureCode[:128]
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE direct_campaigns
SET status='error',launch_failure_code=$1,updated_at=$2
WHERE workspace_id=$3 AND id=$4 AND status='creating'`,
		failureCode, now.UTC(), workspaceID, campaignID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrConflict
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, Action: "direct.campaign.submission_failed",
		EntityType: "direct_campaign", EntityID: campaignID,
		Metadata: mustJSON(map[string]any{"failure_code": failureCode}), CreatedAt: now.UTC(),
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SyncDirectCampaignProviderStatus(
	ctx context.Context, workspaceID, campaignID string, providerCampaignID int64,
	providerStatus, providerState string, now time.Time,
) (DirectCampaign, error) {
	return s.syncDirectCampaignProviderStatus(
		ctx, workspaceID, campaignID, providerCampaignID,
		providerStatus, providerState, nil, now,
	)
}

// SyncDirectCampaignProviderStatusForLaunch fences provider truth observed by a
// launch worker against the exact durable launch generation it owns.
func (s *Store) SyncDirectCampaignProviderStatusForLaunch(
	ctx context.Context, workspaceID, campaignID string, providerCampaignID int64,
	providerStatus, providerState string, expectedLaunchClaimedAt, now time.Time,
) (DirectCampaign, error) {
	expectedLaunchClaimedAt = expectedLaunchClaimedAt.UTC().Truncate(time.Microsecond)
	if expectedLaunchClaimedAt.IsZero() {
		return DirectCampaign{}, ErrConflict
	}
	return s.syncDirectCampaignProviderStatus(
		ctx, workspaceID, campaignID, providerCampaignID,
		providerStatus, providerState, &expectedLaunchClaimedAt, now,
	)
}

func (s *Store) syncDirectCampaignProviderStatus(
	ctx context.Context, workspaceID, campaignID string, providerCampaignID int64,
	providerStatus, providerState string, expectedLaunchClaimedAt *time.Time,
	now time.Time,
) (DirectCampaign, error) {
	providerStatus = normalizeDirectProviderStatus(providerStatus)
	providerState = strings.ToUpper(strings.TrimSpace(providerState))
	status := directCampaignStatusFromProviderLifecycle(providerStatus, providerState)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DirectCampaign{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var currentStatus, currentProviderStatus, currentProviderState, currentLaunchState string
	var currentLaunchClaimedAt sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT status,provider_status,provider_state,launch_state,
       launch_claimed_at
FROM direct_campaigns WHERE workspace_id=$1 AND id=$2 AND provider_campaign_id=$3 FOR UPDATE`,
		workspaceID, campaignID, providerCampaignID).Scan(
		&currentStatus, &currentProviderStatus, &currentProviderState, &currentLaunchState,
		&currentLaunchClaimedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DirectCampaign{}, ErrNotFound
	}
	if err != nil {
		return DirectCampaign{}, err
	}
	if expectedLaunchClaimedAt != nil {
		if !currentLaunchClaimedAt.Valid ||
			!currentLaunchClaimedAt.Time.UTC().Truncate(time.Microsecond).
				Equal(*expectedLaunchClaimedAt) {
			return DirectCampaign{}, ErrConflict
		}
	} else if currentLaunchState == "launching" ||
		currentLaunchState == "reconciling" {
		// Launch workers must use the fenced entry point. Ordinary lifecycle
		// polling never owns an in-flight launch transition.
		return DirectCampaign{}, ErrConflict
	}
	if providerStatus == "ACCEPTED" && providerState == "OFF" &&
		(currentStatus == "active" || currentStatus == "suspended") {
		status = "suspended"
	}
	if currentLaunchState == "confirmed" {
		status = directConfirmedCampaignStatus(providerStatus, providerState, currentStatus)
	}
	if currentLaunchState == "launching" || currentLaunchState == "reconciling" {
		status = currentStatus
	}
	if currentLaunchState == "failed" {
		// A failed launch claim is deliberately retained as an ambiguous,
		// spend-capable state. Persist provider truth without manufacturing a
		// local lifecycle combination rejected by the launch-state constraint.
		status = currentStatus
	}
	promoteDelayedLaunch := (currentLaunchState == "failed" ||
		currentLaunchState == "launching" || currentLaunchState == "reconciling") &&
		providerStatus == "ACCEPTED" && providerState == "ON"
	if promoteDelayedLaunch {
		result, updateErr := tx.ExecContext(ctx, `UPDATE direct_campaigns
SET status='active',provider_status=$1,provider_state=$2,
    provider_next_check_at=$3,launch_state='confirmed',
    launch_reconcile_after=NULL,launched_at=$4,launch_failed_at=NULL,
    launch_failure_code='',updated_at=$4
WHERE workspace_id=$5 AND id=$6
  AND launch_state IN ('failed','launching','reconciling')`,
			providerStatus, providerState, now.UTC().Add(directProviderPollLease),
			now.UTC(), workspaceID, campaignID)
		if updateErr != nil {
			return DirectCampaign{}, updateErr
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return DirectCampaign{}, ErrConflict
		}
		if err := appendAuditEventTx(ctx, tx, AuditEvent{
			WorkspaceID: workspaceID, Action: "direct.campaign.delayed_launch_observed",
			EntityType: "direct_campaign", EntityID: campaignID,
			Metadata: mustJSON(map[string]any{
				"provider_status": providerStatus, "provider_state": providerState,
			}), CreatedAt: now.UTC(),
		}); err != nil {
			return DirectCampaign{}, err
		}
	} else if currentStatus != status || currentProviderStatus != providerStatus || currentProviderState != providerState {
		if _, err := tx.ExecContext(ctx, `UPDATE direct_campaigns
SET status=$1,provider_status=$2,provider_state=$3,
    provider_next_check_at=$4,updated_at=$5
WHERE workspace_id=$6 AND id=$7`, status, providerStatus, providerState,
			now.UTC().Add(directProviderPollLease), now.UTC(), workspaceID, campaignID); err != nil {
			return DirectCampaign{}, err
		}
		if err := appendAuditEventTx(ctx, tx, AuditEvent{
			WorkspaceID: workspaceID, Action: "direct.campaign.provider_status_synced",
			EntityType: "direct_campaign", EntityID: campaignID,
			Metadata:  mustJSON(map[string]any{"provider_status": providerStatus, "provider_state": providerState}),
			CreatedAt: now.UTC(),
		}); err != nil {
			return DirectCampaign{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return DirectCampaign{}, err
	}
	campaign, err := scanDirectCampaign(s.db.QueryRowContext(ctx, `SELECT `+directCampaignColumns+`
FROM direct_campaigns WHERE workspace_id=$1 AND id=$2`, workspaceID, campaignID))
	if err != nil {
		return DirectCampaign{}, err
	}
	campaign.AutoLaunch, err = s.getDirectAutoLaunchSummary(ctx, workspaceID, campaign)
	return campaign, err
}

func (s *Store) GrantDirectAutoLaunchConsent(
	ctx context.Context, actorUserID, workspaceID, campaignID string, request DirectConsentRequest,
) (DirectAutoLaunchConsent, error) {
	if request.Confirmation != "АВТОЗАПУСК" || request.ExpectedVersion <= 0 ||
		strings.TrimSpace(request.ExpectedConnectionID) == "" ||
		strings.TrimSpace(request.ExpectedAccountID) == "" ||
		strings.TrimSpace(request.ExpectedCampaignName) == "" ||
		request.ExpectedProviderID <= 0 ||
		!directGraphHashPattern.MatchString(strings.TrimSpace(request.ExpectedGraphHash)) ||
		strings.TrimSpace(request.ExpectedRevisionID) == "" ||
		request.WeeklyBudgetMinor <= 0 || request.StartsAt.IsZero() || request.EndsAt.IsZero() {
		return DirectAutoLaunchConsent{}, ErrDirectConsentMismatch
	}
	if request.AuthorizedAt.IsZero() {
		request.AuthorizedAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DirectAutoLaunchConsent{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner); err != nil {
		return DirectAutoLaunchConsent{}, err
	}
	campaign, err := scanDirectCampaign(tx.QueryRowContext(ctx, `SELECT `+directCampaignColumns+`
FROM direct_campaigns WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspaceID, campaignID))
	if err != nil {
		return DirectAutoLaunchConsent{}, err
	}
	if err := directCampaignGraphLaunchReady(campaign); err != nil {
		return DirectAutoLaunchConsent{}, err
	}
	if campaign.LaunchState != "idle" {
		return DirectAutoLaunchConsent{}, ErrDirectConsentMismatch
	}
	connection, err := scanDirectConnection(tx.QueryRowContext(ctx, `SELECT `+directConnectionColumns+`
FROM direct_connections
WHERE workspace_id=$1 AND id=$2 AND status='active' AND read_only=FALSE
  AND revoked_at IS NULL FOR SHARE`,
		workspaceID, campaign.ConnectionID))
	if err != nil {
		return DirectAutoLaunchConsent{}, ErrDirectConnectionRequired
	}
	if campaign.Version != request.ExpectedVersion ||
		campaign.ConnectionID != strings.TrimSpace(request.ExpectedConnectionID) ||
		connection.AccountID != strings.TrimSpace(request.ExpectedAccountID) ||
		campaign.Name != strings.TrimSpace(request.ExpectedCampaignName) ||
		*campaign.ProviderCampaignID != request.ExpectedProviderID ||
		campaign.ProviderGraphHash != strings.TrimSpace(request.ExpectedGraphHash) ||
		campaign.ProviderRevisionID != strings.TrimSpace(request.ExpectedRevisionID) ||
		campaign.WeeklyBudgetMinor != request.WeeklyBudgetMinor ||
		!sameDate(campaign.StartsAt, request.StartsAt) || !sameDate(campaign.EndsAt, request.EndsAt) {
		return DirectAutoLaunchConsent{}, ErrDirectConsentMismatch
	}
	existing, existingErr := scanDirectConsent(tx.QueryRowContext(ctx, `SELECT `+directConsentColumns+`
	FROM direct_auto_launch_consents_v2
WHERE workspace_id=$1 AND campaign_id=$2
  AND revoked_at IS NULL AND invalidated_at IS NULL AND consumed_at IS NULL
FOR UPDATE`, workspaceID, campaignID))
	if existingErr == nil {
		if directConsentMatches(existing, campaign, connection) {
			return existing, tx.Commit()
		}
		return DirectAutoLaunchConsent{}, ErrDirectConsentMismatch
	}
	if !errors.Is(existingErr, ErrNotFound) {
		return DirectAutoLaunchConsent{}, existingErr
	}
	consent := DirectAutoLaunchConsent{
		ID: newStoreID("dcons_"), WorkspaceID: workspaceID, CampaignID: campaignID,
		ConnectionID: campaign.ConnectionID, ActorUserID: actorUserID,
		ConsentVersion: DirectAutoLaunchConsentVersion, Confirmation: request.Confirmation,
		CampaignVersion: campaign.Version, AccountID: connection.AccountID,
		ProviderCampaignID: campaign.ProviderCampaignID,
		CampaignName:       campaign.Name,
		WeeklyBudgetMinor:  campaign.WeeklyBudgetMinor, CurrencyCode: campaign.CurrencyCode,
		StartsAt: dateOnly(campaign.StartsAt), EndsAt: dateOnly(campaign.EndsAt),
		ExpectedGraphHash:  campaign.ProviderGraphHash,
		ExpectedRevisionID: campaign.ProviderRevisionID,
		AuthorizedAt:       request.AuthorizedAt.UTC(),
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO direct_auto_launch_consents_v2(
id,workspace_id,campaign_id,connection_id,actor_user_id,consent_version,confirmation,
campaign_version,account_id,provider_campaign_id,weekly_budget_minor,currency_code,
campaign_name,starts_at,ends_at,expected_graph_hash,expected_revision_id,authorized_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		consent.ID, consent.WorkspaceID, consent.CampaignID, consent.ConnectionID,
		consent.ActorUserID, consent.ConsentVersion, consent.Confirmation,
		consent.CampaignVersion, consent.AccountID, consent.ProviderCampaignID,
		consent.WeeklyBudgetMinor, consent.CurrencyCode, consent.CampaignName,
		consent.StartsAt, consent.EndsAt, consent.ExpectedGraphHash,
		consent.ExpectedRevisionID, consent.AuthorizedAt)
	if err != nil {
		return DirectAutoLaunchConsent{}, mapWorkspaceWriteError("grant Direct auto-launch consent", err)
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "direct.auto_launch.authorized",
		EntityType: "direct_campaign", EntityID: campaignID,
		Metadata: mustJSON(map[string]any{
			"consent_version": consent.ConsentVersion, "campaign_version": consent.CampaignVersion,
			"campaign_name": consent.CampaignName,
			"account_id":    consent.AccountID, "provider_campaign_id": consent.ProviderCampaignID,
			"weekly_budget_minor": consent.WeeklyBudgetMinor, "currency_code": consent.CurrencyCode,
			"starts_at": consent.StartsAt.Format(time.DateOnly), "ends_at": consent.EndsAt.Format(time.DateOnly),
			"graph_hash": consent.ExpectedGraphHash, "revision_id": consent.ExpectedRevisionID,
		}), CreatedAt: consent.AuthorizedAt,
	}); err != nil {
		return DirectAutoLaunchConsent{}, err
	}
	if err := tx.Commit(); err != nil {
		return DirectAutoLaunchConsent{}, err
	}
	return consent, nil
}

func (s *Store) RevokeDirectAutoLaunchConsent(
	ctx context.Context, actorUserID, workspaceID, campaignID string, now time.Time,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE direct_auto_launch_consents_v2
SET revoked_at=$1
WHERE workspace_id=$2 AND campaign_id=$3
  AND revoked_at IS NULL AND invalidated_at IS NULL AND consumed_at IS NULL`,
		now.UTC(), workspaceID, campaignID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotFound
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "direct.auto_launch.revoked",
		EntityType: "direct_campaign", EntityID: campaignID, Metadata: json.RawMessage(`{}`),
		CreatedAt: now.UTC(),
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) InvalidateDirectAutoLaunchConsent(
	ctx context.Context, workspaceID, campaignID, reason string, now time.Time,
) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "provider_state_changed"
	}
	if len(reason) > 128 {
		reason = reason[:128]
	}
	result, err := s.db.ExecContext(ctx, `UPDATE direct_auto_launch_consents_v2
SET invalidated_at=$1,invalid_reason=$2
WHERE workspace_id=$3 AND campaign_id=$4
  AND revoked_at IS NULL AND invalidated_at IS NULL AND consumed_at IS NULL`,
		now.UTC(), reason, workspaceID, campaignID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrDirectConsentRequired
	}
	return nil
}

func (s *Store) SetDirectCampaignProviderSnapshotMismatch(
	ctx context.Context, workspaceID, campaignID string, mismatch bool, now time.Time,
) error {
	return s.setDirectCampaignProviderSnapshotMismatch(
		ctx, workspaceID, campaignID, mismatch, nil, now,
	)
}

// SetDirectCampaignProviderSnapshotMismatchForLaunch fences snapshot evidence
// written by a launch worker against the exact durable launch generation it
// owns.
func (s *Store) SetDirectCampaignProviderSnapshotMismatchForLaunch(
	ctx context.Context, workspaceID, campaignID string, mismatch bool,
	expectedLaunchClaimedAt, now time.Time,
) error {
	expectedLaunchClaimedAt = expectedLaunchClaimedAt.UTC().Truncate(time.Microsecond)
	if expectedLaunchClaimedAt.IsZero() {
		return ErrConflict
	}
	return s.setDirectCampaignProviderSnapshotMismatch(
		ctx, workspaceID, campaignID, mismatch, &expectedLaunchClaimedAt, now,
	)
}

func (s *Store) setDirectCampaignProviderSnapshotMismatch(
	ctx context.Context, workspaceID, campaignID string, mismatch bool,
	expectedLaunchClaimedAt *time.Time, now time.Time,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var launchState string
	var launchClaimedAt sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT launch_state,launch_claimed_at
FROM direct_campaigns
WHERE workspace_id=$1 AND id=$2
FOR UPDATE`, workspaceID, campaignID).Scan(&launchState, &launchClaimedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if expectedLaunchClaimedAt != nil {
		if !launchClaimedAt.Valid ||
			!launchClaimedAt.Time.UTC().Truncate(time.Microsecond).
				Equal(*expectedLaunchClaimedAt) {
			return ErrConflict
		}
	} else if launchState == "launching" || launchState == "reconciling" {
		// Launch workers must use the fenced entry point. Ordinary lifecycle
		// polling never owns an in-flight launch transition.
		return ErrConflict
	}
	if mismatch {
		if _, err := tx.ExecContext(ctx, `UPDATE direct_campaigns
SET launch_failure_code=CASE
      WHEN launch_state='failed' THEN launch_failure_code
      ELSE 'provider_snapshot_mismatch'
    END,
    updated_at=$1
WHERE workspace_id=$2 AND id=$3 AND provider_campaign_id IS NOT NULL`,
			now.UTC(), workspaceID, campaignID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE direct_auto_launch_consents_v2
SET invalidated_at=$1,invalid_reason='provider_snapshot_changed'
WHERE workspace_id=$2 AND campaign_id=$3
  AND revoked_at IS NULL AND invalidated_at IS NULL AND consumed_at IS NULL`,
			now.UTC(), workspaceID, campaignID); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE direct_campaigns
SET launch_failure_code='',updated_at=$1
WHERE workspace_id=$2 AND id=$3
  AND launch_failure_code='provider_snapshot_mismatch'`,
			now.UTC(), workspaceID, campaignID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ClaimDirectAutoLaunchCandidates(
	ctx context.Context, now time.Time, limit int,
) ([]string, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	now = now.UTC()
	rows, err := s.db.QueryContext(ctx, `WITH candidates AS (
    SELECT c.workspace_id,c.id
    FROM direct_campaigns c
    JOIN workspaces w ON w.id=c.workspace_id AND w.archived_at IS NULL
    JOIN direct_connections cn
      ON cn.workspace_id=c.workspace_id AND cn.id=c.connection_id
    JOIN direct_auto_launch_consents_v2 ac
      ON ac.workspace_id=c.workspace_id AND ac.campaign_id=c.id
    WHERE c.status = 'accepted'
      AND c.launch_state = 'idle'
      AND c.provider_status = 'ACCEPTED'
      AND c.provider_campaign_id IS NOT NULL
      AND c.graph_verified_at IS NOT NULL
      AND c.provider_graph_hash <> ''
      AND c.provider_revision_id IS NOT NULL
      AND c.moderation_status = 'ACCEPTED'
      AND c.auto_launch_next_attempt_at <= $1
      AND c.starts_at <= ($1 AT TIME ZONE 'Europe/Moscow')::date
      AND c.ends_at >= ($1 AT TIME ZONE 'Europe/Moscow')::date
      AND cn.status='active' AND cn.read_only=FALSE AND cn.revoked_at IS NULL
      AND ac.revoked_at IS NULL AND ac.invalidated_at IS NULL AND ac.consumed_at IS NULL
      AND ac.expected_graph_hash = c.provider_graph_hash
      AND ac.expected_revision_id = c.provider_revision_id
    ORDER BY CASE WHEN c.status='accepted' THEN 0 ELSE 1 END,
             c.auto_launch_next_attempt_at,ac.authorized_at,c.id
    FOR UPDATE OF c SKIP LOCKED
    LIMIT $2
)
UPDATE direct_campaigns c
SET auto_launch_next_attempt_at=$3,updated_at=$1
FROM candidates x
WHERE c.workspace_id=x.workspace_id AND c.id=x.id
RETURNING c.id`, now, limit, now.Add(directProviderPollLease))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

func (s *Store) ClaimDirectProviderSyncCandidates(
	ctx context.Context, now time.Time, limit int,
) ([]DirectLaunchRecoveryCandidate, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	now = now.UTC()
	rows, err := s.db.QueryContext(ctx, `WITH candidates AS (
    SELECT c.workspace_id,c.id
    FROM direct_campaigns c
    JOIN workspaces w ON w.id=c.workspace_id AND w.archived_at IS NULL
    JOIN direct_connections cn
      ON cn.workspace_id=c.workspace_id AND cn.id=c.connection_id
    WHERE c.status IN ('provider_draft','moderation','accepted','rejected','active','suspended')
      AND c.launch_state NOT IN ('launching','reconciling')
      AND c.provider_campaign_id IS NOT NULL
      AND c.provider_next_check_at <= $1
      AND cn.status='active' AND cn.revoked_at IS NULL
    ORDER BY c.provider_next_check_at,c.id
    FOR UPDATE OF c SKIP LOCKED
    LIMIT $2
)
UPDATE direct_campaigns c
SET provider_next_check_at=$3,updated_at=$1
FROM candidates x
WHERE c.workspace_id=x.workspace_id AND c.id=x.id
RETURNING c.workspace_id,c.id`, now, limit, now.Add(directProviderPollLease))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]DirectLaunchRecoveryCandidate, 0)
	for rows.Next() {
		var candidate DirectLaunchRecoveryCandidate
		if err := rows.Scan(&candidate.WorkspaceID, &candidate.CampaignID); err != nil {
			return nil, err
		}
		result = append(result, candidate)
	}
	return result, rows.Err()
}

func (s *Store) ClaimDirectLaunchRecoveryCandidates(
	ctx context.Context, now time.Time, limit int,
) ([]DirectLaunchRecoveryCandidate, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	now = now.UTC().Truncate(time.Microsecond)
	rows, err := s.db.QueryContext(ctx, `WITH candidates AS (
    SELECT workspace_id,id
    FROM direct_campaigns
    WHERE launch_state IN ('launching','reconciling')
      AND launch_reconcile_after <= $1
    ORDER BY launch_reconcile_after,id
    FOR UPDATE SKIP LOCKED
    LIMIT $2
)
UPDATE direct_campaigns c
SET launch_claimed_at=GREATEST(
        $1,
        c.launch_claimed_at + INTERVAL '1 microsecond'
    ),
    launch_reconcile_after=$3,updated_at=$1
FROM candidates x
WHERE c.workspace_id=x.workspace_id AND c.id=x.id
RETURNING c.workspace_id,c.id`, now, limit, now.Add(directLaunchRecoveryLease))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]DirectLaunchRecoveryCandidate, 0)
	for rows.Next() {
		var candidate DirectLaunchRecoveryCandidate
		if err := rows.Scan(&candidate.WorkspaceID, &candidate.CampaignID); err != nil {
			return nil, err
		}
		result = append(result, candidate)
	}
	return result, rows.Err()
}

// GetDirectAutoLaunchMaterial is intentionally worker-scoped. Interactive
// launch paths must use GetDirectManualLaunchMaterial so that the workspace in
// the URL and the actor authorization are both enforced by the store.
func (s *Store) GetDirectAutoLaunchMaterial(
	ctx context.Context, campaignID string,
) (DirectLaunchMaterial, error) {
	var material DirectLaunchMaterial
	campaign, err := scanDirectCampaign(s.db.QueryRowContext(ctx, `SELECT `+directCampaignColumns+`
FROM direct_campaigns WHERE id=$1`, campaignID))
	if err != nil {
		return material, err
	}
	connection, err := s.getDirectConnectionForWorker(ctx, campaign.WorkspaceID, campaign.ConnectionID)
	if err != nil {
		return material, ErrDirectConnectionRequired
	}
	consent, err := scanDirectConsent(s.db.QueryRowContext(ctx, `SELECT `+directConsentColumns+`
FROM direct_auto_launch_consents_v2
WHERE workspace_id=$1 AND campaign_id=$2
  AND revoked_at IS NULL AND invalidated_at IS NULL AND consumed_at IS NULL`,
		campaign.WorkspaceID, campaign.ID))
	if err != nil {
		return material, ErrDirectConsentRequired
	}
	if !directConsentMatches(consent, campaign, connection) {
		return material, ErrDirectConsentMismatch
	}
	material.Campaign, material.Connection, material.Consent = campaign, connection, consent
	material.TokenCiphertext, material.TokenKeyVersion = connection.TokenCiphertext, connection.TokenKeyVersion
	return material, nil
}

func (s *Store) GetDirectManualLaunchMaterial(
	ctx context.Context, actorUserID, workspaceID, campaignID string,
) (DirectLaunchMaterial, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DirectLaunchMaterial{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireWorkspaceRole(
		ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner,
	); err != nil {
		return DirectLaunchMaterial{}, err
	}
	campaign, err := scanDirectCampaign(tx.QueryRowContext(ctx, `SELECT `+directCampaignColumns+`
FROM direct_campaigns WHERE workspace_id=$1 AND id=$2`, workspaceID, campaignID))
	if err != nil {
		return DirectLaunchMaterial{}, err
	}
	connection, err := scanDirectConnection(tx.QueryRowContext(ctx, `SELECT `+directConnectionColumns+`
FROM direct_connections
WHERE workspace_id=$1 AND id=$2 AND status='active' AND revoked_at IS NULL`,
		workspaceID, campaign.ConnectionID))
	if err != nil || connection.ReadOnly {
		return DirectLaunchMaterial{}, ErrDirectConnectionRequired
	}
	return DirectLaunchMaterial{
		Campaign: campaign, Connection: connection,
		TokenCiphertext: connection.TokenCiphertext, TokenKeyVersion: connection.TokenKeyVersion,
	}, nil
}

func (s *Store) GetDirectLaunchRecoveryMaterial(
	ctx context.Context, workspaceID, campaignID string,
) (DirectLaunchMaterial, error) {
	campaign, err := scanDirectCampaign(s.db.QueryRowContext(ctx, `SELECT `+directCampaignColumns+`
FROM direct_campaigns
WHERE workspace_id=$1 AND id=$2 AND launch_state IN ('launching','reconciling')`,
		workspaceID, campaignID))
	if err != nil {
		return DirectLaunchMaterial{}, err
	}
	connection, err := s.getDirectConnectionForWorker(ctx, workspaceID, campaign.ConnectionID)
	if err != nil || connection.Status != "active" || connection.ReadOnly ||
		connection.TokenCiphertext == "" {
		return DirectLaunchMaterial{}, ErrDirectConnectionRequired
	}
	return DirectLaunchMaterial{
		Campaign: campaign, Connection: connection,
		TokenCiphertext: connection.TokenCiphertext, TokenKeyVersion: connection.TokenKeyVersion,
	}, nil
}

func (s *Store) GetDirectLifecycleMaterial(
	ctx context.Context, workspaceID, campaignID string,
) (DirectLaunchMaterial, error) {
	campaign, err := scanDirectCampaign(s.db.QueryRowContext(ctx, `SELECT `+directCampaignColumns+`
FROM direct_campaigns
WHERE workspace_id=$1 AND id=$2
  AND status IN ('provider_draft','moderation','accepted','rejected','active','suspended')
  AND provider_campaign_id IS NOT NULL`, workspaceID, campaignID))
	if err != nil {
		return DirectLaunchMaterial{}, err
	}
	connection, err := s.getDirectConnectionForWorker(ctx, workspaceID, campaign.ConnectionID)
	if err != nil || connection.Status != "active" || connection.TokenCiphertext == "" {
		return DirectLaunchMaterial{}, ErrDirectConnectionRequired
	}
	return DirectLaunchMaterial{
		Campaign: campaign, Connection: connection,
		TokenCiphertext: connection.TokenCiphertext, TokenKeyVersion: connection.TokenKeyVersion,
	}, nil
}

func (s *Store) ClaimDirectAutoCampaignLaunch(
	ctx context.Context, workspaceID, campaignID string, expectedCampaignVersion int64,
	expectedProviderCampaignID int64, expectedAccountID string, expectedBudgetMinor int64,
	expectedStartsAt, expectedEndsAt time.Time, expectedGraphHash, expectedRevisionID string,
	now time.Time,
) (DirectLaunchMaterial, error) {
	return s.claimDirectCampaignLaunch(
		ctx, "", workspaceID, campaignID, "auto", true, expectedCampaignVersion,
		expectedProviderCampaignID, expectedAccountID, expectedBudgetMinor,
		expectedStartsAt, expectedEndsAt, expectedGraphHash, expectedRevisionID, now,
	)
}

func (s *Store) ClaimDirectManualCampaignLaunch(
	ctx context.Context, actorUserID, workspaceID, campaignID string, expectedCampaignVersion int64,
	expectedProviderCampaignID int64, expectedAccountID string, expectedBudgetMinor int64,
	expectedStartsAt, expectedEndsAt time.Time, expectedGraphHash, expectedRevisionID string,
	now time.Time,
) (DirectLaunchMaterial, error) {
	return s.claimDirectCampaignLaunch(
		ctx, actorUserID, workspaceID, campaignID, "manual", false, expectedCampaignVersion,
		expectedProviderCampaignID, expectedAccountID, expectedBudgetMinor,
		expectedStartsAt, expectedEndsAt, expectedGraphHash, expectedRevisionID, now,
	)
}

// claimDirectCampaignLaunch persists a recoverable claim before any provider
// write. A provider timeout is never converted into a definitive local error:
// the claim remains visible to the reconciliation worker until Direct confirms
// that the campaign is running.
func (s *Store) claimDirectCampaignLaunch(
	ctx context.Context, actorUserID, workspaceID, campaignID, launchMode string,
	requireConsent bool, expectedCampaignVersion int64,
	expectedProviderCampaignID int64, expectedAccountID string, expectedBudgetMinor int64,
	expectedStartsAt, expectedEndsAt time.Time, expectedGraphHash, expectedRevisionID string,
	now time.Time,
) (DirectLaunchMaterial, error) {
	if launchMode != "manual" && launchMode != "auto" {
		return DirectLaunchMaterial{}, errors.New("invalid Direct launch mode")
	}
	now = now.UTC().Truncate(time.Microsecond)
	if now.IsZero() {
		now = time.Now().UTC().Truncate(time.Microsecond)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DirectLaunchMaterial{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var lockedWorkspaceID string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID,
	).Scan(&lockedWorkspaceID); err != nil {
		return DirectLaunchMaterial{}, mapWorkspaceWriteError(
			"lock workspace for Direct budget", err,
		)
	}
	campaign, err := scanDirectCampaign(tx.QueryRowContext(ctx, `SELECT `+directCampaignColumns+`
FROM direct_campaigns WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspaceID, campaignID))
	if err != nil {
		return DirectLaunchMaterial{}, err
	}
	if actorUserID != "" {
		if err := requireWorkspaceRole(ctx, tx, actorUserID, workspaceID, WorkspaceRoleOwner); err != nil {
			return DirectLaunchMaterial{}, err
		}
	}
	canClaim := campaign.LaunchState == "idle" ||
		(launchMode == "manual" && campaign.LaunchState == "failed")
	if !canClaim || campaign.Status == "active" || campaign.LaunchedAt != nil {
		return DirectLaunchMaterial{}, ErrDirectLaunchAlreadyClaimed
	}
	if err := directCampaignGraphLaunchReady(campaign); err != nil {
		return DirectLaunchMaterial{}, err
	}
	if campaign.Status != "accepted" || campaign.ProviderStatus != "ACCEPTED" ||
		campaign.ProviderCampaignID == nil || *campaign.ProviderCampaignID != expectedProviderCampaignID ||
		campaign.Version != expectedCampaignVersion || campaign.WeeklyBudgetMinor != expectedBudgetMinor ||
		campaign.ProviderGraphHash != strings.TrimSpace(expectedGraphHash) ||
		campaign.ProviderRevisionID != strings.TrimSpace(expectedRevisionID) ||
		!sameDate(campaign.StartsAt, expectedStartsAt) || !sameDate(campaign.EndsAt, expectedEndsAt) {
		return DirectLaunchMaterial{}, ErrDirectConsentMismatch
	}
	if campaign.WeeklyBudgetMinor > DirectMaxCampaignWeeklyBudgetMinor {
		return DirectLaunchMaterial{}, ErrDirectBudgetCapExceeded
	}
	var committedWeeklyBudgetMinor int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(weekly_budget_minor),0)
FROM direct_campaigns
WHERE workspace_id=$1 AND id<>$2
  AND (
    status IN ('active','suspended')
    OR launch_state IN ('launching','reconciling','confirmed','failed')
  )`, workspaceID, campaignID).Scan(&committedWeeklyBudgetMinor); err != nil {
		return DirectLaunchMaterial{}, err
	}
	if committedWeeklyBudgetMinor > DirectMaxWorkspaceWeeklyBudgetMinor-campaign.WeeklyBudgetMinor {
		return DirectLaunchMaterial{}, ErrDirectBudgetCapExceeded
	}
	connection, err := scanDirectConnection(tx.QueryRowContext(ctx, `SELECT `+directConnectionColumns+`
FROM direct_connections WHERE workspace_id=$1 AND id=$2 AND status='active' AND revoked_at IS NULL FOR SHARE`,
		campaign.WorkspaceID, campaign.ConnectionID))
	if err != nil || connection.AccountID != expectedAccountID {
		return DirectLaunchMaterial{}, ErrDirectConnectionRequired
	}
	if connection.ReadOnly {
		return DirectLaunchMaterial{}, ErrDirectConnectionRequired
	}
	var consent DirectAutoLaunchConsent
	if requireConsent {
		consent, err = scanDirectConsent(tx.QueryRowContext(ctx, `SELECT `+directConsentColumns+`
FROM direct_auto_launch_consents_v2
WHERE workspace_id=$1 AND campaign_id=$2
  AND revoked_at IS NULL AND invalidated_at IS NULL AND consumed_at IS NULL
FOR UPDATE`, campaign.WorkspaceID, campaign.ID))
		if err != nil {
			return DirectLaunchMaterial{}, ErrDirectConsentRequired
		}
		if !directConsentMatches(consent, campaign, connection) {
			return DirectLaunchMaterial{}, ErrDirectConsentMismatch
		}
	}
	if requireConsent {
		if _, err := tx.ExecContext(ctx, `UPDATE direct_auto_launch_consents_v2 SET consumed_at=$1
WHERE id=$2 AND consumed_at IS NULL`, now, consent.ID); err != nil {
			return DirectLaunchMaterial{}, err
		}
	}
	launchClaimedAt := now
	if campaign.LaunchClaimedAt != nil &&
		!launchClaimedAt.After(campaign.LaunchClaimedAt.UTC()) {
		launchClaimedAt = campaign.LaunchClaimedAt.UTC().
			Truncate(time.Microsecond).Add(time.Microsecond)
	}
	result, err := tx.ExecContext(ctx, `UPDATE direct_campaigns
SET launch_claimed_at=$1,launch_state='launching',launch_mode=$2,
    launch_attempt_count=0,launch_reconcile_after=$3,launch_failed_at=NULL,
    launch_failure_code='',updated_at=$1
WHERE workspace_id=$4 AND id=$5 AND status='accepted'
  AND (launch_state='idle' OR (launch_state='failed' AND $2='manual'))`,
		launchClaimedAt, launchMode, now.Add(directLaunchRecoveryLease),
		campaign.WorkspaceID, campaign.ID)
	if err != nil {
		return DirectLaunchMaterial{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return DirectLaunchMaterial{}, ErrDirectLaunchAlreadyClaimed
	}
	metadata := map[string]any{
		"launch_mode": launchMode, "provider_campaign_id": expectedProviderCampaignID,
		"weekly_budget_minor": expectedBudgetMinor, "currency_code": campaign.CurrencyCode,
	}
	if requireConsent {
		metadata["authorized_by"] = consent.ActorUserID
		metadata["consent_id"] = consent.ID
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: campaign.WorkspaceID, ActorUserID: actorUserID, Action: "direct.campaign.launch_claimed",
		EntityType: "direct_campaign", EntityID: campaign.ID,
		Metadata: mustJSON(metadata), CreatedAt: now,
	}); err != nil {
		return DirectLaunchMaterial{}, err
	}
	if err := tx.Commit(); err != nil {
		return DirectLaunchMaterial{}, err
	}
	consumed := now
	if requireConsent {
		consent.ConsumedAt = &consumed
	}
	campaign.LaunchClaimedAt = &launchClaimedAt
	campaign.LaunchState = "launching"
	campaign.LaunchMode = launchMode
	campaign.LaunchAttemptCount = 0
	reconcileAfter := now.Add(directLaunchRecoveryLease)
	campaign.LaunchReconcileAfter = &reconcileAfter
	campaign.UpdatedAt = now
	return DirectLaunchMaterial{
		Campaign: campaign, Connection: connection, Consent: consent,
		TokenCiphertext: connection.TokenCiphertext, TokenKeyVersion: connection.TokenKeyVersion,
	}, nil
}

func (s *Store) MarkDirectCampaignLaunchAttempt(
	ctx context.Context, workspaceID, campaignID string,
	expectedLaunchClaimedAt, now time.Time,
) error {
	expectedLaunchClaimedAt = expectedLaunchClaimedAt.UTC().Truncate(time.Microsecond)
	if expectedLaunchClaimedAt.IsZero() {
		return ErrConflict
	}
	result, err := s.db.ExecContext(ctx, `UPDATE direct_campaigns
SET launch_state='launching',launch_attempt_count=launch_attempt_count+1,
    launch_reconcile_after=$2,launch_failure_code='',updated_at=$1
WHERE workspace_id=$3 AND id=$4
  AND launch_state IN ('launching','reconciling') AND launch_attempt_count<2
  AND launch_claimed_at=$5`,
		now.UTC(), now.UTC().Add(directLaunchRecoveryLease), workspaceID, campaignID,
		expectedLaunchClaimedAt)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrDirectLaunchRetryExhausted
	}
	return nil
}

func (s *Store) MarkDirectCampaignLaunchReconciling(
	ctx context.Context, workspaceID, campaignID string,
	expectedLaunchClaimedAt time.Time, failureCode string, now time.Time,
) error {
	expectedLaunchClaimedAt = expectedLaunchClaimedAt.UTC().Truncate(time.Microsecond)
	if expectedLaunchClaimedAt.IsZero() {
		return ErrConflict
	}
	failureCode = strings.TrimSpace(failureCode)
	if len(failureCode) > 128 {
		failureCode = failureCode[:128]
	}
	result, err := s.db.ExecContext(ctx, `UPDATE direct_campaigns
SET launch_state='reconciling',launch_failure_code=$1,
    launch_reconcile_after=$2,updated_at=$2
WHERE workspace_id=$3 AND id=$4 AND launch_state IN ('launching','reconciling')
  AND launch_claimed_at=$5`,
		failureCode, now.UTC(), workspaceID, campaignID, expectedLaunchClaimedAt)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrDirectLaunchAlreadyClaimed
	}
	return nil
}

func (s *Store) AbortDirectCampaignLaunchForAuthorization(
	ctx context.Context, workspaceID, campaignID string,
	expectedLaunchClaimedAt, now time.Time,
) error {
	now = now.UTC()
	expectedLaunchClaimedAt = expectedLaunchClaimedAt.UTC().Truncate(time.Microsecond)
	if expectedLaunchClaimedAt.IsZero() {
		return ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE direct_campaigns
SET launch_state='idle',launch_mode='',launch_claimed_at=NULL,
    launch_attempt_count=0,launch_reconcile_after=NULL,launch_failed_at=NULL,
    launched_at=NULL,launch_failure_code='authorization_required',updated_at=$1
WHERE workspace_id=$2 AND id=$3 AND status='accepted'
  AND launch_state IN ('launching','reconciling')
  AND launch_claimed_at=$4`,
		now, workspaceID, campaignID, expectedLaunchClaimedAt)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrConflict
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, Action: "direct.campaign.launch_authorization_failed",
		EntityType: "direct_campaign", EntityID: campaignID,
		Metadata:  mustJSON(map[string]any{"failure_code": "authorization_required"}),
		CreatedAt: now,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FailDirectCampaignLaunch(
	ctx context.Context, workspaceID, campaignID string,
	expectedLaunchClaimedAt time.Time, failureCode string, now time.Time,
) error {
	expectedLaunchClaimedAt = expectedLaunchClaimedAt.UTC().Truncate(time.Microsecond)
	if expectedLaunchClaimedAt.IsZero() {
		return ErrConflict
	}
	failureCode = strings.TrimSpace(failureCode)
	if failureCode == "" {
		failureCode = "provider_off_after_retries"
	}
	if len(failureCode) > 128 {
		failureCode = failureCode[:128]
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE direct_campaigns
SET launch_state='failed',launch_reconcile_after=NULL,
    launch_failed_at=$2,launch_failure_code=$1,updated_at=$2
WHERE workspace_id=$3 AND id=$4 AND status='accepted'
  AND launch_state IN ('launching','reconciling') AND launch_attempt_count=2
  AND launch_claimed_at=$5`,
		failureCode, now.UTC(), workspaceID, campaignID, expectedLaunchClaimedAt)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrConflict
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, Action: "direct.campaign.launch_failed",
		EntityType: "direct_campaign", EntityID: campaignID,
		Metadata: mustJSON(map[string]any{"failure_code": failureCode}), CreatedAt: now.UTC(),
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CompleteDirectCampaignLaunch(
	ctx context.Context, workspaceID, campaignID string,
	expectedLaunchClaimedAt, now time.Time,
) error {
	expectedLaunchClaimedAt = expectedLaunchClaimedAt.UTC().Truncate(time.Microsecond)
	if expectedLaunchClaimedAt.IsZero() {
		return ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE direct_campaigns
SET status='active',provider_status='ACCEPTED',provider_state='ON',
    launch_state='confirmed',launched_at=$1,
    launch_reconcile_after=NULL,launch_failed_at=NULL,launch_failure_code='',updated_at=$1
WHERE workspace_id=$2 AND id=$3 AND status IN ('accepted','active')
  AND launch_state IN ('launching','reconciling') AND launch_claimed_at=$4`,
		now.UTC(), workspaceID, campaignID, expectedLaunchClaimedAt)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrDirectLaunchAlreadyClaimed
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, Action: "direct.campaign.launched",
		EntityType: "direct_campaign", EntityID: campaignID,
		Metadata: json.RawMessage(`{}`), CreatedAt: now.UTC(),
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) getDirectAutoLaunchSummary(
	ctx context.Context, workspaceID string, campaign DirectCampaign,
) (DirectAutoLaunchSummary, error) {
	consent, err := scanDirectConsent(s.db.QueryRowContext(ctx, `SELECT `+directConsentColumns+`
FROM direct_auto_launch_consent_evidence WHERE workspace_id=$1 AND campaign_id=$2
ORDER BY (consent_version='yandex-direct-auto-launch-v2') DESC,
         authorized_at DESC,id DESC
LIMIT 1`, workspaceID, campaign.ID))
	if errors.Is(err, ErrNotFound) {
		return DirectAutoLaunchSummary{}, nil
	}
	if err != nil {
		return DirectAutoLaunchSummary{}, err
	}
	summary := DirectAutoLaunchSummary{
		Enabled:      consent.RevokedAt == nil && consent.ConsumedAt == nil,
		CampaignID:   consent.CampaignID,
		CampaignName: consent.CampaignName,
	}
	if consent.ConsentVersion != DirectAutoLaunchConsentVersion {
		summary.WarningCode = "legacy_consent_version"
	} else if campaign.GraphVerifiedAt == nil {
		summary.WarningCode = "provider_graph_unverified"
	}
	if consent.ProviderCampaignID != nil {
		summary.ProviderCampaignID = fmt.Sprintf("%d", *consent.ProviderCampaignID)
	}
	authorized := consent.AuthorizedAt
	summary.AuthorizedAt = &authorized
	if consent.InvalidatedAt != nil {
		summary.Enabled = false
		summary.InvalidReason = consent.InvalidReason
		return summary, nil
	}
	if consent.RevokedAt != nil {
		summary.Enabled = false
		summary.InvalidReason = "revoked"
		return summary, nil
	}
	if consent.ConsumedAt != nil {
		summary.Enabled = false
		summary.InvalidReason = "consumed"
		return summary, nil
	}
	connection, connectionErr := s.getDirectConnectionForWorker(ctx, workspaceID, campaign.ConnectionID)
	if errors.Is(connectionErr, ErrNotFound) {
		summary.InvalidReason = "connection_unavailable"
		return summary, nil
	}
	if connectionErr != nil {
		return DirectAutoLaunchSummary{}, fmt.Errorf(
			"load direct connection for auto-launch summary: %w", connectionErr,
		)
	}
	summary.Valid = directConsentMatches(consent, campaign, connection)
	if !summary.Valid {
		summary.InvalidReason = "campaign_changed"
	}
	return summary, nil
}

func validateDirectCampaignDraft(campaign *DirectCampaign) error {
	campaign.Name = strings.TrimSpace(campaign.Name)
	campaign.Objective = strings.TrimSpace(campaign.Objective)
	campaign.LandingURL = strings.TrimSpace(campaign.LandingURL)
	campaign.Brief = strings.TrimSpace(campaign.Brief)
	campaign.CurrencyCode = strings.ToUpper(strings.TrimSpace(campaign.CurrencyCode))
	campaign.StartsAt = dateOnly(campaign.StartsAt)
	campaign.EndsAt = dateOnly(campaign.EndsAt)
	if campaign.Name == "" || utf8.RuneCountInString(campaign.Name) > 255 {
		return errors.New("direct campaign name must contain 1 to 255 characters")
	}
	if !directIdentifierPattern.MatchString(campaign.Objective) {
		return errors.New("direct campaign objective is invalid")
	}
	landingURL, err := url.Parse(campaign.LandingURL)
	if err != nil || landingURL.Scheme != "https" || landingURL.Host == "" ||
		landingURL.User != nil || landingURL.Fragment != "" || landingURL.RawFragment != "" ||
		len(campaign.LandingURL) > 2048 {
		return errors.New("landing_url must be a safe absolute HTTPS URL")
	}
	if campaign.Brief == "" || utf8.RuneCountInString(campaign.Brief) > 4000 {
		return errors.New("direct campaign brief must contain 1 to 4000 characters")
	}
	if len(campaign.Regions) == 0 || len(campaign.Regions) > 100 {
		return errors.New("direct campaign must contain 1 to 100 regions")
	}
	seenRegions := make(map[string]struct{}, len(campaign.Regions))
	normalizedRegions := make([]string, 0, len(campaign.Regions))
	for _, region := range campaign.Regions {
		region = norm.NFC.String(strings.TrimSpace(region))
		if region == "" || utf8.RuneCountInString(region) > 120 {
			return errors.New("direct campaign region is invalid")
		}
		key := directGraphDuplicateKey(region)
		if _, duplicate := seenRegions[key]; duplicate {
			return errors.New("direct campaign regions must not contain duplicates")
		}
		seenRegions[key] = struct{}{}
		normalizedRegions = append(normalizedRegions, region)
	}
	if len(normalizedRegions) == 0 {
		return errors.New("direct campaign must contain at least one region")
	}
	campaign.Regions = normalizedRegions
	if err := validateDirectCampaignGraphDraft(campaign); err != nil {
		return err
	}
	if campaign.CurrencyCode != "" && campaign.CurrencyCode != "RUB" {
		return errors.New("currency_code must be RUB")
	}
	if campaign.WeeklyBudgetMinor < 30_000 {
		return errors.New("weekly_budget_minor must be at least 30000")
	}
	if campaign.WeeklyBudgetMinor > DirectMaxCampaignWeeklyBudgetMinor {
		return ErrDirectBudgetCapExceeded
	}
	if campaign.StartsAt.IsZero() || campaign.EndsAt.IsZero() || campaign.EndsAt.Before(campaign.StartsAt) {
		return errors.New("direct campaign dates are invalid")
	}
	return nil
}

func normalizeDirectProviderStatus(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "DRAFT":
		return "DRAFT"
	case "MODERATION":
		return "MODERATION"
	case "ACCEPTED":
		return "ACCEPTED"
	case "REJECTED":
		return "REJECTED"
	default:
		return ""
	}
}

func directCampaignStatusFromProvider(providerStatus string) string {
	switch normalizeDirectProviderStatus(providerStatus) {
	case "ACCEPTED":
		return "accepted"
	case "REJECTED":
		return "rejected"
	case "MODERATION":
		return "moderation"
	default:
		// Campaigns.add creates only the provider-side campaign container. It
		// does not create ads or send anything to moderation.
		return "provider_draft"
	}
}

func directCampaignStatusFromProviderLifecycle(providerStatus, providerState string) string {
	status := directCampaignStatusFromProvider(providerStatus)
	if normalizeDirectProviderStatus(providerStatus) != "ACCEPTED" {
		return status
	}
	switch strings.ToUpper(strings.TrimSpace(providerState)) {
	case "ON":
		return "active"
	case "SUSPENDED", "ARCHIVED", "CONVERTED":
		return "suspended"
	case "ENDED":
		return "completed"
	default:
		return status
	}
}

func directConfirmedCampaignStatus(providerStatus, providerState, currentStatus string) string {
	switch normalizeDirectProviderStatus(providerStatus) {
	case "REJECTED":
		return "rejected"
	case "ACCEPTED":
		switch strings.ToUpper(strings.TrimSpace(providerState)) {
		case "ON":
			return "active"
		case "ENDED":
			return "completed"
		case "OFF", "SUSPENDED", "ARCHIVED", "CONVERTED":
			return "suspended"
		default:
			if currentStatus == "active" || currentStatus == "suspended" || currentStatus == "completed" {
				return currentStatus
			}
			return "suspended"
		}
	default:
		return "suspended"
	}
}

func directConsentMatches(
	consent DirectAutoLaunchConsent, campaign DirectCampaign, connection DirectConnection,
) bool {
	providerCampaignMatches := consent.ProviderCampaignID != nil &&
		campaign.ProviderCampaignID != nil &&
		*consent.ProviderCampaignID == *campaign.ProviderCampaignID
	return consent.ConsentVersion == DirectAutoLaunchConsentVersion &&
		directCampaignGraphLaunchReady(campaign) == nil &&
		consent.ExpectedGraphHash == campaign.ProviderGraphHash &&
		consent.ExpectedRevisionID == campaign.ProviderRevisionID &&
		campaign.ModerationStatus == "ACCEPTED" &&
		connection.Status == "active" && connection.RevokedAt == nil && !connection.ReadOnly &&
		consent.CampaignVersion == campaign.Version &&
		consent.ConnectionID == campaign.ConnectionID &&
		consent.AccountID == connection.AccountID &&
		consent.CampaignName == campaign.Name &&
		providerCampaignMatches &&
		consent.WeeklyBudgetMinor == campaign.WeeklyBudgetMinor &&
		consent.CurrencyCode == campaign.CurrencyCode &&
		sameDate(consent.StartsAt, campaign.StartsAt) &&
		sameDate(consent.EndsAt, campaign.EndsAt)
}

func directCampaignAuditMetadata(campaign DirectCampaign) json.RawMessage {
	return mustJSON(map[string]any{
		"weekly_budget_minor": campaign.WeeklyBudgetMinor,
		"currency_code":       campaign.CurrencyCode,
		"starts_at":           dateOnly(campaign.StartsAt).Format(time.DateOnly),
		"ends_at":             dateOnly(campaign.EndsAt).Format(time.DateOnly),
	})
}

func dateOnly(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	value = value.UTC()
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

func sameDate(left, right time.Time) bool {
	return dateOnly(left).Equal(dateOnly(right))
}

func scanDirectConnection(row scanner) (DirectConnection, error) {
	var connection DirectConnection
	err := row.Scan(&connection.ID, &connection.WorkspaceID, &connection.AccountID,
		&connection.ClientLogin, &connection.AccountName, &connection.CurrencyCode,
		&connection.Timezone, &connection.ReadOnly, &connection.TokenCiphertext, &connection.TokenKeyVersion, &connection.Status,
		&connection.ConnectedBy, &connection.LastVerifiedAt, &connection.ErrorCode,
		&connection.CreatedAt, &connection.UpdatedAt, &connection.RevokedAt,
		&connection.TokenRefreshClaimedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DirectConnection{}, ErrNotFound
	}
	if err != nil {
		return DirectConnection{}, err
	}
	normalizeDirectConnection(&connection)
	return connection, nil
}

func scanDirectCampaign(row scanner) (DirectCampaign, error) {
	var campaign DirectCampaign
	var providerCampaignID, providerAdGroupID, providerAdID sql.NullInt64
	var regionsJSON, titlesJSON, textsJSON, keywordsJSON, negativeKeywordsJSON []byte
	var providerKeywordIDsJSON, providerKeywordMappingsJSON, providerWarningsJSON []byte
	var campaignModerationJSON, adGroupModerationJSON, adModerationJSON, keywordModerationJSON []byte
	err := row.Scan(&campaign.ID, &campaign.WorkspaceID, &campaign.ConnectionID,
		&providerCampaignID, &campaign.Name, &campaign.Objective, &campaign.LandingURL,
		&campaign.Brief, &regionsJSON, &campaign.WeeklyBudgetMinor,
		&campaign.CurrencyCode, &campaign.StartsAt, &campaign.EndsAt,
		&campaign.Status, &campaign.ProviderStatus, &campaign.ProviderState,
		&campaign.ProviderNextCheckAt, &campaign.AutoLaunchNextAttemptAt,
		&campaign.Version, &campaign.CreatedBy,
		&campaign.SubmittedAt, &campaign.LaunchClaimedAt,
		&campaign.LaunchState, &campaign.LaunchMode, &campaign.LaunchAttemptCount,
		&campaign.LaunchReconcileAfter, &campaign.LaunchedAt, &campaign.LaunchFailedAt,
		&campaign.LaunchFailureCode, &campaign.CreatedAt,
		&campaign.UpdatedAt, &titlesJSON, &textsJSON, &keywordsJSON,
		&negativeKeywordsJSON, &providerAdGroupID, &providerAdID,
		&providerKeywordIDsJSON, &providerKeywordMappingsJSON, &providerWarningsJSON,
		&campaign.SubmissionOperationID, &campaign.SubmissionStage,
		&campaign.SubmissionOperationMarker, &campaign.SubmissionClaimedAt,
		&campaign.SubmissionLeaseExpiresAt, &campaign.SubmissionFailureCode,
		&campaign.SubmissionFailureClarification, &campaign.ProviderGraphHash,
		&campaign.ProviderRevisionID, &campaign.GraphVerifiedAt,
		&campaign.ModerationStatus, &campaign.ModerationClarification,
		&campaignModerationJSON, &adGroupModerationJSON, &adModerationJSON,
		&keywordModerationJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return DirectCampaign{}, ErrNotFound
	}
	if err != nil {
		return DirectCampaign{}, err
	}
	if providerCampaignID.Valid {
		value := providerCampaignID.Int64
		campaign.ProviderCampaignID = &value
	}
	if providerAdGroupID.Valid {
		value := providerAdGroupID.Int64
		campaign.ProviderAdGroupID = &value
	}
	if providerAdID.Valid {
		value := providerAdID.Int64
		campaign.ProviderAdID = &value
	}
	if err := json.Unmarshal(regionsJSON, &campaign.Regions); err != nil {
		return DirectCampaign{}, fmt.Errorf("decode Direct campaign regions: %w", err)
	}
	for _, item := range []struct {
		name    string
		payload []byte
		target  any
	}{
		{"titles", titlesJSON, &campaign.Titles},
		{"texts", textsJSON, &campaign.Texts},
		{"keywords", keywordsJSON, &campaign.Keywords},
		{"negative_keywords", negativeKeywordsJSON, &campaign.NegativeKeywords},
		{"provider_keyword_ids", providerKeywordIDsJSON, &campaign.ProviderKeywordIDs},
		{"provider_keyword_mappings", providerKeywordMappingsJSON, &campaign.ProviderKeywordMappings},
		{"provider_warnings", providerWarningsJSON, &campaign.ProviderWarnings},
		{"campaign_moderation", campaignModerationJSON, &campaign.CampaignModeration},
		{"ad_group_moderation", adGroupModerationJSON, &campaign.AdGroupModeration},
		{"ad_moderation", adModerationJSON, &campaign.AdModeration},
		{"keyword_moderation", keywordModerationJSON, &campaign.KeywordModeration},
	} {
		if err := json.Unmarshal(item.payload, item.target); err != nil {
			return DirectCampaign{}, fmt.Errorf(
				"decode Direct campaign %s: %w", item.name, err,
			)
		}
	}
	normalizeDirectCampaign(&campaign)
	return campaign, nil
}

func scanDirectConsent(row scanner) (DirectAutoLaunchConsent, error) {
	var consent DirectAutoLaunchConsent
	var providerCampaignID sql.NullInt64
	err := row.Scan(&consent.ID, &consent.WorkspaceID, &consent.CampaignID,
		&consent.ConnectionID, &consent.ActorUserID, &consent.ConsentVersion,
		&consent.Confirmation, &consent.CampaignVersion, &consent.AccountID,
		&providerCampaignID, &consent.CampaignName,
		&consent.WeeklyBudgetMinor, &consent.CurrencyCode,
		&consent.StartsAt, &consent.EndsAt, &consent.ExpectedGraphHash,
		&consent.ExpectedRevisionID, &consent.AuthorizedAt,
		&consent.RevokedAt, &consent.InvalidatedAt, &consent.InvalidReason,
		&consent.ConsumedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DirectAutoLaunchConsent{}, ErrNotFound
	}
	if err != nil {
		return DirectAutoLaunchConsent{}, err
	}
	if providerCampaignID.Valid {
		value := providerCampaignID.Int64
		consent.ProviderCampaignID = &value
	}
	normalizeDirectConsent(&consent)
	return consent, nil
}

func normalizeDirectOAuthState(state *DirectOAuthState) {
	state.CreatedAt = state.CreatedAt.UTC()
	state.ExpiresAt = state.ExpiresAt.UTC()
	if state.ConsumedAt != nil {
		value := state.ConsumedAt.UTC()
		state.ConsumedAt = &value
	}
}

func normalizeDirectConnection(connection *DirectConnection) {
	connection.CreatedAt = connection.CreatedAt.UTC()
	connection.UpdatedAt = connection.UpdatedAt.UTC()
	if connection.LastVerifiedAt != nil {
		value := connection.LastVerifiedAt.UTC()
		connection.LastVerifiedAt = &value
	}
	if connection.RevokedAt != nil {
		value := connection.RevokedAt.UTC()
		connection.RevokedAt = &value
	}
	if connection.TokenRefreshClaimedAt != nil {
		value := connection.TokenRefreshClaimedAt.UTC()
		connection.TokenRefreshClaimedAt = &value
	}
}

func normalizeDirectCampaign(campaign *DirectCampaign) {
	campaign.StartsAt = dateOnly(campaign.StartsAt)
	campaign.EndsAt = dateOnly(campaign.EndsAt)
	campaign.CreatedAt = campaign.CreatedAt.UTC()
	campaign.UpdatedAt = campaign.UpdatedAt.UTC()
	campaign.ProviderNextCheckAt = campaign.ProviderNextCheckAt.UTC()
	campaign.AutoLaunchNextAttemptAt = campaign.AutoLaunchNextAttemptAt.UTC()
	if campaign.SubmittedAt != nil {
		value := campaign.SubmittedAt.UTC()
		campaign.SubmittedAt = &value
	}
	if campaign.LaunchClaimedAt != nil {
		value := campaign.LaunchClaimedAt.UTC()
		campaign.LaunchClaimedAt = &value
	}
	if campaign.LaunchReconcileAfter != nil {
		value := campaign.LaunchReconcileAfter.UTC()
		campaign.LaunchReconcileAfter = &value
	}
	if campaign.LaunchFailedAt != nil {
		value := campaign.LaunchFailedAt.UTC()
		campaign.LaunchFailedAt = &value
	}
	if campaign.LaunchedAt != nil {
		value := campaign.LaunchedAt.UTC()
		campaign.LaunchedAt = &value
	}
	if campaign.SubmissionClaimedAt != nil {
		value := campaign.SubmissionClaimedAt.UTC()
		campaign.SubmissionClaimedAt = &value
	}
	if campaign.SubmissionLeaseExpiresAt != nil {
		value := campaign.SubmissionLeaseExpiresAt.UTC()
		campaign.SubmissionLeaseExpiresAt = &value
	}
	if campaign.GraphVerifiedAt != nil {
		value := campaign.GraphVerifiedAt.UTC()
		campaign.GraphVerifiedAt = &value
	}
}

func normalizeDirectConsent(consent *DirectAutoLaunchConsent) {
	consent.CampaignName = strings.TrimSpace(consent.CampaignName)
	consent.StartsAt = dateOnly(consent.StartsAt)
	consent.EndsAt = dateOnly(consent.EndsAt)
	consent.AuthorizedAt = consent.AuthorizedAt.UTC()
	for source, target := range map[*time.Time]**time.Time{
		consent.RevokedAt:     &consent.RevokedAt,
		consent.InvalidatedAt: &consent.InvalidatedAt,
		consent.ConsumedAt:    &consent.ConsumedAt,
	} {
		if source != nil {
			value := source.UTC()
			*target = &value
		}
	}
}
