package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPublicationChangesSurviveReloadAndResetOnlyAfterConfirmedUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "publication-state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	channel, err := storage.CreateChannel(ctx, Channel{MAXChatID: "fingerprint", Title: "Channel", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(ctx, Post{Title: "Original title", Content: "delivered", Format: FormatMarkdown, ChannelID: &channel.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ClaimForPublishing(ctx, post.ID); err != nil {
		t.Fatal(err)
	}
	post, err = storage.MarkPublished(ctx, post.ID, "fingerprint-mid", "")
	if err != nil || post.PublicationHasChanges || !post.PublicationVersionKnown || len(post.MAXPublishedFingerprint) != 64 {
		t.Fatalf("confirmed initial state = %#v, %v", post, err)
	}
	initialFingerprint := post.MAXPublishedFingerprint
	title := "Internal title only"
	post, err = storage.UpdatePost(ctx, post.ID, PostChanges{Title: &title})
	if err != nil || post.PublicationHasChanges {
		t.Fatalf("internal title marked MAX text as changed: %#v, %v", post, err)
	}
	views := int64(7)
	post, err = storage.SyncPublicationMetadataForUser(ctx, post.UserID, post.ID, channel.ID, post.MAXMessageID,
		"https://max.ru/fingerprint", &views, time.Now().UTC(), true)
	if err != nil || post.PublicationHasChanges {
		t.Fatalf("MAX statistics marked payload as changed: %#v, %v", post, err)
	}
	content := "saved locally"
	if _, err := storage.UpdatePost(ctx, post.ID, PostChanges{Content: &content}); err != nil {
		t.Fatal(err)
	}
	// Reopening from durable storage cannot replace the delivered snapshot with
	// the new local body, as the old in-memory editor snapshot did.
	post, err = storage.GetPost(ctx, post.ID)
	if err != nil || !post.PublicationHasChanges || post.MAXPublishedFingerprint != initialFingerprint {
		t.Fatalf("saved draft disappeared on reload: %#v, %v", post, err)
	}
	claimed, err := storage.ClaimPublishedForUpdate(ctx, post)
	if err != nil {
		t.Fatal(err)
	}
	post, err = storage.ReleasePublishedUpdate(ctx, claimed, "MAX edit failed")
	if err != nil || !post.PublicationHasChanges || post.MAXPublishedFingerprint != initialFingerprint {
		t.Fatalf("failed edit replaced delivered snapshot: %#v, %v", post, err)
	}
	claimed, err = storage.ClaimPublishedForUpdate(ctx, post)
	if err != nil {
		t.Fatal(err)
	}
	post, err = storage.ReleasePublishedUpdate(ctx, claimed, "")
	if err != nil || post.PublicationHasChanges || !post.PublicationVersionKnown || post.MAXPublishedFingerprint == initialFingerprint {
		t.Fatalf("confirmed edit did not replace delivered snapshot: %#v, %v", post, err)
	}
	encoded, err := json.Marshal(post)
	if err != nil || strings.Contains(string(encoded), post.MAXPublishedFingerprint) || !strings.Contains(string(encoded), `"publication_has_changes":false`) ||
		!strings.Contains(string(encoded), `"publication_version_known":true`) {
		t.Fatalf("public JSON leaked snapshot or omitted state: %s, %v", encoded, err)
	}
	// Existing published rows predate the snapshot; never claim their current
	// draft is the version delivered to MAX.
	if _, err := storage.db.ExecContext(ctx, `UPDATE posts SET max_published_fingerprint='' WHERE id=$1`, post.ID); err != nil {
		t.Fatal(err)
	}
	post, err = storage.GetPost(ctx, post.ID)
	if err != nil || !post.PublicationHasChanges || post.PublicationVersionKnown {
		t.Fatalf("unknown legacy snapshot falsely clean: %#v, %v", post, err)
	}
	post, err = storage.ClearPublicationForUser(ctx, post.UserID, post.ID, channel.ID, post.MAXMessageID)
	if err != nil || post.PublicationHasChanges {
		t.Fatalf("removed publication retains dirty flag: %#v, %v", post, err)
	}
}

func TestPublicationFingerprintIncludesGalleryOrderButIgnoresUploadCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "gallery-publication-state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	channel, err := storage.CreateChannel(ctx, Channel{MAXChatID: "gallery-fingerprint", Title: "Gallery", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(ctx, Post{Title: "Gallery", Content: "body", Format: FormatMarkdown, ChannelID: &channel.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"first.png", "second.png"} {
		reservation, err := storage.ReserveMedia(ctx, post.UserID, key, 100,
			MediaLimits{MaxFiles: 20, MaxBytes: 1 << 30}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if err := storage.CompleteMediaReservation(ctx, reservation, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		post, err = storage.AddPostAttachmentForUser(ctx, post.UserID, post.ID, PostAttachment{
			Type: PostAttachmentImage, Position: -1, StorageKey: key, SizeBytes: 100, MIMEType: "image/png",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := storage.ClaimForPublishing(ctx, post.ID); err != nil {
		t.Fatal(err)
	}
	post, err = storage.MarkPublished(ctx, post.ID, "gallery-fingerprint-mid", "")
	if err != nil || post.PublicationHasChanges {
		t.Fatalf("initial gallery state = %#v, %v", post, err)
	}
	initialFingerprint := post.MAXPublishedFingerprint
	attachment := post.Attachments[0]
	if err := storage.CachePostAttachmentProviderToken(ctx, post.UserID, post.ID, attachment.ID,
		attachment.StorageKey, attachment.UpdatedAt, "fresh-upload-cache"); err != nil {
		t.Fatal(err)
	}
	post, err = storage.GetPost(ctx, post.ID)
	if err != nil || post.PublicationHasChanges {
		t.Fatalf("provider cache changed gallery payload: %#v, %v", post, err)
	}
	post, err = storage.ReorderPostAttachmentsForUser(ctx, post.UserID, post.ID, []int64{post.Attachments[1].ID, post.Attachments[0].ID})
	if err != nil || !post.PublicationHasChanges || post.MAXPublishedFingerprint != initialFingerprint {
		t.Fatalf("reordered gallery did not mark delivered order changed: %#v, %v", post, err)
	}
	claimed, err := storage.ClaimPublishedForUpdate(ctx, post)
	if err != nil {
		t.Fatal(err)
	}
	post, err = storage.ReleasePublishedUpdate(ctx, claimed, "")
	if err != nil || post.PublicationHasChanges {
		t.Fatalf("confirmed gallery update did not record new order: %#v, %v", post, err)
	}
	post, err = storage.GetPost(ctx, post.ID)
	if err != nil || post.PublicationHasChanges {
		t.Fatalf("confirmed gallery update lost snapshot on reload: %#v, %v", post, err)
	}
}
