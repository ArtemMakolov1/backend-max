package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound       = errors.New("not found")
	ErrConflict       = errors.New("state conflict")
	ErrScheduleNotDue = errors.New("scheduled post is not due")
)

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, databasePath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(databasePath), 0o750); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	for _, pragma := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA synchronous = NORMAL",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("configure sqlite (%s): %w", pragma, err)
		}
	}

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS channels (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    max_chat_id TEXT NOT NULL UNIQUE,
    title TEXT NOT NULL,
	public_link TEXT NOT NULL DEFAULT '',
	icon_url TEXT NOT NULL DEFAULT '',
	participants_count INTEGER NOT NULL DEFAULT 0,
    is_channel INTEGER NOT NULL DEFAULT 1,
    active INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS posts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    title TEXT NOT NULL DEFAULT '',
    content TEXT NOT NULL DEFAULT '',
    format TEXT NOT NULL DEFAULT 'markdown' CHECK (format IN ('markdown', 'html')),
    status TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'scheduled', 'publishing', 'published', 'failed')),
    channel_id INTEGER REFERENCES channels(id) ON DELETE SET NULL,
    image_url TEXT NOT NULL DEFAULT '',
    image_path TEXT NOT NULL DEFAULT '',
    image_prompt TEXT NOT NULL DEFAULT '',
    notify INTEGER NOT NULL DEFAULT 1,
    disable_link_preview INTEGER NOT NULL DEFAULT 0,
    scheduled_at TEXT,
    max_message_id TEXT NOT NULL DEFAULT '',
    last_error TEXT NOT NULL DEFAULT '',
    published_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_posts_status_scheduled_at ON posts(status, scheduled_at);
CREATE INDEX IF NOT EXISTS idx_posts_channel_id ON posts(channel_id);

CREATE TABLE IF NOT EXISTS auth_sessions (
    token_hash TEXT PRIMARY KEY
        CHECK (length(token_hash) = 64 AND token_hash NOT GLOB '*[^0-9A-Fa-f]*'),
    yandex_user_id TEXT NOT NULL,
    login TEXT NOT NULL DEFAULT '',
    email TEXT NOT NULL DEFAULT '',
    display_name TEXT NOT NULL DEFAULT '',
    allowlist_identity TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_auth_sessions_expires_at ON auth_sessions(expires_at);

CREATE TABLE IF NOT EXISTS oauth_states (
    state_hash TEXT PRIMARY KEY
        CHECK (length(state_hash) = 64 AND state_hash NOT GLOB '*[^0-9A-Fa-f]*'),
    pkce_verifier TEXT NOT NULL,
    return_to TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_oauth_states_expires_at ON oauth_states(expires_at);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE channels ADD COLUMN public_link TEXT NOT NULL DEFAULT ''`); err != nil &&
		!strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return fmt.Errorf("migrate channels.public_link: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE channels ADD COLUMN icon_url TEXT NOT NULL DEFAULT ''`); err != nil &&
		!strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return fmt.Errorf("migrate channels.icon_url: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE channels ADD COLUMN participants_count INTEGER NOT NULL DEFAULT 0`); err != nil &&
		!strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return fmt.Errorf("migrate channels.participants_count: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE auth_sessions ADD COLUMN allowlist_identity TEXT NOT NULL DEFAULT ''`); err != nil &&
		!strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return fmt.Errorf("migrate auth_sessions.allowlist_identity: %w", err)
	}
	// Older builds allowed scheduled_at to drift away from status through the
	// generic post update endpoint. Repair those legacy rows conservatively:
	// never turn a draft into an automatic publication, and never leave a
	// scheduled row that the worker can never claim.
	if _, err := s.db.ExecContext(ctx, `
UPDATE posts
SET status = ?, scheduled_at = NULL, updated_at = ?
WHERE status = ? AND (scheduled_at IS NULL OR julianday(scheduled_at) IS NULL)`,
		PostStatusDraft, nowText(), PostStatusScheduled); err != nil {
		return fmt.Errorf("repair invalid scheduled posts: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
UPDATE posts
SET scheduled_at = NULL, updated_at = ?
WHERE status != ? AND scheduled_at IS NOT NULL`, nowText(), PostStatusScheduled); err != nil {
		return fmt.Errorf("repair orphan scheduled_at values: %w", err)
	}
	return nil
}

func nowText() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (s *Store) CreateChannel(ctx context.Context, channel Channel) (Channel, error) {
	now := nowText()
	result, err := s.db.ExecContext(ctx, `
INSERT INTO channels(max_chat_id, title, public_link, icon_url, participants_count, is_channel, active, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, channel.MAXChatID, channel.Title, channel.PublicLink, channel.IconURL,
		channel.ParticipantsCount, channel.IsChannel, channel.Active, now, now)
	if err != nil {
		return Channel{}, fmt.Errorf("create channel: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Channel{}, fmt.Errorf("channel id: %w", err)
	}
	return s.GetChannel(ctx, id)
}

func (s *Store) UpsertConnectedChannel(ctx context.Context, channel Channel) (Channel, error) {
	now := nowText()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO channels(max_chat_id, title, public_link, icon_url, participants_count, is_channel, active, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(max_chat_id) DO UPDATE SET
	title = excluded.title,
	public_link = excluded.public_link,
	icon_url = excluded.icon_url,
	participants_count = excluded.participants_count,
	is_channel = excluded.is_channel,
	active = excluded.active,
	updated_at = excluded.updated_at`,
		channel.MAXChatID, channel.Title, channel.PublicLink, channel.IconURL, channel.ParticipantsCount,
		channel.IsChannel, channel.Active, now, now)
	if err != nil {
		return Channel{}, fmt.Errorf("connect channel: %w", err)
	}
	return s.GetChannelByMAXChatID(ctx, channel.MAXChatID)
}

func (s *Store) UpsertDiscoveredChannel(ctx context.Context, maxChatID, title string, isChannel bool, active bool) (Channel, error) {
	providedTitle := strings.TrimSpace(title)
	if providedTitle == "" {
		title = "MAX " + maxChatID
	} else {
		title = providedTitle
	}
	now := nowText()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO channels(max_chat_id, title, public_link, icon_url, participants_count, is_channel, active, created_at, updated_at)
VALUES (?, ?, '', '', 0, ?, ?, ?, ?)
ON CONFLICT(max_chat_id) DO UPDATE SET
    title = CASE WHEN ? = '' THEN channels.title ELSE excluded.title END,
    is_channel = excluded.is_channel,
    active = excluded.active,
		updated_at = excluded.updated_at`, maxChatID, title, isChannel, active, now, now, providedTitle)
	if err != nil {
		return Channel{}, fmt.Errorf("upsert discovered channel: %w", err)
	}
	return s.GetChannelByMAXChatID(ctx, maxChatID)
}

func (s *Store) ListChannels(ctx context.Context) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, max_chat_id, title, public_link, icon_url, participants_count, is_channel, active, created_at, updated_at
FROM channels ORDER BY active DESC, title COLLATE NOCASE, id`)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	defer rows.Close()

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

func (s *Store) GetChannel(ctx context.Context, id int64) (Channel, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, max_chat_id, title, public_link, icon_url, participants_count, is_channel, active, created_at, updated_at
FROM channels WHERE id = ?`, id)
	return scanChannel(row)
}

func (s *Store) GetChannelByMAXChatID(ctx context.Context, maxChatID string) (Channel, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, max_chat_id, title, public_link, icon_url, participants_count, is_channel, active, created_at, updated_at
FROM channels WHERE max_chat_id = ?`, maxChatID)
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
UPDATE channels SET max_chat_id = ?, title = ?, public_link = ?, icon_url = ?, participants_count = ?, active = ?, updated_at = ? WHERE id = ?`,
		current.MAXChatID, current.Title, current.PublicLink, current.IconURL, current.ParticipantsCount,
		current.Active, nowText(), id)
	if err != nil {
		return Channel{}, fmt.Errorf("update channel: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return Channel{}, ErrNotFound
	}
	return s.GetChannel(ctx, id)
}

func (s *Store) DeleteChannel(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, `
DELETE FROM channels
WHERE id = ?
  AND NOT EXISTS (
	SELECT 1 FROM posts
	WHERE channel_id = ?
	  AND (max_message_id != '' OR status IN (?, ?, ?))
  )`, id, id, PostStatusScheduled, PostStatusPublishing, PostStatusPublished)
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
	return fmt.Errorf("%w: channel has %d scheduled, publishing, or published post(s)", ErrConflict, count)
}

// CountChannelBlockingPosts reports posts whose MAX publication lifecycle
// would become unmanageable if the channel foreign key were cleared.
func (s *Store) CountChannelBlockingPosts(ctx context.Context, id int64) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM posts
WHERE channel_id = ?
  AND (max_message_id != '' OR status IN (?, ?, ?))`,
		id, PostStatusScheduled, PostStatusPublishing, PostStatusPublished).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count channel publication dependencies: %w", err)
	}
	return count, nil
}

func (s *Store) CreatePost(ctx context.Context, post Post) (Post, error) {
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
	now := nowText()
	result, err := s.db.ExecContext(ctx, `
INSERT INTO posts(title, content, format, status, channel_id, image_url, image_path, image_prompt,
                  notify, disable_link_preview, scheduled_at, max_message_id, last_error, published_at,
                  created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		post.Title, post.Content, post.Format, post.Status, nullableInt64(post.ChannelID), post.ImageURL,
		post.ImagePath, post.ImagePrompt, post.Notify, post.DisableLinkPreview, nullableTime(post.ScheduledAt),
		post.MAXMessageID, post.LastError, nullableTime(post.PublishedAt), now, now)
	if err != nil {
		return Post{}, fmt.Errorf("create post: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Post{}, fmt.Errorf("post id: %w", err)
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
		query += ` ORDER BY julianday(scheduled_at), id`
	} else {
		query += ` ORDER BY created_at DESC, id DESC`
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list posts: %w", err)
	}
	defer rows.Close()

	posts := make([]Post, 0)
	for rows.Next() {
		post, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		posts = append(posts, post)
	}
	return posts, rows.Err()
}

func (s *Store) GetPost(ctx context.Context, id int64) (Post, error) {
	return scanPost(s.db.QueryRowContext(ctx, `SELECT `+postColumns+` FROM posts WHERE id = ?`, id))
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

	result, err := s.db.ExecContext(ctx, `
	UPDATE posts SET title = ?, content = ?, format = ?, channel_id = ?, image_url = ?, image_path = ?,
	             image_prompt = ?, notify = ?, disable_link_preview = ?, status = ?, scheduled_at = ?,
	             last_error = ?, updated_at = ?
	WHERE id = ? AND status = ? AND updated_at = ?`, post.Title, post.Content, post.Format, nullableInt64(post.ChannelID), post.ImageURL,
		post.ImagePath, post.ImagePrompt, post.Notify, post.DisableLinkPreview, post.Status, nullableTime(post.ScheduledAt),
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
	result, err := s.db.ExecContext(ctx, `DELETE FROM posts WHERE id = ? AND status != ?`, id, PostStatusPublishing)
	if err != nil {
		return fmt.Errorf("delete post: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return s.postWriteMiss(ctx, id, "post is currently publishing")
	}
	return nil
}

func (s *Store) DuplicatePost(ctx context.Context, id int64) (Post, error) {
	now := nowText()
	result, err := s.db.ExecContext(ctx, `
INSERT INTO posts(title, content, format, status, channel_id, image_url, image_path, image_prompt,
	              notify, disable_link_preview, scheduled_at, max_message_id, last_error, published_at,
	              created_at, updated_at)
SELECT trim(title || ' (копия)'), content, format, ?, channel_id, image_url, image_path, image_prompt,
	   notify, disable_link_preview, NULL, '', '', NULL, ?, ?
FROM posts WHERE id = ? AND status != ?`, PostStatusDraft, now, now, id, PostStatusPublishing)
	if err != nil {
		return Post{}, fmt.Errorf("duplicate post: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return Post{}, s.postWriteMiss(ctx, id, "post is currently publishing")
	}
	copyID, err := result.LastInsertId()
	if err != nil {
		return Post{}, fmt.Errorf("duplicate post id: %w", err)
	}
	return s.GetPost(ctx, copyID)
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
UPDATE posts SET status = ?, last_error = '', updated_at = ?
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

// ClaimScheduledForPublishing atomically verifies that a scheduled post is
// still due while moving it to publishing. This closes the race where a worker
// lists an ID and the user cancels or postpones it before publication starts.
func (s *Store) ClaimScheduledForPublishing(ctx context.Context, id int64, now time.Time) (Post, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE posts SET status = ?, last_error = '', updated_at = ?
WHERE id = ?
  AND status = ?
  AND scheduled_at IS NOT NULL
  AND julianday(scheduled_at) <= julianday(?)`,
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
SELECT id FROM posts
WHERE status = ? AND scheduled_at IS NOT NULL AND julianday(scheduled_at) <= julianday(?)
ORDER BY julianday(scheduled_at), id LIMIT ?`, PostStatusScheduled, now.UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, fmt.Errorf("list due posts: %w", err)
	}
	defer rows.Close()
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
	const warning = "Previous publication was interrupted; check the MAX channel before retrying to avoid a duplicate post."
	result, err := s.db.ExecContext(ctx, `
UPDATE posts SET status = ?, last_error = ?, scheduled_at = NULL, updated_at = ?
WHERE status = ? AND julianday(updated_at) < julianday(?)`, PostStatusFailed, warning, nowText(), PostStatusPublishing,
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

func (s *Store) MarkPublished(ctx context.Context, id int64, messageID string) (Post, error) {
	now := nowText()
	result, err := s.db.ExecContext(ctx, `
UPDATE posts SET status = ?, max_message_id = ?, last_error = '', scheduled_at = NULL,
                 published_at = ?, updated_at = ? WHERE id = ? AND status = ?`,
		PostStatusPublished, messageID, now, now, id, PostStatusPublishing)
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

func (s *Store) ClearPublication(ctx context.Context, id int64) (Post, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE posts SET status = ?, max_message_id = '', published_at = NULL, last_error = '', updated_at = ?
WHERE id = ? AND status != ?`, PostStatusDraft, nowText(), id, PostStatusPublishing)
	if err != nil {
		return Post{}, fmt.Errorf("clear publication: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return Post{}, s.postWriteMiss(ctx, id, "post is currently publishing")
	}
	return s.GetPost(ctx, id)
}

const postColumns = `id, title, content, format, status, channel_id, image_url, image_path, image_prompt,
notify, disable_link_preview, scheduled_at, max_message_id, last_error, created_at, updated_at, published_at`

type scanner interface {
	Scan(dest ...any) error
}

func scanChannel(row scanner) (Channel, error) {
	var channel Channel
	var createdAt, updatedAt string
	if err := row.Scan(&channel.ID, &channel.MAXChatID, &channel.Title, &channel.PublicLink, &channel.IconURL,
		&channel.ParticipantsCount, &channel.IsChannel, &channel.Active, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Channel{}, ErrNotFound
		}
		return Channel{}, fmt.Errorf("scan channel: %w", err)
	}
	channel.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	channel.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
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
	var scheduledAt, publishedAt sql.NullString
	var createdAt, updatedAt string
	if err := row.Scan(&post.ID, &post.Title, &post.Content, &post.Format, &post.Status, &channelID,
		&post.ImageURL, &post.ImagePath, &post.ImagePrompt, &post.Notify, &post.DisableLinkPreview,
		&scheduledAt, &post.MAXMessageID, &post.LastError, &createdAt, &updatedAt, &publishedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Post{}, ErrNotFound
		}
		return Post{}, fmt.Errorf("scan post: %w", err)
	}
	if channelID.Valid {
		post.ChannelID = &channelID.Int64
	}
	post.ScheduledAt = parseNullableTime(scheduledAt)
	post.PublishedAt = parseNullableTime(publishedAt)
	post.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	post.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
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

func parseNullableTime(value sql.NullString) *time.Time {
	if !value.Valid || value.String == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return nil
	}
	parsed = parsed.UTC()
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
