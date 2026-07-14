package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestChannelVisualMetadataPersistsAcrossOperations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "channel-metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	channel, err := storage.CreateChannel(ctx, Channel{
		MAXChatID: "visual-1", Title: "Visual", PublicLink: "https://max.ru/visual",
		IconURL: "https://cdn.max.ru/visual.png", ParticipantsCount: 1250, IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if channel.IconURL != "https://cdn.max.ru/visual.png" || channel.ParticipantsCount != 1250 {
		t.Fatalf("created channel = %#v", channel)
	}

	title := "Visual renamed"
	active := false
	channel, err = storage.UpdateChannel(ctx, channel.ID, nil, &title, &active)
	if err != nil {
		t.Fatal(err)
	}
	if channel.IconURL != "https://cdn.max.ru/visual.png" || channel.ParticipantsCount != 1250 {
		t.Fatalf("manual update lost visual metadata: %#v", channel)
	}

	channel, err = storage.UpsertConnectedChannel(ctx, Channel{
		MAXChatID: "visual-1", Title: "Fresh MAX metadata", PublicLink: "https://max.ru/visual",
		IconURL: "https://cdn.max.ru/visual-new.png", ParticipantsCount: 1301, IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if channel.IconURL != "https://cdn.max.ru/visual-new.png" || channel.ParticipantsCount != 1301 {
		t.Fatalf("connected upsert did not refresh visual metadata: %#v", channel)
	}

	channel, err = storage.UpsertDiscoveredChannel(ctx, "visual-1", "Webhook title", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if channel.IconURL != "https://cdn.max.ru/visual-new.png" || channel.ParticipantsCount != 1301 {
		t.Fatalf("webhook upsert lost visual metadata: %#v", channel)
	}

	channels, err := storage.ListChannels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 1 || channels[0].IconURL != channel.IconURL || channels[0].ParticipantsCount != channel.ParticipantsCount {
		t.Fatalf("listed channels = %#v", channels)
	}
}

func TestPostLifecycleAndScheduling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	channel, err := storage.CreateChannel(ctx, Channel{
		MAXChatID: "-12345", Title: "Test channel", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(ctx, Post{
		Title: "Draft", Content: "Hello", Format: FormatMarkdown, Status: PostStatusDraft,
		ChannelID: &channel.ID, Notify: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	dueAt := time.Now().UTC().Add(time.Minute)
	post, err = storage.SetPostScheduled(ctx, post.ID, dueAt)
	if err != nil {
		t.Fatal(err)
	}
	if post.Status != PostStatusScheduled || post.ScheduledAt == nil {
		t.Fatalf("unexpected scheduled post: %#v", post)
	}
	due, err := storage.DuePostIDs(ctx, dueAt.Add(time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0] != post.ID {
		t.Fatalf("unexpected due IDs: %v", due)
	}

	post, err = storage.ClaimForPublishing(ctx, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if post.Status != PostStatusPublishing {
		t.Fatalf("status = %q", post.Status)
	}
	post, err = storage.MarkPublished(ctx, post.ID, "message-1")
	if err != nil {
		t.Fatal(err)
	}
	if post.Status != PostStatusPublished || post.MAXMessageID != "message-1" || post.PublishedAt == nil {
		t.Fatalf("unexpected published post: %#v", post)
	}

	copy, err := storage.DuplicatePost(ctx, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if copy.Status != PostStatusDraft || copy.MAXMessageID != "" || copy.PublishedAt != nil {
		t.Fatalf("duplicate retained publication state: %#v", copy)
	}
}

func TestPublishingStateCASAndRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	channel, err := storage.CreateChannel(ctx, Channel{MAXChatID: "10", Title: "Channel", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(ctx, Post{
		Title: "Post", Content: "body", Format: FormatMarkdown, Status: PostStatusDraft, ChannelID: &channel.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err = storage.ClaimForPublishing(ctx, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	newTitle := "racing autosave"
	if _, err := storage.UpdatePost(ctx, post.ID, PostChanges{Title: &newTitle}); !errors.Is(err, ErrConflict) {
		t.Fatalf("UpdatePost() error = %v, want ErrConflict", err)
	}
	if err := storage.DeletePost(ctx, post.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("DeletePost() error = %v, want ErrConflict", err)
	}
	if _, err := storage.DuplicatePost(ctx, post.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("DuplicatePost() error = %v, want ErrConflict", err)
	}
	post, err = storage.MarkPublished(ctx, post.ID, "mid-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.MarkPublished(ctx, post.ID, "mid-2"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second MarkPublished() error = %v, want ErrConflict", err)
	}
	if _, err := storage.SetPostScheduled(ctx, post.ID, time.Now().Add(time.Hour)); !errors.Is(err, ErrConflict) {
		t.Fatalf("SetPostScheduled(published) error = %v, want ErrConflict", err)
	}

	other, err := storage.CreateChannel(ctx, Channel{MAXChatID: "11", Title: "Other", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	otherID := &other.ID
	if _, err := storage.UpdatePost(ctx, post.ID, PostChanges{ChannelID: &otherID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("published channel change error = %v, want ErrConflict", err)
	}
	changedPreview := !post.DisableLinkPreview
	if _, err := storage.UpdatePost(ctx, post.ID, PostChanges{DisableLinkPreview: &changedPreview}); !errors.Is(err, ErrConflict) {
		t.Fatalf("published link preview change error = %v, want ErrConflict", err)
	}

	stale, err := storage.CreatePost(ctx, Post{
		Title: "Stale", Content: "body", Format: FormatMarkdown, Status: PostStatusDraft, ChannelID: &channel.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	due := time.Now().UTC().Add(-time.Hour)
	if _, err := storage.SetPostScheduled(ctx, stale.ID, due); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ClaimForPublishing(ctx, stale.ID); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-20 * time.Minute).Format(time.RFC3339Nano)
	if _, err := storage.db.ExecContext(ctx, `UPDATE posts SET updated_at = ? WHERE id = ?`, old, stale.ID); err != nil {
		t.Fatal(err)
	}
	recovered, err := storage.RecoverStalePublishing(ctx, time.Now().UTC().Add(-10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1", recovered)
	}
	stale, err = storage.GetPost(ctx, stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stale.Status != PostStatusFailed || stale.ScheduledAt != nil || stale.LastError == "" {
		t.Fatalf("unexpected recovered post: %#v", stale)
	}
}

func TestChannelDeletionProtectsPublicationDependencies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "channel-delete.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	channel, err := storage.CreateChannel(ctx, Channel{MAXChatID: "20", Title: "Protected", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	changedID := "21"
	if _, err := storage.UpdateChannel(ctx, channel.ID, &changedID, nil, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("UpdateChannel(max_chat_id) error = %v, want ErrConflict", err)
	}

	newPost := func(title string) Post {
		post, createErr := storage.CreatePost(ctx, Post{
			Title: title, Content: "body", Format: FormatMarkdown, Status: PostStatusDraft, ChannelID: &channel.ID,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return post
	}
	draft := newPost("Draft")
	scheduled := newPost("Scheduled")
	if scheduled, err = storage.SetPostScheduled(ctx, scheduled.ID, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	publishing := newPost("Publishing")
	if publishing, err = storage.ClaimForPublishing(ctx, publishing.ID); err != nil {
		t.Fatal(err)
	}
	published := newPost("Published")
	if published, err = storage.ClaimForPublishing(ctx, published.ID); err != nil {
		t.Fatal(err)
	}
	if published, err = storage.MarkPublished(ctx, published.ID, "mid-20"); err != nil {
		t.Fatal(err)
	}

	count, err := storage.CountChannelBlockingPosts(ctx, channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("blocking post count = %d, want 3", count)
	}
	if err := storage.DeleteChannel(ctx, channel.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("DeleteChannel() error = %v, want ErrConflict", err)
	}
	if _, err := storage.GetChannel(ctx, channel.ID); err != nil {
		t.Fatalf("protected channel disappeared: %v", err)
	}

	if _, err := storage.CancelSchedule(ctx, scheduled.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.MarkPublishFailed(ctx, publishing.ID, "stopped"); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ClearPublication(ctx, published.ID); err != nil {
		t.Fatal(err)
	}
	count, err = storage.CountChannelBlockingPosts(ctx, channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("blocking post count after cleanup = %d, want 0", count)
	}
	if err := storage.DeleteChannel(ctx, channel.ID); err != nil {
		t.Fatal(err)
	}
	for _, postID := range []int64{draft.ID, scheduled.ID, publishing.ID, published.ID} {
		post, getErr := storage.GetPost(ctx, postID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if post.ChannelID != nil {
			t.Errorf("post %d channel_id = %d after channel deletion, want nil", postID, *post.ChannelID)
		}
	}
}

func TestObservedBotChatUpsertIsIdempotentAndOrdered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	now := time.Now().UTC()
	if err := storage.UpsertObservedBotChat(ctx, ObservedBotChat{
		MAXChatID: "777", PublicLink: "https://max.ru/first", Title: "First", Active: true, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := storage.MarkObservedBotChatRemoved(ctx, "777", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := storage.UpsertObservedBotChat(ctx, ObservedBotChat{
		MAXChatID: "777", PublicLink: "https://max.ru/first", Title: "Equal-time replay", Active: true, LastSeenAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetActiveObservedBotChat(ctx, "", "777"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale bot_added reactivated removed chat: %v", err)
	}
}

func TestCalendarStateTransitionsAndUTCOrdering(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "calendar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	channel, err := storage.CreateChannel(ctx, Channel{MAXChatID: "calendar", Title: "Calendar", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	newPost := func(title string) Post {
		post, createErr := storage.CreatePost(ctx, Post{
			Title: title, Content: "body", Format: FormatMarkdown, Status: PostStatusDraft, ChannelID: &channel.ID,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return post
	}

	moscow := time.FixedZone("MSK", 3*60*60)
	baseUTC := time.Date(2030, time.March, 10, 9, 0, 0, 500_000_000, time.UTC)
	firstAt := baseUTC.Add(200 * time.Millisecond).In(moscow)
	first := newPost("First")
	firstAtPointer := &firstAt
	first, err = storage.UpdatePost(ctx, first.ID, PostChanges{ScheduledAt: &firstAtPointer})
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != PostStatusScheduled || first.ScheduledAt == nil || first.ScheduledAt.Location() != time.UTC || !first.ScheduledAt.Equal(firstAt) {
		t.Fatalf("scheduled post was not normalized to UTC: %#v", first)
	}

	secondAt := baseUTC.Add(-200 * time.Millisecond).In(time.FixedZone("UTC-4", -4*60*60))
	second := newPost("Second")
	second, err = storage.SetPostScheduled(ctx, second.ID, secondAt)
	if err != nil {
		t.Fatal(err)
	}
	due, err := storage.DuePostIDs(ctx, baseUTC, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0] != second.ID {
		t.Fatalf("fractional/offset due IDs = %v, want [%d]", due, second.ID)
	}

	listed, err := storage.ListPosts(ctx, PostStatusScheduled, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].ID != second.ID || listed[1].ID != first.ID {
		t.Fatalf("scheduled order = %#v", listed)
	}

	postponed := baseUTC.Add(time.Hour)
	postponedPointer := &postponed
	first, err = storage.UpdatePost(ctx, first.ID, PostChanges{ScheduledAt: &postponedPointer})
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != PostStatusScheduled || first.ScheduledAt == nil || !first.ScheduledAt.Equal(postponed) {
		t.Fatalf("rescheduled post = %#v", first)
	}
	none := (*time.Time)(nil)
	first, err = storage.UpdatePost(ctx, first.ID, PostChanges{ScheduledAt: &none})
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != PostStatusDraft || first.ScheduledAt != nil {
		t.Fatalf("canceled post = %#v", first)
	}
	first, err = storage.CancelSchedule(ctx, first.ID)
	if err != nil || first.Status != PostStatusDraft || first.ScheduledAt != nil {
		t.Fatalf("idempotent cancel = %#v, %v", first, err)
	}
}

func TestDueSelectionCannotPublishAfterPostponeOrCancel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "claim-races.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	post, err := storage.CreatePost(ctx, Post{Title: "Race", Content: "body", Format: FormatMarkdown})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2031, time.January, 1, 12, 0, 0, 0, time.UTC)
	if _, err = storage.SetPostScheduled(ctx, post.ID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	selected, err := storage.DuePostIDs(ctx, now, 10)
	if err != nil || len(selected) != 1 {
		t.Fatalf("selected = %v, error = %v", selected, err)
	}
	if _, err = storage.SetPostScheduled(ctx, post.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = storage.ClaimScheduledForPublishing(ctx, selected[0], now); !errors.Is(err, ErrScheduleNotDue) {
		t.Fatalf("claim postponed post error = %v, want ErrScheduleNotDue", err)
	}
	post, _ = storage.GetPost(ctx, post.ID)
	if post.Status != PostStatusScheduled || post.ScheduledAt == nil || !post.ScheduledAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("postpone state = %#v", post)
	}

	if _, err = storage.SetPostScheduled(ctx, post.ID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	selected, err = storage.DuePostIDs(ctx, now, 10)
	if err != nil || len(selected) != 1 {
		t.Fatalf("selected before cancel = %v, error = %v", selected, err)
	}
	if _, err = storage.CancelSchedule(ctx, post.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = storage.ClaimScheduledForPublishing(ctx, selected[0], now); !errors.Is(err, ErrScheduleNotDue) {
		t.Fatalf("claim canceled post error = %v, want ErrScheduleNotDue", err)
	}
	post, _ = storage.GetPost(ctx, post.ID)
	if post.Status != PostStatusDraft || post.ScheduledAt != nil {
		t.Fatalf("cancel state = %#v", post)
	}
}

func TestClaimAndCancelAreMutuallyExclusive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "claim-cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	post, err := storage.CreatePost(ctx, Post{Title: "Concurrent", Content: "body", Format: FormatMarkdown})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err = storage.SetPostScheduled(ctx, post.ID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errorsByOperation := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		_, claimErr := storage.ClaimScheduledForPublishing(ctx, post.ID, now)
		errorsByOperation <- claimErr
	}()
	go func() {
		defer workers.Done()
		<-start
		_, cancelErr := storage.CancelSchedule(ctx, post.ID)
		errorsByOperation <- cancelErr
	}()
	close(start)
	workers.Wait()
	close(errorsByOperation)
	successes := 0
	for operationErr := range errorsByOperation {
		if operationErr == nil {
			successes++
			continue
		}
		if !errors.Is(operationErr, ErrScheduleNotDue) && !errors.Is(operationErr, ErrConflict) {
			t.Fatalf("unexpected competing operation error: %v", operationErr)
		}
	}
	if successes != 1 {
		t.Fatalf("successful competing operations = %d, want 1", successes)
	}
	post, err = storage.GetPost(ctx, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if post.Status != PostStatusPublishing && post.Status != PostStatusDraft {
		t.Fatalf("final post state = %#v", post)
	}
}

func TestStaleAutosaveCannotRevertPublicationOrReschedule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "stale-autosave.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	now := time.Date(2032, time.April, 5, 12, 0, 0, 0, time.UTC)
	post, err := storage.CreatePost(ctx, Post{
		Title: "Scheduled", Content: "body", Format: FormatMarkdown,
	})
	if err != nil {
		t.Fatal(err)
	}
	if post, err = storage.SetPostScheduled(ctx, post.ID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	staleScheduled := post
	if _, err = storage.ClaimScheduledForPublishing(ctx, post.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = storage.MarkPublished(ctx, post.ID, "max-message-1"); err != nil {
		t.Fatal(err)
	}
	staleTitle := "stale browser edit"
	if _, err = storage.updatePostSnapshot(ctx, staleScheduled, PostChanges{Title: &staleTitle}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale post-publication autosave error = %v, want ErrConflict", err)
	}
	post, err = storage.GetPost(ctx, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if post.Status != PostStatusPublished || post.ScheduledAt != nil || post.MAXMessageID != "max-message-1" {
		t.Fatalf("stale autosave reverted publication lifecycle: %#v", post)
	}

	second, err := storage.CreatePost(ctx, Post{
		Title: "Move me", Content: "body", Format: FormatMarkdown,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second, err = storage.SetPostScheduled(ctx, second.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	staleBeforeMove := second
	movedAt := now.Add(2 * time.Hour)
	concurrentUpdatedAt := now.Add(time.Second).Format(time.RFC3339Nano)
	if _, err = storage.db.ExecContext(ctx, `
UPDATE posts SET scheduled_at = ?, updated_at = ? WHERE id = ?`,
		movedAt.Format(time.RFC3339Nano), concurrentUpdatedAt, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = storage.updatePostSnapshot(ctx, staleBeforeMove, PostChanges{Title: &staleTitle}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale post-reschedule autosave error = %v, want ErrConflict", err)
	}
	second, err = storage.GetPost(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != PostStatusScheduled || second.ScheduledAt == nil || !second.ScheduledAt.Equal(movedAt) {
		t.Fatalf("stale autosave reverted reschedule: %#v", second)
	}

	third, err := storage.CreatePost(ctx, Post{
		Title: "Validate me", Content: "body", Format: FormatMarkdown,
	})
	if err != nil {
		t.Fatal(err)
	}
	validatedSnapshot := third
	if _, err = storage.db.ExecContext(ctx, `
UPDATE posts SET content = '', updated_at = ? WHERE id = ?`,
		now.Add(2*time.Second).Format(time.RFC3339Nano), third.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = storage.SetPostScheduledIfUnchanged(ctx, validatedSnapshot, now.Add(time.Hour)); !errors.Is(err, ErrConflict) {
		t.Fatalf("schedule of stale validated snapshot error = %v, want ErrConflict", err)
	}
	third, err = storage.GetPost(ctx, third.ID)
	if err != nil {
		t.Fatal(err)
	}
	if third.Status != PostStatusDraft || third.ScheduledAt != nil || third.Content != "" {
		t.Fatalf("stale validation scheduled changed post: %#v", third)
	}
}

func TestCreatePostRejectsInconsistentScheduleState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "invalid-schedule.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	if _, err := storage.CreatePost(ctx, Post{Title: "Invalid", Format: FormatMarkdown, Status: PostStatusScheduled}); err == nil {
		t.Fatal("CreatePost accepted scheduled status without scheduled_at")
	}
	at := time.Now().UTC().Add(time.Hour)
	if _, err := storage.CreatePost(ctx, Post{Title: "Invalid", Format: FormatMarkdown, Status: PostStatusDraft, ScheduledAt: &at}); err == nil {
		t.Fatal("CreatePost accepted scheduled_at with draft status")
	}
}
