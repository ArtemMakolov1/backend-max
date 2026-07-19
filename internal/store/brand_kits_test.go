package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestWorkspaceBrandKitVersionRBACAndPersonalWorkspace(t *testing.T) {
	ctx := context.Background()
	storage := openWorkspaceTestStore(t, "brand-kit-rbac")
	for _, userID := range []string{"brand-editor", "brand-viewer"} {
		upsertWorkspaceUser(t, storage, userID, userID+"@example.test")
	}
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Brand team"})
	if err != nil {
		t.Fatal(err)
	}
	for userID, role := range map[string]string{
		"brand-editor": WorkspaceRoleEditor,
		"brand-viewer": WorkspaceRoleViewer,
	} {
		if _, err := storage.AddWorkspaceMember(ctx, "test-owner", WorkspaceMember{
			WorkspaceID: workspace.ID, UserID: userID, Role: role,
		}); err != nil {
			t.Fatal(err)
		}
	}
	kit, err := storage.GetWorkspaceBrandKit(ctx, "brand-viewer", workspace.ID)
	if err != nil || kit.Version != 1 || kit.ForbiddenWords == nil || kit.ExamplePosts == nil {
		t.Fatalf("initial brand kit=%#v err=%v", kit, err)
	}
	profile := BrandProfile{
		Audience: " Владельцы малого бизнеса ", Tone: " Экспертный ", CTA: "Подписаться",
		ForbiddenWords: []string{"Хайп", "хайп", " кликбейт "},
		ExamplePosts:   []string{" Первый пример ", "Второй пример"}, VisualStyle: "Синяя редакционная графика",
	}
	if _, err := storage.UpdateWorkspaceBrandKit(ctx, "brand-viewer", workspace.ID,
		WorkspaceBrandKitUpdate{BrandProfile: profile, ExpectedVersion: kit.Version}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("viewer updated brand kit: %v", err)
	}
	updated, err := storage.UpdateWorkspaceBrandKit(ctx, "brand-editor", workspace.ID,
		WorkspaceBrandKitUpdate{BrandProfile: profile, ExpectedVersion: kit.Version})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != 2 || updated.Audience != "Владельцы малого бизнеса" ||
		len(updated.ForbiddenWords) != 2 || updated.ForbiddenWords[1] != "кликбейт" {
		t.Fatalf("normalized brand kit=%#v", updated)
	}
	if _, err := storage.UpdateWorkspaceBrandKit(ctx, "brand-editor", workspace.ID,
		WorkspaceBrandKitUpdate{BrandProfile: profile, ExpectedVersion: 1}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale brand kit update=%v", err)
	}
	events, err := storage.ListAuditEvents(ctx, "test-owner", workspace.ID, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !containsAuditAction(events, "brand_kit.updated") {
		t.Fatalf("brand audit events=%#v", events)
	}

	personalAccesses, err := storage.ListWorkspaces(ctx, "brand-editor")
	if err != nil {
		t.Fatal(err)
	}
	var personal Workspace
	for _, access := range personalAccesses {
		if access.Workspace.IsPersonal {
			personal = access.Workspace
		}
	}
	personalKit, err := storage.GetWorkspaceBrandKit(ctx, "brand-editor", personal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.UpdateWorkspaceBrandKit(ctx, "brand-editor", personal.ID,
		WorkspaceBrandKitUpdate{BrandProfile: profile, ExpectedVersion: personalKit.Version}); err != nil {
		t.Fatalf("personal workspace brand update: %v", err)
	}
}

func TestChannelTemplatePrecedenceTenantIsolationAndOptimisticDelete(t *testing.T) {
	ctx := context.Background()
	storage := openWorkspaceTestStore(t, "channel-template-context")
	first, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "First brand"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Second brand"})
	if err != nil {
		t.Fatal(err)
	}
	firstChannel, err := storage.CreateChannelForWorkspace(ctx, "test-owner", first.ID, Channel{
		MAXChatID: "-91001", VerifiedMAXOwnerID: "owner", Title: "First channel", Active: true, IsChannel: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	otherFirstChannel, err := storage.CreateChannelForWorkspace(ctx, "test-owner", first.ID, Channel{
		MAXChatID: "-91002", VerifiedMAXOwnerID: "owner", Title: "Other channel", Active: true, IsChannel: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondChannel, err := storage.CreateChannelForWorkspace(ctx, "test-owner", second.ID, Channel{
		MAXChatID: "-92001", VerifiedMAXOwnerID: "owner", Title: "Foreign channel", Active: true, IsChannel: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defaultTemplate, err := storage.CreateChannelTemplate(ctx, "test-owner", first.ID, ChannelTemplateCreate{
		Name: "Workspace default", BrandProfile: BrandProfile{Tone: "Default tone", Audience: "Default audience"}, IsDefault: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	channelTemplate, err := storage.CreateChannelTemplate(ctx, "test-owner", first.ID, ChannelTemplateCreate{
		ChannelID: &firstChannel.ID, Name: "First channel voice", BrandProfile: BrandProfile{Tone: "Channel tone"},
	})
	if err != nil {
		t.Fatal(err)
	}
	foreignTemplate, err := storage.CreateChannelTemplate(ctx, "test-owner", second.ID, ChannelTemplateCreate{
		ChannelID: &secondChannel.ID, Name: "Foreign template", BrandProfile: BrandProfile{Tone: "Foreign"},
	})
	if err != nil {
		t.Fatal(err)
	}

	contextForChannel, err := storage.ResolveWorkspaceBrandContext(
		ctx, "test-owner", first.ID, nil, &firstChannel.ID)
	if err != nil || contextForChannel.Template == nil || contextForChannel.Template.ID != channelTemplate.ID {
		t.Fatalf("channel context=%#v err=%v", contextForChannel, err)
	}
	defaultContext, err := storage.ResolveWorkspaceBrandContext(
		ctx, "test-owner", first.ID, nil, &otherFirstChannel.ID)
	if err != nil || defaultContext.Template == nil || defaultContext.Template.ID != defaultTemplate.ID {
		t.Fatalf("default context=%#v err=%v", defaultContext, err)
	}
	brandOnlyContext, err := storage.ResolveWorkspaceBrandContext(
		ctx, "test-owner", first.ID, nil, nil)
	if err != nil || brandOnlyContext.Template != nil {
		t.Fatalf("brand-only context=%#v err=%v", brandOnlyContext, err)
	}
	if _, err := storage.ResolveWorkspaceBrandContext(ctx, "test-owner", first.ID, nil, &secondChannel.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign channel resolved in workspace: %v", err)
	}
	if _, err := storage.ResolveWorkspaceBrandContext(ctx, "test-owner", first.ID, &foreignTemplate.ID, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign template resolved in workspace: %v", err)
	}
	if _, err := storage.CreateChannelTemplate(ctx, "test-owner", first.ID, ChannelTemplateCreate{
		ChannelID: &secondChannel.ID, Name: "Cross tenant", BrandProfile: BrandProfile{Tone: "Wrong"},
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace channel template=%v", err)
	}
	if _, err := storage.CreateChannelTemplate(ctx, "test-owner", first.ID, ChannelTemplateCreate{
		ChannelID: &firstChannel.ID, Name: "Invalid default", IsDefault: true,
	}); err == nil || !strings.Contains(err.Error(), "must not target") {
		t.Fatalf("channel-specific global default=%v", err)
	}
	if err := storage.DeleteChannelTemplate(ctx, "test-owner", first.ID, channelTemplate.ID, channelTemplate.Version+1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale template delete=%v", err)
	}
	if err := storage.DeleteChannelTemplate(ctx, "test-owner", first.ID, channelTemplate.ID, channelTemplate.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetChannelTemplate(ctx, "test-owner", first.ID, channelTemplate.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted template lookup=%v", err)
	}
	events, err := storage.ListAuditEvents(ctx, "test-owner", first.ID, 50, 0)
	if err != nil || !containsAuditAction(events, "channel_template.created") ||
		!containsAuditAction(events, "channel_template.deleted") {
		t.Fatalf("template audit events=%#v err=%v", events, err)
	}
}

func TestBrandTablesRejectArchivedWorkspaceWrites(t *testing.T) {
	ctx := context.Background()
	storage := openWorkspaceTestStore(t, "brand-kit-archive-guard")
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Archived brand"})
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteWorkspace(ctx, "test-owner", workspace.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.ExecContext(ctx, `UPDATE workspace_brand_kits SET tone=$1 WHERE workspace_id=$2`,
		"Should fail", workspace.ID); err == nil || !strings.Contains(strings.ToLower(err.Error()), "archived") {
		t.Fatalf("archived brand write error=%v", err)
	}
}

func containsAuditAction(events []AuditEvent, action string) bool {
	for _, event := range events {
		if event.Action == action {
			return true
		}
	}
	return false
}
