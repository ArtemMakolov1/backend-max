package app

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/store"
)

func TestSyncMAXPublicationPersistsURLViewsPinAndHistory(t *testing.T) {
	t.Parallel()
	views := int64(81)
	now := time.Date(2037, time.March, 4, 5, 6, 7, 0, time.UTC)
	fake := &fakeMAX{
		chat:          maxclient.ChatInfo{ChatID: "-201", Type: "channel", Status: "active", Title: "Channel"},
		message:       maxclient.Message{MessageID: "mid.stats", ChatID: "-201", URL: "https://max.ru/channel/stats", Views: &views},
		pinnedMessage: &maxclient.Message{MessageID: "mid.stats", ChatID: "-201"},
	}
	application, storage := newTestApp(t, fake)
	application.now = func() time.Time { return now }
	firstSyncAt := now
	channel, err := storage.CreateChannel(context.Background(), store.Channel{
		MAXChatID: "-201", Title: "Channel", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(context.Background(), store.Post{
		Title: "Stats", Content: "body", Format: store.FormatMarkdown, Status: store.PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: "mid.stats",
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err = application.SyncMAXPublication(context.Background(), "test-owner", post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if post.MAXMessageURL != fake.message.URL || post.MAXViews == nil || *post.MAXViews != views ||
		post.MAXStatsSyncedAt == nil || !post.MAXStatsSyncedAt.Equal(now) || !post.MAXIsPinned {
		t.Fatalf("synced post = %#v", post)
	}
	if fake.getMessageCalls != 1 || fake.getPinnedCalls != 1 {
		t.Fatalf("MAX metadata calls = %#v", fake)
	}
	if _, err := application.SyncMAXPublication(context.Background(), "test-owner", post.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("immediate repeated sync error = %v, want conflict", err)
	} else {
		var cooldownErr *MAXStatsCooldownError
		if !errors.As(err, &cooldownErr) || cooldownErr.RetryAfter != manualMAXStatsCooldown {
			t.Fatalf("immediate repeated sync error = %#v, want %s cooldown", err, manualMAXStatsCooldown)
		}
	}
	if fake.getMessageCalls != 1 {
		t.Fatal("throttled manual sync reached MAX")
	}
	now = now.Add(5 * time.Second)
	if _, err := application.SyncMAXPublication(context.Background(), "test-owner", post.ID); err == nil {
		t.Fatal("sync inside cooldown unexpectedly succeeded")
	} else {
		var cooldownErr *MAXStatsCooldownError
		if !errors.As(err, &cooldownErr) || cooldownErr.RetryAfter != 10*time.Second {
			t.Fatalf("partial cooldown error = %#v, want 10s", err)
		}
	}
	history, err := storage.ListPostViewSnapshotsForUser(context.Background(), "test-owner", post.ID, nil, 500)
	if err != nil || len(history) != 1 || history[0].Views != views || !history[0].CapturedAt.Equal(firstSyncAt) {
		t.Fatalf("view history = %#v, %v", history, err)
	}
	now = now.Add(11 * time.Second)
	views = 82
	fake.pinnedMessage = nil
	post, err = application.SyncMAXPublication(context.Background(), "test-owner", post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if post.MAXIsPinned || post.MAXViews == nil || *post.MAXViews != 82 {
		t.Fatalf("official null pin response was not stored as unpinned: %#v", post)
	}
	history, err = storage.ListPostViewSnapshotsForUser(context.Background(), "test-owner", post.ID, nil, 500)
	if err != nil || len(history) != 2 || history[0].Views != 82 {
		t.Fatalf("second view history = %#v, %v", history, err)
	}
}

func TestSyncMAXPublicationRejectsForeignTenantChannelMismatchAndUpstream404(t *testing.T) {
	t.Parallel()
	views := int64(2)
	fake := &fakeMAX{
		chat:          maxclient.ChatInfo{ChatID: "-202", Type: "channel", Status: "active", Title: "Channel"},
		message:       maxclient.Message{MessageID: "mid.secure", ChatID: "-999", Views: &views},
		pinnedMessage: &maxclient.Message{MessageID: "mid.secure", ChatID: "-202"},
	}
	application, storage := newTestApp(t, fake)
	now := time.Date(2037, time.March, 4, 7, 0, 0, 0, time.UTC)
	application.now = func() time.Time { return now }
	channel, err := storage.CreateChannel(context.Background(), store.Channel{MAXChatID: "-202", Title: "Channel", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(context.Background(), store.Post{
		Title: "Secure", Content: "body", Format: store.FormatMarkdown, Status: store.PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: "mid.secure",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.SyncMAXPublication(context.Background(), "foreign", post.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign sync error = %v, want ErrNotFound", err)
	}
	if fake.getMessageCalls != 0 {
		t.Fatal("foreign post reached MAX")
	}
	if _, err := application.SyncMAXPublication(context.Background(), "test-owner", post.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatched chat error = %v, want ErrConflict", err)
	}
	if fake.getPinnedCalls != 0 {
		t.Fatal("mismatched message continued to pin lookup")
	}
	now = now.Add(16 * time.Second)
	fake.message.ChatID = "-202"
	upstream := &maxclient.Error{StatusCode: http.StatusNotFound, Code: "message.not.found", Message: "No pinned message"}
	fake.getPinnedErr = upstream
	if _, err := application.SyncMAXPublication(context.Background(), "test-owner", post.ID); !errors.Is(err, upstream) {
		t.Fatalf("pin 404 error = %v, want upstream", err)
	}
	stored, err := storage.GetPost(context.Background(), post.ID)
	if err != nil || stored.MAXViews != nil || stored.MAXStatsSyncedAt != nil || stored.MAXIsPinned {
		t.Fatalf("failed sync changed post = %#v, %v", stored, err)
	}
}

func TestSyncMAXPublicationReconcilesMessageDeletedInMAX(t *testing.T) {
	t.Parallel()
	upstream := &maxclient.Error{StatusCode: http.StatusNotFound, Code: "message.not.found", Message: "Message not found"}
	fake := &fakeMAX{
		chat:          maxclient.ChatInfo{ChatID: "-207", Type: "channel", Status: "active", Title: "Channel"},
		getMessageErr: upstream,
	}
	application, storage := newTestApp(t, fake)
	now := time.Date(2038, time.April, 6, 9, 0, 0, 0, time.UTC)
	application.now = func() time.Time { return now }
	channel, err := storage.CreateChannel(context.Background(), store.Channel{MAXChatID: "-207", Title: "Channel", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	publishedAt := now.Add(-time.Hour)
	post, err := storage.CreatePost(context.Background(), store.Post{
		Title: "Gone", Content: "body", Format: store.FormatMarkdown, Status: store.PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: "mid.gone.manual", MAXMessageURL: "https://max.ru/channel/gone",
		PublishedAt: &publishedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	views := int64(12)
	post, err = storage.SyncPublicationMetadataForUser(context.Background(), "test-owner", post.ID, channel.ID,
		post.MAXMessageID, post.MAXMessageURL, &views, now.Add(-time.Minute), false)
	if err != nil {
		t.Fatal(err)
	}
	post, err = application.SyncMAXPublication(context.Background(), "test-owner", post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if post.Status != store.PostStatusFailed || post.LastError != store.MAXPublicationMissingLastError ||
		post.MAXMessageID != "" || post.MAXMessageURL != "" || post.MAXViews == nil || *post.MAXViews != views ||
		post.MAXStatsSyncedAt == nil || post.MAXStatsAttemptedAt != nil ||
		post.PublishedAt == nil || !post.PublishedAt.Equal(publishedAt) {
		t.Fatalf("reconciled post = %#v", post)
	}
	if fake.getMessageCalls != 1 || fake.getPinnedCalls != 0 {
		t.Fatalf("unexpected MAX calls = %#v", fake)
	}
	post, err = application.SyncMAXPublication(context.Background(), "test-owner", post.ID)
	if err != nil || !isStoredMAXPublicationMissing(post) || fake.getMessageCalls != 1 {
		t.Fatalf("idempotent sync = %#v, err=%v, MAX calls=%d", post, err, fake.getMessageCalls)
	}
}

func TestDeletePublicationPreservesHistoryAndIsTenantScoped(t *testing.T) {
	t.Parallel()
	fake := &fakeMAX{
		chat: maxclient.ChatInfo{ChatID: "-208", Type: "channel", Status: "active", Title: "Channel"},
		membership: maxclient.Membership{IsAdmin: true, Permissions: []maxclient.Permission{
			maxclient.PermissionReadAllMessages, maxclient.PermissionDelete,
		}},
	}
	application, storage := newTestApp(t, fake)
	ctx := context.Background()
	channel, err := storage.CreateChannel(ctx, store.Channel{MAXChatID: "-208", Title: "Channel", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	publishedAt := time.Date(2038, time.April, 6, 10, 0, 0, 0, time.UTC)
	post, err := storage.CreatePost(ctx, store.Post{
		Title: "Explicit delete", Content: "body", Format: store.FormatMarkdown, Status: store.PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: "mid.explicit", MAXMessageURL: "https://max.ru/channel/explicit",
		PublishedAt: &publishedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	views := int64(23)
	syncedAt := publishedAt.Add(time.Hour)
	post, err = storage.SyncPublicationMetadataForUser(ctx, "test-owner", post.ID, channel.ID, post.MAXMessageID,
		post.MAXMessageURL, &views, syncedAt, true)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := application.DeletePublication(ctx, "foreign-owner", post.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign delete error = %v, want ErrNotFound", err)
	}
	if fake.deleteCalls != 0 {
		t.Fatal("foreign delete reached MAX")
	}
	post, err = application.DeletePublication(ctx, "test-owner", post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fake.deleteCalls != 1 || post.Status != store.PostStatusDraft || post.MAXMessageID != "" ||
		post.MAXMessageURL != "" || post.MAXViews == nil || *post.MAXViews != views ||
		post.MAXStatsSyncedAt == nil || !post.MAXStatsSyncedAt.Equal(syncedAt) ||
		post.MAXStatsAttemptedAt != nil || post.MAXIsPinned || post.PublishedAt == nil ||
		!post.PublishedAt.Equal(publishedAt) {
		t.Fatalf("deleted publication = %#v, MAX calls=%d", post, fake.deleteCalls)
	}
	history, err := storage.ListPostViewSnapshotsForUser(ctx, "test-owner", post.ID, nil, 500)
	if err != nil || len(history) != 1 || history[0].MAXMessageID != "mid.explicit" || history[0].Views != views {
		t.Fatalf("preserved view history = %#v, %v", history, err)
	}
	post, err = storage.ClaimForPublishing(ctx, post.ID)
	if err != nil || post.Status != store.PostStatusPublishing {
		t.Fatalf("republish claim = %#v, %v", post, err)
	}
}

func TestDeletePublicationTreatsMAXNotFoundAsAlreadyDeleted(t *testing.T) {
	t.Parallel()
	notFound := &maxclient.Error{StatusCode: http.StatusNotFound, Code: "message.not.found", Message: "Message not found"}
	fake := &fakeMAX{
		chat: maxclient.ChatInfo{ChatID: "-209", Type: "channel", Status: "active", Title: "Channel"},
		membership: maxclient.Membership{IsAdmin: true, Permissions: []maxclient.Permission{
			maxclient.PermissionReadAllMessages, maxclient.PermissionDelete,
		}},
		deleteErr: notFound,
	}
	application, storage := newTestApp(t, fake)
	ctx := context.Background()
	channel, err := storage.CreateChannel(ctx, store.Channel{
		MAXChatID: "-209", Title: "Channel", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	publishedAt := time.Date(2038, time.April, 6, 11, 0, 0, 0, time.UTC)
	post, err := storage.CreatePost(ctx, store.Post{
		Title: "Already gone", Content: "body", Format: store.FormatMarkdown, Status: store.PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: "mid.already-gone", MAXMessageURL: "https://max.ru/channel/already-gone",
		PublishedAt: &publishedAt,
	})
	if err != nil {
		t.Fatal(err)
	}

	post, err = application.DeletePublication(ctx, "test-owner", post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fake.deleteCalls != 1 || post.Status != store.PostStatusDraft || post.MAXMessageID != "" ||
		post.MAXMessageURL != "" || post.LastError != "" || post.PublishedAt == nil ||
		!post.PublishedAt.Equal(publishedAt) {
		t.Fatalf("idempotent deleted publication = %#v, MAX calls=%d", post, fake.deleteCalls)
	}
}

func TestDeletePublicationReconcilesAmbiguousFailureWhenMessageIsGone(t *testing.T) {
	t.Parallel()
	operationFailed := &maxclient.Error{
		StatusCode: http.StatusOK, Code: "operation_failed", Message: "Error on message delete",
	}
	fake := &fakeMAX{
		chat: maxclient.ChatInfo{ChatID: "-210", Type: "channel", Status: "active", Title: "Channel"},
		membership: maxclient.Membership{IsAdmin: true, Permissions: []maxclient.Permission{
			maxclient.PermissionReadAllMessages, maxclient.PermissionDelete,
		}},
		deleteErr:     operationFailed,
		getMessageErr: &maxclient.Error{StatusCode: http.StatusNotFound, Code: "message.not.found", Message: "Message not found"},
	}
	application, storage := newTestApp(t, fake)
	ctx := context.Background()
	channel, err := storage.CreateChannel(ctx, store.Channel{
		MAXChatID: "-210", Title: "Channel", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(ctx, store.Post{
		Title: "Ambiguous delete", Content: "body", Format: store.FormatMarkdown, Status: store.PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: "mid.ambiguous-delete", MAXMessageURL: "https://max.ru/channel/ambiguous-delete",
	})
	if err != nil {
		t.Fatal(err)
	}

	post, err = application.DeletePublication(ctx, "test-owner", post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fake.deleteCalls != 1 || fake.getMessageCalls != 1 || post.Status != store.PostStatusDraft ||
		post.MAXMessageID != "" || post.MAXMessageURL != "" {
		t.Fatalf("reconciled ambiguous delete = %#v, MAX=%#v", post, fake)
	}
}

func TestDeletePublicationPreservesAmbiguousFailureWhenMessageStillExists(t *testing.T) {
	t.Parallel()
	operationFailed := &maxclient.Error{
		StatusCode: http.StatusOK, Code: "operation_failed", Message: "Error on message delete",
	}
	fake := &fakeMAX{
		chat: maxclient.ChatInfo{ChatID: "-211", Type: "channel", Status: "active", Title: "Channel"},
		membership: maxclient.Membership{IsAdmin: true, Permissions: []maxclient.Permission{
			maxclient.PermissionReadAllMessages, maxclient.PermissionDelete,
		}},
		deleteErr: operationFailed,
		message:   maxclient.Message{MessageID: "mid.delete-still-exists", ChatID: "-211"},
	}
	application, storage := newTestApp(t, fake)
	ctx := context.Background()
	channel, err := storage.CreateChannel(ctx, store.Channel{
		MAXChatID: "-211", Title: "Channel", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(ctx, store.Post{
		Title: "Still exists", Content: "body", Format: store.FormatMarkdown, Status: store.PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: fake.message.MessageID,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := application.DeletePublication(ctx, "test-owner", post.ID); !errors.Is(err, operationFailed) {
		t.Fatalf("delete error = %v, want original operation failure", err)
	}
	stored, err := storage.GetPostForUser(ctx, "test-owner", post.ID)
	if err != nil || stored.Status != store.PostStatusPublished || stored.MAXMessageID != post.MAXMessageID ||
		fake.deleteCalls != 1 || fake.getMessageCalls != 1 {
		t.Fatalf("ambiguous failure changed post = %#v, err=%v, MAX=%#v", stored, err, fake)
	}
}

func TestPinAndUnpinRequireOptionalPinPermission(t *testing.T) {
	t.Parallel()
	fake := &fakeMAX{
		chat: maxclient.ChatInfo{ChatID: "-203", Type: "channel", Status: "active", Title: "Channel"},
		membership: maxclient.Membership{IsAdmin: true, Permissions: []maxclient.Permission{
			maxclient.PermissionReadAllMessages, maxclient.PermissionWrite,
		}},
		pinnedMessage: &maxclient.Message{MessageID: "mid.pin", ChatID: "-203"},
	}
	application, storage := newTestApp(t, fake)
	channel, err := storage.CreateChannel(context.Background(), store.Channel{MAXChatID: "-203", Title: "Channel", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(context.Background(), store.Post{
		Title: "Pin", Content: "body", Format: store.FormatMarkdown, Status: store.PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: "mid.pin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.PinPost(context.Background(), "test-owner", post.ID); err == nil {
		t.Fatal("PinPost accepted missing pin_message permission")
	}
	if fake.pinCalls != 0 {
		t.Fatal("unauthorized pin reached MAX")
	}
	// pin_message remains optional for ordinary connection/publication, but is
	// exposed separately in diagnostics and required by the pin endpoints.
	fake.membership.Permissions = append(fake.membership.Permissions, maxclient.PermissionPinMessage)
	post, err = application.PinPost(context.Background(), "test-owner", post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !post.MAXIsPinned || fake.pinCalls != 1 {
		t.Fatalf("pinned post = %#v, calls=%d", post, fake.pinCalls)
	}
	post, err = application.UnpinPost(context.Background(), "test-owner", post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if post.MAXIsPinned || fake.unpinCalls != 1 || fake.getPinnedCalls != 1 {
		t.Fatalf("unpinned post = %#v, fake=%#v", post, fake)
	}

	// Repeating DELETE after MAX has already removed the pin is successful and
	// does not issue another destructive request.
	fake.pinnedMessage = nil
	post, err = application.UnpinPost(context.Background(), "test-owner", post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if post.MAXIsPinned || fake.unpinCalls != 1 || fake.getPinnedCalls != 2 {
		t.Fatalf("idempotent unpin = %#v, fake=%#v", post, fake)
	}

	fake.pinnedMessage = &maxclient.Message{MessageID: "mid.other", ChatID: "-203"}
	post, err = application.UnpinPost(context.Background(), "test-owner", post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if post.MAXIsPinned {
		t.Fatalf("post remained locally pinned after MAX pinned a different message: %#v", post)
	}
	if fake.unpinCalls != 1 {
		t.Fatal("stale unpin removed another MAX message")
	}
}

func TestStatsWorkerRefreshesDuePostsWithoutMembershipLookup(t *testing.T) {
	t.Parallel()
	views := int64(10)
	now := time.Date(2038, time.April, 5, 6, 7, 8, 0, time.UTC)
	fake := &fakeMAX{
		chat:          maxclient.ChatInfo{ChatID: "-204", Type: "channel", Status: "active", Title: "Channel"},
		message:       maxclient.Message{MessageID: "mid.worker", ChatID: "-204", URL: "https://max.ru/channel/worker", Views: &views},
		pinnedMessage: &maxclient.Message{MessageID: "mid.other", ChatID: "-204"},
	}
	application, storage := newTestApp(t, fake)
	application.now = func() time.Time { return now }
	channel, err := storage.CreateChannel(context.Background(), store.Channel{MAXChatID: "-204", Title: "Channel", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	publishedAt := now.Add(-2 * time.Hour)
	post, err := storage.CreatePost(context.Background(), store.Post{
		Title: "Worker", Content: "body", Format: store.FormatMarkdown, Status: store.PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: "mid.worker", PublishedAt: &publishedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	application.syncDueMAXStats(context.Background(), now)
	post, err = storage.GetPost(context.Background(), post.ID)
	if err != nil || post.MAXViews == nil || *post.MAXViews != views || post.MAXIsPinned {
		t.Fatalf("worker post = %#v, %v", post, err)
	}
	if fake.getMessageCalls != 1 || fake.getPinnedCalls != 1 || fake.memberCalls != 0 {
		t.Fatalf("worker MAX calls = %#v", fake)
	}
	application.syncDueMAXStats(context.Background(), now.Add(30*time.Minute))
	if fake.getMessageCalls != 1 {
		t.Fatal("worker synchronized a post more than once per hour")
	}
}

func TestStatsWorkerRefreshesTeamWorkspacePublication(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	views := int64(42)
	now := time.Date(2038, time.April, 6, 6, 7, 8, 0, time.UTC)
	fake := &fakeMAX{
		chat:    maxclient.ChatInfo{ChatID: "-team-204", OwnerID: "team-max-owner", Type: "channel", Status: "active", Title: "Team"},
		message: maxclient.Message{MessageID: "mid.team.worker", ChatID: "-team-204", URL: "https://max.ru/channel/team-worker", Views: &views},
	}
	application, storage := newTestApp(t, fake)
	application.now = func() time.Time { return now }
	if err := storage.UpsertUser(ctx, store.User{ID: "stats-team-owner", DisplayName: "Team owner"}); err != nil {
		t.Fatal(err)
	}
	workspace, err := storage.CreateWorkspace(ctx, "stats-team-owner", store.Workspace{Name: "Stats team"})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := storage.CreateChannelForWorkspace(ctx, "stats-team-owner", workspace.ID, store.Channel{
		VerifiedMAXOwnerID: "team-max-owner", MAXChatID: fake.chat.ChatID, Title: "Team", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	publishedAt := now.Add(-2 * time.Hour)
	post, err := storage.CreatePostForWorkspace(ctx, "stats-team-owner", workspace.ID, store.Post{
		Title: "Team worker", Content: "body", Format: store.FormatMarkdown, Status: store.PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: fake.message.MessageID, PublishedAt: &publishedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	application.syncDueMAXStats(ctx, now)
	post, err = storage.GetPostForWorkspace(ctx, "stats-team-owner", workspace.ID, post.ID)
	if err != nil || post.MAXViews == nil || *post.MAXViews != views || post.MAXStatsSyncedAt == nil {
		t.Fatalf("team worker post = %#v, %v", post, err)
	}
	if fake.getMessageCalls != 1 || fake.getPinnedCalls != 1 {
		t.Fatalf("team worker MAX calls = %#v", fake)
	}
}

func TestStatsWorkerBacksOffAfterUpstreamFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2038, time.April, 5, 9, 0, 0, 0, time.UTC)
	upstream := &maxclient.Error{StatusCode: http.StatusServiceUnavailable, Code: "temporarily_unavailable", Message: "retry later"}
	fake := &fakeMAX{
		chat:          maxclient.ChatInfo{ChatID: "-205", Type: "channel", Status: "active", Title: "Channel"},
		getMessageErr: upstream,
	}
	application, storage := newTestApp(t, fake)
	application.now = func() time.Time { return now }
	channel, err := storage.CreateChannel(context.Background(), store.Channel{MAXChatID: "-205", Title: "Channel", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	publishedAt := now.Add(-2 * time.Hour)
	post, err := storage.CreatePost(context.Background(), store.Post{
		Title: "Gone", Content: "body", Format: store.FormatMarkdown, Status: store.PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: "mid.gone", PublishedAt: &publishedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	application.syncDueMAXStats(context.Background(), now)
	if fake.getMessageCalls != 1 {
		t.Fatalf("first worker calls = %d", fake.getMessageCalls)
	}
	stored, err := storage.GetPost(context.Background(), post.ID)
	if err != nil || stored.MAXStatsAttemptedAt == nil || !stored.MAXStatsAttemptedAt.Equal(now) ||
		stored.MAXStatsSyncedAt != nil || stored.Status != store.PostStatusPublished {
		t.Fatalf("failed worker post = %#v, %v", stored, err)
	}
	application.syncDueMAXStats(context.Background(), now.Add(time.Minute))
	if fake.getMessageCalls != 1 {
		t.Fatal("failed MAX stats lookup was retried before the one-hour backoff")
	}
}
