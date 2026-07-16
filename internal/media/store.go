package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	MaxImageBytes = 50 << 20
	MaxVideoBytes = 250 << 20
	MaxImageEdge  = 7680

	AttachmentTypeImage = "image"
	AttachmentTypeVideo = "video"
)

type File struct {
	Type     string `json:"type"`
	URL      string `json:"url"`
	Filename string `json:"filename"`
	// Path is an opaque storage key kept under the historical field name so
	// existing database rows and API contracts do not need a breaking change.
	Path     string `json:"-"`
	MIMEType string `json:"mime_type"`
	Size     int64  `json:"size"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	// DurationMS is zero when the container does not expose a cheap duration.
	// It remains nullable in the post attachment API.
	DurationMS int64 `json:"duration_ms,omitempty"`
}

type Object struct {
	Body     io.ReadCloser
	Filename string
	MIMEType string
	// Size is the number of bytes available from Body. TotalSize is the full
	// object length and differs from Size only for range reads.
	Size         int64
	TotalSize    int64
	LastModified time.Time
}

type Info struct {
	Filename     string
	MIMEType     string
	Size         int64
	LastModified time.Time
}

var ErrRangeNotSatisfiable = errors.New("media byte range is not satisfiable")

type objectInfo struct {
	MIMEType     string
	Size         int64
	LastModified time.Time
}

type backend interface {
	Put(context.Context, string, string, int64, io.Reader) error
	Head(context.Context, string) (objectInfo, error)
	Open(context.Context, string) (Object, error)
	OpenRange(context.Context, string, int64, int64) (Object, error)
	Delete(context.Context, string) error
}

type Store struct {
	backend       backend
	publicBaseURL string
	publicBase    *url.URL
	maxImageBytes int64
	maxVideoBytes int64
	maxImageEdge  int
}

// New creates the local filesystem implementation used by local development
// and tests. Production uses NewS3 while preserving the same protected URLs.
func New(dir, publicBaseURL string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("media directory is required")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve media directory: %w", err)
	}
	if err := os.MkdirAll(absDir, 0o750); err != nil {
		return nil, fmt.Errorf("create media directory: %w", err)
	}
	return newStore(&localBackend{dir: absDir}, publicBaseURL)
}

func newStore(storage backend, publicBaseURL string) (*Store, error) {
	if storage == nil {
		return nil, errors.New("media backend is required")
	}
	base, err := url.Parse(strings.TrimRight(publicBaseURL, "/"))
	if err != nil || !base.IsAbs() || base.Host == "" {
		return nil, errors.New("public base URL must be an absolute URL")
	}
	return &Store{
		backend: storage, publicBaseURL: strings.TrimRight(publicBaseURL, "/"), publicBase: base,
		maxImageBytes: MaxImageBytes, maxVideoBytes: MaxVideoBytes, maxImageEdge: MaxImageEdge,
	}, nil
}

// Upload is a validated temporary image. Call Store only after the database
// has atomically reserved the tenant's quota, then always Close it.
type Upload struct {
	store    *Store
	file     File
	tempPath string
}

func (u *Upload) File() File { return u.file }

func (u *Upload) Store(ctx context.Context) error {
	if u == nil || u.store == nil || u.tempPath == "" {
		return errors.New("media upload is closed")
	}
	// #nosec G703 -- tempPath is returned by os.CreateTemp and never includes user input.
	payload, err := os.Open(u.tempPath)
	if err != nil {
		return fmt.Errorf("open validated media: %w", err)
	}
	putErr := u.store.backend.Put(ctx, u.file.Filename, u.file.MIMEType, u.file.Size, payload)
	closeErr := payload.Close()
	if putErr != nil {
		if closeErr != nil {
			putErr = errors.Join(putErr, fmt.Errorf("close validated media: %w", closeErr))
		}
		return fmt.Errorf("store media: %w", putErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close validated media: %w", closeErr)
	}
	return nil
}

func (u *Upload) Close() error {
	if u == nil || u.tempPath == "" {
		return nil
	}
	name := u.tempPath
	u.tempPath = ""
	return os.Remove(name)
}

func (s *Store) Save(ctx context.Context, originalName string, reader io.Reader) (File, error) {
	upload, err := s.Prepare(originalName, reader)
	if err != nil {
		return File{}, err
	}
	defer func() { _ = upload.Close() }()
	if err := upload.Store(ctx); err != nil {
		return File{}, err
	}
	return upload.File(), nil
}

func (s *Store) SaveAttachment(ctx context.Context, attachmentType, originalName string, reader io.Reader) (File, error) {
	upload, err := s.PrepareAttachment(attachmentType, originalName, reader)
	if err != nil {
		return File{}, err
	}
	defer func() { _ = upload.Close() }()
	if err := upload.Store(ctx); err != nil {
		return File{}, err
	}
	return upload.File(), nil
}

// Prepare validates and hashes an image without mutating the backing store.
// This split lets the application reserve per-tenant quota before S3 upload.
func (s *Store) Prepare(originalName string, reader io.Reader) (*Upload, error) {
	return s.PrepareAttachment(AttachmentTypeImage, originalName, reader)
}

// PrepareAttachment validates and hashes an image or video without mutating
// object storage. Input is streamed into a temporary file with a hard byte
// limit, so even a maximum-size video never needs to be buffered in memory.
func (s *Store) PrepareAttachment(attachmentType, originalName string, reader io.Reader) (*Upload, error) {
	if reader == nil {
		return nil, errors.New("media reader is required")
	}
	var maxBytes int64
	switch attachmentType {
	case AttachmentTypeImage:
		maxBytes = s.maxImageBytes
	case AttachmentTypeVideo:
		maxBytes = s.maxVideoBytes
	default:
		return nil, errors.New("attachment type must be image or video")
	}
	tmp, err := os.CreateTemp("", ".maxposty-upload-*")
	if err != nil {
		return nil, fmt.Errorf("create media temp file: %w", err)
	}
	tmpName := tmp.Name()
	keepTemp := false
	defer func() {
		if !keepTemp {
			_ = os.Remove(tmpName)
		}
	}()

	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(reader, maxBytes+1))
	closeErr := tmp.Close()
	if copyErr != nil {
		resultErr := fmt.Errorf("save media: %w", copyErr)
		if closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close image: %w", closeErr))
		}
		return nil, resultErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close image: %w", closeErr)
	}
	if written == 0 {
		return nil, errors.New("media file is empty")
	}
	if written > maxBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", attachmentType, maxBytes)
	}

	var mimeType, ext string
	var width, height int
	if attachmentType == AttachmentTypeImage {
		mimeType, ext, width, height, err = s.inspectImage(tmpName)
	} else {
		mimeType, ext, err = inspectVideo(tmpName, originalName)
	}
	if err != nil {
		return nil, err
	}
	filename := hex.EncodeToString(hash.Sum(nil)) + ext
	_ = originalName // Persisted names are content-addressed to prevent path traversal.
	file := File{
		Type: attachmentType, URL: s.URL(filename), Filename: filename, Path: filename, MIMEType: mimeType,
		Size: written, Width: width, Height: height,
	}
	keepTemp = true
	return &Upload{store: s, file: file, tempPath: tmpName}, nil
}

func (s *Store) inspectImage(tmpName string) (string, string, int, int, error) {
	// #nosec G703 -- tmpName is returned by os.CreateTemp and never includes user input.
	inspection, err := os.Open(tmpName)
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("inspect image: %w", err)
	}
	header := make([]byte, 512)
	headerN, readErr := io.ReadFull(inspection, header)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		_ = inspection.Close()
		return "", "", 0, 0, fmt.Errorf("inspect image header: %w", readErr)
	}
	mimeType := http.DetectContentType(header[:headerN])
	if _, err := inspection.Seek(0, io.SeekStart); err != nil {
		_ = inspection.Close()
		return "", "", 0, 0, fmt.Errorf("rewind image inspection file: %w", err)
	}
	config, _, decodeErr := image.DecodeConfig(inspection)
	closeErr := inspection.Close()
	if decodeErr != nil {
		return "", "", 0, 0, errors.New("unsupported or invalid image; use PNG, JPEG or GIF")
	}
	if closeErr != nil {
		return "", "", 0, 0, fmt.Errorf("close image inspection file: %w", closeErr)
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > s.maxImageEdge || config.Height > s.maxImageEdge {
		return "", "", 0, 0, fmt.Errorf("image dimensions must be between 1 and %d pixels per edge", s.maxImageEdge)
	}
	ext, ok := extensionForMIME(mimeType)
	if !ok {
		return "", "", 0, 0, fmt.Errorf("unsupported image type %q; use PNG, JPEG or GIF", mimeType)
	}
	return mimeType, ext, config.Width, config.Height, nil
}

func inspectVideo(tmpName, originalName string) (string, string, error) {
	// #nosec G703 -- tmpName is returned by os.CreateTemp and never includes user input.
	inspection, err := os.Open(tmpName)
	if err != nil {
		return "", "", fmt.Errorf("inspect video: %w", err)
	}
	header := make([]byte, 4096)
	headerN, readErr := io.ReadFull(inspection, header)
	closeErr := inspection.Close()
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return "", "", fmt.Errorf("inspect video header: %w", readErr)
	}
	if closeErr != nil {
		return "", "", fmt.Errorf("close video inspection file: %w", closeErr)
	}
	header = header[:headerN]
	originalExt := strings.ToLower(filepath.Ext(originalName))
	if len(header) >= 12 && bytes.Equal(header[4:8], []byte("ftyp")) {
		switch originalExt {
		case ".mov":
			return "video/quicktime", ".mov", nil
		case ".mp4":
			return "video/mp4", ".mp4", nil
		default:
			return "", "", errors.New("ISO media video filename must end in .mp4 or .mov")
		}
	}
	if len(header) >= 4 && bytes.Equal(header[:4], []byte{0x1a, 0x45, 0xdf, 0xa3}) {
		lowerHeader := bytes.ToLower(header)
		switch {
		case bytes.Contains(lowerHeader, []byte("webm")):
			if originalExt != ".webm" {
				return "", "", errors.New("video content is WebM but the filename does not end in .webm")
			}
			return "video/webm", ".webm", nil
		case bytes.Contains(lowerHeader, []byte("matroska")):
			if originalExt != ".mkv" {
				return "", "", errors.New("video content is Matroska but the filename does not end in .mkv")
			}
			return "video/x-matroska", ".mkv", nil
		}
	}
	return "", "", errors.New("unsupported or invalid video; use MP4, MOV, MKV or WEBM")
}

func (s *Store) URL(filename string) string {
	return s.publicBaseURL + "/media/" + url.PathEscape(filename)
}

func (s *Store) ResolveURL(ctx context.Context, rawURL string) (string, error) {
	filename, err := s.FilenameFromURL(rawURL)
	if err != nil {
		return "", err
	}
	if _, err := s.backend.Head(ctx, filename); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errors.New("media file does not exist")
		}
		return "", fmt.Errorf("check media file: %w", err)
	}
	return filename, nil
}

// FilenameFromURL validates that rawURL refers to this media store without
// touching storage. This lets callers enforce tenant ownership before
// revealing whether the underlying private object exists.
func (s *Store) FilenameFromURL(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", errors.New("invalid image URL")
	}
	if parsed.IsAbs() {
		if !strings.EqualFold(parsed.Scheme, s.publicBase.Scheme) || !strings.EqualFold(parsed.Host, s.publicBase.Host) {
			return "", errors.New("only images stored by this service can be uploaded to MAX")
		}
	}
	if !strings.HasPrefix(parsed.Path, "/media/") {
		return "", errors.New("image URL is not a local media URL")
	}
	filename, err := url.PathUnescape(strings.TrimPrefix(parsed.Path, "/media/"))
	if err != nil || !validFilename(filename) {
		return "", errors.New("invalid media filename")
	}
	return filename, nil
}

func (s *Store) Open(ctx context.Context, filename string) (Object, error) {
	decoded, err := url.PathUnescape(filename)
	if err != nil || !validFilename(decoded) {
		return Object{}, errors.New("invalid media filename")
	}
	return s.backend.Open(ctx, decoded)
}

func (s *Store) Info(ctx context.Context, filename string) (Info, error) {
	decoded, err := url.PathUnescape(filename)
	if err != nil || !validFilename(decoded) {
		return Info{}, errors.New("invalid media filename")
	}
	info, err := s.backend.Head(ctx, decoded)
	if err != nil {
		return Info{}, err
	}
	return Info{
		Filename: decoded, MIMEType: info.MIMEType, Size: info.Size, LastModified: info.LastModified,
	}, nil
}

// OpenRange returns a closed byte interval from a private media object. The
// caller must validate tenant ownership before invoking it, exactly like Open.
func (s *Store) OpenRange(ctx context.Context, filename string, start, end int64) (Object, error) {
	decoded, err := url.PathUnescape(filename)
	if err != nil || !validFilename(decoded) {
		return Object{}, errors.New("invalid media filename")
	}
	if start < 0 || end < start {
		return Object{}, ErrRangeNotSatisfiable
	}
	return s.backend.OpenRange(ctx, decoded, start, end)
}

// Delete removes a content-addressed object. Missing objects are successful so
// the garbage collector can safely retry after crashes.
func (s *Store) Delete(ctx context.Context, filename string) error {
	if !validFilename(filename) {
		return errors.New("invalid media filename")
	}
	if err := s.backend.Delete(ctx, filename); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func validFilename(filename string) bool {
	return filename != "" && filename == path.Base(filename) && !strings.ContainsAny(filename, `/\\`)
}

type localBackend struct {
	dir string
}

func (b *localBackend) Put(_ context.Context, key, _ string, _ int64, reader io.Reader) error {
	if !validFilename(key) {
		return errors.New("invalid media filename")
	}
	destination := filepath.Join(b.dir, key)
	// #nosec G703 -- key is a validated content-addressed filename and b.dir is resolved at startup.
	if info, err := os.Stat(destination); err == nil && info.Mode().IsRegular() {
		return nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(b.dir, ".persist-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, reader); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// #nosec G703 -- both paths are derived from a private directory and a validated content-addressed key.
	return os.Rename(tmpName, destination)
}

func (b *localBackend) Head(_ context.Context, key string) (objectInfo, error) {
	if !validFilename(key) {
		return objectInfo{}, errors.New("invalid media filename")
	}
	info, err := os.Stat(filepath.Join(b.dir, key))
	if err != nil {
		return objectInfo{}, err
	}
	if !info.Mode().IsRegular() {
		return objectInfo{}, os.ErrNotExist
	}
	return objectInfo{MIMEType: mimeForFilename(key), Size: info.Size(), LastModified: info.ModTime()}, nil
}

func (b *localBackend) Open(_ context.Context, key string) (Object, error) {
	if !validFilename(key) {
		return Object{}, errors.New("invalid media filename")
	}
	file, err := os.Open(filepath.Join(b.dir, key))
	if err != nil {
		return Object{}, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = os.ErrNotExist
		}
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return Object{}, err
	}
	return Object{
		Body: file, Filename: key, MIMEType: mimeForFilename(key),
		Size: info.Size(), TotalSize: info.Size(), LastModified: info.ModTime(),
	}, nil
}

func (b *localBackend) OpenRange(_ context.Context, key string, start, end int64) (Object, error) {
	if !validFilename(key) {
		return Object{}, errors.New("invalid media filename")
	}
	file, err := os.Open(filepath.Join(b.dir, key))
	if err != nil {
		return Object{}, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = os.ErrNotExist
		}
		return Object{}, errors.Join(err, file.Close())
	}
	total := info.Size()
	if start < 0 || start >= total || end < start {
		return Object{}, errors.Join(ErrRangeNotSatisfiable, file.Close())
	}
	if end >= total {
		end = total - 1
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return Object{}, errors.Join(err, file.Close())
	}
	length := end - start + 1
	return Object{
		Body:     &limitedReadCloser{Reader: io.LimitReader(file, length), Closer: file},
		Filename: key, MIMEType: mimeForFilename(key), Size: length, TotalSize: total,
		LastModified: info.ModTime(),
	}, nil
}

type limitedReadCloser struct {
	io.Reader
	io.Closer
}

func (b *localBackend) Delete(_ context.Context, key string) error {
	if !validFilename(key) {
		return errors.New("invalid media filename")
	}
	err := os.Remove(filepath.Join(b.dir, key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func extensionForMIME(mimeType string) (string, bool) {
	switch mimeType {
	case "image/jpeg":
		return ".jpg", true
	case "image/png":
		return ".png", true
	case "image/gif":
		return ".gif", true
	default:
		if extensions, _ := mime.ExtensionsByType(mimeType); len(extensions) > 0 {
			return extensions[0], false
		}
		return "", false
	}
}

func mimeForFilename(filename string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	case ".mp4":
		return "video/mp4"
	default:
		return mime.TypeByExtension(filepath.Ext(filename))
	}
}
