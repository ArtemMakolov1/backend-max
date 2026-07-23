package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMigrateStoresSHA256AndRejectsChangedAppliedSQL(t *testing.T) {
	ctx := context.Background()
	testURL, db := newMigrationTestSchema(t)
	if err := Migrate(ctx, testURL); err != nil {
		t.Fatalf("initial migration: %v", err)
	}
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		var stored string
		if err := db.QueryRowContext(ctx,
			`SELECT checksum_sha256 FROM schema_migrations WHERE version = $1`, migration.version).Scan(&stored); err != nil {
			t.Fatalf("read checksum for %s: %v", migration.version, err)
		}
		if stored != migration.checksumSHA256 || len(stored) != sha256.Size*2 {
			t.Fatalf("checksum for %s = %q, want %q", migration.version, stored, migration.checksumSHA256)
		}
	}

	changed := append([]schemaMigration(nil), migrations...)
	changed[0].contents = append(append([]byte(nil), changed[0].contents...), []byte("\n-- changed after apply\n")...)
	changed[0].checksumSHA256 = migrationChecksumSHA256(changed[0].contents)
	err = runMigrationSet(ctx, testURL, changed)
	if !errors.Is(err, ErrMigrationIntegrity) || !strings.Contains(err.Error(), changed[0].version) {
		t.Fatalf("changed applied migration error = %v, want ErrMigrationIntegrity for %s", err, changed[0].version)
	}
}

func TestSanitizePublicationErrorsMigrationRemovesProviderDetails(t *testing.T) {
	ctx := context.Background()
	testURL, db := newMigrationTestSchema(t)
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) == 0 {
		t.Fatal("no embedded migrations loaded")
	}
	sanitizeIndex := -1
	for index, migration := range migrations {
		if migration.version == "012_sanitize_publication_errors.sql" {
			sanitizeIndex = index
			break
		}
	}
	if sanitizeIndex <= 0 {
		t.Fatalf("012_sanitize_publication_errors.sql not found in embedded migrations")
	}
	if err := runMigrationSet(ctx, testURL, migrations[:sanitizeIndex]); err != nil {
		t.Fatalf("apply prerequisite migrations: %v", err)
	}

	now := time.Now().UTC()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users(id, display_name, created_at, updated_at) VALUES ('owner', 'Owner', $1, $1)`, now); err != nil {
		t.Fatal(err)
	}
	legacyErrors := []string{
		"MAX API error (status 400, code proto.payload): errors.send-message.channel-notify",
		"Previous publication was interrupted; check the MAX channel before retrying to avoid a duplicate post.",
	}
	for _, message := range legacyErrors {
		if _, err := db.ExecContext(ctx, `
INSERT INTO posts(owner_id, title, content, last_error, created_at, updated_at)
VALUES ('owner', 'Legacy post', 'Body', $1, $2, $2)`, message, now); err != nil {
			t.Fatal(err)
		}
	}

	if err := runMigrationSet(ctx, testURL, migrations[:sanitizeIndex+1]); err != nil {
		t.Fatalf("apply sanitizing migration: %v", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT last_error FROM posts ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	count := 0
	for rows.Next() {
		count++
		var message string
		if err := rows.Scan(&message); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(message, "MAX API error") || strings.Contains(message, "proto.payload") || strings.Contains(message, "errors.send-message") || strings.Contains(message, "Previous publication") {
			t.Fatalf("migration kept provider or protocol details in %q", message)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != len(legacyErrors) {
		t.Fatalf("sanitized rows = %d, want %d", count, len(legacyErrors))
	}
}

func TestS3MediaCutoverMigrationClearsLegacyLinksAndPreservesPost(t *testing.T) {
	ctx := context.Background()
	testURL, db := newMigrationTestSchema(t)
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	cutoverIndex := -1
	for index, migration := range migrations {
		if migration.version == "013_s3_media_cutover.sql" {
			cutoverIndex = index
			break
		}
	}
	if cutoverIndex <= 0 {
		t.Fatal("013_s3_media_cutover.sql not found in embedded migrations")
	}
	if err := runMigrationSet(ctx, testURL, migrations[:cutoverIndex]); err != nil {
		t.Fatalf("apply prerequisite migrations: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users(id, display_name, created_at, updated_at) VALUES ('owner', 'Owner', $1, $1)`, now); err != nil {
		t.Fatal(err)
	}
	var postID int64
	if err := db.QueryRowContext(ctx, `
INSERT INTO posts(
	owner_id, title, content, format, status,
	image_url, image_path, image_prompt,
	notify, disable_link_preview, link_buttons,
	max_message_id, max_message_url, max_views, max_is_pinned,
	created_at, updated_at
)
VALUES (
	'owner', 'Preserved title', 'Preserved body', 'html', 'published',
	'/media/legacy.png', 'legacy/owner/image.png', 'Preserved image prompt',
	TRUE, TRUE, '[{"text":"Open","url":"https://example.com"}]'::jsonb,
	'max-message-id', 'https://max.ru/channel/message', 42, TRUE,
	$1, $1
)
RETURNING id`, now).Scan(&postID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO media_assets(owner_id, filename, created_at)
VALUES ('owner', 'legacy.png', $1)`, now); err != nil {
		t.Fatal(err)
	}

	if err := runMigrationSet(ctx, testURL, migrations[:cutoverIndex+1]); err != nil {
		t.Fatalf("apply S3 media cutover migration: %v", err)
	}

	var (
		ownerID, title, content, format, status string
		imageURL, imagePath, imagePrompt        string
		maxMessageID, maxMessageURL             string
		notify, disableLinkPreview              bool
		linkButtonsPreserved, maxIsPinned       bool
		maxViews                                int64
		createdAt, updatedAt                    time.Time
	)
	if err := db.QueryRowContext(ctx, `
SELECT owner_id, title, content, format, status,
       image_url, image_path, image_prompt,
       notify, disable_link_preview,
       link_buttons = '[{"text":"Open","url":"https://example.com"}]'::jsonb,
       max_message_id, max_message_url, max_views, max_is_pinned,
       created_at, updated_at
FROM posts
WHERE id = $1`, postID).Scan(
		&ownerID, &title, &content, &format, &status,
		&imageURL, &imagePath, &imagePrompt,
		&notify, &disableLinkPreview, &linkButtonsPreserved,
		&maxMessageID, &maxMessageURL, &maxViews, &maxIsPinned,
		&createdAt, &updatedAt,
	); err != nil {
		t.Fatal(err)
	}
	if imageURL != "" || imagePath != "" {
		t.Fatalf("legacy image references after cutover: image_url=%q image_path=%q", imageURL, imagePath)
	}
	if ownerID != "owner" || title != "Preserved title" || content != "Preserved body" ||
		format != "html" || status != "published" || imagePrompt != "Preserved image prompt" ||
		!notify || !disableLinkPreview || !linkButtonsPreserved ||
		maxMessageID != "max-message-id" || maxMessageURL != "https://max.ru/channel/message" ||
		maxViews != 42 || !maxIsPinned || !createdAt.Equal(now) || !updatedAt.Equal(now) {
		t.Fatalf("post data changed during cutover: owner=%q title=%q content=%q format=%q status=%q prompt=%q notify=%v disable_preview=%v buttons_preserved=%v max_id=%q max_url=%q views=%d pinned=%v created_at=%v updated_at=%v",
			ownerID, title, content, format, status, imagePrompt, notify, disableLinkPreview, linkButtonsPreserved,
			maxMessageID, maxMessageURL, maxViews, maxIsPinned, createdAt, updatedAt)
	}

	var mediaAssetCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM media_assets WHERE owner_id = 'owner'`).Scan(&mediaAssetCount); err != nil {
		t.Fatal(err)
	}
	if mediaAssetCount != 1 {
		t.Fatalf("legacy media assets for rollback compatibility = %d, want 1", mediaAssetCount)
	}
}

func TestMediaQuotaMigrationQuarantinesLegacyOwnershipUntilCleanup(t *testing.T) {
	ctx := context.Background()
	testURL, db := newMigrationTestSchema(t)
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	quotaIndex := -1
	for index, migration := range migrations {
		if migration.version == "014_media_quota_and_gc.sql" {
			quotaIndex = index
			break
		}
	}
	if quotaIndex <= 0 {
		t.Fatal("014_media_quota_and_gc.sql not found in embedded migrations")
	}
	if err := runMigrationSet(ctx, testURL, migrations[:quotaIndex]); err != nil {
		t.Fatalf("apply prerequisite migrations: %v", err)
	}

	now := time.Now().UTC()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users(id, display_name, created_at, updated_at) VALUES ('owner', 'Owner', $1, $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO media_assets(owner_id, filename, created_at) VALUES ('owner', 'legacy-local.png', $1)`, now); err != nil {
		t.Fatal(err)
	}
	if err := runMigrationSet(ctx, testURL, migrations[:quotaIndex+1]); err != nil {
		t.Fatalf("apply media quota migration: %v", err)
	}

	var assets int
	var sizeBytes int64
	var state, reservationToken string
	var updatedAt time.Time
	if err := db.QueryRowContext(ctx, `SELECT count(*), max(size_bytes), max(state), max(reservation_token), max(updated_at) FROM media_assets`).Scan(
		&assets, &sizeBytes, &state, &reservationToken, &updatedAt,
	); err != nil {
		t.Fatal(err)
	}
	if assets != 1 || sizeBytes != 0 || state != "pending" || reservationToken != "legacy-local-cutover" || !updatedAt.Before(time.Unix(1, 0).UTC()) {
		t.Fatalf("quarantined legacy media = (%d, %d, %q, %q, %v), want stale pending cutover reservation",
			assets, sizeBytes, state, reservationToken, updatedAt)
	}
	// Runtime orphan cleanup follows the current schema and checks both the
	// legacy posts.image_path projection and the normalized attachment rows.
	// Finish the migration set after asserting the isolated 014 cutover state.
	if err := runMigrationSet(ctx, testURL, migrations); err != nil {
		t.Fatalf("apply current attachment schema: %v", err)
	}
	storage := &Store{db: &postgresDB{DB: db}}
	limits := MediaLimits{MaxFiles: 10, MaxBytes: 1 << 20}
	if _, err := storage.ReserveMedia(ctx, "owner", "legacy-local.png", 123, limits, now); !errors.Is(err, ErrMediaUploadBusy) {
		t.Fatalf("reserve quarantined legacy media error = %v, want ErrMediaUploadBusy", err)
	}
	cleanup, err := storage.CleanupOrphanMedia(ctx, now, 10, func(context.Context, string) error { return nil })
	if err != nil {
		t.Fatalf("clean quarantined legacy media: %v", err)
	}
	if cleanup.AssetsRemoved != 1 || cleanup.ObjectsDeleted != 1 {
		t.Fatalf("legacy cleanup = %+v, want one ownership and object removal", cleanup)
	}
	reservation, err := storage.ReserveMedia(ctx, "owner", "legacy-local.png", 123, limits, now.Add(time.Second))
	if err != nil {
		t.Fatalf("reserve cleaned filename with quota: %v", err)
	}
	if reservation.Existing {
		t.Fatal("cleaned legacy filename bypassed quota as an existing object")
	}
	if err := storage.CompleteMediaReservation(ctx, reservation, now.Add(2*time.Second)); err != nil {
		t.Fatalf("complete replacement media: %v", err)
	}
	var usedFiles, usedBytes int64
	if err := db.QueryRowContext(ctx, `SELECT asset_count, total_bytes FROM media_usage WHERE owner_id='owner'`).Scan(&usedFiles, &usedBytes); err != nil {
		t.Fatalf("read replacement quota: %v", err)
	}
	if usedFiles != 1 || usedBytes != 123 {
		t.Fatalf("replacement quota = (%d files, %d bytes), want (1, 123)", usedFiles, usedBytes)
	}
}

func TestMigrationIntegrityFailsClosedAtRuntimeAndMigrator(t *testing.T) {
	ctx := context.Background()
	testURL, db := newMigrationTestSchema(t)
	if err := Migrate(ctx, testURL); err != nil {
		t.Fatalf("initial migration: %v", err)
	}
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	corruptedChecksum := strings.Repeat("0", sha256.Size*2)
	if corruptedChecksum == migrations[0].checksumSHA256 {
		corruptedChecksum = strings.Repeat("f", sha256.Size*2)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE schema_migrations SET checksum_sha256 = $1 WHERE version = $2`, corruptedChecksum, migrations[0].version); err != nil {
		t.Fatalf("corrupt stored checksum: %v", err)
	}

	if err := Migrate(ctx, testURL); !errors.Is(err, ErrMigrationIntegrity) {
		t.Fatalf("Migrate() error = %v, want ErrMigrationIntegrity", err)
	}
	runtimeStore, err := OpenRuntime(ctx, testURL)
	if runtimeStore != nil {
		_ = runtimeStore.Close()
		t.Fatal("OpenRuntime accepted a corrupted applied-migration checksum")
	}
	if !errors.Is(err, ErrMigrationIntegrity) {
		t.Fatalf("OpenRuntime() error = %v, want ErrMigrationIntegrity", err)
	}
}

func TestOpenRuntimeAllowsOnlyNewerUnknownMigrations(t *testing.T) {
	ctx := context.Background()
	testURL, db := newMigrationTestSchema(t)
	if err := Migrate(ctx, testURL); err != nil {
		t.Fatalf("initial migration: %v", err)
	}
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	// Runtime compatibility is based on lexicographic migration versions.
	// Derive the unknown version from the current maximum so this fixture stays
	// newer when a previously "future" numbered migration becomes real.
	futureVersion := strings.TrimSuffix(migrations[len(migrations)-1].version, ".sql") +
		"_future.sql"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, checksum_sha256) VALUES ($1, $2)`,
		futureVersion, strings.Repeat("a", sha256.Size*2)); err != nil {
		t.Fatalf("record future migration: %v", err)
	}

	runtimeStore, err := OpenRuntime(ctx, testURL)
	if err != nil {
		t.Fatalf("OpenRuntime rejected newer additive migration: %v", err)
	}
	if err := runtimeStore.Close(); err != nil {
		t.Fatalf("close runtime store: %v", err)
	}
	if err := Migrate(ctx, testURL); !errors.Is(err, ErrMigrationIntegrity) {
		t.Fatalf("Migrate() with unknown future migration error = %v, want ErrMigrationIntegrity", err)
	}
}

func TestOpenRuntimeRejectsUnknownMigrationAtOrBelowKnownMaximum(t *testing.T) {
	for _, version := range []string{"000_older.sql", "001_z_gap.sql"} {
		t.Run(version, func(t *testing.T) {
			ctx := context.Background()
			testURL, db := newMigrationTestSchema(t)
			if err := Migrate(ctx, testURL); err != nil {
				t.Fatalf("initial migration: %v", err)
			}
			if _, err := db.ExecContext(ctx,
				`INSERT INTO schema_migrations(version, checksum_sha256) VALUES ($1, $2)`,
				version, strings.Repeat("a", sha256.Size*2)); err != nil {
				t.Fatalf("record unknown migration: %v", err)
			}

			runtimeStore, err := OpenRuntime(ctx, testURL)
			if runtimeStore != nil {
				_ = runtimeStore.Close()
				t.Fatalf("OpenRuntime accepted unknown migration %q", version)
			}
			if !errors.Is(err, ErrMigrationIntegrity) || !strings.Contains(err.Error(), version) {
				t.Fatalf("OpenRuntime() error = %v, want ErrMigrationIntegrity for %s", err, version)
			}
		})
	}
}

func TestOpenRuntimeRejectsMissingKnownMigration(t *testing.T) {
	ctx := context.Background()
	testURL, db := newMigrationTestSchema(t)
	if err := Migrate(ctx, testURL); err != nil {
		t.Fatalf("initial migration: %v", err)
	}
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	missing := migrations[len(migrations)-1].version
	if _, err := db.ExecContext(ctx,
		`DELETE FROM schema_migrations WHERE version = $1`, missing); err != nil {
		t.Fatalf("remove known migration record: %v", err)
	}

	runtimeStore, err := OpenRuntime(ctx, testURL)
	if runtimeStore != nil {
		_ = runtimeStore.Close()
		t.Fatalf("OpenRuntime accepted missing migration %q", missing)
	}
	if err == nil || !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "not applied") {
		t.Fatalf("OpenRuntime() error = %v, want missing-schema error for %s", err, missing)
	}
}

func TestMigrateBackfillsLegacyTableAtomically(t *testing.T) {
	ctx := context.Background()
	testURL, db := newMigrationTestSchema(t)
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `
CREATE TABLE schema_migrations (
	version TEXT PRIMARY KEY,
	applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if _, err := tx.ExecContext(ctx, string(migration.contents)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("apply legacy migration %s: %v", migration.version, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version) VALUES ($1)`, migration.version); err != nil {
			_ = tx.Rollback()
			t.Fatalf("record legacy migration %s: %v", migration.version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := Migrate(ctx, testURL); err != nil {
		t.Fatalf("upgrade legacy migration metadata: %v", err)
	}
	for _, migration := range migrations {
		var stored string
		if err := db.QueryRowContext(ctx,
			`SELECT checksum_sha256 FROM schema_migrations WHERE version = $1`, migration.version).Scan(&stored); err != nil {
			t.Fatalf("read backfilled checksum for %s: %v", migration.version, err)
		}
		if stored != migration.checksumSHA256 {
			t.Fatalf("backfilled checksum for %s = %q, want %q", migration.version, stored, migration.checksumSHA256)
		}
	}
	var nullable string
	if err := db.QueryRowContext(ctx, `
SELECT is_nullable
FROM information_schema.columns
WHERE table_schema = current_schema()
  AND table_name = 'schema_migrations'
  AND column_name = 'checksum_sha256'`).Scan(&nullable); err != nil {
		t.Fatal(err)
	}
	if nullable != "NO" {
		t.Fatalf("checksum_sha256 is_nullable = %q, want NO", nullable)
	}
	var constraintValidated bool
	if err := db.QueryRowContext(ctx, `
SELECT convalidated
FROM pg_constraint
WHERE conrelid = 'schema_migrations'::regclass
  AND conname = 'schema_migrations_checksum_sha256'`).Scan(&constraintValidated); err != nil {
		t.Fatal(err)
	}
	if !constraintValidated {
		t.Fatal("schema_migrations checksum constraint is not validated")
	}
	if err := Migrate(ctx, testURL); err != nil {
		t.Fatalf("idempotent migration after legacy backfill: %v", err)
	}
	runtimeStore, err := OpenRuntime(ctx, testURL)
	if err != nil {
		t.Fatalf("OpenRuntime after legacy backfill: %v", err)
	}
	if err := runtimeStore.Close(); err != nil {
		t.Fatalf("close runtime store: %v", err)
	}
}

func TestMigrateRollsBackLegacyUpgradeForUnknownMigration(t *testing.T) {
	ctx := context.Background()
	testURL, db := newMigrationTestSchema(t)
	if _, err := db.ExecContext(ctx, `
CREATE TABLE schema_migrations (
	version TEXT PRIMARY KEY,
	applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO schema_migrations(version) VALUES ('999_unknown.sql')`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, testURL); !errors.Is(err, ErrMigrationIntegrity) {
		t.Fatalf("Migrate() error = %v, want ErrMigrationIntegrity", err)
	}
	var checksumColumnExists bool
	if err := db.QueryRowContext(ctx, `
SELECT EXISTS (
	SELECT 1
	FROM information_schema.columns
	WHERE table_schema = current_schema()
	  AND table_name = 'schema_migrations'
	  AND column_name = 'checksum_sha256'
)`).Scan(&checksumColumnExists); err != nil {
		t.Fatal(err)
	}
	if checksumColumnExists {
		t.Fatal("failed legacy upgrade left a partially-added checksum column")
	}
}

func TestExpandMigrationsBackfillAndEnforceApplicationInvariants(t *testing.T) {
	ctx := context.Background()
	_, db := newMigrationTestSchema(t)
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) < 6 {
		t.Fatalf("loaded %d migrations, want at least 6", len(migrations))
	}
	for _, migration := range migrations[:3] {
		if _, err := db.ExecContext(ctx, string(migration.contents)); err != nil {
			t.Fatalf("apply prerequisite migration %s: %v", migration.version, err)
		}
	}

	now := time.Now().UTC()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users(id, display_name, created_at, updated_at) VALUES ('owner', 'Owner', $1, $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO auth_sessions(token_hash, yandex_user_id, created_at, expires_at)
VALUES ($1, 'owner', $2, $3)`, strings.Repeat("a", 64), now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO posts(owner_id, title, content, created_at, updated_at)
VALUES ('owner', 'Legacy post', 'Body', $1, $1)`, now); err != nil {
		t.Fatal(err)
	}

	for _, migration := range migrations[3:6] {
		if _, err := db.ExecContext(ctx, string(migration.contents)); err != nil {
			t.Fatalf("apply expand migration %s: %v", migration.version, err)
		}
	}

	var linkButtons, messageURL, userAvatar, sessionAvatar string
	var isPinned bool
	if err := db.QueryRowContext(ctx, `
SELECT link_buttons::text, max_message_url, max_is_pinned
FROM posts WHERE owner_id = 'owner'`).Scan(&linkButtons, &messageURL, &isPinned); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT avatar_url FROM users WHERE id = 'owner'`).Scan(&userAvatar); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT avatar_url FROM auth_sessions WHERE yandex_user_id = 'owner'`).Scan(&sessionAvatar); err != nil {
		t.Fatal(err)
	}
	if linkButtons != "[]" || messageURL != "" || isPinned || userAvatar != "" || sessionAvatar != "" {
		t.Fatalf("legacy defaults: buttons=%q url=%q pinned=%v user_avatar=%q session_avatar=%q",
			linkButtons, messageURL, isPinned, userAvatar, sessionAvatar)
	}

	invalidUpdates := []struct {
		name string
		sql  string
	}{
		{name: "null link buttons", sql: `UPDATE posts SET link_buttons = NULL WHERE owner_id = 'owner'`},
		{name: "too many link buttons", sql: `UPDATE posts SET link_buttons = '[{}, {}, {}, {}]'::jsonb WHERE owner_id = 'owner'`},
		{name: "null message URL", sql: `UPDATE posts SET max_message_url = NULL WHERE owner_id = 'owner'`},
		{name: "null pinned flag", sql: `UPDATE posts SET max_is_pinned = NULL WHERE owner_id = 'owner'`},
		{name: "negative views", sql: `UPDATE posts SET max_views = -1 WHERE owner_id = 'owner'`},
		{name: "null user avatar", sql: `UPDATE users SET avatar_url = NULL WHERE id = 'owner'`},
		{name: "null session avatar", sql: `UPDATE auth_sessions SET avatar_url = NULL WHERE yandex_user_id = 'owner'`},
	}
	for _, test := range invalidUpdates {
		t.Run(test.name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, test.sql); err == nil {
				t.Fatal("invalid update unexpectedly succeeded")
			}
		})
	}

	constraintNames := []string{
		"posts_link_buttons_shape",
		"users_avatar_url_not_null",
		"auth_sessions_avatar_url_not_null",
		"posts_max_message_url_not_null",
		"posts_max_is_pinned_not_null",
		"posts_max_views_nonnegative",
	}
	var validated int
	if err := db.QueryRowContext(ctx, `
SELECT count(*)
FROM pg_constraint
WHERE conname = ANY($1)
  AND connamespace = current_schema()::regnamespace
  AND convalidated`, constraintNames).Scan(&validated); err != nil {
		t.Fatal(err)
	}
	if validated != len(constraintNames) {
		t.Fatalf("validated expand constraints = %d, want %d", validated, len(constraintNames))
	}
}

func TestDirectGraphMigrationBackfillsArchivedCampaignWithoutFalseEvidence(
	t *testing.T,
) {
	ctx := context.Background()
	testURL, db := newMigrationTestSchema(t)
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	graphIndex := -1
	for index, migration := range migrations {
		if migration.version == "025_direct_campaign_graph.sql" {
			graphIndex = index
			break
		}
	}
	if graphIndex <= 0 {
		t.Fatal("025_direct_campaign_graph.sql not found")
	}
	if err := runMigrationSet(ctx, testURL, migrations[:graphIndex]); err != nil {
		t.Fatalf("apply Direct graph prerequisites: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := db.ExecContext(ctx, `INSERT INTO users(
id,display_name,created_at,updated_at
) VALUES('direct-archive-owner','Owner',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workspaces(
id,name,owner_user_id,compat_owner_user_id,is_personal,created_by,
created_at,updated_at
) VALUES(
'direct-archive-workspace','Archived Direct',
'direct-archive-owner','direct-archive-owner',FALSE,'direct-archive-owner',$1,$1
)`, now); err != nil {
		t.Fatal(err)
	}
	connectionID := "dcon_" + strings.Repeat("a", 32)
	campaignID := "dcmp_" + strings.Repeat("b", 32)
	consentID := "dcons_" + strings.Repeat("c", 32)
	if _, err := db.ExecContext(ctx, `INSERT INTO direct_connections(
id,workspace_id,account_id,currency_code,timezone,token_ciphertext,
token_key_version,status,connected_by,created_at,updated_at
) VALUES($1,'direct-archive-workspace','account','RUB','Europe/Moscow',
'v1.secret',1,'active','direct-archive-owner',$2,$2)`,
		connectionID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO direct_campaigns(
id,workspace_id,connection_id,provider_campaign_id,name,objective,
landing_url,brief,regions,weekly_budget_minor,currency_code,starts_at,ends_at,
status,provider_status,provider_state,created_by,created_at,updated_at
) VALUES(
$1,'direct-archive-workspace',$2,101,'Legacy campaign','traffic',
'https://maxposty.ru/','Legacy brief','["225"]'::jsonb,30000,'RUB',
$3::date,$4::date,'accepted','ACCEPTED','OFF','direct-archive-owner',$5,$5
)`, campaignID, connectionID, now, now.AddDate(0, 1, 0), now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO direct_auto_launch_consents(
id,workspace_id,campaign_id,connection_id,actor_user_id,consent_version,
confirmation,campaign_version,account_id,provider_campaign_id,campaign_name,
weekly_budget_minor,currency_code,starts_at,ends_at,authorized_at
) VALUES(
$1,'direct-archive-workspace',$2,$3,'direct-archive-owner',
'yandex-direct-auto-launch-v1','АВТОЗАПУСК',1,'account',101,
'Legacy campaign',30000,'RUB',$4::date,$5::date,$6
)`, consentID, campaignID, connectionID, now, now.AddDate(0, 1, 0), now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE workspaces
SET archived_at=$1,updated_at=$1 WHERE id='direct-archive-workspace'`,
		now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := runMigrationSet(
		ctx, testURL, migrations[:graphIndex+1],
	); err != nil {
		t.Fatalf("apply Direct graph migration over archived campaign: %v", err)
	}
	var title, textValue, keyword, graphHash string
	var revisionID sql.NullString
	var verifiedAt sql.NullTime
	if err := db.QueryRowContext(ctx, `SELECT
titles->>0,texts->>0,keywords->>0,provider_graph_hash,
provider_revision_id,graph_verified_at
FROM direct_campaigns WHERE id=$1`, campaignID).Scan(
		&title, &textValue, &keyword, &graphHash, &revisionID, &verifiedAt,
	); err != nil {
		t.Fatal(err)
	}
	if title != "Черновик объявления" ||
		textValue != "Проверьте текст объявления" || keyword != "черновик" ||
		graphHash != "" || revisionID.Valid || verifiedAt.Valid {
		t.Fatalf(
			"archived campaign graph backfill title=%q text=%q keyword=%q hash=%q revision=%#v verified=%#v",
			title, textValue, keyword, graphHash, revisionID, verifiedAt,
		)
	}
	var storedInvalidated, evidenceInvalidated sql.NullTime
	var evidenceReason string
	if err := db.QueryRowContext(ctx, `SELECT invalidated_at
FROM direct_auto_launch_consents WHERE id=$1`, consentID).Scan(
		&storedInvalidated,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT invalidated_at,invalid_reason
FROM direct_auto_launch_consent_evidence WHERE id=$1`, consentID).Scan(
		&evidenceInvalidated, &evidenceReason,
	); err != nil {
		t.Fatal(err)
	}
	if storedInvalidated.Valid || !evidenceInvalidated.Valid ||
		evidenceReason != "legacy_consent_version" {
		t.Fatalf(
			"legacy archived consent stored=%#v evidence=%#v reason=%q",
			storedInvalidated, evidenceInvalidated, evidenceReason,
		)
	}
}

func TestChannelIconBackfillAcceptsOnlyOfficialMAXAssets(t *testing.T) {
	ctx := context.Background()
	testURL, db := newMigrationTestSchema(t)
	if err := Migrate(ctx, testURL); err != nil {
		t.Fatalf("initial migration: %v", err)
	}
	now := time.Now().UTC()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users(id, display_name, created_at, updated_at) VALUES ('owner', 'Owner', $1, $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO channels(owner_id, verified_max_owner_id, max_chat_id, title, active, created_at, updated_at)
VALUES ('owner', 'max-owner', 'safe-chat', 'Safe', FALSE, $1, $1),
       ('owner', 'max-owner', 'unsafe-chat', 'Unsafe', FALSE, $1, $1),
	   ('owner', 'max-owner', 'fragment-chat', 'Fragment', FALSE, $1, $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO observed_bot_chats(max_chat_id, icon_url, active, last_seen_at)
VALUES ('safe-chat', 'https://cdn.max.ru/channel.png', TRUE, $1),
       ('unsafe-chat', 'https://cdn.max.ru.evil.example/tracker.png', TRUE, $1),
       ('fragment-chat', 'https://cdn.max.ru/channel.png#tracking', TRUE, $1)`, now); err != nil {
		t.Fatal(err)
	}

	migrationSQL, err := migrationFiles.ReadFile("migrations/007_channel_icon_backfill.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(migrationSQL)); err != nil {
		t.Fatalf("rerun channel icon backfill: %v", err)
	}

	var safeIcon, unsafeIcon, fragmentIcon string
	if err := db.QueryRowContext(ctx, `SELECT icon_url FROM channels WHERE max_chat_id='safe-chat'`).Scan(&safeIcon); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT icon_url FROM channels WHERE max_chat_id='unsafe-chat'`).Scan(&unsafeIcon); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT icon_url FROM channels WHERE max_chat_id='fragment-chat'`).Scan(&fragmentIcon); err != nil {
		t.Fatal(err)
	}
	if safeIcon != "https://cdn.max.ru/channel.png" || unsafeIcon != "" || fragmentIcon != "" {
		t.Fatalf("backfilled icons: safe=%q unsafe=%q fragment=%q", safeIcon, unsafeIcon, fragmentIcon)
	}
}

func newMigrationTestSchema(t *testing.T) (string, *sql.DB) {
	t.Helper()
	baseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if baseURL == "" {
		t.Fatal("TEST_DATABASE_URL is required for migration tests")
	}
	digest := sha256.Sum256([]byte(t.Name() + time.Now().UTC().String()))
	schema := "test_migration_" + hex.EncodeToString(digest[:8])
	admin, err := openPostgres(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(context.Background(), `CREATE SCHEMA `+quoteIdentifier(schema)); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	testURL, err := withSearchPath(baseURL, schema)
	if err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	db, err := openPostgres(testURL)
	if err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(cleanupCtx, `DROP SCHEMA IF EXISTS `+quoteIdentifier(schema)+` CASCADE`); err != nil {
			t.Errorf("drop migration test schema: %v", err)
		}
		_ = admin.Close()
	})
	return testURL, db
}
