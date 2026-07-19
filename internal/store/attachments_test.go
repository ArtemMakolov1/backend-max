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

func TestPostAttachmentsCRUDOrderTenantIsolationAndDuplicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "attachments.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	const (
		ownerA = "attachment-owner-a"
		ownerB = "attachment-owner-b"
	)
	for _, owner := range []string{ownerA, ownerB} {
		if err := storage.UpsertUser(ctx, User{ID: owner, DisplayName: owner}); err != nil {
			t.Fatal(err)
		}
	}

	readyMedia := func(owner, filename string, size int64) {
		t.Helper()
		reservation, err := storage.ReserveMedia(ctx, owner, filename, size,
			MediaLimits{MaxFiles: 20, MaxBytes: 1 << 30}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if err := storage.CompleteMediaReservation(ctx, reservation, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	for _, media := range []struct {
		key  string
		size int64
	}{
		{key: "first.png", size: 101},
		{key: "clip.mp4", size: 202},
		{key: "middle.jpg", size: 303},
		{key: "replacement.png", size: 404},
	} {
		readyMedia(ownerA, media.key, media.size)
	}
	// Owning an object is tenant-scoped even when another tenant knows its key.
	readyMedia(ownerB, "first.png", 101)

	post, err := storage.CreatePost(ctx, Post{
		UserID: ownerA, Title: "Gallery", Content: "body", Format: FormatMarkdown, Status: PostStatusDraft,
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err = storage.AddPostAttachmentForUser(ctx, ownerA, post.ID, PostAttachment{
		Type: PostAttachmentImage, Position: -1, StorageKey: "first.png", SizeBytes: 101, MIMEType: "image/png",
	})
	if err != nil {
		t.Fatal(err)
	}
	firstID := post.Attachments[0].ID
	post, err = storage.AddPostAttachmentForUser(ctx, ownerA, post.ID, PostAttachment{
		Type: PostAttachmentVideo, Position: -1, StorageKey: "clip.mp4", SizeBytes: 202, MIMEType: "video/mp4",
	})
	if err != nil {
		t.Fatal(err)
	}
	videoID := post.Attachments[1].ID
	post, err = storage.AddPostAttachmentForUser(ctx, ownerA, post.ID, PostAttachment{
		Type: PostAttachmentImage, Position: 1, StorageKey: "middle.jpg", SizeBytes: 303, MIMEType: "image/jpeg",
	})
	if err != nil {
		t.Fatal(err)
	}
	middleID := post.Attachments[1].ID
	assertAttachmentOrder(t, post.Attachments, []string{"first.png", "middle.jpg", "clip.mp4"})
	if post.ImagePath != "first.png" || post.ImageURL != "/media/first.png" {
		t.Fatalf("legacy image projection=(%q,%q), want first image", post.ImagePath, post.ImageURL)
	}

	post, err = storage.ReorderPostAttachmentsForUser(ctx, ownerA, post.ID, []int64{middleID, videoID, firstID})
	if err != nil {
		t.Fatal(err)
	}
	assertAttachmentOrder(t, post.Attachments, []string{"middle.jpg", "clip.mp4", "first.png"})
	if post.ImagePath != "middle.jpg" || post.ImageURL != "/media/middle.jpg" {
		t.Fatalf("projection after reorder=(%q,%q), want middle image", post.ImagePath, post.ImageURL)
	}
	if _, err := storage.ReorderPostAttachmentsForUser(ctx, ownerA, post.ID, []int64{middleID, middleID, firstID}); err == nil {
		t.Fatal("duplicate attachment ids in reorder were accepted")
	}

	post, err = storage.ReplacePostAttachmentForUser(ctx, ownerA, post.ID, middleID, PostAttachment{
		Type: PostAttachmentImage, StorageKey: "replacement.png", SizeBytes: 404, MIMEType: "image/png",
	})
	if err != nil {
		t.Fatal(err)
	}
	if post.Attachments[0].ID != middleID {
		t.Fatalf("replacement changed stable attachment id: got %d want %d", post.Attachments[0].ID, middleID)
	}
	assertAttachmentOrder(t, post.Attachments, []string{"replacement.png", "clip.mp4", "first.png"})

	post, err = storage.DeletePostAttachmentForUser(ctx, ownerA, post.ID, videoID)
	if err != nil {
		t.Fatal(err)
	}
	assertAttachmentOrder(t, post.Attachments, []string{"replacement.png", "first.png"})
	for index, attachment := range post.Attachments {
		if attachment.Position != index {
			t.Fatalf("position[%d]=%d, want compact order", index, attachment.Position)
		}
	}

	if _, err := storage.ListPostAttachmentsForUser(ctx, ownerB, post.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant list error=%v, want ErrNotFound", err)
	}
	if _, err := storage.ReplacePostAttachmentForUser(ctx, ownerB, post.ID, middleID, PostAttachment{
		Type: PostAttachmentImage, StorageKey: "first.png", SizeBytes: 101, MIMEType: "image/png",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant replace error=%v, want ErrNotFound", err)
	}
	if _, err := storage.DeletePostAttachmentForUser(ctx, ownerB, post.ID, firstID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant delete error=%v, want ErrNotFound", err)
	}

	copyPost, err := storage.DuplicatePostForUser(ctx, ownerA, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertAttachmentOrder(t, copyPost.Attachments, []string{"replacement.png", "first.png"})
	for index := range post.Attachments {
		if copyPost.Attachments[index].ID == post.Attachments[index].ID {
			t.Fatalf("duplicate reused attachment row id %d", post.Attachments[index].ID)
		}
		if copyPost.Attachments[index].StorageKey != post.Attachments[index].StorageKey {
			t.Fatalf("duplicate storage key[%d]=%q, want %q", index,
				copyPost.Attachments[index].StorageKey, post.Attachments[index].StorageKey)
		}
	}
	var assets, bytes int64
	if err := storage.db.QueryRowContext(ctx, `SELECT asset_count, total_bytes FROM media_usage WHERE owner_id=$1`, ownerA).
		Scan(&assets, &bytes); err != nil {
		t.Fatal(err)
	}
	if assets != 4 || bytes != 101+202+303+404 {
		t.Fatalf("duplicate changed media quota=(%d,%d), want object quota (4,%d)", assets, bytes, 101+202+303+404)
	}
}

func TestPostAttachmentJSONHidesStorageAndProviderSecrets(t *testing.T) {
	attachment := PostAttachment{
		ID: 17, OwnerID: "owner-secret", PostID: 42, Type: PostAttachmentVideo, Position: 1,
		URL: "/media/public.mp4", StorageKey: "private-storage-key.mp4", ProcessingStatus: AttachmentStatusReady,
		SizeBytes: 1024, MIMEType: "video/mp4", ProviderToken: "provider-secret-token",
		ProviderMeta: json.RawMessage(`{"provider":"secret"}`), ErrorCode: "internal-only",
	}
	encoded, err := json.Marshal(attachment)
	if err != nil {
		t.Fatal(err)
	}
	value := string(encoded)
	for _, secret := range []string{"owner-secret", "private-storage-key", "provider-secret-token", "provider\"", "internal-only", "post_id"} {
		if strings.Contains(value, secret) {
			t.Fatalf("public attachment JSON leaked %q: %s", secret, value)
		}
	}
	if !strings.Contains(value, `"url":"/media/public.mp4"`) || !strings.Contains(value, `"type":"video"`) {
		t.Fatalf("public attachment JSON lost safe fields: %s", value)
	}
}

func TestReplaceFirstImageAttachmentAndPromptIfUnchangedIsAtomic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "atomic-image.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	const owner = "atomic-image-owner"
	if err := storage.UpsertUser(ctx, User{ID: owner, DisplayName: owner}); err != nil {
		t.Fatal(err)
	}
	readyMedia := func(filename string, size int64) {
		t.Helper()
		reservation, reserveErr := storage.ReserveMedia(ctx, owner, filename, size,
			MediaLimits{MaxFiles: 20, MaxBytes: 1 << 30}, time.Now().UTC())
		if reserveErr != nil {
			t.Fatal(reserveErr)
		}
		if completeErr := storage.CompleteMediaReservation(ctx, reservation, time.Now().UTC()); completeErr != nil {
			t.Fatal(completeErr)
		}
	}
	readyMedia("clip.mp4", 100)
	readyMedia("generated.png", 200)
	readyMedia("replacement.png", 300)
	readyMedia("stale.png", 400)

	post, err := storage.CreatePost(ctx, Post{
		UserID: owner, Title: "Atomic", Content: "before", Format: FormatMarkdown, Status: PostStatusDraft,
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err = storage.AddPostAttachmentForUser(ctx, owner, post.ID, PostAttachment{
		Type: PostAttachmentVideo, Position: -1, StorageKey: "clip.mp4", SizeBytes: 100, MIMEType: "video/mp4",
	})
	if err != nil {
		t.Fatal(err)
	}
	videoID := post.Attachments[0].ID

	post, err = storage.ReplaceFirstImageAttachmentAndPromptIfUnchanged(ctx, post, PostAttachment{
		Type: PostAttachmentImage, StorageKey: "generated.png", SizeBytes: 200, MIMEType: "image/png",
	}, "generated prompt")
	if err != nil {
		t.Fatal(err)
	}
	assertAttachmentOrder(t, post.Attachments, []string{"generated.png", "clip.mp4"})
	if post.Attachments[1].ID != videoID {
		t.Fatalf("inserting the first image changed the existing video id: got %d want %d", post.Attachments[1].ID, videoID)
	}
	if post.ImagePrompt != "generated prompt" || post.ImagePath != "generated.png" {
		t.Fatalf("atomic image fields=(%q,%q), want generated values", post.ImagePrompt, post.ImagePath)
	}
	imageID := post.Attachments[0].ID

	post, err = storage.ReplaceFirstImageAttachmentAndPromptIfUnchanged(ctx, post, PostAttachment{
		Type: PostAttachmentImage, StorageKey: "replacement.png", SizeBytes: 300, MIMEType: "image/png",
	}, "replacement prompt")
	if err != nil {
		t.Fatal(err)
	}
	if post.Attachments[0].ID != imageID {
		t.Fatalf("replacing the first image changed its stable id: got %d want %d", post.Attachments[0].ID, imageID)
	}
	assertAttachmentOrder(t, post.Attachments, []string{"replacement.png", "clip.mp4"})
	if post.ImagePrompt != "replacement prompt" || post.ImagePath != "replacement.png" {
		t.Fatalf("replacement fields=(%q,%q), want replacement values", post.ImagePrompt, post.ImagePath)
	}

	stale := post
	updatedContent := "concurrent autosave"
	post, err = storage.UpdatePostIfUnchanged(ctx, post, PostChanges{Content: &updatedContent})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ReplaceFirstImageAttachmentAndPromptIfUnchanged(ctx, stale, PostAttachment{
		Type: PostAttachmentImage, StorageKey: "stale.png", SizeBytes: 400, MIMEType: "image/png",
	}, "stale prompt"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale atomic replacement error=%v, want ErrConflict", err)
	}

	got, err := storage.GetPostForUser(ctx, owner, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertAttachmentOrder(t, got.Attachments, []string{"replacement.png", "clip.mp4"})
	if got.Content != updatedContent || got.ImagePrompt != "replacement prompt" || got.ImagePath != "replacement.png" {
		t.Fatalf("stale operation partially changed post: content=%q prompt=%q image=%q",
			got.Content, got.ImagePrompt, got.ImagePath)
	}
}

func assertAttachmentOrder(t *testing.T, attachments []PostAttachment, want []string) {
	t.Helper()
	if len(attachments) != len(want) {
		t.Fatalf("attachments=%#v, want %d entries", attachments, len(want))
	}
	for index, key := range want {
		if attachments[index].StorageKey != key || attachments[index].Position != index {
			t.Fatalf("attachment[%d]=(%q, position %d), want (%q, position %d)", index,
				attachments[index].StorageKey, attachments[index].Position, key, index)
		}
	}
}
