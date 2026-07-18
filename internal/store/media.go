package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type MediaLimits struct {
	MaxFiles int64
	MaxBytes int64
}

type MediaReservation struct {
	OwnerID     string
	WorkspaceID string
	Filename    string
	Token       string
	Existing    bool
}

type MediaCleanupResult struct {
	AssetsRemoved  int64
	ObjectsDeleted int64
	BytesReleased  int64
}

type mediaCleanupCandidate struct {
	ownerID     string
	workspaceID string
	filename    string
}

func validateMediaKey(userID, filename string) (string, error) {
	filename = strings.TrimSpace(filename)
	if strings.TrimSpace(userID) == "" || filename == "" || filename != filepath.Base(filename) || strings.ContainsAny(filename, `/\\`) {
		return "", errors.New("valid user id and media filename are required")
	}
	return filename, nil
}

func mediaAdvisoryLock(ctx context.Context, tx *sql.Tx, filename string) error {
	// All ownership creation and physical deletion for a content-addressed key
	// share this lock. It prevents a new tenant reservation from racing with GC.
	_, err := tx.ExecContext(ctx, bindSQL(`SELECT pg_advisory_xact_lock(hashtextextended(?, 741947))`), filename)
	return err
}

func newMediaReservationToken() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

// ReserveMedia serializes quota accounting in PostgreSQL before any S3 write.
// Duplicate content owned by the same tenant consumes quota only once.
func (s *Store) ReserveMedia(ctx context.Context, userID, filename string, size int64, limits MediaLimits, now time.Time) (MediaReservation, error) {
	filename, err := validateMediaKey(userID, filename)
	if err != nil {
		return MediaReservation{}, err
	}
	if size < 0 || limits.MaxFiles <= 0 || limits.MaxBytes <= 0 {
		return MediaReservation{}, errors.New("valid media size and limits are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MediaReservation{}, fmt.Errorf("begin media reservation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := mediaAdvisoryLock(ctx, tx, filename); err != nil {
		return MediaReservation{}, fmt.Errorf("lock media object: %w", err)
	}

	var state string
	lookupErr := tx.QueryRowContext(ctx, bindSQL(`SELECT state FROM media_assets
WHERE owner_id=? AND filename=? FOR UPDATE`), userID, filename).Scan(&state)
	if lookupErr == nil {
		if state == "pending" {
			return MediaReservation{}, ErrMediaUploadBusy
		}
		if _, err := tx.ExecContext(ctx, bindSQL(`UPDATE media_assets SET updated_at=?
WHERE owner_id=? AND filename=?`), now.UTC(), userID, filename); err != nil {
			return MediaReservation{}, fmt.Errorf("refresh media ownership: %w", err)
		}
		if _, err := tx.ExecContext(ctx, bindSQL(`DELETE FROM media_gc_queue WHERE filename=?`), filename); err != nil {
			return MediaReservation{}, fmt.Errorf("cancel media garbage collection: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return MediaReservation{}, fmt.Errorf("commit existing media reservation: %w", err)
		}
		return MediaReservation{OwnerID: userID, Filename: filename, Existing: true}, nil
	}
	if !errors.Is(lookupErr, sql.ErrNoRows) {
		return MediaReservation{}, fmt.Errorf("lookup media reservation: %w", lookupErr)
	}

	if _, err := tx.ExecContext(ctx, bindSQL(`INSERT INTO media_usage(owner_id, asset_count, total_bytes, updated_at)
VALUES (?,0,0,?) ON CONFLICT(owner_id) DO NOTHING`), userID, now.UTC()); err != nil {
		return MediaReservation{}, fmt.Errorf("initialize media usage: %w", err)
	}
	var usedFiles, usedBytes int64
	if err := tx.QueryRowContext(ctx, bindSQL(`SELECT asset_count, total_bytes FROM media_usage
WHERE owner_id=? FOR UPDATE`), userID).Scan(&usedFiles, &usedBytes); err != nil {
		return MediaReservation{}, fmt.Errorf("lock media usage: %w", err)
	}
	if usedFiles >= limits.MaxFiles || size > limits.MaxBytes || usedBytes > limits.MaxBytes-size {
		return MediaReservation{}, ErrMediaQuotaExceeded
	}
	token, err := newMediaReservationToken()
	if err != nil {
		return MediaReservation{}, fmt.Errorf("create media reservation token: %w", err)
	}
	// The insert trigger resolves the owner's personal workspace id. Charge
	// that workspace ledger together with media_usage: release and GC drain
	// both ledgers for personal workspaces, so reservation must fill both.
	var workspaceID string
	if err := tx.QueryRowContext(ctx, bindSQL(`INSERT INTO media_assets
(owner_id, filename, created_at, size_bytes, state, reservation_token, updated_at)
VALUES (?,?,?,?,'pending',?,?) RETURNING workspace_id`), userID, filename, now.UTC(), size, token, now.UTC()).Scan(&workspaceID); err != nil {
		return MediaReservation{}, fmt.Errorf("reserve media ownership: %w", err)
	}
	if _, err := tx.ExecContext(ctx, bindSQL(`UPDATE media_usage SET asset_count=asset_count+1,
total_bytes=total_bytes+?, updated_at=? WHERE owner_id=?`), size, now.UTC(), userID); err != nil {
		return MediaReservation{}, fmt.Errorf("reserve media quota: %w", err)
	}
	if _, err := tx.ExecContext(ctx, bindSQL(`INSERT INTO workspace_media_usage(workspace_id, asset_count, total_bytes, updated_at)
VALUES (?,1,?,?) ON CONFLICT(workspace_id) DO UPDATE SET
asset_count=workspace_media_usage.asset_count+1,
total_bytes=workspace_media_usage.total_bytes+excluded.total_bytes,
updated_at=excluded.updated_at`), workspaceID, size, now.UTC()); err != nil {
		return MediaReservation{}, fmt.Errorf("reserve workspace media quota: %w", err)
	}
	if _, err := tx.ExecContext(ctx, bindSQL(`DELETE FROM media_gc_queue WHERE filename=?`), filename); err != nil {
		return MediaReservation{}, fmt.Errorf("cancel media garbage collection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return MediaReservation{}, fmt.Errorf("commit media reservation: %w", err)
	}
	return MediaReservation{OwnerID: userID, Filename: filename, Token: token}, nil
}

func (s *Store) CompleteMediaReservation(ctx context.Context, reservation MediaReservation, now time.Time) error {
	if reservation.Existing {
		return nil
	}
	query := `UPDATE media_assets SET state='ready',reservation_token='',updated_at=?
WHERE owner_id=? AND filename=? AND state='pending' AND reservation_token=?`
	args := []any{now.UTC(), reservation.OwnerID, reservation.Filename, reservation.Token}
	if reservation.WorkspaceID != "" {
		query += ` AND workspace_id=?`
		args = append(args, reservation.WorkspaceID)
	}
	result, err := s.db.ExecContext(ctx, bindSQL(query), args...)
	if err != nil {
		return fmt.Errorf("complete media reservation: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read media completion result: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf("%w: media reservation is no longer active", ErrConflict)
	}
	return nil
}

func (s *Store) ReleaseMediaReservation(ctx context.Context, reservation MediaReservation, now time.Time) error {
	if reservation.Existing {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin media reservation release: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := mediaAdvisoryLock(ctx, tx, reservation.Filename); err != nil {
		return fmt.Errorf("lock media reservation release: %w", err)
	}
	var size int64
	var workspaceID string
	query := `DELETE FROM media_assets WHERE owner_id=? AND filename=? AND state='pending' AND reservation_token=?`
	args := []any{reservation.OwnerID, reservation.Filename, reservation.Token}
	if reservation.WorkspaceID != "" {
		query += ` AND workspace_id=?`
		args = append(args, reservation.WorkspaceID)
	}
	query += ` RETURNING size_bytes, workspace_id`
	err = tx.QueryRowContext(ctx, bindSQL(query), args...).Scan(&size, &workspaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("release media ownership: %w", err)
	}
	// Refund exactly the ledgers the reservation charged: the workspace ledger
	// always, plus media_usage for personal workspaces. This mirrors
	// cleanupMediaAsset and keeps the media_usage-before-workspace lock order
	// shared by every media ledger writer.
	var personalWorkspace bool
	if err := tx.QueryRowContext(ctx, bindSQL(`SELECT is_personal FROM workspaces WHERE id=?`), workspaceID).Scan(&personalWorkspace); err != nil {
		return fmt.Errorf("resolve released media workspace: %w", err)
	}
	if personalWorkspace {
		if _, err := tx.ExecContext(ctx, bindSQL(`UPDATE media_usage SET
asset_count=GREATEST(asset_count-1,0), total_bytes=GREATEST(total_bytes-?,0), updated_at=?
WHERE owner_id=?`), size, now.UTC(), reservation.OwnerID); err != nil {
			return fmt.Errorf("release media quota: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workspace_media_usage SET
asset_count=GREATEST(asset_count-1,0),total_bytes=GREATEST(total_bytes-$1,0),updated_at=$2
WHERE workspace_id=$3`, size, now.UTC(), workspaceID); err != nil {
		return fmt.Errorf("release workspace media quota: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit media reservation release: %w", err)
	}
	return nil
}

// CleanupOrphanMedia removes stale reservations and ready assets that are not
// referenced by a post. Each candidate is rechecked under a tenant/key lock;
// the physical object is deleted only after the final ownership row is gone.
func (s *Store) CleanupOrphanMedia(ctx context.Context, before time.Time, limit int, deleteObject func(context.Context, string) error) (MediaCleanupResult, error) {
	if limit <= 0 || deleteObject == nil {
		return MediaCleanupResult{}, errors.New("valid media cleanup limit and deleter are required")
	}
	candidates, err := func() ([]mediaCleanupCandidate, error) {
		rows, err := s.db.QueryContext(ctx, bindSQL(`SELECT owner_id, workspace_id, filename FROM media_assets ma
WHERE ma.updated_at < ? AND (
    ma.state='pending' OR (
        NOT EXISTS (
            SELECT 1 FROM posts p WHERE p.workspace_id=ma.workspace_id AND p.image_path=ma.filename
        ) AND NOT EXISTS (
            SELECT 1 FROM post_attachments pa
            WHERE pa.workspace_id=ma.workspace_id AND pa.storage_key=ma.filename
        )
    )
)
ORDER BY ma.updated_at, ma.owner_id, ma.filename LIMIT ?`), before.UTC(), limit)
		if err != nil {
			return nil, fmt.Errorf("list orphan media assets: %w", err)
		}
		defer func() { _ = rows.Close() }()
		result := make([]mediaCleanupCandidate, 0, limit)
		for rows.Next() {
			var candidate mediaCleanupCandidate
			if err := rows.Scan(&candidate.ownerID, &candidate.workspaceID, &candidate.filename); err != nil {
				return nil, fmt.Errorf("scan orphan media asset: %w", err)
			}
			result = append(result, candidate)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterate orphan media assets: %w", err)
		}
		return result, nil
	}()
	if err != nil {
		return MediaCleanupResult{}, err
	}

	var result MediaCleanupResult
	for _, candidate := range candidates {
		removed, bytesReleased, err := s.cleanupMediaAsset(ctx, candidate, before)
		if err != nil {
			return result, err
		}
		if removed {
			result.AssetsRemoved++
			result.BytesReleased += bytesReleased
		}
	}

	// Ownership removal is committed before any external storage mutation. The
	// queue gets its own bounded pass so objects orphaned above are normally
	// deleted in the same cycle; failures remain queued for the next cycle.
	queued, err := func() ([]string, error) {
		queueRows, err := s.db.QueryContext(ctx, bindSQL(`SELECT filename FROM media_gc_queue
WHERE orphaned_at < ? ORDER BY orphaned_at, filename LIMIT ?`), before.UTC(), limit)
		if err != nil {
			return nil, fmt.Errorf("list queued media objects: %w", err)
		}
		defer func() { _ = queueRows.Close() }()
		result := make([]string, 0, limit)
		for queueRows.Next() {
			var filename string
			if err := queueRows.Scan(&filename); err != nil {
				return nil, fmt.Errorf("scan queued media object: %w", err)
			}
			result = append(result, filename)
		}
		if err := queueRows.Err(); err != nil {
			return nil, fmt.Errorf("iterate queued media rows: %w", err)
		}
		return result, nil
	}()
	if err != nil {
		return result, err
	}
	for _, filename := range queued {
		deleted, err := s.cleanupQueuedMediaObject(ctx, filename, before, deleteObject)
		if err != nil {
			return result, err
		}
		if deleted {
			result.ObjectsDeleted++
		}
	}
	return result, nil
}

func (s *Store) cleanupMediaAsset(ctx context.Context, candidate mediaCleanupCandidate, before time.Time) (bool, int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := mediaAdvisoryLock(ctx, tx, candidate.filename); err != nil {
		return false, 0, err
	}
	var state string
	var size int64
	var updated time.Time
	var personalWorkspace bool
	err = tx.QueryRowContext(ctx, bindSQL(`SELECT ma.state,ma.size_bytes,ma.updated_at,w.is_personal FROM media_assets ma
JOIN workspaces w ON w.id=ma.workspace_id
WHERE ma.owner_id=? AND ma.workspace_id=? AND ma.filename=? FOR UPDATE`),
		candidate.ownerID, candidate.workspaceID, candidate.filename).Scan(&state, &size, &updated, &personalWorkspace)
	// workspace_id is included in the candidate to prevent a same-owner object
	// in another workspace from affecting this quota ledger.
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, tx.Commit()
	}
	if err != nil {
		return false, 0, err
	}
	if !updated.Before(before) {
		return false, 0, tx.Commit()
	}
	if state == "ready" {
		var referenced bool
		if err := tx.QueryRowContext(ctx, bindSQL(`SELECT
EXISTS(SELECT 1 FROM posts WHERE workspace_id=? AND image_path=?) OR
EXISTS(SELECT 1 FROM post_attachments WHERE workspace_id=? AND storage_key=?)`),
			candidate.workspaceID, candidate.filename, candidate.workspaceID, candidate.filename).Scan(&referenced); err != nil {
			return false, 0, err
		}
		if referenced {
			return false, 0, tx.Commit()
		}
	}
	if _, err := tx.ExecContext(ctx, bindSQL(`DELETE FROM media_assets WHERE owner_id=? AND workspace_id=? AND filename=?`),
		candidate.ownerID, candidate.workspaceID, candidate.filename); err != nil {
		return false, 0, err
	}
	// Drain the same ledgers the reservation filled: media_usage for personal
	// workspaces, then the workspace ledger. Keeping media_usage first matches
	// the lock order of the reserve and release paths.
	if personalWorkspace {
		if _, err := tx.ExecContext(ctx, bindSQL(`UPDATE media_usage SET
asset_count=GREATEST(asset_count-1,0), total_bytes=GREATEST(total_bytes-?,0), updated_at=CURRENT_TIMESTAMP
WHERE owner_id=?`), size, candidate.ownerID); err != nil {
			return false, 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workspace_media_usage SET
asset_count=GREATEST(asset_count-1,0),total_bytes=GREATEST(total_bytes-$1,0),updated_at=CURRENT_TIMESTAMP
WHERE workspace_id=$2`, size, candidate.workspaceID); err != nil {
		return false, 0, err
	}
	// The delete trigger inserted the queue row with the deletion time. This
	// asset has already survived the grace period, so preserve its older
	// timestamp and let the queue pass delete it without a second grace period.
	if _, err := tx.ExecContext(ctx, bindSQL(`UPDATE media_gc_queue SET orphaned_at=LEAST(orphaned_at, ?)
WHERE filename=?`), updated.UTC(), candidate.filename); err != nil {
		return false, 0, err
	}
	if err := tx.Commit(); err != nil {
		return false, 0, err
	}
	return true, size, nil
}

func (s *Store) cleanupQueuedMediaObject(ctx context.Context, filename string, before time.Time, deleteObject func(context.Context, string) error) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := mediaAdvisoryLock(ctx, tx, filename); err != nil {
		return false, err
	}
	var orphaned time.Time
	err = tx.QueryRowContext(ctx, bindSQL(`SELECT orphaned_at FROM media_gc_queue WHERE filename=? FOR UPDATE`), filename).Scan(&orphaned)
	if errors.Is(err, sql.ErrNoRows) {
		return false, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	if !orphaned.Before(before) {
		return false, tx.Commit()
	}
	var owners int64
	if err := tx.QueryRowContext(ctx, bindSQL(`SELECT COUNT(*) FROM media_assets WHERE filename=?`), filename).Scan(&owners); err != nil {
		return false, err
	}
	deleted := false
	if owners == 0 {
		if err := deleteObject(ctx, filename); err != nil {
			return false, fmt.Errorf("delete queued media object: %w", err)
		}
		deleted = true
	}
	if _, err := tx.ExecContext(ctx, bindSQL(`DELETE FROM media_gc_queue WHERE filename=?`), filename); err != nil {
		return false, err
	}
	return deleted, tx.Commit()
}

// RegisterMedia remains for compatibility with trusted test/setup paths. New
// uploads must use ReserveMedia before touching S3.
func (s *Store) RegisterMedia(ctx context.Context, userID, filename string, now time.Time) error {
	reservation, err := s.ReserveMedia(ctx, userID, filename, 0, MediaLimits{MaxFiles: 1 << 60, MaxBytes: 1 << 60}, now)
	if err != nil {
		return err
	}
	return s.CompleteMediaReservation(ctx, reservation, now)
}

func (s *Store) UserOwnsMedia(ctx context.Context, userID, filename string) (bool, error) {
	var owned bool
	err := s.db.QueryRowContext(ctx, bindSQL(`SELECT EXISTS(SELECT 1 FROM media_assets
WHERE owner_id=? AND filename=? AND state='ready')`), userID, filename).Scan(&owned)
	if err != nil {
		return false, fmt.Errorf("check media ownership: %w", err)
	}
	return owned, nil
}
