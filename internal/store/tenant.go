package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	maxActiveClaimsPerUser = 5
	channelClaimColumns    = `id, token_hash, confirm_token_hash, cancel_token_hash, owner_id, workspace_id, requested_by_user_id, max_chat_id,
public_link, requested_title, status, max_user_id, channel_id, error_code,
requester_label, comparison_code, created_at, expires_at, consumed_at, updated_at`
)

// TouchObservedBotChat persists the authenticated lifecycle fact before any
// MAX metadata enrichment is attempted. A bot_added event can arrive while the
// bot is still only a subscriber, so owner/admin lookups are not guaranteed to
// work yet. Existing verified metadata is deliberately preserved.
func (s *Store) TouchObservedBotChat(ctx context.Context, maxChatID string, seenAt time.Time) error {
	maxChatID = strings.TrimSpace(maxChatID)
	if maxChatID == "" || seenAt.IsZero() {
		return errors.New("observed MAX chat id and seen_at are required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO observed_bot_chats(max_chat_id, active, last_seen_at, removed_at)
VALUES ($1,TRUE,$2,NULL)
ON CONFLICT(max_chat_id) DO UPDATE SET active=TRUE,last_seen_at=excluded.last_seen_at,removed_at=NULL,
max_owner_id=CASE WHEN observed_bot_chats.active THEN observed_bot_chats.max_owner_id ELSE '' END
WHERE excluded.last_seen_at > observed_bot_chats.last_seen_at`, maxChatID, seenAt.UTC())
	if err != nil {
		return fmt.Errorf("touch observed MAX chat: %w", err)
	}
	return nil
}

// RefreshObservedBotChatMetadata enriches only a currently active lifecycle
// row. In particular, an in-flight refresh that started before bot_removed must
// never resurrect the channel after that removal has been stored.
func (s *Store) RefreshObservedBotChatMetadata(ctx context.Context, chat ObservedBotChat) error {
	if strings.TrimSpace(chat.MAXChatID) == "" || chat.LastSeenAt.IsZero() {
		return errors.New("observed MAX chat id and last_seen_at are required")
	}
	_, err := s.db.ExecContext(ctx, `WITH refreshed_chat AS (
UPDATE observed_bot_chats SET
public_link=CASE WHEN trim($1)<>'' THEN $1 ELSE public_link END,
title=CASE WHEN trim($2)<>'' THEN $2 ELSE title END,
max_owner_id=CASE WHEN trim($3)<>'' THEN $3 ELSE max_owner_id END,
icon_url=CASE WHEN trim($4)<>'' THEN $4 ELSE icon_url END,
participants_count=CASE WHEN $5>0 THEN $5 ELSE participants_count END,
description=CASE WHEN $6::timestamptz IS NOT NULL AND (max_info_synced_at IS NULL OR $6>=max_info_synced_at) THEN $7 ELSE description END,
is_public=CASE WHEN $6::timestamptz IS NOT NULL AND (max_info_synced_at IS NULL OR $6>=max_info_synced_at) THEN $8 ELSE is_public END,
messages_count=CASE WHEN $6::timestamptz IS NOT NULL AND (max_info_synced_at IS NULL OR $6>=max_info_synced_at) THEN $9 ELSE messages_count END,
has_pinned_message=CASE WHEN $6::timestamptz IS NOT NULL AND (max_info_synced_at IS NULL OR $6>=max_info_synced_at) THEN $10 ELSE has_pinned_message END,
max_last_event_time=CASE WHEN $6::timestamptz IS NOT NULL AND (max_info_synced_at IS NULL OR $6>=max_info_synced_at) THEN $11 ELSE max_last_event_time END,
max_info_synced_at=CASE WHEN $6::timestamptz IS NOT NULL AND (max_info_synced_at IS NULL OR $6>=max_info_synced_at) THEN $6 ELSE max_info_synced_at END,
last_seen_at=GREATEST(last_seen_at,$12)
WHERE max_chat_id=$13 AND active AND $12>=last_seen_at
RETURNING max_chat_id,public_link,title,description,max_owner_id,icon_url,participants_count,is_public,messages_count,
has_pinned_message,max_last_event_time,max_info_synced_at,last_seen_at
)
UPDATE channels AS connected SET
title=CASE WHEN trim(refreshed_chat.title)<>'' THEN refreshed_chat.title ELSE connected.title END,
public_link=CASE WHEN trim(refreshed_chat.public_link)<>'' THEN refreshed_chat.public_link ELSE connected.public_link END,
icon_url=refreshed_chat.icon_url,
participants_count=refreshed_chat.participants_count,
description=CASE WHEN refreshed_chat.max_info_synced_at IS NOT NULL THEN refreshed_chat.description ELSE connected.description END,
is_public=CASE WHEN refreshed_chat.max_info_synced_at IS NOT NULL THEN refreshed_chat.is_public ELSE connected.is_public END,
messages_count=CASE WHEN refreshed_chat.max_info_synced_at IS NOT NULL THEN refreshed_chat.messages_count ELSE connected.messages_count END,
has_pinned_message=CASE WHEN refreshed_chat.max_info_synced_at IS NOT NULL THEN refreshed_chat.has_pinned_message ELSE connected.has_pinned_message END,
max_last_event_time=CASE WHEN refreshed_chat.max_info_synced_at IS NOT NULL THEN refreshed_chat.max_last_event_time ELSE connected.max_last_event_time END,
max_info_synced_at=CASE WHEN refreshed_chat.max_info_synced_at IS NOT NULL THEN refreshed_chat.max_info_synced_at ELSE connected.max_info_synced_at END,
updated_at=refreshed_chat.last_seen_at
FROM refreshed_chat
WHERE connected.max_chat_id=refreshed_chat.max_chat_id
  AND connected.verified_max_owner_id=refreshed_chat.max_owner_id
  AND connected.updated_at<=refreshed_chat.last_seen_at`, chat.PublicLink, chat.Title, chat.MAXOwnerID,
		chat.IconURL, chat.ParticipantsCount, chat.MAXInfoSyncedAt, chat.Description, chat.IsPublic,
		chat.MessagesCount, chat.HasPinnedMessage, chat.MAXLastEventTime, chat.LastSeenAt.UTC(), chat.MAXChatID)
	if err != nil {
		return fmt.Errorf("refresh observed MAX chat metadata: %w", err)
	}
	return nil
}

func (s *Store) UpsertObservedBotChat(ctx context.Context, chat ObservedBotChat) error {
	if strings.TrimSpace(chat.MAXChatID) == "" || chat.LastSeenAt.IsZero() {
		return errors.New("observed MAX chat id and last_seen_at are required")
	}
	var removedAt any
	if chat.RemovedAt != nil {
		removedAt = chat.RemovedAt.UTC()
	}
	_, err := s.db.ExecContext(ctx, `WITH refreshed_chat AS (
INSERT INTO observed_bot_chats(
max_chat_id, public_link, title, description, max_owner_id, icon_url, participants_count, is_public, messages_count,
has_pinned_message, max_last_event_time, max_info_synced_at, active, last_seen_at, removed_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(max_chat_id) DO UPDATE SET public_link=excluded.public_link, title=excluded.title,
max_owner_id=excluded.max_owner_id, icon_url=excluded.icon_url, participants_count=excluded.participants_count,
description=CASE WHEN excluded.max_info_synced_at IS NOT NULL
                      AND (observed_bot_chats.max_info_synced_at IS NULL OR excluded.max_info_synced_at>=observed_bot_chats.max_info_synced_at)
                 THEN excluded.description ELSE observed_bot_chats.description END,
is_public=CASE WHEN excluded.max_info_synced_at IS NOT NULL
                   AND (observed_bot_chats.max_info_synced_at IS NULL OR excluded.max_info_synced_at>=observed_bot_chats.max_info_synced_at)
              THEN excluded.is_public ELSE observed_bot_chats.is_public END,
messages_count=CASE WHEN excluded.max_info_synced_at IS NOT NULL
                         AND (observed_bot_chats.max_info_synced_at IS NULL OR excluded.max_info_synced_at>=observed_bot_chats.max_info_synced_at)
                    THEN excluded.messages_count ELSE observed_bot_chats.messages_count END,
has_pinned_message=CASE WHEN excluded.max_info_synced_at IS NOT NULL
                             AND (observed_bot_chats.max_info_synced_at IS NULL OR excluded.max_info_synced_at>=observed_bot_chats.max_info_synced_at)
                        THEN excluded.has_pinned_message ELSE observed_bot_chats.has_pinned_message END,
max_last_event_time=CASE WHEN excluded.max_info_synced_at IS NOT NULL
                              AND (observed_bot_chats.max_info_synced_at IS NULL OR excluded.max_info_synced_at>=observed_bot_chats.max_info_synced_at)
                         THEN excluded.max_last_event_time ELSE observed_bot_chats.max_last_event_time END,
max_info_synced_at=CASE WHEN excluded.max_info_synced_at IS NOT NULL
                             AND (observed_bot_chats.max_info_synced_at IS NULL OR excluded.max_info_synced_at>=observed_bot_chats.max_info_synced_at)
                        THEN excluded.max_info_synced_at ELSE observed_bot_chats.max_info_synced_at END,
active=excluded.active, last_seen_at=excluded.last_seen_at, removed_at=excluded.removed_at
WHERE excluded.last_seen_at > observed_bot_chats.last_seen_at
   OR (excluded.last_seen_at = observed_bot_chats.last_seen_at
       AND observed_bot_chats.active AND observed_bot_chats.removed_at IS NULL)
RETURNING max_chat_id, public_link, title, description, max_owner_id, icon_url, participants_count, is_public,
messages_count, has_pinned_message, max_last_event_time, max_info_synced_at, last_seen_at
)
UPDATE channels AS connected SET
title=CASE WHEN trim(refreshed_chat.title)<>'' THEN refreshed_chat.title ELSE connected.title END,
public_link=CASE WHEN trim(refreshed_chat.public_link)<>'' THEN refreshed_chat.public_link ELSE connected.public_link END,
icon_url=refreshed_chat.icon_url,
participants_count=refreshed_chat.participants_count,
description=CASE WHEN refreshed_chat.max_info_synced_at IS NOT NULL THEN refreshed_chat.description ELSE connected.description END,
is_public=CASE WHEN refreshed_chat.max_info_synced_at IS NOT NULL THEN refreshed_chat.is_public ELSE connected.is_public END,
messages_count=CASE WHEN refreshed_chat.max_info_synced_at IS NOT NULL THEN refreshed_chat.messages_count ELSE connected.messages_count END,
has_pinned_message=CASE WHEN refreshed_chat.max_info_synced_at IS NOT NULL THEN refreshed_chat.has_pinned_message ELSE connected.has_pinned_message END,
max_last_event_time=CASE WHEN refreshed_chat.max_info_synced_at IS NOT NULL THEN refreshed_chat.max_last_event_time ELSE connected.max_last_event_time END,
max_info_synced_at=CASE WHEN refreshed_chat.max_info_synced_at IS NOT NULL THEN refreshed_chat.max_info_synced_at ELSE connected.max_info_synced_at END,
updated_at=refreshed_chat.last_seen_at
FROM refreshed_chat
WHERE connected.max_chat_id=refreshed_chat.max_chat_id
  AND connected.verified_max_owner_id=refreshed_chat.max_owner_id
  AND connected.updated_at<=refreshed_chat.last_seen_at`, chat.MAXChatID, chat.PublicLink, chat.Title,
		chat.Description, chat.MAXOwnerID, chat.IconURL, chat.ParticipantsCount, chat.IsPublic,
		chat.MessagesCount, chat.HasPinnedMessage, chat.MAXLastEventTime, chat.MAXInfoSyncedAt,
		chat.Active, chat.LastSeenAt.UTC(), removedAt)
	if err != nil {
		return fmt.Errorf("upsert observed MAX chat: %w", err)
	}
	return nil
}

func (s *Store) MarkObservedBotChatRemoved(ctx context.Context, maxChatID string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var applied int
	err = tx.QueryRowContext(ctx, `INSERT INTO observed_bot_chats(max_chat_id, active, last_seen_at, removed_at)
VALUES ($1,FALSE,$2,$2)
ON CONFLICT(max_chat_id) DO UPDATE SET active=FALSE, removed_at=excluded.removed_at,
last_seen_at=excluded.last_seen_at
WHERE excluded.last_seen_at >= observed_bot_chats.last_seen_at
RETURNING 1`, maxChatID, now.UTC()).Scan(&applied)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("mark observed MAX chat removed: %w", err)
	}
	if applied == 1 {
		if _, err := tx.ExecContext(ctx, `UPDATE channels SET active=FALSE, updated_at=$1 WHERE max_chat_id=$2`, now.UTC(), maxChatID); err != nil {
			return fmt.Errorf("deactivate removed connected channel: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) GetActiveObservedBotChat(ctx context.Context, publicLink, maxChatID string) (ObservedBotChat, error) {
	query, value := `SELECT max_chat_id, public_link, title, description, max_owner_id, COALESCE(icon_url,''), COALESCE(participants_count,0),
is_public, messages_count, has_pinned_message, max_last_event_time, max_info_synced_at, active, last_seen_at, removed_at
FROM observed_bot_chats WHERE active AND max_chat_id = ?`, maxChatID
	if strings.TrimSpace(publicLink) != "" {
		query, value = `SELECT max_chat_id, public_link, title, description, max_owner_id, COALESCE(icon_url,''), COALESCE(participants_count,0),
is_public, messages_count, has_pinned_message, max_last_event_time, max_info_synced_at, active, last_seen_at, removed_at
FROM observed_bot_chats WHERE active AND lower(public_link) = lower(?)`, strings.TrimRight(strings.TrimSpace(publicLink), "/")
	}
	var chat ObservedBotChat
	var maxLastEventTime, maxInfoSyncedAt, removed sql.NullTime
	if err := s.db.QueryRowContext(ctx, query, value).Scan(&chat.MAXChatID, &chat.PublicLink, &chat.Title,
		&chat.Description, &chat.MAXOwnerID, &chat.IconURL, &chat.ParticipantsCount, &chat.IsPublic,
		&chat.MessagesCount, &chat.HasPinnedMessage, &maxLastEventTime, &maxInfoSyncedAt,
		&chat.Active, &chat.LastSeenAt, &removed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ObservedBotChat{}, ErrNotFound
		}
		return ObservedBotChat{}, fmt.Errorf("get observed MAX chat: %w", err)
	}
	chat.LastSeenAt = chat.LastSeenAt.UTC()
	chat.MAXLastEventTime = parseNullableTime(maxLastEventTime)
	chat.MAXInfoSyncedAt = parseNullableTime(maxInfoSyncedAt)
	if removed.Valid {
		value := removed.Time.UTC()
		chat.RemovedAt = &value
	}
	return chat, nil
}

func (s *Store) CreateChannelClaim(ctx context.Context, claim ChannelClaim) error {
	if claim.ActorUserID == "" {
		claim.ActorUserID = claim.UserID
	}
	if strings.TrimSpace(claim.ID) == "" || strings.TrimSpace(claim.ActorUserID) == "" || strings.TrimSpace(claim.MAXChatID) == "" ||
		strings.TrimSpace(claim.RequesterLabel) == "" || len(claim.ComparisonCode) != 6 {
		return errors.New("claim id, user id and MAX chat id are required")
	}
	if claim.WorkspaceID != "" {
		access, err := s.ResolveWorkspaceAccess(ctx, claim.ActorUserID, claim.WorkspaceID)
		if err != nil {
			return err
		}
		if access.Member.Role != WorkspaceRoleOwner && access.Member.Role != WorkspaceRoleEditor {
			return ErrNotFound
		}
		claim.UserID = access.Workspace.CompatOwnerUserID
	} else {
		claim.UserID = claim.ActorUserID
	}
	if err := validateSHA256Hex("claim token hash", claim.TokenHash); err != nil {
		return err
	}
	if err := validateLifetime(claim.CreatedAt, claim.ExpiresAt); err != nil {
		return fmt.Errorf("channel claim: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin channel claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := claim.CreatedAt.UTC()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "claim:"+claim.ActorUserID); err != nil {
		return fmt.Errorf("lock user channel claims: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE channel_claims SET status = 'expired', error_code = 'expired', updated_at = $1
WHERE expires_at <= $1 AND status IN ('pending','awaiting_confirmation','identity_verified')`, now); err != nil {
		return fmt.Errorf("expire channel claims: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE channel_claims SET status = 'failed', error_code = 'superseded', updated_at = $1
WHERE owner_id = $2 AND max_chat_id = $3 AND status IN ('pending','awaiting_confirmation','identity_verified')`,
		now, claim.UserID, claim.MAXChatID); err != nil {
		return fmt.Errorf("supersede channel claim: %w", err)
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM channel_claims
WHERE requested_by_user_id = $1 AND status IN ('pending','awaiting_confirmation','identity_verified')`, claim.ActorUserID).Scan(&active); err != nil {
		return fmt.Errorf("count active channel claims: %w", err)
	}
	if active >= maxActiveClaimsPerUser {
		return fmt.Errorf("%w: too many active channel connection attempts", ErrConflict)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO channel_claims(
id, token_hash, owner_id, workspace_id, requested_by_user_id, max_chat_id, public_link, requested_title, requester_label, comparison_code,
status, created_at, expires_at, updated_at)
VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9,$10,'pending',$11,$12,$11)`, claim.ID, claim.TokenHash, claim.UserID,
		claim.WorkspaceID, claim.ActorUserID, claim.MAXChatID, claim.PublicLink, claim.RequestedTitle,
		claim.RequesterLabel, claim.ComparisonCode, now, claim.ExpiresAt.UTC())
	if err != nil {
		return fmt.Errorf("create channel claim: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit channel claim: %w", err)
	}
	return nil
}

// StartChannelClaimConfirmation atomically consumes the deep-link token. The
// returned bool is true only for the first bot_started event; replays are safe
// and must not send another confirmation message.
func (s *Store) StartChannelClaimConfirmation(ctx context.Context, tokenHash, maxUserID, confirmHash, cancelHash string, now time.Time) (ChannelClaim, bool, error) {
	for name, value := range map[string]string{"claim token hash": tokenHash, "confirm token hash": confirmHash, "cancel token hash": cancelHash} {
		if err := validateSHA256Hex(name, value); err != nil {
			return ChannelClaim{}, false, err
		}
	}
	if strings.TrimSpace(maxUserID) == "" || now.IsZero() {
		return ChannelClaim{}, false, errors.New("MAX user id and current time are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ChannelClaim{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	claim, err := scanChannelClaim(tx.QueryRowContext(ctx, bindSQL(`SELECT `+channelClaimColumns+
		` FROM channel_claims WHERE token_hash = ? FOR UPDATE`), tokenHash))
	if err != nil {
		return ChannelClaim{}, false, err
	}
	if !claim.ExpiresAt.After(now) {
		_, _ = tx.ExecContext(ctx, `UPDATE channel_claims SET status='expired', error_code='expired', updated_at=$1 WHERE id=$2`, now.UTC(), claim.ID)
		if commitErr := tx.Commit(); commitErr != nil {
			return ChannelClaim{}, false, commitErr
		}
		return ChannelClaim{}, false, ErrNotFound
	}
	if claim.Status != ChannelClaimPending {
		if claim.Status == ChannelClaimAwaitingConfirmation && claim.MAXUserID == maxUserID {
			return claim, false, tx.Commit()
		}
		return ChannelClaim{}, false, ErrNotFound
	}
	_, err = tx.ExecContext(ctx, `UPDATE channel_claims SET status=$1, max_user_id=$2,
confirm_token_hash=$3, cancel_token_hash=$4, consumed_at=$5, updated_at=$5 WHERE id=$6`,
		ChannelClaimAwaitingConfirmation, maxUserID, confirmHash, cancelHash, now.UTC(), claim.ID)
	if err != nil {
		return ChannelClaim{}, false, fmt.Errorf("start channel confirmation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ChannelClaim{}, false, err
	}
	claim.Status = ChannelClaimAwaitingConfirmation
	claim.MAXUserID = maxUserID
	claim.ConfirmTokenHash = confirmHash
	claim.CancelTokenHash = cancelHash
	consumedAt := now.UTC()
	claim.ConsumedAt = &consumedAt
	return claim, true, nil
}

func (s *Store) ConfirmChannelClaim(ctx context.Context, callbackHash, maxUserID string, confirm bool, now time.Time) (ChannelClaim, error) {
	if err := validateSHA256Hex("callback token hash", callbackHash); err != nil {
		return ChannelClaim{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ChannelClaim{}, err
	}
	defer func() { _ = tx.Rollback() }()
	claim, err := scanChannelClaim(tx.QueryRowContext(ctx, bindSQL(`SELECT `+channelClaimColumns+
		` FROM channel_claims WHERE (confirm_token_hash = ? OR cancel_token_hash = ?) FOR UPDATE`), callbackHash, callbackHash))
	if err != nil {
		return ChannelClaim{}, err
	}
	if claim.MAXUserID != maxUserID || !claim.ExpiresAt.After(now) {
		return ChannelClaim{}, ErrNotFound
	}
	wantedHash := claim.CancelTokenHash
	status, errorCode := ChannelClaimFailed, "canceled"
	if confirm {
		wantedHash = claim.ConfirmTokenHash
		status, errorCode = ChannelClaimIdentityVerified, ""
	}
	if callbackHash != wantedHash {
		return ChannelClaim{}, ErrNotFound
	}
	if claim.Status == status {
		return claim, tx.Commit()
	}
	if claim.Status != ChannelClaimAwaitingConfirmation {
		return ChannelClaim{}, ErrNotFound
	}
	_, err = tx.ExecContext(ctx, `UPDATE channel_claims SET status=$1, error_code=$2, updated_at=$3 WHERE id=$4`,
		status, errorCode, now.UTC(), claim.ID)
	if err != nil {
		return ChannelClaim{}, err
	}
	if err := tx.Commit(); err != nil {
		return ChannelClaim{}, err
	}
	claim.Status, claim.ErrorCode, claim.UpdatedAt = status, errorCode, now.UTC()
	return claim, nil
}

func (s *Store) GetChannelClaimForUser(ctx context.Context, userID, claimID string, now time.Time) (ChannelClaim, error) {
	claim, err := scanChannelClaim(s.db.QueryRowContext(ctx, `SELECT `+channelClaimColumns+
		` FROM channel_claims c WHERE c.id = ? AND EXISTS(
SELECT 1 FROM workspace_members wm WHERE wm.workspace_id=c.workspace_id AND wm.user_id=?)`, claimID, userID))
	if err != nil {
		return ChannelClaim{}, err
	}
	if (claim.Status == ChannelClaimPending || claim.Status == ChannelClaimAwaitingConfirmation || claim.Status == ChannelClaimIdentityVerified) && !claim.ExpiresAt.After(now) {
		_, _ = s.db.ExecContext(ctx, `UPDATE channel_claims SET status=?, error_code='expired', updated_at=?
			WHERE workspace_id=? AND id=? AND status IN ('pending','awaiting_confirmation','identity_verified')`,
			ChannelClaimExpired, now.UTC(), claim.WorkspaceID, claimID)
		claim.Status, claim.ErrorCode, claim.UpdatedAt = ChannelClaimExpired, "expired", now.UTC()
	}
	return claim, nil
}

func (s *Store) CompleteChannelClaim(ctx context.Context, claim ChannelClaim, channel Channel) (Channel, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Channel{}, err
	}
	defer func() { _ = tx.Rollback() }()
	locked, err := scanChannelClaim(tx.QueryRowContext(ctx, bindSQL(`SELECT `+channelClaimColumns+
		` FROM channel_claims WHERE id = ? FOR UPDATE`), claim.ID))
	if err != nil {
		return Channel{}, err
	}
	// Lock the target workspace membership together with the workspace row. A
	// claim may be confirmed asynchronously, so the actor must still be able to
	// manage channels when completion mutates tenant-owned data.
	var targetCompatOwner, targetRole string
	var targetIsPersonal bool
	err = tx.QueryRowContext(ctx, `SELECT w.compat_owner_user_id,w.is_personal,wm.role
FROM workspaces w
JOIN workspace_members wm ON wm.workspace_id=w.id AND wm.user_id=$1
WHERE w.id=$2 AND w.archived_at IS NULL
FOR UPDATE OF w,wm`, locked.ActorUserID, locked.WorkspaceID).Scan(
		&targetCompatOwner, &targetIsPersonal, &targetRole)
	if errors.Is(err, sql.ErrNoRows) {
		return Channel{}, ErrNotFound
	}
	if err != nil {
		return Channel{}, fmt.Errorf("lock channel claim workspace access: %w", err)
	}
	if targetRole != WorkspaceRoleOwner && targetRole != WorkspaceRoleEditor {
		return Channel{}, ErrNotFound
	}
	if locked.UserID != targetCompatOwner {
		return Channel{}, fmt.Errorf("%w: channel claim owner no longer matches its workspace", ErrConflict)
	}
	if locked.Status == ChannelClaimConnected && locked.ChannelID != nil {
		if err := tx.Commit(); err != nil {
			return Channel{}, err
		}
		return s.GetChannelForWorkspace(ctx, locked.ActorUserID, locked.WorkspaceID, *locked.ChannelID)
	}
	if locked.Status != ChannelClaimIdentityVerified || locked.MAXUserID != channel.VerifiedMAXOwnerID || locked.MAXChatID != channel.MAXChatID {
		return Channel{}, fmt.Errorf("%w: channel confirmation is not complete", ErrConflict)
	}
	var existingID int64
	var existingOwner, existingWorkspace string
	lookupErr := tx.QueryRowContext(ctx, `SELECT id,owner_id,workspace_id FROM channels
WHERE max_chat_id=$1 FOR UPDATE`, channel.MAXChatID).Scan(&existingID, &existingOwner, &existingWorkspace)
	completedAt := time.Now().UTC()
	transferredFromWorkspace := ""
	switch {
	case lookupErr == nil && existingOwner != locked.UserID:
		// The only cross-owner move allowed is the explicit transfer of the
		// actor's own active personal channel into a team workspace. This keeps a
		// team editor from taking another member's or another team's channel.
		if targetIsPersonal || existingWorkspace == locked.WorkspaceID {
			return Channel{}, ErrChannelOwned
		}
		var sourceOwner, sourceCompatOwner string
		var sourceIsPersonal bool
		sourceErr := tx.QueryRowContext(ctx, `SELECT owner_user_id,compat_owner_user_id,is_personal
FROM workspaces WHERE id=$1 AND archived_at IS NULL FOR UPDATE`, existingWorkspace).Scan(
			&sourceOwner, &sourceCompatOwner, &sourceIsPersonal)
		if errors.Is(sourceErr, sql.ErrNoRows) {
			return Channel{}, ErrChannelOwned
		}
		if sourceErr != nil {
			return Channel{}, fmt.Errorf("inspect channel source workspace: %w", sourceErr)
		}
		if !sourceIsPersonal || sourceOwner != locked.ActorUserID ||
			sourceCompatOwner != existingOwner || existingOwner != locked.ActorUserID {
			return Channel{}, ErrChannelOwned
		}
		var linked int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM posts WHERE channel_id=$1`, existingID).Scan(&linked); err != nil {
			return Channel{}, err
		}
		if linked != 0 {
			return Channel{}, fmt.Errorf("%w: connected channel has linked posts", ErrConflict)
		}
		// Historical personal claims use a composite (owner_id, channel_id)
		// foreign key. Detach their optional channel pointer before changing the
		// channel owner; the claim history and its personal workspace stay intact.
		if _, err := tx.ExecContext(ctx, `UPDATE channel_claims SET channel_id=NULL,updated_at=$1
WHERE workspace_id=$2 AND owner_id=$3 AND channel_id=$4`,
			completedAt, existingWorkspace, existingOwner, existingID); err != nil {
			return Channel{}, fmt.Errorf("detach personal channel claims: %w", err)
		}
		result, updateErr := tx.ExecContext(ctx, `UPDATE channels SET owner_id=$1,workspace_id=$2,
verified_max_owner_id=$3,title=$4,description=$5,public_link=$6,icon_url=$7,participants_count=$8,
is_public=$9,messages_count=$10,has_pinned_message=$11,max_last_event_time=$12,max_info_synced_at=$13,
is_channel=$14,active=TRUE,updated_at=$15
WHERE id=$16 AND owner_id=$17 AND workspace_id=$18`,
			locked.UserID, locked.WorkspaceID, channel.VerifiedMAXOwnerID, channel.Title,
			channel.Description, channel.PublicLink, channel.IconURL, channel.ParticipantsCount, channel.IsPublic,
			channel.MessagesCount, channel.HasPinnedMessage, channel.MAXLastEventTime, channel.MAXInfoSyncedAt,
			channel.IsChannel, completedAt, existingID, existingOwner, existingWorkspace)
		if updateErr != nil {
			return Channel{}, fmt.Errorf("transfer personal channel: %w", updateErr)
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return Channel{}, fmt.Errorf("%w: channel changed while it was being transferred", ErrConflict)
		}
		transferredFromWorkspace = existingWorkspace
	case lookupErr == nil:
		if existingWorkspace != locked.WorkspaceID {
			return Channel{}, ErrChannelOwned
		}
		_, err = tx.ExecContext(ctx, `UPDATE channels SET verified_max_owner_id=$1, title=$2, description=$3, public_link=$4,
icon_url=$5, participants_count=$6, is_public=$7, messages_count=$8, has_pinned_message=$9,
max_last_event_time=$10, max_info_synced_at=$11, is_channel=$12, active=TRUE,workspace_id=$13,updated_at=$14
WHERE id=$15 AND owner_id=$16`,
			channel.VerifiedMAXOwnerID, channel.Title, channel.Description, channel.PublicLink, channel.IconURL,
			channel.ParticipantsCount, channel.IsPublic, channel.MessagesCount, channel.HasPinnedMessage,
			channel.MAXLastEventTime, channel.MAXInfoSyncedAt, channel.IsChannel, locked.WorkspaceID,
			completedAt, existingID, locked.UserID)
	case errors.Is(lookupErr, sql.ErrNoRows):
		err = tx.QueryRowContext(ctx, `INSERT INTO channels(owner_id,workspace_id,verified_max_owner_id,max_chat_id,title,
description, public_link, icon_url, participants_count, is_public, messages_count, has_pinned_message,
max_last_event_time, max_info_synced_at, is_channel, active, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,TRUE,$16,$16) RETURNING id`,
			locked.UserID, locked.WorkspaceID, channel.VerifiedMAXOwnerID, channel.MAXChatID, channel.Title,
			channel.Description, channel.PublicLink, channel.IconURL, channel.ParticipantsCount, channel.IsPublic,
			channel.MessagesCount, channel.HasPinnedMessage, channel.MAXLastEventTime, channel.MAXInfoSyncedAt,
			channel.IsChannel, completedAt).Scan(&existingID)
	default:
		return Channel{}, fmt.Errorf("lookup channel ownership: %w", lookupErr)
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Channel{}, ErrChannelOwned
		}
		return Channel{}, fmt.Errorf("connect channel: %w", err)
	}
	_, err = tx.ExecContext(ctx, `UPDATE channel_claims SET status=$1, channel_id=$2, error_code='', updated_at=$3
WHERE workspace_id=$4 AND id=$5`, ChannelClaimConnected, existingID, completedAt, locked.WorkspaceID, claim.ID)
	if err != nil {
		return Channel{}, fmt.Errorf("complete channel claim: %w", err)
	}
	auditAction := "channel.connected"
	auditMetadata := map[string]any{"max_chat_id": channel.MAXChatID}
	if transferredFromWorkspace != "" {
		auditAction = "channel.transferred"
		auditMetadata["source_workspace_id"] = transferredFromWorkspace
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: locked.WorkspaceID, ActorUserID: locked.ActorUserID, Action: auditAction,
		EntityType: "channel", EntityID: fmt.Sprint(existingID),
		Metadata: mustJSON(auditMetadata), CreatedAt: completedAt,
	}); err != nil {
		return Channel{}, err
	}
	if err := tx.Commit(); err != nil {
		return Channel{}, err
	}
	return s.GetChannelForWorkspace(ctx, locked.ActorUserID, locked.WorkspaceID, existingID)
}

func (s *Store) FailChannelClaim(ctx context.Context, userID, claimID, code string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE channel_claims SET status=?, error_code=?, updated_at=?
WHERE id=? AND status IN ('pending','awaiting_confirmation','identity_verified')
AND EXISTS(SELECT 1 FROM workspace_members wm WHERE wm.workspace_id=channel_claims.workspace_id AND wm.user_id=?)`,
		ChannelClaimFailed, code, now.UTC(), claimID, userID)
	return err
}

func scanChannelClaim(row scanner) (ChannelClaim, error) {
	var claim ChannelClaim
	var confirmHash, cancelHash sql.NullString
	var channelID sql.NullInt64
	var consumedAt sql.NullTime
	if err := row.Scan(&claim.ID, &claim.TokenHash, &confirmHash, &cancelHash, &claim.UserID, &claim.WorkspaceID, &claim.ActorUserID, &claim.MAXChatID,
		&claim.PublicLink, &claim.RequestedTitle, &claim.Status, &claim.MAXUserID, &channelID, &claim.ErrorCode,
		&claim.RequesterLabel, &claim.ComparisonCode, &claim.CreatedAt, &claim.ExpiresAt, &consumedAt, &claim.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ChannelClaim{}, ErrNotFound
		}
		return ChannelClaim{}, fmt.Errorf("scan channel claim: %w", err)
	}
	claim.ConfirmTokenHash, claim.CancelTokenHash = confirmHash.String, cancelHash.String
	if channelID.Valid {
		claim.ChannelID = &channelID.Int64
	}
	if consumedAt.Valid {
		value := consumedAt.Time.UTC()
		claim.ConsumedAt = &value
	}
	claim.CreatedAt, claim.ExpiresAt, claim.UpdatedAt = claim.CreatedAt.UTC(), claim.ExpiresAt.UTC(), claim.UpdatedAt.UTC()
	return claim, nil
}
