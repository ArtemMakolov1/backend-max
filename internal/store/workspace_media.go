package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *Store) WorkspaceOwnsMedia(ctx context.Context, actorUserID, workspaceID, filename string) (bool, error) {
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID); err != nil {
		return false, err
	}
	filename, err := validateMediaKey("workspace", filename)
	if err != nil {
		return false, err
	}
	var owned bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM media_assets
WHERE workspace_id=$1 AND filename=$2 AND state='ready')`, workspaceID, filename).Scan(&owned); err != nil {
		return false, err
	}
	return owned, nil
}

func (s *Store) ReserveMediaForWorkspace(ctx context.Context, actorUserID, workspaceID, filename string, size int64, limits MediaLimits, now time.Time) (MediaReservation, error) {
	access, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID)
	if err != nil {
		return MediaReservation{}, err
	}
	if access.Member.Role != WorkspaceRoleOwner && access.Member.Role != WorkspaceRoleEditor {
		return MediaReservation{}, ErrNotFound
	}
	filename, err = validateMediaKey(access.Workspace.CompatOwnerUserID, filename)
	if err != nil {
		return MediaReservation{}, err
	}
	if size < 0 || limits.MaxFiles <= 0 || limits.MaxBytes <= 0 || now.IsZero() {
		return MediaReservation{}, errors.New("valid media size, limits and time are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MediaReservation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := mediaAdvisoryLock(ctx, tx, filename); err != nil {
		return MediaReservation{}, err
	}
	var state string
	lookupErr := tx.QueryRowContext(ctx, `SELECT state FROM media_assets
WHERE workspace_id=$1 AND filename=$2 FOR UPDATE`, workspaceID, filename).Scan(&state)
	if lookupErr == nil {
		if state == "pending" {
			return MediaReservation{}, ErrMediaUploadBusy
		}
		if _, err := tx.ExecContext(ctx, `UPDATE media_assets SET updated_at=$1
WHERE workspace_id=$2 AND filename=$3`, now.UTC(), workspaceID, filename); err != nil {
			return MediaReservation{}, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM media_gc_queue WHERE filename=$1`, filename); err != nil {
			return MediaReservation{}, err
		}
		if err := tx.Commit(); err != nil {
			return MediaReservation{}, err
		}
		return MediaReservation{OwnerID: access.Workspace.CompatOwnerUserID, WorkspaceID: workspaceID, Filename: filename, Existing: true}, nil
	}
	if !errors.Is(lookupErr, sql.ErrNoRows) {
		return MediaReservation{}, lookupErr
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_media_usage(workspace_id,asset_count,total_bytes,updated_at)
VALUES($1,0,0,$2) ON CONFLICT(workspace_id) DO NOTHING`, workspaceID, now.UTC()); err != nil {
		return MediaReservation{}, err
	}
	var usedFiles, usedBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT asset_count,total_bytes FROM workspace_media_usage
WHERE workspace_id=$1 FOR UPDATE`, workspaceID).Scan(&usedFiles, &usedBytes); err != nil {
		return MediaReservation{}, err
	}
	if usedFiles >= limits.MaxFiles || size > limits.MaxBytes || usedBytes > limits.MaxBytes-size {
		return MediaReservation{}, ErrMediaQuotaExceeded
	}
	token, err := newMediaReservationToken()
	if err != nil {
		return MediaReservation{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO media_assets(
owner_id,workspace_id,filename,created_at,size_bytes,state,reservation_token,updated_at)
VALUES($1,$2,$3,$4,$5,'pending',$6,$4)`, access.Workspace.CompatOwnerUserID,
		workspaceID, filename, now.UTC(), size, token); err != nil {
		return MediaReservation{}, mapWorkspaceWriteError("reserve workspace media", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workspace_media_usage SET
asset_count=asset_count+1,total_bytes=total_bytes+$1,updated_at=$2 WHERE workspace_id=$3`,
		size, now.UTC(), workspaceID); err != nil {
		return MediaReservation{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM media_gc_queue WHERE filename=$1`, filename); err != nil {
		return MediaReservation{}, err
	}
	if err := tx.Commit(); err != nil {
		return MediaReservation{}, fmt.Errorf("commit workspace media reservation: %w", err)
	}
	return MediaReservation{OwnerID: access.Workspace.CompatOwnerUserID, WorkspaceID: workspaceID,
		Filename: filename, Token: token}, nil
}
