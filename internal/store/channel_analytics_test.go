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
	if report.Summary.ViewsChange == nil || *report.Summary.ViewsChange != 9 {
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
		report.Daily[1].Views == nil || *report.Daily[1].Views != 9 ||
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
