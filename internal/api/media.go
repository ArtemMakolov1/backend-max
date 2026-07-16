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

	"maxpilot/backend/internal/app"
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
	if postID != nil {
		// Keep the historical /image and /media?post_id= APIs as a projection
		// of the new gallery model. Replacing the first image must not remove
		// videos or other gallery entries.
		post, err := s.app.SavePostImageForUser(r.Context(), userID, *postID, fileHeader.Filename, upload)
		return post, media.File{}, err
	}
	file, err := s.app.SaveMediaForUser(r.Context(), userID, fileHeader.Filename, upload)
	return store.Post{}, file, err
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
	s.serveAuthorizedMedia(w, r, filename)
}

func (s *Server) serveWorkspaceMedia(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityMediaRead)
	if !ok {
		return
	}
	filename := chi.URLParam(r, "filename")
	owned, err := s.app.Store().WorkspaceOwnsMedia(r.Context(), access.UserID, access.WorkspaceID, filename)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if !owned {
		s.problem(w, http.StatusNotFound, "not_found", "Media file was not found", nil)
		return
	}
	s.serveAuthorizedMedia(w, r, filename)
}

func (s *Server) serveAuthorizedMedia(w http.ResponseWriter, r *http.Request, filename string) {
	var object media.Object
	var err error
	status := http.StatusOK
	var requestedRange mediaByteRange
	var rangeTotal int64
	rawRange := strings.TrimSpace(r.Header.Get("Range"))
	if rawRange != "" {
		info, infoErr := s.app.Media().Info(r.Context(), filename)
		if infoErr != nil {
			s.writeMediaReadError(w, filename, infoErr)
			return
		}
		byteRange, rangeErr := parseMediaRange(rawRange, info.Size)
		if rangeErr != nil {
			w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(info.Size, 10))
			s.problem(w, http.StatusRequestedRangeNotSatisfiable, "range_not_satisfiable", "Requested media range is not available", nil)
			return
		}
		requestedRange = byteRange
		rangeTotal = info.Size
		object, err = s.app.Media().OpenRange(r.Context(), filename, byteRange.start, byteRange.end)
		status = http.StatusPartialContent
	} else {
		object, err = s.app.Media().Open(r.Context(), filename)
	}
	if err != nil {
		if errors.Is(err, media.ErrRangeNotSatisfiable) {
			w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(rangeTotal, 10))
			s.problem(w, http.StatusRequestedRangeNotSatisfiable, "range_not_satisfiable", "Requested media range is not available", nil)
			return
		}
		s.writeMediaReadError(w, filename, err)
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
	w.Header().Set("Accept-Ranges", "bytes")
	if object.Size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(object.Size, 10))
	}
	if status == http.StatusPartialContent {
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(requestedRange.start, 10)+"-"+
			strconv.FormatInt(requestedRange.start+object.Size-1, 10)+"/"+strconv.FormatInt(object.TotalSize, 10))
	}
	// The URL is tenant-scoped by the session, so it must not survive logout or
	// be replayed from a browser/CDN cache under another account.
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if _, err := io.Copy(w, object.Body); err != nil {
		s.logger.Warn("private media response interrupted", "filename", filename, "error", err)
	}
}

func (s *Server) writeMediaReadError(w http.ResponseWriter, filename string, err error) {
	if errors.Is(err, os.ErrNotExist) {
		s.problem(w, http.StatusNotFound, "not_found", "Media file was not found", nil)
		return
	}
	s.logger.Error("could not load private media", "filename", filename, "error", err)
	s.problem(w, http.StatusInternalServerError, "internal_error", "Could not load media", nil)
}

type mediaByteRange struct {
	start int64
	end   int64
}

func parseMediaRange(raw string, size int64) (mediaByteRange, error) {
	if size <= 0 || !strings.HasPrefix(strings.ToLower(raw), "bytes=") {
		return mediaByteRange{}, media.ErrRangeNotSatisfiable
	}
	spec := strings.TrimSpace(raw[len("bytes="):])
	if spec == "" || strings.Contains(spec, ",") {
		return mediaByteRange{}, media.ErrRangeNotSatisfiable
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return mediaByteRange{}, media.ErrRangeNotSatisfiable
	}
	if strings.TrimSpace(parts[0]) == "" {
		suffix, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil || suffix <= 0 {
			return mediaByteRange{}, media.ErrRangeNotSatisfiable
		}
		if suffix > size {
			suffix = size
		}
		return mediaByteRange{start: size - suffix, end: size - 1}, nil
	}
	start, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil || start < 0 || start >= size {
		return mediaByteRange{}, media.ErrRangeNotSatisfiable
	}
	end := size - 1
	if strings.TrimSpace(parts[1]) != "" {
		end, err = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil || end < start {
			return mediaByteRange{}, media.ErrRangeNotSatisfiable
		}
		if end >= size {
			end = size - 1
		}
	}
	return mediaByteRange{start: start, end: end}, nil
}
