package media

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
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
