package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestPublicationMetadataIsAtomicTenantScopedAndHistorical(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "publication-metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	channel, err := storage.CreateChannel(ctx, Channel{MAXChatID: "-101", Title: "Channel", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	createPublished := func(messageID string) Post {
		post, createErr := storage.CreatePost(ctx, Post{
			Title: messageID, Content: "body", Format: FormatMarkdown, Status: PostStatusPublished,
			ChannelID: &channel.ID, MAXMessageID: messageID,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return post
	}
	first := createPublished("mid.first")
	second := createPublished("mid.second")
	firstViews := int64(7)
	firstSync := time.Date(2035, time.January, 2, 3, 4, 5, 0, time.UTC)
	first, err = storage.SyncPublicationMetadataForUser(ctx, "test-owner", first.ID, channel.ID, first.MAXMessageID,
		"https://max.ru/channel/first", &firstViews, firstSync, true)
	if err != nil {
		t.Fatal(err)
	}
	if first.MAXMessageURL != "https://max.ru/channel/first" || first.MAXViews == nil || *first.MAXViews != 7 ||
		first.MAXStatsSyncedAt == nil || !first.MAXStatsSyncedAt.Equal(firstSync) || !first.MAXIsPinned {
		t.Fatalf("first synced post = %#v", first)
	}
	secondViews := int64(11)
	secondSync := firstSync.Add(time.Hour)
	second, err = storage.SyncPublicationMetadataForUser(ctx, "test-owner", second.ID, channel.ID, second.MAXMessageID,
		"https://max.ru/channel/second", &secondViews, secondSync, true)
	if err != nil {
		t.Fatal(err)
	}
	first, err = storage.GetPost(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.MAXIsPinned || !second.MAXIsPinned {
		t.Fatalf("single channel pin was not reconciled: first=%#v second=%#v", first, second)
	}
	secondViews = 14
	if _, err := storage.SyncPublicationMetadataForUser(ctx, "test-owner", second.ID, channel.ID, second.MAXMessageID,
		second.MAXMessageURL, &secondViews, secondSync.Add(time.Hour), true); err != nil {
		t.Fatal(err)
	}
	snapshots, err := storage.ListPostViewSnapshotsForUser(ctx, "test-owner", second.ID, nil, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 || snapshots[0].Views != 14 || snapshots[1].Views != 11 ||
		snapshots[0].MAXMessageID != second.MAXMessageID || !snapshots[0].CapturedAt.Equal(secondSync.Add(time.Hour)) {
		t.Fatalf("view snapshots = %#v", snapshots)
	}
	if _, err := storage.ListPostViewSnapshotsForUser(ctx, "foreign-owner", second.ID, nil, 500); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign history error = %v, want ErrNotFound", err)
	}
	if _, err := storage.SyncPublicationMetadataForUser(ctx, "foreign-owner", second.ID, channel.ID, second.MAXMessageID,
		"https://max.ru/stolen", &secondViews, secondSync, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign sync error = %v, want ErrNotFound", err)
	}
}

func TestMarkAndClearPublicationMetadata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "publication-lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	channel, err := storage.CreateChannel(ctx, Channel{MAXChatID: "-102", Title: "Channel", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(ctx, Post{Title: "Post", Content: "body", Status: PostStatusPublishing, ChannelID: &channel.ID})
	if err != nil {
		t.Fatal(err)
	}
	post, err = storage.MarkPublished(ctx, post.ID, "mid.lifecycle", "https://max.ru/channel/lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	if post.MAXMessageID != "mid.lifecycle" || post.MAXMessageURL != "https://max.ru/channel/lifecycle" || post.MAXIsPinned || post.MAXViews != nil {
		t.Fatalf("marked post = %#v", post)
	}
	if post.PublishedAt == nil {
		t.Fatal("marked post has no published_at")
	}
	publishedAt := *post.PublishedAt
	views := int64(3)
	firstSyncAt := publishedAt.Add(time.Minute)
	post, err = storage.SyncPublicationMetadataForUser(ctx, "test-owner", post.ID, channel.ID, post.MAXMessageID,
		post.MAXMessageURL, &views, firstSyncAt, true)
	if err != nil {
		t.Fatal(err)
	}
	if post.MAXViews == nil || !post.MAXIsPinned || post.MAXStatsSyncedAt == nil {
		t.Fatalf("sync did not set lifecycle metadata: %#v", post)
	}
	if _, err := storage.ClearPublicationForUser(ctx, "foreign-owner", post.ID, channel.ID, post.MAXMessageID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign clear error = %v, want ErrNotFound", err)
	}
	if _, err := storage.ClearPublicationForUser(ctx, "test-owner", post.ID, channel.ID, "mid.stale"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale clear error = %v, want ErrConflict", err)
	}
	post, err = storage.ClearPublicationForUser(ctx, "test-owner", post.ID, channel.ID, post.MAXMessageID)
	if err != nil {
		t.Fatal(err)
	}
	if post.Status != PostStatusDraft || post.MAXMessageID != "" || post.MAXMessageURL != "" ||
		post.MAXViews == nil || *post.MAXViews != views || post.MAXStatsSyncedAt == nil ||
		!post.MAXStatsSyncedAt.Equal(firstSyncAt) || post.MAXStatsAttemptedAt != nil || post.MAXIsPinned ||
		post.PublishedAt == nil || !post.PublishedAt.Equal(publishedAt) || post.LastError != "" {
		t.Fatalf("cleared post = %#v", post)
	}
	history, err := storage.ListPostViewSnapshotsForUser(ctx, "test-owner", post.ID, nil, 500)
	if err != nil || len(history) != 1 || history[0].MAXMessageID != "mid.lifecycle" || history[0].Views != views {
		t.Fatalf("preserved explicit-delete history = %#v, %v", history, err)
	}
	post, err = storage.ClaimForPublishing(ctx, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	post, err = storage.MarkPublished(ctx, post.ID, "mid.lifecycle.second", "https://max.ru/channel/lifecycle-second")
	if err != nil {
		t.Fatal(err)
	}
	secondViews := int64(1)
	if _, err := storage.SyncPublicationMetadataForUser(ctx, "test-owner", post.ID, channel.ID, post.MAXMessageID,
		post.MAXMessageURL, &secondViews, time.Now().UTC().Add(time.Minute), false); err != nil {
		t.Fatal(err)
	}
	history, err = storage.ListPostViewSnapshotsForUser(ctx, "test-owner", post.ID, nil, 500)
	if err != nil || len(history) != 2 || history[0].MAXMessageID != "mid.lifecycle.second" || history[1].MAXMessageID != "mid.lifecycle" {
		t.Fatalf("publication-segmented history = %#v, %v", history, err)
	}
}

func TestClearPublicationSupportsLegacyStatesButNeverPublishing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "legacy-publication-clear.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	channel, err := storage.CreateChannel(ctx, Channel{
		MAXChatID: "-legacy-clear", Title: "Channel", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	publishedAt := time.Date(2038, time.April, 7, 8, 0, 0, 0, time.UTC)
	failed, err := storage.CreatePost(ctx, Post{
		Title: "Legacy failed", Content: "body", Format: FormatMarkdown, Status: PostStatusFailed,
		ChannelID: &channel.ID, MAXMessageID: "mid.legacy-failed", MAXMessageURL: "https://max.ru/channel/legacy-failed",
		LastError: "legacy failure", PublishedAt: &publishedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, err = storage.ClearPublicationForUser(ctx, "test-owner", failed.ID, channel.ID, failed.MAXMessageID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != PostStatusDraft || failed.MAXMessageID != "" || failed.MAXMessageURL != "" ||
		failed.LastError != "" || failed.PublishedAt == nil || !failed.PublishedAt.Equal(publishedAt) {
		t.Fatalf("cleared legacy failed publication = %#v", failed)
	}
	if err := storage.DeletePostForUser(ctx, "test-owner", failed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetPostForUser(ctx, "test-owner", failed.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted legacy post lookup = %v, want ErrNotFound", err)
	}
	draft, err := storage.CreatePost(ctx, Post{
		Title: "Legacy draft", Content: "body", Format: FormatMarkdown, Status: PostStatusDraft,
		ChannelID: &channel.ID, MAXMessageID: "mid.legacy-draft", MAXMessageURL: "https://max.ru/channel/legacy-draft",
	})
	if err != nil {
		t.Fatal(err)
	}
	draft, err = storage.ClearPublicationForUser(ctx, "test-owner", draft.ID, channel.ID, draft.MAXMessageID)
	if err != nil || draft.Status != PostStatusDraft || draft.MAXMessageID != "" || draft.MAXMessageURL != "" {
		t.Fatalf("cleared legacy draft publication = %#v, %v", draft, err)
	}

	publishing, err := storage.CreatePost(ctx, Post{
		Title: "Publishing", Content: "body", Format: FormatMarkdown, Status: PostStatusPublishing,
		ChannelID: &channel.ID, MAXMessageID: "mid.still-publishing",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ClearPublicationForUser(ctx, "test-owner", publishing.ID, channel.ID,
		publishing.MAXMessageID); !errors.Is(err, ErrConflict) {
		t.Fatalf("publishing clear error = %v, want ErrConflict", err)
	}
	publishing, err = storage.GetPostForUser(ctx, "test-owner", publishing.ID)
	if err != nil || publishing.Status != PostStatusPublishing || publishing.MAXMessageID != "mid.still-publishing" {
		t.Fatalf("publishing post changed = %#v, %v", publishing, err)
	}
}

func TestMarkMAXPublicationMissingPreservesHistoryAndAllowsRepublish(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "missing-publication.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	channel, err := storage.CreateChannel(ctx, Channel{MAXChatID: "-120", Title: "Channel", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	publishedAt := time.Date(2036, time.January, 2, 3, 4, 5, 0, time.UTC)
	post, err := storage.CreatePost(ctx, Post{
		Title: "Deleted remotely", Content: "body", Format: FormatMarkdown, Status: PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: "mid.deleted", MAXMessageURL: "https://max.ru/channel/deleted",
		PublishedAt: &publishedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	views := int64(17)
	post, err = storage.SyncPublicationMetadataForUser(ctx, "test-owner", post.ID, channel.ID, post.MAXMessageID,
		post.MAXMessageURL, &views, publishedAt.Add(time.Hour), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.MarkMAXPublicationMissingForUser(ctx, "foreign-owner", post.ID, channel.ID, post.MAXMessageID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign reconciliation error = %v, want ErrNotFound", err)
	}
	if _, err := storage.MarkMAXPublicationMissingForUser(ctx, "test-owner", post.ID, channel.ID, "mid.other"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale reconciliation error = %v, want ErrConflict", err)
	}
	post, err = storage.MarkMAXPublicationMissingForUser(ctx, "test-owner", post.ID, channel.ID, post.MAXMessageID)
	if err != nil {
		t.Fatal(err)
	}
	if post.Status != PostStatusFailed || post.LastError != MAXPublicationMissingLastError ||
		post.MAXMessageID != "" || post.MAXMessageURL != "" || post.MAXViews == nil || *post.MAXViews != views ||
		post.MAXStatsSyncedAt == nil || !post.MAXStatsSyncedAt.Equal(publishedAt.Add(time.Hour)) ||
		post.MAXStatsAttemptedAt != nil || post.MAXIsPinned || post.PublishedAt == nil || !post.PublishedAt.Equal(publishedAt) {
		t.Fatalf("reconciled post = %#v", post)
	}
	history, err := storage.ListPostViewSnapshotsForUser(ctx, "test-owner", post.ID, nil, 500)
	if err != nil || len(history) != 1 || history[0].MAXMessageID != "mid.deleted" || history[0].Views != views {
		t.Fatalf("preserved history = %#v, %v", history, err)
	}
	// Reconciliation is idempotent, and a failed post remains publishable.
	if _, err := storage.MarkMAXPublicationMissingForUser(ctx, "test-owner", post.ID, channel.ID, "mid.deleted"); err != nil {
		t.Fatal(err)
	}
	post, err = storage.ClaimForPublishing(ctx, post.ID)
	if err != nil || post.Status != PostStatusPublishing || post.LastError != "" {
		t.Fatalf("republish claim = %#v, %v", post, err)
	}
}

func TestListPostsDueForStatsUsesLatestSuccessfulSync(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "stats-due.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	channel, err := storage.CreateChannel(ctx, Channel{MAXChatID: "-103", Title: "Channel", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2036, time.February, 3, 12, 0, 0, 0, time.UTC)
	oldPublished := now.Add(-2 * time.Hour)
	due, err := storage.CreatePost(ctx, Post{
		Title: "Due", Content: "body", Format: FormatMarkdown, Status: PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: "mid.due", PublishedAt: &oldPublished,
	})
	if err != nil {
		t.Fatal(err)
	}
	recentPublished := now.Add(-30 * time.Minute)
	if _, err := storage.CreatePost(ctx, Post{
		Title: "Recent", Content: "body", Format: FormatMarkdown, Status: PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: "mid.recent", PublishedAt: &recentPublished,
	}); err != nil {
		t.Fatal(err)
	}
	posts, err := storage.ListPostsDueForStats(ctx, now, time.Hour, 10)
	if err != nil || len(posts) != 1 || posts[0].ID != due.ID || posts[0].UserID != "test-owner" {
		t.Fatalf("due posts = %#v, %v", posts, err)
	}
	views := int64(1)
	if _, err := storage.SyncPublicationMetadataForUser(ctx, "test-owner", due.ID, channel.ID, due.MAXMessageID,
		"https://max.ru/channel/due", &views, now, false); err != nil {
		t.Fatal(err)
	}
	posts, err = storage.ListPostsDueForStats(ctx, now.Add(29*time.Minute), time.Hour, 10)
	if err != nil || len(posts) != 0 {
		t.Fatalf("recently synced posts = %#v, %v", posts, err)
	}
}
