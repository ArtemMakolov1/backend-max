package media

import (
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
	MaxImageEdge  = 7680
)

type File struct {
	URL      string `json:"url"`
	Filename string `json:"filename"`
	// Path is an opaque storage key kept under the historical field name so
	// existing database rows and API contracts do not need a breaking change.
	Path     string `json:"-"`
	MIMEType string `json:"mime_type"`
	Size     int64  `json:"size"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

type Object struct {
	Body         io.ReadCloser
	Filename     string
	MIMEType     string
	Size         int64
	LastModified time.Time
}

type objectInfo struct {
	MIMEType     string
	Size         int64
	LastModified time.Time
}

type backend interface {
	Put(context.Context, string, string, int64, io.Reader) error
	Head(context.Context, string) (objectInfo, error)
	Open(context.Context, string) (Object, error)
	Delete(context.Context, string) error
}

type Store struct {
	backend       backend
	publicBaseURL string
	publicBase    *url.URL
	maxImageBytes int64
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
		maxImageBytes: MaxImageBytes, maxImageEdge: MaxImageEdge,
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
		return fmt.Errorf("open validated image: %w", err)
	}
	putErr := u.store.backend.Put(ctx, u.file.Filename, u.file.MIMEType, u.file.Size, payload)
	closeErr := payload.Close()
	if putErr != nil {
		if closeErr != nil {
			putErr = errors.Join(putErr, fmt.Errorf("close validated image: %w", closeErr))
		}
		return fmt.Errorf("store media: %w", putErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close validated image: %w", closeErr)
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

// Prepare validates and hashes an image without mutating the backing store.
// This split lets the application reserve per-tenant quota before S3 upload.
func (s *Store) Prepare(originalName string, reader io.Reader) (*Upload, error) {
	if reader == nil {
		return nil, errors.New("image reader is required")
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
	written, copyErr := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(reader, s.maxImageBytes+1))
	closeErr := tmp.Close()
	if copyErr != nil {
		resultErr := fmt.Errorf("save image: %w", copyErr)
		if closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close image: %w", closeErr))
		}
		return nil, resultErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close image: %w", closeErr)
	}
	if written == 0 {
		return nil, errors.New("image is empty")
	}
	if written > s.maxImageBytes {
		return nil, fmt.Errorf("image exceeds %d bytes", s.maxImageBytes)
	}

	// #nosec G703 -- tmpName is returned by os.CreateTemp and never includes user input.
	inspection, err := os.Open(tmpName)
	if err != nil {
		return nil, fmt.Errorf("inspect image: %w", err)
	}
	header := make([]byte, 512)
	headerN, readErr := io.ReadFull(inspection, header)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		resultErr := fmt.Errorf("inspect image header: %w", readErr)
		if closeErr := inspection.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close image inspection file: %w", closeErr))
		}
		return nil, resultErr
	}
	mimeType := http.DetectContentType(header[:headerN])
	if _, err := inspection.Seek(0, io.SeekStart); err != nil {
		_ = inspection.Close()
		return nil, fmt.Errorf("rewind image inspection file: %w", err)
	}
	config, _, decodeErr := image.DecodeConfig(inspection)
	inspectionCloseErr := inspection.Close()
	if decodeErr != nil {
		resultErr := errors.New("unsupported or invalid image; use PNG, JPEG or GIF")
		if inspectionCloseErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close image inspection file: %w", inspectionCloseErr))
		}
		return nil, resultErr
	}
	if inspectionCloseErr != nil {
		return nil, fmt.Errorf("close image inspection file: %w", inspectionCloseErr)
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > s.maxImageEdge || config.Height > s.maxImageEdge {
		return nil, fmt.Errorf("image dimensions must be between 1 and %d pixels per edge", s.maxImageEdge)
	}

	ext, ok := extensionForMIME(mimeType)
	if !ok {
		return nil, fmt.Errorf("unsupported image type %q; use PNG, JPEG or GIF", mimeType)
	}
	filename := hex.EncodeToString(hash.Sum(nil)) + ext
	_ = originalName // Persisted names are content-addressed to prevent path traversal.
	file := File{
		URL: s.URL(filename), Filename: filename, Path: filename, MIMEType: mimeType,
		Size: written, Width: config.Width, Height: config.Height,
	}
	keepTemp = true
	return &Upload{store: s, file: file, tempPath: tmpName}, nil
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
	return objectInfo{MIMEType: mime.TypeByExtension(filepath.Ext(key)), Size: info.Size(), LastModified: info.ModTime()}, nil
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
		Body: file, Filename: key, MIMEType: mime.TypeByExtension(filepath.Ext(key)),
		Size: info.Size(), LastModified: info.ModTime(),
	}, nil
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
