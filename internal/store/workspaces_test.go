package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceMigrationBackfillsPersonalTenant(t *testing.T) {
	ctx := context.Background()
	testURL, db := newMigrationTestSchema(t)
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	const workspaceMigration = "016_workspaces.sql"
	workspaceIndex := -1
	for index, migration := range migrations {
		if migration.version == workspaceMigration {
			workspaceIndex = index
			break
		}
	}
	if workspaceIndex <= 0 {
		t.Fatalf("%s not found", workspaceMigration)
	}
	if err := runMigrationSet(ctx, testURL, migrations[:workspaceIndex]); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := db.ExecContext(ctx, `INSERT INTO users(id,display_name,created_at,updated_at)
VALUES('legacy-owner','Legacy owner',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	var postID int64
	if err := db.QueryRowContext(ctx, `INSERT INTO posts(owner_id,title,content,created_at,updated_at)
VALUES('legacy-owner','Legacy','body',$1,$1) RETURNING id`, now).Scan(&postID); err != nil {
		t.Fatal(err)
	}
	if err := runMigrationSet(ctx, testURL, migrations); err != nil {
		t.Fatal(err)
	}
	var workspaceID, postWorkspaceID, role string
	if err := db.QueryRowContext(ctx, `SELECT w.id,p.workspace_id,wm.role
FROM workspaces w JOIN workspace_members wm ON wm.workspace_id=w.id
JOIN posts p ON p.owner_id=w.owner_user_id
WHERE w.owner_user_id='legacy-owner' AND w.is_personal AND p.id=$1`, postID).Scan(
		&workspaceID, &postWorkspaceID, &role); err != nil {
		t.Fatal(err)
	}
	if workspaceID == "" || postWorkspaceID != workspaceID || role != WorkspaceRoleOwner {
		t.Fatalf("backfill=(%q,%q,%q)", workspaceID, postWorkspaceID, role)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO channel_claims(
id,token_hash,owner_id,max_chat_id,requester_label,comparison_code,status,created_at,expires_at,updated_at)
VALUES('legacy-claim',$1,'legacy-owner','123','Legacy','123456','pending',$2,$3,$2)`,
		strings.Repeat("c", 64), now, now.Add(time.Hour)); err != nil {
		t.Fatalf("legacy channel claim insert after migration: %v", err)
	}
	var claimWorkspaceID, requestedBy string
	if err := db.QueryRowContext(ctx, `SELECT workspace_id,requested_by_user_id FROM channel_claims
WHERE id='legacy-claim'`).Scan(&claimWorkspaceID, &requestedBy); err != nil || claimWorkspaceID != workspaceID || requestedBy != "legacy-owner" {
		t.Fatalf("legacy claim compat=(%q,%q) err=%v", claimWorkspaceID, requestedBy, err)
	}
	var revisionID int64
	if err := db.QueryRowContext(ctx, `SELECT current_revision_id FROM posts WHERE id=$1`, postID).Scan(&revisionID); err != nil || revisionID == 0 {
		t.Fatalf("initial revision=%d err=%v", revisionID, err)
	}
}

func TestWorkspaceMembershipInvitationAndOwnership(t *testing.T) {
	ctx := context.Background()
	storage := openWorkspaceTestStore(t, "membership")
	upsertWorkspaceUser(t, storage, "editor", "editor@example.test")
	upsertWorkspaceUser(t, storage, "approver", "")
	upsertWorkspaceUser(t, storage, "outsider", "outside@example.test")

	personal, err := storage.ListWorkspaces(ctx, "editor")
	if err != nil || len(personal) != 1 || !personal[0].Workspace.IsPersonal {
		t.Fatalf("new user personal workspace=%#v err=%v", personal, err)
	}
	if _, err := storage.AddWorkspaceMember(ctx, "editor", WorkspaceMember{
		WorkspaceID: personal[0].Workspace.ID, UserID: "outsider", Role: WorkspaceRoleViewer,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("personal workspace accepted a member: %v", err)
	}
	if _, err := storage.UpdateWorkspaceMemberRole(ctx, "editor", personal[0].Workspace.ID, "editor", WorkspaceRoleViewer); !errors.Is(err, ErrConflict) {
		t.Fatalf("personal workspace accepted a role mutation: %v", err)
	}
	if err := storage.RemoveWorkspaceMember(ctx, "editor", personal[0].Workspace.ID, "editor"); !errors.Is(err, ErrConflict) {
		t.Fatalf("personal workspace accepted member removal: %v", err)
	}
	personalInvite := WorkspaceInvitation{
		WorkspaceID: personal[0].Workspace.ID, Role: WorkspaceRoleViewer,
		TokenHash: strings.Repeat("d", 64), CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if _, err := storage.CreateWorkspaceInvitation(ctx, "editor", personalInvite); !errors.Is(err, ErrConflict) {
		t.Fatalf("personal workspace accepted invitation: %v", err)
	}
	if err := storage.RevokeWorkspaceInvitation(ctx, "editor", personal[0].Workspace.ID, "missing", time.Now().UTC()); !errors.Is(err, ErrConflict) {
		t.Fatalf("personal workspace accepted invitation revoke: %v", err)
	}
	requireApproval := true
	if _, err := storage.UpdateWorkspace(ctx, "editor", personal[0].Workspace.ID, WorkspaceChanges{ApprovalRequired: &requireApproval}); !errors.Is(err, ErrConflict) {
		t.Fatalf("personal workspace accepted approval workflow: %v", err)
	}
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Agency"})
	if err != nil {
		t.Fatal(err)
	}
	if !workspace.ApprovalRequired || !workspace.RequireDistinctApprover ||
		workspace.CompatOwnerUserID == "test-owner" || !strings.HasPrefix(workspace.CompatOwnerUserID, "workspace_compat_") {
		t.Fatalf("team defaults=%#v", workspace)
	}
	var syntheticPersonalCount int
	if err := storage.db.QueryRowContext(ctx, `SELECT count(*) FROM workspaces
WHERE owner_user_id=$1 AND is_personal`, workspace.CompatOwnerUserID).Scan(&syntheticPersonalCount); err != nil || syntheticPersonalCount != 0 {
		t.Fatalf("synthetic compat owner personal workspaces=%d err=%v", syntheticPersonalCount, err)
	}
	if _, err := storage.ResolveWorkspaceAccess(ctx, "outsider", workspace.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider access=%v", err)
	}
	member, err := storage.AddWorkspaceMember(ctx, "test-owner", WorkspaceMember{
		WorkspaceID: workspace.ID, UserID: "editor", Role: WorkspaceRoleEditor,
	})
	if err != nil || member.Role != WorkspaceRoleEditor {
		t.Fatalf("add editor=%#v err=%v", member, err)
	}
	if _, err := storage.TransferWorkspaceOwnership(ctx, "test-owner", workspace.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	oldOwner, err := storage.ResolveWorkspaceAccess(ctx, "test-owner", workspace.ID)
	if err != nil || oldOwner.Member.Role != WorkspaceRoleEditor {
		t.Fatalf("old owner=%#v err=%v", oldOwner, err)
	}
	newOwner, err := storage.ResolveWorkspaceAccess(ctx, "editor", workspace.ID)
	if err != nil || newOwner.Member.Role != WorkspaceRoleOwner || newOwner.Workspace.CompatOwnerUserID != workspace.CompatOwnerUserID {
		t.Fatalf("new owner=%#v err=%v", newOwner, err)
	}

	now := time.Now().UTC()
	invitation, err := storage.CreateWorkspaceInvitation(ctx, "editor", WorkspaceInvitation{
		WorkspaceID: workspace.ID, TargetUserID: "approver", Role: WorkspaceRoleApprover,
		TokenHash: strings.Repeat("a", 64), CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil || invitation.Email != "" || invitation.TokenHash == "" {
		t.Fatalf("bearer invitation=%#v err=%v", invitation, err)
	}
	accepted, err := storage.AcceptWorkspaceInvitation(ctx, "approver", invitation.TokenHash, now.Add(time.Minute))
	if err != nil || accepted.Role != WorkspaceRoleApprover {
		t.Fatalf("accept=%#v err=%v", accepted, err)
	}
	if _, err := storage.AcceptWorkspaceInvitation(ctx, "approver", invitation.TokenHash, now.Add(2*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replayed invitation=%v", err)
	}
	expired, err := storage.CreateWorkspaceInvitation(ctx, "editor", WorkspaceInvitation{
		WorkspaceID: workspace.ID, Role: WorkspaceRoleViewer, TokenHash: strings.Repeat("b", 64),
		CreatedAt: now, ExpiresAt: now.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.AcceptWorkspaceInvitation(ctx, "outsider", expired.TokenHash, now.Add(time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired invitation=%v", err)
	}
}

func TestWorkspaceOwnershipTransferSerializesConcurrentRequests(t *testing.T) {
	ctx := context.Background()
	storage := openWorkspaceTestStore(t, "ownership-transfer-race")
	for _, userID := range []string{"transfer-first", "transfer-second"} {
		upsertWorkspaceUser(t, storage, userID, userID+"@example.test")
	}
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Ownership race"})
	if err != nil {
		t.Fatal(err)
	}
	for _, userID := range []string{"transfer-first", "transfer-second"} {
		if _, err := storage.AddWorkspaceMember(ctx, "test-owner", WorkspaceMember{
			WorkspaceID: workspace.ID, UserID: userID, Role: WorkspaceRoleEditor,
		}); err != nil {
			t.Fatal(err)
		}
	}

	results := make(chan error, 2)
	for _, userID := range []string{"transfer-first", "transfer-second"} {
		userID := userID
		go func() {
			_, transferErr := storage.TransferWorkspaceOwnership(ctx, "test-owner", workspace.ID, userID)
			results <- transferErr
		}()
	}
	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatalf("concurrent transfer error=%v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent transfers=%d, want 1", successes)
	}

	var ownerCount int
	var workspaceOwner, memberOwner string
	if err := storage.db.QueryRowContext(ctx, `SELECT count(*) FROM workspace_members
WHERE workspace_id=$1 AND role='owner'`, workspace.ID).Scan(&ownerCount); err != nil {
		t.Fatal(err)
	}
	if err := storage.db.QueryRowContext(ctx, `SELECT owner_user_id FROM workspaces WHERE id=$1`, workspace.ID).Scan(&workspaceOwner); err != nil {
		t.Fatal(err)
	}
	if err := storage.db.QueryRowContext(ctx, `SELECT user_id FROM workspace_members
WHERE workspace_id=$1 AND role='owner'`, workspace.ID).Scan(&memberOwner); err != nil {
		t.Fatal(err)
	}
	if ownerCount != 1 || workspaceOwner != memberOwner || workspaceOwner == "test-owner" {
		t.Fatalf("ownership invariant count=%d workspace=%q member=%q", ownerCount, workspaceOwner, memberOwner)
	}
}

func TestOwnedTeamWorkspaceLimitSerializesCreateAndTransfer(t *testing.T) {
	ctx := context.Background()
	storage := openWorkspaceTestStore(t, "owned-workspace-limit-race")
	for _, userID := range []string{"quota-candidate", "quota-source"} {
		upsertWorkspaceUser(t, storage, userID, userID+"@example.test")
	}
	incoming, err := storage.CreateWorkspace(ctx, "quota-source", Workspace{Name: "Incoming"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.AddWorkspaceMember(ctx, "quota-source", WorkspaceMember{
		WorkspaceID: incoming.ID, UserID: "quota-candidate", Role: WorkspaceRoleEditor,
	}); err != nil {
		t.Fatal(err)
	}

	results := make(chan error, 2)
	go func() {
		_, createErr := storage.CreateWorkspaceLimited(
			ctx, "quota-candidate", Workspace{Name: "Created concurrently"}, 1,
		)
		results <- createErr
	}()
	go func() {
		_, transferErr := storage.TransferWorkspaceOwnershipLimited(
			ctx, "quota-source", incoming.ID, "quota-candidate", 1,
		)
		results <- transferErr
	}()

	successes := 0
	limitErrors := 0
	for range 2 {
		switch err := <-results; {
		case err == nil:
			successes++
		case errors.Is(err, ErrOwnedTeamWorkspaceLimit):
			limitErrors++
		default:
			t.Fatalf("concurrent ownership mutation error=%v", err)
		}
	}
	if successes != 1 || limitErrors != 1 {
		t.Fatalf("concurrent ownership results: successes=%d limit_errors=%d", successes, limitErrors)
	}
	var owned int
	if err := storage.db.QueryRowContext(ctx, `SELECT count(*) FROM workspaces
WHERE owner_user_id=$1 AND is_personal=FALSE`, "quota-candidate").Scan(&owned); err != nil {
		t.Fatal(err)
	}
	if owned != 1 {
		t.Fatalf("owned team workspaces=%d, want 1", owned)
	}
}

func TestOwnedTeamWorkspaceLimitCountsArchivedStorage(t *testing.T) {
	ctx := context.Background()
	storage := openWorkspaceTestStore(t, "owned-workspace-limit-archive")
	workspace, err := storage.CreateWorkspaceLimited(
		ctx, "test-owner", Workspace{Name: "Retained archive"}, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteWorkspace(ctx, "test-owner", workspace.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateWorkspaceLimited(
		ctx, "test-owner", Workspace{Name: "Quota bypass"}, 1,
	); !errors.Is(err, ErrOwnedTeamWorkspaceLimit) {
		t.Fatalf("create after archive error=%v, want ErrOwnedTeamWorkspaceLimit", err)
	}
}

func TestWorkspaceReviewInvalidationCommentsAndArchiveAudit(t *testing.T) {
	ctx := context.Background()
	storage := openWorkspaceTestStore(t, "review")
	upsertWorkspaceUser(t, storage, "editor", "editor@example.test")
	upsertWorkspaceUser(t, storage, "approver", "approver@example.test")
	if _, err := storage.db.ExecContext(ctx, `UPDATE users SET display_name='' WHERE id='approver'`); err != nil {
		t.Fatal(err)
	}
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Review team"})
	if err != nil {
		t.Fatal(err)
	}
	for userID, role := range map[string]string{"editor": WorkspaceRoleEditor, "approver": WorkspaceRoleApprover} {
		if _, err := storage.AddWorkspaceMember(ctx, "test-owner", WorkspaceMember{WorkspaceID: workspace.ID, UserID: userID, Role: role}); err != nil {
			t.Fatal(err)
		}
	}
	ownerPost, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{Title: "Owner draft", Content: "Body"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ownerRevision, err := storage.SubmitPostForReview(ctx, "test-owner", workspace.ID, ownerPost.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.DecidePostReview(ctx, "test-owner", workspace.ID, ownerPost.ID, ownerRevision.ID,
		ReviewDecisionApproved, "self", now.Add(time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("distinct approver decision=%v", err)
	}
	if _, err := storage.DecidePostReview(ctx, "approver", workspace.ID, ownerPost.ID, ownerRevision.ID,
		ReviewDecisionApproved, "OK", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePostForWorkspace(ctx, "editor", workspace.ID, Post{
		Title: "Draft", Content: "Body", ImageURL: "https://example.test/review.png", ImagePath: "review.png",
	})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := storage.SubmitPostForReview(ctx, "editor", workspace.ID, post.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if revision.AuthorDisplayName != "editor" {
		t.Fatalf("revision author display name=%q", revision.AuthorDisplayName)
	}
	var revisionSnapshot map[string]any
	if err := json.Unmarshal(revision.Snapshot, &revisionSnapshot); err != nil {
		t.Fatal(err)
	}
	if revisionSnapshot["image_url"] != post.ImageURL || revisionSnapshot["image_path"] != post.ImagePath {
		t.Fatalf("revision omitted publish image fields: %#v", revisionSnapshot)
	}
	decision, err := storage.DecidePostReview(ctx, "approver", workspace.ID, post.ID, revision.ID,
		ReviewDecisionApproved, "OK", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if decision.ReviewerDisplayName != "approver" {
		t.Fatalf("reviewer display name=%q", decision.ReviewerDisplayName)
	}
	if strings.Contains(decision.ReviewerDisplayName, "@") {
		t.Fatalf("reviewer email leaked as display name=%q", decision.ReviewerDisplayName)
	}
	if approved, err := storage.IsCurrentRevisionApproved(ctx, workspace.ID, post.ID); err != nil || !approved {
		t.Fatalf("approved=%v err=%v", approved, err)
	}
	post, err = storage.GetPostForWorkspace(ctx, "editor", workspace.ID, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	changedTitle := "Edited after approval"
	if _, err := storage.UpdatePostForWorkspaceIfUnchanged(ctx, "editor", workspace.ID, post, PostChanges{Title: &changedTitle}); err != nil {
		t.Fatal(err)
	}
	if approved, err := storage.IsCurrentRevisionApproved(ctx, workspace.ID, post.ID); err != nil || approved {
		t.Fatalf("approval survived payload edit=%v err=%v", approved, err)
	}
	revision, err = storage.SubmitPostForReview(ctx, "editor", workspace.ID, post.ID, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.DecidePostReview(ctx, "editor", workspace.ID, post.ID, revision.ID,
		ReviewDecisionApproved, "self", now.Add(3*time.Second)); !errors.Is(err, ErrNotFound) {
		// Editors cannot decide at all; distinct-author protection is separately
		// exercised with the owner below.
		t.Fatalf("editor decision=%v", err)
	}
	if _, err := storage.DecidePostReview(ctx, "approver", workspace.ID, post.ID, revision.ID,
		ReviewDecisionApproved, "OK", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.ExecContext(ctx, `INSERT INTO media_assets(
owner_id,workspace_id,filename,created_at,size_bytes,state,reservation_token,updated_at)
VALUES($1,$2,'attachment.png',$3,10,'ready','',$3)`, workspace.CompatOwnerUserID, workspace.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.ExecContext(ctx, `INSERT INTO post_attachments(
owner_id,workspace_id,post_id,type,position,storage_key,size_bytes,mime_type)
VALUES($1,$2,$3,'image',0,'attachment.png',10,'image/png')`,
		workspace.CompatOwnerUserID, workspace.ID, post.ID); err != nil {
		t.Fatal(err)
	}
	if approved, err := storage.IsCurrentRevisionApproved(ctx, workspace.ID, post.ID); err != nil || approved {
		t.Fatalf("approval survived attachment edit=%v err=%v", approved, err)
	}

	comment, err := storage.CreatePostComment(ctx, "editor", PostComment{WorkspaceID: workspace.ID, PostID: post.ID, Body: "Please check"})
	if err != nil {
		t.Fatal(err)
	}
	if comment.AuthorDisplayName != "editor" {
		t.Fatalf("comment author display name=%q", comment.AuthorDisplayName)
	}
	if _, err := storage.CreatePostComment(ctx, "approver", PostComment{
		WorkspaceID: workspace.ID, PostID: post.ID, Body: "Reviewer note",
	}); err != nil {
		t.Fatal(err)
	}
	notifications, err := storage.ListNotifications(ctx, "editor", workspace.ID, false, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	foundCommentNotification := false
	for _, notification := range notifications {
		if notification.Kind == NotificationKindCommentCreated && notification.EntityID == fmt.Sprint(post.ID) {
			foundCommentNotification = true
			break
		}
	}
	if !foundCommentNotification {
		t.Fatalf("post author did not receive comment notification: %#v", notifications)
	}
	second, err := storage.CreatePostForWorkspace(ctx, "editor", workspace.ID, Post{Title: "Second", Content: "Body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreatePostComment(ctx, "editor", PostComment{
		WorkspaceID: workspace.ID, PostID: second.ID, ParentID: &comment.ID, Body: "cross-post",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-post parent=%v", err)
	}
	resolved, err := storage.ResolvePostComment(ctx, "editor", workspace.ID, comment.ID, true, now)
	if err != nil || resolved.ResolvedAt == nil || resolved.ResolvedByUserID != "editor" ||
		resolved.AuthorDisplayName != "editor" || resolved.ResolvedByDisplayName != "editor" {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}
	auditEvents, err := storage.ListAuditEvents(ctx, "test-owner", workspace.ID, 20, 0)
	if err != nil || len(auditEvents) == 0 || auditEvents[0].ActorDisplayName == "" {
		t.Fatalf("audit display names=%#v err=%v", auditEvents, err)
	}
	if _, err := storage.SetPostScheduled(ctx, second.ID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteWorkspace(ctx, "test-owner", workspace.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("workspace archived with a scheduled post: %v", err)
	}
	if _, err := storage.CancelSchedule(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	archiveTokenHash := strings.Repeat("f", 64)
	archiveInvite, err := storage.CreateWorkspaceInvitation(ctx, "test-owner", WorkspaceInvitation{
		WorkspaceID: workspace.ID, Role: WorkspaceRoleViewer, TokenHash: archiveTokenHash,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteWorkspace(ctx, "test-owner", workspace.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.SetPostScheduled(ctx, second.ID, now.Add(time.Hour)); err == nil {
		t.Fatal("archived workspace accepted a post lifecycle update")
	}
	if _, err := storage.CreatePost(ctx, Post{
		UserID: workspace.CompatOwnerUserID, WorkspaceID: workspace.ID,
		Title: "Late post", Content: "Body", Format: FormatMarkdown,
	}); err == nil {
		t.Fatal("archived workspace accepted a new post")
	}
	if _, err := storage.AcceptWorkspaceInvitation(ctx, "approver", archiveTokenHash, now.Add(time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("archived workspace invitation accepted: %v", err)
	}
	var archivedInviteStatus string
	if err := storage.db.QueryRowContext(ctx, `SELECT status FROM workspace_invitations WHERE id=$1`, archiveInvite.ID).Scan(&archivedInviteStatus); err != nil || archivedInviteStatus != InvitationStatusRevoked {
		t.Fatalf("archived invitation status=%q err=%v", archivedInviteStatus, err)
	}
	if _, err := storage.ResolveWorkspaceAccess(ctx, "test-owner", workspace.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("archived access=%v", err)
	}
	if due, err := storage.DuePostIDs(ctx, now, 100); err != nil || len(due) != 0 {
		t.Fatalf("archived workspace due posts=%v err=%v", due, err)
	}
	var auditCount int
	if err := storage.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE workspace_id=$1`, workspace.ID).Scan(&auditCount); err != nil || auditCount == 0 {
		t.Fatalf("audit count=%d err=%v", auditCount, err)
	}
}

func TestWorkspaceQuotasAreIsolated(t *testing.T) {
	ctx := context.Background()
	storage := openWorkspaceTestStore(t, "quota")
	first, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "First"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Second"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	limits := AILimits{PerMinute: 1, PerDay: 1, MaxConcurrent: 2, LeaseTTL: time.Minute}
	if _, err := storage.AcquireWorkspaceAILease(ctx, first.ID, AIOperationImage, limits, now); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.AcquireWorkspaceAILease(ctx, first.ID, AIOperationImage, limits, now); err == nil {
		t.Fatal("first workspace exceeded AI quota")
	}
	if _, err := storage.AcquireWorkspaceAILease(ctx, second.ID, AIOperationImage, limits, now); err != nil {
		t.Fatalf("second workspace shared AI quota: %v", err)
	}
	mediaLimits := MediaLimits{MaxFiles: 1, MaxBytes: 100}
	if _, err := storage.ReserveMediaForWorkspace(ctx, "test-owner", first.ID, "first.png", 10, mediaLimits, now); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ReserveMediaForWorkspace(ctx, "test-owner", first.ID, "blocked.png", 10, mediaLimits, now); !errors.Is(err, ErrMediaQuotaExceeded) {
		t.Fatalf("first workspace media overflow=%v", err)
	}
	if _, err := storage.ReserveMediaForWorkspace(ctx, "test-owner", second.ID, "second.png", 10, mediaLimits, now); err != nil {
		t.Fatalf("second workspace shared media quota: %v", err)
	}
}

func openWorkspaceTestStore(t *testing.T, name string) *Store {
	t.Helper()
	storage, err := Open(context.Background(), filepath.Join(t.TempDir(), name+".db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	return storage
}

func upsertWorkspaceUser(t *testing.T, storage *Store, userID, email string) {
	t.Helper()
	if err := storage.UpsertUser(context.Background(), User{ID: userID, Email: email, DisplayName: userID}); err != nil {
		t.Fatal(err)
	}
}
