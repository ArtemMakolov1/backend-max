package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCampaignMaterializationCalendarAndPostDetach(t *testing.T) {
	ctx := context.Background()
	storage := openWorkspaceTestStore(t, "campaign-materialize")
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Campaign team"})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := storage.CreateChannelForWorkspace(ctx, "test-owner", workspace.ID, Channel{
		MAXChatID: "-881001", VerifiedMAXOwnerID: "max-owner", Title: "Main", Active: true, IsChannel: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	plannedAt := time.Now().UTC().Add(4 * time.Hour).Truncate(time.Microsecond)
	campaign, err := storage.CreateCampaign(ctx, "test-owner", workspace.ID, Campaign{Name: "Launch"}, []CampaignVariant{{
		ChannelID: channel.ID, Title: "Variant", Content: "Approved later", Format: FormatMarkdown, PlannedAt: plannedAt,
	}})
	if err != nil {
		t.Fatal(err)
	}
	items, err := storage.ListWorkspaceCalendar(ctx, "test-owner", workspace.ID,
		plannedAt.Add(-time.Hour), plannedAt.Add(time.Hour), nil)
	if err != nil || len(items) != 1 || items[0].PostID != nil || items[0].CampaignID != campaign.ID ||
		items[0].VariantUpdatedAt == nil {
		t.Fatalf("planned calendar=%#v err=%v", items, err)
	}
	campaign, err = storage.MaterializeCampaign(ctx, "test-owner", workspace.ID, campaign.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(campaign.Variants) != 1 || campaign.Variants[0].PostID == nil || campaign.Variants[0].PostStatus != PostStatusDraft {
		t.Fatalf("materialized campaign=%#v", campaign)
	}
	postID := *campaign.Variants[0].PostID
	if err := storage.DeletePostForWorkspace(ctx, "test-owner", workspace.ID, postID); err != nil {
		t.Fatal(err)
	}
	campaign, err = storage.GetCampaign(ctx, "test-owner", workspace.ID, campaign.ID)
	if err != nil || campaign.Variants[0].PostID != nil || campaign.Variants[0].Status != "planned" {
		t.Fatalf("detached variant=%#v err=%v", campaign.Variants, err)
	}
}

func TestCampaignBatchScheduleIsAtomicAndApprovalGated(t *testing.T) {
	ctx := context.Background()
	storage := openWorkspaceTestStore(t, "campaign-schedule")
	upsertWorkspaceUser(t, storage, "campaign-approver", "approver@example.test")
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Approval campaign"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.AddWorkspaceMember(ctx, "test-owner", WorkspaceMember{
		WorkspaceID: workspace.ID, UserID: "campaign-approver", Role: WorkspaceRoleApprover,
	}); err != nil {
		t.Fatal(err)
	}
	channels := make([]Channel, 0, 2)
	for index, chatID := range []string{"-882001", "-882002"} {
		channel, err := storage.CreateChannelForWorkspace(ctx, "test-owner", workspace.ID, Channel{
			MAXChatID: chatID, VerifiedMAXOwnerID: "max-owner", Title: "Channel " + chatID,
			Active: true, IsChannel: true,
		})
		if err != nil {
			t.Fatalf("channel %d: %v", index, err)
		}
		channels = append(channels, channel)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	campaign, err := storage.CreateCampaign(ctx, "test-owner", workspace.ID, Campaign{Name: "Atomic batch"}, []CampaignVariant{
		{ChannelID: channels[0].ID, Title: "First", Content: "First body", Format: FormatMarkdown, PlannedAt: now.Add(4 * time.Hour)},
		{ChannelID: channels[1].ID, Title: "Second", Content: "Second body", Format: FormatMarkdown, PlannedAt: now.Add(5 * time.Hour)},
	})
	if err != nil {
		t.Fatal(err)
	}
	campaign, err = storage.MaterializeCampaign(ctx, "test-owner", workspace.ID, campaign.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	approveCampaignVariant(t, storage, workspace.ID, campaign.Variants[0], now.Add(time.Minute))
	campaign, err = storage.GetCampaign(ctx, "test-owner", workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	items := campaignScheduleItems(campaign)
	if _, err := storage.BatchScheduleCampaign(ctx, "test-owner", workspace.ID, campaign.ID, items, now); err == nil {
		t.Fatal("batch scheduled an unapproved revision")
	} else {
		var batchErr *CampaignScheduleError
		if !errors.As(err, &batchErr) || len(batchErr.Items) != 1 || batchErr.Items[0].Code != "approval_required" {
			t.Fatalf("batch conflict=%#v err=%v", batchErr, err)
		}
	}
	first, err := storage.GetPostForWorkspace(ctx, "test-owner", workspace.ID, *campaign.Variants[0].PostID)
	if err != nil || first.Status != PostStatusDraft || first.ScheduledAt != nil {
		t.Fatalf("partial schedule escaped rollback: %#v err=%v", first, err)
	}
	approveCampaignVariant(t, storage, workspace.ID, campaign.Variants[1], now.Add(2*time.Minute))
	campaign, err = storage.GetCampaign(ctx, "test-owner", workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	items = campaignScheduleItems(campaign)
	campaign, err = storage.BatchScheduleCampaign(ctx, "test-owner", workspace.ID, campaign.ID, items, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range campaign.Variants {
		if variant.PostStatus != PostStatusScheduled || variant.Status != "scheduled" {
			t.Fatalf("scheduled variant=%#v", variant)
		}
	}

	stale := *campaign.Variants[0].PostUpdatedAt
	post, err := storage.RescheduleWorkspacePost(ctx, "test-owner", workspace.ID,
		*campaign.Variants[0].PostID, now.Add(8*time.Hour), stale, now)
	if err != nil || post.ScheduledAt == nil || !post.ScheduledAt.Equal(now.Add(8*time.Hour)) {
		t.Fatalf("reschedule=%#v err=%v", post, err)
	}
	if _, err := storage.RescheduleWorkspacePost(ctx, "test-owner", workspace.ID,
		post.ID, now.Add(9*time.Hour), stale, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale reschedule error=%v", err)
	}
	cancelled, err := storage.CancelSchedule(ctx, post.ID)
	if err != nil || cancelled.Status != PostStatusDraft || cancelled.ScheduledAt != nil {
		t.Fatalf("cancelled campaign post=%#v err=%v", cancelled, err)
	}
	campaign, err = storage.GetCampaign(ctx, "test-owner", workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range campaign.Variants {
		if variant.PostID != nil && *variant.PostID == post.ID && variant.Status != "materialized" {
			t.Fatalf("cancelled campaign variant=%#v", variant)
		}
	}
}

func TestCalendarScheduleAllowsImageAndAttachmentOnlyPosts(t *testing.T) {
	ctx := context.Background()
	storage := openWorkspaceTestStore(t, "calendar-media-only")
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Media calendar"})
	if err != nil {
		t.Fatal(err)
	}
	approvalRequired := false
	if _, err := storage.UpdateWorkspace(ctx, "test-owner", workspace.ID, WorkspaceChanges{
		ApprovalRequired: &approvalRequired,
	}); err != nil {
		t.Fatal(err)
	}
	channel, err := storage.CreateChannelForWorkspace(ctx, "test-owner", workspace.ID, Channel{
		MAXChatID: "-882401", VerifiedMAXOwnerID: "max-owner", Title: "Media",
		Active: true, IsChannel: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)

	imagePost, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{
		Title: "Image only", Content: "", Format: FormatMarkdown, Status: PostStatusDraft,
		ChannelID: &channel.ID, ImageURL: "/media/calendar-image.png", Notify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	imagePost, err = storage.RescheduleWorkspacePost(ctx, "test-owner", workspace.ID,
		imagePost.ID, now.Add(time.Hour), imagePost.UpdatedAt, now)
	if err != nil || imagePost.Status != PostStatusScheduled || imagePost.ScheduledAt == nil {
		t.Fatalf("schedule image-only post=%#v err=%v", imagePost, err)
	}

	attachmentPost, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{
		Title: "Video only", Content: "", Format: FormatMarkdown, Status: PostStatusDraft,
		ChannelID: &channel.ID, Notify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := storage.ReserveMediaForWorkspace(ctx, "test-owner", workspace.ID,
		"calendar-video.mp4", 128, MediaLimits{MaxFiles: 10, MaxBytes: 1 << 20}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.CompleteMediaReservation(ctx, reservation, now); err != nil {
		t.Fatal(err)
	}
	attachmentPost, err = storage.AddPostAttachmentForWorkspace(ctx, "test-owner", workspace.ID,
		attachmentPost.ID, PostAttachment{
			Type: PostAttachmentVideo, Position: -1, StorageKey: "calendar-video.mp4",
			SizeBytes: 128, MIMEType: "video/mp4",
		})
	if err != nil {
		t.Fatal(err)
	}
	if attachmentPost.ImageURL != "" || len(attachmentPost.Attachments) != 1 {
		t.Fatalf("video-only fixture unexpectedly has image projection: %#v", attachmentPost)
	}
	attachmentPost, err = storage.RescheduleWorkspacePost(ctx, "test-owner", workspace.ID,
		attachmentPost.ID, now.Add(2*time.Hour), attachmentPost.UpdatedAt, now)
	if err != nil || attachmentPost.Status != PostStatusScheduled || attachmentPost.ScheduledAt == nil {
		t.Fatalf("schedule attachment-only post=%#v err=%v", attachmentPost, err)
	}
	attachmentPost, err = storage.CancelSchedule(ctx, attachmentPost.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.ExecContext(ctx, `UPDATE posts SET content='Text fallback' WHERE workspace_id=$1 AND id=$2`,
		workspace.ID, attachmentPost.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.ExecContext(ctx, `UPDATE post_attachments SET processing_status='processing'
WHERE workspace_id=$1 AND post_id=$2`, workspace.ID, attachmentPost.ID); err != nil {
		t.Fatal(err)
	}
	attachmentPost, err = storage.GetPostForWorkspace(ctx, "test-owner", workspace.ID, attachmentPost.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.RescheduleWorkspacePost(ctx, "test-owner", workspace.ID,
		attachmentPost.ID, now.Add(3*time.Hour), attachmentPost.UpdatedAt, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("post with a non-ready attachment schedule error=%v, want ErrConflict", err)
	}

	emptyPost, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{
		Title: "Actually empty", Content: "", Format: FormatMarkdown, Status: PostStatusDraft,
		ChannelID: &channel.ID, Notify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.RescheduleWorkspacePost(ctx, "test-owner", workspace.ID,
		emptyPost.ID, now.Add(4*time.Hour), emptyPost.UpdatedAt, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("empty post schedule error=%v, want ErrConflict", err)
	}
}

func TestCampaignLifecycleCompletesWhenEveryVariantIsPublished(t *testing.T) {
	ctx := context.Background()
	storage := openWorkspaceTestStore(t, "campaign-published-lifecycle")
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Published campaign"})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := storage.CreateChannelForWorkspace(ctx, "test-owner", workspace.ID, Channel{
		MAXChatID: "-882501", VerifiedMAXOwnerID: "max-owner", Title: "Published", Active: true, IsChannel: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	campaign, err := storage.CreateCampaign(ctx, "test-owner", workspace.ID, Campaign{Name: "Finish"}, []CampaignVariant{{
		ChannelID: channel.ID, Title: "Only variant", Content: "Publish the campaign", Format: FormatMarkdown,
		PlannedAt: time.Now().UTC().Add(time.Hour),
	}})
	if err != nil {
		t.Fatal(err)
	}
	campaign, err = storage.MaterializeCampaign(ctx, "test-owner", workspace.ID, campaign.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	postID := *campaign.Variants[0].PostID
	now := time.Now().UTC()
	if _, err := storage.db.ExecContext(ctx, `UPDATE posts
SET status='published',scheduled_at=NULL,published_at=$1,max_message_id='campaign-message',updated_at=$1
WHERE workspace_id=$2 AND id=$3`, now, workspace.ID, postID); err != nil {
		t.Fatal(err)
	}
	campaign, err = storage.GetCampaign(ctx, "test-owner", workspace.ID, campaign.ID)
	if err != nil || campaign.Status != "completed" || campaign.Variants[0].Status != "published" {
		t.Fatalf("published lifecycle=%#v err=%v", campaign, err)
	}
	campaign, err = storage.AddCampaignVariants(ctx, "test-owner", workspace.ID, campaign.ID, []CampaignVariant{{
		ChannelID: channel.ID, Title: "Follow-up", Content: "Reopen the completed campaign", Format: FormatMarkdown,
		PlannedAt: now.Add(2 * time.Hour),
	}})
	if err != nil || campaign.Status != "active" {
		t.Fatalf("campaign with a new planned variant=%#v err=%v", campaign, err)
	}
	var added CampaignVariant
	for _, variant := range campaign.Variants {
		if variant.PostID == nil {
			added = variant
		}
	}
	if added.ID == "" {
		t.Fatalf("new planned variant missing from %#v", campaign.Variants)
	}
	if err := storage.DeleteCampaignVariant(ctx, "test-owner", workspace.ID, campaign.ID,
		added.ID, added.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	campaign, err = storage.GetCampaign(ctx, "test-owner", workspace.ID, campaign.ID)
	if err != nil || campaign.Status != "completed" {
		t.Fatalf("campaign after removing its only pending variant=%#v err=%v", campaign, err)
	}
	if _, err := storage.db.ExecContext(ctx, `DELETE FROM posts WHERE workspace_id=$1 AND id=$2`, workspace.ID, postID); err != nil {
		t.Fatal(err)
	}
	campaign, err = storage.GetCampaign(ctx, "test-owner", workspace.ID, campaign.ID)
	if err != nil || campaign.Status != "planned" || campaign.Variants[0].PostID != nil ||
		campaign.Variants[0].Status != "planned" {
		t.Fatalf("campaign after post detach=%#v err=%v", campaign, err)
	}
}

func TestCampaignVariantBlocksChannelDeleteWithConflict(t *testing.T) {
	ctx := context.Background()
	storage := openWorkspaceTestStore(t, "campaign-channel-delete")
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Channel dependency"})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := storage.CreateChannelForWorkspace(ctx, "test-owner", workspace.ID, Channel{
		MAXChatID: "-883001", VerifiedMAXOwnerID: "max-owner", Title: "Planned", Active: true, IsChannel: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = storage.CreateCampaign(ctx, "test-owner", workspace.ID, Campaign{Name: "Dependency"}, []CampaignVariant{{
		ChannelID: channel.ID, Title: "Plan", Content: strings.Repeat("x", 10), Format: FormatMarkdown,
		PlannedAt: time.Now().UTC().Add(time.Hour),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteChannelForWorkspace(ctx, "test-owner", workspace.ID, channel.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete planned channel error=%v, want ErrConflict", err)
	}
}

func TestDeleteCampaignVariantRequiresCurrentTimestamp(t *testing.T) {
	ctx := context.Background()
	storage := openWorkspaceTestStore(t, "campaign-variant-delete-cas")
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Variant delete CAS"})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := storage.CreateChannelForWorkspace(ctx, "test-owner", workspace.ID, Channel{
		MAXChatID: "-883501", VerifiedMAXOwnerID: "max-owner", Title: "CAS", Active: true, IsChannel: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	campaign, err := storage.CreateCampaign(ctx, "test-owner", workspace.ID, Campaign{Name: "Delete safely"}, []CampaignVariant{
		{ChannelID: channel.ID, Title: "Planned", Content: "Keep concurrent edits", Format: FormatMarkdown, PlannedAt: now.Add(time.Hour)},
		{ChannelID: channel.ID, Title: "Keep", Content: "Campaign cannot become empty", Format: FormatMarkdown, PlannedAt: now.Add(2 * time.Hour)},
	})
	if err != nil {
		t.Fatal(err)
	}
	variant := campaign.Variants[0]
	if err := storage.DeleteCampaignVariant(ctx, "test-owner", workspace.ID, campaign.ID,
		variant.ID, variant.UpdatedAt.Add(-time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale delete error=%v, want ErrConflict", err)
	}
	if _, err := storage.GetCampaign(ctx, "test-owner", workspace.ID, campaign.ID); err != nil {
		t.Fatalf("stale delete removed campaign variant: %v", err)
	}
	if err := storage.DeleteCampaignVariant(ctx, "test-owner", workspace.ID, campaign.ID,
		variant.ID, variant.UpdatedAt); err != nil {
		t.Fatalf("current delete: %v", err)
	}
	campaign, err = storage.GetCampaign(ctx, "test-owner", workspace.ID, campaign.ID)
	if err != nil || len(campaign.Variants) != 1 {
		t.Fatalf("variants after delete=%#v err=%v", campaign.Variants, err)
	}
	remaining := campaign.Variants[0]
	if err := storage.DeleteCampaignVariant(ctx, "test-owner", workspace.ID, campaign.ID,
		remaining.ID, remaining.UpdatedAt); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete final variant error=%v, want ErrConflict", err)
	}
}

func TestArchivedCampaignVariantsAreImmutable(t *testing.T) {
	ctx := context.Background()
	storage := openWorkspaceTestStore(t, "campaign-archived-immutable")
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Archived campaign"})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := storage.CreateChannelForWorkspace(ctx, "test-owner", workspace.ID, Channel{
		MAXChatID: "-883601", VerifiedMAXOwnerID: "max-owner", Title: "Archive", Active: true, IsChannel: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	campaign, err := storage.CreateCampaign(ctx, "test-owner", workspace.ID, Campaign{Name: "Freeze me"}, []CampaignVariant{{
		ChannelID: channel.ID, Title: "Frozen", Content: "Cannot change after archive", Format: FormatMarkdown,
		PlannedAt: time.Now().UTC().Add(2 * time.Hour),
	}})
	if err != nil {
		t.Fatal(err)
	}
	campaign, err = storage.MaterializeCampaign(ctx, "test-owner", workspace.ID, campaign.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	variant := campaign.Variants[0]
	if variant.PostID == nil {
		t.Fatal("materialized campaign variant has no post")
	}
	if err := storage.ArchiveCampaign(ctx, "test-owner", workspace.ID, campaign.ID, campaign.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	items, err := storage.ListWorkspaceCalendar(ctx, "test-owner", workspace.ID,
		variant.PlannedAt.Add(-time.Hour), variant.PlannedAt.Add(time.Hour), nil)
	if err != nil || len(items) != 0 {
		t.Fatalf("archived materialized draft leaked into calendar: %#v err=%v", items, err)
	}
	if _, err := storage.db.ExecContext(ctx, `UPDATE posts
SET status='scheduled',scheduled_at=$1,updated_at=$2 WHERE workspace_id=$3 AND id=$4`,
		variant.PlannedAt, time.Now().UTC(), workspace.ID, *variant.PostID); err != nil {
		t.Fatal(err)
	}
	items, err = storage.ListWorkspaceCalendar(ctx, "test-owner", workspace.ID,
		variant.PlannedAt.Add(-time.Hour), variant.PlannedAt.Add(time.Hour), nil)
	if err != nil || len(items) != 1 || items[0].Kind != "post" || items[0].CampaignID != "" || items[0].VariantID != "" {
		t.Fatalf("archived campaign schedule should be an ordinary post: %#v err=%v", items, err)
	}
	changed := "Changed"
	if _, err := storage.UpdateCampaignVariant(ctx, "test-owner", workspace.ID, campaign.ID, variant.ID,
		CampaignVariantChanges{Title: &changed, ExpectedUpdatedAt: variant.UpdatedAt}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update archived variant error=%v, want ErrNotFound", err)
	}
	if err := storage.DeleteCampaignVariant(ctx, "test-owner", workspace.ID, campaign.ID,
		variant.ID, variant.UpdatedAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete archived variant error=%v, want ErrNotFound", err)
	}
}

func TestCreateCampaignFromExistingDraftKeepsApprovalAndSchedulePending(t *testing.T) {
	ctx := context.Background()
	storage := openWorkspaceTestStore(t, "campaign-existing-draft")
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Analytics repeat"})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := storage.CreateChannelForWorkspace(ctx, "test-owner", workspace.ID, Channel{
		MAXChatID: "-884001", VerifiedMAXOwnerID: "max-owner", Title: "Analytics", Active: true, IsChannel: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	draft, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{
		Title: "Repeat winner", Content: "Still a draft", Format: FormatMarkdown,
		Status: PostStatusDraft, ChannelID: &channel.ID, Notify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	plannedAt := time.Now().UTC().Add(12 * time.Hour)
	campaign, err := storage.CreateCampaignFromExistingDraft(ctx, "test-owner", workspace.ID,
		Campaign{Name: "Повторить в удачное время"}, draft.ID, plannedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(campaign.Variants) != 1 || campaign.Variants[0].PostID == nil ||
		*campaign.Variants[0].PostID != draft.ID || campaign.Variants[0].Status != "materialized" {
		t.Fatalf("linked campaign=%#v", campaign)
	}
	stored, err := storage.GetPostForWorkspace(ctx, "test-owner", workspace.ID, draft.ID)
	if err != nil || stored.Status != PostStatusDraft || stored.ScheduledAt != nil || stored.ReviewStatus != ReviewStatusDraft {
		t.Fatalf("source draft lifecycle changed=%#v err=%v", stored, err)
	}
}

func TestCampaignSchedulingSerializesApprovalPolicyAndPostLocks(t *testing.T) {
	ctx := context.Background()
	storage := openWorkspaceTestStore(t, "campaign-scheduling-lock-order")
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Scheduling locks"})
	if err != nil {
		t.Fatal(err)
	}
	approvalRequired := false
	workspace, err = storage.UpdateWorkspace(ctx, "test-owner", workspace.ID, WorkspaceChanges{
		ApprovalRequired: &approvalRequired,
	})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := storage.CreateChannelForWorkspace(ctx, "test-owner", workspace.ID, Channel{
		MAXChatID: "-884501", VerifiedMAXOwnerID: "max-owner", Title: "Concurrent",
		Active: true, IsChannel: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	campaign, err := storage.CreateCampaign(ctx, "test-owner", workspace.ID, Campaign{Name: "Concurrent schedule"}, []CampaignVariant{{
		ChannelID: channel.ID, Title: "Variant", Content: "Needs serialization", Format: FormatMarkdown,
		PlannedAt: now.Add(3 * time.Hour),
	}})
	if err != nil {
		t.Fatal(err)
	}
	campaign, err = storage.MaterializeCampaign(ctx, "test-owner", workspace.ID, campaign.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	variant := campaign.Variants[0]
	if variant.PostID == nil || variant.PostUpdatedAt == nil {
		t.Fatalf("materialized variant=%#v", variant)
	}

	policyTx, err := storage.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := policyTx.ExecContext(ctx, `UPDATE workspaces SET approval_required=TRUE WHERE id=$1`, workspace.ID); err != nil {
		_ = policyTx.Rollback()
		t.Fatal(err)
	}
	policyResult := make(chan error, 1)
	go func() {
		_, scheduleErr := storage.RescheduleWorkspacePost(ctx, "test-owner", workspace.ID, *variant.PostID,
			now.Add(4*time.Hour), *variant.PostUpdatedAt, now)
		policyResult <- scheduleErr
	}()
	select {
	case scheduleErr := <-policyResult:
		_ = policyTx.Rollback()
		t.Fatalf("reschedule bypassed an uncommitted approval policy change: %v", scheduleErr)
	case <-time.After(150 * time.Millisecond):
	}
	if err := policyTx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case scheduleErr := <-policyResult:
		if !errors.Is(scheduleErr, ErrCampaignApprovalRequired) {
			t.Fatalf("reschedule after approval policy commit=%v", scheduleErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reschedule remained blocked after approval policy commit")
	}

	approvalRequired = false
	if _, err := storage.UpdateWorkspace(ctx, "test-owner", workspace.ID, WorkspaceChanges{
		ApprovalRequired: &approvalRequired,
	}); err != nil {
		t.Fatal(err)
	}
	campaign, err = storage.GetCampaign(ctx, "test-owner", workspace.ID, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	variant = campaign.Variants[0]
	start := make(chan struct{})
	results := make(chan error, 2)
	concurrentCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	go func() {
		<-start
		_, scheduleErr := storage.BatchScheduleCampaign(concurrentCtx, "test-owner", workspace.ID, campaign.ID,
			[]CampaignScheduleItem{{VariantID: variant.ID, ScheduledAt: variant.PlannedAt, ExpectedUpdatedAt: *variant.PostUpdatedAt}}, now)
		results <- scheduleErr
	}()
	go func() {
		<-start
		_, scheduleErr := storage.RescheduleWorkspacePost(concurrentCtx, "test-owner", workspace.ID, *variant.PostID,
			now.Add(5*time.Hour), *variant.PostUpdatedAt, now)
		results <- scheduleErr
	}()
	close(start)
	successes := 0
	for index := 0; index < 2; index++ {
		select {
		case scheduleErr := <-results:
			if scheduleErr == nil {
				successes++
			} else if !errors.Is(scheduleErr, ErrConflict) {
				t.Fatalf("concurrent scheduler returned a lock error: %v", scheduleErr)
			}
		case <-concurrentCtx.Done():
			t.Fatalf("concurrent schedulers did not finish: %v", concurrentCtx.Err())
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent schedule successes=%d, want exactly one", successes)
	}
}

func approveCampaignVariant(t *testing.T, storage *Store, workspaceID string, variant CampaignVariant, at time.Time) {
	t.Helper()
	if variant.PostID == nil {
		t.Fatal("campaign variant has no post")
	}
	revision, err := storage.SubmitPostForReview(context.Background(), "test-owner", workspaceID, *variant.PostID, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.DecidePostReview(context.Background(), "campaign-approver", workspaceID,
		*variant.PostID, revision.ID, ReviewDecisionApproved, "Approved", at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
}

func campaignScheduleItems(campaign Campaign) []CampaignScheduleItem {
	result := make([]CampaignScheduleItem, 0, len(campaign.Variants))
	for _, variant := range campaign.Variants {
		result = append(result, CampaignScheduleItem{
			VariantID: variant.ID, ScheduledAt: variant.PlannedAt,
			ExpectedUpdatedAt: *variant.PostUpdatedAt,
		})
	}
	return result
}
