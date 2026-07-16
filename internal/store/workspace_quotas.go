package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AcquireWorkspaceAILease is the workspace-scoped equivalent of
// AcquireAILease. Team requests are charged only to the workspace ledger and
// never consume the lifecycle owner's personal allowance.
func (s *Store) AcquireWorkspaceAILease(ctx context.Context, workspaceID, operation string, limits AILimits, now time.Time) (AILease, error) {
	return s.acquireWorkspaceAILease(ctx, workspaceID, operation, "", 1, false, limits, now)
}

// AcquireWorkspaceAILeaseWithMonthlyUsage charges the rate-limit buckets, the
// quantity-based monthly plan ledger and the in-flight lease atomically.
func (s *Store) AcquireWorkspaceAILeaseWithMonthlyUsage(
	ctx context.Context, workspaceID, operation, monthlyMetric string,
	amount int64, enforceMonthly bool, limits AILimits, now time.Time,
) (AILease, error) {
	return s.acquireWorkspaceAILease(ctx, workspaceID, operation, monthlyMetric, amount, enforceMonthly, limits, now)
}

func (s *Store) acquireWorkspaceAILease(
	ctx context.Context, workspaceID, operation, monthlyMetric string,
	amount int64, enforceMonthly bool, limits AILimits, now time.Time,
) (AILease, error) {
	if workspaceID == "" {
		return AILease{}, errors.New("AI limit workspace ID is required")
	}
	if !aiOperationPattern.MatchString(operation) {
		return AILease{}, errors.New("AI operation must contain only lowercase letters, digits and underscores")
	}
	if err := limits.Validate(); err != nil {
		return AILease{}, err
	}
	if now.IsZero() {
		return AILease{}, errors.New("AI lease time is required")
	}
	if amount <= 0 {
		return AILease{}, errors.New("AI monthly usage amount must be positive")
	}
	leaseID, err := randomAILeaseID()
	if err != nil {
		return AILease{}, err
	}
	now = now.UTC()
	minuteStart := now.Truncate(time.Minute)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	expiresAt := now.Add(limits.LeaseTTL)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return AILease{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,$2))`,
		"maxstudio:workspace-ai:"+workspaceID+":"+operation, aiAdvisoryLockSeed); err != nil {
		return AILease{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_ai_request_leases
WHERE workspace_id=$1 AND operation=$2 AND expires_at<=$3`, workspaceID, operation, now); err != nil {
		return AILease{}, err
	}
	var active int
	var earliest sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT count(*),min(expires_at) FROM workspace_ai_request_leases
WHERE workspace_id=$1 AND operation=$2 AND expires_at>$3`, workspaceID, operation, now).Scan(&active, &earliest); err != nil {
		return AILease{}, err
	}
	if active >= limits.MaxConcurrent {
		retry := time.Second
		if earliest.Valid {
			retry = earliest.Time.Sub(now)
		}
		return AILease{}, &AILimitError{Reason: AILimitReasonConcurrency, RetryAfter: positiveRetryAfter(retry)}
	}
	dayUsed, err := readWorkspaceAIUsageBucket(ctx, tx, workspaceID, operation, "day", dayStart)
	if err != nil {
		return AILease{}, err
	}
	if dayUsed >= limits.PerDay {
		return AILease{}, &AILimitError{Reason: AILimitReasonDay, RetryAfter: positiveRetryAfter(dayStart.AddDate(0, 0, 1).Sub(now))}
	}
	minuteUsed, err := readWorkspaceAIUsageBucket(ctx, tx, workspaceID, operation, "minute", minuteStart)
	if err != nil {
		return AILease{}, err
	}
	if minuteUsed >= limits.PerMinute {
		return AILease{}, &AILimitError{Reason: AILimitReasonMinute, RetryAfter: positiveRetryAfter(minuteStart.Add(time.Minute).Sub(now))}
	}
	if monthlyMetric == "" {
		monthlyMetric, err = monthlyUsageMetricForAIOperation(operation)
		if err != nil {
			return AILease{}, err
		}
	}
	if _, err := chargeWorkspaceMonthlyUsageTx(
		ctx, tx, workspaceID, monthlyMetric, amount, enforceMonthly, now,
	); err != nil {
		var usageLimit *WorkspaceUsageLimitError
		if errors.As(err, &usageLimit) {
			return AILease{}, &AILimitError{Reason: AILimitReasonMonthly, RetryAfter: usageLimit.RetryAfter}
		}
		return AILease{}, fmt.Errorf("charge workspace monthly AI usage: %w", err)
	}
	if err := writeWorkspaceAIUsageBucket(ctx, tx, workspaceID, operation, "day", dayStart, dayUsed+1, now); err != nil {
		return AILease{}, err
	}
	if err := writeWorkspaceAIUsageBucket(ctx, tx, workspaceID, operation, "minute", minuteStart, minuteUsed+1, now); err != nil {
		return AILease{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_ai_request_leases(
id,workspace_id,operation,acquired_at,expires_at) VALUES($1,$2,$3,$4,$5)`,
		leaseID, workspaceID, operation, now, expiresAt); err != nil {
		return AILease{}, err
	}
	if err := tx.Commit(); err != nil {
		return AILease{}, err
	}
	return AILease{ID: leaseID, WorkspaceID: workspaceID, Operation: operation, ExpiresAt: expiresAt}, nil
}

func (s *Store) ReleaseWorkspaceAILease(ctx context.Context, workspaceID, leaseID string) error {
	if workspaceID == "" || !aiLeaseIDPattern.MatchString(leaseID) {
		return errors.New("workspace or AI lease ID is invalid")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM workspace_ai_request_leases WHERE id=$1 AND workspace_id=$2`,
		leaseID, workspaceID); err != nil {
		return fmt.Errorf("release workspace AI lease: %w", err)
	}
	return nil
}

func readWorkspaceAIUsageBucket(ctx context.Context, tx *sql.Tx, workspaceID, operation, kind string, window time.Time) (int, error) {
	var stored time.Time
	var used int
	err := tx.QueryRowContext(ctx, `SELECT window_start,used FROM workspace_ai_usage_buckets
WHERE workspace_id=$1 AND operation=$2 AND bucket_kind=$3`, workspaceID, operation, kind).Scan(&stored, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !stored.Equal(window) {
		return 0, nil
	}
	return used, nil
}

func writeWorkspaceAIUsageBucket(ctx context.Context, tx *sql.Tx, workspaceID, operation, kind string, window time.Time, used int, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO workspace_ai_usage_buckets(
workspace_id,operation,bucket_kind,window_start,used,updated_at)
VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT(workspace_id,operation,bucket_kind) DO UPDATE SET
window_start=excluded.window_start,used=excluded.used,updated_at=excluded.updated_at`,
		workspaceID, operation, kind, window, used, now)
	return err
}

func (s *Store) GetWorkspaceMediaUsage(ctx context.Context, workspaceID string) (WorkspaceMediaUsage, error) {
	var usage WorkspaceMediaUsage
	err := s.db.QueryRowContext(ctx, `SELECT workspace_id,asset_count,total_bytes,updated_at
FROM workspace_media_usage WHERE workspace_id=$1`, workspaceID).Scan(
		&usage.WorkspaceID, &usage.AssetCount, &usage.TotalBytes, &usage.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		if _, workspaceErr := s.GetWorkspace(ctx, workspaceID); workspaceErr != nil {
			return WorkspaceMediaUsage{}, workspaceErr
		}
		return WorkspaceMediaUsage{WorkspaceID: workspaceID}, nil
	}
	if err != nil {
		return WorkspaceMediaUsage{}, err
	}
	usage.UpdatedAt = usage.UpdatedAt.UTC()
	return usage, nil
}
