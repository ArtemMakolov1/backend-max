package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"
)

const (
	UsageMetricChannels           = "channels"
	UsageMetricSeats              = "seats"
	UsageMetricStorageBytes       = "storage_bytes"
	UsageMetricAIImageCredits     = "ai_image_credits"
	UsageMetricAIResearchRequests = "ai_research_requests"
	UsageMetricAIFormatRequests   = "ai_format_requests"
)

var (
	workspaceUsageMetricPattern        = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	ErrWorkspaceEntitlementUnavailable = errors.New("workspace entitlement is unavailable")
)

// BillingPlan is one immutable catalog snapshot. A changed price or included
// allowance must be represented by another Version so an existing workspace
// subscription never changes underneath its historical usage.
type BillingPlan struct {
	Code              string `json:"code"`
	Version           int    `json:"version"`
	CatalogVersion    string `json:"catalog_version"`
	Name              string `json:"name"`
	Description       string `json:"description"`
	CurrencyCode      string `json:"currency_code"`
	MonthlyPriceMinor int64  `json:"monthly_price_minor"`
	BillingInterval   string `json:"billing_interval"`
	Public            bool   `json:"public"`
	Available         bool   `json:"available"`
}

// BillingEntitlement exposes both the user-facing allowance and its ledger
// representation. For images UnitScale=9 means one displayed medium-equivalent
// image consumes nine ai_image_credits; low/medium/high charges are 1/9/36.
type BillingEntitlement struct {
	Key            string `json:"key"`
	UsageMetric    string `json:"usage_metric"`
	Limit          int64  `json:"limit"`
	LimitBaseUnits int64  `json:"limit_base_units"`
	Unit           string `json:"unit"`
	Period         string `json:"period"`
	UnitScale      int64  `json:"unit_scale"`
	HardLimit      bool   `json:"hard_limit"`
}

type BillingCatalogEntry struct {
	Plan         BillingPlan          `json:"plan"`
	Entitlements []BillingEntitlement `json:"entitlements"`
}

type WorkspaceSubscriptionState struct {
	Plan      BillingPlan `json:"plan"`
	Status    string      `json:"status"`
	StartedAt time.Time   `json:"started_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}

type WorkspaceUsageMetric struct {
	Metric      string     `json:"metric"`
	Quantity    int64      `json:"quantity"`
	Period      string     `json:"period"`
	PeriodStart *time.Time `json:"period_start,omitempty"`
	PeriodEnd   *time.Time `json:"period_end,omitempty"`
}

type WorkspaceBillingState struct {
	WorkspaceID  string                     `json:"workspace_id"`
	Subscription WorkspaceSubscriptionState `json:"subscription"`
	Entitlements []BillingEntitlement       `json:"entitlements"`
	Usage        []WorkspaceUsageMetric     `json:"usage"`
}

type WorkspaceMonthlyUsage struct {
	WorkspaceID string    `json:"workspace_id"`
	Metric      string    `json:"metric"`
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end"`
	Quantity    int64     `json:"quantity"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// WorkspaceUsageLimitError is returned before the counter is incremented.
// Callers can safely reject the upstream request without refund logic.
type WorkspaceUsageLimitError struct {
	Metric     string
	Limit      int64
	Used       int64
	Requested  int64
	RetryAfter time.Duration
}

// WorkspacePlanInactiveError is independent from overage enforcement. A
// paused or canceled workspace must never create provider cost, including
// while the deployment is collecting usage in observe-only mode.
type WorkspacePlanInactiveError struct {
	Status string
}

func (e *WorkspacePlanInactiveError) Error() string {
	if e == nil || e.Status == "" {
		return "workspace plan is inactive"
	}
	return "workspace plan is " + e.Status
}

// UpdateWorkspaceSubscriptionStatus is a trusted lifecycle operation for a
// future billing worker. It is intentionally not exposed through the public
// API while checkout and payment-provider reconciliation are unavailable.
func (s *Store) UpdateWorkspaceSubscriptionStatus(
	ctx context.Context, workspaceID, status string, now time.Time,
) error {
	if s == nil || s.db == nil {
		return errors.New("store is required")
	}
	if workspaceID == "" {
		return errors.New("workspace is required")
	}
	switch status {
	case "active", "trialing", "paused", "canceled":
	default:
		return errors.New("invalid workspace subscription status")
	}
	if now.IsZero() {
		return errors.New("subscription update time is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE workspace_subscriptions
SET status=$2,updated_at=$3
WHERE workspace_id=$1`, workspaceID, status, now.UTC())
	if err != nil {
		return fmt.Errorf("update workspace subscription status: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotFound
	}
	return nil
}

func (e *WorkspaceUsageLimitError) Error() string {
	if e == nil {
		return "workspace usage limit exceeded"
	}
	return fmt.Sprintf("workspace %s monthly limit exceeded: used=%d requested=%d limit=%d",
		e.Metric, e.Used, e.Requested, e.Limit)
}

type billingRowsQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

const billingPlanColumns = `p.plan_code,p.version,p.catalog_version::text,p.name,p.description,
p.currency_code,p.monthly_price_minor,p.billing_interval,p.public,p.available`

// ListPublicBillingPlans is the only unauthenticated catalog read. The SQL
// predicate is deliberately fail-closed: internal future prices never depend
// on an HTTP-layer filter to remain hidden.
func (s *Store) ListPublicBillingPlans(ctx context.Context) ([]BillingCatalogEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+billingPlanColumns+`,
e.entitlement_key,e.usage_metric,e.limit_value,e.limit_value*e.unit_scale,
e.unit,e.period,e.unit_scale,e.hard_limit
FROM billing_plan_versions p
JOIN billing_plan_entitlements e
  ON e.plan_code=p.plan_code AND e.plan_version=p.version
WHERE p.public=TRUE AND p.available=TRUE
ORDER BY p.monthly_price_minor,p.plan_code,p.version,e.entitlement_key`)
	if err != nil {
		return nil, fmt.Errorf("list public billing plans: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make([]BillingCatalogEntry, 0)
	for rows.Next() {
		var plan BillingPlan
		var entitlement BillingEntitlement
		if err := scanBillingPlanAndEntitlement(rows, &plan, &entitlement); err != nil {
			return nil, fmt.Errorf("scan public billing plan: %w", err)
		}
		last := len(result) - 1
		if last < 0 || result[last].Plan.Code != plan.Code || result[last].Plan.Version != plan.Version {
			result = append(result, BillingCatalogEntry{Plan: plan, Entitlements: make([]BillingEntitlement, 0, 6)})
			last++
		}
		result[last].Entitlements = append(result[last].Entitlements, entitlement)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate public billing plans: %w", err)
	}
	return result, nil
}

// GetWorkspaceBillingState requires membership in the requested workspace and
// never accepts an owner/user alias as the tenant key. This keeps subscription
// and usage rows undiscoverable across workspaces even if a handler is later
// refactored without its current authorization guard.
func (s *Store) GetWorkspaceBillingState(
	ctx context.Context, actorUserID, workspaceID string, now time.Time,
) (WorkspaceBillingState, error) {
	if actorUserID == "" || workspaceID == "" {
		return WorkspaceBillingState{}, ErrNotFound
	}
	if now.IsZero() {
		return WorkspaceBillingState{}, errors.New("billing usage time is required")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return WorkspaceBillingState{}, fmt.Errorf("begin workspace billing read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := resolveWorkspaceAccess(ctx, tx, actorUserID, workspaceID); err != nil {
		return WorkspaceBillingState{}, err
	}

	state := WorkspaceBillingState{WorkspaceID: workspaceID}
	if err := tx.QueryRowContext(ctx, `SELECT `+billingPlanColumns+`,
s.status,s.started_at,s.updated_at
FROM workspace_subscriptions s
JOIN billing_plan_versions p
  ON p.plan_code=s.plan_code AND p.version=s.plan_version
WHERE s.workspace_id=$1`, workspaceID).Scan(
		&state.Subscription.Plan.Code, &state.Subscription.Plan.Version,
		&state.Subscription.Plan.CatalogVersion, &state.Subscription.Plan.Name,
		&state.Subscription.Plan.Description, &state.Subscription.Plan.CurrencyCode,
		&state.Subscription.Plan.MonthlyPriceMinor, &state.Subscription.Plan.BillingInterval,
		&state.Subscription.Plan.Public, &state.Subscription.Plan.Available,
		&state.Subscription.Status, &state.Subscription.StartedAt, &state.Subscription.UpdatedAt,
	); err != nil {
		return WorkspaceBillingState{}, fmt.Errorf("read workspace subscription: %w", err)
	}
	state.Subscription.StartedAt = state.Subscription.StartedAt.UTC()
	state.Subscription.UpdatedAt = state.Subscription.UpdatedAt.UTC()

	state.Entitlements, err = readBillingEntitlements(
		ctx, tx, state.Subscription.Plan.Code, state.Subscription.Plan.Version)
	if err != nil {
		return WorkspaceBillingState{}, err
	}
	state.Usage, err = readWorkspaceUsage(ctx, tx, workspaceID, state.Entitlements, now.UTC())
	if err != nil {
		return WorkspaceBillingState{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkspaceBillingState{}, fmt.Errorf("commit workspace billing read: %w", err)
	}
	return state, nil
}

// ChargeWorkspaceMonthlyUsage atomically adds an arbitrary positive amount to
// one calendar-month metric. Observe mode (enforce=false) always records the
// charge. Enforce mode first resolves the workspace's versioned entitlement
// and rejects an over-limit charge without changing the counter.
func (s *Store) ChargeWorkspaceMonthlyUsage(
	ctx context.Context, workspaceID, metric string, amount int64, enforce bool, now time.Time,
) (WorkspaceMonthlyUsage, error) {
	if s == nil || s.db == nil {
		return WorkspaceMonthlyUsage{}, errors.New("store is required")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return WorkspaceMonthlyUsage{}, fmt.Errorf("begin monthly usage charge: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	usage, err := chargeWorkspaceMonthlyUsageTx(ctx, tx, workspaceID, metric, amount, enforce, now)
	if err != nil {
		return WorkspaceMonthlyUsage{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkspaceMonthlyUsage{}, fmt.Errorf("commit monthly usage charge: %w", err)
	}
	return usage, nil
}

func chargeWorkspaceMonthlyUsageTx(
	ctx context.Context, tx *sql.Tx, workspaceID, metric string, amount int64, enforce bool, now time.Time,
) (WorkspaceMonthlyUsage, error) {
	if workspaceID == "" || !workspaceUsageMetricPattern.MatchString(metric) {
		return WorkspaceMonthlyUsage{}, errors.New("workspace and valid usage metric are required")
	}
	if amount <= 0 {
		return WorkspaceMonthlyUsage{}, errors.New("monthly usage amount must be positive")
	}
	if now.IsZero() {
		return WorkspaceMonthlyUsage{}, errors.New("monthly usage time is required")
	}
	now = now.UTC()
	periodStart, periodEnd := workspaceMonthlyUsagePeriod(now)
	if err := requireActiveWorkspaceSubscription(ctx, tx, workspaceID); err != nil {
		return WorkspaceMonthlyUsage{}, err
	}
	lockKey := "maxstudio:workspace-usage:" + workspaceID + ":" + metric + ":" + periodStart.Format("2006-01-02")
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1,$2))`, lockKey, aiAdvisoryLockSeed); err != nil {
		return WorkspaceMonthlyUsage{}, fmt.Errorf("lock monthly workspace usage: %w", err)
	}

	var used int64
	err := tx.QueryRowContext(ctx, `SELECT quantity FROM workspace_usage_monthly
WHERE workspace_id=$1 AND period_start=$2 AND metric=$3`, workspaceID, periodStart, metric).Scan(&used)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return WorkspaceMonthlyUsage{}, fmt.Errorf("read monthly workspace usage: %w", err)
	}
	if enforce {
		limit, limitErr := readWorkspaceMonthlyEntitlementLimit(ctx, tx, workspaceID, metric)
		if limitErr != nil {
			return WorkspaceMonthlyUsage{}, limitErr
		}
		if used > limit || amount > limit-used {
			return WorkspaceMonthlyUsage{}, &WorkspaceUsageLimitError{
				Metric: metric, Limit: limit, Used: used, Requested: amount,
				RetryAfter: positiveRetryAfter(periodEnd.Sub(now)),
			}
		}
	}

	usage := WorkspaceMonthlyUsage{
		WorkspaceID: workspaceID, Metric: metric, PeriodStart: periodStart, PeriodEnd: periodEnd,
	}
	if err := tx.QueryRowContext(ctx, `INSERT INTO workspace_usage_monthly(
workspace_id,period_start,metric,quantity,updated_at)
VALUES($1,$2,$3,$4,$5)
ON CONFLICT(workspace_id,period_start,metric) DO UPDATE SET
quantity=workspace_usage_monthly.quantity+EXCLUDED.quantity,
updated_at=EXCLUDED.updated_at
RETURNING quantity,updated_at`, workspaceID, periodStart, metric, amount, now).Scan(
		&usage.Quantity, &usage.UpdatedAt,
	); err != nil {
		return WorkspaceMonthlyUsage{}, fmt.Errorf("charge monthly workspace usage: %w", err)
	}
	usage.UpdatedAt = usage.UpdatedAt.UTC()
	return usage, nil
}

func requireActiveWorkspaceSubscription(ctx context.Context, tx *sql.Tx, workspaceID string) error {
	var status string
	err := tx.QueryRowContext(ctx, `SELECT status FROM workspace_subscriptions
WHERE workspace_id=$1 FOR SHARE`, workspaceID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: subscription", ErrWorkspaceEntitlementUnavailable)
	}
	if err != nil {
		return fmt.Errorf("read workspace subscription status: %w", err)
	}
	if status != "active" && status != "trialing" {
		return &WorkspacePlanInactiveError{Status: status}
	}
	return nil
}

func readWorkspaceMonthlyEntitlementLimit(
	ctx context.Context, tx *sql.Tx, workspaceID, metric string,
) (int64, error) {
	var limit, scale int64
	err := tx.QueryRowContext(ctx, `SELECT e.limit_value,e.unit_scale
FROM workspace_subscriptions s
JOIN billing_plan_entitlements e
  ON e.plan_code=s.plan_code AND e.plan_version=s.plan_version
WHERE s.workspace_id=$1
  AND s.status IN ('active','trialing')
  AND e.usage_metric=$2
  AND e.period='month'
  AND e.hard_limit=TRUE`, workspaceID, metric).Scan(&limit, &scale)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s", ErrWorkspaceEntitlementUnavailable, metric)
	}
	if err != nil {
		return 0, fmt.Errorf("read workspace entitlement: %w", err)
	}
	if scale <= 0 || limit > math.MaxInt64/scale {
		return 0, fmt.Errorf("%w: invalid limit for %s", ErrWorkspaceEntitlementUnavailable, metric)
	}
	return limit * scale, nil
}

func readBillingEntitlements(
	ctx context.Context, q billingRowsQueryer, planCode string, planVersion int,
) ([]BillingEntitlement, error) {
	rows, err := q.QueryContext(ctx, `SELECT entitlement_key,usage_metric,limit_value,
limit_value*unit_scale,unit,period,unit_scale,hard_limit
FROM billing_plan_entitlements
WHERE plan_code=$1 AND plan_version=$2
ORDER BY entitlement_key`, planCode, planVersion)
	if err != nil {
		return nil, fmt.Errorf("read billing entitlements: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]BillingEntitlement, 0, 6)
	for rows.Next() {
		var entitlement BillingEntitlement
		if err := rows.Scan(
			&entitlement.Key, &entitlement.UsageMetric, &entitlement.Limit,
			&entitlement.LimitBaseUnits, &entitlement.Unit, &entitlement.Period,
			&entitlement.UnitScale, &entitlement.HardLimit,
		); err != nil {
			return nil, fmt.Errorf("scan billing entitlement: %w", err)
		}
		result = append(result, entitlement)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate billing entitlements: %w", err)
	}
	return result, nil
}

func readWorkspaceUsage(
	ctx context.Context, tx *sql.Tx, workspaceID string, entitlements []BillingEntitlement, now time.Time,
) ([]WorkspaceUsageMetric, error) {
	periodStart, periodEnd := workspaceMonthlyUsagePeriod(now)
	quantities := map[string]int64{}
	rows, err := tx.QueryContext(ctx, `SELECT metric,quantity FROM workspace_usage_monthly
WHERE workspace_id=$1 AND period_start=$2`, workspaceID, periodStart)
	if err != nil {
		return nil, fmt.Errorf("read workspace monthly usage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var metric string
		var quantity int64
		if err := rows.Scan(&metric, &quantity); err != nil {
			scanErr := fmt.Errorf("scan workspace monthly usage: %w", err)
			if closeErr := rows.Close(); closeErr != nil {
				return nil, errors.Join(scanErr, closeErr)
			}
			return nil, scanErr
		}
		quantities[metric] = quantity
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close workspace monthly usage: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workspace monthly usage: %w", err)
	}

	var channels, seats, storageBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT
  (SELECT count(*) FROM channels WHERE workspace_id=$1),
  (SELECT count(*) FROM workspace_members WHERE workspace_id=$1),
  COALESCE((SELECT total_bytes FROM workspace_media_usage WHERE workspace_id=$1),0)`, workspaceID).Scan(
		&channels, &seats, &storageBytes,
	); err != nil {
		return nil, fmt.Errorf("read workspace current usage: %w", err)
	}
	quantities[UsageMetricChannels] = channels
	quantities[UsageMetricSeats] = seats
	quantities[UsageMetricStorageBytes] = storageBytes

	result := make([]WorkspaceUsageMetric, 0, len(entitlements))
	seen := make(map[string]struct{}, len(entitlements))
	for _, entitlement := range entitlements {
		if _, exists := seen[entitlement.UsageMetric]; exists {
			continue
		}
		seen[entitlement.UsageMetric] = struct{}{}
		metric := WorkspaceUsageMetric{
			Metric: entitlement.UsageMetric, Quantity: quantities[entitlement.UsageMetric], Period: entitlement.Period,
		}
		if entitlement.Period == "month" {
			start, end := periodStart, periodEnd
			metric.PeriodStart, metric.PeriodEnd = &start, &end
		}
		result = append(result, metric)
	}
	return result, nil
}

func scanBillingPlanAndEntitlement(
	scanner interface{ Scan(...any) error }, plan *BillingPlan, entitlement *BillingEntitlement,
) error {
	return scanner.Scan(
		&plan.Code, &plan.Version, &plan.CatalogVersion, &plan.Name, &plan.Description,
		&plan.CurrencyCode, &plan.MonthlyPriceMinor, &plan.BillingInterval, &plan.Public, &plan.Available,
		&entitlement.Key, &entitlement.UsageMetric, &entitlement.Limit,
		&entitlement.LimitBaseUnits, &entitlement.Unit, &entitlement.Period,
		&entitlement.UnitScale, &entitlement.HardLimit,
	)
}

func workspaceMonthlyUsagePeriod(now time.Time) (time.Time, time.Time) {
	now = now.UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

func monthlyUsageMetricForAIOperation(operation string) (string, error) {
	switch operation {
	case AIOperationImage:
		return UsageMetricAIImageCredits, nil
	case AIOperationResearch:
		return UsageMetricAIResearchRequests, nil
	default:
		return "", errors.New("unsupported AI operation")
	}
}
