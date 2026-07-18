package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrChannelPublicationInProgress protects an in-flight MAX API call from
// losing the channel and local publication state underneath it.
var ErrChannelPublicationInProgress = errors.New("channel publication is in progress")

// DeleteChannelContentForUser removes a personal channel and every local
// content row associated with it. It deliberately does not call MAX: messages
// that were already published remain in the channel itself.
func (s *Store) DeleteChannelContentForUser(ctx context.Context, userID string, channelID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin personal channel content deletion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var workspaceID string
	err = tx.QueryRowContext(ctx, bindSQL(`SELECT workspace_id FROM channels
WHERE owner_id=? AND id=? AND workspace_id IN (
  SELECT id FROM workspaces WHERE is_personal AND archived_at IS NULL
) FOR UPDATE`), userID, channelID).Scan(&workspaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock personal channel for content deletion: %w", err)
	}
	if err := deleteChannelContentTx(ctx, tx, workspaceID, channelID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit personal channel content deletion: %w", err)
	}
	return nil
}

// DeleteChannelContentForWorkspace is the team-workspace equivalent of
// DeleteChannelContentForUser. Authorization is checked before any rows are
// locked or removed.
func (s *Store) DeleteChannelContentForWorkspace(ctx context.Context, actorUserID, workspaceID string, channelID int64) error {
	if err := requireWorkspaceRole(ctx, s.db, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workspace channel content deletion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var lockedID int64
	err = tx.QueryRowContext(ctx, bindSQL(`SELECT id FROM channels
WHERE workspace_id=? AND id=? FOR UPDATE`), workspaceID, channelID).Scan(&lockedID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock workspace channel for content deletion: %w", err)
	}
	if err := deleteChannelContentTx(ctx, tx, workspaceID, channelID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit workspace channel content deletion: %w", err)
	}
	return s.recordWorkspaceAudit(ctx, actorUserID, workspaceID, "channel.deleted", "channel", channelID)
}

// deleteChannelContentTx locks every linked post before checking lifecycle
// state. This closes the race with the scheduler: either the worker has already
// claimed a post and deletion is refused, or the delete commits before a claim
// can start.
func deleteChannelContentTx(ctx context.Context, tx *sql.Tx, workspaceID string, channelID int64) error {
	rows, err := tx.QueryContext(ctx, bindSQL(`SELECT status FROM posts
WHERE workspace_id=? AND channel_id=? FOR UPDATE`), workspaceID, channelID)
	if err != nil {
		return fmt.Errorf("lock channel posts for deletion: %w", err)
	}
	hasPublishing := false
	for rows.Next() {
		var status string
		if err := rows.Scan(&status); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan channel post before deletion: %w", err)
		}
		if status == PostStatusPublishing {
			hasPublishing = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate channel posts before deletion: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close channel post deletion rows: %w", err)
	}
	if hasPublishing {
		return ErrChannelPublicationInProgress
	}

	// Campaign variants use ON DELETE RESTRICT for channels, so remove only the
	// selected channel's variants before posts and the channel itself.
	if _, err := tx.ExecContext(ctx, bindSQL(`DELETE FROM campaign_variants
WHERE workspace_id=? AND channel_id=?`), workspaceID, channelID); err != nil {
		return fmt.Errorf("delete channel campaign variants: %w", err)
	}
	if _, err := tx.ExecContext(ctx, bindSQL(`DELETE FROM posts
WHERE workspace_id=? AND channel_id=?`), workspaceID, channelID); err != nil {
		return fmt.Errorf("delete channel posts: %w", err)
	}
	result, err := tx.ExecContext(ctx, bindSQL(`DELETE FROM channels
WHERE workspace_id=? AND id=?`), workspaceID, channelID)
	if err != nil {
		return fmt.Errorf("delete channel with content: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotFound
	}
	return nil
}
