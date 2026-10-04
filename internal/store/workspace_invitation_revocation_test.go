package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestInvitationRevocationMigrationPreservesEarlierMemberRemoval(t *testing.T) {
	ctx := context.Background()
	testURL, db := newMigrationTestSchema(t)
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	cutoffIndex := -1
	for index, migration := range migrations {
		if migration.version == "035_workspace_invitation_revocation.sql" {
			cutoffIndex = index
			break
		}
	}
	if cutoffIndex <= 0 {
		t.Fatal("invitation revocation migration is not embedded")
	}
	if err := runMigrationSet(ctx, testURL, migrations[:cutoffIndex]); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, id := range []string{"legacy-owner", "legacy-guest", "workspace_compat_legacy"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO users(id,email,display_name,created_at,updated_at)
VALUES($1,$2,$1,$3,$3)`, id, id+"@example.test", now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workspaces(id,name,owner_user_id,compat_owner_user_id,is_personal,created_by,created_at,updated_at)
VALUES('legacy-team','Legacy team','legacy-owner','workspace_compat_legacy',FALSE,'legacy-owner',$1,$1)`, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO audit_events(workspace_id,actor_user_id,action,entity_type,entity_id,metadata,created_at)
VALUES('legacy-team','legacy-owner','member.removed','user','legacy-guest','{}',$1)`, now); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"shared", "email"} {
		email := ""
		if kind == "email" {
			email = "legacy-guest@example.test"
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO workspace_invitations(id,workspace_id,email,token_hash,role,status,invited_by,created_at,expires_at)
VALUES($1,'legacy-team',$2,$3,'editor','pending','legacy-owner',$4,$5)`, kind, email,
			billingTestDedupe(kind), now.Add(-time.Minute), now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := runMigrationSet(ctx, testURL, migrations); err != nil {
		t.Fatal(err)
	}
	var invalidBefore time.Time
	if err := db.QueryRowContext(ctx, `SELECT invalid_before FROM workspace_member_invitation_cutoffs
WHERE workspace_id='legacy-team' AND user_id='legacy-guest'`).Scan(&invalidBefore); err != nil || !invalidBefore.Equal(now) {
		t.Fatalf("past member removal cutoff was not preserved: cutoff=%s err=%v", invalidBefore, err)
	}
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM workspace_invitations WHERE id='email'`).Scan(&status); err != nil || status != InvitationStatusRevoked {
		t.Fatalf("old addressed invitation was not revoked: status=%s err=%v", status, err)
	}
	storage, err := OpenWithMigrationURL(ctx, testURL, testURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	if _, err := storage.AcceptWorkspaceInvitation(ctx, "legacy-guest", billingTestDedupe("shared"), time.Now().UTC()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old shared invitation bypassed pre-deployment removal: %v", err)
	}
}

func TestOldInvitationsCannotRestoreChangedOrRemovedMembership(t *testing.T) {
	for _, kind := range []string{"shared", "email", "user"} {
		t.Run(kind, func(t *testing.T) {
			storage := openWorkspaceTestStore(t, "invitation-revocation-"+kind)
			upsertWorkspaceUser(t, storage, "guest", "guest@example.test")
			upsertWorkspaceUser(t, storage, "other-guest", "other@example.test")
			workspace, err := storage.CreateWorkspace(t.Context(), "test-owner", Workspace{Name: "Invitations"})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			seedBillingContract(t, storage, workspace.ID, "pro", now.Add(-time.Hour), now.AddDate(0, 1, 0), "sealed-method")
			old := createRevocationTestInvitation(t, storage, workspace.ID, kind, "old")
			join := createRevocationTestInvitation(t, storage, workspace.ID, "shared", "join")
			if _, err := storage.AcceptWorkspaceInvitation(t.Context(), "guest", join.TokenHash, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if _, err := storage.UpdateWorkspaceMemberRole(t.Context(), "test-owner", workspace.ID, "guest", WorkspaceRoleViewer); err != nil {
				t.Fatal(err)
			}
			if _, err := storage.AcceptWorkspaceInvitation(t.Context(), "guest", old.TokenHash, time.Now().UTC()); !errors.Is(err, ErrNotFound) {
				t.Fatalf("old %s invitation restored downgraded access: %v", kind, err)
			}
			access, err := storage.ResolveWorkspaceAccess(t.Context(), "guest", workspace.ID)
			if err != nil || access.Member.Role != WorkspaceRoleViewer {
				t.Fatalf("downgraded role changed: %#v err=%v", access.Member, err)
			}
			if kind == "shared" {
				if _, err := storage.AcceptWorkspaceInvitation(t.Context(), "other-guest", old.TokenHash, time.Now().UTC()); err != nil {
					t.Fatalf("shared invitation should remain valid for another user: %v", err)
				}
			} else {
				var revoked, audited int
				if err := storage.db.QueryRowContext(t.Context(), `SELECT count(*) FROM workspace_invitations WHERE id=$1 AND status='revoked'`, old.ID).Scan(&revoked); err != nil {
					t.Fatal(err)
				}
				if err := storage.db.QueryRowContext(t.Context(), `SELECT count(*) FROM audit_events WHERE workspace_id=$1 AND action='invitation.revoked' AND entity_id=$2`, workspace.ID, old.ID).Scan(&audited); err != nil {
					t.Fatal(err)
				}
				if revoked != 1 || audited != 1 {
					t.Fatalf("addressed invitation was not revoked with audit evidence: revoked=%d audited=%d", revoked, audited)
				}
			}
			beforeRemoval := createRevocationTestInvitation(t, storage, workspace.ID, kind, "before-removal")
			if err := storage.RemoveWorkspaceMember(t.Context(), "test-owner", workspace.ID, "guest"); err != nil {
				t.Fatal(err)
			}
			if _, err := storage.AcceptWorkspaceInvitation(t.Context(), "guest", beforeRemoval.TokenHash, time.Now().UTC()); !errors.Is(err, ErrNotFound) {
				t.Fatalf("old %s invitation restored removed access: %v", kind, err)
			}
			if _, err := storage.ResolveWorkspaceAccess(t.Context(), "guest", workspace.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("removed membership reappeared: %v", err)
			}
			fresh := createRevocationTestInvitation(t, storage, workspace.ID, kind, "fresh")
			if member, err := storage.AcceptWorkspaceInvitation(t.Context(), "guest", fresh.TokenHash, time.Now().UTC()); err != nil || member.Role != WorkspaceRoleEditor {
				t.Fatalf("fresh owner-issued invitation must permit intentional rejoining: %#v err=%v", member, err)
			}
		})
	}
}

func TestInvitationsNeverChangeAnExistingMembersRole(t *testing.T) {
	storage := openWorkspaceTestStore(t, "invitation-existing-role")
	upsertWorkspaceUser(t, storage, "guest", "")
	workspace, err := storage.CreateWorkspace(t.Context(), "test-owner", Workspace{Name: "Existing member"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seedBillingContract(t, storage, workspace.ID, "pro", now.Add(-time.Hour), now.AddDate(0, 1, 0), "sealed-method")
	if _, err := storage.AddWorkspaceMember(t.Context(), "test-owner", WorkspaceMember{WorkspaceID: workspace.ID, UserID: "guest", Role: WorkspaceRoleViewer}); err != nil {
		t.Fatal(err)
	}
	invitation := createRevocationTestInvitation(t, storage, workspace.ID, "shared", "new-editor-invite")
	member, err := storage.AcceptWorkspaceInvitation(t.Context(), "guest", invitation.TokenHash, time.Now().UTC())
	if err != nil || member.Role != WorkspaceRoleViewer {
		t.Fatalf("invitation changed an existing role: %#v err=%v", member, err)
	}
}

func TestInvitationAcceptanceAndMemberRemovalSerialize(t *testing.T) {
	storage := openWorkspaceTestStore(t, "invitation-removal-race")
	upsertWorkspaceUser(t, storage, "guest", "")
	workspace, err := storage.CreateWorkspace(t.Context(), "test-owner", Workspace{Name: "Concurrent removal"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seedBillingContract(t, storage, workspace.ID, "pro", now.Add(-time.Hour), now.AddDate(0, 1, 0), "sealed-method")
	for iteration := 0; iteration < 12; iteration++ {
		if _, err := storage.AddWorkspaceMember(t.Context(), "test-owner", WorkspaceMember{WorkspaceID: workspace.ID, UserID: "guest", Role: WorkspaceRoleEditor}); err != nil {
			t.Fatal(err)
		}
		invitation := createRevocationTestInvitation(t, storage, workspace.ID, "shared", fmt.Sprint(iteration))
		start := make(chan struct{})
		accepted := make(chan error, 1)
		removed := make(chan error, 1)
		go func() {
			<-start
			_, err := storage.AcceptWorkspaceInvitation(t.Context(), "guest", invitation.TokenHash, time.Now().UTC())
			accepted <- err
		}()
		go func() {
			<-start
			removed <- storage.RemoveWorkspaceMember(t.Context(), "test-owner", workspace.ID, "guest")
		}()
		close(start)
		if err := <-accepted; err != nil && !errors.Is(err, ErrNotFound) {
			t.Fatalf("concurrent accept: %v", err)
		}
		if err := <-removed; err != nil {
			t.Fatalf("concurrent removal: %v", err)
		}
		if _, err := storage.ResolveWorkspaceAccess(t.Context(), "guest", workspace.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("concurrent old invitation undid member removal: %v", err)
		}
	}
}

func createRevocationTestInvitation(t *testing.T, storage *Store, workspaceID, kind, label string) WorkspaceInvitation {
	t.Helper()
	now := time.Now().UTC()
	invitation := WorkspaceInvitation{
		WorkspaceID: workspaceID, Role: WorkspaceRoleEditor, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		TokenHash: billingTestDedupe(workspaceID + ":" + label),
	}
	switch kind {
	case "email":
		invitation.Email = "guest@example.test"
	case "user":
		invitation.TargetUserID = "guest"
	}
	created, err := storage.CreateWorkspaceInvitation(t.Context(), "test-owner", invitation)
	if err != nil {
		t.Fatal(err)
	}
	return created
}
