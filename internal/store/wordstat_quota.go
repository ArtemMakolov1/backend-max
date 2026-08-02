package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"
)

const (
	// A single logical GetTop call can make at most three provider attempts.
	// Reserving one third of Yandex's 10 requests/second and 100 requests/hour
	// limits keeps the physical traffic at no more than 9/second and 99/hour,
	// including retries performed inside the Wordstat client.
	WordstatProviderLogicalPerSecond = 3
	WordstatProviderLogicalPerHour   = 33

	WordstatWorkspaceLogicalPerSecond = 2
	WordstatWorkspaceLogicalPerHour   = 20
	WordstatUserWorkspacePerSecond    = 1
	WordstatUserWorkspacePerHour      = 10

	wordstatQuotaAdvisoryLockSeed = 0
)

const (
	WordstatQuotaReasonProviderSecond      = "provider_second"
	WordstatQuotaReasonProviderHour        = "provider_hour"
	WordstatQuotaReasonWorkspaceSecond     = "workspace_second"
	WordstatQuotaReasonWorkspaceHour       = "workspace_hour"
	WordstatQuotaReasonUserWorkspaceSecond = "user_workspace_second"
	WordstatQuotaReasonUserWorkspaceHour   = "user_workspace_hour"
)

var wordstatProviderKeyHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// WordstatQuotaError is safe to expose as HTTP 429. RetryAfter is the first
// instant at which the rejected rolling-window constraint can clear.
type WordstatQuotaError struct {
	Reason     string
	RetryAfter time.Duration
}

func (e *WordstatQuotaError) Error() string {
	if e == nil {
		return "Wordstat quota exceeded"
	}
	return fmt.Sprintf("Wordstat %s quota exceeded; retry after %s", e.Reason, positiveRetryAfter(e.RetryAfter))
}

type wordstatQuotaScope string

const (
	wordstatQuotaProvider      wordstatQuotaScope = "provider"
	wordstatQuotaWorkspace     wordstatQuotaScope = "workspace"
	wordstatQuotaUserWorkspace wordstatQuotaScope = "user_workspace"
)

type wordstatRollingLimit struct {
	scope  wordstatQuotaScope
	window time.Duration
	limit  int
	reason string
}

// AcquireWordstatQuota atomically reserves one logical Wordstat request. A
// transaction-scoped provider-key advisory lock serializes all replicas that
// share that key; workspace and user/workspace locks preserve tenant fairness
// during a rolling key deployment as well. The event ledger implements true
// rolling windows, so a burst across a wall-clock boundary cannot double the
// permitted traffic.
func (s *Store) AcquireWordstatQuota(
	ctx context.Context, providerKeyHash, actorUserID, workspaceID string, now time.Time,
) error {
	if s == nil || s.db == nil {
		return errors.New("store is required")
	}
	if !wordstatProviderKeyHashPattern.MatchString(providerKeyHash) {
		return errors.New("wordstat provider key hash must be a lowercase SHA-256 digest")
	}
	if actorUserID == "" {
		return errors.New("wordstat actor user ID is required")
	}
	if workspaceID == "" {
		return errors.New("wordstat workspace ID is required")
	}
	if now.IsZero() {
		return errors.New("wordstat quota time is required")
	}
	now = now.UTC()

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin Wordstat quota transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	lockKeys := []string{
		"maxstudio:wordstat:provider:" + providerKeyHash,
		"maxstudio:wordstat:workspace:" + workspaceID,
		"maxstudio:wordstat:user-workspace:" + workspaceID + ":" + actorUserID,
	}
	for _, lockKey := range lockKeys {
		if _, err := tx.ExecContext(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended($1,$2))`,
			lockKey, wordstatQuotaAdvisoryLockSeed,
		); err != nil {
			return fmt.Errorf("lock Wordstat quota: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM wordstat_quota_events WHERE occurred_at <= $1`, now.Add(-time.Hour),
	); err != nil {
		return fmt.Errorf("expire Wordstat quota events: %w", err)
	}

	limits := []wordstatRollingLimit{
		{wordstatQuotaUserWorkspace, time.Second, WordstatUserWorkspacePerSecond, WordstatQuotaReasonUserWorkspaceSecond},
		{wordstatQuotaUserWorkspace, time.Hour, WordstatUserWorkspacePerHour, WordstatQuotaReasonUserWorkspaceHour},
		{wordstatQuotaWorkspace, time.Second, WordstatWorkspaceLogicalPerSecond, WordstatQuotaReasonWorkspaceSecond},
		{wordstatQuotaWorkspace, time.Hour, WordstatWorkspaceLogicalPerHour, WordstatQuotaReasonWorkspaceHour},
		{wordstatQuotaProvider, time.Second, WordstatProviderLogicalPerSecond, WordstatQuotaReasonProviderSecond},
		{wordstatQuotaProvider, time.Hour, WordstatProviderLogicalPerHour, WordstatQuotaReasonProviderHour},
	}
	for _, limit := range limits {
		used, oldest, err := readWordstatRollingUsage(
			ctx, tx, limit.scope, providerKeyHash, actorUserID, workspaceID, now.Add(-limit.window),
		)
		if err != nil {
			return err
		}
		if used >= limit.limit {
			retryAfter := time.Second
			if oldest.Valid {
				retryAfter = oldest.Time.Add(limit.window).Sub(now)
			}
			return &WordstatQuotaError{
				Reason: limit.reason, RetryAfter: positiveRetryAfter(retryAfter),
			}
		}
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO wordstat_quota_events(provider_key_hash,workspace_id,actor_user_id,occurred_at)
VALUES($1,$2,$3,$4)`, providerKeyHash, workspaceID, actorUserID, now); err != nil {
		return fmt.Errorf("record Wordstat quota event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Wordstat quota event: %w", err)
	}
	return nil
}

func readWordstatRollingUsage(
	ctx context.Context, tx *sql.Tx, scope wordstatQuotaScope,
	providerKeyHash, actorUserID, workspaceID string, cutoff time.Time,
) (int, sql.NullTime, error) {
	var query string
	var args []any
	switch scope {
	case wordstatQuotaProvider:
		query = `SELECT count(*),min(occurred_at) FROM wordstat_quota_events
WHERE provider_key_hash=$1 AND occurred_at>$2`
		args = []any{providerKeyHash, cutoff}
	case wordstatQuotaWorkspace:
		query = `SELECT count(*),min(occurred_at) FROM wordstat_quota_events
WHERE workspace_id=$1 AND occurred_at>$2`
		args = []any{workspaceID, cutoff}
	case wordstatQuotaUserWorkspace:
		query = `SELECT count(*),min(occurred_at) FROM wordstat_quota_events
WHERE actor_user_id=$1 AND workspace_id=$2 AND occurred_at>$3`
		args = []any{actorUserID, workspaceID, cutoff}
	default:
		return 0, sql.NullTime{}, errors.New("invalid Wordstat quota scope")
	}
	var used int
	var oldest sql.NullTime
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&used, &oldest); err != nil {
		return 0, sql.NullTime{}, fmt.Errorf("read Wordstat %s quota: %w", scope, err)
	}
	return used, oldest, nil
}
