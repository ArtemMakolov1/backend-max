package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SyncMAXChannelPermissions observes MAX access without changing the manual
// active flag. Only the verified owner receives notifications, and old or
// repeated webhook events cannot reapply state or notify twice.
func (s *Store) SyncMAXChannelPermissions(ctx context.Context, maxChatID, maxOwnerID string,
	canPublish bool, eventAt, checkedAt time.Time,
) error {
	if strings.TrimSpace(maxChatID) == "" || strings.TrimSpace(maxOwnerID) == "" || eventAt.IsZero() || checkedAt.IsZero() {
		return errors.New("MAX chat, owner and permission timestamps are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var active bool
	var lastSeenAt time.Time
	err = tx.QueryRowContext(ctx, `SELECT active,last_seen_at FROM observed_bot_chats WHERE max_chat_id=$1 FOR UPDATE`, maxChatID).
		Scan(&active, &lastSeenAt)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (!active || lastSeenAt.After(eventAt))) {
		return nil
	}
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT connected.id,connected.workspace_id,connected.title
FROM channels connected JOIN workspaces workspace ON workspace.id=connected.workspace_id
WHERE connected.max_chat_id=$1 AND connected.verified_max_owner_id=$2 AND workspace.archived_at IS NULL
ORDER BY connected.id FOR UPDATE OF connected`, maxChatID, maxOwnerID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	type connectedChannel struct {
		id                 int64
		workspaceID, title string
	}
	channels := []connectedChannel{}
	for rows.Next() {
		var channel connectedChannel
		if err := rows.Scan(&channel.id, &channel.workspaceID, &channel.title); err != nil {
			return err
		}
		channels = append(channels, channel)
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if err := errors.Join(rowsErr, closeErr); err != nil {
		return err
	}
	for _, channel := range channels {
		var previousCanPublish bool
		var previousEventAt time.Time
		err := tx.QueryRowContext(ctx, `SELECT can_publish,event_at FROM channel_max_permissions WHERE channel_id=$1`, channel.id).
			Scan(&previousCanPublish, &previousEventAt)
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if exists && !eventAt.After(previousEventAt) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO channel_max_permissions(channel_id,can_publish,event_at,checked_at)
VALUES ($1,$2,$3,$4) ON CONFLICT(channel_id) DO UPDATE
SET can_publish=excluded.can_publish,event_at=excluded.event_at,checked_at=excluded.checked_at`,
			channel.id, canPublish, eventAt.UTC(), checkedAt.UTC()); err != nil {
			return err
		}
		if !canPublish && (!exists || previousCanPublish) {
			if err := notifyRolesTx(ctx, tx, channel.workspaceID, "", []string{WorkspaceRoleOwner, WorkspaceRoleEditor}, Notification{
				Kind: "channel.permissions_lost", Title: "Помощник потерял право публикации",
				Body:       fmt.Sprintf("Проверьте права помощника в канале «%s»: нужны права администратора, чтения и публикации сообщений. Запланированные посты могут не отправиться.", channel.title),
				EntityType: "channel", EntityID: fmt.Sprint(channel.id),
				DedupeKey: fmt.Sprintf("channel.permissions_lost:%d:%s", channel.id, eventAt.UTC().Format(time.RFC3339Nano)),
			}); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
