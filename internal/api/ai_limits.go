package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"maxpilot/backend/internal/store"
)

const (
	AIHandlerTimeout = 3 * time.Minute

	defaultAIGlobalMaxConcurrent = 4
	defaultAIUserMaxConcurrent   = 1
	defaultAIImagePerMinute      = 2
	defaultAIImagePerDay         = 20
	defaultAIResearchPerMinute   = 2
	defaultAIResearchPerDay      = 20
	defaultAILeaseTTL            = 4 * time.Minute
)

// AILimitOptions are deliberately conservative defaults for a shared OpenAI
// account. The two image endpoints share the image operation, so users cannot
// bypass a quota by switching routes.
type AILimitOptions struct {
	GlobalMaxConcurrent    int
	UserMaxConcurrent      int
	ImagePerMinute         int
	ImagePerDay            int
	ResearchPerMinute      int
	ResearchPerDay         int
	LeaseTTL               time.Duration
	MonthlyPlanEnforcement bool
}

func DefaultAILimitOptions() AILimitOptions {
	return AILimitOptions{
		GlobalMaxConcurrent: defaultAIGlobalMaxConcurrent,
		UserMaxConcurrent:   defaultAIUserMaxConcurrent,
		ImagePerMinute:      defaultAIImagePerMinute,
		ImagePerDay:         defaultAIImagePerDay,
		ResearchPerMinute:   defaultAIResearchPerMinute,
		ResearchPerDay:      defaultAIResearchPerDay,
		LeaseTTL:            defaultAILeaseTTL,
	}
}

func (o AILimitOptions) Validate() error {
	if o.GlobalMaxConcurrent <= 0 || o.GlobalMaxConcurrent > store.MaxAIConcurrent {
		return fmt.Errorf("AI global concurrency must be between 1 and %d", store.MaxAIConcurrent)
	}
	if o.LeaseTTL <= AIHandlerTimeout {
		return fmt.Errorf("AI lease TTL must be greater than the %s handler timeout", AIHandlerTimeout)
	}
	for name, limits := range map[string]store.AILimits{
		"image": {
			PerMinute: o.ImagePerMinute, PerDay: o.ImagePerDay,
			MaxConcurrent: o.UserMaxConcurrent, LeaseTTL: o.LeaseTTL,
		},
		"research": {
			PerMinute: o.ResearchPerMinute, PerDay: o.ResearchPerDay,
			MaxConcurrent: o.UserMaxConcurrent, LeaseTTL: o.LeaseTTL,
		},
	} {
		if err := limits.Validate(); err != nil {
			return fmt.Errorf("%s AI limits: %w", name, err)
		}
	}
	return nil
}

type aiRequestLimiter struct {
	storage *store.Store
	logger  *slog.Logger
	options AILimitOptions
	slots   chan struct{}
}

func newAIRequestLimiter(storage *store.Store, logger *slog.Logger, options AILimitOptions) *aiRequestLimiter {
	if err := options.Validate(); err != nil {
		panic(fmt.Sprintf("invalid AI limit options: %v", err))
	}
	return &aiRequestLimiter{
		storage: storage,
		logger:  logger,
		options: options,
		slots:   make(chan struct{}, options.GlobalMaxConcurrent),
	}
}

func (l *aiRequestLimiter) acquire(ctx context.Context, userID, operation string, now time.Time) (func(), error) {
	return l.acquireAmount(ctx, userID, operation, 1, now)
}

func (l *aiRequestLimiter) acquireAmount(
	ctx context.Context, userID, operation string, amount int64, now time.Time,
) (func(), error) {
	return l.acquireMetric(ctx, userID, operation, defaultAIMonthlyMetric(operation), amount, now)
}

func (l *aiRequestLimiter) acquireMetric(
	ctx context.Context, userID, operation, monthlyMetric string, amount int64, now time.Time,
) (func(), error) {
	select {
	case l.slots <- struct{}{}:
	default:
		return nil, &store.AILimitError{Reason: store.AILimitReasonGlobal, RetryAfter: time.Second}
	}

	limits, err := l.limitsFor(operation)
	if err != nil {
		<-l.slots
		return nil, err
	}
	lease, err := l.storage.AcquireAILeaseWithMonthlyUsage(
		ctx, userID, operation, monthlyMetric, amount, l.options.MonthlyPlanEnforcement, limits, now)
	if err != nil {
		<-l.slots
		return nil, err
	}

	var once sync.Once
	release := func() {
		once.Do(func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := l.storage.ReleaseAILease(releaseCtx, userID, lease.ID); err != nil {
				l.logger.Error("could not release AI request lease", "operation", operation, "error", err)
			}
			<-l.slots
		})
	}
	return release, nil
}

func (l *aiRequestLimiter) acquireWorkspace(ctx context.Context, workspaceID, operation string, now time.Time) (func(), error) {
	return l.acquireWorkspaceAmount(ctx, workspaceID, operation, 1, now)
}

func (l *aiRequestLimiter) acquireWorkspaceAmount(
	ctx context.Context, workspaceID, operation string, amount int64, now time.Time,
) (func(), error) {
	return l.acquireWorkspaceMetric(
		ctx, workspaceID, operation, defaultAIMonthlyMetric(operation), amount, now)
}

func (l *aiRequestLimiter) acquireWorkspaceMetric(
	ctx context.Context, workspaceID, operation, monthlyMetric string, amount int64, now time.Time,
) (func(), error) {
	select {
	case l.slots <- struct{}{}:
	default:
		return nil, &store.AILimitError{Reason: store.AILimitReasonGlobal, RetryAfter: time.Second}
	}
	limits, err := l.limitsFor(operation)
	if err != nil {
		<-l.slots
		return nil, err
	}
	lease, err := l.storage.AcquireWorkspaceAILeaseWithMonthlyUsage(
		ctx, workspaceID, operation, monthlyMetric, amount, l.options.MonthlyPlanEnforcement, limits, now)
	if err != nil {
		<-l.slots
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := l.storage.ReleaseWorkspaceAILease(releaseCtx, workspaceID, lease.ID); err != nil {
				l.logger.Error("could not release workspace AI request lease", "operation", operation, "error", err)
			}
			<-l.slots
		})
	}, nil
}

func (l *aiRequestLimiter) acquireForWorkspace(
	ctx context.Context, actorUserID string, workspace store.Workspace, operation string, now time.Time,
) (func(), error) {
	return l.acquireForWorkspaceAmount(ctx, actorUserID, workspace, operation, 1, now)
}

func (l *aiRequestLimiter) acquireForWorkspaceAmount(
	ctx context.Context, actorUserID string, workspace store.Workspace, operation string, amount int64, now time.Time,
) (func(), error) {
	return l.acquireForWorkspaceMetric(
		ctx, actorUserID, workspace, operation, defaultAIMonthlyMetric(operation), amount, now)
}

func (l *aiRequestLimiter) acquireForWorkspaceMetric(
	ctx context.Context, actorUserID string, workspace store.Workspace,
	operation, monthlyMetric string, amount int64, now time.Time,
) (func(), error) {
	// Nested personal routes and their legacy aliases must share the same
	// buckets and leases; otherwise alternating between them doubles every
	// minute/day/concurrency allowance. Team workspaces keep an independent
	// workspace ledger because several members share that tenant.
	if workspace.IsPersonal {
		return l.acquireMetric(ctx, actorUserID, operation, monthlyMetric, amount, now)
	}
	return l.acquireWorkspaceMetric(ctx, workspace.ID, operation, monthlyMetric, amount, now)
}

func (l *aiRequestLimiter) limitsFor(operation string) (store.AILimits, error) {
	limits := store.AILimits{MaxConcurrent: l.options.UserMaxConcurrent, LeaseTTL: l.options.LeaseTTL}
	switch operation {
	case store.AIOperationImage:
		limits.PerMinute, limits.PerDay = l.options.ImagePerMinute, l.options.ImagePerDay
	case store.AIOperationResearch:
		limits.PerMinute, limits.PerDay = l.options.ResearchPerMinute, l.options.ResearchPerDay
	default:
		return store.AILimits{}, errors.New("unsupported AI operation")
	}
	return limits, nil
}

func retryAfterSeconds(value time.Duration) int64 {
	if value <= 0 {
		return 1
	}
	seconds := int64(value / time.Second)
	if value%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		return 1
	}
	return seconds
}

// imageUsageCredits reflects the material provider-cost difference between
// qualities. Empty quality follows the client default (medium); auto is charged
// conservatively as high because the provider may choose the expensive tier.
func imageUsageCredits(quality string) int64 {
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "low":
		return 1
	case "high", "auto":
		return 36
	default:
		return 9
	}
}

func defaultAIMonthlyMetric(operation string) string {
	if operation == store.AIOperationImage {
		return store.UsageMetricAIImageCredits
	}
	return store.UsageMetricAIResearchRequests
}
