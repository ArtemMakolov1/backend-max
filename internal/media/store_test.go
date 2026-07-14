package media

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"
)

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
	file, err := mediaStore.Save("../../unsafe.png", bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if file.Width != 2 || file.Height != 3 || file.MIMEType != "image/png" {
		t.Fatalf("unexpected media metadata: %#v", file)
	}
	resolved, err := mediaStore.ResolveURL(file.URL)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != file.Path {
		t.Fatalf("resolved path = %q, want %q", resolved, file.Path)
	}
	if _, err := os.Stat(file.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := mediaStore.ResolveURL("https://attacker.invalid/media/file.png"); err == nil {
		t.Fatal("external media URL was accepted")
	}
}
