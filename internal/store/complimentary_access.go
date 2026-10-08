package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrBillingComplimentaryAccess = errors.New("complimentary access does not require a paid checkout")

func workspaceComplimentaryAccess(ctx context.Context, q workspaceQueryer, workspaceID string) (bool, error) {
	var active bool
	if err := q.QueryRowContext(ctx, `SELECT workspace_has_complimentary_access($1)`, workspaceID).Scan(&active); err != nil {
		return false, fmt.Errorf("read workspace complimentary access: %w", err)
	}
	return active, nil
}

// Follow the same billing -> parent order as ownership changes. Holding the
// parent fence until the quota transaction commits prevents an old owner's
// grant from authorizing a charge after a completed ownership transfer.
func lockWorkspaceComplimentaryAccess(ctx context.Context, tx *sql.Tx, workspaceID string) (bool, error) {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "maxstudio:billing:"+workspaceID); err != nil {
		return false, err
	}
	var ownerID string
	if err := tx.QueryRowContext(ctx, `SELECT owner_user_id FROM workspaces WHERE id=$1 AND archived_at IS NULL FOR KEY SHARE`, workspaceID).Scan(&ownerID); errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	} else if err != nil {
		return false, err
	}
	return workspaceComplimentaryAccess(ctx, tx, workspaceID)
}

func accountComplimentaryAccess(ctx context.Context, q workspaceQueryer, ownerID string) (bool, error) {
	var active bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM account_complimentary_access WHERE owner_user_id=$1 AND active)`, ownerID).Scan(&active)
	return active, err
}
