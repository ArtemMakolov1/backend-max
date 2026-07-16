package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAnalyticsContentHeatmapUsesExplicitTimezoneAcrossDayRollover(t *testing.T) {
	publishedAt := time.Date(2026, time.July, 12, 23, 30, 0, 0, time.UTC) // Sunday UTC.
	viewsPer1K := 125.0
	viewsPerHour := 8.5
	heatmap, best := buildAnalyticsContentHeatmap([]AnalyticsContentPost{{
		ID: 1, PublishedAt: &publishedAt,
		ViewsPer1KAudience: &viewsPer1K, ViewsPerHour: &viewsPerHour,
	}}, time.Date(2026, time.July, 13, 8, 0, 0, 0, time.UTC), 180)

	if len(heatmap) != 7*24 {
		t.Fatalf("heatmap cells = %d, want 168", len(heatmap))
	}
	cell := heatmap[2] // Monday=0, 02:00 local.
	if cell.Weekday != 0 || cell.Hour != 2 || cell.Posts != 1 ||
		cell.ViewsPer1KAudience == nil || *cell.ViewsPer1KAudience != 125 ||
		cell.Score == nil || *cell.Score != 32.6 {
		t.Fatalf("rollover cell = %#v", cell)
	}
	if best == nil || best.Weekday != 0 || best.Hour != 2 || best.SampleSize != 1 || best.Score != 32.6 {
		t.Fatalf("best time = %#v", best)
	}
	// 02:00 at UTC+03:00 is 23:00 UTC on the previous calendar day.
	wantNext := time.Date(2026, time.July, 19, 23, 0, 0, 0, time.UTC)
	if !best.NextAt.Equal(wantNext) {
		t.Fatalf("next recommendation = %s, want %s", best.NextAt, wantNext)
	}
}

func TestAnalyticsContentBestTimeRequiresBothNormalizedSignals(t *testing.T) {
	publishedAt := time.Date(2026, time.July, 12, 10, 0, 0, 0, time.UTC)
	viewsPer1K := 125.0
	heatmap, best := buildAnalyticsContentHeatmap([]AnalyticsContentPost{{
		ID: 1, PublishedAt: &publishedAt, ViewsPer1KAudience: &viewsPer1K,
	}}, time.Date(2026, time.July, 13, 8, 0, 0, 0, time.UTC), 0)

	cell := heatmap[6*24+10]
	if cell.ViewsPer1KAudience == nil || cell.ViewsPerHour != nil || cell.Score != nil {
		t.Fatalf("partial-metric heatmap cell = %#v", cell)
	}
	if best != nil {
		t.Fatalf("best time from incomparable partial metrics = %#v", best)
	}
}

func TestCreateAnalyticsRepeatPlanRollsBackDraftWhenCampaignCannotBeCreated(t *testing.T) {
	ctx := context.Background()
	storage := openWorkspaceTestStore(t, "analytics-repeat-atomic")
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Atomic analytics repeat"})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := storage.CreateChannelForWorkspace(ctx, "test-owner", workspace.ID, Channel{
		MAXChatID: "-889001", VerifiedMAXOwnerID: "max-owner", Title: "Repeat source",
		Active: true, IsChannel: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	source, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{
		Title: "Winner", Content: "Copy me", Format: FormatMarkdown,
		Status: PostStatusDraft, ChannelID: &channel.ID, Notify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	inactive := false
	if _, err := storage.UpdateChannelForWorkspace(
		ctx, "test-owner", workspace.ID, channel.ID, nil, &inactive,
	); err != nil {
		t.Fatal(err)
	}

	var postsBefore, campaignsBefore int
	if err := storage.db.QueryRowContext(ctx, `SELECT count(*) FROM posts WHERE workspace_id=$1`, workspace.ID).Scan(&postsBefore); err != nil {
		t.Fatal(err)
	}
	if err := storage.db.QueryRowContext(ctx, `SELECT count(*) FROM campaigns WHERE workspace_id=$1`, workspace.ID).Scan(&campaignsBefore); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.CreateAnalyticsRepeatPlan(
		ctx, "test-owner", workspace.ID, source.ID, time.Now().UTC().Add(time.Hour),
	); err == nil || !strings.Contains(err.Error(), "channel is inactive") {
		t.Fatalf("repeat failure=%v, want inactive channel error", err)
	}

	var postsAfter, campaignsAfter, duplicateAudits int
	if err := storage.db.QueryRowContext(ctx, `SELECT count(*) FROM posts WHERE workspace_id=$1`, workspace.ID).Scan(&postsAfter); err != nil {
		t.Fatal(err)
	}
	if err := storage.db.QueryRowContext(ctx, `SELECT count(*) FROM campaigns WHERE workspace_id=$1`, workspace.ID).Scan(&campaignsAfter); err != nil {
		t.Fatal(err)
	}
	if err := storage.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events
WHERE workspace_id=$1 AND action='post.duplicated'`, workspace.ID).Scan(&duplicateAudits); err != nil {
		t.Fatal(err)
	}
	if postsAfter != postsBefore || campaignsAfter != campaignsBefore || duplicateAudits != 0 {
		t.Fatalf("failed repeat leaked state: posts %d->%d campaigns %d->%d duplicate_audits=%d",
			postsBefore, postsAfter, campaignsBefore, campaignsAfter, duplicateAudits)
	}
}
