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
	const futureVersion = "009_future_additive.sql"
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
INSERT INTO channels(owner_id, verified_max_owner_id, max_chat_id, title, created_at, updated_at)
VALUES ('owner', 'max-owner', 'safe-chat', 'Safe', $1, $1),
       ('owner', 'max-owner', 'unsafe-chat', 'Unsafe', $1, $1),
	   ('owner', 'max-owner', 'fragment-chat', 'Fragment', $1, $1)`, now); err != nil {
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
