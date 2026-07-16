package store

import (
	"context"
	"testing"
)

func TestWorkspacePlansMigrationIsIdempotentAndBackfillsSubscriptions(t *testing.T) {
	ctx := context.Background()
	testURL, db := newMigrationTestSchema(t)
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	planIndex := -1
	for index, migration := range migrations {
		if migration.version == "019_workspace_plans.sql" {
			planIndex = index
			break
		}
	}
	if planIndex <= 0 {
		t.Fatal("019_workspace_plans.sql not found in embedded migrations")
	}
	if err := runMigrationSet(ctx, testURL, migrations[:planIndex]); err != nil {
		t.Fatalf("apply prerequisite migrations: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO users(id,display_name,created_at,updated_at)
VALUES('migration-existing','Existing',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := db.ExecContext(ctx, string(migrations[planIndex].contents)); err != nil {
			t.Fatalf("apply billing migration attempt %d: %v", attempt, err)
		}
	}

	var plans, entitlements, subscriptions int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_plan_versions`).Scan(&plans); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_plan_entitlements`).Scan(&entitlements); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM workspace_subscriptions`).Scan(&subscriptions); err != nil {
		t.Fatal(err)
	}
	if plans != 4 || entitlements != 24 || subscriptions == 0 {
		t.Fatalf("idempotent catalog counts plans=%d entitlements=%d subscriptions=%d", plans, entitlements, subscriptions)
	}
	if _, err := db.ExecContext(ctx, `UPDATE billing_plan_versions
SET monthly_price_minor=1 WHERE plan_code='free' AND version=1`); err == nil {
		t.Fatal("immutable plan price was updated in place")
	}
	if _, err := db.ExecContext(ctx, `UPDATE billing_plan_entitlements
SET limit_value=999 WHERE plan_code='free' AND plan_version=1 AND entitlement_key='channels'`); err == nil {
		t.Fatal("immutable entitlement was updated in place")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM billing_plan_entitlements
WHERE plan_code='agency' AND plan_version=1 AND entitlement_key='channels'`); err == nil {
		t.Fatal("immutable entitlement was deleted")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_plan_entitlements(
plan_code,plan_version,entitlement_key,usage_metric,limit_value,unit,period,unit_scale,hard_limit)
VALUES('free',1,'late_feature','late_feature',1,'request','month',1,TRUE)`); err == nil {
		t.Fatal("new entitlement was added to an already subscribed plan version")
	}
	if _, err := db.ExecContext(ctx, `UPDATE billing_plan_versions
SET public=FALSE,available=FALSE WHERE plan_code='free' AND version=1`); err != nil {
		t.Fatalf("rollout flags should remain mutable: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE billing_plan_versions
SET public=TRUE,available=TRUE WHERE plan_code='free' AND version=1`); err != nil {
		t.Fatalf("restore rollout flags: %v", err)
	}

	var existingPlan string
	if err := db.QueryRowContext(ctx, `SELECT s.plan_code
FROM workspace_subscriptions s
JOIN workspaces w ON w.id=s.workspace_id
WHERE w.owner_user_id='migration-existing' AND w.is_personal=TRUE`).Scan(&existingPlan); err != nil {
		t.Fatal(err)
	}
	if existingPlan != "free" {
		t.Fatalf("backfilled plan=%q, want free", existingPlan)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO users(id,display_name,created_at,updated_at)
VALUES('migration-new','New',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	var newPlan string
	if err := db.QueryRowContext(ctx, `SELECT s.plan_code
FROM workspace_subscriptions s
JOIN workspaces w ON w.id=s.workspace_id
WHERE w.owner_user_id='migration-new' AND w.is_personal=TRUE`).Scan(&newPlan); err != nil {
		t.Fatal(err)
	}
	if newPlan != "free" {
		t.Fatalf("triggered plan=%q, want free", newPlan)
	}
}
