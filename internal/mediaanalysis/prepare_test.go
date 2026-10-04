package mediaanalysis

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func requireDecoders(t *testing.T) {
	t.Helper()
	if !Available() {
		t.Skip("local ffmpeg/ffprobe required; runtime image smoke test also exercises decoding")
	}
}

func TestPrepareImageProducesBoundedJPEGAndRemovesTemporaryFiles(t *testing.T) {
	requireDecoders(t)
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	var raw bytes.Buffer
	if err := png.Encode(&raw, image.NewRGBA(image.Rect(0, 0, 1800, 900))); err != nil {
		t.Fatal(err)
	}
	result, err := Prepare(t.Context(), bytes.NewReader(raw.Bytes()), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Frames) != 1 || len(result.Audio) != 0 || result.HasAudio || result.Sampled || len(result.Frames[0]) > 256<<10 {
		t.Fatalf("invalid prepared image: %#v", result)
	}
	img, _, err := image.Decode(bytes.NewReader(result.Frames[0]))
	if err != nil || img.Bounds().Dx() > 768 || img.Bounds().Dy() > 768 {
		t.Fatalf("unbounded frame: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("media temp files retained: %v %v", entries, err)
	}
}

func TestPrepareVideoExtractsRealFrameAndSoundWithoutURLs(t *testing.T) {
	requireDecoders(t)
	path := filepath.Join(t.TempDir(), "synthetic.mp4")
	cmd := exec.CommandContext(t.Context(), "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "color=c=blue:s=64x64:d=0.4", "-f", "lavfi", "-i", "sine=frequency=440:duration=0.4", "-threads", "1", "-c:v", "mpeg4", "-c:a", "aac", "-shortest", "-y", path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("synthetic fixture generation: %v %s", err, output)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	result, err := Prepare(t.Context(), file, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Frames) != 1 || !result.HasAudio || len(result.Audio) < 44 || len(result.Audio) > 2<<20 || string(result.Audio[:4]) != "RIFF" || string(result.Audio[8:12]) != "WAVE" {
		t.Fatalf("real video/audio extraction absent: frames=%d hasAudio=%t bytes=%d", len(result.Frames), result.HasAudio, len(result.Audio))
	}
}

func TestPrepareRejectsPlaylistAndCanceledInputWithoutKeepingFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	for _, raw := range []string{"#EXTM3U\nhttps://127.0.0.1/private\n", "ffconcat version 1.0\nfile /etc/passwd\n", "RIFFnot-a-video"} {
		if _, err := Prepare(t.Context(), bytes.NewBufferString(raw), true); err == nil {
			t.Fatal("unsafe external/unsupported container accepted")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Prepare(ctx, bytes.NewReader([]byte{0xff, 0xd8, 0xff}), false); err == nil {
		t.Fatal("canceled work executed")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed temp files retained: %v %v", entries, err)
	}
}
