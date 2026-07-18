package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDeleteChannelContentForWorkspaceRemovesLinkedContent(t *testing.T) {
	ctx := context.Background()
	storage := openWorkspaceTestStore(t, "channel-content-delete")
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Deletion team"})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := storage.CreateChannelForWorkspace(ctx, "test-owner", workspace.ID, Channel{
		MAXChatID: "-889001", VerifiedMAXOwnerID: "max-owner", Title: "Delete me", Active: true, IsChannel: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduledAt := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Microsecond)
	posts := []Post{
		{Title: "Draft", Content: "draft body", Status: PostStatusDraft, ChannelID: &channel.ID},
		{Title: "Scheduled", Content: "scheduled body", Status: PostStatusScheduled, ChannelID: &channel.ID, ScheduledAt: &scheduledAt},
		{Title: "Published", Content: "published body", Status: PostStatusPublished, ChannelID: &channel.ID, MAXMessageID: "mid-published"},
	}
	createdIDs := make([]int64, 0, len(posts))
	for _, post := range posts {
		created, createErr := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, post)
		if createErr != nil {
			t.Fatal(createErr)
		}
		createdIDs = append(createdIDs, created.ID)
	}
	campaign, err := storage.CreateCampaign(ctx, "test-owner", workspace.ID, Campaign{Name: "Channel plan"}, []CampaignVariant{{
		ChannelID: channel.ID, Title: "Planned", Content: "planned body", Format: FormatMarkdown,
		PlannedAt: scheduledAt.Add(time.Hour),
	}})
	if err != nil {
		t.Fatal(err)
	}

	if err := storage.DeleteChannelContentForWorkspace(ctx, "test-owner", workspace.ID, channel.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetChannelForWorkspace(ctx, "test-owner", workspace.ID, channel.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted channel lookup error=%v, want ErrNotFound", err)
	}
	for _, postID := range createdIDs {
		if _, err := storage.GetPostForWorkspace(ctx, "test-owner", workspace.ID, postID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted post %d lookup error=%v, want ErrNotFound", postID, err)
		}
	}
	remainingCampaign, err := storage.GetCampaign(ctx, "test-owner", workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(remainingCampaign.Variants) != 0 {
		t.Fatalf("remaining channel campaign variants=%#v", remainingCampaign.Variants)
	}
}

func TestDeleteChannelContentForWorkspaceRefusesPublishingPostAtomically(t *testing.T) {
	ctx := context.Background()
	storage := openWorkspaceTestStore(t, "channel-content-delete-publishing")
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Publishing team"})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := storage.CreateChannelForWorkspace(ctx, "test-owner", workspace.ID, Channel{
		MAXChatID: "-889002", VerifiedMAXOwnerID: "max-owner", Title: "Busy", Active: true, IsChannel: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{
		Title: "Publishing", Content: "publishing body", Status: PostStatusPublishing, ChannelID: &channel.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = storage.DeleteChannelContentForWorkspace(ctx, "test-owner", workspace.ID, channel.ID)
	if !errors.Is(err, ErrChannelPublicationInProgress) {
		t.Fatalf("delete publishing channel error=%v, want ErrChannelPublicationInProgress", err)
	}
	if _, err := storage.GetChannelForWorkspace(ctx, "test-owner", workspace.ID, channel.ID); err != nil {
		t.Fatalf("channel was partially deleted: %v", err)
	}
	if _, err := storage.GetPostForWorkspace(ctx, "test-owner", workspace.ID, post.ID); err != nil {
		t.Fatalf("publishing post was partially deleted: %v", err)
	}
}
