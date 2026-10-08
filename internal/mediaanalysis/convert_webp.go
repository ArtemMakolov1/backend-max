package mediaanalysis

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"time"
)

const MaxStaticWebPBytes = 8 << 20

// ConvertStaticWebP preserves the decoded pixels, dimensions and alpha in PNG.
// An animation is never silently replaced with its first frame. The decoder
// receives validated bytes in one private local file, never a source URL.
func ConvertStaticWebP(ctx context.Context, body io.Reader) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case decoderSlots <- struct{}{}:
		defer func() { <-decoderSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if body == nil {
		return nil, errors.New("WebP reader is required")
	}
	stopClose := context.AfterFunc(ctx, func() {
		if closer, ok := body.(io.Closer); ok {
			_ = closer.Close()
		}
	})
	defer stopClose()
	raw, err := io.ReadAll(io.LimitReader(body, MaxStaticWebPBytes+1))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err != nil || len(raw) > MaxStaticWebPBytes {
		return nil, errors.New("WebP exceeds conversion input bound")
	}
	width, height, err := staticWebPDimensions(raw)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "maxposty-webp-")
	if err != nil {
		return nil, errors.New("WebP temporary storage unavailable")
	}
	defer func() { _ = os.RemoveAll(dir) }()
	input, output := filepath.Join(dir, "input.webp"), filepath.Join(dir, "output.png")
	if err := os.WriteFile(input, raw, 0600); err != nil {
		return nil, errors.New("WebP temporary storage unavailable")
	}
	args := []string{"-nostdin", "-v", "error", "-threads", "1", "-filter_threads", "1", "-max_alloc", "67108864",
		"-protocol_whitelist", "file,pipe", "-f", "webp_pipe", "-i", input, "-map", "0:v:0", "-an", "-sn", "-dn", "-frames:v", "1",
		"-c:v", "png", "-pix_fmt", "rgba", "-map_metadata", "-1", "-fs", "8388608", "-y", output}
	if _, err := command(ctx, "ffmpeg", 1024, args...); err != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("WebP conversion unavailable")
	}
	encoded, err := readSmallFile(output, MaxStaticWebPBytes)
	if err != nil {
		return nil, errors.New("PNG exceeds conversion output bound")
	}
	config, err := png.DecodeConfig(bytes.NewReader(encoded))
	if err != nil || config.Width != width || config.Height != height {
		return nil, errors.New("WebP conversion changed image dimensions")
	}
	if _, err := png.Decode(bytes.NewReader(encoded)); err != nil {
		return nil, errors.New("WebP conversion produced invalid PNG")
	}
	return encoded, nil
}

func staticWebPDimensions(raw []byte) (int, int, error) {
	invalid := errors.New("invalid or animated WebP")
	if len(raw) < 20 || string(raw[:4]) != "RIFF" || string(raw[8:12]) != "WEBP" || uint64(binary.LittleEndian.Uint32(raw[4:8]))+8 != uint64(len(raw)) {
		return 0, 0, invalid
	}
	width, height, canvasWidth, canvasHeight, chunks, pictures := 0, 0, 0, 0, 0, 0
	for offset := uint64(12); offset < uint64(len(raw)); {
		if uint64(len(raw))-offset < 8 || chunks >= 128 {
			return 0, 0, invalid
		}
		chunks++
		size := uint64(binary.LittleEndian.Uint32(raw[offset+4 : offset+8]))
		end := offset + 8 + size
		padded := end + size%2
		if padded > uint64(len(raw)) || (size%2 != 0 && raw[end] != 0) {
			return 0, 0, invalid
		}
		data := raw[offset+8 : end]
		switch string(raw[offset : offset+4]) {
		case "ANIM", "ANMF":
			return 0, 0, invalid
		case "VP8X":
			if chunks != 1 || len(data) != 10 || data[0]&0xc3 != 0 || data[1] != 0 || data[2] != 0 || data[3] != 0 {
				return 0, 0, invalid
			}
			canvasWidth = 1 + int(data[4]) + int(data[5])<<8 + int(data[6])<<16
			canvasHeight = 1 + int(data[7]) + int(data[8])<<8 + int(data[9])<<16
		case "VP8 ":
			if len(data) < 10 || data[0]&1 != 0 || !bytes.Equal(data[3:6], []byte{0x9d, 0x01, 0x2a}) {
				return 0, 0, invalid
			}
			pictures++
			width, height = int(binary.LittleEndian.Uint16(data[6:8])&0x3fff), int(binary.LittleEndian.Uint16(data[8:10])&0x3fff)
		case "VP8L":
			if len(data) < 5 || data[0] != 0x2f {
				return 0, 0, invalid
			}
			bits := binary.LittleEndian.Uint32(data[1:5])
			if bits>>29 != 0 {
				return 0, 0, invalid
			}
			pictures++
			width, height = int(bits&0x3fff)+1, int((bits>>14)&0x3fff)+1
		}
		offset = padded
	}
	if pictures != 1 || width <= 0 || height <= 0 || width > 7680 || height > 7680 || int64(width)*int64(height) > 16000000 || (canvasWidth != 0 && (canvasWidth != width || canvasHeight != height)) {
		return 0, 0, invalid
	}
	return width, height, nil
}
