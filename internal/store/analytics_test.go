package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestTouchUserActivityAndRollingActiveUsers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "analytics-activity.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	for _, userID := range []string{"weekly-user", "monthly-user", "old-user", "expired-user", "boundary-expired", "boundary-kept"} {
		if err := storage.UpsertUser(ctx, User{ID: userID, CreatedAt: now.Add(-60 * 24 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}

	if err := storage.TouchUserActivity(ctx, "test-owner", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := storage.TouchUserActivity(ctx, "test-owner", now); err != nil {
		t.Fatal(err)
	}
	// A stale retry on the same day must not move last_seen_at backwards.
	if err := storage.TouchUserActivity(ctx, "test-owner", now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := storage.TouchUserActivity(ctx, "weekly-user", now.AddDate(0, 0, -6)); err != nil {
		t.Fatal(err)
	}
	if err := storage.TouchUserActivity(ctx, "monthly-user", now.AddDate(0, 0, -29)); err != nil {
		t.Fatal(err)
	}
	if err := storage.TouchUserActivity(ctx, "old-user", now.AddDate(0, 0, -30)); err != nil {
		t.Fatal(err)
	}
	if err := storage.TouchUserActivity(ctx, "expired-user", now.AddDate(0, 0, -40)); err != nil {
		t.Fatal(err)
	}
	if err := storage.TouchUserActivity(ctx, "boundary-expired", now.AddDate(0, 0, -35)); err != nil {
		t.Fatal(err)
	}
	if err := storage.TouchUserActivity(ctx, "boundary-kept", now.AddDate(0, 0, -34)); err != nil {
		t.Fatal(err)
	}
	// A current touch alone does not drive global retention; the independent
	// aggregate refresh below must remove expired rows even without user traffic.
	if err := storage.TouchUserActivity(ctx, "test-owner", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ProductAnalyticsSnapshot(ctx, now); err != nil {
		t.Fatal(err)
	}
	var expiredRows int
	if err := storage.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM user_activity_daily WHERE owner_id IN (?, ?)`, "expired-user", "boundary-expired").Scan(&expiredRows); err != nil {
		t.Fatal(err)
	}
	if expiredRows != 0 {
		t.Fatalf("expired activity rows=%d, want 0", expiredRows)
	}
	var boundaryKeptRows int
	if err := storage.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM user_activity_daily WHERE owner_id = ?`, "boundary-kept").Scan(&boundaryKeptRows); err != nil {
		t.Fatal(err)
	}
	if boundaryKeptRows != 1 {
		t.Fatalf("retained boundary activity rows=%d, want 1", boundaryKeptRows)
	}

	var rows int
	var lastSeen time.Time
	if err := storage.db.QueryRowContext(ctx, `SELECT COUNT(*), MAX(last_seen_at)
FROM user_activity_daily WHERE owner_id = ?`, "test-owner").Scan(&rows, &lastSeen); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || !lastSeen.Equal(now.Add(time.Minute)) {
		t.Fatalf("daily activity rows=%d last_seen_at=%s, want one row at %s", rows, lastSeen, now.Add(time.Minute))
	}

	snapshot, err := storage.ProductAnalyticsSnapshot(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DailyActiveUsers != 1 || snapshot.WeeklyActiveUsers != 2 || snapshot.MonthlyActiveUsers != 3 {
		t.Fatalf("active users = DAU %d, WAU %d, MAU %d; want 1, 2, 3",
			snapshot.DailyActiveUsers, snapshot.WeeklyActiveUsers, snapshot.MonthlyActiveUsers)
	}

	if err := storage.TouchUserActivity(ctx, "", now); err == nil {
		t.Fatal("TouchUserActivity accepted an empty user id")
	}
	if err := storage.TouchUserActivity(ctx, "test-owner", time.Time{}); err == nil {
		t.Fatal("TouchUserActivity accepted a zero activity time")
	}
	if err := storage.TouchUserActivity(ctx, "missing-user", now); err == nil {
		t.Fatal("TouchUserActivity accepted a missing account")
	}
	if _, err := storage.ProductAnalyticsSnapshot(ctx, time.Time{}); err == nil {
		t.Fatal("ProductAnalyticsSnapshot accepted a zero current time")
	}
}

func TestProductAnalyticsFunnelIsProgressive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "analytics-funnel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	baseline, err := storage.ProductAnalyticsSnapshot(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	users := []string{"registered-only", "linked-only", "channel-only", "post-only", "ready", "published"}
	for _, userID := range users {
		if err := storage.UpsertUser(ctx, User{ID: userID, CreatedAt: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	for index, userID := range users[1:] {
		if _, err := storage.db.ExecContext(ctx, `
INSERT INTO max_identity_links(owner_id, max_user_id, linked_at, updated_at)
VALUES (?, ?, ?, ?)`, userID, 1000+index, now, now); err != nil {
			t.Fatal(err)
		}
	}

	channels := make(map[string]Channel)
	for index, userID := range users[2:] {
		channel, err := storage.CreateChannel(ctx, Channel{
			UserID: userID, VerifiedMAXOwnerID: "max-owner-" + userID,
			MAXChatID: "analytics-chat-" + userID, Title: userID,
			IsChannel: true, Active: true,
		})
		if err != nil {
			t.Fatalf("create channel %d for %s: %v", index, userID, err)
		}
		channels[userID] = channel
	}

	postOnlyChannel := channels["post-only"]
	if _, err := storage.CreatePost(ctx, Post{
		UserID: "post-only", ChannelID: &postOnlyChannel.ID, Title: "Draft", Status: PostStatusDraft,
	}); err != nil {
		t.Fatal(err)
	}
	readyChannel := channels["ready"]
	scheduledAt := now.Add(time.Hour)
	if _, err := storage.CreatePost(ctx, Post{
		UserID: "ready", ChannelID: &readyChannel.ID, Title: "Scheduled", Status: PostStatusScheduled,
		ScheduledAt: &scheduledAt,
	}); err != nil {
		t.Fatal(err)
	}
	publishedChannel := channels["published"]
	publishedAt := now.Add(-time.Hour)
	if _, err := storage.CreatePost(ctx, Post{
		UserID: "published", ChannelID: &publishedChannel.ID, Title: "Published", Status: PostStatusPublished,
		PublishedAt: &publishedAt, MAXMessageID: "analytics-message",
	}); err != nil {
		t.Fatal(err)
	}

	snapshot, err := storage.ProductAnalyticsSnapshot(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	wants := ProductAnalyticsSnapshot{
		RegisteredUsers:               baseline.RegisteredUsers + 6,
		MAXLinkedUsers:                baseline.MAXLinkedUsers + 5,
		ChannelConnectedUsers:         baseline.ChannelConnectedUsers + 4,
		PostCreatedUsers:              baseline.PostCreatedUsers + 3,
		PostScheduledOrPublishedUsers: baseline.PostScheduledOrPublishedUsers + 2,
		PostPublishedUsers:            baseline.PostPublishedUsers + 1,
	}
	if snapshot.RegisteredUsers != wants.RegisteredUsers ||
		snapshot.MAXLinkedUsers != wants.MAXLinkedUsers ||
		snapshot.ChannelConnectedUsers != wants.ChannelConnectedUsers ||
		snapshot.PostCreatedUsers != wants.PostCreatedUsers ||
		snapshot.PostScheduledOrPublishedUsers != wants.PostScheduledOrPublishedUsers ||
		snapshot.PostPublishedUsers != wants.PostPublishedUsers {
		t.Fatalf("funnel = %#v, want counts from %#v", snapshot, wants)
	}
	if snapshot.RegisteredUsers < snapshot.MAXLinkedUsers ||
		snapshot.MAXLinkedUsers < snapshot.ChannelConnectedUsers ||
		snapshot.ChannelConnectedUsers < snapshot.PostCreatedUsers ||
		snapshot.PostCreatedUsers < snapshot.PostScheduledOrPublishedUsers ||
		snapshot.PostScheduledOrPublishedUsers < snapshot.PostPublishedUsers {
		t.Fatalf("funnel is not progressive: %#v", snapshot)
	}
}

func TestProductAnalyticsCountsPublishedAttachmentAdoption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "analytics-media.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	baseline, err := storage.ProductAnalyticsSnapshot(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	const owner = "analytics-media-owner"
	if err := storage.UpsertUser(ctx, User{ID: owner, DisplayName: owner}); err != nil {
		t.Fatal(err)
	}
	for _, filename := range []string{"single.png", "multi-1.png", "multi-2.png", "video.mp4", "mixed.png", "mixed.mp4"} {
		reservation, err := storage.ReserveMedia(ctx, owner, filename, 100,
			MediaLimits{MaxFiles: 20, MaxBytes: 1 << 30}, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := storage.CompleteMediaReservation(ctx, reservation, now); err != nil {
			t.Fatal(err)
		}
	}
	createPublished := func(title string, attachments []PostAttachment) {
		t.Helper()
		publishedAt := now.Add(-time.Hour)
		post, err := storage.CreatePost(ctx, Post{
			UserID: owner, Title: title, Status: PostStatusPublished, PublishedAt: &publishedAt,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, attachment := range attachments {
			if _, err := storage.AddPostAttachmentForUser(ctx, owner, post.ID, attachment); err != nil {
				t.Fatal(err)
			}
		}
	}
	image := func(key string) PostAttachment {
		return PostAttachment{Type: PostAttachmentImage, Position: -1, StorageKey: key, SizeBytes: 100, MIMEType: "image/png"}
	}
	video := func(key string) PostAttachment {
		return PostAttachment{Type: PostAttachmentVideo, Position: -1, StorageKey: key, SizeBytes: 100, MIMEType: "video/mp4"}
	}
	createPublished("text only", nil)
	createPublished("single image", []PostAttachment{image("single.png")})
	createPublished("gallery", []PostAttachment{image("multi-1.png"), image("multi-2.png")})
	createPublished("video", []PostAttachment{video("video.mp4")})
	createPublished("mixed", []PostAttachment{image("mixed.png"), video("mixed.mp4")})

	snapshot, err := storage.ProductAnalyticsSnapshot(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.PublishedPosts != baseline.PublishedPosts+5 ||
		snapshot.PublishedPostsWithMedia != baseline.PublishedPostsWithMedia+4 ||
		snapshot.PublishedPostsWithMultiple != baseline.PublishedPostsWithMultiple+2 ||
		snapshot.PublishedPostsWithVideo != baseline.PublishedPostsWithVideo+2 ||
		snapshot.PublishedPostsWithMixedMedia != baseline.PublishedPostsWithMixedMedia+1 {
		t.Fatalf("media adoption snapshot=%#v baseline=%#v", snapshot, baseline)
	}
}

func TestProductAnalyticsHonorsContextCancellation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "analytics-context.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = storage.ProductAnalyticsSnapshot(canceled, time.Now())
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("ProductAnalyticsSnapshot() error = %v, want context.Canceled", err)
	}
}
