package app

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"maxpilot/backend/internal/store"
	"maxpilot/backend/internal/yandexwordstat"
)

type fakeWordstatQuotaStore struct {
	calls atomic.Int32
	err   error
}

func (f *fakeWordstatQuotaStore) AcquireWordstatQuota(
	context.Context, string, string, string, time.Time,
) error {
	f.calls.Add(1)
	return f.err
}

type countedWordstatProvider struct {
	calls   atomic.Int32
	result  yandexwordstat.TopResult
	err     error
	started chan struct{}
	release chan struct{}
}

func (f *countedWordstatProvider) GetTop(
	_ context.Context, _ yandexwordstat.TopRequest,
) (yandexwordstat.TopResult, error) {
	f.calls.Add(1)
	if f.started != nil {
		select {
		case f.started <- struct{}{}:
		default:
		}
	}
	if f.release != nil {
		<-f.release
	}
	return f.result, f.err
}

func TestWordstatNormalizedCacheAvoidsDuplicateLogicalQuota(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	quota := &fakeWordstatQuotaStore{}
	provider := &countedWordstatProvider{result: yandexwordstat.TopResult{
		TotalCount: 12, Results: []yandexwordstat.Phrase{{Phrase: "канал MAX", Count: 12}},
	}}
	application := &App{
		now: func() time.Time { return now }, wordstatQuota: quota,
		wordstatCache: make(map[string]wordstatCacheEntry),
	}
	if err := application.ConfigureWordstatWithProviderKey(provider, "provider-secret"); err != nil {
		t.Fatal(err)
	}

	first, firstFetchedAt, err := application.getWordstatTop(
		context.Background(), "user-a", "workspace-a", yandexwordstat.TopRequest{
			Phrase: "  Канал   MAX ", Limit: 10, Regions: []string{"225", "1", "225"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	first.Results[0].Phrase = "mutated by caller"
	second, secondFetchedAt, err := application.getWordstatTop(
		context.Background(), "user-a", "workspace-a", yandexwordstat.TopRequest{
			Phrase: "канал max", Limit: 10, Regions: []string{"1", "225"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls.Load() != 1 || quota.calls.Load() != 1 {
		t.Fatalf("provider calls=%d quota calls=%d, want one logical request",
			provider.calls.Load(), quota.calls.Load())
	}
	if second.Results[0].Phrase != "канал MAX" {
		t.Fatalf("cached result was aliased: %#v", second)
	}
	if !firstFetchedAt.Equal(secondFetchedAt) || !firstFetchedAt.Equal(now) {
		t.Fatalf("fetched_at changed across cache hit: %s / %s", firstFetchedAt, secondFetchedAt)
	}

	now = now.Add(wordstatCacheTTL + time.Second)
	if _, _, err := application.getWordstatTop(
		context.Background(), "user-a", "workspace-a",
		yandexwordstat.TopRequest{Phrase: "канал max", Limit: 10, Regions: []string{"1", "225"}},
	); err != nil {
		t.Fatal(err)
	}
	if provider.calls.Load() != 2 || quota.calls.Load() != 2 {
		t.Fatalf("expired cache provider calls=%d quota calls=%d, want two",
			provider.calls.Load(), quota.calls.Load())
	}
}

func TestWordstatCacheDoesNotCrossTenantOrActorBoundary(t *testing.T) {
	t.Parallel()
	quota := &fakeWordstatQuotaStore{}
	provider := &countedWordstatProvider{result: yandexwordstat.TopResult{
		TotalCount: 7, Results: []yandexwordstat.Phrase{{Phrase: "канал MAX", Count: 7}},
	}}
	application := &App{
		now: func() time.Time {
			return time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
		},
		wordstatQuota: quota, wordstatCache: make(map[string]wordstatCacheEntry),
	}
	if err := application.ConfigureWordstatWithProviderKey(provider, "provider-secret"); err != nil {
		t.Fatal(err)
	}
	request := yandexwordstat.TopRequest{Phrase: "канал MAX", Limit: 10, Regions: []string{"225"}}
	for _, tenant := range []struct{ actor, workspace string }{
		{"user-a", "workspace-a"},
		{"user-b", "workspace-a"},
		{"user-a", "workspace-b"},
	} {
		if _, _, err := application.getWordstatTop(
			context.Background(), tenant.actor, tenant.workspace, request,
		); err != nil {
			t.Fatal(err)
		}
	}
	if provider.calls.Load() != 3 || quota.calls.Load() != 3 {
		t.Fatalf("provider calls=%d quota calls=%d, want tenant-scoped 3/3",
			provider.calls.Load(), quota.calls.Load())
	}
}

func TestWordstatSingleflightCollapsesConcurrentRequests(t *testing.T) {
	t.Parallel()
	quota := &fakeWordstatQuotaStore{}
	provider := &countedWordstatProvider{
		result:  yandexwordstat.TopResult{TotalCount: 1},
		started: make(chan struct{}, 1), release: make(chan struct{}),
	}
	application := &App{
		now: func() time.Time {
			return time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
		},
		wordstatQuota: quota, wordstatCache: make(map[string]wordstatCacheEntry),
	}
	if err := application.ConfigureWordstatWithProviderKey(provider, "provider-secret"); err != nil {
		t.Fatal(err)
	}

	const requests = 12
	start := make(chan struct{})
	var ready sync.WaitGroup
	var done sync.WaitGroup
	ready.Add(requests)
	done.Add(requests)
	errorsSeen := make(chan error, requests)
	for index := 0; index < requests; index++ {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			_, _, err := application.getWordstatTop(
				context.Background(), "user-a", "workspace-a",
				yandexwordstat.TopRequest{Phrase: "канал MAX", Limit: 10, Regions: []string{"225"}},
			)
			errorsSeen <- err
		}()
	}
	ready.Wait()
	close(start)
	<-provider.started
	for index := 0; index < requests; index++ {
		runtime.Gosched()
	}
	close(provider.release)
	done.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if provider.calls.Load() != 1 || quota.calls.Load() != 1 {
		t.Fatalf("provider calls=%d quota calls=%d, want one",
			provider.calls.Load(), quota.calls.Load())
	}
}

func TestWordstatRateLimitsPreserveRetryAfter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		quotaError error
		provider   error
		wantReason string
		wantRetry  time.Duration
	}{
		{
			name: "local quota",
			quotaError: &store.WordstatQuotaError{
				Reason: store.WordstatQuotaReasonWorkspaceHour, RetryAfter: 7 * time.Minute,
			},
			wantReason: store.WordstatQuotaReasonWorkspaceHour, wantRetry: 7 * time.Minute,
		},
		{
			name: "provider 429",
			provider: &yandexwordstat.Error{
				StatusCode: http.StatusTooManyRequests, Code: "resource_exhausted",
				RetryAfter: 17 * time.Second,
			},
			wantReason: "provider", wantRetry: 17 * time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			quota := &fakeWordstatQuotaStore{err: tt.quotaError}
			provider := &countedWordstatProvider{err: tt.provider}
			application := &App{
				now: func() time.Time { return time.Now().UTC() }, wordstatQuota: quota,
				wordstatCache: make(map[string]wordstatCacheEntry),
			}
			if err := application.ConfigureWordstatWithProviderKey(provider, "provider-secret"); err != nil {
				t.Fatal(err)
			}
			_, _, err := application.getWordstatTop(
				context.Background(), "user-a", "workspace-a",
				yandexwordstat.TopRequest{Phrase: "канал MAX", Limit: 10, Regions: []string{"225"}},
			)
			var limitErr *WordstatRateLimitError
			if !errors.As(err, &limitErr) || limitErr.Reason != tt.wantReason ||
				limitErr.RetryAfter != tt.wantRetry {
				t.Fatalf("rate limit error = %#v (%v)", limitErr, err)
			}
			if tt.quotaError != nil && provider.calls.Load() != 0 {
				t.Fatalf("provider called after rejected quota: %d", provider.calls.Load())
			}
		})
	}
}

func TestWordstatSuggestionItemsKeepBoundedLongResponsePhrases(t *testing.T) {
	t.Parallel()
	copyOnlyPhrase := strings.Repeat("я", 600)
	oversizedPhrase := strings.Repeat("я", wordstatSuggestionMaxRunes+1)
	items := directKeywordSuggestionItems(yandexwordstat.TopResult{
		Results: []yandexwordstat.Phrase{
			{Phrase: copyOnlyPhrase, Count: 50},
			{Phrase: oversizedPhrase, Count: 40},
		},
		Associations: []yandexwordstat.Phrase{
			{Phrase: "короткая фраза", Count: 30},
		},
	}, 3)
	if len(items) != 2 || items[0].Phrase != copyOnlyPhrase ||
		items[0].Source != "included" || items[1].Phrase != "короткая фраза" {
		t.Fatalf("suggestion items = %#v", items)
	}
}
