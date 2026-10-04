package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func seedExternalEditTestSnapshot(t *testing.T, s *Store, actor, workspace, connection string, now time.Time) {
	t.Helper()
	generation, err := s.ClaimDirectExternalCampaignSync(context.Background(), actor, workspace, connection, now, 0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	err = s.ReplaceDirectExternalCampaigns(context.Background(), actor, workspace, connection, generation, []DirectExternalCampaign{{ProviderCampaignID: 77, Name: "Provider campaign", CampaignType: "TEXT_CAMPAIGN", ProviderStatus: "ACCEPTED", ProviderState: "ON", ProviderStatusPayment: "ALLOWED", StartsAt: now, Timezone: "Europe/Moscow"}}, now)
	if err != nil {
		t.Fatal(err)
	}
}
func TestExternalEditClaimConcurrencyReadbackAndBookmarksSurviveSync(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, owner, workspace := newDirectStoreFixture(t, ctx)
	connection := connectDirectTestAccount(t, ctx, s, owner, workspace.ID)
	now := time.Now().UTC()
	seedExternalEditTestSnapshot(t, s, owner, workspace.ID, connection.ID, now)
	baseline, desired := strings.Repeat("a", 64), strings.Repeat("b", 64)
	control, err := s.ObserveDirectExternalEdit(ctx, owner, workspace.ID, connection.ID, 77, baseline, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BookmarkDirectExternalCampaign(ctx, owner, workspace.ID, connection.ID, 77, true); err != nil {
		t.Fatal(err)
	}
	seedExternalEditTestSnapshot(t, s, owner, workspace.ID, connection.ID, now.Add(time.Second))
	control, err = s.ObserveDirectExternalEdit(ctx, owner, workspace.ID, connection.ID, 77, baseline, now)
	if err != nil || !control.Bookmarked {
		t.Fatalf("bookmark lost on sync: %+v %v", control, err)
	}
	fence := DirectExternalEditFence{ConnectionID: connection.ID, Version: control.Version, ObservedHash: control.ObservedHash, RevisionID: control.RevisionID}
	var wg sync.WaitGroup
	results := make(chan string, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			operation, err := s.ClaimDirectExternalEdit(ctx, owner, workspace.ID, 77, fence, desired, now)
			if err != nil {
				errs <- err
			} else {
				results <- operation
			}
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	if len(results) != 1 || len(errs) != 1 {
		t.Fatalf("concurrent claims success=%d failure=%d", len(results), len(errs))
	}
	operation := <-results
	for err := range errs {
		if !errors.Is(err, ErrDirectProviderOperationBusy) {
			t.Fatal(err)
		}
	}
	if err = s.BeginDirectExternalWrite(ctx, owner, workspace.ID, connection.ID, 77, operation, now); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishDirectExternalEdit(ctx, workspace.ID, connection.ID, 77, operation, true); err != nil {
		t.Fatal(err)
	}
	blocked, err := s.ObserveDirectExternalEdit(ctx, owner, workspace.ID, connection.ID, 77, baseline, now.Add(time.Hour))
	if err != nil || blocked.EditState != "uncertain" {
		t.Fatalf("ambiguous mutation unlocked by different readback: %+v %v", blocked, err)
	}
	restored, err := s.ObserveDirectExternalEdit(ctx, owner, workspace.ID, connection.ID, 77, desired, now.Add(time.Hour))
	if err != nil || restored.EditState != "idle" || restored.ObservedHash != desired || !restored.Bookmarked {
		t.Fatalf("exact readback did not settle: %+v %v", restored, err)
	}
	if err = s.FinishDirectExternalEdit(ctx, workspace.ID, connection.ID, 77, operation, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale finalizer changed completed operation: %v", err)
	}
	if _, err = s.GetDirectExternalEditContext(ctx, "outsider", workspace.ID, connection.ID, 77, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant access: %v", err)
	}
	if _, err = s.GetDirectExternalEditContext(ctx, owner, workspace.ID, "other-account", 77, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("connection fence: %v", err)
	}
}
func TestExternalExpiredPrewriteClaimCannotSendAndManagedCampaignCannotBypassGraph(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, owner, workspace := newDirectStoreFixture(t, ctx)
	connection := connectDirectTestAccount(t, ctx, s, owner, workspace.ID)
	now := time.Now().UTC()
	seedExternalEditTestSnapshot(t, s, owner, workspace.ID, connection.ID, now)
	hash := strings.Repeat("a", 64)
	control, err := s.ObserveDirectExternalEdit(ctx, owner, workspace.ID, connection.ID, 77, hash, now)
	if err != nil {
		t.Fatal(err)
	}
	fence := DirectExternalEditFence{connection.ID, control.Version, control.ObservedHash, control.RevisionID}
	operation, err := s.ClaimDirectExternalEdit(ctx, owner, workspace.ID, 77, fence, strings.Repeat("b", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.ObserveDirectExternalEdit(ctx, owner, workspace.ID, connection.ID, 77, hash, now.Add(3*time.Minute))
	if err != nil || current.EditState != "idle" {
		t.Fatal("unsent expired claim not released", err)
	}
	if err = s.BeginDirectExternalWrite(ctx, owner, workspace.ID, connection.ID, 77, operation, now.Add(3*time.Minute)); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired worker could write: %v", err)
	}
	campaign := createDirectTestCampaign(t, ctx, s, owner, workspace.ID, now)
	campaign = acceptDirectTestCampaign(t, ctx, s, owner, workspace.ID, campaign, now)
	if _, err = s.db.ExecContext(ctx, `UPDATE direct_external_campaigns SET provider_campaign_id=$1 WHERE workspace_id=$2 AND provider_campaign_id=77`, *campaign.ProviderCampaignID, workspace.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetDirectExternalEditContext(ctx, owner, workspace.ID, connection.ID, *campaign.ProviderCampaignID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("managed graph bypass: %v", err)
	}
}
func TestProviderBudgetPatchRequiresSpendRoleOnlyForActualChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, owner, personal := newDirectStoreFixture(t, ctx)
	_ = personal
	team, err := s.CreateWorkspace(ctx, owner, Workspace{Name: "Direct team"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seedBillingContract(t, s, team.ID, "pro", now.Add(-time.Hour), now.AddDate(0, 1, 0), "test-method")
	for _, role := range []string{WorkspaceRoleEditor, WorkspaceRoleApprover, WorkspaceRoleViewer} {
		user := "member-" + role
		if err = s.UpsertUser(ctx, User{ID: user, DisplayName: user}); err != nil {
			t.Fatal(err)
		}
		if _, err = s.AddWorkspaceMember(ctx, owner, WorkspaceMember{WorkspaceID: team.ID, UserID: user, Role: role}); err != nil {
			t.Fatal(err)
		}
	}
	connectDirectTestAccount(t, ctx, s, owner, team.ID)
	campaign := createDirectTestCampaign(t, ctx, s, owner, team.ID, now)
	campaign = acceptDirectTestCampaign(t, ctx, s, owner, team.ID, campaign, now)
	changed := campaign.WeeklyBudgetMinor + 1000
	for _, role := range []string{WorkspaceRoleEditor, WorkspaceRoleApprover, WorkspaceRoleViewer} {
		_, err = s.ClaimDirectCampaignProviderEdit(ctx, "member-"+role, team.ID, campaign.ID, DirectCampaignChanges{WeeklyBudgetMinor: &changed, ExpectedVersion: campaign.Version}, campaign.ProviderGraphHash, campaign.ProviderRevisionID, "provider-role-check-"+role, now.Add(time.Minute))
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s changed budget: %v", role, err)
		}
	}
	unchanged := campaign.WeeklyBudgetMinor
	texts := []string{"Новый текст объявления"}
	if _, err = s.ClaimDirectCampaignProviderEdit(ctx, "member-editor", team.ID, campaign.ID, DirectCampaignChanges{WeeklyBudgetMinor: &unchanged, Texts: &texts, ExpectedVersion: campaign.Version}, campaign.ProviderGraphHash, campaign.ProviderRevisionID, "provider-editor-copy", now.Add(time.Minute)); err != nil {
		t.Fatalf("editor unchanged full-form budget rejected: %v", err)
	}
	second := createDirectTestCampaign(t, ctx, s, owner, team.ID, now)
	second = acceptDirectTestCampaign(t, ctx, s, owner, team.ID, second, now)
	if _, err = s.ClaimDirectCampaignProviderEdit(ctx, owner, team.ID, second.ID, DirectCampaignChanges{WeeklyBudgetMinor: &changed, ExpectedVersion: second.Version}, second.ProviderGraphHash, second.ProviderRevisionID, "provider-owner-budget", now.Add(time.Minute)); err != nil {
		t.Fatalf("owner changed budget rejected: %v", err)
	}
	draft := createDirectTestCampaign(t, ctx, s, owner, team.ID, now)
	if _, err = s.UpdateDirectCampaignDraft(ctx, "member-editor", team.ID, draft.ID, DirectCampaignChanges{WeeklyBudgetMinor: &changed, ExpectedVersion: draft.Version}); err != nil {
		t.Fatalf("draft-only workflow changed: %v", err)
	}
}

func TestExternalClaimRechecksRevokedActorAndProtectsReconciliationCredential(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, owner, _ := newDirectStoreFixture(t, ctx)
	team, err := s.CreateWorkspace(ctx, owner, Workspace{Name: "External team"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seedBillingContract(t, s, team.ID, "pro", now.Add(-time.Hour), now.AddDate(0, 1, 0), "team-method")
	editor := "external-editor"
	if err = s.UpsertUser(ctx, User{ID: editor, DisplayName: editor}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddWorkspaceMember(ctx, owner, WorkspaceMember{WorkspaceID: team.ID, UserID: editor, Role: WorkspaceRoleEditor}); err != nil {
		t.Fatal(err)
	}
	connection := connectDirectTestAccount(t, ctx, s, owner, team.ID)
	seedExternalEditTestSnapshot(t, s, owner, team.ID, connection.ID, now)
	baseline := strings.Repeat("a", 64)
	control, err := s.ObserveDirectExternalEdit(ctx, owner, team.ID, connection.ID, 77, baseline, now)
	if err != nil {
		t.Fatal(err)
	}
	fence := DirectExternalEditFence{connection.ID, control.Version, control.ObservedHash, control.RevisionID}
	operation, err := s.ClaimDirectExternalEdit(ctx, editor, team.ID, 77, fence, strings.Repeat("b", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RemoveWorkspaceMember(ctx, owner, team.ID, editor); err != nil {
		t.Fatal(err)
	}
	if err = s.BeginDirectExternalWrite(ctx, editor, team.ID, connection.ID, 77, operation, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked actor could write: %v", err)
	}
	if err = s.RevokeDirectConnection(ctx, owner, team.ID, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("active operation lost reconciliation credential: %v", err)
	}
	if _, err = s.ReplaceDirectConnection(ctx, owner, team.ID, DirectConnection{AccountID: "other", CurrencyCode: "RUB", Timezone: "Europe/Moscow", TokenCiphertext: "v1.other", TokenKeyVersion: 1, CreatedAt: now}); !errors.Is(err, ErrConflict) {
		t.Fatalf("operation account replaced: %v", err)
	}
	if err = s.FinishDirectExternalEdit(ctx, team.ID, connection.ID, 77, operation, false); err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeDirectConnection(ctx, owner, team.ID, now); err != nil {
		t.Fatalf("unsent operation still blocked account revoke: %v", err)
	}
	if _, err = s.GetDirectExternalEditContext(ctx, owner, team.ID, connection.ID, 77, false); !errors.Is(err, ErrDirectConnectionRequired) {
		t.Fatalf("revoked connection still inspectable: %v", err)
	}
}

func TestExternalWriteBlockedOnParentRechecksCommittedRoleDemotion(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, owner, _ := newDirectStoreFixture(t, ctx)
	team, err := s.CreateWorkspace(ctx, owner, Workspace{Name: "Role race"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seedBillingContract(t, s, team.ID, "pro", now.Add(-time.Hour), now.AddDate(0, 1, 0), "race-method")
	editor := "race-editor"
	if err = s.UpsertUser(ctx, User{ID: editor, DisplayName: editor}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddWorkspaceMember(ctx, owner, WorkspaceMember{WorkspaceID: team.ID, UserID: editor, Role: WorkspaceRoleEditor}); err != nil {
		t.Fatal(err)
	}
	connection := connectDirectTestAccount(t, ctx, s, owner, team.ID)
	seedExternalEditTestSnapshot(t, s, owner, team.ID, connection.ID, now)
	control, err := s.ObserveDirectExternalEdit(ctx, owner, team.ID, connection.ID, 77, strings.Repeat("a", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	fence := DirectExternalEditFence{connection.ID, control.Version, control.ObservedHash, control.RevisionID}
	operation, err := s.ClaimDirectExternalEdit(ctx, editor, team.ID, 77, fence, strings.Repeat("b", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback() }()
	var id string
	var pid int
	if err = blocker.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, team.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err = blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- s.BeginDirectExternalWrite(ctx, editor, team.ID, connection.ID, 77, operation, now) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("writer did not reach deterministic parent barrier")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = blocker.ExecContext(ctx, `UPDATE workspace_members SET role='viewer',updated_at=$3 WHERE workspace_id=$1 AND user_id=$2`, team.ID, editor, now); err != nil {
		t.Fatal(err)
	}
	if err = blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-result; !errors.Is(err, ErrNotFound) {
		t.Fatalf("blocked writer used pre-demotion authority: %v", err)
	}
	var started bool
	if err = s.db.QueryRowContext(ctx, `SELECT write_started FROM direct_external_edit_controls WHERE workspace_id=$1 AND connection_id=$2 AND provider_campaign_id=77`, team.ID, connection.ID).Scan(&started); err != nil || started {
		t.Fatalf("unauthorized provider intent was persisted: %v %v", started, err)
	}
}
