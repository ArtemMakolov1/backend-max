package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestChannelAnalyticsIsTenantScopedAndUsesObservedData(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "channel-analytics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	for _, userID := range []string{"analytics-owner", "other-owner"} {
		if err := storage.UpsertUser(ctx, User{ID: userID, Login: userID, DisplayName: userID}); err != nil {
			t.Fatal(err)
		}
	}
	channel, err := storage.CreateChannel(ctx, Channel{
		UserID: "analytics-owner", VerifiedMAXOwnerID: "max-owner", MAXChatID: "analytics-channel",
		Title: "Analytics channel", IconURL: "https://cdn.example/channel.png",
		ParticipantsCount: 99, IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	foreignChannel, err := storage.CreateChannel(ctx, Channel{
		UserID: "other-owner", VerifiedMAXOwnerID: "other-max-owner", MAXChatID: "foreign-channel",
		Title: "Foreign", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	today := utcDate(time.Now())
	fromDay := today.AddDate(0, 0, -2)
	toDay := today
	firstCapture := fromDay.Add(10 * time.Hour)
	secondCapture := fromDay.AddDate(0, 0, 1).Add(11 * time.Hour)

	published := createAnalyticsTestPost(t, storage, Post{
		UserID: "analytics-owner", Title: "Growing post", Content: "body", Status: PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: "message-growing", PublishedAt: timePointer(firstCapture.Add(-time.Hour)),
	})
	views10 := int64(10)
	if _, err := storage.SyncPublicationMetadataForUser(ctx, "analytics-owner", published.ID, channel.ID,
		published.MAXMessageID, "https://max.ru/channel/growing", &views10, firstCapture, false); err != nil {
		t.Fatal(err)
	}
	views15 := int64(15)
	if _, err := storage.SyncPublicationMetadataForUser(ctx, "analytics-owner", published.ID, channel.ID,
		published.MAXMessageID, "https://max.ru/channel/growing", &views15, secondCapture, false); err != nil {
		t.Fatal(err)
	}

	removed := createAnalyticsTestPost(t, storage, Post{
		UserID: "analytics-owner", Title: "Removed post", Content: "body", Status: PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: "message-removed", PublishedAt: timePointer(secondCapture.Add(-time.Hour)),
	})
	views4 := int64(4)
	if _, err := storage.SyncPublicationMetadataForUser(ctx, "analytics-owner", removed.ID, channel.ID,
		removed.MAXMessageID, "https://max.ru/channel/removed", &views4, secondCapture.Add(time.Minute), false); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.MarkMAXPublicationMissingForUser(ctx, "analytics-owner", removed.ID, channel.ID,
		removed.MAXMessageID); err != nil {
		t.Fatal(err)
	}

	views7 := int64(7)
	fallbackSyncedAt := secondCapture.Add(2 * time.Minute)
	createAnalyticsTestPost(t, storage, Post{
		UserID: "analytics-owner", Title: "Fallback post", Content: "body", Status: PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: "message-fallback", MAXMessageURL: "https://max.ru/channel/fallback",
		MAXViews: &views7, MAXStatsSyncedAt: &fallbackSyncedAt, PublishedAt: timePointer(secondCapture.Add(-30 * time.Minute)),
	})
	createAnalyticsTestPost(t, storage, Post{
		UserID: "analytics-owner", Title: "Draft", Content: "body", Status: PostStatusDraft, ChannelID: &channel.ID,
	})
	foreignPost := createAnalyticsTestPost(t, storage, Post{
		UserID: "other-owner", Title: "Foreign post", Content: "secret", Status: PostStatusPublished,
		ChannelID: &foreignChannel.ID, MAXMessageID: "foreign-message", PublishedAt: timePointer(firstCapture),
	})
	foreignViews := int64(999)
	if _, err := storage.SyncPublicationMetadataForUser(ctx, "other-owner", foreignPost.ID, foreignChannel.ID,
		foreignPost.MAXMessageID, "https://max.ru/foreign", &foreignViews, secondCapture, false); err != nil {
		t.Fatal(err)
	}

	if _, err := storage.SyncChannelParticipantStatsForUser(ctx, "analytics-owner", channel.ID, channel.MAXChatID,
		channel.IconURL, 100, fromDay.AddDate(0, 0, -1).Add(9*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.SyncChannelParticipantStatsForUser(ctx, "analytics-owner", channel.ID, channel.MAXChatID,
		channel.IconURL, 103, firstCapture); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.SyncChannelParticipantStatsForUser(ctx, "analytics-owner", channel.ID, channel.MAXChatID,
		channel.IconURL, 105, secondCapture); err != nil {
		t.Fatal(err)
	}

	report, err := storage.GetChannelAnalyticsForUser(ctx, "analytics-owner", channel.ID, fromDay, toDay)
	if err != nil {
		t.Fatal(err)
	}
	if report.Channel.ID != channel.ID || report.Channel.Title != channel.Title || report.Channel.IconURL != channel.IconURL {
		t.Fatalf("channel = %#v", report.Channel)
	}
	if report.Period.From != fromDay.Format(time.DateOnly) || report.Period.To != toDay.Format(time.DateOnly) {
		t.Fatalf("period = %#v", report.Period)
	}
	if report.Summary.PostsTotal != 4 || report.Summary.PublishedPosts != 3 {
		t.Fatalf("post summary = %#v", report.Summary)
	}
	if report.Summary.TotalViews == nil || *report.Summary.TotalViews != 26 {
		t.Fatalf("total views = %#v", report.Summary.TotalViews)
	}
	// The removed post has only one snapshot (4 views), so its initial
	// cumulative counter must not be presented as growth. Only the comparable
	// 10 -> 15 observations of Growing post contribute to the delta.
	if report.Summary.ViewsChange == nil || *report.Summary.ViewsChange != 5 {
		t.Fatalf("view change = %#v", report.Summary.ViewsChange)
	}
	if report.Summary.ParticipantsCurrent == nil || *report.Summary.ParticipantsCurrent != 105 ||
		report.Summary.ParticipantsChange == nil || *report.Summary.ParticipantsChange != 5 {
		t.Fatalf("participant summary = %#v", report.Summary)
	}
	if len(report.Posts) != 3 {
		t.Fatalf("posts = %#v", report.Posts)
	}
	postsByTitle := make(map[string]AnalyticsPost, len(report.Posts))
	for _, post := range report.Posts {
		postsByTitle[post.Title] = post
		if post.Title == foreignPost.Title || post.Views != nil && *post.Views == foreignViews {
			t.Fatalf("foreign data leaked: %#v", report.Posts)
		}
	}
	if got := postsByTitle["Removed post"]; !got.RemovedFromMAX || got.PublicationState != "removed" || got.MAXMessageURL != "" ||
		got.Views == nil || *got.Views != 4 {
		t.Fatalf("removed post = %#v", got)
	}
	if got := postsByTitle["Fallback post"]; got.Views == nil || *got.Views != 7 || got.MAXStatsSyncedAt == nil ||
		!got.MAXStatsSyncedAt.Equal(fallbackSyncedAt) {
		t.Fatalf("fallback post = %#v", got)
	}
	if len(report.Daily) != 2 || report.Daily[0].Date != fromDay.Format(time.DateOnly) || report.Daily[0].Views != nil ||
		report.Daily[0].ViewsTotal == nil || *report.Daily[0].ViewsTotal != 10 ||
		report.Daily[0].ParticipantsCount == nil || *report.Daily[0].ParticipantsCount != 103 ||
		report.Daily[1].Views == nil || *report.Daily[1].Views != 5 ||
		report.Daily[1].ViewsTotal == nil || *report.Daily[1].ViewsTotal != 19 ||
		report.Daily[1].ParticipantsCount == nil || *report.Daily[1].ParticipantsCount != 105 {
		t.Fatalf("daily = %#v", report.Daily)
	}

	if _, err := storage.GetChannelAnalyticsForUser(ctx, "analytics-owner", foreignChannel.ID, fromDay, toDay); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign analytics error = %v", err)
	}
	if _, err := storage.GetChannelAnalyticsForUser(ctx, "analytics-owner", channel.ID, toDay, fromDay); err == nil {
		t.Fatal("reversed analytics range succeeded")
	}
	if _, err := storage.GetChannelAnalyticsForUser(ctx, "analytics-owner", channel.ID,
		toDay.AddDate(0, 0, -MaxChannelAnalyticsDays), toDay); err == nil {
		t.Fatal("oversized analytics range succeeded")
	}
}

func TestBuildAnalyticsDailyRequiresComparablePublicationSnapshots(t *testing.T) {
	t.Parallel()
	dayOne := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	dayTwo := dayOne.AddDate(0, 0, 1)
	observations := []analyticsViewObservation{
		{PostID: 1, MAXMessageID: "first", Views: 10, CapturedAt: dayOne},
		{PostID: 1, MAXMessageID: "first", Views: 13, CapturedAt: dayTwo},
		// The first observation of another publication increases the
		// cumulative total but is not a comparable period delta.
		{PostID: 2, MAXMessageID: "second", Views: 7, CapturedAt: dayTwo.Add(time.Minute)},
	}

	daily, change := buildAnalyticsDaily(observations, nil, nil)
	if len(daily) != 2 {
		t.Fatalf("daily = %#v", daily)
	}
	if daily[0].Views != nil || daily[0].ViewsTotal == nil || *daily[0].ViewsTotal != 10 {
		t.Fatalf("first day = %#v", daily[0])
	}
	if daily[1].Views == nil || *daily[1].Views != 3 || daily[1].ViewsTotal == nil ||
		*daily[1].ViewsTotal != 20 {
		t.Fatalf("second day = %#v", daily[1])
	}
	if change == nil || *change != 3 {
		t.Fatalf("change = %#v", change)
	}
}

func TestBuildAnalyticsDailyCalculatesOnlyConsecutiveParticipantGrowth(t *testing.T) {
	baselineDay := time.Date(2026, time.July, 12, 0, 0, 0, 0, time.UTC)
	baseline := &ChannelParticipantSnapshot{
		ObservedOn: baselineDay.Format(time.DateOnly), ParticipantsCount: 10,
	}
	history := []ChannelParticipantSnapshot{
		{ObservedOn: baselineDay.AddDate(0, 0, 1).Format(time.DateOnly), ParticipantsCount: 12},
		{ObservedOn: baselineDay.AddDate(0, 0, 2).Format(time.DateOnly), ParticipantsCount: 11},
		// A missing calendar day must not be presented as a 24-hour change.
		{ObservedOn: baselineDay.AddDate(0, 0, 4).Format(time.DateOnly), ParticipantsCount: 15},
	}

	daily, _ := buildAnalyticsDaily(nil, history, baseline)
	if len(daily) != 3 || daily[0].ParticipantsChange == nil || *daily[0].ParticipantsChange != 2 ||
		daily[1].ParticipantsChange == nil || *daily[1].ParticipantsChange != -1 ||
		daily[2].ParticipantsChange != nil {
		t.Fatalf("participant daily growth = %#v", daily)
	}
}

func createAnalyticsTestPost(t *testing.T, storage *Store, post Post) Post {
	t.Helper()
	created, err := storage.CreatePost(t.Context(), post)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func timePointer(value time.Time) *time.Time {
	copy := value.UTC()
	return &copy
}
