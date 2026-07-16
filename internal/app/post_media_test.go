package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"strings"
	"testing"
	"time"

	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

func TestValidatePostAttachmentsHonorsMAXKeyboardLimit(t *testing.T) {
	attachments := make([]store.PostAttachment, store.MaxPostAttachments)
	for index := range attachments {
		attachments[index] = store.PostAttachment{
			ID:               int64(index + 1),
			Type:             store.PostAttachmentImage,
			Position:         index,
			StorageKey:       "image.png",
			ProcessingStatus: store.AttachmentStatusReady,
		}
	}
	if err := validatePostAttachments(store.Post{Attachments: attachments}); err != nil {
		t.Fatalf("validate image-only gallery: %v", err)
	}

	post := store.Post{
		Attachments: attachments,
		LinkButtons: []store.LinkButton{{Text: "Открыть", URL: "https://example.com"}},
	}
	if err := validatePostAttachments(post); err == nil || !strings.Contains(err.Error(), "11") {
		t.Fatalf("validate gallery with keyboard = %v, want eleven-media limit", err)
	}
}

func TestValidatePostAttachmentsBlocksUnreadyAndBrokenOrder(t *testing.T) {
	post := store.Post{Attachments: []store.PostAttachment{{
		ID: 7, Type: store.PostAttachmentVideo, Position: 0,
		StorageKey: "video.mp4", ProcessingStatus: store.AttachmentStatusProcessing,
	}}}
	if err := validatePostAttachments(post); err == nil || !strings.Contains(err.Error(), "processing") {
		t.Fatalf("validate processing attachment = %v", err)
	}

	post.Attachments[0].ProcessingStatus = store.AttachmentStatusReady
	post.Attachments[0].Position = 2
	if err := validatePostAttachments(post); err == nil || !strings.Contains(err.Error(), "order") {
		t.Fatalf("validate broken order = %v", err)
	}
}

func TestPublishedPostReusesCachedProviderToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := &fakeMAX{
		chat:           maxclient.ChatInfo{ChatID: "media-cache", Type: "channel", Status: "active", Title: "Media cache"},
		message:        maxclient.Message{MessageID: "mid-media-cache", ChatID: "media-cache"},
		publishMessage: maxclient.Message{MessageID: "mid-media-cache", URL: "https://max.ru/channel/media-cache"},
		membership: maxclient.Membership{IsAdmin: true, Permissions: []maxclient.Permission{
			maxclient.PermissionReadAllMessages, maxclient.PermissionWrite, maxclient.PermissionEdit,
		}},
	}
	application, storage := newTestApp(t, fake)
	channel, err := storage.CreateChannel(ctx, store.Channel{
		MAXChatID: "media-cache", Title: "Media cache", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(ctx, store.Post{
		Title: "Cached media", Content: "first", Format: store.FormatMarkdown, ChannelID: &channel.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	post = savePostAttachmentImage(t, application, post, "cached.png")
	post, err = application.PublishPost(ctx, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fake.uploadCalls != 1 || len(fake.lastPublishRequest.ImageTokens) != 1 {
		t.Fatalf("publish upload count=%d request=%#v", fake.uploadCalls, fake.lastPublishRequest)
	}
	if len(post.Attachments) != 1 || post.Attachments[0].ProviderToken != "image-token" {
		t.Fatalf("published attachment cache=%#v", post.Attachments)
	}

	updatedContent := "text-only edit"
	if _, err := storage.UpdatePost(ctx, post.ID, store.PostChanges{Content: &updatedContent}); err != nil {
		t.Fatal(err)
	}
	post, err = application.UpdatePublishedPost(ctx, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fake.uploadCalls != 1 {
		t.Fatalf("unchanged media was uploaded %d times, want once", fake.uploadCalls)
	}
	if fake.editCalls != 1 || len(fake.lastEditRequest.ImageTokens) != 1 || fake.lastEditRequest.ImageTokens[0] != "image-token" {
		t.Fatalf("edit request=%#v fake=%#v", fake.lastEditRequest, fake)
	}
	if post.Status != store.PostStatusPublished || len(post.Attachments) != 1 || post.Attachments[0].ProviderToken != "image-token" {
		t.Fatalf("updated post=%#v", post)
	}
}

func TestPublishedPostRejectsAttachmentReplacementDuringUpload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	uploadStarted := make(chan struct{})
	releaseUpload := make(chan struct{})
	fake := &fakeMAX{
		chat:    maxclient.ChatInfo{ChatID: "media-race", Type: "channel", Status: "active", Title: "Media race"},
		message: maxclient.Message{MessageID: "mid-media-race", ChatID: "media-race"},
		membership: maxclient.Membership{IsAdmin: true, Permissions: []maxclient.Permission{
			maxclient.PermissionReadAllMessages, maxclient.PermissionEdit,
		}},
	}
	fake.uploadImageFn = func(context.Context, string, io.Reader) (maxclient.UploadResult, error) {
		close(uploadStarted)
		<-releaseUpload
		return maxclient.UploadResult{Token: "stale-upload-token"}, nil
	}
	application, storage := newTestApp(t, fake)
	channel, err := storage.CreateChannel(ctx, store.Channel{
		MAXChatID: "media-race", Title: "Media race", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(ctx, store.Post{
		Title: "Media race", Content: "body", Format: store.FormatMarkdown, ChannelID: &channel.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	post = savePostAttachmentImage(t, application, post, "before.png")
	post, err = storage.ClaimForPublishing(ctx, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	post, err = storage.MarkPublished(ctx, post.ID, "mid-media-race", "https://max.ru/channel/media-race")
	if err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		post store.Post
		err  error
	}
	finished := make(chan outcome, 1)
	go func() {
		updated, updateErr := application.UpdatePublishedPost(ctx, post.ID)
		finished <- outcome{post: updated, err: updateErr}
	}()
	select {
	case <-uploadStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("MAX upload did not start")
	}

	replacement := saveMediaImage(t, application, post.UserID, "after.png")
	post, err = storage.ReplacePostAttachmentForUser(ctx, post.UserID, post.ID, post.Attachments[0].ID, attachmentFromImage(replacement))
	if err != nil {
		t.Fatal(err)
	}
	close(releaseUpload)
	select {
	case result := <-finished:
		if !errors.Is(result.err, store.ErrConflict) {
			t.Fatalf("concurrent replacement update error=%v, want conflict", result.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("MAX update did not finish")
	}
	if fake.editCalls != 0 {
		t.Fatalf("MAX edit calls=%d, want none for stale attachment", fake.editCalls)
	}
	post, err = storage.GetPost(ctx, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(post.Attachments) != 1 || post.Attachments[0].StorageKey != replacement.Path || post.Attachments[0].ProviderToken != "" {
		t.Fatalf("replacement was corrupted by stale upload: %#v", post.Attachments)
	}

	fake.uploadImageFn = nil
	post, err = application.UpdatePublishedPost(ctx, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fake.uploadCalls != 2 || fake.editCalls != 1 || len(post.Attachments) != 1 || post.Attachments[0].ProviderToken != "image-token" {
		t.Fatalf("retry did not publish current attachment: post=%#v fake=%#v", post, fake)
	}
}

func TestWorkspacePostImageUploadUsesWorkspaceTenantAndAudit(t *testing.T) {
	ctx := context.Background()
	application, storage := newTestApp(t, nil)
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", store.Workspace{Name: "Media team"})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, store.Post{
		Title: "Workspace image", Content: "body", Format: store.FormatMarkdown,
	})
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	updated, err := application.SavePostImageForWorkspace(
		ctx, "test-owner", workspace.ID, post.ID, "workspace.png", bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if updated.WorkspaceID != workspace.ID || updated.UserID != workspace.CompatOwnerUserID ||
		len(updated.Attachments) != 1 {
		t.Fatalf("workspace image escaped tenant: %#v", updated)
	}
	usage, err := storage.GetWorkspaceMediaUsage(ctx, workspace.ID)
	if err != nil || usage.AssetCount != 1 || usage.TotalBytes <= 0 {
		t.Fatalf("workspace media usage=%#v err=%v", usage, err)
	}
	events, err := storage.ListAuditEvents(ctx, "test-owner", workspace.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Action == "post.image_uploaded" && event.EntityID == fmt.Sprint(post.ID) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("workspace image audit missing: %#v", events)
	}
}

func TestPersonalWorkspaceAndLegacyMediaRoutesShareOneQuota(t *testing.T) {
	ctx := context.Background()
	application, storage := newTestApp(t, nil)
	if err := application.ConfigureMediaPolicy(MediaPolicy{
		MaxFiles: 1, MaxBytes: 1 << 20, OrphanGrace: time.Hour,
		CleanupInterval: time.Hour, CleanupBatch: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := storage.UpsertUser(ctx, store.User{ID: "media-alias-second", DisplayName: "Second"}); err != nil {
		t.Fatal(err)
	}
	personalWorkspace := func(userID string) store.Workspace {
		workspaces, err := storage.ListWorkspaces(ctx, userID)
		if err != nil {
			t.Fatal(err)
		}
		for _, access := range workspaces {
			if access.Workspace.IsPersonal {
				return access.Workspace
			}
		}
		t.Fatalf("personal workspace missing for %s", userID)
		return store.Workspace{}
	}
	imageNumber := 0
	imageReader := func() *bytes.Reader {
		imageNumber++
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2+imageNumber, 2))); err != nil {
			t.Fatal(err)
		}
		return bytes.NewReader(encoded.Bytes())
	}

	first := personalWorkspace("test-owner")
	if _, err := application.SaveMediaForUser(ctx, "test-owner", "legacy-first.png", imageReader()); err != nil {
		t.Fatal(err)
	}
	if _, err := application.SaveAttachmentMediaForWorkspace(ctx, "test-owner", first.ID,
		media.AttachmentTypeImage, "nested-bypass.png", imageReader()); !errors.Is(err, store.ErrMediaQuotaExceeded) {
		t.Fatalf("nested personal media bypass=%v", err)
	}

	second := personalWorkspace("media-alias-second")
	if _, err := application.SaveAttachmentMediaForWorkspace(ctx, "media-alias-second", second.ID,
		media.AttachmentTypeImage, "nested-first.png", imageReader()); err != nil {
		t.Fatal(err)
	}
	if _, err := application.SaveMediaForUser(ctx, "media-alias-second", "legacy-bypass.png",
		imageReader()); !errors.Is(err, store.ErrMediaQuotaExceeded) {
		t.Fatalf("legacy personal media bypass=%v", err)
	}
}

func savePostAttachmentImage(t *testing.T, application *App, post store.Post, filename string) store.Post {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	updated, err := application.SavePostImageForUser(context.Background(), post.UserID, post.ID, filename, bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func saveMediaImage(t *testing.T, application *App, userID, filename string) media.File {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	file, err := application.SaveMediaForUser(context.Background(), userID, filename, bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return file
}
