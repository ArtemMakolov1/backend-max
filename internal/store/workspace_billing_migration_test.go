package store

import (
	"context"
	"testing"
)

func TestWorkspacePlansMigrationBackfillsSubscriptions(t *testing.T) {
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
	if _, err := db.ExecContext(ctx, string(migrations[planIndex].contents)); err != nil {
		t.Fatalf("apply billing migration: %v", err)
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

func TestYooKassaMigrationMovesOnlyCompliantFreeWorkspacesToV2(t *testing.T) {
	ctx := context.Background()
	testURL, db := newMigrationTestSchema(t)
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	billingIndex := -1
	for index, migration := range migrations {
		if migration.version == "021_yookassa_subscriptions.sql" {
			billingIndex = index
			break
		}
	}
	if billingIndex <= 0 {
		t.Fatal("021_yookassa_subscriptions.sql not found in embedded migrations")
	}
	if err := runMigrationSet(ctx, testURL, migrations[:billingIndex]); err != nil {
		t.Fatalf("apply prerequisite migrations: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO users(id,display_name,created_at,updated_at) VALUES
('migration-compliant','Compliant',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP),
('migration-overlimit','Over limit',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP),
('migration-extra-member','Extra member',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workspace_members(
workspace_id,user_id,role,created_by,joined_at,updated_at)
SELECT w.id,'migration-extra-member','viewer','migration-overlimit',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP
FROM workspaces w WHERE w.owner_user_id='migration-overlimit' AND w.is_personal=TRUE`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO channels(
owner_id,verified_max_owner_id,max_chat_id,title,active,workspace_id,created_at,updated_at)
SELECT owner_user_id,'migration-overlimit','migration-legacy-one','Legacy one',TRUE,id,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP
FROM workspaces WHERE owner_user_id='migration-overlimit' AND is_personal=TRUE
UNION ALL
SELECT owner_user_id,'migration-overlimit','migration-legacy-two','Legacy two',TRUE,id,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP
FROM workspaces WHERE owner_user_id='migration-overlimit' AND is_personal=TRUE`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workspace_media_usage(workspace_id,asset_count,total_bytes,updated_at)
SELECT id,2,2147483648,CURRENT_TIMESTAMP FROM workspaces
WHERE owner_user_id='migration-overlimit' AND is_personal=TRUE
ON CONFLICT(workspace_id) DO UPDATE SET asset_count=EXCLUDED.asset_count,total_bytes=EXCLUDED.total_bytes`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(migrations[billingIndex].contents)); err != nil {
		t.Fatalf("apply YooKassa migration: %v", err)
	}

	versions := make(map[string]int)
	rows, err := db.QueryContext(ctx, `SELECT w.owner_user_id,s.plan_version
FROM workspaces w JOIN workspace_subscriptions s ON s.workspace_id=w.id
WHERE w.owner_user_id IN ('migration-compliant','migration-overlimit') AND w.is_personal=TRUE`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var owner string
		var version int
		if err := rows.Scan(&owner, &version); err != nil {
			t.Fatal(err)
		}
		versions[owner] = version
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if versions["migration-compliant"] != 2 {
		t.Fatalf("compliant workspace version=%d, want 2", versions["migration-compliant"])
	}
	if versions["migration-overlimit"] != 1 {
		t.Fatalf("over-limit workspace version=%d, want legacy 1", versions["migration-overlimit"])
	}
	var legacyLimit, compliantLimit int64
	if err := db.QueryRowContext(ctx, `SELECT workspace_entitlement_limit(w.id,'channels')
FROM workspaces w WHERE w.owner_user_id='migration-overlimit' AND w.is_personal=TRUE`).Scan(&legacyLimit); err != nil {
		t.Fatal(err)
	}
	if legacyLimit != 1 {
		t.Fatalf("legacy channel growth limit=%d, want 1", legacyLimit)
	}
	if err := db.QueryRowContext(ctx, `SELECT workspace_entitlement_limit(w.id,'channels')
FROM workspaces w WHERE w.owner_user_id='migration-compliant' AND w.is_personal=TRUE`).Scan(&compliantLimit); err != nil {
		t.Fatal(err)
	}
	if compliantLimit != 1 {
		t.Fatalf("compliant channel limit=%d, want 1", compliantLimit)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO posts(
owner_id,title,content,status,workspace_id,created_at,updated_at)
SELECT owner_user_id,'Legacy draft','Still editable','draft',id,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP
FROM workspaces WHERE owner_user_id='migration-overlimit' AND is_personal=TRUE`); err != nil {
		t.Fatalf("legacy workspace could not continue editing posts: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE posts SET status='publishing',content='Publish still works'
WHERE owner_id='migration-overlimit'`); err != nil {
		t.Fatalf("legacy workspace could not continue publishing: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE channels SET active=FALSE
WHERE max_chat_id='migration-legacy-two'`); err != nil {
		t.Fatalf("legacy workspace could not reduce channels: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE channels SET active=TRUE
WHERE max_chat_id='migration-legacy-two'`); err == nil {
		t.Fatal("legacy workspace reactivated a channel beyond the nominal limit")
	}
	if _, err := db.ExecContext(ctx, `UPDATE workspace_media_usage SET total_bytes=1610612736
WHERE workspace_id=(SELECT id FROM workspaces WHERE owner_user_id='migration-overlimit' AND is_personal=TRUE)`); err != nil {
		t.Fatalf("legacy workspace could not reduce storage: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE workspace_media_usage SET total_bytes=1610612737
WHERE workspace_id=(SELECT id FROM workspaces WHERE owner_user_id='migration-overlimit' AND is_personal=TRUE)`); err == nil {
		t.Fatal("legacy workspace increased storage while still beyond the nominal limit")
	}

	if _, err := db.ExecContext(ctx, `INSERT INTO channels(
owner_id,verified_max_owner_id,max_chat_id,title,active,workspace_id,created_at,updated_at)
SELECT owner_user_id,'migration-compliant','migration-compliant-active','Active',TRUE,id,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP
FROM workspaces WHERE owner_user_id='migration-compliant' AND is_personal=TRUE`); err != nil {
		t.Fatalf("insert first compliant channel: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO channels(
owner_id,verified_max_owner_id,max_chat_id,title,active,workspace_id,created_at,updated_at)
SELECT owner_user_id,'migration-compliant','migration-compliant-inactive','Inactive',FALSE,id,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP
FROM workspaces WHERE owner_user_id='migration-compliant' AND is_personal=TRUE`); err != nil {
		t.Fatalf("insert inactive compliant channel: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE channels SET active=TRUE
WHERE max_chat_id='migration-compliant-inactive'`); err == nil {
		t.Fatal("reactivating a second channel bypassed the Free v2 limit")
	}

	if _, err := db.ExecContext(ctx, `INSERT INTO users(id,display_name,created_at,updated_at)
VALUES('migration-after-v2','New V2',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM workspace_members
WHERE workspace_id=(SELECT id FROM workspaces WHERE owner_user_id='migration-overlimit' AND is_personal=TRUE)
  AND user_id='migration-extra-member'`); err != nil {
		t.Fatalf("legacy workspace could not reduce seats: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workspace_members(
workspace_id,user_id,role,created_by,joined_at,updated_at)
SELECT id,'migration-after-v2','viewer','migration-overlimit',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP
FROM workspaces WHERE owner_user_id='migration-overlimit' AND is_personal=TRUE`); err == nil {
		t.Fatal("legacy workspace added a member beyond the nominal limit")
	}
	var newVersion int
	if err := db.QueryRowContext(ctx, `SELECT s.plan_version
FROM workspaces w JOIN workspace_subscriptions s ON s.workspace_id=w.id
WHERE w.owner_user_id='migration-after-v2' AND w.is_personal=TRUE`).Scan(&newVersion); err != nil {
		t.Fatal(err)
	}
	if newVersion != 2 {
		t.Fatalf("new workspace version=%d, want 2", newVersion)
	}
}

func TestRecurringTermsMigrationPreservesPriorConsentEvidence(t *testing.T) {
	ctx := context.Background()
	testURL, db := newMigrationTestSchema(t)
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	termsIndex := -1
	for index, migration := range migrations {
		if migration.version == "026_billing_recurring_terms_20260723.sql" {
			termsIndex = index
			break
		}
	}
	if termsIndex <= 0 {
		t.Fatal("026_billing_recurring_terms_20260723.sql not found in embedded migrations")
	}
	if err := runMigrationSet(ctx, testURL, migrations[:termsIndex]); err != nil {
		t.Fatalf("apply prerequisite migrations: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO users(id,display_name,created_at,updated_at)
VALUES('terms-migration-owner','Terms migration',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	var workspaceID string
	if err := db.QueryRowContext(ctx, `SELECT id FROM workspaces
WHERE owner_user_id='terms-migration-owner' AND is_personal=TRUE`).Scan(&workspaceID); err != nil {
		t.Fatal(err)
	}
	insertAttempt := func(id, idempotencyKey, planCode string, price int64) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `INSERT INTO billing_payment_attempts(
id,workspace_id,requested_by_user_id,purpose,idempotency_key,plan_code,plan_version,
amount_minor,currency_code,status,provider_description,provider_return_url,
create_deadline,next_attempt_at,created_at,updated_at)
VALUES($1,$2,'terms-migration-owner','checkout',$3,$4,2,$5,'RUB','failed',
'Terms migration checkout','https://maxposty.ru/app/',
CURRENT_TIMESTAMP+INTERVAL '1 day',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
			id, workspaceID, idempotencyKey, planCode, price); err != nil {
			t.Fatal(err)
		}
	}
	insertConsent := func(attemptID, termsVersion, planCode string, price int64) error {
		_, err := db.ExecContext(ctx, `INSERT INTO billing_recurring_consents(
payment_attempt_id,workspace_id,actor_user_id,consent_version,consent_text,terms_version,
terms_url,plan_code,plan_version,monthly_price_minor,currency_code,accepted_at)
VALUES($1,$2,'terms-migration-owner','yookassa-recurring-v2','Immutable consent',$3,
'https://maxposty.ru/terms/',$4,2,$5,'RUB',CURRENT_TIMESTAMP)`,
			attemptID, workspaceID, termsVersion, planCode, price)
		return err
	}

	oldAttemptID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	insertAttempt(oldAttemptID, "11111111-1111-1111-1111-111111111111", "solo", 99000)
	if err := insertConsent(oldAttemptID, "2026-07-22", "solo", 99000); err != nil {
		t.Fatalf("insert prior consent: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(migrations[termsIndex].contents)); err != nil {
		t.Fatalf("apply recurring terms migration: %v", err)
	}
	var preservedVersion, effectivePreservedVersion string
	var acceptedVersionIsNull bool
	if err := db.QueryRowContext(ctx, `SELECT terms_version,accepted_terms_version IS NULL,
COALESCE(accepted_terms_version,terms_version) FROM billing_recurring_consents
WHERE payment_attempt_id=$1`, oldAttemptID).Scan(
		&preservedVersion, &acceptedVersionIsNull, &effectivePreservedVersion,
	); err != nil {
		t.Fatal(err)
	}
	if preservedVersion != "2026-07-22" || !acceptedVersionIsNull || effectivePreservedVersion != "2026-07-22" {
		t.Fatalf("prior consent legacy=%q accepted_is_null=%t effective=%q, want 2026-07-22/true/2026-07-22",
			preservedVersion, acceptedVersionIsNull, effectivePreservedVersion)
	}
	if _, err := db.ExecContext(ctx, `UPDATE billing_recurring_consents
SET accepted_terms_version='2026-07-23' WHERE payment_attempt_id=$1`, oldAttemptID); err == nil {
		t.Fatal("prior immutable consent accepted an effective terms version rewrite")
	}
	currentAttemptID := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	insertAttempt(currentAttemptID, "22222222-2222-2222-2222-222222222222", "pro", 249000)
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_recurring_consents(
payment_attempt_id,workspace_id,actor_user_id,consent_version,consent_text,terms_version,
accepted_terms_version,terms_url,plan_code,plan_version,monthly_price_minor,currency_code,accepted_at)
VALUES($1,$2,'terms-migration-owner','yookassa-recurring-v2','Immutable consent','2026-07-22',
'2026-07-23','https://maxposty.ru/terms/','pro',2,249000,'RUB',CURRENT_TIMESTAMP)`,
		currentAttemptID, workspaceID); err != nil {
		t.Fatalf("insert current consent: %v", err)
	}
	var currentLegacyVersion, currentAcceptedVersion, effectiveCurrentVersion string
	if err := db.QueryRowContext(ctx, `SELECT terms_version,accepted_terms_version,
COALESCE(accepted_terms_version,terms_version) FROM billing_recurring_consents
WHERE payment_attempt_id=$1`, currentAttemptID).Scan(
		&currentLegacyVersion, &currentAcceptedVersion, &effectiveCurrentVersion,
	); err != nil {
		t.Fatal(err)
	}
	if currentLegacyVersion != "2026-07-22" || currentAcceptedVersion != "2026-07-23" ||
		effectiveCurrentVersion != "2026-07-23" {
		t.Fatalf("current consent legacy=%q accepted=%q effective=%q, want 2026-07-22/2026-07-23/2026-07-23",
			currentLegacyVersion, currentAcceptedVersion, effectiveCurrentVersion)
	}
	invalidAttemptID := "cccccccccccccccccccccccccccccccc"
	insertAttempt(invalidAttemptID, "33333333-3333-3333-3333-333333333333", "solo", 99000)
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_recurring_consents(
payment_attempt_id,workspace_id,actor_user_id,consent_version,consent_text,terms_version,
accepted_terms_version,terms_url,plan_code,plan_version,monthly_price_minor,currency_code,accepted_at)
VALUES($1,$2,'terms-migration-owner','yookassa-recurring-v2','Immutable consent','2026-07-22',
'2026-07-24','https://maxposty.ru/terms/','solo',2,99000,'RUB',CURRENT_TIMESTAMP)`,
		invalidAttemptID, workspaceID); err == nil {
		t.Fatal("future unknown recurring terms version passed the migrated CHECK")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_recurring_consents(
payment_attempt_id,workspace_id,actor_user_id,consent_version,consent_text,terms_version,
accepted_terms_version,terms_url,plan_code,plan_version,monthly_price_minor,currency_code,accepted_at)
VALUES($1,$2,'terms-migration-owner','yookassa-recurring-v2','Immutable consent','2026-07-23',
'2026-07-23','https://maxposty.ru/terms/','solo',2,99000,'RUB',CURRENT_TIMESTAMP)`,
		invalidAttemptID, workspaceID); err == nil {
		t.Fatal("legacy terms_version CHECK was widened by the additive migration")
	}
	var nullable string
	if err := db.QueryRowContext(ctx, `SELECT is_nullable FROM information_schema.columns
WHERE table_schema=current_schema() AND table_name='billing_recurring_consents'
  AND column_name='accepted_terms_version'`).Scan(&nullable); err != nil {
		t.Fatal(err)
	}
	if nullable != "YES" {
		t.Fatalf("accepted_terms_version nullable=%q, want YES for prior immutable evidence", nullable)
	}
	var legacyValidated, acceptedValidated bool
	if err := db.QueryRowContext(ctx, `SELECT
COALESCE(bool_or(conname='billing_recurring_consents_terms_version_check' AND convalidated),FALSE),
COALESCE(bool_or(conname='billing_recurring_consents_accepted_terms_version_check' AND convalidated),FALSE)
FROM pg_constraint
WHERE conrelid='billing_recurring_consents'::regclass`).Scan(
		&legacyValidated, &acceptedValidated,
	); err != nil {
		t.Fatal(err)
	}
	if !legacyValidated || !acceptedValidated {
		t.Fatalf("recurring terms checks validated: legacy=%t accepted=%t, want true/true",
			legacyValidated, acceptedValidated)
	}
}
