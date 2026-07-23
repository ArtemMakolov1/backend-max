package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

var (
	ErrNotFound                = errors.New("not found")
	ErrConflict                = errors.New("state conflict")
	ErrPublicationExists       = errors.New("MAX publication exists")
	ErrChannelOwned            = errors.New("channel is already connected to another account")
	ErrScheduleNotDue          = errors.New("scheduled post is not due")
	ErrMigrationIntegrity      = errors.New("migration integrity check failed")
	ErrMediaQuotaExceeded      = errors.New("media storage quota exceeded")
	ErrMediaUploadBusy         = errors.New("media upload is already in progress")
	ErrOwnedTeamWorkspaceLimit = errors.New("team workspace ownership limit reached")
)

type Store struct {
	db             *postgresDB
	cleanup        func(context.Context) error
	defaultOwnerID string
}

type postgresDB struct{ *sql.DB }

func (db *postgresDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return db.DB.ExecContext(ctx, bindSQL(query), args...)
}

func (db *postgresDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return db.DB.QueryContext(ctx, bindSQL(query), args...)
}

func (db *postgresDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return db.DB.QueryRowContext(ctx, bindSQL(query), args...)
}

//go:embed migrations/*.sql
var migrationFiles embed.FS

const RequiredSchemaVersion = "021_yookassa_subscriptions.sql"

type schemaMigration struct {
	version        string
	contents       []byte
	checksumSHA256 string
}

// Open remains convenient for tests and simple deployments. Production should
// use OpenWithMigrationURL so runtime traffic goes through PgBouncer while DDL
// uses a direct PostgreSQL connection.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	if strings.HasPrefix(databaseURL, "postgres://") || strings.HasPrefix(databaseURL, "postgresql://") {
		return OpenWithMigrationURL(ctx, databaseURL, databaseURL)
	}
	return openIsolatedTestStore(ctx, databaseURL)
}

func OpenWithMigrationURL(ctx context.Context, databaseURL, directDatabaseURL string) (*Store, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	if strings.TrimSpace(directDatabaseURL) == "" {
		return nil, errors.New("DIRECT_DATABASE_URL is required for migrations")
	}
	if err := Migrate(ctx, directDatabaseURL); err != nil {
		return nil, err
	}
	db, err := openPostgres(databaseURL)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}
	return &Store{db: &postgresDB{DB: db}}, nil
}

// Migrate is intended for the short-lived cmd/migrate process. The runtime
// server never receives or uses the DDL-capable direct database credential.
func Migrate(ctx context.Context, directDatabaseURL string) error {
	if strings.TrimSpace(directDatabaseURL) == "" {
		return errors.New("DIRECT_DATABASE_URL is required for migrations")
	}
	return runMigrations(ctx, directDatabaseURL)
}

func OpenRuntime(ctx context.Context, databaseURL string) (*Store, error) {
	return OpenRuntimeWithTracer(ctx, databaseURL, nil)
}

// OpenRuntimeWithTracer opens the PgBouncer-compatible runtime pool and wires
// a pgx tracer into every physical connection. The tracer must not retain SQL
// text or arguments because they can contain tenant data.
func OpenRuntimeWithTracer(ctx context.Context, databaseURL string, tracer pgx.QueryTracer) (*Store, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		return nil, err
	}
	db, err := openPostgresWithTracer(databaseURL, tracer)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}
	if err := verifyRuntimeMigrations(ctx, db, migrations); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("check database schema (run cmd/migrate first): %w", err)
	}
	return &Store{db: &postgresDB{DB: db}}, nil
}

func openPostgres(databaseURL string) (*sql.DB, error) {
	return openPostgresWithTracer(databaseURL, nil)
}

func openPostgresWithTracer(databaseURL string, tracer pgx.QueryTracer) (*sql.DB, error) {
	config, err := pgx.ParseConfig(strings.TrimSpace(databaseURL))
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL URL: %w", err)
	}
	// PgBouncer transaction pooling cannot safely retain server-side prepared
	// statements between transactions. Exec uses the extended protocol without
	// preparing or caching statements on a server connection.
	config.DefaultQueryExecMode = pgx.QueryExecModeExec
	config.Tracer = tracer
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxIdleTime(5 * time.Minute)
	db.SetConnMaxLifetime(30 * time.Minute)
	return db, nil
}

func openIsolatedTestStore(ctx context.Context, testID string) (*Store, error) {
	baseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if baseURL == "" {
		return nil, errors.New("TEST_DATABASE_URL is required for PostgreSQL-backed tests")
	}
	digest := sha256.Sum256([]byte(testID + time.Now().UTC().String()))
	schema := "test_" + hex.EncodeToString(digest[:12])
	admin, err := openPostgres(baseURL)
	if err != nil {
		return nil, err
	}
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+quoteIdentifier(schema)); err != nil {
		_ = admin.Close()
		return nil, fmt.Errorf("create test schema: %w", err)
	}
	if err := admin.Close(); err != nil {
		return nil, fmt.Errorf("close test schema connection: %w", err)
	}
	testURL, err := withSearchPath(baseURL, schema)
	if err != nil {
		return nil, err
	}
	storage, err := OpenWithMigrationURL(ctx, testURL, testURL)
	if err != nil {
		return nil, err
	}
	storage.defaultOwnerID = "test-owner"
	if err := storage.UpsertUser(ctx, User{ID: storage.defaultOwnerID, DisplayName: "Test Owner"}); err != nil {
		_ = storage.Close()
		return nil, err
	}
	storage.cleanup = func(cleanupCtx context.Context) error {
		db, openErr := openPostgres(baseURL)
		if openErr != nil {
			return openErr
		}
		defer func() { _ = db.Close() }()
		_, dropErr := db.ExecContext(cleanupCtx, `DROP SCHEMA IF EXISTS `+quoteIdentifier(schema)+` CASCADE`)
		return dropErr
	}
	return storage, nil
}

func withSearchPath(databaseURL, schema string) (string, error) {
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		return "", fmt.Errorf("parse test PostgreSQL URL: %w", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func quoteIdentifier(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }

func (s *Store) Close() error {
	closeErr := s.db.Close()
	if s.cleanup != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.cleanup(cleanupCtx); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("drop test schema: %w", err))
		}
	}
	return closeErr
}

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// DBStats returns a point-in-time snapshot of the runtime pool for Prometheus.
func (s *Store) DBStats() sql.DBStats { return s.db.Stats() }

func runMigrations(ctx context.Context, directDatabaseURL string) error {
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		return err
	}
	return runMigrationSet(ctx, directDatabaseURL, migrations)
}

func runMigrationSet(ctx context.Context, directDatabaseURL string, migrations []schemaMigration) error {
	db, err := openPostgres(directDatabaseURL)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtext('maxstudio_schema_migrations'))`); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock(hashtext('maxstudio_schema_migrations'))`)
	}()
	applied, err := prepareMigrationTable(ctx, conn, migrations)
	if err != nil {
		return err
	}
	for _, migration := range migrations {
		if _, ok := applied[migration.version]; ok {
			continue
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", migration.version, err)
		}
		if _, err := tx.ExecContext(ctx, string(migration.contents)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", migration.version, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, checksum_sha256) VALUES ($1, $2)`,
			migration.version, migration.checksumSHA256); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", migration.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", migration.version, err)
		}
	}
	if err := verifyAppliedMigrations(ctx, conn, migrations); err != nil {
		return fmt.Errorf("verify migrations after apply: %w", err)
	}
	return nil
}

func loadEmbeddedMigrations() ([]schemaMigration, error) {
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	migrations := make([]schemaMigration, 0, len(entries))
	requiredFound := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		contents, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		migrations = append(migrations, schemaMigration{
			version:        entry.Name(),
			contents:       contents,
			checksumSHA256: migrationChecksumSHA256(contents),
		})
		if entry.Name() == RequiredSchemaVersion {
			requiredFound = true
		}
	}
	if len(migrations) == 0 {
		return nil, errors.New("no embedded SQL migrations found")
	}
	if !requiredFound {
		return nil, fmt.Errorf("required schema migration %s is not embedded", RequiredSchemaVersion)
	}
	return migrations, nil
}

func migrationChecksumSHA256(contents []byte) string {
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}

func prepareMigrationTable(ctx context.Context, conn *sql.Conn, migrations []schemaMigration) (map[string]struct{}, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin schema_migrations upgrade: %w", err)
	}
	rollback := func(resultErr error) (map[string]struct{}, error) {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			resultErr = errors.Join(resultErr, fmt.Errorf("rollback schema_migrations upgrade: %w", rollbackErr))
		}
		return nil, resultErr
	}
	if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
	version TEXT PRIMARY KEY,
	checksum_sha256 TEXT NOT NULL,
	applied_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	CONSTRAINT schema_migrations_checksum_sha256 CHECK (checksum_sha256 ~ '^[0-9a-f]{64}$')
)`); err != nil {
		return rollback(fmt.Errorf("create schema_migrations: %w", err))
	}
	// Existing installations created this table without a checksum column. Add
	// it as nullable first so the existing rows can be backfilled atomically.
	if _, err := tx.ExecContext(ctx, `ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS checksum_sha256 TEXT`); err != nil {
		return rollback(fmt.Errorf("add schema_migrations checksum: %w", err))
	}
	expected := make(map[string]string, len(migrations))
	for _, migration := range migrations {
		expected[migration.version] = migration.checksumSHA256
	}
	applied := make(map[string]struct{}, len(migrations))
	records, err := readAppliedMigrationRecords(ctx, tx)
	if err != nil {
		return rollback(err)
	}
	for _, record := range records {
		expectedChecksum, ok := expected[record.version]
		if !ok {
			return rollback(fmt.Errorf("%w: database contains unknown migration %q", ErrMigrationIntegrity, record.version))
		}
		if record.checksum.Valid && record.checksum.String != expectedChecksum {
			return rollback(migrationChecksumMismatch(record.version, record.checksum.String, expectedChecksum))
		}
		if !record.checksum.Valid {
			result, err := tx.ExecContext(ctx,
				`UPDATE schema_migrations SET checksum_sha256 = $1 WHERE version = $2 AND checksum_sha256 IS NULL`,
				expectedChecksum, record.version)
			if err != nil {
				return rollback(fmt.Errorf("backfill checksum for migration %s: %w", record.version, err))
			}
			updated, err := result.RowsAffected()
			if err != nil {
				return rollback(fmt.Errorf("count checksum backfill for migration %s: %w", record.version, err))
			}
			if updated != 1 {
				return rollback(fmt.Errorf("%w: checksum backfill for migration %q updated %d rows", ErrMigrationIntegrity, record.version, updated))
			}
		}
		applied[record.version] = struct{}{}
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE schema_migrations ALTER COLUMN checksum_sha256 SET NOT NULL`); err != nil {
		return rollback(fmt.Errorf("require schema_migrations checksum: %w", err))
	}
	if _, err := tx.ExecContext(ctx, `
DO $$
BEGIN
	IF NOT EXISTS (
		SELECT 1
		FROM pg_constraint
		WHERE conrelid = 'schema_migrations'::regclass
		  AND conname = 'schema_migrations_checksum_sha256'
	) THEN
		ALTER TABLE schema_migrations
			ADD CONSTRAINT schema_migrations_checksum_sha256
			CHECK (checksum_sha256 ~ '^[0-9a-f]{64}$') NOT VALID;
	END IF;
END
$$`); err != nil {
		return rollback(fmt.Errorf("add schema_migrations checksum constraint: %w", err))
	}
	if _, err := tx.ExecContext(ctx,
		`ALTER TABLE schema_migrations VALIDATE CONSTRAINT schema_migrations_checksum_sha256`); err != nil {
		return rollback(fmt.Errorf("validate schema_migrations checksum constraint: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit schema_migrations upgrade: %w", err)
	}
	return applied, nil
}

type migrationRowsQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type appliedMigrationRecord struct {
	version  string
	checksum sql.NullString
}

func readAppliedMigrationRecords(ctx context.Context, queryer migrationRowsQueryer) ([]appliedMigrationRecord, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT version, checksum_sha256 FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read applied migration checksums: %w", err)
	}
	defer func() { _ = rows.Close() }()
	records := make([]appliedMigrationRecord, 0)
	for rows.Next() {
		var record appliedMigrationRecord
		if err := rows.Scan(&record.version, &record.checksum); err != nil {
			return nil, fmt.Errorf("scan applied migration checksum: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied migration checksums: %w", err)
	}
	return records, nil
}

func verifyAppliedMigrations(ctx context.Context, queryer migrationRowsQueryer, migrations []schemaMigration) error {
	return verifyAppliedMigrationSet(ctx, queryer, migrations, false)
}

func verifyRuntimeMigrations(ctx context.Context, queryer migrationRowsQueryer, migrations []schemaMigration) error {
	return verifyAppliedMigrationSet(ctx, queryer, migrations, true)
}

func verifyAppliedMigrationSet(
	ctx context.Context,
	queryer migrationRowsQueryer,
	migrations []schemaMigration,
	allowNewerMigrations bool,
) error {
	expected := make(map[string]string, len(migrations))
	maxKnownVersion := ""
	for _, migration := range migrations {
		expected[migration.version] = migration.checksumSHA256
		if migration.version > maxKnownVersion {
			maxKnownVersion = migration.version
		}
	}
	if maxKnownVersion == "" {
		return fmt.Errorf("%w: binary contains no known migrations", ErrMigrationIntegrity)
	}
	applied := make(map[string]struct{}, len(migrations))
	records, err := readAppliedMigrationRecords(ctx, queryer)
	if err != nil {
		return err
	}
	for _, record := range records {
		expectedChecksum, ok := expected[record.version]
		if !ok && (!allowNewerMigrations || record.version <= maxKnownVersion) {
			return fmt.Errorf("%w: database contains unknown migration %q", ErrMigrationIntegrity, record.version)
		}
		if !record.checksum.Valid {
			return fmt.Errorf("%w: migration %q has no SHA-256 checksum; run cmd/migrate to upgrade metadata", ErrMigrationIntegrity, record.version)
		}
		if !ok {
			continue
		}
		if record.checksum.String != expectedChecksum {
			return migrationChecksumMismatch(record.version, record.checksum.String, expectedChecksum)
		}
		applied[record.version] = struct{}{}
	}
	for _, migration := range migrations {
		if _, ok := applied[migration.version]; !ok {
			return fmt.Errorf("database schema %s is not applied; run cmd/migrate first", migration.version)
		}
	}
	return nil
}

func migrationChecksumMismatch(version, stored, expected string) error {
	return fmt.Errorf(
		"%w: migration %q SHA-256 checksum mismatch (database %s, binary %s)",
		ErrMigrationIntegrity, version, stored, expected,
	)
}

func bindSQL(query string) string {
	var builder strings.Builder
	builder.Grow(len(query) + 16)
	parameter := 1
	for _, r := range query {
		if r == '?' {
			fmt.Fprintf(&builder, "$%d", parameter)
			parameter++
			continue
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

func nowText() string { return time.Now().UTC().Format(time.RFC3339Nano) }

const channelColumns = `id, owner_id, workspace_id, verified_max_owner_id, max_chat_id, title, description, public_link, icon_url,
participants_count, is_public, messages_count, has_pinned_message, max_last_event_time, max_info_synced_at,
is_channel, active, created_at, updated_at`

func (s *Store) CreateChannel(ctx context.Context, channel Channel) (Channel, error) {
	if channel.UserID == "" {
		channel.UserID = s.defaultOwnerID
	}
	if channel.VerifiedMAXOwnerID == "" && s.defaultOwnerID != "" {
		channel.VerifiedMAXOwnerID = "test-max-owner"
	}
	if channel.UserID == "" || channel.VerifiedMAXOwnerID == "" {
		return Channel{}, errors.New("channel owner and verified MAX owner are required")
	}
	now := nowText()
	var id int64
	err := s.db.QueryRowContext(ctx, `
INSERT INTO channels(owner_id, workspace_id, verified_max_owner_id, max_chat_id, title, description, public_link, icon_url,
participants_count, is_public, messages_count, has_pinned_message, max_last_event_time, max_info_synced_at,
is_channel, active, created_at, updated_at)
VALUES (?, NULLIF(?,''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		channel.UserID, channel.WorkspaceID, channel.VerifiedMAXOwnerID, channel.MAXChatID, channel.Title,
		channel.Description, channel.PublicLink, channel.IconURL, channel.ParticipantsCount, channel.IsPublic,
		channel.MessagesCount, channel.HasPinnedMessage, channel.MAXLastEventTime, channel.MAXInfoSyncedAt,
		channel.IsChannel, channel.Active, now, now).Scan(&id)
	if err != nil {
		return Channel{}, fmt.Errorf("create channel: %w", err)
	}
	return s.GetChannel(ctx, id)
}

func (s *Store) UpsertConnectedChannel(ctx context.Context, channel Channel) (Channel, error) {
	if channel.UserID == "" {
		channel.UserID = s.defaultOwnerID
	}
	if channel.VerifiedMAXOwnerID == "" && s.defaultOwnerID != "" {
		channel.VerifiedMAXOwnerID = "test-max-owner"
	}
	if channel.UserID == "" || channel.VerifiedMAXOwnerID == "" {
		return Channel{}, errors.New("channel owner and verified MAX owner are required")
	}
	now := nowText()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO channels(owner_id, workspace_id, verified_max_owner_id, max_chat_id, title, description, public_link, icon_url,
participants_count, is_public, messages_count, has_pinned_message, max_last_event_time, max_info_synced_at,
is_channel, active, created_at, updated_at)
VALUES (?, NULLIF(?,''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(max_chat_id) DO UPDATE SET
	title = excluded.title,
	description = excluded.description,
	public_link = excluded.public_link,
	icon_url = excluded.icon_url,
	participants_count = excluded.participants_count,
	is_public = excluded.is_public,
	messages_count = excluded.messages_count,
	has_pinned_message = excluded.has_pinned_message,
	max_last_event_time = excluded.max_last_event_time,
	max_info_synced_at = excluded.max_info_synced_at,
	is_channel = excluded.is_channel,
	active = excluded.active,
	updated_at = excluded.updated_at`,
		channel.UserID, channel.WorkspaceID, channel.VerifiedMAXOwnerID, channel.MAXChatID, channel.Title,
		channel.Description, channel.PublicLink, channel.IconURL, channel.ParticipantsCount, channel.IsPublic,
		channel.MessagesCount, channel.HasPinnedMessage, channel.MAXLastEventTime, channel.MAXInfoSyncedAt,
		channel.IsChannel, channel.Active, now, now)
	if err != nil {
		return Channel{}, fmt.Errorf("connect channel: %w", err)
	}
	return s.GetChannelByMAXChatID(ctx, channel.MAXChatID)
}

func (s *Store) UpsertDiscoveredChannel(ctx context.Context, maxChatID, title string, isChannel bool, active bool) (Channel, error) {
	providedTitle := strings.TrimSpace(title)
	_, err := s.db.ExecContext(ctx, `
UPDATE channels SET
    title = CASE WHEN ? = '' THEN title ELSE ? END,
    is_channel = ?, active = ?, updated_at = ?
WHERE max_chat_id = ?`, providedTitle, providedTitle, isChannel, active, nowText(), maxChatID)
	if err != nil {
		return Channel{}, fmt.Errorf("refresh discovered channel: %w", err)
	}
	return s.GetChannelByMAXChatID(ctx, maxChatID)
}

func (s *Store) ListChannels(ctx context.Context) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+channelColumns+`
FROM channels ORDER BY active DESC, lower(title), id`)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	channels := make([]Channel, 0)
	for rows.Next() {
		channel, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		channels = append(channels, channel)
	}
	return channels, rows.Err()
}

func (s *Store) ListChannelsForUser(ctx context.Context, userID string) ([]Channel, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, errors.New("user id is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+channelColumns+`
FROM channels WHERE owner_id = ? AND workspace_id IN (
SELECT id FROM workspaces WHERE is_personal AND archived_at IS NULL
) ORDER BY active DESC, lower(title), id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list user channels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	channels := make([]Channel, 0)
	for rows.Next() {
		channel, scanErr := scanChannel(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		channels = append(channels, channel)
	}
	return channels, rows.Err()
}

func (s *Store) GetChannel(ctx context.Context, id int64) (Channel, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+channelColumns+` FROM channels WHERE id = ?`, id)
	return scanChannel(row)
}

func (s *Store) GetChannelForUser(ctx context.Context, userID string, id int64) (Channel, error) {
	return scanChannel(s.db.QueryRowContext(ctx, `SELECT `+channelColumns+
		` FROM channels WHERE owner_id = ? AND id = ? AND workspace_id IN (
SELECT id FROM workspaces WHERE is_personal AND archived_at IS NULL)`, userID, id))
}

// getChannelForOwner is an owner-scoped primitive for trusted store workflows.
// HTTP authorization must continue to use GetChannelForUser or workspace
// membership APIs; team resources use an immutable synthetic compat owner.
func (s *Store) getChannelForOwner(ctx context.Context, ownerID string, id int64) (Channel, error) {
	return scanChannel(s.db.QueryRowContext(ctx, `SELECT `+channelColumns+
		` FROM channels WHERE owner_id = ? AND id = ?`, ownerID, id))
}

func (s *Store) GetChannelByMAXChatIDForUser(ctx context.Context, userID, maxChatID string) (Channel, error) {
	return scanChannel(s.db.QueryRowContext(ctx, `SELECT `+channelColumns+
		` FROM channels WHERE owner_id = ? AND max_chat_id = ? AND workspace_id IN (
SELECT id FROM workspaces WHERE is_personal AND archived_at IS NULL)`, userID, maxChatID))
}

func (s *Store) GetChannelByMAXChatID(ctx context.Context, maxChatID string) (Channel, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+channelColumns+` FROM channels WHERE max_chat_id = ?`, maxChatID)
	return scanChannel(row)
}

func (s *Store) UpdateChannel(ctx context.Context, id int64, maxChatID, title *string, active *bool) (Channel, error) {
	current, err := s.GetChannel(ctx, id)
	if err != nil {
		return Channel{}, err
	}
	if maxChatID != nil && *maxChatID != current.MAXChatID {
		return Channel{}, fmt.Errorf("%w: max_chat_id is immutable; reconnect the MAX channel", ErrConflict)
	}
	if title != nil {
		current.Title = *title
	}
	if active != nil {
		current.Active = *active
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE channels SET max_chat_id = ?, title = ?, description = ?, public_link = ?, icon_url = ?, participants_count = ?,
is_public = ?, messages_count = ?, has_pinned_message = ?, max_last_event_time = ?, max_info_synced_at = ?,
active = ?, updated_at = ? WHERE id = ?`,
		current.MAXChatID, current.Title, current.Description, current.PublicLink, current.IconURL, current.ParticipantsCount,
		current.IsPublic, current.MessagesCount, current.HasPinnedMessage, current.MAXLastEventTime, current.MAXInfoSyncedAt,
		current.Active, nowText(), id)
	if err != nil {
		return Channel{}, fmt.Errorf("update channel: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return Channel{}, ErrNotFound
	}
	return s.GetChannel(ctx, id)
}

func (s *Store) UpdateChannelForUser(ctx context.Context, userID string, id int64, title *string, active *bool) (Channel, error) {
	current, err := s.GetChannelForUser(ctx, userID, id)
	if err != nil {
		return Channel{}, err
	}
	if title != nil {
		current.Title = *title
	}
	if active != nil {
		current.Active = *active
	}
	result, err := s.db.ExecContext(ctx, `UPDATE channels SET title = ?, active = ?, updated_at = ?
WHERE owner_id = ? AND id = ? AND workspace_id IN (
SELECT id FROM workspaces WHERE is_personal AND archived_at IS NULL)`, current.Title, current.Active, nowText(), userID, id)
	if err != nil {
		return Channel{}, fmt.Errorf("update user channel: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return Channel{}, ErrNotFound
	}
	return s.GetChannelForUser(ctx, userID, id)
}

// RefreshChannelVisualMetadataForUser stores visual metadata obtained from an
// authenticated MAX chat lookup without widening access to another tenant's
// channel. URL trust is validated by the application before this boundary.
func (s *Store) RefreshChannelVisualMetadataForUser(ctx context.Context, userID string, id int64, iconURL string, participantsCount int) (Channel, error) {
	if strings.TrimSpace(userID) == "" || id <= 0 {
		return Channel{}, errors.New("user id and channel id are required")
	}
	if participantsCount < 0 {
		participantsCount = 0
	}
	channel, err := s.GetChannelForUser(ctx, userID, id)
	if err != nil {
		return Channel{}, err
	}
	return s.SyncChannelParticipantStatsForUser(ctx, userID, id, channel.MAXChatID,
		iconURL, participantsCount, time.Now().UTC())
}

func (s *Store) DeleteChannel(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, `
DELETE FROM channels
WHERE id = ?
  AND NOT EXISTS (
	SELECT 1 FROM posts
	WHERE channel_id = ?
  )`, id, id)
	if err != nil {
		return fmt.Errorf("delete channel: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 0 {
		return nil
	}

	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM channels WHERE id = ?)`, id).Scan(&exists); err != nil {
		return fmt.Errorf("check channel after delete: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	count, err := s.CountChannelBlockingPosts(ctx, id)
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: channel has %d linked post(s); delete or move them before deleting the channel", ErrConflict, count)
}

func (s *Store) DeleteChannelForUser(ctx context.Context, userID string, id int64) error {
	result, err := s.db.ExecContext(ctx, `
DELETE FROM channels
WHERE owner_id = ? AND id = ? AND workspace_id IN (
  SELECT id FROM workspaces WHERE is_personal AND archived_at IS NULL
)
  AND NOT EXISTS (
	SELECT 1 FROM posts
	WHERE channel_id = ?
  )`, userID, id, id)
	if err != nil {
		return fmt.Errorf("delete user channel: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 0 {
		return nil
	}
	if _, err := s.GetChannelForUser(ctx, userID, id); err != nil {
		return err
	}
	return fmt.Errorf("%w: channel has linked posts; delete or move them before deleting the channel", ErrConflict)
}

// CountChannelBlockingPosts reports every post whose channel association would
// be cleared by the database foreign key if the channel were deleted.
func (s *Store) CountChannelBlockingPosts(ctx context.Context, id int64) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM posts
WHERE channel_id = ?`, id).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count channel post dependencies: %w", err)
	}
	return count, nil
}

func (s *Store) CreatePost(ctx context.Context, post Post) (Post, error) {
	if post.UserID == "" {
		post.UserID = s.defaultOwnerID
	}
	if post.UserID == "" {
		return Post{}, errors.New("post owner is required")
	}
	if post.Format == "" {
		post.Format = FormatMarkdown
	}
	if post.Status == "" {
		post.Status = PostStatusDraft
	}
	if post.Status == PostStatusScheduled && (post.ScheduledAt == nil || post.ScheduledAt.IsZero()) {
		return Post{}, errors.New("scheduled post requires scheduled_at")
	}
	if post.Status != PostStatusScheduled && post.ScheduledAt != nil {
		return Post{}, errors.New("scheduled_at requires scheduled status")
	}
	linkButtonsJSON, err := marshalLinkButtons(post.LinkButtons)
	if err != nil {
		return Post{}, err
	}
	now := nowText()
	var id int64
	err = s.db.QueryRowContext(ctx, `
INSERT INTO posts(owner_id, workspace_id, title, content, format, status, channel_id, image_url, image_path, image_prompt, link_buttons,
                  notify, disable_link_preview, scheduled_at, max_message_id, max_message_url, max_views,
                  max_stats_synced_at, max_is_pinned, last_error, published_at, created_at, updated_at)
VALUES (?, NULLIF(?,''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		post.UserID, post.WorkspaceID, post.Title, post.Content, post.Format, post.Status, nullableInt64(post.ChannelID), post.ImageURL,
		post.ImagePath, post.ImagePrompt, linkButtonsJSON, post.Notify, post.DisableLinkPreview, nullableTime(post.ScheduledAt),
		post.MAXMessageID, post.MAXMessageURL, nullableInt64(post.MAXViews), nullableTime(post.MAXStatsSyncedAt),
		post.MAXIsPinned, post.LastError, nullableTime(post.PublishedAt), now, now).Scan(&id)
	if err != nil {
		return Post{}, fmt.Errorf("create post: %w", err)
	}
	return s.GetPost(ctx, id)
}

func (s *Store) ListPosts(ctx context.Context, status string, channelID *int64) ([]Post, error) {
	query := `SELECT ` + postColumns + ` FROM posts WHERE 1=1`
	args := make([]any, 0, 2)
	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	if channelID != nil {
		query += ` AND channel_id = ?`
		args = append(args, *channelID)
	}
	if status == PostStatusScheduled {
		query += ` ORDER BY scheduled_at, id`
	} else {
		query += ` ORDER BY created_at DESC, id DESC`
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list posts: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	posts := make([]Post, 0)
	for rows.Next() {
		post, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		posts = append(posts, post)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.hydratePostAttachments(ctx, posts); err != nil {
		return nil, err
	}
	return posts, nil
}

func (s *Store) ListPostsForUser(ctx context.Context, userID, status string, channelID *int64) ([]Post, error) {
	query := `SELECT ` + postColumns + ` FROM posts WHERE owner_id = ? AND workspace_id IN (
SELECT id FROM workspaces WHERE is_personal AND archived_at IS NULL)`
	args := []any{userID}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	if channelID != nil {
		query += ` AND channel_id = ?`
		args = append(args, *channelID)
	}
	if status == PostStatusScheduled {
		query += ` ORDER BY scheduled_at, id`
	} else {
		query += ` ORDER BY created_at DESC, id DESC`
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list user posts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	posts := make([]Post, 0)
	for rows.Next() {
		post, scanErr := scanPost(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		posts = append(posts, post)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.hydratePostAttachments(ctx, posts); err != nil {
		return nil, err
	}
	return posts, nil
}

func (s *Store) GetPost(ctx context.Context, id int64) (Post, error) {
	post, err := s.getPostWithoutAttachments(ctx, id)
	if err != nil {
		return Post{}, err
	}
	posts := []Post{post}
	if err := s.hydratePostAttachments(ctx, posts); err != nil {
		return Post{}, err
	}
	return posts[0], nil
}

func (s *Store) GetPostForUser(ctx context.Context, userID string, id int64) (Post, error) {
	post, err := s.getPostForUserWithoutAttachments(ctx, userID, id)
	if err != nil {
		return Post{}, err
	}
	posts := []Post{post}
	if err := s.hydratePostAttachments(ctx, posts); err != nil {
		return Post{}, err
	}
	return posts[0], nil
}

// getPostForOwner is the trusted owner-scoped counterpart to the deliberately
// personal-only GetPostForUser. It is used after a mutation already matched
// both owner_id and the globally unique post ID, including team compat owners.
func (s *Store) getPostForOwner(ctx context.Context, ownerID string, id int64) (Post, error) {
	post, err := scanPost(s.db.QueryRowContext(ctx, `SELECT `+postColumns+
		` FROM posts WHERE owner_id = ? AND id = ?`, ownerID, id))
	if err != nil {
		return Post{}, err
	}
	posts := []Post{post}
	if err := s.hydratePostAttachments(ctx, posts); err != nil {
		return Post{}, err
	}
	return posts[0], nil
}

func (s *Store) getPostWithoutAttachments(ctx context.Context, id int64) (Post, error) {
	return scanPost(s.db.QueryRowContext(ctx, `SELECT `+postColumns+` FROM posts WHERE id = ?`, id))
}

func (s *Store) getPostForUserWithoutAttachments(ctx context.Context, userID string, id int64) (Post, error) {
	return scanPost(s.db.QueryRowContext(ctx, `SELECT `+postColumns+` FROM posts
WHERE owner_id = ? AND id = ? AND workspace_id IN (
SELECT id FROM workspaces WHERE is_personal AND archived_at IS NULL)`, userID, id))
}

func (s *Store) UpdatePost(ctx context.Context, id int64, changes PostChanges) (Post, error) {
	post, err := s.GetPost(ctx, id)
	if err != nil {
		return Post{}, err
	}
	return s.updatePostSnapshot(ctx, post, changes)
}

// UpdatePostIfUnchanged couples validation performed by a caller with the
// exact row revision it validated. A concurrent edit causes ErrConflict.
func (s *Store) UpdatePostIfUnchanged(ctx context.Context, current Post, changes PostChanges) (Post, error) {
	return s.updatePostSnapshot(ctx, current, changes)
}

// updatePostSnapshot applies an edit only while the lifecycle row still
// matches the snapshot that was read. Without this CAS, an autosave that read
// "scheduled" could finish after the worker published the post and overwrite
// the new status/scheduled_at with stale values, making the post publish twice.
func (s *Store) updatePostSnapshot(ctx context.Context, post Post, changes PostChanges) (Post, error) {
	expectedStatus := post.Status
	expectedUpdatedAt := post.UpdatedAt.UTC().Format(time.RFC3339Nano)
	if post.Status == PostStatusPublishing {
		return Post{}, fmt.Errorf("%w: post is currently publishing", ErrConflict)
	}
	if post.Status == PostStatusPublished {
		if changes.ChannelID != nil && !sameInt64Pointer(post.ChannelID, *changes.ChannelID) {
			return Post{}, fmt.Errorf("%w: channel_id cannot change after publication", ErrConflict)
		}
		if changes.DisableLinkPreview != nil && *changes.DisableLinkPreview != post.DisableLinkPreview {
			return Post{}, fmt.Errorf("%w: disable_link_preview cannot change after publication", ErrConflict)
		}
		if changes.ScheduledAt != nil && !sameTimePointer(post.ScheduledAt, *changes.ScheduledAt) {
			return Post{}, fmt.Errorf("%w: scheduled_at cannot change after publication", ErrConflict)
		}
	}
	if changes.Title != nil {
		post.Title = *changes.Title
	}
	if changes.Content != nil {
		post.Content = *changes.Content
	}
	if changes.Format != nil {
		post.Format = *changes.Format
	}
	if changes.ChannelID != nil {
		post.ChannelID = *changes.ChannelID
	}
	if changes.ImageURL != nil {
		post.ImageURL = *changes.ImageURL
	}
	if changes.ImagePath != nil {
		post.ImagePath = *changes.ImagePath
	}
	if changes.ImagePrompt != nil {
		post.ImagePrompt = *changes.ImagePrompt
	}
	if changes.LinkButtons != nil {
		post.LinkButtons = normalizeLinkButtons(append([]LinkButton(nil), (*changes.LinkButtons)...))
		if len(post.LinkButtons) > 0 && len(post.Attachments) > MaxPostAttachmentsWithKeyboard {
			return Post{}, fmt.Errorf("link buttons require no more than %d media attachments", MaxPostAttachmentsWithKeyboard)
		}
	}
	if changes.Notify != nil {
		post.Notify = *changes.Notify
	}
	if changes.DisableLinkPreview != nil {
		post.DisableLinkPreview = *changes.DisableLinkPreview
	}
	if changes.ScheduledAt != nil {
		scheduleChanged := !sameTimePointer(post.ScheduledAt, *changes.ScheduledAt)
		if scheduleChanged {
			switch {
			case *changes.ScheduledAt == nil:
				if post.Status == PostStatusScheduled {
					post.Status = PostStatusDraft
				}
			case post.Status == PostStatusDraft || post.Status == PostStatusFailed || post.Status == PostStatusScheduled:
				if (*changes.ScheduledAt).IsZero() {
					return Post{}, errors.New("scheduled_at must not be zero")
				}
				post.Status = PostStatusScheduled
				post.LastError = ""
			default:
				return Post{}, fmt.Errorf("%w: post cannot be scheduled from its current status", ErrConflict)
			}
		}
		post.ScheduledAt = *changes.ScheduledAt
	}

	linkButtonsJSON, err := marshalLinkButtons(post.LinkButtons)
	if err != nil {
		return Post{}, err
	}
	result, err := s.db.ExecContext(ctx, `
	UPDATE posts SET title = ?, content = ?, format = ?, channel_id = ?, image_url = ?, image_path = ?,
	             image_prompt = ?, link_buttons = ?, notify = ?, disable_link_preview = ?, status = ?, scheduled_at = ?,
	             last_error = ?, updated_at = ?
	WHERE id = ? AND status = ? AND updated_at = ?`, post.Title, post.Content, post.Format, nullableInt64(post.ChannelID), post.ImageURL,
		post.ImagePath, post.ImagePrompt, linkButtonsJSON, post.Notify, post.DisableLinkPreview, post.Status, nullableTime(post.ScheduledAt),
		post.LastError, nowText(), post.ID, expectedStatus, expectedUpdatedAt)
	if err != nil {
		return Post{}, fmt.Errorf("update post: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return Post{}, s.postWriteMiss(ctx, post.ID, "post changed while it was being saved; reload and retry")
	}
	return s.GetPost(ctx, post.ID)
}

func (s *Store) DeletePost(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM posts WHERE id = ? AND status != ? AND max_message_id = ''`, id, PostStatusPublishing)
	if err != nil {
		return fmt.Errorf("delete post: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		post, getErr := s.GetPost(ctx, id)
		if getErr != nil {
			return getErr
		}
		if post.MAXMessageID != "" {
			return ErrPublicationExists
		}
		return fmt.Errorf("%w: post is currently publishing", ErrConflict)
	}
	return nil
}

// DeletePostForUser atomically keeps tenant ownership and MAX publication
// lifecycle checks in the delete predicate. A caller must explicitly remove
// the live MAX publication before deleting the local post.
func (s *Store) DeletePostForUser(ctx context.Context, userID string, id int64) error {
	result, err := s.db.ExecContext(ctx, `
DELETE FROM posts
WHERE owner_id = ? AND id = ? AND status != ? AND max_message_id = ''
AND workspace_id IN (SELECT id FROM workspaces WHERE is_personal AND archived_at IS NULL)`,
		userID, id, PostStatusPublishing)
	if err != nil {
		return fmt.Errorf("delete user post: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 1 {
		return nil
	}
	post, err := s.GetPostForUser(ctx, userID, id)
	if err != nil {
		return err
	}
	if post.MAXMessageID != "" {
		return ErrPublicationExists
	}
	return fmt.Errorf("%w: post is currently publishing", ErrConflict)
}

func (s *Store) DuplicatePost(ctx context.Context, id int64) (Post, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Post{}, fmt.Errorf("begin duplicate post: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := nowText()
	var copyID int64
	err = tx.QueryRowContext(ctx, bindSQL(`
INSERT INTO posts(owner_id, workspace_id, title, content, format, status, channel_id, image_url, image_path, image_prompt, link_buttons,
	              notify, disable_link_preview, scheduled_at, max_message_id, max_message_url, max_views,
	              max_stats_synced_at, max_is_pinned, last_error, published_at, created_at, updated_at)
SELECT owner_id, workspace_id, trim(title || ' (копия)'), content, format, ?, channel_id, image_url, image_path, image_prompt, link_buttons,
	   notify, disable_link_preview, NULL, '', '', NULL, NULL, FALSE, '', NULL, ?, ?
FROM posts WHERE id = ? AND status != ? RETURNING id`), PostStatusDraft, now, now, id, PostStatusPublishing).Scan(&copyID)
	if errors.Is(err, sql.ErrNoRows) {
		return Post{}, s.postWriteMiss(ctx, id, "post is currently publishing")
	}
	if err != nil {
		return Post{}, fmt.Errorf("duplicate post: %w", err)
	}
	if _, err := tx.ExecContext(ctx, bindSQL(`
INSERT INTO post_attachments(owner_id, workspace_id, post_id, type, position, storage_key, processing_status, size_bytes, mime_type,
                             width, height, duration_ms, provider_token, provider_token_expires_at, provider_meta,
                             error_code, created_at, updated_at)
SELECT owner_id, workspace_id, ?, type, position, storage_key, 'ready', size_bytes, mime_type,
       width, height, duration_ms, '', NULL, '{}', '', ?, ?
FROM post_attachments
WHERE post_id = ?
ORDER BY position, id`), copyID, now, now, id); err != nil {
		return Post{}, fmt.Errorf("duplicate post attachments: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Post{}, fmt.Errorf("commit duplicate post: %w", err)
	}
	return s.GetPost(ctx, copyID)
}

func (s *Store) DuplicatePostForUser(ctx context.Context, userID string, id int64) (Post, error) {
	if _, err := s.GetPostForUser(ctx, userID, id); err != nil {
		return Post{}, err
	}
	return s.DuplicatePost(ctx, id)
}

func (s *Store) SetPostScheduled(ctx context.Context, id int64, at time.Time) (Post, error) {
	if at.IsZero() {
		return Post{}, errors.New("scheduled_at must not be zero")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE posts SET status = ?, scheduled_at = ?, last_error = '', updated_at = ?
WHERE id = ? AND status IN (?, ?, ?)`,
		PostStatusScheduled, at.UTC().Format(time.RFC3339Nano), nowText(), id,
		PostStatusDraft, PostStatusFailed, PostStatusScheduled)
	if err != nil {
		return Post{}, fmt.Errorf("schedule post: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		if _, getErr := s.GetPost(ctx, id); getErr != nil {
			return Post{}, getErr
		}
		return Post{}, fmt.Errorf("%w: only draft, failed or scheduled posts can be scheduled", ErrConflict)
	}
	return s.GetPost(ctx, id)
}

// SetPostScheduledIfUnchanged schedules only the exact revision that was
// validated by the application layer. This prevents a concurrent autosave
// from clearing required content/channel between validation and transition.
func (s *Store) SetPostScheduledIfUnchanged(ctx context.Context, current Post, at time.Time) (Post, error) {
	if at.IsZero() {
		return Post{}, errors.New("scheduled_at must not be zero")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE posts SET status = ?, scheduled_at = ?, last_error = '', updated_at = ?
WHERE id = ?
  AND status = ?
  AND updated_at = ?
  AND status IN (?, ?, ?)`,
		PostStatusScheduled, at.UTC().Format(time.RFC3339Nano), nowText(), current.ID,
		current.Status, current.UpdatedAt.UTC().Format(time.RFC3339Nano),
		PostStatusDraft, PostStatusFailed, PostStatusScheduled)
	if err != nil {
		return Post{}, fmt.Errorf("schedule post: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return Post{}, s.postWriteMiss(ctx, current.ID, "post changed before it could be scheduled; reload and retry")
	}
	return s.GetPost(ctx, current.ID)
}

func (s *Store) CancelSchedule(ctx context.Context, id int64) (Post, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE posts SET status = ?, scheduled_at = NULL, updated_at = ? WHERE id = ? AND status = ?`,
		PostStatusDraft, nowText(), id, PostStatusScheduled)
	if err != nil {
		return Post{}, fmt.Errorf("cancel schedule: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		post, getErr := s.GetPost(ctx, id)
		if getErr != nil {
			return Post{}, getErr
		}
		if post.Status == PostStatusDraft && post.ScheduledAt == nil {
			return post, nil
		}
		return Post{}, fmt.Errorf("%w: post is not scheduled", ErrConflict)
	}
	return s.GetPost(ctx, id)
}

func (s *Store) ClaimForPublishing(ctx context.Context, id int64) (Post, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE posts SET status = ?, scheduled_at = NULL, last_error = '', updated_at = ?
WHERE id = ? AND status IN (?, ?, ?)`,
		PostStatusPublishing, nowText(), id, PostStatusDraft, PostStatusScheduled, PostStatusFailed)
	if err != nil {
		return Post{}, fmt.Errorf("claim post: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		if _, getErr := s.GetPost(ctx, id); getErr != nil {
			return Post{}, getErr
		}
		return Post{}, fmt.Errorf("post cannot be published from its current status")
	}
	return s.GetPost(ctx, id)
}

// ClaimPublishedForUpdate is the final compare-and-swap before an edit is
// sent to MAX. Moving the post to publishing blocks concurrent editor and
// attachment writes until the outbound operation is released.
func (s *Store) ClaimPublishedForUpdate(ctx context.Context, current Post) (Post, error) {
	if current.ID <= 0 || current.UserID == "" || current.MAXMessageID == "" {
		return Post{}, fmt.Errorf("%w: published post snapshot is incomplete", ErrConflict)
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, bindSQL(`UPDATE posts
SET status=?, last_error='', updated_at=?
WHERE owner_id=? AND id=? AND status=? AND max_message_id=? AND updated_at=?`),
		PostStatusPublishing, now, current.UserID, current.ID, PostStatusPublished,
		current.MAXMessageID, current.UpdatedAt.UTC())
	if err != nil {
		return Post{}, fmt.Errorf("claim published post update: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return Post{}, s.postWriteMiss(ctx, current.ID, "post changed before its MAX update; reload and retry")
	}
	return s.getPostForOwner(ctx, current.UserID, current.ID)
}

// ReleasePublishedUpdate returns an edit claim to its normal published state.
// The claimed snapshot guards against releasing a different publishing
// operation after a crash or operator intervention.
func (s *Store) ReleasePublishedUpdate(ctx context.Context, claimed Post, lastError string) (Post, error) {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, bindSQL(`UPDATE posts
SET status=?, last_error=?, updated_at=?
WHERE owner_id=? AND id=? AND status=? AND max_message_id=? AND updated_at=?`),
		PostStatusPublished, truncate(lastError, 2000), now, claimed.UserID, claimed.ID,
		PostStatusPublishing, claimed.MAXMessageID, claimed.UpdatedAt.UTC())
	if err != nil {
		return Post{}, fmt.Errorf("release published post update: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return Post{}, s.postWriteMiss(ctx, claimed.ID, "MAX update claim changed before it could be released")
	}
	return s.getPostForOwner(ctx, claimed.UserID, claimed.ID)
}

// ClaimScheduledForPublishing atomically verifies that a scheduled post is
// still due while moving it to publishing. This closes the race where a worker
// lists an ID and the user cancels or postpones it before publication starts.
func (s *Store) ClaimScheduledForPublishing(ctx context.Context, id int64, now time.Time) (Post, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE posts SET status = ?, scheduled_at = NULL, last_error = '', updated_at = ?
WHERE id = ?
  AND status = ?
  AND scheduled_at IS NOT NULL
  AND scheduled_at <= ?
  AND EXISTS(SELECT 1 FROM workspaces w WHERE w.id=posts.workspace_id AND w.archived_at IS NULL)`,
		PostStatusPublishing, nowText(), id, PostStatusScheduled, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return Post{}, fmt.Errorf("claim scheduled post: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		if _, getErr := s.GetPost(ctx, id); getErr != nil {
			return Post{}, getErr
		}
		return Post{}, ErrScheduleNotDue
	}
	return s.GetPost(ctx, id)
}

func (s *Store) DuePostIDs(ctx context.Context, now time.Time, limit int) ([]int64, error) {
	if limit <= 0 {
		return []int64{}, nil
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT p.id FROM posts p JOIN workspaces w ON w.id=p.workspace_id
WHERE p.owner_id <> '' AND w.archived_at IS NULL AND p.status = ? AND p.scheduled_at IS NOT NULL AND p.scheduled_at <= ?
ORDER BY p.scheduled_at, p.id LIMIT ?`, PostStatusScheduled, now.UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, fmt.Errorf("list due posts: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) RecoverStalePublishing(ctx context.Context, staleBefore time.Time) (int64, error) {
	const publishWarning = "Previous publication was interrupted; check the MAX channel before retrying to avoid a duplicate post."
	const editWarning = "Previous MAX update was interrupted; refresh the post before making another change."
	result, err := s.db.ExecContext(ctx, `
UPDATE posts
SET status = CASE WHEN max_message_id <> '' THEN ? ELSE ? END,
    last_error = CASE WHEN max_message_id <> '' THEN ? ELSE ? END,
    scheduled_at = NULL,
    updated_at = ?
WHERE owner_id <> '' AND status = ? AND updated_at < ?`,
		PostStatusPublished, PostStatusFailed, editWarning, publishWarning, nowText(), PostStatusPublishing,
		staleBefore.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("recover stale publishing posts: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count recovered posts: %w", err)
	}
	return count, nil
}

func (s *Store) MarkPublished(ctx context.Context, id int64, messageID, messageURL string) (Post, error) {
	now := nowText()
	result, err := s.db.ExecContext(ctx, `
UPDATE posts SET status = ?, max_message_id = ?, max_message_url = ?, max_views = NULL,
                 max_stats_synced_at = NULL, max_stats_attempted_at = NULL, max_is_pinned = FALSE,
                 last_error = '', scheduled_at = NULL,
                 published_at = ?, updated_at = ? WHERE id = ? AND status = ?`,
		PostStatusPublished, messageID, messageURL, now, now, id, PostStatusPublishing)
	if err != nil {
		return Post{}, fmt.Errorf("mark published: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return Post{}, s.postWriteMiss(ctx, id, "post is no longer publishing")
	}
	return s.GetPost(ctx, id)
}

func (s *Store) MarkPublishFailed(ctx context.Context, id int64, message string) (Post, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE posts SET status = ?, last_error = ?, scheduled_at = NULL, updated_at = ? WHERE id = ? AND status = ?`,
		PostStatusFailed, truncate(message, 2000), nowText(), id, PostStatusPublishing)
	if err != nil {
		return Post{}, fmt.Errorf("mark publish failed: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return Post{}, s.postWriteMiss(ctx, id, "post is no longer publishing")
	}
	return s.GetPost(ctx, id)
}

// RestoreInterruptedSchedule returns a post whose scheduled publication was
// cut short by a canceled context (typically a service stop) to the calendar
// with its original slot. The publishing claim already cleared scheduled_at
// and the failure handler may have marked the post failed before the restore
// runs, so both intermediate states are accepted.
func (s *Store) RestoreInterruptedSchedule(ctx context.Context, id int64, at time.Time) (Post, error) {
	if at.IsZero() {
		return Post{}, errors.New("scheduled_at must not be zero")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE posts SET status = ?, scheduled_at = ?, last_error = '', updated_at = ?
WHERE id = ? AND status IN (?, ?)`,
		PostStatusScheduled, at.UTC().Format(time.RFC3339Nano), nowText(), id,
		PostStatusPublishing, PostStatusFailed)
	if err != nil {
		return Post{}, fmt.Errorf("restore interrupted schedule: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return Post{}, s.postWriteMiss(ctx, id, "post changed before its schedule could be restored")
	}
	return s.GetPost(ctx, id)
}

const postColumns = `id, owner_id, workspace_id, title, content, format, status, channel_id, image_url, image_path, image_prompt, link_buttons,
notify, disable_link_preview, scheduled_at, max_message_id, max_message_url, max_views, max_stats_synced_at,
max_stats_attempted_at, max_is_pinned, last_error, created_at, updated_at, published_at, review_status, current_revision_id`

type scanner interface {
	Scan(dest ...any) error
}

func scanChannel(row scanner) (Channel, error) {
	var channel Channel
	var maxLastEventTime, maxInfoSyncedAt sql.NullTime
	if err := row.Scan(&channel.ID, &channel.UserID, &channel.WorkspaceID, &channel.VerifiedMAXOwnerID, &channel.MAXChatID, &channel.Title,
		&channel.Description, &channel.PublicLink, &channel.IconURL, &channel.ParticipantsCount, &channel.IsPublic,
		&channel.MessagesCount, &channel.HasPinnedMessage, &maxLastEventTime, &maxInfoSyncedAt, &channel.IsChannel, &channel.Active,
		&channel.CreatedAt, &channel.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Channel{}, ErrNotFound
		}
		return Channel{}, fmt.Errorf("scan channel: %w", err)
	}
	channel.CreatedAt = channel.CreatedAt.UTC()
	channel.UpdatedAt = channel.UpdatedAt.UTC()
	channel.MAXLastEventTime = parseNullableTime(maxLastEventTime)
	channel.MAXInfoSyncedAt = parseNullableTime(maxInfoSyncedAt)
	return channel, nil
}

func (s *Store) postWriteMiss(ctx context.Context, id int64, message string) error {
	if _, err := s.GetPost(ctx, id); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s", ErrConflict, message)
}

func scanPost(row scanner) (Post, error) {
	var post Post
	var channelID sql.NullInt64
	var scheduledAt, publishedAt, statsSyncedAt, statsAttemptedAt sql.NullTime
	var maxViews sql.NullInt64
	var currentRevisionID sql.NullInt64
	var linkButtonsJSON []byte
	if err := row.Scan(&post.ID, &post.UserID, &post.WorkspaceID, &post.Title, &post.Content, &post.Format, &post.Status, &channelID,
		&post.ImageURL, &post.ImagePath, &post.ImagePrompt, &linkButtonsJSON, &post.Notify, &post.DisableLinkPreview,
		&scheduledAt, &post.MAXMessageID, &post.MAXMessageURL, &maxViews, &statsSyncedAt, &statsAttemptedAt, &post.MAXIsPinned,
		&post.LastError, &post.CreatedAt, &post.UpdatedAt, &publishedAt, &post.ReviewStatus, &currentRevisionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Post{}, ErrNotFound
		}
		return Post{}, fmt.Errorf("scan post: %w", err)
	}
	if channelID.Valid {
		post.ChannelID = &channelID.Int64
	}
	linkButtons, err := unmarshalLinkButtons(linkButtonsJSON)
	if err != nil {
		return Post{}, fmt.Errorf("scan post: %w", err)
	}
	post.LinkButtons = linkButtons
	post.Attachments = []PostAttachment{}
	post.ScheduledAt = parseNullableTime(scheduledAt)
	if maxViews.Valid {
		post.MAXViews = &maxViews.Int64
	}
	post.MAXStatsSyncedAt = parseNullableTime(statsSyncedAt)
	post.MAXStatsAttemptedAt = parseNullableTime(statsAttemptedAt)
	post.PublishedAt = parseNullableTime(publishedAt)
	if currentRevisionID.Valid {
		post.CurrentRevisionID = &currentRevisionID.Int64
	}
	post.CreatedAt = post.CreatedAt.UTC()
	post.UpdatedAt = post.UpdatedAt.UTC()
	return post, nil
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func parseNullableTime(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	parsed := value.Time.UTC()
	return &parsed
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}

func sameInt64Pointer(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameTimePointer(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}
