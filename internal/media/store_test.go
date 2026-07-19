package media

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"testing"
)

func TestPrepareDoesNotWriteUntilReservationCanBeMade(t *testing.T) {
	t.Parallel()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	mediaStore, err := New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	upload, err := mediaStore.Prepare("image.png", bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upload.Close() }()
	file := upload.File()
	if _, err := mediaStore.Open(ctx, file.Filename); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Prepare wrote an object before quota reservation: %v", err)
	}
	if err := upload.Store(ctx); err != nil {
		t.Fatal(err)
	}
	object, err := mediaStore.Open(ctx, file.Filename)
	if err != nil {
		t.Fatal(err)
	}
	_ = object.Body.Close()
	if err := mediaStore.Delete(ctx, file.Filename); err != nil {
		t.Fatal(err)
	}
	if err := mediaStore.Delete(ctx, file.Filename); err != nil {
		t.Fatalf("Delete must be idempotent: %v", err)
	}
}

func TestLocalRangeReadSeeksWithoutLoadingWholeObject(t *testing.T) {
	t.Parallel()
	mediaStore, err := New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	file, err := mediaStore.Save(context.Background(), "range.png", bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	object, err := mediaStore.OpenRange(context.Background(), file.Path, 3, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = object.Body.Close() }()
	payload, err := io.ReadAll(object.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, encoded.Bytes()[3:9]) || object.Size != 6 || object.TotalSize != int64(encoded.Len()) {
		t.Fatalf("local range bytes=%x size=%d total=%d", payload, object.Size, object.TotalSize)
	}
	if _, err := mediaStore.OpenRange(context.Background(), file.Path, int64(encoded.Len()), int64(encoded.Len()+1)); !errors.Is(err, ErrRangeNotSatisfiable) {
		t.Fatalf("out-of-bounds range error = %v", err)
	}
}

func TestSaveAndResolveURL(t *testing.T) {
	t.Parallel()
	var encoded bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 3))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}

	mediaStore, err := New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	file, err := mediaStore.Save(ctx, "../../unsafe.png", bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if file.Width != 2 || file.Height != 3 || file.MIMEType != "image/png" {
		t.Fatalf("unexpected media metadata: %#v", file)
	}
	resolved, err := mediaStore.ResolveURL(ctx, file.URL)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != file.Path {
		t.Fatalf("resolved path = %q, want %q", resolved, file.Path)
	}
	object, err := mediaStore.Open(ctx, file.Path)
	if err != nil {
		t.Fatal(err)
	}
	_ = object.Body.Close()
	if _, err := mediaStore.ResolveURL(ctx, "https://attacker.invalid/media/file.png"); err == nil {
		t.Fatal("external media URL was accepted")
	}
}

func TestPrepareVideoValidatesContainerAndStreamsToTempFile(t *testing.T) {
	t.Parallel()
	mediaStore, err := New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	mp4 := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{0}, 64)...)
	upload, err := mediaStore.PrepareAttachment(AttachmentTypeVideo, "clip.mp4", bytes.NewReader(mp4))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upload.Close() }()
	file := upload.File()
	if file.Type != AttachmentTypeVideo || file.MIMEType != "video/mp4" || file.Size != int64(len(mp4)) || file.Width != 0 || file.Height != 0 {
		t.Fatalf("unexpected video metadata: %#v", file)
	}
	if _, err := mediaStore.PrepareAttachment(AttachmentTypeVideo, "clip.webm", bytes.NewReader(mp4)); err == nil {
		t.Fatal("MP4 content with a WebM extension was accepted")
	}
}

func TestPrepareVideoEnforcesConfiguredLimit(t *testing.T) {
	t.Parallel()
	mediaStore, err := New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	mediaStore.maxVideoBytes = 16
	payload := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{0}, 16)...)
	if _, err := mediaStore.PrepareAttachment(AttachmentTypeVideo, "large.mp4", bytes.NewReader(payload)); err == nil {
		t.Fatal("oversized video was accepted")
	}
}

func TestPrepareRejectsActualWebPContent(t *testing.T) {
	t.Parallel()
	mediaStore, err := New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	webp := append([]byte("RIFF\x10\x00\x00\x00WEBPVP8 "), bytes.Repeat([]byte{0}, 32)...)
	if _, err := mediaStore.PrepareAttachment(AttachmentTypeImage, "image.webp", bytes.NewReader(webp)); err == nil {
		t.Fatal("unsupported WebP content was accepted as a MAX-compatible image")
	}
}
