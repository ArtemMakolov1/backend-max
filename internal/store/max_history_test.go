package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMAXHistoryImportPagesRemoteMediaIncrementalOverlapAndCopy(t *testing.T) {
	ctx := context.Background()
	storage, workspace, channel := openMAXHistoryStoreTest(t, "max-history-pages", 4)
	now := time.Now().UTC().Truncate(time.Microsecond)
	lease := 2 * time.Minute

	localPublishedAt := now.Add(-time.Minute)
	local, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{
		Title: "Already local", Content: "existing", Status: PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: "local-message", PublishedAt: &localPublishedAt,
	})
	if err != nil {
		t.Fatal(err)
	}

	claim, err := storage.ClaimMAXHistoryImport(ctx, "history-editor", workspace.ID, channel.ID, now, lease)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Status != MAXHistoryImportStatusInProgress || claim.Generation <= 0 || claim.RunID <= 0 ||
		claim.Channel.ID != channel.ID || claim.ExpectedCount != 4 || claim.ClaimedAt == nil {
		t.Fatalf("initial claim = %#v", claim)
	}
	if _, err := storage.ClaimMAXHistoryImport(
		ctx, "test-owner", workspace.ID, channel.ID, now.Add(time.Second), lease,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("active lease claim error = %v, want ErrConflict", err)
	}

	views := int64(42)
	sender := true
	unsupportedSender := false
	supportedAt := now.Add(-2 * time.Minute)
	unsupportedAt := now.Add(-3 * time.Minute)
	supported := MAXHistoryItem{
		Title: "Imported with media", Content: "MAX body", URL: "https://max.ru/channel/message-1",
		MessageID: "history-message-1", PublishedAt: supportedAt, Views: &views,
		SenderIsBot: &sender, RoundTrip: true,
		LinkButtons: []LinkButton{{Text: "Подробнее", URL: "https://example.com/details"}},
		Raw:         json.RawMessage(`{"message_id":"history-message-1","secret":"raw-only"}`),
		Attachments: []MAXHistoryAttachment{{
			Type: PostAttachmentImage, ProviderToken: "provider-secret-token",
			RemoteURL: "https://cdn.max.ru/history/image.jpg", Width: maxHistoryInt(1280),
			Height: maxHistoryInt(720), ProviderMeta: json.RawMessage(`{"provider":"max"}`),
		}},
	}
	unsupported := MAXHistoryItem{
		Title: "Unsupported", Content: "Reference only", URL: "https://max.ru/channel/message-2",
		MessageID: "history-message-2", PublishedAt: unsupportedAt,
		SenderIsBot: &unsupportedSender, RoundTrip: false,
		Raw: json.RawMessage(`{"message_id":"history-message-2","unsupported":true}`),
	}
	firstCursor := unsupportedAt.UnixMilli()
	progress, err := storage.ApplyMAXHistoryPage(ctx, "history-editor", workspace.ID, claim.Generation,
		[]MAXHistoryItem{
			{Title: "Collision", Content: "provider copy", MessageID: local.MAXMessageID,
				PublishedAt: localPublishedAt, RoundTrip: true, Raw: json.RawMessage(`{"collision":true}`)},
			supported,
			unsupported,
		}, &firstCursor, false, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if progress.Status != MAXHistoryImportStatusInProgress || progress.ClaimedAt != nil ||
		progress.ProcessedCount != 3 || progress.ImportedCount != 2 || progress.ExistingCount != 1 ||
		progress.SkippedCount != 0 || progress.CursorFrom == nil || *progress.CursorFrom != firstCursor {
		t.Fatalf("first page progress = %#v", progress)
	}
	if err := storage.ReleaseMAXHistoryImport(ctx, "history-editor", workspace.ID, claim.Generation,
		"uncertain_after_commit", now.Add(3*time.Second)); err != nil {
		t.Fatalf("idempotent release after applied page: %v", err)
	}

	imported := maxHistoryPostByMessageID(t, storage, workspace.ID, supported.MessageID)
	if imported.Origin != PostOriginMAXHistory || !imported.MAXHistoryAttachmentsComplete ||
		imported.MAXSenderIsBot == nil || !*imported.MAXSenderIsBot || imported.Status != PostStatusPublished {
		t.Fatalf("supported imported post = %#v", imported)
	}
	if !imported.CreatedAt.Equal(supportedAt) || imported.PublishedAt == nil || !imported.PublishedAt.Equal(supportedAt) {
		t.Fatalf("imported source timestamps: created=%v published=%v want=%v",
			imported.CreatedAt, imported.PublishedAt, supportedAt)
	}
	if len(imported.Attachments) != 1 || imported.Attachments[0].Source != PostAttachmentSourceMAXHistory ||
		imported.Attachments[0].URL != supported.Attachments[0].RemoteURL || imported.Attachments[0].StorageKey != "" ||
		imported.Attachments[0].ProviderToken != supported.Attachments[0].ProviderToken {
		t.Fatalf("remote attachment projection = %#v", imported.Attachments)
	}
	publicJSON, err := json.Marshal(imported)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(publicJSON), "provider-secret-token") || strings.Contains(string(publicJSON), "raw-only") ||
		!strings.Contains(string(publicJSON), supported.Attachments[0].RemoteURL) {
		t.Fatalf("unsafe or incomplete public JSON: %s", publicJSON)
	}

	newTitle := "Edited safe imported text"
	imported, err = storage.UpdatePostForWorkspaceIfUnchanged(ctx, "history-editor", workspace.ID, imported,
		PostChanges{Title: &newTitle})
	if err != nil || imported.Title != newTitle {
		t.Fatalf("safe imported text edit = %#v, %v", imported, err)
	}
	if _, err := storage.AddPostAttachmentForUser(ctx, imported.UserID, imported.ID, PostAttachment{
		Type: PostAttachmentImage, StorageKey: "replacement.png", MIMEType: "image/png",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("remote attachment mutation error = %v, want ErrConflict", err)
	}
	unsafe := maxHistoryPostByMessageID(t, storage, workspace.ID, unsupported.MessageID)
	if unsafe.MAXHistoryAttachmentsComplete || unsafe.MAXSenderIsBot == nil || *unsafe.MAXSenderIsBot {
		t.Fatalf("unsupported imported safety flags = %#v", unsafe)
	}
	unsafeTitle := "must not save"
	if _, err := storage.UpdatePostForWorkspaceIfUnchanged(ctx, "history-editor", workspace.ID, unsafe,
		PostChanges{Title: &unsafeTitle}); !errors.Is(err, ErrConflict) {
		t.Fatalf("unsafe imported text edit error = %v, want ErrConflict", err)
	}

	resumed, err := storage.ClaimMAXHistoryImport(
		ctx, "history-editor", workspace.ID, channel.ID, now.Add(4*time.Second), lease,
	)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Generation == claim.Generation || resumed.RunID != claim.RunID || resumed.CursorFrom == nil ||
		*resumed.CursorFrom != firstCursor || resumed.ProcessedCount != 3 {
		t.Fatalf("page-boundary resume = %#v", resumed)
	}
	if err := storage.ReleaseMAXHistoryImport(ctx, "history-editor", workspace.ID, resumed.Generation,
		"provider_unavailable", now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	retry, err := storage.ClaimMAXHistoryImport(
		ctx, "history-editor", workspace.ID, channel.ID, now.Add(6*time.Second), lease,
	)
	if err != nil {
		t.Fatal(err)
	}
	if retry.RunID != claim.RunID || retry.Generation == resumed.Generation || retry.ProcessedCount != 3 {
		t.Fatalf("failed import resume = %#v", retry)
	}
	if _, err := storage.ApplyMAXHistoryPage(ctx, "history-editor", workspace.ID, resumed.Generation,
		nil, nil, true, now.Add(7*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale generation apply error = %v, want ErrConflict", err)
	}
	if err := storage.ReleaseMAXHistoryImport(ctx, "history-editor", workspace.ID, resumed.Generation,
		"stale_worker", now.Add(7*time.Second)); err != nil {
		t.Fatalf("stale release must be a no-op: %v", err)
	}

	olderAt := now.Add(-4 * time.Minute)
	older := MAXHistoryItem{
		Title: "Older", Content: "older body", URL: "https://max.ru/channel/message-3",
		MessageID: "history-message-3", PublishedAt: olderAt, SenderIsBot: &sender,
		RoundTrip: true, Raw: json.RawMessage(`{"message_id":"history-message-3"}`),
	}
	finished, err := storage.ApplyMAXHistoryPage(ctx, "history-editor", workspace.ID, retry.Generation,
		[]MAXHistoryItem{supported, older}, nil, true, now.Add(8*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != MAXHistoryImportStatusComplete || finished.ProcessedCount != 4 ||
		finished.ImportedCount != 3 || finished.ExistingCount != 1 || finished.CursorFrom != nil ||
		finished.CompletedAt == nil {
		t.Fatalf("completed import = %#v", finished)
	}
	if err := storage.ReleaseMAXHistoryImport(ctx, "history-editor", workspace.ID, retry.Generation,
		"late_cleanup", now.Add(9*time.Second)); err != nil {
		t.Fatalf("release after completion must be idempotent: %v", err)
	}

	duplicate, err := storage.DuplicatePostForWorkspace(ctx, "history-editor", workspace.ID, imported.ID)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.Origin != PostOriginMAXPosty || !duplicate.MAXHistoryAttachmentsComplete ||
		duplicate.MAXSenderIsBot != nil || len(duplicate.Attachments) != 0 ||
		duplicate.ImageURL != "" || duplicate.ImagePath != "" || duplicate.MAXMessageID != "" {
		t.Fatalf("editable imported copy = %#v", duplicate)
	}

	incremental, err := storage.ClaimMAXHistoryImport(
		ctx, "history-editor", workspace.ID, channel.ID, now.Add(10*time.Second), lease,
	)
	if err != nil {
		t.Fatal(err)
	}
	if incremental.RunID == finished.RunID || incremental.ProcessedCount != 0 ||
		incremental.PreviousCompletedAt == nil || incremental.CursorFrom != nil {
		t.Fatalf("new incremental run = %#v", incremental)
	}
	newAt := now.Add(time.Minute)
	newer := MAXHistoryItem{
		Title: "New since last sync", Content: "delta", URL: "https://max.ru/channel/message-4",
		MessageID: "history-message-4", PublishedAt: newAt, SenderIsBot: &sender,
		RoundTrip: true, Raw: json.RawMessage(`{"message_id":"history-message-4"}`),
	}
	incrementalCursor := supportedAt.UnixMilli()
	incrementalDone, err := storage.ApplyMAXHistoryPage(ctx, "history-editor", workspace.ID, incremental.Generation,
		[]MAXHistoryItem{newer, supported}, &incrementalCursor, false, now.Add(11*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if incrementalDone.Status != MAXHistoryImportStatusComplete || incrementalDone.ProcessedCount != 2 ||
		incrementalDone.ImportedCount != 1 || incrementalDone.ExistingCount != 1 ||
		incrementalDone.ExpectedCount != 2 || incrementalDone.CursorFrom != nil {
		t.Fatalf("incremental overlap completion = %#v", incrementalDone)
	}

	var bulkAuditCount int
	if err := storage.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events
WHERE workspace_id=? AND action='max.history.page_imported'`, workspace.ID).Scan(&bulkAuditCount); err != nil {
		t.Fatal(err)
	}
	if bulkAuditCount != 3 {
		t.Fatalf("bulk page audit count = %d, want 3", bulkAuditCount)
	}

	if _, err := storage.db.ExecContext(ctx, `DELETE FROM posts WHERE workspace_id=? AND id=?`, workspace.ID, imported.ID); err != nil {
		t.Fatal(err)
	}
	var mappings, remoteAttachments int
	if err := storage.db.QueryRowContext(ctx, `SELECT
(SELECT count(*) FROM max_history_messages WHERE post_id=?),
(SELECT count(*) FROM max_history_post_attachments WHERE post_id=?)`, imported.ID, imported.ID).Scan(
		&mappings, &remoteAttachments,
	); err != nil {
		t.Fatal(err)
	}
	if mappings != 0 || remoteAttachments != 0 {
		t.Fatalf("post cascade left mappings=%d remote_attachments=%d", mappings, remoteAttachments)
	}
}

func TestMAXHistoryImportLeaseFencingAndRBAC(t *testing.T) {
	ctx := context.Background()
	storage, workspace, channel := openMAXHistoryStoreTest(t, "max-history-fencing", 0)
	now := time.Now().UTC().Truncate(time.Microsecond)
	lease := time.Minute

	if _, err := storage.ClaimMAXHistoryImport(ctx, "history-viewer", workspace.ID, channel.ID, now, lease); !errors.Is(err, ErrNotFound) {
		t.Fatalf("viewer claim error = %v, want ErrNotFound", err)
	}
	if _, err := storage.ClaimMAXHistoryImport(ctx, "foreign-user", workspace.ID, channel.ID, now, lease); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign claim error = %v, want ErrNotFound", err)
	}
	first, err := storage.ClaimMAXHistoryImport(ctx, "test-owner", workspace.ID, channel.ID, now, lease)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ClaimMAXHistoryImport(
		ctx, "history-editor", workspace.ID, channel.ID, now.Add(lease-time.Microsecond), lease,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("unexpired claim error = %v, want ErrConflict", err)
	}
	reclaimed, err := storage.ClaimMAXHistoryImport(
		ctx, "history-editor", workspace.ID, channel.ID, now.Add(lease), lease,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.Generation == first.Generation || reclaimed.RunID != first.RunID {
		t.Fatalf("expired lease reclaim = %#v, first=%#v", reclaimed, first)
	}
	if _, err := storage.ApplyMAXHistoryPage(ctx, "test-owner", workspace.ID, first.Generation,
		nil, nil, true, now.Add(lease+time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("fenced apply error = %v, want ErrConflict", err)
	}
	if err := storage.ReleaseMAXHistoryImport(ctx, "test-owner", workspace.ID, first.Generation,
		"stale", now.Add(lease+time.Second)); err != nil {
		t.Fatalf("fenced release = %v", err)
	}
	if err := storage.ReleaseMAXHistoryImport(ctx, "history-editor", workspace.ID, reclaimed.Generation,
		"provider_failed", now.Add(lease+2*time.Second)); err != nil {
		t.Fatal(err)
	}
	resumed, err := storage.ClaimMAXHistoryImport(
		ctx, "history-editor", workspace.ID, channel.ID, now.Add(lease+3*time.Second), lease,
	)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.RunID != first.RunID || resumed.Generation == reclaimed.Generation {
		t.Fatalf("failed lease resume = %#v", resumed)
	}
	complete, err := storage.ApplyMAXHistoryPage(ctx, "history-editor", workspace.ID, resumed.Generation,
		nil, nil, true, now.Add(lease+4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if complete.Status != MAXHistoryImportStatusComplete || complete.ProcessedCount != 0 {
		t.Fatalf("empty completion = %#v", complete)
	}
	restarted, err := storage.ClaimMAXHistoryImport(
		ctx, "test-owner", workspace.ID, channel.ID, now.Add(lease+5*time.Second), lease,
	)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.RunID == complete.RunID || restarted.Generation == complete.Generation ||
		restarted.PreviousCompletedAt == nil {
		t.Fatalf("completed run restart = %#v", restarted)
	}
}

func TestMAXHistoryImportWaitsForActivePublication(t *testing.T) {
	ctx := context.Background()
	storage, workspace, channel := openMAXHistoryStoreTest(t, "max-history-publishing", 1)
	now := time.Now().UTC().Truncate(time.Microsecond)

	post, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{
		Title: "Concurrent publication", Content: "Publishing now", Status: PostStatusDraft,
		ChannelID: &channel.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.ExecContext(ctx, `UPDATE posts SET status=? WHERE id=?`, PostStatusPublishing, post.ID); err != nil {
		t.Fatal(err)
	}
	claim, err := storage.ClaimMAXHistoryImport(ctx, "history-editor", workspace.ID, channel.ID, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	item := MAXHistoryItem{
		Title: "Just published", Content: "Provider copy", MessageID: "mid.concurrent",
		PublishedAt: now, RoundTrip: true, Raw: json.RawMessage(`{}`),
	}
	if _, err := storage.ApplyMAXHistoryPage(
		ctx, "history-editor", workspace.ID, claim.Generation, []MAXHistoryItem{item}, nil, true, now.Add(time.Second),
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("active publication apply error = %v, want ErrConflict", err)
	}
	var mappings int
	if err := storage.db.QueryRowContext(ctx, `SELECT count(*) FROM max_history_messages WHERE workspace_id=?`, workspace.ID).Scan(&mappings); err != nil {
		t.Fatal(err)
	}
	if mappings != 0 {
		t.Fatalf("history import raced an active publication and created %d mappings", mappings)
	}
}

func openMAXHistoryStoreTest(t *testing.T, name string, messagesCount int) (*Store, Workspace, Channel) {
	t.Helper()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), name+".db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "MAX history " + name})
	if err != nil {
		t.Fatal(err)
	}
	billingNow := time.Now().UTC().Truncate(time.Microsecond)
	seedBillingContract(t, storage, workspace.ID, "pro",
		billingNow.AddDate(0, -1, 0), billingNow.AddDate(0, 1, 0), "max-history-test-method")
	for _, member := range []struct {
		id   string
		role string
	}{
		{id: "history-editor", role: WorkspaceRoleEditor},
		{id: "history-viewer", role: WorkspaceRoleViewer},
		{id: "foreign-user", role: ""},
	} {
		if err := storage.UpsertUser(ctx, User{ID: member.id, DisplayName: member.id}); err != nil {
			t.Fatal(err)
		}
		if member.role != "" {
			if _, err := storage.AddWorkspaceMember(ctx, "test-owner", WorkspaceMember{
				WorkspaceID: workspace.ID, UserID: member.id, Role: member.role,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	channel, err := storage.CreateChannelForWorkspace(ctx, "test-owner", workspace.ID, Channel{
		MAXChatID: "-990001", VerifiedMAXOwnerID: "max-history-owner", Title: "History channel",
		MessagesCount: messagesCount, Active: true, IsChannel: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return storage, workspace, channel
}

func maxHistoryPostByMessageID(t *testing.T, storage *Store, workspaceID, messageID string) Post {
	t.Helper()
	var postID int64
	if err := storage.db.QueryRowContext(context.Background(), `SELECT post_id FROM max_history_messages
WHERE workspace_id=? AND max_message_id=?`, workspaceID, messageID).Scan(&postID); err != nil {
		t.Fatal(err)
	}
	post, err := storage.GetPostForWorkspace(context.Background(), "test-owner", workspaceID, postID)
	if err != nil {
		t.Fatal(err)
	}
	return post
}

func maxHistoryInt(value int) *int { return &value }
