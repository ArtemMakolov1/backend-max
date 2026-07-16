package api

import (
	"context"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

const attachmentMultipartOverhead = 2 << 20

var (
	errAttachmentUnsupported = errors.New("unsupported attachment")
	errAttachmentTooLarge    = errors.New("attachment is too large")
)

type attachmentOrderRequest struct {
	AttachmentIDs        []int64 `json:"attachment_ids"`
	OrderedAttachmentIDs []int64 `json:"ordered_attachment_ids,omitempty"`
}

type attachmentMutationResponse struct {
	Attachment store.PostAttachment `json:"attachment"`
	Post       store.Post           `json:"post"`
}

func (s *Server) uploadPostAttachment(w http.ResponseWriter, r *http.Request) {
	userID, postID, post, release, ok := s.prepareAttachmentUpload(w, r, 0)
	if !ok {
		return
	}
	defer release()
	startedAt := time.Now()
	metricType, metricSize, metricOutcome := "unknown", int64(0), "error"
	defer func() {
		s.observeAttachmentUpload(metricType, metricOutcome, metricSize, time.Since(startedAt))
	}()

	limit := store.MaxPostAttachments
	if len(post.LinkButtons) > 0 {
		limit = store.MaxPostAttachmentsWithKeyboard
	}
	if len(post.Attachments) >= limit {
		s.writeError(w, fmtAttachmentLimit(limit))
		return
	}

	fileHeader, attachmentType, position, err := s.parseAttachmentMultipart(w, r, true)
	if err != nil {
		metricOutcome = attachmentUploadOutcome(err)
		s.writeError(w, err)
		return
	}
	metricType, metricSize = attachmentType, fileHeader.Size
	file, err := openAndSaveAttachment(r, s, userID, attachmentType, fileHeader)
	if err != nil {
		metricOutcome = attachmentUploadOutcome(err)
		s.writeError(w, err)
		return
	}
	metricSize = file.Size
	updated, err := s.app.Store().AddPostAttachmentForUser(r.Context(), userID, postID, attachmentFromMedia(file, position))
	if err != nil {
		metricOutcome = attachmentUploadOutcome(err)
		s.writeError(w, err)
		return
	}
	created, ok := newestAttachmentByStorageKey(updated.Attachments, file.Path)
	if !ok {
		s.writeError(w, errors.New("created attachment is missing from the updated post"))
		return
	}
	metricOutcome = "success"
	s.writeJSON(w, http.StatusCreated, attachmentMutationResponse{Attachment: created, Post: updated})
}

func (s *Server) replacePostAttachment(w http.ResponseWriter, r *http.Request) {
	attachmentID, err := parseAttachmentID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	userID, postID, post, release, ok := s.prepareAttachmentUpload(w, r, attachmentID)
	if !ok {
		return
	}
	defer release()
	startedAt := time.Now()
	metricType, metricSize, metricOutcome := "unknown", int64(0), "error"
	defer func() {
		s.observeAttachmentUpload(metricType, metricOutcome, metricSize, time.Since(startedAt))
	}()
	if !postHasAttachment(post, attachmentID) {
		s.writeError(w, store.ErrNotFound)
		return
	}

	fileHeader, attachmentType, _, err := s.parseAttachmentMultipart(w, r, false)
	if err != nil {
		metricOutcome = attachmentUploadOutcome(err)
		s.writeError(w, err)
		return
	}
	metricType, metricSize = attachmentType, fileHeader.Size
	file, err := openAndSaveAttachment(r, s, userID, attachmentType, fileHeader)
	if err != nil {
		metricOutcome = attachmentUploadOutcome(err)
		s.writeError(w, err)
		return
	}
	metricSize = file.Size
	updated, err := s.app.Store().ReplacePostAttachmentForUser(r.Context(), userID, postID, attachmentID, attachmentFromMedia(file, -1))
	if err != nil {
		metricOutcome = attachmentUploadOutcome(err)
		s.writeError(w, err)
		return
	}
	replaced, ok := postAttachmentByID(updated.Attachments, attachmentID)
	if !ok {
		s.writeError(w, errors.New("replaced attachment is missing from the updated post"))
		return
	}
	metricOutcome = "success"
	s.writeJSON(w, http.StatusOK, attachmentMutationResponse{Attachment: replaced, Post: updated})
}

func (s *Server) reorderPostAttachments(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	postID, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	var request attachmentOrderRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	orderedIDs := request.AttachmentIDs
	if len(orderedIDs) == 0 {
		orderedIDs = request.OrderedAttachmentIDs
	}
	if orderedIDs == nil {
		s.writeError(w, errors.New("attachment_ids is required"))
		return
	}
	updated, err := s.app.Store().ReorderPostAttachmentsForUser(r.Context(), userID, postID, orderedIDs)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deletePostAttachment(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	postID, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	attachmentID, err := parseAttachmentID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	updated, err := s.app.Store().DeletePostAttachmentForUser(r.Context(), userID, postID, attachmentID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, updated)
}

func (s *Server) prepareAttachmentUpload(w http.ResponseWriter, r *http.Request, _ int64) (string, int64, store.Post, func(), bool) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return "", 0, store.Post{}, nil, false
	}
	postID, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return "", 0, store.Post{}, nil, false
	}
	// Authorize the resource before parsing a potentially large body. A post
	// owned by another tenant remains indistinguishable from a missing post.
	post, err := s.app.Store().GetPostForUser(r.Context(), userID, postID)
	if err != nil {
		s.writeError(w, err)
		return "", 0, store.Post{}, nil, false
	}
	release, acquired := s.mediaUploads.tryAcquire(userID)
	if !acquired {
		s.observeAttachmentUpload("unknown", "busy", 0, 0)
		s.writeError(w, errMediaUploadRateLimited)
		return "", 0, store.Post{}, nil, false
	}
	return userID, postID, post, release, true
}

func (s *Server) parseAttachmentMultipart(w http.ResponseWriter, r *http.Request, allowPosition bool) (*multipart.FileHeader, string, int, error) {
	r.Body = http.MaxBytesReader(w, r.Body, media.MaxVideoBytes+attachmentMultipartOverhead)
	// #nosec G120 -- MaxBytesReader bounds the whole multipart request. Large
	// parts spill to a temporary file instead of staying in process memory.
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return nil, "", 0, fmt.Errorf("%w: maximum video size is %d bytes", errAttachmentTooLarge, media.MaxVideoBytes)
		}
		return nil, "", 0, fmt.Errorf("invalid multipart upload: %w", err)
	}
	fileHeader, err := firstFile(r, "file", "attachment", "image", "video")
	if err != nil {
		_ = r.MultipartForm.RemoveAll()
		return nil, "", 0, err
	}
	attachmentType, err := requestedAttachmentType(r.FormValue("type"), fileHeader.Filename)
	if err != nil {
		_ = r.MultipartForm.RemoveAll()
		return nil, "", 0, err
	}
	position := -1
	if allowPosition && strings.TrimSpace(r.FormValue("position")) != "" {
		position, err = strconv.Atoi(strings.TrimSpace(r.FormValue("position")))
		if err != nil || position < 0 || position >= store.MaxPostAttachments {
			_ = r.MultipartForm.RemoveAll()
			return nil, "", 0, errors.New("position must be an integer between 0 and 11")
		}
	}
	return fileHeader, attachmentType, position, nil
}

func openAndSaveAttachment(r *http.Request, s *Server, userID, attachmentType string, header *multipart.FileHeader) (media.File, error) {
	if r.MultipartForm != nil {
		defer func() { _ = r.MultipartForm.RemoveAll() }()
	}
	upload, err := header.Open()
	if err != nil {
		return media.File{}, err
	}
	defer func() { _ = upload.Close() }()
	return s.app.SaveAttachmentMediaForUser(r.Context(), userID, attachmentType, header.Filename, upload)
}

func requestedAttachmentType(raw, filename string) (string, error) {
	attachmentType := strings.ToLower(strings.TrimSpace(raw))
	if attachmentType != "" {
		if attachmentType != media.AttachmentTypeImage && attachmentType != media.AttachmentTypeVideo {
			return "", fmt.Errorf("%w: attachment type must be image or video", errAttachmentUnsupported)
		}
		return attachmentType, nil
	}
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".png", ".jpg", ".jpeg", ".gif":
		return media.AttachmentTypeImage, nil
	case ".mp4", ".mov", ".mkv", ".webm":
		return media.AttachmentTypeVideo, nil
	default:
		return "", fmt.Errorf("%w: use PNG, JPEG, GIF, MP4, MOV, MKV or WEBM", errAttachmentUnsupported)
	}
}

func (s *Server) observeAttachmentUpload(attachmentType, outcome string, sizeBytes int64, elapsed time.Duration) {
	if s.metrics != nil {
		s.metrics.ObserveAttachmentUpload(attachmentType, outcome, sizeBytes, elapsed)
	}
}

func attachmentUploadOutcome(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	case errors.Is(err, store.ErrMediaQuotaExceeded):
		return "quota_exceeded"
	case errors.Is(err, store.ErrMediaUploadBusy), errors.Is(err, errMediaUploadRateLimited):
		return "busy"
	case errors.Is(err, errAttachmentTooLarge), strings.Contains(strings.ToLower(err.Error()), "exceeds"),
		strings.Contains(strings.ToLower(err.Error()), "too large"):
		return "too_large"
	case errors.Is(err, errAttachmentUnsupported), strings.Contains(strings.ToLower(err.Error()), "unsupported"):
		return "unsupported"
	default:
		return "error"
	}
}

func attachmentFromMedia(file media.File, position int) store.PostAttachment {
	attachment := store.PostAttachment{
		Type: file.Type, Position: position, StorageKey: file.Path,
		ProcessingStatus: store.AttachmentStatusReady, SizeBytes: file.Size, MIMEType: file.MIMEType,
	}
	if file.Width > 0 {
		width := file.Width
		attachment.Width = &width
	}
	if file.Height > 0 {
		height := file.Height
		attachment.Height = &height
	}
	if file.DurationMS > 0 {
		duration := file.DurationMS
		attachment.DurationMS = &duration
	}
	return attachment
}

func parseAttachmentID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, "attachment_id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("attachment_id must be a positive integer")
	}
	return id, nil
}

func postHasAttachment(post store.Post, attachmentID int64) bool {
	_, ok := postAttachmentByID(post.Attachments, attachmentID)
	return ok
}

func postAttachmentByID(attachments []store.PostAttachment, attachmentID int64) (store.PostAttachment, bool) {
	for _, attachment := range attachments {
		if attachment.ID == attachmentID {
			return attachment, true
		}
	}
	return store.PostAttachment{}, false
}

// Duplicate content intentionally reuses one S3 object, so storage_key is not
// unique within a gallery. AddPostAttachment allocates a new row id; choosing
// the greatest matching id returns that row even when the same image is added
// more than once or inserted before an older attachment.
func newestAttachmentByStorageKey(attachments []store.PostAttachment, storageKey string) (store.PostAttachment, bool) {
	var newest store.PostAttachment
	found := false
	for _, attachment := range attachments {
		if attachment.StorageKey == storageKey && (!found || attachment.ID > newest.ID) {
			newest, found = attachment, true
		}
	}
	return newest, found
}

func fmtAttachmentLimit(limit int) error {
	return errors.New("attachment count must not exceed " + strconv.Itoa(limit))
}
