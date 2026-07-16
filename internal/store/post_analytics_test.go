package store

import (
	"errors"
	"testing"
	"time"
)

func TestWorkspacePostAnalyticsSegmentsPublicationAndCalculatesObservedMetrics(t *testing.T) {
	ctx := t.Context()
	storage := openWorkspaceTestStore(t, "workspace-post-analytics")
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Post analytics"})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := storage.CreateChannelForWorkspace(ctx, "test-owner", workspace.ID, Channel{
		MAXChatID: "-post-analytics", VerifiedMAXOwnerID: "max-owner", Title: "Growth",
		ParticipantsCount: 1500, Active: true, IsChannel: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	publishedAt := time.Date(2026, time.July, 9, 20, 0, 0, 0, time.UTC)
	statsSyncedAt := publishedAt.Add(10 * time.Hour)
	post, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{
		Title: "One publication", Content: "body", Format: FormatMarkdown,
		Status: PostStatusPublished, ChannelID: &channel.ID, MAXMessageID: "active-message",
		MAXMessageURL: "https://max.ru/active", MAXStatsSyncedAt: &statsSyncedAt,
		PublishedAt: &publishedAt, Notify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.ExecContext(ctx, `
INSERT INTO channel_participant_snapshots(channel_id,observed_on,captured_at,participants_count)
VALUES(?,?,?,?)`, channel.ID, publishedAt.Format(time.DateOnly), publishedAt.Add(-time.Hour), 1000); err != nil {
		t.Fatal(err)
	}

	insertPostAnalyticsObservation(t, storage, workspace.ID, post, "active-message", 80, publishedAt.Add(3*time.Hour)) // Period anchor.
	insertPostAnalyticsObservation(t, storage, workspace.ID, post, "active-message", 100, publishedAt.Add(5*time.Hour))
	insertPostAnalyticsObservation(t, storage, workspace.ID, post, "active-message", 160, publishedAt.Add(7*time.Hour))
	insertPostAnalyticsObservation(t, storage, workspace.ID, post, "active-message", 140, publishedAt.Add(8*time.Hour))
	insertPostAnalyticsObservation(t, storage, workspace.ID, post, "active-message", 200, publishedAt.Add(10*time.Hour))
	// A stale publication may have a newer and much larger counter. It must not
	// influence the active publication's report.
	insertPostAnalyticsObservation(t, storage, workspace.ID, post, "stale-message", 999, publishedAt.Add(24*time.Hour))

	report, err := storage.GetWorkspacePostAnalytics(
		ctx, "test-owner", workspace.ID, post.ID,
		time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC),
		time.Date(2026, time.July, 12, 0, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if report.Post.PublicationState != PostAnalyticsPublicationPublished || report.Post.RemovedFromMAX ||
		report.Post.MAXMessageURL != "https://max.ru/active" || report.Post.ChannelID == nil ||
		*report.Post.ChannelID != channel.ID || report.Post.ChannelTitle != channel.Title {
		t.Fatalf("post analytics metadata = %#v", report.Post)
	}
	summary := report.Summary
	if summary.Views == nil || *summary.Views != 200 || summary.ViewsChange == nil || *summary.ViewsChange != 120 ||
		summary.Audience == nil || *summary.Audience != 1000 ||
		summary.AudienceSource != PostAnalyticsAudienceSnapshotAtPublish ||
		summary.ViewsPer1KAudience == nil || *summary.ViewsPer1KAudience != 200 ||
		summary.LifetimeViewsPerHour == nil || *summary.LifetimeViewsPerHour != 20 ||
		summary.ObservedViewsPerHour != nil ||
		summary.Observations != 5 || summary.FirstObservedAt == nil || summary.LastObservedAt == nil ||
		!summary.CorrectionDetected {
		t.Fatalf("post analytics summary = %#v", summary)
	}
	if len(report.Series) != 5 || report.Series[0].Views != 80 || report.Series[0].Delta != nil ||
		report.Series[0].IntervalViewsPerHour != nil || report.Series[1].Delta == nil ||
		*report.Series[1].Delta != 20 || report.Series[1].IntervalViewsPerHour == nil ||
		*report.Series[1].IntervalViewsPerHour != 10 || report.Series[2].Delta == nil ||
		*report.Series[2].Delta != 60 || report.Series[2].IntervalViewsPerHour == nil ||
		*report.Series[2].IntervalViewsPerHour != 30 || report.Series[3].Delta == nil ||
		*report.Series[3].Delta != -20 || !report.Series[3].Correction ||
		report.Series[3].IntervalViewsPerHour != nil || report.Series[4].IntervalViewsPerHour == nil ||
		*report.Series[4].IntervalViewsPerHour != 30 {
		t.Fatalf("post analytics series = %#v", report.Series)
	}

	// The latest value is an as-of metric, while observations and growth are
	// deliberately restricted to the selected window.
	outsideWindow, err := storage.GetWorkspacePostAnalytics(
		ctx, "test-owner", workspace.ID, post.ID,
		time.Date(2026, time.July, 11, 0, 0, 0, 0, time.UTC),
		time.Date(2026, time.July, 12, 0, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if outsideWindow.Summary.Views == nil || *outsideWindow.Summary.Views != 200 ||
		outsideWindow.Summary.ViewsChange != nil || outsideWindow.Summary.Observations != 1 ||
		len(outsideWindow.Series) != 1 || outsideWindow.Series[0].Views != 200 {
		t.Fatalf("post analytics outside observation window = %#v", outsideWindow)
	}

	if _, err := storage.GetWorkspacePostAnalytics(
		ctx, "workspace-outsider", workspace.ID, post.ID,
		publishedAt, publishedAt,
	); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider post analytics error = %v, want ErrNotFound", err)
	}
}

func TestWorkspacePostAnalyticsSelectsLatestHistoricalPublicationAndHandlesDraft(t *testing.T) {
	ctx := t.Context()
	storage := openWorkspaceTestStore(t, "historical-post-analytics")
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Historical analytics"})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := storage.CreateChannelForWorkspace(ctx, "test-owner", workspace.ID, Channel{
		MAXChatID: "-historical-analytics", VerifiedMAXOwnerID: "max-owner", Title: "History",
		ParticipantsCount: 400, Active: true, IsChannel: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	publishedAt := time.Date(2026, time.July, 8, 9, 0, 0, 0, time.UTC)
	post, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{
		Title: "Removed publication", Content: "body", Format: FormatMarkdown,
		Status: PostStatusFailed, ChannelID: &channel.ID, MAXMessageURL: "https://max.ru/stale",
		PublishedAt: &publishedAt, Notify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	insertPostAnalyticsObservation(t, storage, workspace.ID, post, "old-message", 80, publishedAt.Add(time.Hour))
	insertPostAnalyticsObservation(t, storage, workspace.ID, post, "latest-message", 20, publishedAt.Add(24*time.Hour))
	insertPostAnalyticsObservation(t, storage, workspace.ID, post, "latest-message", 25, publishedAt.Add(26*time.Hour))

	report, err := storage.GetWorkspacePostAnalytics(
		ctx, "test-owner", workspace.ID, post.ID,
		time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC),
		time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if report.Post.PublicationState != PostAnalyticsPublicationRemoved || !report.Post.RemovedFromMAX ||
		report.Post.MAXMessageURL != "" || len(report.Series) != 2 || report.Series[0].Views != 20 ||
		report.Summary.Views == nil || *report.Summary.Views != 25 ||
		report.Summary.ViewsChange == nil || *report.Summary.ViewsChange != 5 {
		t.Fatalf("historical post analytics = %#v", report)
	}

	draft, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{
		Title: "Never published", Content: "draft", Format: FormatMarkdown,
		Status: PostStatusDraft, Notify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	draftReport, err := storage.GetWorkspacePostAnalytics(
		ctx, "test-owner", workspace.ID, draft.ID,
		time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC),
		time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if draftReport.Post.PublicationState != PostAnalyticsPublicationUnpublished || draftReport.Post.ChannelID != nil ||
		draftReport.Summary.Audience != nil || draftReport.Summary.AudienceSource != PostAnalyticsAudienceMissing ||
		draftReport.Summary.Views != nil || draftReport.Summary.ViewsChange != nil ||
		draftReport.Summary.Observations != 0 || len(draftReport.Series) != 0 {
		t.Fatalf("draft post analytics = %#v", draftReport)
	}

	stale, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{
		Title: "Failed stale identity", Content: "failed", Format: FormatMarkdown,
		Status: PostStatusFailed, MAXMessageID: "stale-without-history", MAXMessageURL: "https://max.ru/stale",
		Notify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	staleReport, err := storage.GetWorkspacePostAnalytics(
		ctx, "test-owner", workspace.ID, stale.ID,
		time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC),
		time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if staleReport.Post.PublicationState != PostAnalyticsPublicationUnpublished ||
		staleReport.Post.MAXMessageURL != "" || staleReport.Summary.Views != nil {
		t.Fatalf("failed post with stale message identity = %#v", staleReport)
	}

	legacyActive, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{
		Title: "Legacy active without timestamp", Content: "legacy", Format: FormatMarkdown,
		Status: PostStatusPublished, ChannelID: &channel.ID, MAXMessageID: "legacy-active",
		MAXMessageURL: "https://max.ru/legacy-active", Notify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	legacyActiveReport, err := storage.GetWorkspacePostAnalytics(
		ctx, "test-owner", workspace.ID, legacyActive.ID,
		time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC),
		time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if legacyActiveReport.Post.PublicationState != PostAnalyticsPublicationPublished ||
		legacyActiveReport.Post.PublishedAt != nil || legacyActiveReport.Post.MAXMessageURL == "" {
		t.Fatalf("legacy active publication = %#v", legacyActiveReport)
	}

	removedWithoutHistory, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{
		Title: "Removed without counters", Content: "removed", Format: FormatMarkdown,
		Status: PostStatusFailed, ChannelID: &channel.ID, PublishedAt: &publishedAt, Notify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	removedWithoutHistoryReport, err := storage.GetWorkspacePostAnalytics(
		ctx, "test-owner", workspace.ID, removedWithoutHistory.ID,
		time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC),
		time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if removedWithoutHistoryReport.Post.PublicationState != PostAnalyticsPublicationRemoved ||
		!removedWithoutHistoryReport.Post.RemovedFromMAX || removedWithoutHistoryReport.Summary.Views != nil ||
		len(removedWithoutHistoryReport.Series) != 0 {
		t.Fatalf("removed post without snapshots = %#v", removedWithoutHistoryReport)
	}

	futurePublishedAt := time.Date(2026, time.July, 12, 10, 0, 0, 0, time.UTC)
	futureStatsAt := futurePublishedAt.Add(time.Hour)
	future, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{
		Title: "Republished after report", Content: "new cycle", Format: FormatMarkdown,
		Status: PostStatusPublished, ChannelID: &channel.ID, MAXMessageID: "future-current",
		MAXMessageURL: "https://max.ru/future", PublishedAt: &futurePublishedAt,
		MAXStatsSyncedAt: &futureStatsAt, Notify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	oldFirst := time.Date(2026, time.July, 9, 10, 0, 0, 0, time.UTC)
	oldLast := oldFirst.Add(2 * time.Hour)
	insertPostAnalyticsObservation(t, storage, workspace.ID, future, "old-cycle", 50, oldFirst)
	insertPostAnalyticsObservation(t, storage, workspace.ID, future, "old-cycle", 70, oldLast)
	futureReport, err := storage.GetWorkspacePostAnalytics(
		ctx, "test-owner", workspace.ID, future.ID,
		time.Date(2026, time.July, 9, 0, 0, 0, 0, time.UTC),
		time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if futureReport.Post.PublicationState != PostAnalyticsPublicationRemoved || !futureReport.Post.RemovedFromMAX ||
		futureReport.Post.PublishedAt != nil || futureReport.Post.MAXMessageURL != "" ||
		futureReport.Post.MAXStatsSyncedAt == nil || !futureReport.Post.MAXStatsSyncedAt.Equal(oldLast) ||
		futureReport.Summary.LifetimeViewsPerHour != nil || futureReport.Summary.Audience == nil ||
		*futureReport.Summary.Audience != 400 ||
		futureReport.Summary.AudienceSource != PostAnalyticsAudienceCurrentChannel ||
		futureReport.Summary.Views == nil || *futureReport.Summary.Views != 70 || len(futureReport.Series) != 2 {
		t.Fatalf("historical cycle before current publication = %#v", futureReport)
	}
}

func TestBuildPostAnalyticsSeriesKeepsNegativeCorrectionsAndNullRates(t *testing.T) {
	start := time.Date(2026, time.July, 10, 10, 0, 0, 0, time.UTC)
	summary := PostAnalyticsSummary{}
	series := buildPostAnalyticsSeries([]postAnalyticsObservation{
		{Views: 100, CapturedAt: start},
		{Views: 90, CapturedAt: start.Add(time.Hour)},
	}, &summary)
	if len(series) != 2 || series[1].Delta == nil || *series[1].Delta != -10 ||
		!series[1].Correction || series[1].IntervalViewsPerHour != nil ||
		summary.ViewsChange == nil || *summary.ViewsChange != -10 ||
		summary.ObservedViewsPerHour != nil || !summary.CorrectionDetected {
		t.Fatalf("corrected series = %#v summary=%#v", series, summary)
	}
}

func TestPostAnalyticsSeriesKeepsLatestThousandPointsAndMarksTruncation(t *testing.T) {
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	newestFirst := make([]postAnalyticsObservation, 0, MaxPostAnalyticsSeriesPoints+1)
	for index := MaxPostAnalyticsSeriesPoints; index >= 0; index-- {
		newestFirst = append(newestFirst, postAnalyticsObservation{
			Views: int64(index), CapturedAt: start.Add(time.Duration(index) * time.Hour),
		})
	}
	bounded, truncated := boundPostAnalyticsObservations(newestFirst, MaxPostAnalyticsSeriesPoints-1)
	bounded = append([]postAnalyticsObservation{{Views: 0, CapturedAt: start.Add(-time.Hour)}}, bounded...)
	summary := PostAnalyticsSummary{SeriesTruncated: truncated}
	series := buildPostAnalyticsSeries(bounded, &summary)
	if !summary.SeriesTruncated || len(series) != MaxPostAnalyticsSeriesPoints ||
		series[0].Views != 0 || series[len(series)-1].Views != MaxPostAnalyticsSeriesPoints ||
		summary.ViewsChange == nil || *summary.ViewsChange != MaxPostAnalyticsSeriesPoints ||
		summary.Observations != MaxPostAnalyticsSeriesPoints {
		t.Fatalf("bounded post analytics series first=%#v last=%#v summary=%#v",
			series[0], series[len(series)-1], summary)
	}
}

func insertPostAnalyticsObservation(
	t *testing.T,
	storage *Store,
	workspaceID string,
	post Post,
	messageID string,
	views int64,
	capturedAt time.Time,
) {
	t.Helper()
	if _, err := storage.db.ExecContext(t.Context(), `
INSERT INTO post_view_snapshots(owner_id,workspace_id,post_id,max_message_id,views,captured_at)
VALUES(?,?,?,?,?,?)`, post.UserID, workspaceID, post.ID, messageID, views, capturedAt.UTC()); err != nil {
		t.Fatal(err)
	}
}
