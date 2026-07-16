package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const workspaceColumns = `id, name, owner_user_id, compat_owner_user_id, is_personal, approval_required,
require_distinct_approver, COALESCE(created_by, ''), created_at, updated_at, archived_at`

const qualifiedWorkspaceColumns = `w.id, w.name, w.owner_user_id, w.compat_owner_user_id, w.is_personal, w.approval_required,
w.require_distinct_approver, COALESCE(w.created_by, ''), w.created_at, w.updated_at, w.archived_at`

type workspaceQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func ResolveWorkspaceCapabilities(role string) WorkspaceCapabilities {
	switch role {
	case WorkspaceRoleOwner:
		return WorkspaceCapabilities{
			ManageWorkspace: true, ManageMembers: true, EditContent: true,
			SubmitReview: true, ApproveReview: true, Comment: true, ViewAudit: true,
		}
	case WorkspaceRoleEditor:
		return WorkspaceCapabilities{EditContent: true, SubmitReview: true, Comment: true}
	case WorkspaceRoleApprover:
		return WorkspaceCapabilities{ApproveReview: true, Comment: true}
	default:
		return WorkspaceCapabilities{}
	}
}

func (s *Store) ResolveWorkspaceAccess(ctx context.Context, userID, workspaceID string) (WorkspaceAccess, error) {
	return resolveWorkspaceAccess(ctx, s.db, userID, workspaceID)
}

func resolveWorkspaceAccess(ctx context.Context, q workspaceQueryer, userID, workspaceID string) (WorkspaceAccess, error) {
	var access WorkspaceAccess
	err := q.QueryRowContext(ctx, `SELECT `+qualifiedWorkspaceColumns+`,
wm.workspace_id, wm.user_id, wm.role, COALESCE(wm.created_by, ''), wm.joined_at, wm.updated_at,
u.display_name, u.email, u.avatar_url
FROM workspaces w
JOIN workspace_members wm ON wm.workspace_id=w.id
JOIN users u ON u.id=wm.user_id
WHERE w.id=$1 AND w.archived_at IS NULL AND wm.user_id=$2`, workspaceID, userID).Scan(
		&access.Workspace.ID, &access.Workspace.Name, &access.Workspace.OwnerUserID, &access.Workspace.CompatOwnerUserID,
		&access.Workspace.IsPersonal, &access.Workspace.ApprovalRequired,
		&access.Workspace.RequireDistinctApprover, &access.Workspace.CreatedBy,
		&access.Workspace.CreatedAt, &access.Workspace.UpdatedAt,
		&access.Workspace.ArchivedAt,
		&access.Member.WorkspaceID, &access.Member.UserID, &access.Member.Role,
		&access.Member.CreatedBy, &access.Member.JoinedAt, &access.Member.UpdatedAt,
		&access.Member.DisplayName, &access.Member.Email, &access.Member.AvatarURL,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkspaceAccess{}, ErrNotFound
	}
	if err != nil {
		return WorkspaceAccess{}, fmt.Errorf("resolve workspace access: %w", err)
	}
	normalizeWorkspace(&access.Workspace)
	normalizeWorkspaceMember(&access.Member)
	access.Capabilities = ResolveWorkspaceCapabilities(access.Member.Role)
	return access, nil
}

func (s *Store) ListWorkspaces(ctx context.Context, userID string) ([]WorkspaceAccess, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+qualifiedWorkspaceColumns+`,
wm.workspace_id, wm.user_id, wm.role, COALESCE(wm.created_by, ''), wm.joined_at, wm.updated_at,
u.display_name, u.email, u.avatar_url
FROM workspace_members wm
JOIN workspaces w ON w.id=wm.workspace_id
JOIN users u ON u.id=wm.user_id
WHERE wm.user_id=$1 AND w.archived_at IS NULL
ORDER BY w.is_personal DESC, lower(w.name), w.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]WorkspaceAccess, 0)
	for rows.Next() {
		var access WorkspaceAccess
		if err := rows.Scan(
			&access.Workspace.ID, &access.Workspace.Name, &access.Workspace.OwnerUserID, &access.Workspace.CompatOwnerUserID,
			&access.Workspace.IsPersonal, &access.Workspace.ApprovalRequired,
			&access.Workspace.RequireDistinctApprover, &access.Workspace.CreatedBy,
			&access.Workspace.CreatedAt, &access.Workspace.UpdatedAt,
			&access.Workspace.ArchivedAt,
			&access.Member.WorkspaceID, &access.Member.UserID, &access.Member.Role,
			&access.Member.CreatedBy, &access.Member.JoinedAt, &access.Member.UpdatedAt,
			&access.Member.DisplayName, &access.Member.Email, &access.Member.AvatarURL,
		); err != nil {
			return nil, fmt.Errorf("scan workspace: %w", err)
		}
		normalizeWorkspace(&access.Workspace)
		normalizeWorkspaceMember(&access.Member)
		access.Capabilities = ResolveWorkspaceCapabilities(access.Member.Role)
		result = append(result, access)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workspaces: %w", err)
	}
	return result, nil
}

func (s *Store) GetWorkspaceForUser(ctx context.Context, userID, workspaceID string) (Workspace, error) {
	access, err := s.ResolveWorkspaceAccess(ctx, userID, workspaceID)
	return access.Workspace, err
}

// GetWorkspace is reserved for trusted background workers which already got a
// workspace-scoped post from the database. Request handlers must use
// GetWorkspaceForUser or ResolveWorkspaceAccess.
func (s *Store) GetWorkspace(ctx context.Context, workspaceID string) (Workspace, error) {
	var workspace Workspace
	if err := s.db.QueryRowContext(ctx, `SELECT `+workspaceColumns+` FROM workspaces WHERE id=$1`, workspaceID).Scan(
		&workspace.ID, &workspace.Name, &workspace.OwnerUserID, &workspace.CompatOwnerUserID, &workspace.IsPersonal,
		&workspace.ApprovalRequired, &workspace.RequireDistinctApprover, &workspace.CreatedBy,
		&workspace.CreatedAt, &workspace.UpdatedAt, &workspace.ArchivedAt,
	); errors.Is(err, sql.ErrNoRows) {
		return Workspace{}, ErrNotFound
	} else if err != nil {
		return Workspace{}, fmt.Errorf("get workspace: %w", err)
	}
	normalizeWorkspace(&workspace)
	return workspace, nil
}

func (s *Store) CreateWorkspace(ctx context.Context, actorUserID string, workspace Workspace) (Workspace, error) {
	return s.createWorkspace(ctx, actorUserID, workspace, 0)
}

// CreateWorkspaceLimited creates a team workspace while atomically enforcing
// the maximum number of retained team workspaces owned by the actor. Production
// request paths should use this method; CreateWorkspace remains available for
// trusted migrations and compatibility callers.
func (s *Store) CreateWorkspaceLimited(
	ctx context.Context,
	actorUserID string,
	workspace Workspace,
	maxOwnedTeamWorkspaces int,
) (Workspace, error) {
	if maxOwnedTeamWorkspaces <= 0 {
		return Workspace{}, errors.New("owned team workspace limit must be positive")
	}
	return s.createWorkspace(ctx, actorUserID, workspace, maxOwnedTeamWorkspaces)
}

func (s *Store) createWorkspace(
	ctx context.Context,
	actorUserID string,
	workspace Workspace,
	maxOwnedTeamWorkspaces int,
) (Workspace, error) {
	workspace.Name = strings.TrimSpace(workspace.Name)
	if actorUserID == "" || workspace.Name == "" {
		return Workspace{}, errors.New("workspace name and actor are required")
	}
	if workspace.ID == "" {
		workspace.ID = newStoreID("ws_")
	}
	if workspace.IsPersonal {
		return Workspace{}, errors.New("personal workspaces are created with the user account")
	}
	now := workspace.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	workspace.OwnerUserID = actorUserID
	workspace.CompatOwnerUserID = newStoreID("workspace_compat_")
	workspace.CreatedBy = actorUserID
	workspace.IsPersonal = false
	// Team workspaces start protected. Owners may relax either setting later.
	workspace.ApprovalRequired = true
	workspace.RequireDistinctApprover = true
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Workspace{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if maxOwnedTeamWorkspaces > 0 {
		if err := requireOwnedTeamWorkspaceCapacity(ctx, tx, actorUserID, maxOwnedTeamWorkspaces); err != nil {
			return Workspace{}, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO users(id,login,email,display_name,created_at,updated_at)
VALUES($1,'','','',$2,$2)`, workspace.CompatOwnerUserID, now.UTC())
	if err != nil {
		return Workspace{}, mapWorkspaceWriteError("create workspace compatibility tenant", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO workspaces(
id,name,owner_user_id,compat_owner_user_id,is_personal,approval_required,require_distinct_approver,created_by,created_at,updated_at)
VALUES($1,$2,$3,$4,FALSE,TRUE,TRUE,$3,$5,$5)`, workspace.ID, workspace.Name,
		actorUserID, workspace.CompatOwnerUserID, now.UTC())
	if err != nil {
		return Workspace{}, mapWorkspaceWriteError("create workspace", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO workspace_members(
workspace_id,user_id,role,created_by,joined_at,updated_at)
VALUES($1,$2,'owner',$2,$3,$3)`, workspace.ID, actorUserID, now.UTC())
	if err != nil {
		return Workspace{}, fmt.Errorf("create workspace owner membership: %w", err)
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspace.ID, ActorUserID: actorUserID, Action: "workspace.created",
		EntityType: "workspace", EntityID: workspace.ID, Metadata: json.RawMessage(`{}`), CreatedAt: now,
	}); err != nil {
		return Workspace{}, err
	}
	if err := tx.Commit(); err != nil {
		return Workspace{}, fmt.Errorf("commit workspace: %w", err)
	}
	return s.GetWorkspaceForUser(ctx, actorUserID, workspace.ID)
}

func (s *Store) UpdateWorkspace(ctx context.Context, actorUserID, workspaceID string, changes WorkspaceChanges) (Workspace, error) {
	if changes.Name == nil && changes.ApprovalRequired == nil && changes.RequireDistinctApprover == nil {
		return s.GetWorkspaceForUser(ctx, actorUserID, workspaceID)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Workspace{}, err
	}
	defer func() { _ = tx.Rollback() }()
	access, err := resolveWorkspaceAccess(ctx, tx, actorUserID, workspaceID)
	if err != nil {
		return Workspace{}, err
	}
	if access.Member.Role != WorkspaceRoleOwner {
		return Workspace{}, ErrNotFound
	}
	if access.Workspace.IsPersonal && (changes.ApprovalRequired != nil || changes.RequireDistinctApprover != nil) {
		return Workspace{}, ErrConflict
	}
	name := access.Workspace.Name
	approvalRequired := access.Workspace.ApprovalRequired
	distinctApprover := access.Workspace.RequireDistinctApprover
	if changes.Name != nil {
		name = strings.TrimSpace(*changes.Name)
		if name == "" {
			return Workspace{}, errors.New("workspace name is required")
		}
	}
	if changes.ApprovalRequired != nil {
		approvalRequired = *changes.ApprovalRequired
	}
	if changes.RequireDistinctApprover != nil {
		distinctApprover = *changes.RequireDistinctApprover
	}
	now := time.Now().UTC()
	_, err = tx.ExecContext(ctx, `UPDATE workspaces SET name=$1, approval_required=$2,
require_distinct_approver=$3, updated_at=$4 WHERE id=$5`, name, approvalRequired, distinctApprover, now, workspaceID)
	if err != nil {
		return Workspace{}, fmt.Errorf("update workspace: %w", err)
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "workspace.updated",
		EntityType: "workspace", EntityID: workspaceID, Metadata: json.RawMessage(`{}`), CreatedAt: now,
	}); err != nil {
		return Workspace{}, err
	}
	if err := tx.Commit(); err != nil {
		return Workspace{}, err
	}
	return s.GetWorkspaceForUser(ctx, actorUserID, workspaceID)
}

func (s *Store) DeleteWorkspace(ctx context.Context, actorUserID, workspaceID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	access, err := resolveWorkspaceAccess(ctx, tx, actorUserID, workspaceID)
	if err != nil {
		return err
	}
	if access.Member.Role != WorkspaceRoleOwner || access.Workspace.IsPersonal {
		return ErrConflict
	}
	// Serialize archival with FK-backed resource creation and with updates to
	// existing posts. A workspace with an active or scheduled external
	// lifecycle must be cleaned up explicitly; otherwise archiving would hide
	// the only routes that can cancel the schedule or delete the MAX message.
	var lockedWorkspaceID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM workspaces
WHERE id=$1 AND archived_at IS NULL FOR UPDATE`, workspaceID).Scan(&lockedWorkspaceID); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	var activePostID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM posts
WHERE workspace_id=$1 AND (status IN ('scheduled','publishing') OR max_message_id<>'')
ORDER BY id LIMIT 1`, workspaceID).Scan(&activePostID)
	if err == nil {
		return fmt.Errorf("%w: cancel scheduled posts and delete MAX publications before archiving", ErrConflict)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("inspect workspace publication lifecycle: %w", err)
	}
	now := time.Now().UTC()
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "workspace.archived",
		EntityType: "workspace", EntityID: workspaceID, Metadata: json.RawMessage(`{}`), CreatedAt: now,
	}); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workspace_invitations
SET status='revoked',revoked_at=$1
WHERE workspace_id=$2 AND status='pending'`, now, workspaceID); err != nil {
		return fmt.Errorf("revoke workspace invitations: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE workspaces SET archived_at=$1,updated_at=$1
WHERE id=$2 AND archived_at IS NULL`, now, workspaceID)
	if err != nil {
		return fmt.Errorf("archive workspace: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) ListWorkspaceMembers(ctx context.Context, actorUserID, workspaceID string) ([]WorkspaceMember, error) {
	if _, err := s.ResolveWorkspaceAccess(ctx, actorUserID, workspaceID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT wm.workspace_id,wm.user_id,wm.role,COALESCE(wm.created_by,''),
wm.joined_at,wm.updated_at,u.display_name,u.email,u.avatar_url
FROM workspace_members wm JOIN users u ON u.id=wm.user_id WHERE wm.workspace_id=$1
ORDER BY CASE role WHEN 'owner' THEN 0 WHEN 'editor' THEN 1 WHEN 'approver' THEN 2 ELSE 3 END,
joined_at, user_id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list workspace members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]WorkspaceMember, 0)
	for rows.Next() {
		var member WorkspaceMember
		if err := rows.Scan(&member.WorkspaceID, &member.UserID, &member.Role, &member.CreatedBy,
			&member.JoinedAt, &member.UpdatedAt, &member.DisplayName, &member.Email, &member.AvatarURL); err != nil {
			return nil, err
		}
		normalizeWorkspaceMember(&member)
		result = append(result, member)
	}
	return result, rows.Err()
}

func (s *Store) AddWorkspaceMember(ctx context.Context, actorUserID string, member WorkspaceMember) (WorkspaceMember, error) {
	if !validNonOwnerWorkspaceRole(member.Role) || member.UserID == "" {
		return WorkspaceMember{}, errors.New("member user and non-owner role are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkspaceMember{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireTeamWorkspaceOwner(ctx, tx, actorUserID, member.WorkspaceID); err != nil {
		return WorkspaceMember{}, err
	}
	now := member.JoinedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO workspace_members(
workspace_id,user_id,role,created_by,joined_at,updated_at)
VALUES($1,$2,$3,$4,$5,$5)
RETURNING workspace_id,user_id,role,COALESCE(created_by,''),joined_at,updated_at`,
		member.WorkspaceID, member.UserID, member.Role, actorUserID, now.UTC()).Scan(
		&member.WorkspaceID, &member.UserID, &member.Role, &member.CreatedBy, &member.JoinedAt, &member.UpdatedAt)
	if err != nil {
		return WorkspaceMember{}, mapWorkspaceWriteError("add workspace member", err)
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: member.WorkspaceID, ActorUserID: actorUserID, Action: "member.added",
		EntityType: "user", EntityID: member.UserID, Metadata: mustJSON(map[string]any{"role": member.Role}), CreatedAt: now,
	}); err != nil {
		return WorkspaceMember{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkspaceMember{}, err
	}
	normalizeWorkspaceMember(&member)
	return member, nil
}

func (s *Store) UpdateWorkspaceMemberRole(ctx context.Context, actorUserID, workspaceID, userID, role string) (WorkspaceMember, error) {
	if !validNonOwnerWorkspaceRole(role) {
		return WorkspaceMember{}, errors.New("invalid workspace role")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkspaceMember{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireTeamWorkspaceOwner(ctx, tx, actorUserID, workspaceID); err != nil {
		return WorkspaceMember{}, err
	}
	var currentRole string
	if err := tx.QueryRowContext(ctx, `SELECT role FROM workspace_members
WHERE workspace_id=$1 AND user_id=$2 FOR UPDATE`, workspaceID, userID).Scan(&currentRole); errors.Is(err, sql.ErrNoRows) {
		return WorkspaceMember{}, ErrNotFound
	} else if err != nil {
		return WorkspaceMember{}, err
	}
	if currentRole == WorkspaceRoleOwner {
		return WorkspaceMember{}, ErrConflict
	}
	var member WorkspaceMember
	now := time.Now().UTC()
	err = tx.QueryRowContext(ctx, `UPDATE workspace_members SET role=$1,updated_at=$2
WHERE workspace_id=$3 AND user_id=$4
RETURNING workspace_id,user_id,role,COALESCE(created_by,''),joined_at,updated_at`,
		role, now, workspaceID, userID).Scan(&member.WorkspaceID, &member.UserID, &member.Role,
		&member.CreatedBy, &member.JoinedAt, &member.UpdatedAt)
	if err != nil {
		return WorkspaceMember{}, err
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "member.role_updated",
		EntityType: "user", EntityID: userID, Metadata: mustJSON(map[string]any{"role": role}), CreatedAt: now,
	}); err != nil {
		return WorkspaceMember{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkspaceMember{}, err
	}
	normalizeWorkspaceMember(&member)
	return member, nil
}

func (s *Store) RemoveWorkspaceMember(ctx context.Context, actorUserID, workspaceID, userID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireTeamWorkspaceOwner(ctx, tx, actorUserID, workspaceID); err != nil {
		return err
	}
	var role string
	if err := tx.QueryRowContext(ctx, `SELECT role FROM workspace_members WHERE workspace_id=$1 AND user_id=$2 FOR UPDATE`,
		workspaceID, userID).Scan(&role); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if role == WorkspaceRoleOwner {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_members WHERE workspace_id=$1 AND user_id=$2`, workspaceID, userID); err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "member.removed",
		EntityType: "user", EntityID: userID, Metadata: json.RawMessage(`{}`), CreatedAt: now,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) TransferWorkspaceOwnership(ctx context.Context, actorUserID, workspaceID, newOwnerUserID string) (Workspace, error) {
	return s.transferWorkspaceOwnership(ctx, actorUserID, workspaceID, newOwnerUserID, 0)
}

// TransferWorkspaceOwnershipLimited transfers ownership while atomically
// enforcing the recipient's retained team workspace ownership limit.
func (s *Store) TransferWorkspaceOwnershipLimited(
	ctx context.Context,
	actorUserID, workspaceID, newOwnerUserID string,
	maxOwnedTeamWorkspaces int,
) (Workspace, error) {
	if maxOwnedTeamWorkspaces <= 0 {
		return Workspace{}, errors.New("owned team workspace limit must be positive")
	}
	return s.transferWorkspaceOwnership(
		ctx, actorUserID, workspaceID, newOwnerUserID, maxOwnedTeamWorkspaces,
	)
}

func (s *Store) transferWorkspaceOwnership(
	ctx context.Context,
	actorUserID, workspaceID, newOwnerUserID string,
	maxOwnedTeamWorkspaces int,
) (Workspace, error) {
	if newOwnerUserID == "" || newOwnerUserID == actorUserID {
		return Workspace{}, ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Workspace{}, err
	}
	defer func() { _ = tx.Rollback() }()
	access, err := resolveWorkspaceAccess(ctx, tx, actorUserID, workspaceID)
	if err != nil {
		return Workspace{}, err
	}
	if access.Member.Role != WorkspaceRoleOwner || access.Workspace.IsPersonal {
		return Workspace{}, ErrConflict
	}
	// Serialize ownership changes on the workspace row. Two requests may both
	// resolve the actor as owner before either transaction reaches this point;
	// after waiting for the lock, only the request that still sees that owner is
	// allowed to continue.
	var lockedOwner string
	var lockedPersonal bool
	if err := tx.QueryRowContext(ctx, `SELECT owner_user_id,is_personal FROM workspaces
WHERE id=$1 AND archived_at IS NULL FOR UPDATE`, workspaceID).Scan(&lockedOwner, &lockedPersonal); errors.Is(err, sql.ErrNoRows) {
		return Workspace{}, ErrNotFound
	} else if err != nil {
		return Workspace{}, err
	}
	if lockedPersonal || lockedOwner != actorUserID {
		return Workspace{}, ErrConflict
	}
	var newOwnerRole string
	if err := tx.QueryRowContext(ctx, `SELECT role FROM workspace_members
WHERE workspace_id=$1 AND user_id=$2 FOR UPDATE`, workspaceID, newOwnerUserID).Scan(&newOwnerRole); errors.Is(err, sql.ErrNoRows) {
		return Workspace{}, ErrNotFound
	} else if err != nil {
		return Workspace{}, err
	}
	if maxOwnedTeamWorkspaces > 0 {
		if err := requireOwnedTeamWorkspaceCapacity(ctx, tx, newOwnerUserID, maxOwnedTeamWorkspaces); err != nil {
			return Workspace{}, err
		}
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE workspace_members SET role='editor',updated_at=$1
WHERE workspace_id=$2 AND user_id=$3 AND role='owner'`, now, workspaceID, actorUserID)
	if err != nil {
		return Workspace{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return Workspace{}, ErrConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE workspace_members SET role='owner',updated_at=$1
WHERE workspace_id=$2 AND user_id=$3 AND role=$4`, now, workspaceID, newOwnerUserID, newOwnerRole)
	if err != nil {
		return Workspace{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return Workspace{}, ErrConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE workspaces SET owner_user_id=$1,updated_at=$2
WHERE id=$3 AND owner_user_id=$4`, newOwnerUserID, now, workspaceID, actorUserID)
	if err != nil {
		return Workspace{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return Workspace{}, ErrConflict
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "workspace.owner_transferred",
		EntityType: "user", EntityID: newOwnerUserID,
		Metadata: mustJSON(map[string]any{"previous_owner_user_id": actorUserID}), CreatedAt: now,
	}); err != nil {
		return Workspace{}, err
	}
	if err := tx.Commit(); err != nil {
		return Workspace{}, err
	}
	return s.GetWorkspaceForUser(ctx, newOwnerUserID, workspaceID)
}

func requireOwnedTeamWorkspaceCapacity(
	ctx context.Context,
	tx *sql.Tx,
	ownerUserID string,
	maxOwnedTeamWorkspaces int,
) error {
	// The transaction-scoped advisory lock makes create and ownership-transfer
	// requests for the same prospective owner share one serialization point.
	// It avoids relying on a read-then-write count that concurrent requests could
	// all observe below the limit.
	lockKey := "maxposty:owned-team-workspaces:" + ownerUserID
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
		return fmt.Errorf("lock owned team workspace quota: %w", err)
	}
	var owned int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM workspaces
WHERE owner_user_id=$1 AND is_personal=FALSE`, ownerUserID).Scan(&owned); err != nil {
		return fmt.Errorf("count owned team workspaces: %w", err)
	}
	if owned >= maxOwnedTeamWorkspaces {
		return ErrOwnedTeamWorkspaceLimit
	}
	return nil
}

func (s *Store) CreateWorkspaceInvitation(ctx context.Context, actorUserID string, invitation WorkspaceInvitation) (WorkspaceInvitation, error) {
	invitation.Email = strings.ToLower(strings.TrimSpace(invitation.Email))
	if invitation.ID == "" {
		invitation.ID = newStoreID("wi_")
	}
	if !validNonOwnerWorkspaceRole(invitation.Role) {
		return WorkspaceInvitation{}, errors.New("invitation role is required")
	}
	if err := validateSHA256Hex("invitation token hash", invitation.TokenHash); err != nil {
		return WorkspaceInvitation{}, err
	}
	if invitation.CreatedAt.IsZero() {
		invitation.CreatedAt = time.Now().UTC()
	}
	if err := validateLifetime(invitation.CreatedAt, invitation.ExpiresAt); err != nil {
		return WorkspaceInvitation{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkspaceInvitation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireTeamWorkspaceOwner(ctx, tx, actorUserID, invitation.WorkspaceID); err != nil {
		return WorkspaceInvitation{}, err
	}
	invitation.Status, invitation.InvitedBy = InvitationStatusPending, actorUserID
	err = tx.QueryRowContext(ctx, `INSERT INTO workspace_invitations(
id,workspace_id,email,target_user_id,token_hash,role,status,invited_by,created_at,expires_at)
VALUES($1,$2,$3,NULLIF($4,''),$5,$6,'pending',$7,$8,$9)
RETURNING id,workspace_id,email,COALESCE(target_user_id,''),token_hash,role,status,invited_by,COALESCE(accepted_by,''),
created_at,expires_at,accepted_at,revoked_at`, invitation.ID, invitation.WorkspaceID,
		invitation.Email, invitation.TargetUserID, invitation.TokenHash, invitation.Role, actorUserID,
		invitation.CreatedAt.UTC(), invitation.ExpiresAt.UTC()).Scan(
		&invitation.ID, &invitation.WorkspaceID, &invitation.Email, &invitation.TargetUserID, &invitation.TokenHash,
		&invitation.Role, &invitation.Status, &invitation.InvitedBy, &invitation.AcceptedBy,
		&invitation.CreatedAt, &invitation.ExpiresAt, &invitation.AcceptedAt, &invitation.RevokedAt)
	if err != nil {
		return WorkspaceInvitation{}, mapWorkspaceWriteError("create workspace invitation", err)
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: invitation.WorkspaceID, ActorUserID: actorUserID, Action: "invitation.created",
		EntityType: "invitation", EntityID: invitation.ID,
		Metadata: mustJSON(map[string]any{"email": invitation.Email, "role": invitation.Role}), CreatedAt: invitation.CreatedAt,
	}); err != nil {
		return WorkspaceInvitation{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkspaceInvitation{}, err
	}
	normalizeInvitation(&invitation)
	return invitation, nil
}

func (s *Store) ListWorkspaceInvitations(ctx context.Context, actorUserID, workspaceID string, includeClosed bool) ([]WorkspaceInvitation, error) {
	if err := requireTeamWorkspaceOwner(ctx, s.db, actorUserID, workspaceID); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	_, _ = s.db.ExecContext(ctx, `UPDATE workspace_invitations SET status='expired'
WHERE workspace_id=$1 AND status='pending' AND expires_at<=$2`, workspaceID, now)
	where := `workspace_id=$1 AND status='pending'`
	if includeClosed {
		where = `workspace_id=$1`
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,workspace_id,email,COALESCE(target_user_id,''),token_hash,role,status,invited_by,
COALESCE(accepted_by,''),created_at,expires_at,accepted_at,revoked_at
FROM workspace_invitations WHERE `+where+` ORDER BY created_at DESC,id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]WorkspaceInvitation, 0)
	for rows.Next() {
		invitation, err := scanWorkspaceInvitation(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, invitation)
	}
	return result, rows.Err()
}

func (s *Store) RevokeWorkspaceInvitation(ctx context.Context, actorUserID, workspaceID, invitationID string, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireTeamWorkspaceOwner(ctx, tx, actorUserID, workspaceID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE workspace_invitations SET status='revoked',revoked_at=$1
WHERE workspace_id=$2 AND id=$3 AND status='pending'`, now.UTC(), workspaceID, invitationID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotFound
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, ActorUserID: actorUserID, Action: "invitation.revoked",
		EntityType: "invitation", EntityID: invitationID, Metadata: json.RawMessage(`{}`), CreatedAt: now,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AcceptWorkspaceInvitation(ctx context.Context, userID, tokenHash string, now time.Time) (WorkspaceMember, error) {
	if err := validateSHA256Hex("invitation token hash", tokenHash); err != nil {
		return WorkspaceMember{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkspaceMember{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var invitation WorkspaceInvitation
	err = tx.QueryRowContext(ctx, `SELECT i.id,i.workspace_id,i.email,COALESCE(i.target_user_id,''),i.token_hash,
i.role,i.status,i.invited_by,COALESCE(i.accepted_by,''),i.created_at,i.expires_at,i.accepted_at,i.revoked_at
FROM workspace_invitations i
JOIN workspaces w ON w.id=i.workspace_id
WHERE i.token_hash=$1 AND w.archived_at IS NULL
FOR UPDATE OF i,w`, tokenHash).Scan(
		&invitation.ID, &invitation.WorkspaceID, &invitation.Email, &invitation.TargetUserID, &invitation.TokenHash,
		&invitation.Role, &invitation.Status, &invitation.InvitedBy, &invitation.AcceptedBy,
		&invitation.CreatedAt, &invitation.ExpiresAt, &invitation.AcceptedAt, &invitation.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkspaceMember{}, ErrNotFound
	}
	if err != nil {
		return WorkspaceMember{}, err
	}
	if invitation.Status != InvitationStatusPending || !invitation.ExpiresAt.After(now) {
		if invitation.Status == InvitationStatusPending {
			_, _ = tx.ExecContext(ctx, `UPDATE workspace_invitations SET status='expired' WHERE id=$1`, invitation.ID)
			_ = tx.Commit()
		}
		return WorkspaceMember{}, ErrNotFound
	}
	var userEmail string
	if err := tx.QueryRowContext(ctx, `SELECT lower(email) FROM users WHERE id=$1`, userID).Scan(&userEmail); errors.Is(err, sql.ErrNoRows) {
		return WorkspaceMember{}, ErrNotFound
	} else if err != nil {
		return WorkspaceMember{}, err
	}
	if invitation.TargetUserID != "" && invitation.TargetUserID != userID {
		return WorkspaceMember{}, ErrNotFound
	}
	if invitation.Email != "" && !strings.EqualFold(strings.TrimSpace(userEmail), invitation.Email) {
		return WorkspaceMember{}, ErrNotFound
	}
	var member WorkspaceMember
	err = tx.QueryRowContext(ctx, `INSERT INTO workspace_members(
workspace_id,user_id,role,created_by,joined_at,updated_at)
VALUES($1,$2,$3,$4,$5,$5)
ON CONFLICT(workspace_id,user_id) DO UPDATE SET
role=CASE WHEN workspace_members.role='owner' THEN workspace_members.role ELSE excluded.role END,
updated_at=excluded.updated_at
RETURNING workspace_id,user_id,role,COALESCE(created_by,''),joined_at,updated_at`,
		invitation.WorkspaceID, userID, invitation.Role, invitation.InvitedBy, now.UTC()).Scan(
		&member.WorkspaceID, &member.UserID, &member.Role, &member.CreatedBy, &member.JoinedAt, &member.UpdatedAt)
	if err != nil {
		return WorkspaceMember{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE workspace_invitations SET status='accepted',accepted_by=$1,accepted_at=$2
WHERE id=$3`, userID, now.UTC(), invitation.ID)
	if err != nil {
		return WorkspaceMember{}, err
	}
	if err := appendAuditEventTx(ctx, tx, AuditEvent{
		WorkspaceID: invitation.WorkspaceID, ActorUserID: userID, Action: "invitation.accepted",
		EntityType: "invitation", EntityID: invitation.ID, Metadata: json.RawMessage(`{}`), CreatedAt: now,
	}); err != nil {
		return WorkspaceMember{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkspaceMember{}, err
	}
	normalizeWorkspaceMember(&member)
	return member, nil
}

func requireWorkspaceRole(ctx context.Context, q workspaceQueryer, userID, workspaceID string, roles ...string) error {
	access, err := resolveWorkspaceAccess(ctx, q, userID, workspaceID)
	if err != nil {
		return err
	}
	for _, role := range roles {
		if access.Member.Role == role {
			return nil
		}
	}
	return ErrNotFound
}

func requireTeamWorkspaceOwner(ctx context.Context, q workspaceQueryer, userID, workspaceID string) error {
	access, err := resolveWorkspaceAccess(ctx, q, userID, workspaceID)
	if err != nil {
		return err
	}
	if access.Member.Role != WorkspaceRoleOwner {
		return ErrNotFound
	}
	if access.Workspace.IsPersonal {
		return ErrConflict
	}
	return nil
}

func validNonOwnerWorkspaceRole(role string) bool {
	return role == WorkspaceRoleEditor || role == WorkspaceRoleApprover || role == WorkspaceRoleViewer
}

func scanWorkspaceInvitation(row scanner) (WorkspaceInvitation, error) {
	var invitation WorkspaceInvitation
	if err := row.Scan(&invitation.ID, &invitation.WorkspaceID, &invitation.Email, &invitation.TargetUserID, &invitation.TokenHash,
		&invitation.Role, &invitation.Status, &invitation.InvitedBy, &invitation.AcceptedBy,
		&invitation.CreatedAt, &invitation.ExpiresAt, &invitation.AcceptedAt, &invitation.RevokedAt); err != nil {
		return WorkspaceInvitation{}, err
	}
	normalizeInvitation(&invitation)
	return invitation, nil
}

func normalizeWorkspace(workspace *Workspace) {
	workspace.CreatedAt = workspace.CreatedAt.UTC()
	workspace.UpdatedAt = workspace.UpdatedAt.UTC()
	if workspace.ArchivedAt != nil {
		value := workspace.ArchivedAt.UTC()
		workspace.ArchivedAt = &value
	}
}

func normalizeWorkspaceMember(member *WorkspaceMember) {
	member.JoinedAt = member.JoinedAt.UTC()
	member.UpdatedAt = member.UpdatedAt.UTC()
}

func normalizeInvitation(invitation *WorkspaceInvitation) {
	invitation.CreatedAt = invitation.CreatedAt.UTC()
	invitation.ExpiresAt = invitation.ExpiresAt.UTC()
	if invitation.AcceptedAt != nil {
		value := invitation.AcceptedAt.UTC()
		invitation.AcceptedAt = &value
	}
	if invitation.RevokedAt != nil {
		value := invitation.RevokedAt.UTC()
		invitation.RevokedAt = &value
	}
}

func newStoreID(prefix string) string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		panic(fmt.Sprintf("read cryptographic randomness: %v", err))
	}
	return prefix + hex.EncodeToString(value)
}

func mapWorkspaceWriteError(operation string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return fmt.Errorf("%w: %s", ErrConflict, operation)
		case "23503":
			return ErrNotFound
		}
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}
