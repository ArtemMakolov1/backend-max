package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PostViewSnapshot is one immutable observation of a published MAX post's
// view counter. Keeping observations separate from Post.MAXViews makes later
// reports and growth charts possible without changing the current-post API.
type PostViewSnapshot struct {
	ID           int64     `json:"id"`
	PostID       int64     `json:"post_id"`
	MAXMessageID string    `json:"max_message_id"`
	Views        int64     `json:"views"`
	CapturedAt   time.Time `json:"captured_at"`
}

// ListPostsDueForStats selects a small cross-tenant worker batch. Every
// returned row still carries its owner ID, and SyncMAXPublication performs the
// tenant/channel/message checks again before any upstream or database write.
func (s *Store) ListPostsDueForStats(ctx context.Context, now time.Time, syncInterval time.Duration, limit int) ([]Post, error) {
	if now.IsZero() || syncInterval <= 0 {
		return nil, errors.New("stats time and positive sync interval are required")
	}
	if limit <= 0 {
		return []Post{}, nil
	}
	if limit > 100 {
		limit = 100
	}
	cutoff := now.UTC().Add(-syncInterval)
	rows, err := s.db.QueryContext(ctx, `
SELECT `+postColumns+` FROM posts
WHERE owner_id <> ''
  AND status = ?
  AND max_message_id <> ''
  AND channel_id IS NOT NULL
  AND COALESCE(max_stats_attempted_at, max_stats_synced_at, published_at, created_at) <= ?
ORDER BY COALESCE(max_stats_attempted_at, max_stats_synced_at, published_at, created_at), id
LIMIT ?`, PostStatusPublished, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("list posts due for MAX stats: %w", err)
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
		return nil, fmt.Errorf("list posts due for MAX stats: %w", err)
	}
	return posts, nil
}

// ClaimPostStatsAttemptForUser atomically records a worker attempt only when
// the publication is still due. This both prevents retry storms after an
// upstream failure and collapses races between multiple scheduler instances.
func (s *Store) ClaimPostStatsAttemptForUser(ctx context.Context, userID string, postID, channelID int64,
	expectedMessageID string, attemptedAt time.Time, syncInterval time.Duration,
) (bool, error) {
	if err := validatePublicationMutation(userID, postID, channelID, expectedMessageID); err != nil {
		return false, err
	}
	if attemptedAt.IsZero() || syncInterval <= 0 {
		return false, errors.New("stats attempt time and positive sync interval are required")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE posts SET max_stats_attempted_at = ?
WHERE owner_id = ? AND id = ? AND channel_id = ? AND max_message_id = ? AND status = ?
  AND (max_stats_attempted_at IS NULL OR max_stats_attempted_at <= ?)`,
		attemptedAt.UTC(), userID, postID, channelID, expectedMessageID, PostStatusPublished,
		attemptedAt.UTC().Add(-syncInterval))
	if err != nil {
		return false, fmt.Errorf("claim MAX stats attempt: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 1 {
		return true, nil
	}
	if _, err := s.GetPostForUser(ctx, userID, postID); err != nil {
		return false, err
	}
	return false, nil
}

// SyncPublicationMetadataForUser atomically updates the latest metadata,
// reconciles the single pinned post for the tenant/channel, and appends a view
// observation. The expected MAX identifiers prevent a concurrent delete or
// republish from attaching stale metadata to a different publication.
func (s *Store) SyncPublicationMetadataForUser(ctx context.Context, userID string, postID, channelID int64,
	expectedMessageID, messageURL string, views *int64, syncedAt time.Time, pinned bool,
) (Post, error) {
	if err := validatePublicationMutation(userID, postID, channelID, expectedMessageID); err != nil {
		return Post{}, err
	}
	if syncedAt.IsZero() {
		return Post{}, errors.New("publication metadata sync time is required")
	}
	if views != nil && *views < 0 {
		return Post{}, errors.New("MAX view count must not be negative")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Post{}, fmt.Errorf("begin publication metadata sync: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockPublishedPost(tx, ctx, userID, postID, channelID, expectedMessageID); err != nil {
		return Post{}, err
	}
	if pinned {
		if _, err := tx.ExecContext(ctx, bindSQL(`
UPDATE posts SET max_is_pinned = FALSE
WHERE owner_id = ? AND channel_id = ? AND id != ? AND max_is_pinned`), userID, channelID, postID); err != nil {
			return Post{}, fmt.Errorf("clear previous pinned post: %w", err)
		}
	}
	result, err := tx.ExecContext(ctx, bindSQL(`
UPDATE posts
SET max_message_url = ?, max_views = ?, max_stats_synced_at = ?, max_stats_attempted_at = ?, max_is_pinned = ?
WHERE owner_id = ? AND id = ? AND channel_id = ? AND max_message_id = ? AND status = ?`),
		strings.TrimSpace(messageURL), nullableInt64(views), syncedAt.UTC(), syncedAt.UTC(), pinned,
		userID, postID, channelID, expectedMessageID, PostStatusPublished)
	if err != nil {
		return Post{}, fmt.Errorf("sync publication metadata: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return Post{}, fmt.Errorf("%w: MAX publication changed while metadata was being synchronized", ErrConflict)
	}
	if views != nil {
		if _, err := tx.ExecContext(ctx, bindSQL(`
INSERT INTO post_view_snapshots(owner_id, post_id, max_message_id, views, captured_at) VALUES (?, ?, ?, ?, ?)`),
			userID, postID, expectedMessageID, *views, syncedAt.UTC()); err != nil {
			return Post{}, fmt.Errorf("append MAX view snapshot: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Post{}, fmt.Errorf("commit publication metadata sync: %w", err)
	}
	return s.GetPostForUser(ctx, userID, postID)
}

// MarkMAXPublicationMissingForUser reconciles a post that was published by
// MaxPosty but has since been deleted directly in MAX. The original
// published_at, the last observed view count, and view snapshots remain
// historical facts; live MAX identifiers are cleared and the failed lifecycle
// state permits publishing the post again.
// expectedMessageID makes a delayed 404 harmless if the post was concurrently
// republished with a different MAX message.
func (s *Store) MarkMAXPublicationMissingForUser(ctx context.Context, userID string, postID, channelID int64,
	expectedMessageID string,
) (Post, error) {
	if err := validatePublicationMutation(userID, postID, channelID, expectedMessageID); err != nil {
		return Post{}, err
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE posts
SET status = ?, max_message_id = '', max_message_url = '',
    max_stats_attempted_at = NULL, max_is_pinned = FALSE,
    scheduled_at = NULL, last_error = ?, updated_at = ?
WHERE owner_id = ? AND id = ? AND channel_id = ? AND max_message_id = ? AND status = ?`,
		PostStatusFailed, MAXPublicationMissingLastError, nowText(), userID, postID, channelID,
		expectedMessageID, PostStatusPublished)
	if err != nil {
		return Post{}, fmt.Errorf("mark missing MAX publication: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 1 {
		return s.GetPostForUser(ctx, userID, postID)
	}
	current, getErr := s.GetPostForUser(ctx, userID, postID)
	if getErr != nil {
		return Post{}, getErr
	}
	if current.Status == PostStatusFailed && current.LastError == MAXPublicationMissingLastError &&
		current.MAXMessageID == "" {
		return current, nil
	}
	return Post{}, fmt.Errorf("%w: MAX publication changed while its deletion was being reconciled", ErrConflict)
}

// SetPublicationPinnedForUser reconciles the local pin flags after a
// successful MAX mutation. MAX permits one pin per chat, so setting true also
// clears any older flag for this tenant and channel in the same transaction.
func (s *Store) SetPublicationPinnedForUser(ctx context.Context, userID string, postID, channelID int64,
	expectedMessageID string, pinned bool,
) (Post, error) {
	if err := validatePublicationMutation(userID, postID, channelID, expectedMessageID); err != nil {
		return Post{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Post{}, fmt.Errorf("begin publication pin update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockPublishedPost(tx, ctx, userID, postID, channelID, expectedMessageID); err != nil {
		return Post{}, err
	}
	if pinned {
		if _, err := tx.ExecContext(ctx, bindSQL(`
UPDATE posts SET max_is_pinned = FALSE
WHERE owner_id = ? AND channel_id = ? AND id != ? AND max_is_pinned`), userID, channelID, postID); err != nil {
			return Post{}, fmt.Errorf("clear previous pinned post: %w", err)
		}
	}
	result, err := tx.ExecContext(ctx, bindSQL(`
UPDATE posts SET max_is_pinned = ?
WHERE owner_id = ? AND id = ? AND channel_id = ? AND max_message_id = ? AND status = ?`),
		pinned, userID, postID, channelID, expectedMessageID, PostStatusPublished)
	if err != nil {
		return Post{}, fmt.Errorf("update publication pin: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return Post{}, fmt.Errorf("%w: MAX publication changed while its pin was being updated", ErrConflict)
	}
	if err := tx.Commit(); err != nil {
		return Post{}, fmt.Errorf("commit publication pin update: %w", err)
	}
	return s.GetPostForUser(ctx, userID, postID)
}

func (s *Store) ListPostViewSnapshotsForUser(ctx context.Context, userID string, postID int64, before *time.Time, limit int) ([]PostViewSnapshot, error) {
	if strings.TrimSpace(userID) == "" || postID <= 0 {
		return nil, errors.New("post owner and positive post ID are required")
	}
	if limit <= 0 || limit > 1000 {
		return nil, errors.New("view history limit must be between 1 and 1000")
	}
	if before != nil && before.IsZero() {
		return nil, errors.New("view history before timestamp must not be zero")
	}
	if _, err := s.GetPostForUser(ctx, userID, postID); err != nil {
		return nil, err
	}
	query := `
SELECT id, post_id, max_message_id, views, captured_at
FROM post_view_snapshots
WHERE owner_id = ? AND post_id = ?`
	args := []any{userID, postID}
	if before != nil {
		query += ` AND captured_at < ?`
		args = append(args, before.UTC())
	}
	query += ` ORDER BY captured_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list MAX view snapshots: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]PostViewSnapshot, 0)
	for rows.Next() {
		var snapshot PostViewSnapshot
		if err := rows.Scan(&snapshot.ID, &snapshot.PostID, &snapshot.MAXMessageID, &snapshot.Views, &snapshot.CapturedAt); err != nil {
			return nil, fmt.Errorf("scan MAX view snapshot: %w", err)
		}
		snapshot.CapturedAt = snapshot.CapturedAt.UTC()
		result = append(result, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list MAX view snapshots: %w", err)
	}
	return result, nil
}

func validatePublicationMutation(userID string, postID, channelID int64, messageID string) error {
	if strings.TrimSpace(userID) == "" || postID <= 0 || channelID <= 0 || strings.TrimSpace(messageID) == "" {
		return errors.New("post owner, post, channel and MAX message are required")
	}
	return nil
}

func lockPublishedPost(tx *sql.Tx, ctx context.Context, userID string, postID, channelID int64, messageID string) error {
	var foundID int64
	err := tx.QueryRowContext(ctx, bindSQL(`
SELECT id FROM posts
WHERE owner_id = ? AND id = ? AND channel_id = ? AND max_message_id = ? AND status = ?
FOR UPDATE`), userID, postID, channelID, messageID, PostStatusPublished).Scan(&foundID)
	if errors.Is(err, sql.ErrNoRows) {
		var ownedID int64
		if lookupErr := tx.QueryRowContext(ctx, bindSQL(`SELECT id FROM posts WHERE owner_id = ? AND id = ?`), userID, postID).Scan(&ownedID); errors.Is(lookupErr, sql.ErrNoRows) {
			return ErrNotFound
		} else if lookupErr != nil {
			return fmt.Errorf("check publication owner: %w", lookupErr)
		}
		return fmt.Errorf("%w: post has no matching active MAX publication", ErrConflict)
	}
	if err != nil {
		return fmt.Errorf("lock MAX publication: %w", err)
	}
	return nil
}
