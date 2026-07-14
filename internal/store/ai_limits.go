package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

const (
	AIOperationImage    = "image"
	AIOperationResearch = "research"

	AILimitReasonMinute      = "minute"
	AILimitReasonDay         = "day"
	AILimitReasonConcurrency = "concurrency"
	AILimitReasonGlobal      = "global_concurrency"

	MaxAIPerMinute     = 10_000
	MaxAIPerDay        = 1_000_000
	MaxAIConcurrent    = 100
	MaxAILeaseTTL      = 24 * time.Hour
	MinAILeaseTTL      = time.Second
	leaseRandomBytes   = 16
	aiAdvisoryLockSeed = 0
)

var (
	aiOperationPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	aiLeaseIDPattern   = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// AILimits controls one user's access to one AI operation. Minute and day
// usage is charged when a lease is acquired, before an upstream request is
// made. This deliberately counts uncertain upstream failures because they can
// still incur provider cost.
type AILimits struct {
	PerMinute     int
	PerDay        int
	MaxConcurrent int
	LeaseTTL      time.Duration
}

func (l AILimits) Validate() error {
	switch {
	case l.PerMinute <= 0 || l.PerMinute > MaxAIPerMinute:
		return fmt.Errorf("AI per-minute limit must be between 1 and %d", MaxAIPerMinute)
	case l.PerDay <= 0 || l.PerDay > MaxAIPerDay:
		return fmt.Errorf("AI daily limit must be between 1 and %d", MaxAIPerDay)
	case l.MaxConcurrent <= 0 || l.MaxConcurrent > MaxAIConcurrent:
		return fmt.Errorf("AI per-user concurrency limit must be between 1 and %d", MaxAIConcurrent)
	case l.LeaseTTL < MinAILeaseTTL || l.LeaseTTL > MaxAILeaseTTL:
		return fmt.Errorf("AI lease TTL must be between %s and %s", MinAILeaseTTL, MaxAILeaseTTL)
	default:
		return nil
	}
}

type AILease struct {
	ID        string
	UserID    string
	Operation string
	ExpiresAt time.Time
}

// AILimitError is safe to expose as an HTTP 429. RetryAfter is the earliest
// time at which the rejected constraint may clear.
type AILimitError struct {
	Reason     string
	RetryAfter time.Duration
}

func (e *AILimitError) Error() string {
	if e == nil {
		return "AI usage limit exceeded"
	}
	return fmt.Sprintf("AI %s limit exceeded; retry after %s", e.Reason, positiveRetryAfter(e.RetryAfter))
}

// AcquireAILease atomically charges the current minute and UTC-day buckets
// and creates an expiring in-flight lease. The transaction-scoped advisory
// lock serializes one user+operation across server replicas and is compatible
// with PgBouncer transaction pooling.
func (s *Store) AcquireAILease(ctx context.Context, userID, operation string, limits AILimits, now time.Time) (AILease, error) {
	if s == nil || s.db == nil {
		return AILease{}, errors.New("store is required")
	}
	if userID == "" {
		return AILease{}, errors.New("AI limit user ID is required")
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
		return AILease{}, fmt.Errorf("begin AI limit transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	lockKey := "maxstudio:ai:" + userID + ":" + operation
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, $2))`, lockKey, aiAdvisoryLockSeed); err != nil {
		return AILease{}, fmt.Errorf("lock AI usage: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM ai_request_leases WHERE owner_id = $1 AND operation = $2 AND expires_at <= $3`,
		userID, operation, now); err != nil {
		return AILease{}, fmt.Errorf("expire AI leases: %w", err)
	}

	var active int
	var earliestExpiry sql.NullTime
	if err := tx.QueryRowContext(ctx, `
SELECT count(*), min(expires_at)
FROM ai_request_leases
WHERE owner_id = $1 AND operation = $2 AND expires_at > $3`, userID, operation, now).Scan(&active, &earliestExpiry); err != nil {
		return AILease{}, fmt.Errorf("count active AI leases: %w", err)
	}
	if active >= limits.MaxConcurrent {
		retryAfter := time.Second
		if earliestExpiry.Valid {
			retryAfter = earliestExpiry.Time.Sub(now)
		}
		return AILease{}, &AILimitError{Reason: AILimitReasonConcurrency, RetryAfter: positiveRetryAfter(retryAfter)}
	}

	dayUsed, err := readAIUsageBucket(ctx, tx, userID, operation, "day", dayStart)
	if err != nil {
		return AILease{}, err
	}
	if dayUsed >= limits.PerDay {
		nextDay := dayStart.AddDate(0, 0, 1)
		return AILease{}, &AILimitError{Reason: AILimitReasonDay, RetryAfter: positiveRetryAfter(nextDay.Sub(now))}
	}
	minuteUsed, err := readAIUsageBucket(ctx, tx, userID, operation, "minute", minuteStart)
	if err != nil {
		return AILease{}, err
	}
	if minuteUsed >= limits.PerMinute {
		return AILease{}, &AILimitError{Reason: AILimitReasonMinute, RetryAfter: positiveRetryAfter(minuteStart.Add(time.Minute).Sub(now))}
	}

	if err := writeAIUsageBucket(ctx, tx, userID, operation, "day", dayStart, dayUsed+1, now); err != nil {
		return AILease{}, err
	}
	if err := writeAIUsageBucket(ctx, tx, userID, operation, "minute", minuteStart, minuteUsed+1, now); err != nil {
		return AILease{}, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO ai_request_leases(id, owner_id, operation, acquired_at, expires_at)
VALUES ($1, $2, $3, $4, $5)`, leaseID, userID, operation, now, expiresAt); err != nil {
		return AILease{}, fmt.Errorf("create AI request lease: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return AILease{}, fmt.Errorf("commit AI request lease: %w", err)
	}
	return AILease{ID: leaseID, UserID: userID, Operation: operation, ExpiresAt: expiresAt}, nil
}

// ReleaseAILease is tenant-scoped and idempotent. Expiry remains a crash-safe
// fallback if the handler cannot release the lease during shutdown.
func (s *Store) ReleaseAILease(ctx context.Context, userID, leaseID string) error {
	if s == nil || s.db == nil {
		return errors.New("store is required")
	}
	if userID == "" {
		return errors.New("AI limit user ID is required")
	}
	if !aiLeaseIDPattern.MatchString(leaseID) {
		return errors.New("AI lease ID is invalid")
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM ai_request_leases WHERE id = ? AND owner_id = ?`, leaseID, userID); err != nil {
		return fmt.Errorf("release AI request lease: %w", err)
	}
	return nil
}

func readAIUsageBucket(ctx context.Context, tx *sql.Tx, userID, operation, bucketKind string, windowStart time.Time) (int, error) {
	var storedStart time.Time
	var used int
	err := tx.QueryRowContext(ctx, `
SELECT window_start, used
FROM ai_usage_buckets
WHERE owner_id = $1 AND operation = $2 AND bucket_kind = $3`, userID, operation, bucketKind).Scan(&storedStart, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read AI %s bucket: %w", bucketKind, err)
	}
	if !storedStart.Equal(windowStart) {
		return 0, nil
	}
	return used, nil
}

func writeAIUsageBucket(ctx context.Context, tx *sql.Tx, userID, operation, bucketKind string, windowStart time.Time, used int, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO ai_usage_buckets(owner_id, operation, bucket_kind, window_start, used, updated_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (owner_id, operation, bucket_kind) DO UPDATE SET
    window_start = EXCLUDED.window_start,
    used = EXCLUDED.used,
    updated_at = EXCLUDED.updated_at`, userID, operation, bucketKind, windowStart, used, now)
	if err != nil {
		return fmt.Errorf("write AI %s bucket: %w", bucketKind, err)
	}
	return nil
}

func randomAILeaseID() (string, error) {
	random := make([]byte, leaseRandomBytes)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate AI lease ID: %w", err)
	}
	return hex.EncodeToString(random), nil
}

func positiveRetryAfter(value time.Duration) time.Duration {
	if value < time.Second {
		return time.Second
	}
	return value
}
