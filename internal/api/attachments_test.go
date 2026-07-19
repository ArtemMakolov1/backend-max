package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

func TestRequestedAttachmentType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		raw      string
		filename string
		want     string
		wantErr  bool
	}{
		{name: "infer png", filename: "cover.PNG", want: media.AttachmentTypeImage},
		{name: "infer jpeg", filename: "cover.jpeg", want: media.AttachmentTypeImage},
		{name: "infer mp4", filename: "clip.MP4", want: media.AttachmentTypeVideo},
		{name: "infer webm", filename: "clip.webm", want: media.AttachmentTypeVideo},
		{name: "explicit normalized", raw: " VIDEO ", filename: "opaque", want: media.AttachmentTypeVideo},
		{name: "unsupported extension", filename: "document.pdf", wantErr: true},
		{name: "unsupported explicit type", raw: "audio", filename: "voice.mp3", wantErr: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := requestedAttachmentType(test.raw, test.filename)
			if test.wantErr {
				if !errors.Is(err, errAttachmentUnsupported) {
					t.Fatalf("requestedAttachmentType() error = %v, want unsupported attachment", err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("requestedAttachmentType() = (%q, %v), want (%q, nil)", got, err, test.want)
			}
		})
	}
}

func TestAttachmentFromMediaMapsOnlyAvailableMetadata(t *testing.T) {
	t.Parallel()
	video := attachmentFromMedia(media.File{
		Type: media.AttachmentTypeVideo, Path: "video-key.mp4", MIMEType: "video/mp4", Size: 4096,
		Width: 1920, Height: 1080, DurationMS: 37_500,
	}, 2)
	if video.Type != store.PostAttachmentVideo || video.Position != 2 || video.StorageKey != "video-key.mp4" ||
		video.ProcessingStatus != store.AttachmentStatusReady || video.SizeBytes != 4096 || video.MIMEType != "video/mp4" {
		t.Fatalf("attachmentFromMedia() lost core metadata: %#v", video)
	}
	if video.Width == nil || *video.Width != 1920 || video.Height == nil || *video.Height != 1080 ||
		video.DurationMS == nil || *video.DurationMS != 37_500 {
		t.Fatalf("attachmentFromMedia() lost video metadata: %#v", video)
	}

	image := attachmentFromMedia(media.File{
		Type: media.AttachmentTypeImage, Path: "image-key.png", MIMEType: "image/png", Size: 1024,
		Width: 800, Height: 600,
	}, -1)
	if image.DurationMS != nil || image.Width == nil || *image.Width != 800 || image.Height == nil || *image.Height != 600 {
		t.Fatalf("attachmentFromMedia() produced invalid optional image metadata: %#v", image)
	}
}

func TestAttachmentUploadOutcomeIsBounded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "success", want: "success"},
		{name: "unsupported sentinel", err: errAttachmentUnsupported, want: "unsupported"},
		{name: "unsupported validator", err: errors.New("unsupported or invalid video"), want: "unsupported"},
		{name: "too large sentinel", err: errAttachmentTooLarge, want: "too_large"},
		{name: "too large validator", err: errors.New("video exceeds 1024 bytes"), want: "too_large"},
		{name: "quota", err: store.ErrMediaQuotaExceeded, want: "quota_exceeded"},
		{name: "duplicate upload", err: store.ErrMediaUploadBusy, want: "busy"},
		{name: "gate busy", err: errMediaUploadRateLimited, want: "busy"},
		{name: "canceled", err: context.Canceled, want: "canceled"},
		{name: "deadline", err: context.DeadlineExceeded, want: "canceled"},
		{name: "other", err: errors.New("S3 unavailable"), want: "error"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := attachmentUploadOutcome(test.err); got != test.want {
				t.Fatalf("attachmentUploadOutcome(%v) = %q, want %q", test.err, got, test.want)
			}
		})
	}
}

func TestAttachmentResponseSelectionDoesNotDependOnGalleryPosition(t *testing.T) {
	t.Parallel()
	attachments := []store.PostAttachment{
		{ID: 101, Position: 0, StorageKey: "new.png"},
		{ID: 99, Position: 1, StorageKey: "old.png"},
		{ID: 100, Position: 2, StorageKey: "new.png"},
	}
	created, ok := newestAttachmentByStorageKey(attachments, "new.png")
	if !ok || created.ID != 101 || created.Position != 0 {
		t.Fatalf("newestAttachmentByStorageKey() = (%#v, %t), want newly inserted id 101 at position 0", created, ok)
	}
	replaced, ok := postAttachmentByID(attachments, 99)
	if !ok || replaced.StorageKey != "old.png" {
		t.Fatalf("postAttachmentByID() = (%#v, %t), want stable replacement id 99", replaced, ok)
	}
	if _, ok := newestAttachmentByStorageKey(attachments, "missing.png"); ok {
		t.Fatal("newestAttachmentByStorageKey() found a missing object")
	}
}

func TestAttachmentMutationResponseCarriesLatestPostRevision(t *testing.T) {
	t.Parallel()
	updated := store.Post{ID: 42, UpdatedAt: time.Date(2026, 7, 16, 12, 30, 0, 0, time.UTC)}
	attachment := store.PostAttachment{ID: 7, Position: 0, StorageKey: "new.png"}
	response := attachmentMutationResponse{Attachment: attachment, Post: updated}
	if response.Attachment.ID != 7 || response.Post.ID != 42 || response.Post.UpdatedAt != updated.UpdatedAt {
		t.Fatalf("attachmentMutationResponse lost the attachment or latest post revision: %#v", response)
	}
}
