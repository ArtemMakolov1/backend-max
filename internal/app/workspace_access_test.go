package app

import (
	"errors"
	"testing"

	"maxpilot/backend/internal/store"
)

func TestAccessContextForRole(t *testing.T) {
	t.Parallel()
	tests := []struct {
		role      string
		allowed   []Capability
		forbidden []Capability
	}{
		{
			role:    "owner",
			allowed: []Capability{CapabilityWorkspaceDelete, CapabilityMembersManage, CapabilityReviewDecide, CapabilityPostsPublish},
		},
		{
			role:      "editor",
			allowed:   []Capability{CapabilityPostsWrite, CapabilityCommentsWrite, CapabilityReviewSubmit, CapabilityPostsPublish},
			forbidden: []Capability{CapabilityMembersManage, CapabilityReviewDecide, CapabilityAuditRead},
		},
		{
			role:      "approver",
			allowed:   []Capability{CapabilityPostsRead, CapabilityCommentsWrite, CapabilityReviewDecide},
			forbidden: []Capability{CapabilityPostsWrite, CapabilityPostsPublish},
		},
		{
			role:      "viewer",
			allowed:   []Capability{CapabilityPostsRead, CapabilityCommentsRead},
			forbidden: []Capability{CapabilityPostsWrite, CapabilityCommentsWrite, CapabilityReviewSubmit},
		},
		{role: "unknown", forbidden: []Capability{CapabilityWorkspaceRead, CapabilityPostsRead}},
	}

	for _, test := range tests {
		t.Run(test.role, func(t *testing.T) {
			access := AccessContextForRole("workspace", "user", test.role)
			for _, capability := range test.allowed {
				if !access.Can(capability) {
					t.Errorf("role %q does not grant %q", test.role, capability)
				}
			}
			for _, capability := range test.forbidden {
				if access.Can(capability) {
					t.Errorf("role %q unexpectedly grants %q", test.role, capability)
				}
			}
		})
	}
}

func TestPersonalWorkspaceRemovesTeamLifecycleCapabilities(t *testing.T) {
	t.Parallel()
	access := AccessContextForWorkspace(store.Workspace{ID: "personal", IsPersonal: true}, "user", store.WorkspaceRoleOwner)
	for _, capability := range []Capability{
		CapabilityWorkspaceDelete, CapabilityMembersRead, CapabilityMembersManage,
		CapabilityInvitesRead, CapabilityInvitesManage,
	} {
		if access.Can(capability) {
			t.Errorf("personal workspace unexpectedly grants %q", capability)
		}
	}
	for _, capability := range []Capability{CapabilityWorkspaceRead, CapabilityPostsWrite, CapabilityPostsPublish} {
		if !access.Can(capability) {
			t.Errorf("personal workspace unexpectedly removes %q", capability)
		}
	}
}

func TestClaimedTeamPostRechecksCurrentApprovalBeforeMAX(t *testing.T) {
	fake := &fakeMAX{}
	application, storage := newTestApp(t, fake)
	ctx := t.Context()
	if err := storage.UpsertUser(ctx, store.User{ID: "approval-owner", DisplayName: "Owner"}); err != nil {
		t.Fatal(err)
	}
	workspace, err := storage.CreateWorkspace(ctx, "approval-owner", store.Workspace{Name: "Approval race"})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := storage.CreateChannelForWorkspace(ctx, "approval-owner", workspace.ID, store.Channel{
		VerifiedMAXOwnerID: "approval-max-owner", MAXChatID: "-77001", Title: "Approval", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePostForWorkspace(ctx, "approval-owner", workspace.ID, store.Post{
		Title: "Race", Content: "Must remain gated", Format: store.FormatMarkdown,
		Status: store.PostStatusDraft, ChannelID: &channel.ID, Notify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := storage.ClaimForPublishing(ctx, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.publishClaimedPost(ctx, claimed); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("publishClaimedPost error = %v, want ErrApprovalRequired", err)
	}
	if fake.publishCalls != 0 || fake.getChatCalls != 0 {
		t.Fatalf("approval gate reached MAX: publish=%d get_chat=%d", fake.publishCalls, fake.getChatCalls)
	}
}
