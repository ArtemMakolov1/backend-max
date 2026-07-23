package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ConnectDiscoverableChannelForWorkspace persists a channel only after the
// application has verified MAX inventory and identity. The global MAX chat
// uniqueness rule never permits taking a channel from another compat owner.
func (s *Store) ConnectDiscoverableChannelForWorkspace(ctx context.Context, actorUserID, workspaceID, maxChatID string, channel Channel) (Channel, error) {
	access, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID)
	if err != nil {
		return Channel{}, err
	}
	if access.Member.Role != WorkspaceRoleOwner && access.Member.Role != WorkspaceRoleEditor {
		return Channel{}, ErrNotFound
	}
	maxChatID = strings.TrimSpace(maxChatID)
	if maxChatID == "" || channel.VerifiedMAXOwnerID == "" {
		return Channel{}, errors.New("MAX chat and verified owner are required")
	}
	channel.MAXChatID = maxChatID
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Channel{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var id int64
	var ownerID, existingWorkspace string
	lookupErr := tx.QueryRowContext(ctx, `SELECT id,owner_id,workspace_id FROM channels
WHERE max_chat_id=$1 FOR UPDATE`, maxChatID).Scan(&id, &ownerID, &existingWorkspace)
	now := nowText()
	switch {
	case lookupErr == nil && ownerID != access.Workspace.CompatOwnerUserID:
		return Channel{}, ErrChannelOwned
	case lookupErr == nil:
		if existingWorkspace != workspaceID {
			var linked int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM posts WHERE channel_id=$1`, id).Scan(&linked); err != nil {
				return Channel{}, err
			}
			if linked != 0 {
				return Channel{}, fmt.Errorf("%w: channel has linked posts", ErrConflict)
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE channels SET workspace_id=$1,verified_max_owner_id=$2,title=$3,
description=$4,public_link=$5,icon_url=$6,participants_count=$7,is_public=$8,messages_count=$9,
has_pinned_message=$10,max_last_event_time=$11,max_info_synced_at=$12,is_channel=$13,active=TRUE,
updated_at=$14 WHERE id=$15`,
			workspaceID, channel.VerifiedMAXOwnerID, channel.Title, channel.Description, channel.PublicLink,
			channel.IconURL, channel.ParticipantsCount, channel.IsPublic, channel.MessagesCount,
			channel.HasPinnedMessage, channel.MAXLastEventTime, channel.MAXInfoSyncedAt, channel.IsChannel, now, id)
	case errors.Is(lookupErr, sql.ErrNoRows):
		err = tx.QueryRowContext(ctx, `INSERT INTO channels(owner_id,workspace_id,verified_max_owner_id,
max_chat_id,title,description,public_link,icon_url,participants_count,is_public,messages_count,has_pinned_message,
max_last_event_time,max_info_synced_at,is_channel,active,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,TRUE,$16,$16) RETURNING id`,
			access.Workspace.CompatOwnerUserID, workspaceID, channel.VerifiedMAXOwnerID, maxChatID,
			channel.Title, channel.Description, channel.PublicLink, channel.IconURL, channel.ParticipantsCount,
			channel.IsPublic, channel.MessagesCount, channel.HasPinnedMessage, channel.MAXLastEventTime,
			channel.MAXInfoSyncedAt, channel.IsChannel, now).Scan(&id)
	default:
		return Channel{}, lookupErr
	}
	if err != nil {
		return Channel{}, mapWorkspaceWriteError("connect workspace channel", err)
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "channel.connected",
		EntityType: "channel", EntityID: fmt.Sprint(id),
		Metadata: mustJSON(map[string]any{"max_chat_id": maxChatID}),
	}); err != nil {
		return Channel{}, err
	}
	if err := tx.Commit(); err != nil {
		return Channel{}, err
	}
	return s.GetChannelForWorkspace(ctx, actorUserID, workspaceID, id)
}

func (s *Store) ListPostsForWorkspace(ctx context.Context, actorUserID, workspaceID, status string, channelID *int64) ([]Post, error) {
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID); err != nil {
		return nil, err
	}
	query := `SELECT ` + postColumns + ` FROM posts WHERE workspace_id=?`
	args := []any{workspaceID}
	if status != "" {
		query += ` AND status=?`
		args = append(args, status)
	}
	if channelID != nil {
		query += ` AND channel_id=?`
		args = append(args, *channelID)
	}
	if status == PostStatusScheduled {
		query += ` ORDER BY scheduled_at,id`
	} else {
		query += ` ORDER BY created_at DESC,id DESC`
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list workspace posts: %w", err)
	}
	defer func() { _ = rows.Close() }()
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

// ListRecentPublishedPostContentsForWorkspace is a narrow AI-context query.
// It deliberately returns no post metadata, media, attachments, or drafts and
// enforces both workspace and channel scope in SQL.
func (s *Store) ListRecentPublishedPostContentsForWorkspace(
	ctx context.Context, actorUserID, workspaceID string, channelID int64,
) ([]string, error) {
	if channelID <= 0 {
		return nil, ErrNotFound
	}
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT content
FROM posts
WHERE workspace_id=? AND channel_id=? AND status=? AND content ~ '[^[:space:]]'
ORDER BY published_at DESC NULLS LAST,id DESC
LIMIT 10`, workspaceID, channelID, PostStatusPublished)
	if err != nil {
		return nil, fmt.Errorf("list recent published workspace post content: %w", err)
	}
	defer func() { _ = rows.Close() }()
	contents := make([]string, 0, 10)
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			return nil, err
		}
		contents = append(contents, content)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return contents, nil
}

func (s *Store) GetPostForWorkspace(ctx context.Context, actorUserID, workspaceID string, postID int64) (Post, error) {
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID); err != nil {
		return Post{}, err
	}
	post, err := scanPost(s.db.QueryRowContext(ctx, `SELECT `+postColumns+`
FROM posts WHERE workspace_id=? AND id=?`, workspaceID, postID))
	if err != nil {
		return Post{}, err
	}
	posts := []Post{post}
	if err := s.hydratePostAttachments(ctx, posts); err != nil {
		return Post{}, err
	}
	return posts[0], nil
}

func (s *Store) CreatePostForWorkspace(ctx context.Context, actorUserID, workspaceID string, post Post) (Post, error) {
	access, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID)
	if err != nil {
		return Post{}, err
	}
	if access.Member.Role != WorkspaceRoleOwner && access.Member.Role != WorkspaceRoleEditor {
		return Post{}, ErrNotFound
	}
	if post.ChannelID != nil {
		if _, err := scanChannel(s.db.QueryRowContext(ctx, `SELECT `+channelColumns+`
FROM channels WHERE workspace_id=? AND id=?`, workspaceID, *post.ChannelID)); err != nil {
			return Post{}, err
		}
	}
	post.UserID = access.Workspace.CompatOwnerUserID
	post.WorkspaceID = workspaceID
	created, err := s.CreatePost(ctx, post)
	if err != nil {
		return Post{}, err
	}
	if _, err := s.CreatePostRevision(ctx, actorUserID, workspaceID, created.ID); err != nil {
		// The post is usable even if revision creation raced with workspace
		// archival; avoid deleting user content as compensation.
		return Post{}, fmt.Errorf("initialize post revision: %w", err)
	}
	if err := s.recordWorkspaceAudit(ctx, actorUserID, workspaceID, "post.created", "post", created.ID); err != nil {
		return Post{}, err
	}
	return s.GetPostForWorkspace(ctx, actorUserID, workspaceID, created.ID)
}

func (s *Store) UpdatePostForWorkspaceIfUnchanged(ctx context.Context, actorUserID, workspaceID string, current Post, changes PostChanges) (Post, error) {
	if err := requireWorkspaceRole(ctx, s.db, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return Post{}, err
	}
	if current.WorkspaceID != workspaceID {
		return Post{}, ErrNotFound
	}
	if changes.ChannelID != nil && *changes.ChannelID != nil {
		if _, err := scanChannel(s.db.QueryRowContext(ctx, `SELECT `+channelColumns+`
FROM channels WHERE workspace_id=? AND id=?`, workspaceID, **changes.ChannelID)); err != nil {
			return Post{}, err
		}
	}
	updated, err := s.updatePostSnapshot(ctx, current, changes)
	if err != nil {
		return Post{}, err
	}
	if updated.WorkspaceID != workspaceID {
		return Post{}, ErrNotFound
	}
	if err := s.recordWorkspaceAudit(ctx, actorUserID, workspaceID, "post.updated", "post", updated.ID); err != nil {
		return Post{}, err
	}
	return updated, nil
}

func (s *Store) DeletePostForWorkspace(ctx context.Context, actorUserID, workspaceID string, postID int64) error {
	if err := requireWorkspaceRole(ctx, s.db, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM posts
WHERE workspace_id=? AND id=? AND status!=? AND max_message_id=''`, workspaceID, postID, PostStatusPublishing)
	if err != nil {
		return fmt.Errorf("delete workspace post: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 1 {
		return s.recordWorkspaceAudit(ctx, actorUserID, workspaceID, "post.deleted", "post", postID)
	}
	post, err := s.GetPostForWorkspace(ctx, actorUserID, workspaceID, postID)
	if err != nil {
		return err
	}
	if post.MAXMessageID != "" {
		return ErrPublicationExists
	}
	return ErrConflict
}

func (s *Store) DuplicatePostForWorkspace(ctx context.Context, actorUserID, workspaceID string, postID int64) (Post, error) {
	if err := requireWorkspaceRole(ctx, s.db, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return Post{}, err
	}
	if _, err := s.GetPostForWorkspace(ctx, actorUserID, workspaceID, postID); err != nil {
		return Post{}, err
	}
	duplicate, err := s.DuplicatePost(ctx, postID)
	if err != nil {
		return Post{}, err
	}
	if duplicate.WorkspaceID != workspaceID {
		return Post{}, ErrNotFound
	}
	if _, err := s.CreatePostRevision(ctx, actorUserID, workspaceID, duplicate.ID); err != nil {
		return Post{}, fmt.Errorf("initialize duplicate revision: %w", err)
	}
	if err := s.recordWorkspaceAudit(ctx, actorUserID, workspaceID, "post.duplicated", "post", duplicate.ID); err != nil {
		return Post{}, err
	}
	return s.GetPostForWorkspace(ctx, actorUserID, workspaceID, duplicate.ID)
}

func (s *Store) ListChannelsForWorkspace(ctx context.Context, actorUserID, workspaceID string) ([]Channel, error) {
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+channelColumns+`
FROM channels WHERE workspace_id=? ORDER BY active DESC,lower(title),id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
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

func (s *Store) GetChannelForWorkspace(ctx context.Context, actorUserID, workspaceID string, channelID int64) (Channel, error) {
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID); err != nil {
		return Channel{}, err
	}
	return scanChannel(s.db.QueryRowContext(ctx, `SELECT `+channelColumns+`
FROM channels WHERE workspace_id=? AND id=?`, workspaceID, channelID))
}

func (s *Store) CreateChannelForWorkspace(ctx context.Context, actorUserID, workspaceID string, channel Channel) (Channel, error) {
	access, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID)
	if err != nil {
		return Channel{}, err
	}
	if access.Member.Role != WorkspaceRoleOwner && access.Member.Role != WorkspaceRoleEditor {
		return Channel{}, ErrNotFound
	}
	channel.UserID = access.Workspace.CompatOwnerUserID
	channel.WorkspaceID = workspaceID
	created, err := s.CreateChannel(ctx, channel)
	if err != nil {
		return Channel{}, err
	}
	if err := s.recordWorkspaceAudit(ctx, actorUserID, workspaceID, "channel.created", "channel", created.ID); err != nil {
		return Channel{}, err
	}
	return s.GetChannelForWorkspace(ctx, actorUserID, workspaceID, created.ID)
}

func (s *Store) UpdateChannelForWorkspace(ctx context.Context, actorUserID, workspaceID string, channelID int64, title *string, active *bool) (Channel, error) {
	if err := requireWorkspaceRole(ctx, s.db, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return Channel{}, err
	}
	current, err := s.GetChannelForWorkspace(ctx, actorUserID, workspaceID, channelID)
	if err != nil {
		return Channel{}, err
	}
	if title != nil {
		current.Title = strings.TrimSpace(*title)
		if current.Title == "" {
			return Channel{}, errors.New("channel title is required")
		}
	}
	if active != nil {
		current.Active = *active
	}
	result, err := s.db.ExecContext(ctx, `UPDATE channels SET title=?,active=?,updated_at=?
WHERE workspace_id=? AND id=?`, current.Title, current.Active, nowText(), workspaceID, channelID)
	if err != nil {
		return Channel{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return Channel{}, ErrNotFound
	}
	if err := s.recordWorkspaceAudit(ctx, actorUserID, workspaceID, "channel.updated", "channel", channelID); err != nil {
		return Channel{}, err
	}
	return s.GetChannelForWorkspace(ctx, actorUserID, workspaceID, channelID)
}

func (s *Store) DeleteChannelForWorkspace(ctx context.Context, actorUserID, workspaceID string, channelID int64) error {
	if err := requireWorkspaceRole(ctx, s.db, actorUserID, workspaceID, WorkspaceRoleOwner, WorkspaceRoleEditor); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM channels c
WHERE c.workspace_id=? AND c.id=?
AND NOT EXISTS(SELECT 1 FROM posts p WHERE p.workspace_id=c.workspace_id AND p.channel_id=c.id)
AND NOT EXISTS(SELECT 1 FROM campaign_variants cv WHERE cv.workspace_id=c.workspace_id AND cv.channel_id=c.id)`, workspaceID, channelID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 1 {
		return s.recordWorkspaceAudit(ctx, actorUserID, workspaceID, "channel.deleted", "channel", channelID)
	}
	if _, err := s.GetChannelForWorkspace(ctx, actorUserID, workspaceID, channelID); err != nil {
		return err
	}
	return fmt.Errorf("%w: channel has linked posts or campaign variants", ErrConflict)
}

func (s *Store) AddPostAttachmentForWorkspace(ctx context.Context, actorUserID, workspaceID string, postID int64, attachment PostAttachment) (Post, error) {
	ownerID, err := s.workspaceAttachmentOwner(ctx, actorUserID, workspaceID, postID)
	if err != nil {
		return Post{}, err
	}
	post, err := s.AddPostAttachmentForUser(ctx, ownerID, postID, attachment)
	if err != nil {
		return Post{}, err
	}
	return ensurePostWorkspace(post, workspaceID)
}

func (s *Store) ReplacePostAttachmentForWorkspace(ctx context.Context, actorUserID, workspaceID string, postID, attachmentID int64, replacement PostAttachment) (Post, error) {
	ownerID, err := s.workspaceAttachmentOwner(ctx, actorUserID, workspaceID, postID)
	if err != nil {
		return Post{}, err
	}
	post, err := s.ReplacePostAttachmentForUser(ctx, ownerID, postID, attachmentID, replacement)
	if err != nil {
		return Post{}, err
	}
	return ensurePostWorkspace(post, workspaceID)
}

func (s *Store) ReorderPostAttachmentsForWorkspace(ctx context.Context, actorUserID, workspaceID string, postID int64, orderedIDs []int64) (Post, error) {
	ownerID, err := s.workspaceAttachmentOwner(ctx, actorUserID, workspaceID, postID)
	if err != nil {
		return Post{}, err
	}
	post, err := s.ReorderPostAttachmentsForUser(ctx, ownerID, postID, orderedIDs)
	if err != nil {
		return Post{}, err
	}
	return ensurePostWorkspace(post, workspaceID)
}

func (s *Store) DeletePostAttachmentForWorkspace(ctx context.Context, actorUserID, workspaceID string, postID, attachmentID int64) (Post, error) {
	ownerID, err := s.workspaceAttachmentOwner(ctx, actorUserID, workspaceID, postID)
	if err != nil {
		return Post{}, err
	}
	post, err := s.DeletePostAttachmentForUser(ctx, ownerID, postID, attachmentID)
	if err != nil {
		return Post{}, err
	}
	return ensurePostWorkspace(post, workspaceID)
}

func (s *Store) ListPostAttachmentsForWorkspace(ctx context.Context, actorUserID, workspaceID string, postID int64) ([]PostAttachment, error) {
	if _, err := s.GetPostForWorkspace(ctx, actorUserID, workspaceID, postID); err != nil {
		return nil, err
	}
	return queryPostAttachments(ctx, s.db,
		`WHERE workspace_id=? AND post_id=? ORDER BY position,id`, workspaceID, postID)
}

func (s *Store) workspaceAttachmentOwner(ctx context.Context, actorUserID, workspaceID string, postID int64) (string, error) {
	access, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID)
	if err != nil {
		return "", err
	}
	if access.Member.Role != WorkspaceRoleOwner && access.Member.Role != WorkspaceRoleEditor {
		return "", ErrNotFound
	}
	post, err := s.GetPostForWorkspace(ctx, actorUserID, workspaceID, postID)
	if err != nil {
		return "", err
	}
	if post.UserID != access.Workspace.CompatOwnerUserID {
		return "", ErrConflict
	}
	return access.Workspace.CompatOwnerUserID, nil
}

func ensurePostWorkspace(post Post, workspaceID string) (Post, error) {
	if post.WorkspaceID != workspaceID {
		return Post{}, ErrNotFound
	}
	return post, nil
}

func (s *Store) recordWorkspaceAudit(ctx context.Context, actorUserID, workspaceID, action, entityType string, entityID int64) error {
	_, err := s.CreateAuditEvent(ctx, actorUserID, AuditEvent{
		WorkspaceID: workspaceID, Action: action, EntityType: entityType, EntityID: fmt.Sprint(entityID),
	})
	return err
}
