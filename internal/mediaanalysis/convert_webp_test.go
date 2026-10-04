package mediaanalysis

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"
)

// A 7x5 synthetic lossless WebP with known RGB/alpha, generated offline.
//
//go:embed testdata/synthetic-alpha.webp
var transparentWebPFixture []byte

func syntheticTransparentWebP(t *testing.T) ([]byte, *image.NRGBA) {
	t.Helper()
	requireDecoders(t)
	img := image.NewNRGBA(image.Rect(0, 0, 7, 5))
	for y := 0; y < 5; y++ {
		for x := 0; x < 7; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 30), G: uint8(y * 40), B: 120, A: uint8((x + y) * 20)})
		}
	}
	return append([]byte(nil), transparentWebPFixture...), img
}

func TestConvertStaticWebPPreservesDimensionsPixelsAndAlpha(t *testing.T) {
	raw, original := syntheticTransparentWebP(t)
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	result, err := ConvertStaticWebP(t.Context(), bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	converted, err := png.Decode(bytes.NewReader(result))
	if err != nil || converted.Bounds() != original.Bounds() {
		t.Fatalf("conversion changed dimensions: %v err=%v", converted, err)
	}
	for y := 0; y < 5; y++ {
		for x := 0; x < 7; x++ {
			a := color.NRGBAModel.Convert(converted.At(x, y)).(color.NRGBA)
			b := original.NRGBAAt(x, y)
			if a.A != b.A || (b.A != 0 && a != b) {
				t.Fatalf("pixel/alpha changed at %d,%d: got=%#v want=%#v", x, y, a, b)
			}
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("conversion retained temporary media")
	}
}

func TestConvertStaticWebPRejectsAnimatedCorruptLargeAndCanceledInputs(t *testing.T) {
	raw, _ := syntheticTransparentWebP(t)
	animated := append([]byte(nil), raw...)
	if string(animated[12:16]) != "VP8X" {
		// Lossless encoders may use simple VP8L with intrinsic alpha. Build a
		// valid extended canvas around it to exercise the animation flag too.
		chunk := []byte{'V', 'P', '8', 'X', 10, 0, 0, 0, 0x10, 0, 0, 0, 6, 0, 0, 4, 0, 0}
		animated = append(append(append([]byte(nil), raw[:12]...), chunk...), raw[12:]...)
		binary.LittleEndian.PutUint32(animated[4:8], binary.LittleEndian.Uint32(raw[4:8])+18)
	}
	animated[20] |= 2
	malformedChunk := append([]byte(nil), raw...)
	binary.LittleEndian.PutUint32(malformedChunk[16:20], ^uint32(0))
	for _, sample := range [][]byte{animated, malformedChunk, append(append([]byte(nil), raw...), 0), bytes.Repeat([]byte{0}, MaxStaticWebPBytes+1)} {
		if _, err := ConvertStaticWebP(t.Context(), bytes.NewReader(sample)); err == nil {
			t.Fatal("unsafe/animated/oversized WebP accepted")
		}
	}
	oversized := append([]byte(nil), animated...)
	oversized[20] &^= 2
	oversized[24], oversized[25], oversized[26] = 0xff, 0xff, 0xff
	if _, err := ConvertStaticWebP(t.Context(), bytes.NewReader(oversized)); err == nil {
		t.Fatal("oversized canvas reached decoder")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ConvertStaticWebP(ctx, bytes.NewReader(raw)); err == nil {
		t.Fatal("canceled conversion completed")
	}
}
