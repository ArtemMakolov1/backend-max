package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAcquireWordstatQuotaSerializesProviderKeyAcrossReplicas(t *testing.T) {
	t.Parallel()
	first, second := newAILimitTestStores(t)
	assertDifferentPostgreSQLConnections(t, first.db.DB, second.db.DB)
	ctx := context.Background()
	for _, userID := range []string{"wordstat-c", "wordstat-d"} {
		if err := first.UpsertUser(ctx, User{ID: userID, DisplayName: userID}); err != nil {
			t.Fatal(err)
		}
	}
	users := []string{"user-a", "user-b", "wordstat-c", "wordstat-d"}
	workspaces := make([]string, 0, len(users))
	for _, userID := range users {
		workspaces = append(workspaces, personalWorkspaceIDForTest(t, first, userID))
	}

	providerKeyHash := strings.Repeat("a", 64)
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	start := make(chan struct{})
	type result struct{ err error }
	results := make(chan result, len(users))
	var group sync.WaitGroup
	for index, userID := range users {
		storage := first
		if index%2 != 0 {
			storage = second
		}
		workspaceID := workspaces[index]
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			results <- result{err: storage.AcquireWordstatQuota(
				ctx, providerKeyHash, userID, workspaceID, now,
			)}
		}()
	}
	close(start)
	group.Wait()
	close(results)

	accepted, rejected := 0, 0
	for result := range results {
		if result.err == nil {
			accepted++
			continue
		}
		var quotaErr *WordstatQuotaError
		if !errors.As(result.err, &quotaErr) || quotaErr.Reason != WordstatQuotaReasonProviderSecond ||
			quotaErr.RetryAfter != time.Second {
			t.Fatalf("rejection = %#v (%v)", quotaErr, result.err)
		}
		rejected++
	}
	if accepted != WordstatProviderLogicalPerSecond || rejected != 1 {
		t.Fatalf("accepted=%d rejected=%d, want %d/1", accepted, rejected, WordstatProviderLogicalPerSecond)
	}
	var recorded int
	if err := first.db.QueryRowContext(ctx,
		`SELECT count(*) FROM wordstat_quota_events WHERE provider_key_hash=$1`, providerKeyHash,
	).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != WordstatProviderLogicalPerSecond {
		t.Fatalf("recorded events=%d, want %d", recorded, WordstatProviderLogicalPerSecond)
	}
}

func TestAcquireWordstatQuotaEnforcesUserWorkspaceRollingWindows(t *testing.T) {
	t.Parallel()
	first, second := newAILimitTestStores(t)
	ctx := context.Background()
	providerKeyHash := strings.Repeat("b", 64)
	workspaceID := personalWorkspaceIDForTest(t, first, "user-a")
	now := time.Date(2026, time.August, 2, 14, 0, 0, 0, time.UTC)

	if err := first.AcquireWordstatQuota(ctx, providerKeyHash, "user-a", workspaceID, now); err != nil {
		t.Fatal(err)
	}
	err := second.AcquireWordstatQuota(
		ctx, providerKeyHash, "user-a", workspaceID, now.Add(500*time.Millisecond),
	)
	quotaErr := assertWordstatQuotaReason(
		t, err, WordstatQuotaReasonUserWorkspaceSecond,
	)
	if quotaErr.RetryAfter != time.Second {
		t.Fatalf("RetryAfter=%s, want %s", quotaErr.RetryAfter, time.Second)
	}
	if err := second.AcquireWordstatQuota(
		ctx, providerKeyHash, "user-a", workspaceID, now.Add(time.Second),
	); err != nil {
		t.Fatalf("rolling second did not reopen at boundary: %v", err)
	}
}

func TestAcquireWordstatQuotaEnforcesUserWorkspaceHourlyFairUse(t *testing.T) {
	t.Parallel()
	first, second := newAILimitTestStores(t)
	ctx := context.Background()
	providerKeyHash := strings.Repeat("c", 64)
	workspaceID := personalWorkspaceIDForTest(t, first, "user-a")
	now := time.Date(2026, time.August, 2, 16, 0, 0, 0, time.UTC)
	for index := 0; index < WordstatUserWorkspacePerHour; index++ {
		storage := first
		if index%2 != 0 {
			storage = second
		}
		if err := storage.AcquireWordstatQuota(
			ctx, providerKeyHash, "user-a", workspaceID, now.Add(time.Duration(index)*2*time.Second),
		); err != nil {
			t.Fatalf("request %d: %v", index+1, err)
		}
	}
	rejectedAt := now.Add(time.Duration(WordstatUserWorkspacePerHour) * 2 * time.Second)
	err := second.AcquireWordstatQuota(ctx, providerKeyHash, "user-a", workspaceID, rejectedAt)
	quotaErr := assertWordstatQuotaReason(t, err, WordstatQuotaReasonUserWorkspaceHour)
	wantRetry := now.Add(time.Hour).Sub(rejectedAt)
	if quotaErr.RetryAfter != wantRetry {
		t.Fatalf("RetryAfter=%s, want %s", quotaErr.RetryAfter, wantRetry)
	}
}

func personalWorkspaceIDForTest(t *testing.T, storage *Store, userID string) string {
	t.Helper()
	workspaces, err := storage.ListWorkspaces(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	for _, access := range workspaces {
		if access.Workspace.IsPersonal {
			return access.Workspace.ID
		}
	}
	t.Fatalf("personal workspace for %q is missing", userID)
	return ""
}

func assertWordstatQuotaReason(t *testing.T, err error, reason string) *WordstatQuotaError {
	t.Helper()
	var quotaErr *WordstatQuotaError
	if !errors.As(err, &quotaErr) {
		t.Fatalf("error=%v, want *WordstatQuotaError", err)
	}
	if quotaErr.Reason != reason || quotaErr.RetryAfter < time.Second {
		t.Fatalf("quota error=%#v, want reason %q with positive retry", quotaErr, reason)
	}
	return quotaErr
}
