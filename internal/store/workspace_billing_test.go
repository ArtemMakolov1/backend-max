package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maxpilot/backend/internal/legal"
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
	if len(public) != 3 || public[0].Plan.Code != "free" || public[0].Plan.Version != 2 ||
		public[0].Plan.MonthlyPriceMinor != 0 || !public[0].Plan.Public ||
		!public[0].Plan.Available || len(public[0].Entitlements) != 6 {
		t.Fatalf("public catalog = %#v", public)
	}
	image := entitlementByKey(t, public[0].Entitlements, "ai_images_monthly")
	if image.Limit != 0 || image.LimitBaseUnits != 0 || image.UnitScale != 9 ||
		image.UsageMetric != UsageMetricAIImageCredits || !image.HardLimit {
		t.Fatalf("free image entitlement = %#v", image)
	}
	if channels := entitlementByKey(t, public[0].Entitlements, "channels"); !channels.HardLimit {
		t.Fatalf("channel limit must be enforced after paid launch: %#v", channels)
	}
	format := entitlementByKey(t, public[0].Entitlements, "ai_format_monthly")
	if format.Limit != 0 || format.UsageMetric != UsageMetricAIFormatRequests {
		t.Fatalf("free format entitlement = %#v", format)
	}

	wantPrices := map[string]int64{"free": 0, "solo": 99000, "pro": 249000}
	for _, entry := range public {
		if entry.Plan.Code == "free" {
			if entry.RecurringConsentText != "" || entry.RecurringConsentVersion != "" {
				t.Fatalf("Free unexpectedly has recurring consent: %#v", entry)
			}
			continue
		}
		if entry.RecurringConsentVersion != BillingRecurringConsentVersion ||
			entry.RecurringConsentTermsVersion != BillingRecurringTermsVersion ||
			entry.RecurringConsentTermsURL != BillingRecurringTermsURL ||
			!strings.Contains(entry.RecurringConsentText, entry.Plan.Name) ||
			!strings.Contains(entry.RecurringConsentText, "до отмены подписки") {
			t.Fatalf("paid consent is not authoritative: %#v", entry)
		}
	}
	rows, err := storage.db.QueryContext(ctx, `SELECT plan_code,monthly_price_minor,public,available
FROM billing_plan_versions WHERE public=TRUE AND available=TRUE ORDER BY monthly_price_minor`)
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
		if !isPublic || !available {
			t.Fatalf("public plan %s is public=%v available=%v", code, isPublic, available)
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
	state, err := storage.GetWorkspaceBillingState(ctx, "billing-owner", workspace.ID,
		time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if state.Subscription.Plan.Code != "free" || state.Subscription.Plan.Version != 2 ||
		state.Subscription.Status != "active" || len(state.Usage) != 6 || state.Features.AIImages {
		t.Fatalf("workspace billing state = %#v", state)
	}
	checkoutSnapshot := BillingCheckoutSnapshot{
		PlanCode: "solo", PlanVersion: 2, MonthlyPriceMinor: 99000, CurrencyCode: "RUB",
		RecurringConsent: true, RecurringConsentVersion: BillingRecurringConsentVersion,
		RecurringConsentTermsVersion: BillingRecurringTermsVersion,
	}
	if _, err := storage.CreateBillingCheckoutAttempt(
		ctx, "billing-owner", workspace.ID, checkoutSnapshot,
		"https://maxposty.ru/app/?billing=pending#/workspace/settings/plan", time.Now().UTC(),
	); !errors.Is(err, ErrBillingLegalConsentRequired) {
		t.Fatalf("checkout without current legal consent error=%v, want ErrBillingLegalConsentRequired", err)
	}
	if _, err := storage.db.ExecContext(ctx, `INSERT INTO user_consents(
owner_id,document,version,accepted_at,source) VALUES
	($1,'terms',$2,CURRENT_TIMESTAMP,'test'),
	($1,'personal_data',$3,CURRENT_TIMESTAMP,'test')`,
		"billing-owner", legal.CurrentTermsVersion, legal.CurrentPersonalDataVersion); err != nil {
		t.Fatal(err)
	}
	staleSnapshot := checkoutSnapshot
	staleSnapshot.MonthlyPriceMinor--
	if _, err := storage.CreateBillingCheckoutAttempt(
		ctx, "billing-owner", workspace.ID, staleSnapshot,
		"https://maxposty.ru/app/?billing=pending#/workspace/settings/plan", time.Now().UTC(),
	); !errors.Is(err, ErrBillingCheckoutSnapshotMismatch) {
		t.Fatalf("stale checkout snapshot error=%v, want ErrBillingCheckoutSnapshotMismatch", err)
	}
	attempt, err := storage.CreateBillingCheckoutAttempt(
		ctx, "billing-owner", workspace.ID, checkoutSnapshot,
		"https://maxposty.ru/app/?billing=pending#/workspace/settings/plan", time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	var storedVersion, storedText, storedTermsVersion, storedTermsURL string
	if err := storage.db.QueryRowContext(ctx, `SELECT consent_version,consent_text,terms_version,terms_url
FROM billing_recurring_consents WHERE payment_attempt_id=$1`, attempt.ID).Scan(
		&storedVersion, &storedText, &storedTermsVersion, &storedTermsURL,
	); err != nil {
		t.Fatal(err)
	}
	var soloConsent string
	for _, entry := range public {
		if entry.Plan.Code == "solo" {
			soloConsent = entry.RecurringConsentText
		}
	}
	if storedVersion != BillingRecurringConsentVersion || storedText != soloConsent ||
		storedTermsVersion != BillingRecurringTermsVersion || storedTermsURL != BillingRecurringTermsURL {
		t.Fatalf("stored consent does not match catalog: version=%q text=%q terms=%q url=%q",
			storedVersion, storedText, storedTermsVersion, storedTermsURL)
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
	periodID := seedBillingContract(
		t, storage, workspace.ID, "solo",
		time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC), "sealed-usage-method",
	)

	// Observe mode records usage even after the paid allowance is exhausted.
	usage, err := storage.ChargeWorkspaceMonthlyUsage(
		ctx, workspace.ID, UsageMetricAIImageCredits, 109, false, now)
	if err != nil || usage.Quantity != 109 {
		t.Fatalf("observe charge = %#v, %v", usage, err)
	}
	usage, err = storage.ChargeWorkspaceMonthlyUsage(
		ctx, workspace.ID, UsageMetricAIImageCredits, 1, false, now.Add(time.Minute))
	if err != nil || usage.Quantity != 110 {
		t.Fatalf("second observe charge = %#v, %v", usage, err)
	}

	_, err = storage.ChargeWorkspaceMonthlyUsage(
		ctx, workspace.ID, UsageMetricAIImageCredits, 1, true, now.Add(2*time.Minute))
	var limitErr *WorkspaceUsageLimitError
	if !errors.As(err, &limitErr) || limitErr.Limit != 108 || limitErr.Used != 110 || limitErr.Requested != 1 {
		t.Fatalf("enforced error = %#v (%v)", limitErr, err)
	}
	var stored int64
	if err := storage.db.QueryRowContext(ctx, `SELECT quantity FROM workspace_usage_periods
WHERE subscription_period_id=$1 AND workspace_id=$2 AND metric=$3`,
		periodID, workspace.ID, UsageMetricAIImageCredits).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 110 {
		t.Fatalf("rejected charge changed quantity to %d", stored)
	}

	// Metrics are independent, while paid usage remains in the immutable
	// provider period even when that period crosses a calendar-month boundary.
	format, err := storage.ChargeWorkspaceMonthlyUsage(
		ctx, workspace.ID, UsageMetricAIFormatRequests, 4, true, now)
	if err != nil || format.Quantity != 4 {
		t.Fatalf("format charge = %#v, %v", format, err)
	}
	_, err = storage.ChargeWorkspaceMonthlyUsage(
		ctx, workspace.ID, UsageMetricAIImageCredits, 1, true, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	if !errors.As(err, &limitErr) {
		t.Fatalf("calendar rollover reset a paid-period quota: %v", err)
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
	seedBillingContract(t, storage, workspace.ID, "solo", now.AddDate(0, -1, 0), now.AddDate(0, 1, 0), "sealed-inactive-method")

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
FROM workspace_usage_periods WHERE workspace_id=$1`, workspace.ID).Scan(&stored)
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

func TestFreePlanAlwaysRejectsAIFeaturesEvenInObserveMode(t *testing.T) {
	storage, err := Open(context.Background(), filepath.Join(t.TempDir(), "billing-free-ai.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	ctx := context.Background()
	if err := storage.UpsertUser(ctx, User{ID: "free-owner", DisplayName: "Owner"}); err != nil {
		t.Fatal(err)
	}
	workspace, err := storage.CreateWorkspace(ctx, "free-owner", Workspace{Name: "Free team"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = storage.ChargeWorkspaceMonthlyUsage(
		ctx, workspace.ID, UsageMetricAIFormatRequests, 1, false, time.Now().UTC())
	var upgrade *WorkspacePlanUpgradeRequiredError
	if !errors.As(err, &upgrade) || upgrade.Feature != "ai_format" {
		t.Fatalf("free AI error=%#v (%v)", upgrade, err)
	}
}

func TestFreePlanNeverAdvertisesLegacyAIEntitlementsAsFeatures(t *testing.T) {
	features := billingFeatures("free", []BillingEntitlement{
		{UsageMetric: UsageMetricAIImageCredits, LimitBaseUnits: 27},
		{UsageMetric: UsageMetricAIResearchRequests, LimitBaseUnits: 3},
		{UsageMetric: UsageMetricAIFormatRequests, LimitBaseUnits: 10},
	})
	if features.AIImages || features.AIResearch || features.AIFormat ||
		features.AIChannelDescription || features.AIBrandKit {
		t.Fatalf("legacy Free advertised premium AI features: %#v", features)
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
