package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAcquireAILeaseSerializesAcrossPhysicalConnections(t *testing.T) {
	t.Parallel()
	first, second := newAILimitTestStores(t)
	assertDifferentPostgreSQLConnections(t, first.db.DB, second.db.DB)

	limits := AILimits{PerMinute: 10, PerDay: 100, MaxConcurrent: 1, LeaseTTL: 5 * time.Minute}
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	start := make(chan struct{})
	type result struct {
		lease AILease
		err   error
	}
	results := make(chan result, 2)
	var group sync.WaitGroup
	for _, storage := range []*Store{first, second} {
		group.Add(1)
		go func(storage *Store) {
			defer group.Done()
			<-start
			lease, err := storage.AcquireAILease(context.Background(), "user-a", AIOperationImage, limits, now)
			results <- result{lease: lease, err: err}
		}(storage)
	}
	close(start)
	group.Wait()
	close(results)

	var acquired []AILease
	var rejected int
	for result := range results {
		if result.err == nil {
			acquired = append(acquired, result.lease)
			continue
		}
		_ = assertAILimitReason(t, result.err, AILimitReasonConcurrency)
		rejected++
	}
	if len(acquired) != 1 || rejected != 1 {
		t.Fatalf("acquired=%d rejected=%d, want 1/1", len(acquired), rejected)
	}
	if err := first.ReleaseAILease(context.Background(), "user-a", acquired[0].ID); err != nil {
		t.Fatalf("release winner: %v", err)
	}
}

func TestAcquireAILeasePersistsMinuteAndUTCDayBuckets(t *testing.T) {
	t.Parallel()
	first, second := newAILimitTestStores(t)
	limits := AILimits{PerMinute: 1, PerDay: 2, MaxConcurrent: 1, LeaseTTL: 5 * time.Minute}
	now := time.Date(2026, 7, 14, 12, 34, 30, 0, time.UTC)

	lease, err := first.AcquireAILease(context.Background(), "user-a", AIOperationImage, limits, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.ReleaseAILease(context.Background(), "user-a", lease.ID); err != nil {
		t.Fatal(err)
	}

	_, err = second.AcquireAILease(context.Background(), "user-a", AIOperationImage, limits, now.Add(10*time.Second))
	limitErr := assertAILimitReason(t, err, AILimitReasonMinute)
	if limitErr.RetryAfter != 20*time.Second {
		t.Fatalf("minute RetryAfter = %s, want 20s", limitErr.RetryAfter)
	}

	lease, err = second.AcquireAILease(context.Background(), "user-a", AIOperationImage, limits, now.Add(31*time.Second))
	if err != nil {
		t.Fatalf("next minute acquire: %v", err)
	}
	if err := second.ReleaseAILease(context.Background(), "user-a", lease.ID); err != nil {
		t.Fatal(err)
	}

	_, err = first.AcquireAILease(context.Background(), "user-a", AIOperationImage, limits, now.Add(2*time.Minute))
	limitErr = assertAILimitReason(t, err, AILimitReasonDay)
	wantRetry := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC).Sub(now.Add(2 * time.Minute))
	if limitErr.RetryAfter != wantRetry {
		t.Fatalf("day RetryAfter = %s, want %s", limitErr.RetryAfter, wantRetry)
	}
}

func TestAcquireAILeaseIsolatesUsersAndOperations(t *testing.T) {
	t.Parallel()
	first, second := newAILimitTestStores(t)
	limits := AILimits{PerMinute: 1, PerDay: 1, MaxConcurrent: 1, LeaseTTL: 5 * time.Minute}
	now := time.Date(2026, 7, 14, 8, 15, 0, 0, time.UTC)

	lease, err := first.AcquireAILease(context.Background(), "user-a", AIOperationImage, limits, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.ReleaseAILease(context.Background(), "user-a", lease.ID); err != nil {
		t.Fatal(err)
	}
	_, err = second.AcquireAILease(context.Background(), "user-a", AIOperationImage, limits, now)
	_ = assertAILimitReason(t, err, AILimitReasonDay)

	otherUser, err := second.AcquireAILease(context.Background(), "user-b", AIOperationImage, limits, now)
	if err != nil {
		t.Fatalf("other user was not isolated: %v", err)
	}
	if err := second.ReleaseAILease(context.Background(), "user-b", otherUser.ID); err != nil {
		t.Fatal(err)
	}
	otherOperation, err := first.AcquireAILease(context.Background(), "user-a", AIOperationResearch, limits, now)
	if err != nil {
		t.Fatalf("other operation was not isolated: %v", err)
	}
	if err := first.ReleaseAILease(context.Background(), "user-a", otherOperation.ID); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireAILeaseRollsBackBucketsWhenLeaseInsertFails(t *testing.T) {
	t.Parallel()
	first, _ := newAILimitTestStores(t)
	ctx := context.Background()
	_, err := first.db.ExecContext(ctx, `
CREATE FUNCTION reject_ai_request_lease() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'forced lease failure';
END
$$;
CREATE TRIGGER reject_ai_request_lease
BEFORE INSERT ON ai_request_leases
FOR EACH ROW EXECUTE FUNCTION reject_ai_request_lease()`)
	if err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	limits := AILimits{PerMinute: 2, PerDay: 2, MaxConcurrent: 1, LeaseTTL: 5 * time.Minute}
	_, err = first.AcquireAILease(ctx, "user-a", AIOperationImage, limits, time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("acquire unexpectedly succeeded")
	}
	var buckets, leases int
	if err := first.db.QueryRowContext(ctx,
		`SELECT count(*) FROM ai_usage_buckets WHERE owner_id = ? AND operation = ?`, "user-a", AIOperationImage).Scan(&buckets); err != nil {
		t.Fatal(err)
	}
	if err := first.db.QueryRowContext(ctx,
		`SELECT count(*) FROM ai_request_leases WHERE owner_id = ? AND operation = ?`, "user-a", AIOperationImage).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if buckets != 0 || leases != 0 {
		t.Fatalf("rollback left buckets=%d leases=%d", buckets, leases)
	}

	if _, err := first.db.ExecContext(ctx, `DROP TRIGGER reject_ai_request_lease ON ai_request_leases; DROP FUNCTION reject_ai_request_lease()`); err != nil {
		t.Fatalf("drop failure trigger: %v", err)
	}
	lease, err := first.AcquireAILease(ctx, "user-a", AIOperationImage, limits, time.Date(2026, 7, 14, 9, 0, 1, 0, time.UTC))
	if err != nil {
		t.Fatalf("acquire after rollback: %v", err)
	}
	if err := first.ReleaseAILease(ctx, "user-a", lease.ID); err != nil {
		t.Fatal(err)
	}
}

func TestAILeaseReleaseOwnershipAndExpiry(t *testing.T) {
	t.Parallel()
	first, second := newAILimitTestStores(t)
	limits := AILimits{PerMinute: 20, PerDay: 20, MaxConcurrent: 1, LeaseTTL: 4 * time.Minute}
	now := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)

	lease, err := first.AcquireAILease(context.Background(), "user-a", AIOperationResearch, limits, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.ReleaseAILease(context.Background(), "user-b", lease.ID); err != nil {
		t.Fatalf("foreign release should be an idempotent no-op: %v", err)
	}
	_, err = second.AcquireAILease(context.Background(), "user-a", AIOperationResearch, limits, now.Add(time.Second))
	_ = assertAILimitReason(t, err, AILimitReasonConcurrency)
	if err := first.ReleaseAILease(context.Background(), "user-a", lease.ID); err != nil {
		t.Fatal(err)
	}
	releasedReplacement, err := second.AcquireAILease(context.Background(), "user-a", AIOperationResearch, limits, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := second.ReleaseAILease(context.Background(), "user-a", releasedReplacement.ID); err != nil {
		t.Fatal(err)
	}

	expiring, err := first.AcquireAILease(context.Background(), "user-a", AIOperationResearch, limits, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	afterExpiry, err := second.AcquireAILease(context.Background(), "user-a", AIOperationResearch, limits, expiring.ExpiresAt)
	if err != nil {
		t.Fatalf("acquire at lease expiry: %v", err)
	}
	if err := second.ReleaseAILease(context.Background(), "user-a", afterExpiry.ID); err != nil {
		t.Fatal(err)
	}
	var expiredStillStored int
	if err := first.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM ai_request_leases WHERE id = ?`, expiring.ID).Scan(&expiredStillStored); err != nil {
		t.Fatal(err)
	}
	if expiredStillStored != 0 {
		t.Fatalf("expired lease was not deleted")
	}
}

func TestAILimitsRejectUnboundedValues(t *testing.T) {
	t.Parallel()
	valid := AILimits{PerMinute: 1, PerDay: 1, MaxConcurrent: 1, LeaseTTL: time.Second}
	tests := []AILimits{
		{PerMinute: 0, PerDay: valid.PerDay, MaxConcurrent: valid.MaxConcurrent, LeaseTTL: valid.LeaseTTL},
		{PerMinute: MaxAIPerMinute + 1, PerDay: valid.PerDay, MaxConcurrent: valid.MaxConcurrent, LeaseTTL: valid.LeaseTTL},
		{PerMinute: valid.PerMinute, PerDay: MaxAIPerDay + 1, MaxConcurrent: valid.MaxConcurrent, LeaseTTL: valid.LeaseTTL},
		{PerMinute: valid.PerMinute, PerDay: valid.PerDay, MaxConcurrent: MaxAIConcurrent + 1, LeaseTTL: valid.LeaseTTL},
		{PerMinute: valid.PerMinute, PerDay: valid.PerDay, MaxConcurrent: valid.MaxConcurrent, LeaseTTL: MaxAILeaseTTL + time.Second},
	}
	for i, limits := range tests {
		if err := limits.Validate(); err == nil {
			t.Errorf("invalid limits[%d] passed validation: %+v", i, limits)
		}
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid limits rejected: %v", err)
	}
}

func TestOpenRuntimeRequiresAIQuotaMigration(t *testing.T) {
	t.Parallel()
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Fatal("TEST_DATABASE_URL is required")
	}
	leaseID, err := randomAILeaseID()
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_schema_version_" + leaseID
	admin, err := openPostgres(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	if _, err := admin.ExecContext(context.Background(), `CREATE SCHEMA `+quoteIdentifier(schema)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+quoteIdentifier(schema)+` CASCADE`)
	})
	testURL, err := withSearchPath(baseURL, schema)
	if err != nil {
		t.Fatal(err)
	}
	db, err := openPostgres(testURL)
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	var initialChecksum string
	for _, migration := range migrations {
		if migration.version == "001_initial.sql" {
			initialChecksum = migration.checksumSHA256
			break
		}
	}
	if initialChecksum == "" {
		_ = db.Close()
		t.Fatal("001_initial.sql is not embedded")
	}
	if _, err := db.ExecContext(context.Background(), `
CREATE TABLE schema_migrations (
	version TEXT PRIMARY KEY,
	checksum_sha256 TEXT NOT NULL,
	applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO schema_migrations(version, checksum_sha256) VALUES ('001_initial.sql', $1)`, initialChecksum); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()

	runtimeStore, err := OpenRuntime(context.Background(), testURL)
	if runtimeStore != nil {
		_ = runtimeStore.Close()
		t.Fatal("OpenRuntime accepted schema without AI limits migration")
	}
	if err == nil || !strings.Contains(err.Error(), "002_ai_limits.sql") {
		t.Fatalf("OpenRuntime error = %v", err)
	}
}

func newAILimitTestStores(t *testing.T) (*Store, *Store) {
	t.Helper()
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Fatal("TEST_DATABASE_URL is required for real PostgreSQL AI limit tests")
	}
	leaseID, err := randomAILeaseID()
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_ai_" + leaseID
	admin, err := openPostgres(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(context.Background(), `CREATE SCHEMA `+quoteIdentifier(schema)); err != nil {
		_ = admin.Close()
		t.Fatalf("create test schema: %v", err)
	}
	testURL, err := withSearchPath(baseURL, schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), testURL); err != nil {
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+quoteIdentifier(schema)+` CASCADE`)
		_ = admin.Close()
		t.Fatalf("migrate test schema: %v", err)
	}

	firstDB, err := openPostgres(testURL)
	if err != nil {
		t.Fatal(err)
	}
	secondDB, err := openPostgres(testURL)
	if err != nil {
		_ = firstDB.Close()
		t.Fatal(err)
	}
	firstDB.SetMaxOpenConns(1)
	secondDB.SetMaxOpenConns(1)
	first := &Store{db: &postgresDB{DB: firstDB}}
	second := &Store{db: &postgresDB{DB: secondDB}}
	for _, userID := range []string{"user-a", "user-b"} {
		if err := first.UpsertUser(context.Background(), User{ID: userID, DisplayName: userID}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = firstDB.Close()
		_ = secondDB.Close()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(cleanupCtx, `DROP SCHEMA IF EXISTS `+quoteIdentifier(schema)+` CASCADE`); err != nil {
			t.Errorf("drop AI limit test schema: %v", err)
		}
		_ = admin.Close()
	})
	return first, second
}

func assertDifferentPostgreSQLConnections(t *testing.T, first, second *sql.DB) {
	t.Helper()
	var firstPID, secondPID int
	if err := first.QueryRowContext(context.Background(), `SELECT pg_backend_pid()`).Scan(&firstPID); err != nil {
		t.Fatal(err)
	}
	if err := second.QueryRowContext(context.Background(), `SELECT pg_backend_pid()`).Scan(&secondPID); err != nil {
		t.Fatal(err)
	}
	if firstPID == secondPID {
		t.Fatalf("tests unexpectedly share PostgreSQL backend PID %d", firstPID)
	}
}

func assertAILimitReason(t *testing.T, err error, reason string) *AILimitError {
	t.Helper()
	var limitErr *AILimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("error = %v, want *AILimitError", err)
	}
	if limitErr.Reason != reason {
		t.Fatalf("limit reason = %q, want %q", limitErr.Reason, reason)
	}
	if limitErr.RetryAfter < time.Second {
		t.Fatalf("RetryAfter = %s, want at least 1s", limitErr.RetryAfter)
	}
	return limitErr
}

func ExampleAILimitError() {
	err := &AILimitError{Reason: AILimitReasonMinute, RetryAfter: 5 * time.Second}
	fmt.Println(err)
	// Output: AI minute limit exceeded; retry after 5s
}
