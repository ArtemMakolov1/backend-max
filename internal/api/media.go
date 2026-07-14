package api

import (
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

func (s *Server) uploadPostImage(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	post, _, err := s.saveMultipartImage(w, r, &id)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, post)
}

func (s *Server) uploadMedia(w http.ResponseWriter, r *http.Request) {
	var postID *int64
	if raw := r.URL.Query().Get("post_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			s.problem(w, http.StatusBadRequest, "validation_error", "post_id must be a positive integer", nil)
			return
		}
		postID = &id
	}
	post, file, err := s.saveMultipartImage(w, r, postID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if postID != nil {
		s.writeJSON(w, http.StatusOK, post)
		return
	}
	s.writeJSON(w, http.StatusCreated, file)
}

func (s *Server) saveMultipartImage(w http.ResponseWriter, r *http.Request, postID *int64) (store.Post, media.File, error) {
	r.Body = http.MaxBytesReader(w, r.Body, media.MaxImageBytes+(1<<20))
	//nolint:gosec // G120: MaxBytesReader caps the entire request body before multipart parsing.
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		return store.Post{}, media.File{}, errors.New("invalid multipart upload or image is too large")
	}
	if r.MultipartForm != nil {
		defer func() {
			_ = r.MultipartForm.RemoveAll()
		}()
	}
	if postID == nil {
		if raw := strings.TrimSpace(r.FormValue("post_id")); raw != "" {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || id <= 0 {
				return store.Post{}, media.File{}, errors.New("post_id must be a positive integer")
			}
			postID = &id
		}
	}
	fileHeader, err := firstFile(r, "file", "image")
	if err != nil {
		return store.Post{}, media.File{}, err
	}
	upload, err := fileHeader.Open()
	if err != nil {
		return store.Post{}, media.File{}, err
	}
	defer func() {
		_ = upload.Close()
	}()
	file, err := s.app.Media().Save(fileHeader.Filename, upload)
	if err != nil {
		return store.Post{}, media.File{}, err
	}
	if postID == nil {
		return store.Post{}, file, nil
	}
	if _, err := s.app.Store().GetPost(r.Context(), *postID); err != nil {
		return store.Post{}, media.File{}, err
	}
	emptyPrompt := ""
	post, err := s.app.Store().UpdatePost(r.Context(), *postID, store.PostChanges{
		ImageURL: &file.URL, ImagePath: &file.Path, ImagePrompt: &emptyPrompt,
	})
	return post, file, err
}

func firstFile(r *http.Request, fieldNames ...string) (*multipart.FileHeader, error) {
	for _, field := range fieldNames {
		if files := r.MultipartForm.File[field]; len(files) > 0 {
			return files[0], nil
		}
	}
	return nil, errors.New("multipart field file is required")
}

func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request) {
	file, info, err := s.app.Media().Open(chi.URLParam(r, "filename"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.problem(w, http.StatusNotFound, "not_found", "Media file was not found", nil)
			return
		}
		s.problem(w, http.StatusBadRequest, "invalid_media_path", err.Error(), nil)
		return
	}
	defer func() {
		_ = file.Close()
	}()
	header := make([]byte, 512)
	n, _ := io.ReadFull(file, header)
	_, _ = file.Seek(0, io.SeekStart)
	w.Header().Set("Content-Type", http.DetectContentType(header[:n]))
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}
