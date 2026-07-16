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
	userID, err := authenticatedUserID(r)
	if err != nil {
		return store.Post{}, media.File{}, err
	}
	releaseUpload, acquired := s.mediaUploads.tryAcquire(userID)
	if !acquired {
		return store.Post{}, media.File{}, errMediaUploadRateLimited
	}
	defer releaseUpload()
	// Route and query post IDs are known before multipart parsing. Authorize
	// them first so a foreign resource consistently looks absent regardless of
	// whether the request body is valid.
	if postID != nil {
		if _, err := s.app.Store().GetPostForUser(r.Context(), userID, *postID); err != nil {
			return store.Post{}, media.File{}, err
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, media.MaxImageBytes+(1<<20))
	// #nosec G120 -- MaxBytesReader above bounds the entire multipart request before ParseMultipartForm allocates memory or temporary files.
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
	if postID != nil {
		if _, err := s.app.Store().GetPostForUser(r.Context(), userID, *postID); err != nil {
			return store.Post{}, media.File{}, err
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
	file, err := s.app.SaveMediaForUser(r.Context(), userID, fileHeader.Filename, upload)
	if err != nil {
		return store.Post{}, media.File{}, err
	}
	if postID == nil {
		return store.Post{}, file, nil
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
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	filename := chi.URLParam(r, "filename")
	owned, err := s.app.Store().UserOwnsMedia(r.Context(), userID, filename)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if !owned {
		s.problem(w, http.StatusNotFound, "not_found", "Media file was not found", nil)
		return
	}
	object, err := s.app.Media().Open(r.Context(), filename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.problem(w, http.StatusNotFound, "not_found", "Media file was not found", nil)
			return
		}
		s.logger.Error("could not load private media", "filename", filename, "error", err)
		s.problem(w, http.StatusInternalServerError, "internal_error", "Could not load image", nil)
		return
	}
	defer func() {
		_ = object.Body.Close()
	}()
	mimeType := strings.TrimSpace(object.MIMEType)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mimeType)
	if object.Size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(object.Size, 10))
	}
	// The URL is tenant-scoped by the session, so it must not survive logout or
	// be replayed from a browser/CDN cache under another account.
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, object.Body); err != nil {
		s.logger.Warn("private media response interrupted", "filename", filename, "error", err)
	}
}
