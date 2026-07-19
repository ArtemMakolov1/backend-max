package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ChannelParticipantSnapshot is the latest MAX participant observation for a
// channel on one UTC day. The persisted row intentionally contains no tenant
// identity, MAX chat ID, title or public link.
type ChannelParticipantSnapshot struct {
	ObservedOn        string    `json:"observed_on"`
	ParticipantsCount int       `json:"participants_count"`
	CapturedAt        time.Time `json:"captured_at"`
}

// ListChannelsDueForParticipantStats returns a bounded cross-tenant worker
// batch. Callers must claim each row and recheck its owner/chat identity before
// contacting MAX or writing an observation.
func (s *Store) ListChannelsDueForParticipantStats(ctx context.Context, now time.Time, syncInterval time.Duration, limit int) ([]Channel, error) {
	if now.IsZero() || syncInterval <= 0 {
		return nil, errors.New("participant stats time and positive sync interval are required")
	}
	if limit <= 0 {
		return []Channel{}, nil
	}
	if limit > 100 {
		limit = 100
	}
	cutoff := now.UTC().Add(-syncInterval)
	rows, err := s.db.QueryContext(ctx, `
SELECT `+channelColumns+` FROM channels
WHERE owner_id <> ''
  AND EXISTS(SELECT 1 FROM workspaces w WHERE w.id=channels.workspace_id AND w.archived_at IS NULL)
  AND active
  AND COALESCE(participants_stats_attempted_at, '-infinity'::timestamptz) <= ?
ORDER BY COALESCE(participants_stats_attempted_at, '-infinity'::timestamptz), id
LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("list channels due for MAX participant stats: %w", err)
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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list channels due for MAX participant stats: %w", err)
	}
	return channels, nil
}

// ClaimChannelParticipantStatsAttemptForUser atomically reserves one due
// channel. Owner, local channel and immutable MAX chat ID all participate in
// the write so another tenant or a stale worker cannot claim it.
func (s *Store) ClaimChannelParticipantStatsAttemptForUser(ctx context.Context, userID string, channelID int64,
	expectedMAXChatID string, attemptedAt time.Time, syncInterval time.Duration,
) (bool, error) {
	if strings.TrimSpace(userID) == "" || channelID <= 0 || strings.TrimSpace(expectedMAXChatID) == "" {
		return false, errors.New("channel owner, channel and MAX chat ID are required")
	}
	if attemptedAt.IsZero() || syncInterval <= 0 {
		return false, errors.New("participant stats attempt time and positive sync interval are required")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE channels SET participants_stats_attempted_at = ?
WHERE owner_id = ? AND id = ? AND max_chat_id = ? AND active
  AND (participants_stats_attempted_at IS NULL OR participants_stats_attempted_at <= ?)`,
		attemptedAt.UTC(), userID, channelID, expectedMAXChatID, attemptedAt.UTC().Add(-syncInterval))
	if err != nil {
		return false, fmt.Errorf("claim MAX channel participant stats attempt: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 1 {
		return true, nil
	}
	channel, err := s.getChannelForOwner(ctx, userID, channelID)
	if err != nil {
		return false, err
	}
	if channel.MAXChatID != expectedMAXChatID {
		return false, fmt.Errorf("%w: MAX channel changed before participant statistics were claimed", ErrConflict)
	}
	return false, nil
}

// SyncChannelParticipantStatsForUser atomically updates current visual/MAX
// participant metadata and the latest daily observation. The expected MAX chat
// ID protects against a delayed response being attached to another channel.
func (s *Store) SyncChannelParticipantStatsForUser(ctx context.Context, userID string, channelID int64,
	expectedMAXChatID, iconURL string, participantsCount int, capturedAt time.Time,
) (Channel, error) {
	if strings.TrimSpace(userID) == "" || channelID <= 0 || strings.TrimSpace(expectedMAXChatID) == "" {
		return Channel{}, errors.New("channel owner, channel and MAX chat ID are required")
	}
	if participantsCount < 0 {
		return Channel{}, errors.New("MAX participant count must not be negative")
	}
	if capturedAt.IsZero() {
		return Channel{}, errors.New("participant stats capture time is required")
	}
	capturedAt = capturedAt.UTC()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Channel{}, fmt.Errorf("begin channel participant stats sync: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var currentMAXChatID string
	var lastSynced sql.NullTime
	err = tx.QueryRowContext(ctx, bindSQL(`
SELECT max_chat_id, participants_stats_synced_at
FROM channels WHERE owner_id = ? AND id = ? FOR UPDATE`), userID, channelID).Scan(&currentMAXChatID, &lastSynced)
	if errors.Is(err, sql.ErrNoRows) {
		return Channel{}, ErrNotFound
	}
	if err != nil {
		return Channel{}, fmt.Errorf("lock channel participant stats: %w", err)
	}
	if currentMAXChatID != expectedMAXChatID {
		return Channel{}, fmt.Errorf("%w: MAX channel changed while participant statistics were being synchronized", ErrConflict)
	}
	// A delayed upstream response must never replace a more recent observation.
	if lastSynced.Valid && capturedAt.Before(lastSynced.Time.UTC()) {
		if err := tx.Commit(); err != nil {
			return Channel{}, fmt.Errorf("commit stale channel participant stats no-op: %w", err)
		}
		return s.getChannelForOwner(ctx, userID, channelID)
	}
	result, err := tx.ExecContext(ctx, bindSQL(`
UPDATE channels
SET icon_url = ?, participants_count = ?, participants_stats_synced_at = ?,
    participants_stats_attempted_at = ?, updated_at = ?
WHERE owner_id = ? AND id = ? AND max_chat_id = ?`),
		iconURL, participantsCount, capturedAt, capturedAt, capturedAt,
		userID, channelID, expectedMAXChatID)
	if err != nil {
		return Channel{}, fmt.Errorf("sync channel participant stats: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return Channel{}, fmt.Errorf("%w: MAX channel changed while participant statistics were being synchronized", ErrConflict)
	}
	observedOn := capturedAt.Format(time.DateOnly)
	if _, err := tx.ExecContext(ctx, bindSQL(`
INSERT INTO channel_participant_snapshots(channel_id, observed_on, captured_at, participants_count)
VALUES (?, ?, ?, ?)
ON CONFLICT(channel_id, observed_on) DO UPDATE SET
    captured_at = excluded.captured_at,
    participants_count = excluded.participants_count
WHERE excluded.captured_at >= channel_participant_snapshots.captured_at`),
		channelID, observedOn, capturedAt, participantsCount); err != nil {
		return Channel{}, fmt.Errorf("upsert channel participant snapshot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Channel{}, fmt.Errorf("commit channel participant stats sync: %w", err)
	}
	return s.getChannelForOwner(ctx, userID, channelID)
}

// ListChannelParticipantSnapshotsForUser returns an ascending daily series and
// authorizes through the parent channel. This remains tenant-safe even though
// snapshot rows intentionally do not duplicate owner identity.
func (s *Store) ListChannelParticipantSnapshotsForUser(ctx context.Context, userID string, channelID int64,
	fromDay, toDay time.Time,
) ([]ChannelParticipantSnapshot, error) {
	if strings.TrimSpace(userID) == "" || channelID <= 0 {
		return nil, errors.New("channel owner and positive channel ID are required")
	}
	if fromDay.IsZero() || toDay.IsZero() {
		return nil, errors.New("participant history date range is required")
	}
	fromDay, toDay = utcDate(fromDay), utcDate(toDay)
	if toDay.Before(fromDay) {
		return nil, errors.New("participant history end date must not precede start date")
	}
	if _, err := s.GetChannelForUser(ctx, userID, channelID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT snapshots.observed_on, snapshots.participants_count, snapshots.captured_at
FROM channel_participant_snapshots AS snapshots
JOIN channels AS channel ON channel.id = snapshots.channel_id
WHERE channel.owner_id = ? AND channel.id = ?
  AND snapshots.observed_on >= ? AND snapshots.observed_on <= ?
ORDER BY snapshots.observed_on, snapshots.captured_at`,
		userID, channelID, fromDay.Format(time.DateOnly), toDay.Format(time.DateOnly))
	if err != nil {
		return nil, fmt.Errorf("list channel participant snapshots: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]ChannelParticipantSnapshot, 0)
	for rows.Next() {
		var snapshot ChannelParticipantSnapshot
		var observedOn time.Time
		if err := rows.Scan(&observedOn, &snapshot.ParticipantsCount, &snapshot.CapturedAt); err != nil {
			return nil, fmt.Errorf("scan channel participant snapshot: %w", err)
		}
		snapshot.ObservedOn = observedOn.UTC().Format(time.DateOnly)
		snapshot.CapturedAt = snapshot.CapturedAt.UTC()
		result = append(result, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list channel participant snapshots: %w", err)
	}
	return result, nil
}

func utcDate(value time.Time) time.Time {
	year, month, day := value.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}
