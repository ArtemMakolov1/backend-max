package media

import (
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
)

const (
	MaxImageBytes = 50 << 20
	MaxImageEdge  = 7680
)

type File struct {
	URL      string `json:"url"`
	Filename string `json:"filename"`
	Path     string `json:"-"`
	MIMEType string `json:"mime_type"`
	Size     int64  `json:"size"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

type Store struct {
	dir           string
	publicBaseURL string
	publicBase    *url.URL
	maxImageBytes int64
	maxImageEdge  int
}

func New(dir, publicBaseURL string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("media directory is required")
	}
	base, err := url.Parse(strings.TrimRight(publicBaseURL, "/"))
	if err != nil || !base.IsAbs() || base.Host == "" {
		return nil, errors.New("public base URL must be an absolute URL")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve media directory: %w", err)
	}
	if err := os.MkdirAll(absDir, 0o750); err != nil {
		return nil, fmt.Errorf("create media directory: %w", err)
	}
	return &Store{
		dir: absDir, publicBaseURL: strings.TrimRight(publicBaseURL, "/"), publicBase: base,
		maxImageBytes: MaxImageBytes, maxImageEdge: MaxImageEdge,
	}, nil
}

func (s *Store) Save(originalName string, reader io.Reader) (File, error) {
	if reader == nil {
		return File{}, errors.New("image reader is required")
	}
	tmp, err := os.CreateTemp(s.dir, ".upload-*")
	if err != nil {
		return File{}, fmt.Errorf("create media temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()

	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(reader, s.maxImageBytes+1))
	closeErr := tmp.Close()
	if copyErr != nil {
		resultErr := fmt.Errorf("save image: %w", copyErr)
		if closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close image: %w", closeErr))
		}
		return File{}, resultErr
	}
	if closeErr != nil {
		return File{}, fmt.Errorf("close image: %w", closeErr)
	}
	if written == 0 {
		return File{}, errors.New("image is empty")
	}
	if written > s.maxImageBytes {
		return File{}, fmt.Errorf("image exceeds %d bytes", s.maxImageBytes)
	}

	// #nosec G703 -- tmpName is returned by os.CreateTemp inside the configured private media directory.
	file, err := os.Open(tmpName)
	if err != nil {
		return File{}, fmt.Errorf("inspect image: %w", err)
	}
	header := make([]byte, 512)
	headerN, readErr := io.ReadFull(file, header)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		resultErr := fmt.Errorf("inspect image header: %w", readErr)
		if closeErr := file.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close image inspection file: %w", closeErr))
		}
		return File{}, resultErr
	}
	mimeType := http.DetectContentType(header[:headerN])
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		resultErr := err
		if closeErr := file.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close image inspection file: %w", closeErr))
		}
		return File{}, resultErr
	}
	config, _, err := image.DecodeConfig(file)
	fileCloseErr := file.Close()
	if err != nil {
		resultErr := errors.New("unsupported or invalid image; use PNG, JPEG or GIF")
		if fileCloseErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close image inspection file: %w", fileCloseErr))
		}
		return File{}, resultErr
	}
	if fileCloseErr != nil {
		return File{}, fmt.Errorf("close image inspection file: %w", fileCloseErr)
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > s.maxImageEdge || config.Height > s.maxImageEdge {
		return File{}, fmt.Errorf("image dimensions must be between 1 and %d pixels per edge", s.maxImageEdge)
	}

	ext, ok := extensionForMIME(mimeType)
	if !ok {
		return File{}, fmt.Errorf("unsupported image type %q; use PNG, JPEG or GIF", mimeType)
	}
	filename := hex.EncodeToString(hash.Sum(nil)) + ext
	destination := filepath.Join(s.dir, filename)
	if _, err := os.Stat(destination); errors.Is(err, os.ErrNotExist) {
		// #nosec G703 -- tmpName is returned by os.CreateTemp inside the configured private media directory.
		if err := os.Chmod(tmpName, 0o600); err != nil {
			return File{}, fmt.Errorf("set media permissions: %w", err)
		}
		// #nosec G703 -- source is a CreateTemp path and destination is a SHA-256 filename joined to the private media directory.
		if err := os.Rename(tmpName, destination); err != nil {
			return File{}, fmt.Errorf("store media: %w", err)
		}
	} else if err != nil {
		return File{}, fmt.Errorf("check media destination: %w", err)
	}

	_ = originalName // Kept out of the persisted filename to prevent path traversal.
	return File{
		URL: s.URL(filename), Filename: filename, Path: destination, MIMEType: mimeType,
		Size: written, Width: config.Width, Height: config.Height,
	}, nil
}

func (s *Store) URL(filename string) string {
	return s.publicBaseURL + "/media/" + url.PathEscape(filename)
}

func (s *Store) ResolveURL(rawURL string) (string, error) {
	filename, err := s.FilenameFromURL(rawURL)
	if err != nil {
		return "", err
	}
	fullPath := filepath.Join(s.dir, filename)
	info, err := os.Stat(fullPath)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("media file does not exist")
	}
	return fullPath, nil
}

// FilenameFromURL validates that rawURL refers to this media store without
// touching the filesystem. This lets callers enforce tenant ownership before
// revealing whether the underlying private file exists.
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
	if err != nil || filename == "" || filename != path.Base(filename) || strings.ContainsAny(filename, `/\\`) {
		return "", errors.New("invalid media filename")
	}
	return filename, nil
}

func (s *Store) Open(filename string) (*os.File, os.FileInfo, error) {
	decoded, err := url.PathUnescape(filename)
	if err != nil || decoded == "" || decoded != filepath.Base(decoded) || strings.ContainsAny(decoded, `/\\`) {
		return nil, nil, errors.New("invalid media filename")
	}
	file, err := os.Open(filepath.Join(s.dir, decoded))
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = errors.New("not a regular media file")
		}
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close media file: %w", closeErr))
		}
		return nil, nil, err
	}
	return file, info, nil
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
