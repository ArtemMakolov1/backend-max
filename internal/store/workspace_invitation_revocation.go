package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func lockWorkspaceMembership(ctx context.Context, tx *sql.Tx, workspaceID string) error {
	// Seat enforcement and billing lifecycle writes acquire this lock before
	// the workspace row. Keep that order so accepting an invite cannot deadlock
	// with renewal, ownership transfer or archival.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		"maxstudio:billing:"+workspaceID); err != nil {
		return err
	}
	var lockedID string
	err := tx.QueryRowContext(ctx, `SELECT id FROM workspaces
WHERE id=$1 AND archived_at IS NULL FOR UPDATE`, workspaceID).Scan(&lockedID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func lockTeamWorkspaceOwner(ctx context.Context, tx *sql.Tx, actorUserID, workspaceID string) error {
	if err := lockWorkspaceMembership(ctx, tx, workspaceID); err != nil {
		return err
	}
	return requireTeamWorkspaceOwner(ctx, tx, actorUserID, workspaceID)
}

// A shared invitation cannot be associated with its future recipient. Keep a
// per-member cutoff as well as revoking addressed invitations, so an old shared
// link cannot undo a removal while it remains usable by other invited users.
func revokePriorWorkspaceInvitationsForMemberTx(
	ctx context.Context, tx *sql.Tx, actorUserID, workspaceID, userID string, now time.Time,
) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_member_invitation_cutoffs(workspace_id,user_id,invalid_before)
VALUES($1,$2,$3) ON CONFLICT(workspace_id,user_id) DO UPDATE
SET invalid_before=GREATEST(workspace_member_invitation_cutoffs.invalid_before,EXCLUDED.invalid_before)`,
		workspaceID, userID, now); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `UPDATE workspace_invitations SET status='revoked',revoked_at=$3
WHERE workspace_id=$1 AND status='pending' AND created_at<=$3
  AND (target_user_id=$2 OR (email<>'' AND lower(email)=(SELECT lower(email) FROM users WHERE id=$2)))
RETURNING id`, workspaceID, userID, now)
	if err != nil {
		return err
	}
	var invitationIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		invitationIDs = append(invitationIDs, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range invitationIDs {
		if err := appendAuditEventTx(ctx, tx, AuditEvent{
			WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "invitation.revoked",
			EntityType: "invitation", EntityID: id,
			Metadata: mustJSON(map[string]any{"reason": "member_access_changed", "user_id": userID}), CreatedAt: now,
		}); err != nil {
			return err
		}
	}
	return nil
}
