// Package mediaanalysis prepares bounded local media for semantic analysis.
// Decoders receive a single temporary file and have no network protocols.
package mediaanalysis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const MaxInputBytes int64 = 64 << 20
const SampleSeconds = 30

var decoderSlots = make(chan struct{}, 2)

type Result struct {
	Frames   [][]byte
	Audio    []byte
	HasAudio bool
	Sampled  bool
}

// Prepare never accepts a URL or a caller-supplied decoder command. Paths and
// media bytes are scoped to a private directory that is removed on every exit.
func Prepare(ctx context.Context, body io.Reader, video bool) (Result, error) {
	select {
	case decoderSlots <- struct{}{}:
		defer func() { <-decoderSlots }()
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if closer, ok := body.(io.Closer); ok {
		stop := context.AfterFunc(ctx, func() { _ = closer.Close() })
		defer stop()
	}
	dir, err := os.MkdirTemp("", "maxposty-analysis-")
	if err != nil {
		return Result{}, errors.New("analysis temporary storage unavailable")
	}
	defer func() { _ = os.RemoveAll(dir) }()
	input := filepath.Join(dir, "input.bin")
	file, err := os.OpenFile(input, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Result{}, errors.New("analysis temporary storage unavailable")
	}
	n, copyErr := io.Copy(file, io.LimitReader(body, MaxInputBytes+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || n > MaxInputBytes || n == 0 {
		return Result{}, errors.New("analysis media exceeds input bound or is unreadable")
	}
	headerFile, err := os.Open(input)
	if err != nil {
		return Result{}, err
	}
	header := make([]byte, 16)
	_, _ = io.ReadFull(headerFile, header)
	_ = headerFile.Close()
	format := inputFormat(header, video)
	if format == "" {
		return Result{}, errors.New("unsupported analysis media container")
	}
	// Explicit demuxers exclude playlists, external-file references and format
	// auto-detection. Whitelisted protocols exclude all decoder network access.
	inputArgs := []string{"-protocol_whitelist", "file,pipe", "-f", format}
	if format == "mov" {
		inputArgs = append(inputArgs, "-enable_drefs", "0", "-use_absolute_path", "0")
	}
	inputArgs = append(inputArgs, "-i", input)
	probeArgs := append([]string{"-v", "error", "-max_alloc", "67108864"}, inputArgs...)
	probeArgs = append(probeArgs, "-show_entries", "stream=codec_type,width,height:format=duration", "-of", "json")
	probe, err := command(ctx, "ffprobe", 64<<10, probeArgs...)
	if err != nil {
		return Result{}, errors.New("analysis media metadata unavailable")
	}
	var metadata struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if json.Unmarshal(probe, &metadata) != nil || len(metadata.Streams) > 24 {
		return Result{}, errors.New("invalid analysis media metadata")
	}
	hasVisual, hasAudio := false, false
	for _, stream := range metadata.Streams {
		if stream.CodecType == "video" {
			if stream.Width <= 0 || stream.Height <= 0 || stream.Width > 8192 || stream.Height > 8192 || int64(stream.Width)*int64(stream.Height) > 16000000 {
				return Result{}, errors.New("analysis media dimensions exceed bound")
			}
			hasVisual = true
		}
		if stream.CodecType == "audio" {
			hasAudio = true
		}
	}
	if !hasVisual {
		return Result{}, errors.New("analysis media has no visual stream")
	}
	duration, durationErr := strconv.ParseFloat(metadata.Format.Duration, 64)
	result := Result{HasAudio: video && hasAudio, Sampled: video && (durationErr != nil || duration <= 0 || math.IsNaN(duration) || math.IsInf(duration, 0) || duration > SampleSeconds)}
	positions := []int{0}
	if video {
		for _, seconds := range []int{10, 20} {
			if duration > float64(seconds) {
				positions = append(positions, seconds)
			}
		}
	}
	for _, seconds := range positions {
		output := filepath.Join(dir, fmt.Sprintf("frame-%d.jpg", seconds))
		args := []string{"-nostdin", "-v", "error", "-threads", "1", "-filter_threads", "1", "-max_alloc", "67108864"}
		args = append(args, inputArgs...)
		args = append(args, "-ss", strconv.Itoa(seconds), "-map", "0:v:0", "-an", "-sn", "-dn", "-frames:v", "1", "-vf", "scale=768:768:force_original_aspect_ratio=decrease:force_divisible_by=2:flags=bilinear", "-q:v", "8", "-map_metadata", "-1", "-fs", "262144", "-y", output)
		if _, err := command(ctx, "ffmpeg", 1024, args...); err != nil {
			continue
		}
		frame, err := readSmallFile(output, 256<<10)
		if err != nil {
			continue
		}
		decoded, _, err := image.Decode(bytes.NewReader(frame))
		if err != nil || decoded.Bounds().Dx() > 768 || decoded.Bounds().Dy() > 768 {
			continue
		}
		result.Frames = append(result.Frames, frame)
	}
	if len(result.Frames) == 0 {
		return Result{}, errors.New("analysis frames unavailable")
	}
	if result.HasAudio {
		output := filepath.Join(dir, "audio.wav")
		args := []string{"-nostdin", "-v", "error", "-threads", "1", "-max_alloc", "67108864"}
		args = append(args, inputArgs...)
		args = append(args, "-map", "0:a:0", "-vn", "-sn", "-dn", "-t", strconv.Itoa(SampleSeconds), "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-map_metadata", "-1", "-fs", "2097152", "-y", output)
		if _, err := command(ctx, "ffmpeg", 1024, args...); err == nil {
			result.Audio, _ = readSmallFile(output, 2<<20)
		}
	}
	return result, nil
}

func inputFormat(header []byte, video bool) string {
	if video {
		if len(header) >= 8 && string(header[4:8]) == "ftyp" {
			return "mov"
		}
		if bytes.HasPrefix(header, []byte{0x1a, 0x45, 0xdf, 0xa3}) {
			return "matroska"
		}
		return ""
	}
	switch {
	case bytes.HasPrefix(header, []byte{0xff, 0xd8, 0xff}):
		return "jpeg_pipe"
	case bytes.HasPrefix(header, []byte{0x89, 'P', 'N', 'G', 13, 10, 26, 10}):
		return "png_pipe"
	case bytes.HasPrefix(header, []byte("GIF87a")) || bytes.HasPrefix(header, []byte("GIF89a")):
		return "gif"
	case len(header) >= 12 && string(header[:4]) == "RIFF" && string(header[8:12]) == "WEBP":
		return "webp_pipe"
	default:
		return ""
	}
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errors.New("decoder output exceeds bound")
	}
	return b.Buffer.Write(p)
}

func command(ctx context.Context, name string, maxOutput int, args ...string) ([]byte, error) {
	// #nosec G204 -- this private helper is called only with fixed ffmpeg/ffprobe commands and server-built arguments; no URLs or caller commands enter it.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LANG=C", "LC_ALL=C"}
	cmd.WaitDelay = time.Second
	var stdout = boundedBuffer{limit: maxOutput}
	var stderr = boundedBuffer{limit: 16 << 10}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, errors.New("media decoder failed")
	}
	return stdout.Bytes(), nil
}

func readSmallFile(path string, limit int) ([]byte, error) {
	// #nosec G703 -- path is generated inside the private temporary directory, never from a caller or media metadata.
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil || len(body) > limit || len(body) == 0 {
		return nil, errors.New("invalid prepared media")
	}
	return body, nil
}

// Available reports an installation problem before downloading large media.
func Available() bool {
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if path, err := exec.LookPath(name); err != nil || strings.TrimSpace(path) == "" {
			return false
		}
	}
	return true
}
