package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const maxIdentityAttemptColumns = `id, token_hash, confirm_token_hash, cancel_token_hash, owner_id,
requester_label, comparison_code, status, max_user_id, error_code, created_at, expires_at, consumed_at, updated_at`

func (s *Store) CreateMAXIdentityLinkAttempt(ctx context.Context, attempt MAXIdentityLinkAttempt) error {
	if strings.TrimSpace(attempt.ID) == "" || strings.TrimSpace(attempt.UserID) == "" ||
		strings.TrimSpace(attempt.RequesterLabel) == "" || len(attempt.ComparisonCode) != 6 {
		return errors.New("identity request id, user id, requester label and comparison code are required")
	}
	if err := validateSHA256Hex("identity deep-link token hash", attempt.TokenHash); err != nil {
		return err
	}
	if err := validateLifetime(attempt.CreatedAt, attempt.ExpiresAt); err != nil {
		return fmt.Errorf("MAX identity link attempt: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin MAX identity link attempt: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := attempt.CreatedAt.UTC()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "max-identity-owner:"+attempt.UserID); err != nil {
		return fmt.Errorf("lock MAX identity owner: %w", err)
	}
	var alreadyLinked bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM max_identity_links WHERE owner_id=$1)`, attempt.UserID).Scan(&alreadyLinked); err != nil {
		return fmt.Errorf("check existing MAX identity link: %w", err)
	}
	if alreadyLinked {
		return fmt.Errorf("%w: MAX identity is already linked", ErrConflict)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE max_identity_link_attempts
SET status='expired', error_code='expired', updated_at=$1
WHERE expires_at <= $1 AND status IN ('pending','awaiting_confirmation')`, now); err != nil {
		return fmt.Errorf("expire MAX identity attempts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE max_identity_link_attempts
SET status='failed', error_code='superseded', updated_at=$1
WHERE owner_id=$2 AND status IN ('pending','awaiting_confirmation')`, now, attempt.UserID); err != nil {
		return fmt.Errorf("supersede MAX identity attempt: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO max_identity_link_attempts(
id, token_hash, owner_id, requester_label, comparison_code, status, created_at, expires_at, updated_at)
VALUES ($1,$2,$3,$4,$5,'pending',$6,$7,$6)`, attempt.ID, attempt.TokenHash, attempt.UserID,
		attempt.RequesterLabel, attempt.ComparisonCode, now, attempt.ExpiresAt.UTC())
	if err != nil {
		return fmt.Errorf("create MAX identity link attempt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit MAX identity link attempt: %w", err)
	}
	return nil
}

// StartMAXIdentityLinkConfirmation atomically consumes the one-time deep-link
// token. Replayed bot_started events for the same MAX user are idempotent and
// never cause a second confirmation message.
func (s *Store) StartMAXIdentityLinkConfirmation(ctx context.Context, tokenHash, maxUserID, confirmHash, cancelHash string, now time.Time) (MAXIdentityLinkAttempt, bool, error) {
	for name, value := range map[string]string{
		"identity deep-link token hash": tokenHash, "identity confirm token hash": confirmHash, "identity cancel token hash": cancelHash,
	} {
		if err := validateSHA256Hex(name, value); err != nil {
			return MAXIdentityLinkAttempt{}, false, err
		}
	}
	if strings.TrimSpace(maxUserID) == "" || now.IsZero() {
		return MAXIdentityLinkAttempt{}, false, errors.New("MAX user id and current time are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MAXIdentityLinkAttempt{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	attempt, err := scanMAXIdentityLinkAttempt(tx.QueryRowContext(ctx, bindSQL(`SELECT `+maxIdentityAttemptColumns+
		` FROM max_identity_link_attempts WHERE token_hash=? FOR UPDATE`), tokenHash))
	if err != nil {
		return MAXIdentityLinkAttempt{}, false, err
	}
	if !attempt.ExpiresAt.After(now) {
		_, _ = tx.ExecContext(ctx, `UPDATE max_identity_link_attempts SET status='expired', error_code='expired', updated_at=$1 WHERE id=$2`, now.UTC(), attempt.ID)
		if err := tx.Commit(); err != nil {
			return MAXIdentityLinkAttempt{}, false, err
		}
		return MAXIdentityLinkAttempt{}, false, ErrNotFound
	}
	if attempt.Status != MAXIdentityAttemptPending {
		if attempt.Status == MAXIdentityAttemptAwaitingConfirmation && attempt.MAXUserID == maxUserID {
			return attempt, false, tx.Commit()
		}
		return MAXIdentityLinkAttempt{}, false, ErrNotFound
	}
	_, err = tx.ExecContext(ctx, `UPDATE max_identity_link_attempts SET status=$1, max_user_id=$2,
confirm_token_hash=$3, cancel_token_hash=$4, consumed_at=$5, updated_at=$5 WHERE id=$6`,
		MAXIdentityAttemptAwaitingConfirmation, maxUserID, confirmHash, cancelHash, now.UTC(), attempt.ID)
	if err != nil {
		return MAXIdentityLinkAttempt{}, false, fmt.Errorf("start MAX identity confirmation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return MAXIdentityLinkAttempt{}, false, err
	}
	attempt.Status, attempt.MAXUserID = MAXIdentityAttemptAwaitingConfirmation, maxUserID
	attempt.ConfirmTokenHash, attempt.CancelTokenHash = confirmHash, cancelHash
	consumedAt := now.UTC()
	attempt.ConsumedAt, attempt.UpdatedAt = &consumedAt, consumedAt
	return attempt, true, nil
}

// ConfirmMAXIdentityLink validates an owner-bound callback and establishes the
// one-to-one link in the same transaction as the attempt state transition.
func (s *Store) ConfirmMAXIdentityLink(ctx context.Context, callbackHash, maxUserID string, confirm bool, now time.Time) (MAXIdentityLinkAttempt, error) {
	if err := validateSHA256Hex("identity callback token hash", callbackHash); err != nil {
		return MAXIdentityLinkAttempt{}, err
	}
	if strings.TrimSpace(maxUserID) == "" || now.IsZero() {
		return MAXIdentityLinkAttempt{}, errors.New("MAX user id and current time are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MAXIdentityLinkAttempt{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var ownerID string
	lookupOwnerErr := tx.QueryRowContext(ctx, bindSQL(`SELECT owner_id FROM max_identity_link_attempts
WHERE (confirm_token_hash=? OR cancel_token_hash=?)`), callbackHash, callbackHash).Scan(&ownerID)
	if errors.Is(lookupOwnerErr, sql.ErrNoRows) {
		return MAXIdentityLinkAttempt{}, ErrNotFound
	}
	if lookupOwnerErr != nil {
		return MAXIdentityLinkAttempt{}, fmt.Errorf("lookup MAX identity callback owner: %w", lookupOwnerErr)
	}
	// Match CreateMAXIdentityLinkAttempt's owner-before-attempt lock order to
	// avoid deadlocks between a callback and a concurrent new browser request.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "max-identity-owner:"+ownerID); err != nil {
		return MAXIdentityLinkAttempt{}, err
	}
	attempt, err := scanMAXIdentityLinkAttempt(tx.QueryRowContext(ctx, bindSQL(`SELECT `+maxIdentityAttemptColumns+
		` FROM max_identity_link_attempts WHERE (confirm_token_hash=? OR cancel_token_hash=?) FOR UPDATE`), callbackHash, callbackHash))
	if err != nil {
		return MAXIdentityLinkAttempt{}, err
	}
	if attempt.MAXUserID != maxUserID {
		return MAXIdentityLinkAttempt{}, ErrNotFound
	}
	if !attempt.ExpiresAt.After(now) {
		if attempt.Status == MAXIdentityAttemptPending || attempt.Status == MAXIdentityAttemptAwaitingConfirmation {
			_, _ = tx.ExecContext(ctx, `UPDATE max_identity_link_attempts SET status='expired', error_code='expired', updated_at=$1 WHERE id=$2`, now.UTC(), attempt.ID)
			if err := tx.Commit(); err != nil {
				return MAXIdentityLinkAttempt{}, err
			}
		}
		return MAXIdentityLinkAttempt{}, ErrNotFound
	}
	wantedHash := attempt.CancelTokenHash
	if confirm {
		wantedHash = attempt.ConfirmTokenHash
	}
	if callbackHash != wantedHash {
		return MAXIdentityLinkAttempt{}, ErrNotFound
	}
	if (!confirm && attempt.Status == MAXIdentityAttemptFailed) || (confirm && attempt.Status == MAXIdentityAttemptLinked) {
		return attempt, tx.Commit()
	}
	if attempt.Status != MAXIdentityAttemptAwaitingConfirmation {
		return MAXIdentityLinkAttempt{}, ErrNotFound
	}
	if !confirm {
		_, err = tx.ExecContext(ctx, `UPDATE max_identity_link_attempts SET status='failed', error_code='canceled', updated_at=$1 WHERE id=$2`, now.UTC(), attempt.ID)
		if err != nil {
			return MAXIdentityLinkAttempt{}, err
		}
		if err := tx.Commit(); err != nil {
			return MAXIdentityLinkAttempt{}, err
		}
		attempt.Status, attempt.ErrorCode, attempt.UpdatedAt = MAXIdentityAttemptFailed, "canceled", now.UTC()
		return attempt, nil
	}
	var currentMAXUserID string
	ownerLookupErr := tx.QueryRowContext(ctx, `SELECT max_user_id FROM max_identity_links WHERE owner_id=$1 FOR UPDATE`, attempt.UserID).Scan(&currentMAXUserID)
	if ownerLookupErr != nil && !errors.Is(ownerLookupErr, sql.ErrNoRows) {
		return MAXIdentityLinkAttempt{}, fmt.Errorf("check owner MAX identity: %w", ownerLookupErr)
	}
	if ownerLookupErr == nil && currentMAXUserID != maxUserID {
		_, err = tx.ExecContext(ctx, `UPDATE max_identity_link_attempts SET status='failed', error_code='owner_identity_already_linked', updated_at=$1 WHERE id=$2`, now.UTC(), attempt.ID)
		if err != nil {
			return MAXIdentityLinkAttempt{}, err
		}
		if err := tx.Commit(); err != nil {
			return MAXIdentityLinkAttempt{}, err
		}
		attempt.Status, attempt.ErrorCode, attempt.UpdatedAt = MAXIdentityAttemptFailed, "owner_identity_already_linked", now.UTC()
		return attempt, nil
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "max-identity-user:"+maxUserID); err != nil {
		return MAXIdentityLinkAttempt{}, err
	}
	var linkedOwner string
	lookupErr := tx.QueryRowContext(ctx, `SELECT owner_id FROM max_identity_links WHERE max_user_id=$1 FOR UPDATE`, maxUserID).Scan(&linkedOwner)
	if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
		return MAXIdentityLinkAttempt{}, fmt.Errorf("check linked MAX identity: %w", lookupErr)
	}
	if lookupErr == nil && linkedOwner != attempt.UserID {
		_, err = tx.ExecContext(ctx, `UPDATE max_identity_link_attempts SET status='failed', error_code='max_identity_already_linked', updated_at=$1 WHERE id=$2`, now.UTC(), attempt.ID)
		if err != nil {
			return MAXIdentityLinkAttempt{}, err
		}
		if err := tx.Commit(); err != nil {
			return MAXIdentityLinkAttempt{}, err
		}
		attempt.Status, attempt.ErrorCode, attempt.UpdatedAt = MAXIdentityAttemptFailed, "max_identity_already_linked", now.UTC()
		return attempt, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO max_identity_links(owner_id, max_user_id, linked_at, updated_at)
VALUES ($1,$2,$3,$3)
ON CONFLICT(owner_id) DO UPDATE SET max_user_id=excluded.max_user_id,
linked_at=CASE WHEN max_identity_links.max_user_id=excluded.max_user_id THEN max_identity_links.linked_at ELSE excluded.linked_at END,
updated_at=excluded.updated_at`, attempt.UserID, maxUserID, now.UTC()); err != nil {
		return MAXIdentityLinkAttempt{}, fmt.Errorf("persist MAX identity link: %w", err)
	}
	identityResult, err := tx.ExecContext(ctx, `INSERT INTO auth_identities(provider, subject, owner_id, created_at, updated_at)
VALUES ('max',$1,$2,$3,$3)
ON CONFLICT(provider, subject) DO UPDATE SET updated_at=excluded.updated_at
WHERE auth_identities.owner_id=excluded.owner_id`, maxUserID, attempt.UserID, now.UTC())
	if err != nil {
		return MAXIdentityLinkAttempt{}, fmt.Errorf("persist MAX auth identity: %w", err)
	}
	if affected, _ := identityResult.RowsAffected(); affected == 0 {
		return MAXIdentityLinkAttempt{}, fmt.Errorf("%w: MAX auth identity belongs to another account", ErrConflict)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE max_identity_link_attempts SET status='linked', error_code='', updated_at=$1 WHERE id=$2`, now.UTC(), attempt.ID); err != nil {
		return MAXIdentityLinkAttempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return MAXIdentityLinkAttempt{}, err
	}
	attempt.Status, attempt.ErrorCode, attempt.UpdatedAt = MAXIdentityAttemptLinked, "", now.UTC()
	return attempt, nil
}

func (s *Store) FailMAXIdentityLinkAttempt(ctx context.Context, userID, requestID, code string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE max_identity_link_attempts SET status='failed', error_code=?, updated_at=?
WHERE owner_id=? AND id=? AND status IN ('pending','awaiting_confirmation')`, code, now.UTC(), userID, requestID)
	return err
}

func (s *Store) GetMAXIdentityLinkForUser(ctx context.Context, userID string) (MAXIdentityLink, error) {
	var link MAXIdentityLink
	err := s.db.QueryRowContext(ctx, `SELECT owner_id, max_user_id, linked_at, updated_at FROM max_identity_links WHERE owner_id=?`, userID).
		Scan(&link.UserID, &link.MAXUserID, &link.LinkedAt, &link.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MAXIdentityLink{}, ErrNotFound
	}
	if err != nil {
		return MAXIdentityLink{}, fmt.Errorf("get MAX identity link: %w", err)
	}
	link.LinkedAt, link.UpdatedAt = link.LinkedAt.UTC(), link.UpdatedAt.UTC()
	return link, nil
}

func (s *Store) GetLatestMAXIdentityLinkAttemptForUser(ctx context.Context, userID string, now time.Time) (MAXIdentityLinkAttempt, error) {
	attempt, err := scanMAXIdentityLinkAttempt(s.db.QueryRowContext(ctx, `SELECT `+maxIdentityAttemptColumns+`
FROM max_identity_link_attempts WHERE owner_id=? ORDER BY created_at DESC, id DESC LIMIT 1`, userID))
	if err != nil {
		return MAXIdentityLinkAttempt{}, err
	}
	if (attempt.Status == MAXIdentityAttemptPending || attempt.Status == MAXIdentityAttemptAwaitingConfirmation) && !attempt.ExpiresAt.After(now) {
		_, _ = s.db.ExecContext(ctx, `UPDATE max_identity_link_attempts SET status='expired', error_code='expired', updated_at=?
WHERE owner_id=? AND id=? AND status IN ('pending','awaiting_confirmation')`, now.UTC(), userID, attempt.ID)
		attempt.Status, attempt.ErrorCode, attempt.UpdatedAt = MAXIdentityAttemptExpired, "expired", now.UTC()
	}
	return attempt, nil
}

func (s *Store) ListDiscoverableChannelsForUser(ctx context.Context, userID string) ([]DiscoverableChannel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT o.max_chat_id, o.title, o.public_link,
COALESCE(o.icon_url, ''), COALESCE(o.participants_count, 0), c.id, COALESCE(c.active, FALSE)
FROM max_identity_links l
JOIN observed_bot_chats o ON o.active AND o.max_owner_id=l.max_user_id
LEFT JOIN channels c ON c.max_chat_id=o.max_chat_id
WHERE l.owner_id=? AND (c.id IS NULL OR c.owner_id=l.owner_id)
ORDER BY lower(o.title), o.max_chat_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list discoverable MAX channels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	channels := make([]DiscoverableChannel, 0)
	for rows.Next() {
		var channel DiscoverableChannel
		var connectedID sql.NullInt64
		var active bool
		if err := rows.Scan(&channel.MAXChatID, &channel.Title, &channel.PublicLink, &channel.IconURL,
			&channel.ParticipantsCount, &connectedID, &active); err != nil {
			return nil, fmt.Errorf("scan discoverable MAX channel: %w", err)
		}
		channel.OwnerVerified = true
		if connectedID.Valid {
			id := connectedID.Int64
			channel.Connected, channel.ConnectedChannelID = active, &id
		}
		channels = append(channels, channel)
	}
	return channels, rows.Err()
}

// ConnectDiscoverableChannelForUser is the final transactional authorization
// boundary. It rechecks both the identity link and webhook inventory under row
// locks before creating or refreshing a tenant-owned channel.
func (s *Store) ConnectDiscoverableChannelForUser(ctx context.Context, userID, maxChatID string, channel Channel) (Channel, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(maxChatID) == "" || channel.MAXChatID != maxChatID ||
		strings.TrimSpace(channel.VerifiedMAXOwnerID) == "" {
		return Channel{}, errors.New("user, MAX chat and verified MAX owner are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Channel{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var linkedMAXUserID string
	if err := tx.QueryRowContext(ctx, `SELECT max_user_id FROM max_identity_links WHERE owner_id=$1 FOR UPDATE`, userID).Scan(&linkedMAXUserID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Channel{}, ErrNotFound
		}
		return Channel{}, err
	}
	var observedOwner string
	if err := tx.QueryRowContext(ctx, `SELECT max_owner_id FROM observed_bot_chats
WHERE max_chat_id=$1 AND active FOR UPDATE`, maxChatID).Scan(&observedOwner); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Channel{}, ErrNotFound
		}
		return Channel{}, err
	}
	if observedOwner == "" || observedOwner != linkedMAXUserID || channel.VerifiedMAXOwnerID != linkedMAXUserID {
		return Channel{}, ErrNotFound
	}
	var channelID int64
	var existingOwner string
	lookupErr := tx.QueryRowContext(ctx, `SELECT id, owner_id FROM channels WHERE max_chat_id=$1 FOR UPDATE`, maxChatID).
		Scan(&channelID, &existingOwner)
	now := time.Now().UTC()
	switch {
	case lookupErr == nil && existingOwner != userID:
		return Channel{}, ErrChannelOwned
	case lookupErr == nil:
		_, err = tx.ExecContext(ctx, `UPDATE channels SET verified_max_owner_id=$1, title=$2, public_link=$3,
icon_url=$4, participants_count=$5, is_channel=TRUE, active=TRUE, updated_at=$6 WHERE id=$7 AND owner_id=$8`,
			channel.VerifiedMAXOwnerID, channel.Title, channel.PublicLink, channel.IconURL, channel.ParticipantsCount,
			now, channelID, userID)
	case errors.Is(lookupErr, sql.ErrNoRows):
		err = tx.QueryRowContext(ctx, `INSERT INTO channels(owner_id, verified_max_owner_id, max_chat_id, title,
public_link, icon_url, participants_count, is_channel, active, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,TRUE,TRUE,$8,$8) RETURNING id`, userID, channel.VerifiedMAXOwnerID,
			maxChatID, channel.Title, channel.PublicLink, channel.IconURL, channel.ParticipantsCount, now).Scan(&channelID)
	default:
		return Channel{}, fmt.Errorf("lookup discovered channel ownership: %w", lookupErr)
	}
	if err != nil {
		return Channel{}, fmt.Errorf("connect discovered MAX channel: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Channel{}, err
	}
	return s.GetChannelForUser(ctx, userID, channelID)
}

func scanMAXIdentityLinkAttempt(row scanner) (MAXIdentityLinkAttempt, error) {
	var attempt MAXIdentityLinkAttempt
	var confirmHash, cancelHash sql.NullString
	var consumedAt sql.NullTime
	if err := row.Scan(&attempt.ID, &attempt.TokenHash, &confirmHash, &cancelHash, &attempt.UserID,
		&attempt.RequesterLabel, &attempt.ComparisonCode, &attempt.Status, &attempt.MAXUserID, &attempt.ErrorCode,
		&attempt.CreatedAt, &attempt.ExpiresAt, &consumedAt, &attempt.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MAXIdentityLinkAttempt{}, ErrNotFound
		}
		return MAXIdentityLinkAttempt{}, fmt.Errorf("scan MAX identity link attempt: %w", err)
	}
	attempt.ConfirmTokenHash, attempt.CancelTokenHash = confirmHash.String, cancelHash.String
	if consumedAt.Valid {
		value := consumedAt.Time.UTC()
		attempt.ConsumedAt = &value
	}
	attempt.CreatedAt, attempt.ExpiresAt, attempt.UpdatedAt = attempt.CreatedAt.UTC(), attempt.ExpiresAt.UTC(), attempt.UpdatedAt.UTC()
	return attempt, nil
}
