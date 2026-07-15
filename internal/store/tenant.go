package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	maxActiveClaimsPerUser = 5
	channelClaimColumns    = `id, token_hash, confirm_token_hash, cancel_token_hash, owner_id, max_chat_id,
public_link, requested_title, status, max_user_id, channel_id, error_code,
requester_label, comparison_code, created_at, expires_at, consumed_at, updated_at`
)

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
max_chat_id, public_link, title, max_owner_id, icon_url, participants_count, active, last_seen_at, removed_at)
VALUES (?,?,?,?,?,?,?,?,?)
ON CONFLICT(max_chat_id) DO UPDATE SET public_link=excluded.public_link, title=excluded.title,
max_owner_id=excluded.max_owner_id, icon_url=excluded.icon_url, participants_count=excluded.participants_count,
active=excluded.active, last_seen_at=excluded.last_seen_at, removed_at=excluded.removed_at
WHERE excluded.last_seen_at > observed_bot_chats.last_seen_at
RETURNING max_chat_id, max_owner_id, icon_url, participants_count, last_seen_at
)
UPDATE channels AS connected SET
icon_url=refreshed_chat.icon_url,
participants_count=refreshed_chat.participants_count,
updated_at=refreshed_chat.last_seen_at
FROM refreshed_chat
WHERE connected.max_chat_id=refreshed_chat.max_chat_id
  AND connected.verified_max_owner_id=refreshed_chat.max_owner_id`, chat.MAXChatID, chat.PublicLink, chat.Title, chat.MAXOwnerID,
		chat.IconURL, chat.ParticipantsCount, chat.Active, chat.LastSeenAt.UTC(), removedAt)
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
	query, value := `SELECT max_chat_id, public_link, title, max_owner_id, COALESCE(icon_url,''), COALESCE(participants_count,0), active, last_seen_at, removed_at
FROM observed_bot_chats WHERE active AND max_chat_id = ?`, maxChatID
	if strings.TrimSpace(publicLink) != "" {
		query, value = `SELECT max_chat_id, public_link, title, max_owner_id, COALESCE(icon_url,''), COALESCE(participants_count,0), active, last_seen_at, removed_at
FROM observed_bot_chats WHERE active AND lower(public_link) = lower(?)`, strings.TrimRight(strings.TrimSpace(publicLink), "/")
	}
	var chat ObservedBotChat
	var removed sql.NullTime
	if err := s.db.QueryRowContext(ctx, query, value).Scan(&chat.MAXChatID, &chat.PublicLink, &chat.Title,
		&chat.MAXOwnerID, &chat.IconURL, &chat.ParticipantsCount, &chat.Active, &chat.LastSeenAt, &removed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ObservedBotChat{}, ErrNotFound
		}
		return ObservedBotChat{}, fmt.Errorf("get observed MAX chat: %w", err)
	}
	chat.LastSeenAt = chat.LastSeenAt.UTC()
	if removed.Valid {
		value := removed.Time.UTC()
		chat.RemovedAt = &value
	}
	return chat, nil
}

func (s *Store) CreateChannelClaim(ctx context.Context, claim ChannelClaim) error {
	if strings.TrimSpace(claim.ID) == "" || strings.TrimSpace(claim.UserID) == "" || strings.TrimSpace(claim.MAXChatID) == "" ||
		strings.TrimSpace(claim.RequesterLabel) == "" || len(claim.ComparisonCode) != 6 {
		return errors.New("claim id, user id and MAX chat id are required")
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
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "claim:"+claim.UserID); err != nil {
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
WHERE owner_id = $1 AND status IN ('pending','awaiting_confirmation','identity_verified')`, claim.UserID).Scan(&active); err != nil {
		return fmt.Errorf("count active channel claims: %w", err)
	}
	if active >= maxActiveClaimsPerUser {
		return fmt.Errorf("%w: too many active channel connection attempts", ErrConflict)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO channel_claims(
id, token_hash, owner_id, max_chat_id, public_link, requested_title, requester_label, comparison_code,
status, created_at, expires_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'pending',$9,$10,$9)`, claim.ID, claim.TokenHash, claim.UserID,
		claim.MAXChatID, claim.PublicLink, claim.RequestedTitle, claim.RequesterLabel, claim.ComparisonCode, now, claim.ExpiresAt.UTC())
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
		` FROM channel_claims WHERE owner_id = ? AND id = ?`, userID, claimID))
	if err != nil {
		return ChannelClaim{}, err
	}
	if (claim.Status == ChannelClaimPending || claim.Status == ChannelClaimAwaitingConfirmation || claim.Status == ChannelClaimIdentityVerified) && !claim.ExpiresAt.After(now) {
		_, _ = s.db.ExecContext(ctx, `UPDATE channel_claims SET status=?, error_code='expired', updated_at=?
WHERE owner_id=? AND id=? AND status IN ('pending','awaiting_confirmation','identity_verified')`, ChannelClaimExpired, now.UTC(), userID, claimID)
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
		` FROM channel_claims WHERE owner_id = ? AND id = ? FOR UPDATE`), claim.UserID, claim.ID))
	if err != nil {
		return Channel{}, err
	}
	if locked.Status == ChannelClaimConnected && locked.ChannelID != nil {
		if err := tx.Commit(); err != nil {
			return Channel{}, err
		}
		return s.GetChannelForUser(ctx, claim.UserID, *locked.ChannelID)
	}
	if locked.Status != ChannelClaimIdentityVerified || locked.MAXUserID != channel.VerifiedMAXOwnerID || locked.MAXChatID != channel.MAXChatID {
		return Channel{}, fmt.Errorf("%w: channel confirmation is not complete", ErrConflict)
	}
	var existingID int64
	var existingOwner string
	lookupErr := tx.QueryRowContext(ctx, `SELECT id, owner_id FROM channels WHERE max_chat_id=$1 FOR UPDATE`, channel.MAXChatID).Scan(&existingID, &existingOwner)
	switch {
	case lookupErr == nil && existingOwner != claim.UserID:
		return Channel{}, ErrChannelOwned
	case lookupErr == nil:
		_, err = tx.ExecContext(ctx, `UPDATE channels SET verified_max_owner_id=$1, title=$2, public_link=$3,
icon_url=$4, participants_count=$5, is_channel=$6, active=TRUE, updated_at=$7 WHERE id=$8 AND owner_id=$9`,
			channel.VerifiedMAXOwnerID, channel.Title, channel.PublicLink, channel.IconURL, channel.ParticipantsCount,
			channel.IsChannel, nowText(), existingID, claim.UserID)
	case errors.Is(lookupErr, sql.ErrNoRows):
		err = tx.QueryRowContext(ctx, `INSERT INTO channels(owner_id, verified_max_owner_id, max_chat_id, title,
public_link, icon_url, participants_count, is_channel, active, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,TRUE,$9,$9) RETURNING id`, claim.UserID, channel.VerifiedMAXOwnerID,
			channel.MAXChatID, channel.Title, channel.PublicLink, channel.IconURL, channel.ParticipantsCount,
			channel.IsChannel, time.Now().UTC()).Scan(&existingID)
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
WHERE owner_id=$4 AND id=$5`, ChannelClaimConnected, existingID, time.Now().UTC(), claim.UserID, claim.ID)
	if err != nil {
		return Channel{}, fmt.Errorf("complete channel claim: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Channel{}, err
	}
	return s.GetChannelForUser(ctx, claim.UserID, existingID)
}

func (s *Store) FailChannelClaim(ctx context.Context, userID, claimID, code string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE channel_claims SET status=?, error_code=?, updated_at=?
WHERE owner_id=? AND id=? AND status IN ('pending','awaiting_confirmation','identity_verified')`,
		ChannelClaimFailed, code, now.UTC(), userID, claimID)
	return err
}

func scanChannelClaim(row scanner) (ChannelClaim, error) {
	var claim ChannelClaim
	var confirmHash, cancelHash sql.NullString
	var channelID sql.NullInt64
	var consumedAt sql.NullTime
	if err := row.Scan(&claim.ID, &claim.TokenHash, &confirmHash, &cancelHash, &claim.UserID, &claim.MAXChatID,
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

func (s *Store) RegisterMedia(ctx context.Context, userID, filename string, now time.Time) error {
	filename = filepath.Base(strings.TrimSpace(filename))
	if userID == "" || filename == "" || strings.ContainsAny(filename, `/\\`) {
		return errors.New("valid user id and media filename are required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO media_assets(owner_id, filename, created_at) VALUES (?,?,?)
ON CONFLICT(owner_id, filename) DO NOTHING`, userID, filename, now.UTC())
	if err != nil {
		return fmt.Errorf("register media ownership: %w", err)
	}
	return nil
}

func (s *Store) UserOwnsMedia(ctx context.Context, userID, filename string) (bool, error) {
	var owned bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM media_assets WHERE owner_id=? AND filename=?)`,
		userID, filename).Scan(&owned)
	if err != nil {
		return false, fmt.Errorf("check media ownership: %w", err)
	}
	return owned, nil
}
