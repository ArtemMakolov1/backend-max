package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestBillingCatalogKeepsFuturePlansInternalAndCreatesFreeSubscriptions(t *testing.T) {
	storage, err := Open(context.Background(), filepath.Join(t.TempDir(), "billing-catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	ctx := context.Background()
	for _, user := range []User{
		{ID: "billing-owner", DisplayName: "Owner"},
		{ID: "billing-viewer", DisplayName: "Viewer"},
		{ID: "billing-outsider", DisplayName: "Outsider"},
	} {
		if err := storage.UpsertUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}

	public, err := storage.ListPublicBillingPlans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(public) != 1 || public[0].Plan.Code != "free" || public[0].Plan.MonthlyPriceMinor != 0 ||
		!public[0].Plan.Public || !public[0].Plan.Available || len(public[0].Entitlements) != 6 {
		t.Fatalf("public catalog = %#v", public)
	}
	image := entitlementByKey(t, public[0].Entitlements, "ai_images_monthly")
	if image.Limit != 3 || image.LimitBaseUnits != 27 || image.UnitScale != 9 ||
		image.UsageMetric != UsageMetricAIImageCredits || !image.HardLimit {
		t.Fatalf("free image entitlement = %#v", image)
	}
	if channels := entitlementByKey(t, public[0].Entitlements, "channels"); channels.HardLimit {
		t.Fatalf("channels must remain display-only before paid launch: %#v", channels)
	}
	format := entitlementByKey(t, public[0].Entitlements, "ai_format_monthly")
	if format.Limit != 10 || format.UsageMetric != UsageMetricAIFormatRequests {
		t.Fatalf("free format entitlement = %#v", format)
	}

	wantPrices := map[string]int64{"free": 0, "solo": 149000, "pro": 549000, "agency": 1599000}
	rows, err := storage.db.QueryContext(ctx, `SELECT plan_code,monthly_price_minor,public,available
FROM billing_plan_versions ORDER BY plan_code`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	seen := make(map[string]int64)
	for rows.Next() {
		var code string
		var price int64
		var isPublic, available bool
		if err := rows.Scan(&code, &price, &isPublic, &available); err != nil {
			t.Fatal(err)
		}
		seen[code] = price
		if code != "free" && (isPublic || available) {
			t.Fatalf("internal plan %s is public=%v available=%v", code, isPublic, available)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for code, price := range wantPrices {
		if seen[code] != price {
			t.Fatalf("price %s=%d, want %d (catalog=%v)", code, seen[code], price, seen)
		}
	}

	workspace, err := storage.CreateWorkspace(ctx, "billing-owner", Workspace{Name: "Billing team"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.AddWorkspaceMember(ctx, "billing-owner", WorkspaceMember{
		WorkspaceID: workspace.ID, UserID: "billing-viewer", Role: WorkspaceRoleViewer,
	}); err != nil {
		t.Fatal(err)
	}
	state, err := storage.GetWorkspaceBillingState(ctx, "billing-viewer", workspace.ID,
		time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if state.Subscription.Plan.Code != "free" || state.Subscription.Status != "active" || len(state.Usage) != 6 {
		t.Fatalf("workspace billing state = %#v", state)
	}
	if _, err := storage.GetWorkspaceBillingState(ctx, "billing-outsider", workspace.ID, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider billing error=%v, want ErrNotFound", err)
	}
}

func TestMonthlyUsageObserveAndEnforceAreAtomicAndQuantityBased(t *testing.T) {
	storage, err := Open(context.Background(), filepath.Join(t.TempDir(), "billing-usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	ctx := context.Background()
	if err := storage.UpsertUser(ctx, User{ID: "usage-owner", DisplayName: "Owner"}); err != nil {
		t.Fatal(err)
	}
	workspace, err := storage.CreateWorkspace(ctx, "usage-owner", Workspace{Name: "Usage team"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	// Observe mode records an expensive high-quality request even though the
	// Free allowance is only 27 credits.
	usage, err := storage.ChargeWorkspaceMonthlyUsage(
		ctx, workspace.ID, UsageMetricAIImageCredits, 36, false, now)
	if err != nil || usage.Quantity != 36 {
		t.Fatalf("observe charge = %#v, %v", usage, err)
	}
	usage, err = storage.ChargeWorkspaceMonthlyUsage(
		ctx, workspace.ID, UsageMetricAIImageCredits, 1, false, now.Add(time.Minute))
	if err != nil || usage.Quantity != 37 {
		t.Fatalf("second observe charge = %#v, %v", usage, err)
	}

	_, err = storage.ChargeWorkspaceMonthlyUsage(
		ctx, workspace.ID, UsageMetricAIImageCredits, 1, true, now.Add(2*time.Minute))
	var limitErr *WorkspaceUsageLimitError
	if !errors.As(err, &limitErr) || limitErr.Limit != 27 || limitErr.Used != 37 || limitErr.Requested != 1 {
		t.Fatalf("enforced error = %#v (%v)", limitErr, err)
	}
	var stored int64
	if err := storage.db.QueryRowContext(ctx, `SELECT quantity FROM workspace_usage_monthly
WHERE workspace_id=$1 AND period_start=DATE '2026-07-01' AND metric=$2`,
		workspace.ID, UsageMetricAIImageCredits).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 37 {
		t.Fatalf("rejected charge changed quantity to %d", stored)
	}

	// Metrics and months are independent; a new month starts from zero.
	format, err := storage.ChargeWorkspaceMonthlyUsage(
		ctx, workspace.ID, UsageMetricAIFormatRequests, 4, true, now)
	if err != nil || format.Quantity != 4 {
		t.Fatalf("format charge = %#v, %v", format, err)
	}
	august, err := storage.ChargeWorkspaceMonthlyUsage(
		ctx, workspace.ID, UsageMetricAIImageCredits, 27, true, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || august.Quantity != 27 || !august.PeriodStart.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("August charge = %#v, %v", august, err)
	}
}

func TestInactiveWorkspaceSubscriptionRejectsUsageWithoutCharging(t *testing.T) {
	storage, err := Open(context.Background(), filepath.Join(t.TempDir(), "billing-inactive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	ctx := context.Background()
	if err := storage.UpsertUser(ctx, User{ID: "inactive-owner", DisplayName: "Owner"}); err != nil {
		t.Fatal(err)
	}
	workspace, err := storage.CreateWorkspace(ctx, "inactive-owner", Workspace{Name: "Inactive team"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	for _, status := range []string{"paused", "canceled"} {
		if err := storage.UpdateWorkspaceSubscriptionStatus(ctx, workspace.ID, status, now); err != nil {
			t.Fatalf("set %s status: %v", status, err)
		}
		for _, enforce := range []bool{false, true} {
			_, err := storage.ChargeWorkspaceMonthlyUsage(
				ctx, workspace.ID, UsageMetricAIImageCredits, 1, enforce, now)
			var inactive *WorkspacePlanInactiveError
			if !errors.As(err, &inactive) || inactive.Status != status {
				t.Fatalf("status=%s enforce=%v error=%#v (%v)", status, enforce, inactive, err)
			}
		}
	}

	var stored int64
	err = storage.db.QueryRowContext(ctx, `SELECT COALESCE(sum(quantity),0)
FROM workspace_usage_monthly WHERE workspace_id=$1`, workspace.ID).Scan(&stored)
	if err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatalf("inactive subscription charged %d usage units", stored)
	}
	if err := storage.UpdateWorkspaceSubscriptionStatus(ctx, workspace.ID, "active", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	usage, err := storage.ChargeWorkspaceMonthlyUsage(
		ctx, workspace.ID, UsageMetricAIImageCredits, 1, true, now.Add(time.Minute))
	if err != nil || usage.Quantity != 1 {
		t.Fatalf("reactivated charge = %#v, %v", usage, err)
	}
}

func entitlementByKey(t *testing.T, entitlements []BillingEntitlement, key string) BillingEntitlement {
	t.Helper()
	for _, entitlement := range entitlements {
		if entitlement.Key == key {
			return entitlement
		}
	}
	t.Fatalf("entitlement %s missing from %#v", key, entitlements)
	return BillingEntitlement{}
}
