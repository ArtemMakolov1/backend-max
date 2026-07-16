package api

import (
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

func (s *Server) uploadWorkspacePostImage(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	if !access.Can(app.CapabilityMediaWrite) {
		s.problem(w, http.StatusForbidden, "workspace_forbidden", "Workspace media write access is required", nil)
		return
	}
	release, acquired := s.mediaUploads.tryAcquire(access.UserID)
	if !acquired {
		s.writeError(w, errMediaUploadRateLimited)
		return
	}
	defer release()
	r.Body = http.MaxBytesReader(w, r.Body, media.MaxImageBytes+(1<<20))
	// #nosec G120 -- MaxBytesReader bounds the entire multipart request before
	// ParseMultipartForm can allocate memory or spill parts to temporary files.
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		s.writeError(w, errors.New("invalid multipart upload or image is too large"))
		return
	}
	if r.MultipartForm != nil {
		defer func() { _ = r.MultipartForm.RemoveAll() }()
	}
	header, err := firstFile(r, "file", "image")
	if err != nil {
		s.writeError(w, err)
		return
	}
	upload, err := header.Open()
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer func() { _ = upload.Close() }()
	updated, err := s.app.SavePostImageForWorkspace(
		r.Context(), access.UserID, access.WorkspaceID, postID, header.Filename, upload)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, scopeWorkspacePostMedia(updated, access.WorkspaceID))
}

func (s *Server) uploadWorkspacePostAttachment(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	if !access.Can(app.CapabilityMediaWrite) {
		s.problem(w, http.StatusForbidden, "workspace_forbidden", "Workspace media write access is required", nil)
		return
	}
	post, err := s.app.Store().GetPostForWorkspace(r.Context(), access.UserID, access.WorkspaceID, postID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	release, acquired := s.mediaUploads.tryAcquire(access.UserID)
	if !acquired {
		s.observeAttachmentUpload("unknown", "busy", 0, 0)
		s.writeError(w, errMediaUploadRateLimited)
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
	file, err := openAndSaveWorkspaceAttachment(r, s, access.UserID, access.WorkspaceID, attachmentType, fileHeader)
	if err != nil {
		metricOutcome = attachmentUploadOutcome(err)
		s.writeError(w, err)
		return
	}
	metricSize = file.Size
	updated, err := s.app.Store().AddPostAttachmentForWorkspace(
		r.Context(), access.UserID, access.WorkspaceID, postID, attachmentFromMedia(file, position))
	if err != nil {
		metricOutcome = attachmentUploadOutcome(err)
		s.writeError(w, err)
		return
	}
	created, found := newestAttachmentByStorageKey(updated.Attachments, file.Path)
	if !found {
		s.writeError(w, errors.New("created attachment is missing from the updated post"))
		return
	}
	metricOutcome = "success"
	updated = scopeWorkspacePostMedia(updated, access.WorkspaceID)
	created = scopeWorkspaceAttachment(created, access.WorkspaceID)
	s.writeJSON(w, http.StatusCreated, attachmentMutationResponse{Attachment: created, Post: updated})
}

func (s *Server) replaceWorkspacePostAttachment(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	if !access.Can(app.CapabilityMediaWrite) {
		s.problem(w, http.StatusForbidden, "workspace_forbidden", "Workspace media write access is required", nil)
		return
	}
	attachmentID, err := parseAttachmentID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	post, err := s.app.Store().GetPostForWorkspace(r.Context(), access.UserID, access.WorkspaceID, postID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if !postHasAttachment(post, attachmentID) {
		s.writeError(w, store.ErrNotFound)
		return
	}
	release, acquired := s.mediaUploads.tryAcquire(access.UserID)
	if !acquired {
		s.observeAttachmentUpload("unknown", "busy", 0, 0)
		s.writeError(w, errMediaUploadRateLimited)
		return
	}
	defer release()
	startedAt := time.Now()
	metricType, metricSize, metricOutcome := "unknown", int64(0), "error"
	defer func() {
		s.observeAttachmentUpload(metricType, metricOutcome, metricSize, time.Since(startedAt))
	}()

	fileHeader, attachmentType, _, err := s.parseAttachmentMultipart(w, r, false)
	if err != nil {
		metricOutcome = attachmentUploadOutcome(err)
		s.writeError(w, err)
		return
	}
	metricType, metricSize = attachmentType, fileHeader.Size
	file, err := openAndSaveWorkspaceAttachment(r, s, access.UserID, access.WorkspaceID, attachmentType, fileHeader)
	if err != nil {
		metricOutcome = attachmentUploadOutcome(err)
		s.writeError(w, err)
		return
	}
	metricSize = file.Size
	updated, err := s.app.Store().ReplacePostAttachmentForWorkspace(
		r.Context(), access.UserID, access.WorkspaceID, postID, attachmentID, attachmentFromMedia(file, -1))
	if err != nil {
		metricOutcome = attachmentUploadOutcome(err)
		s.writeError(w, err)
		return
	}
	replaced, found := postAttachmentByID(updated.Attachments, attachmentID)
	if !found {
		s.writeError(w, errors.New("replaced attachment is missing from the updated post"))
		return
	}
	metricOutcome = "success"
	updated = scopeWorkspacePostMedia(updated, access.WorkspaceID)
	replaced = scopeWorkspaceAttachment(replaced, access.WorkspaceID)
	s.writeJSON(w, http.StatusOK, attachmentMutationResponse{Attachment: replaced, Post: updated})
}

func (s *Server) reorderWorkspacePostAttachments(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
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
	post, err := s.app.Store().ReorderPostAttachmentsForWorkspace(
		r.Context(), access.UserID, access.WorkspaceID, postID, orderedIDs)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, scopeWorkspacePostMedia(post, access.WorkspaceID))
}

func (s *Server) deleteWorkspacePostAttachment(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	attachmentID, err := parseAttachmentID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	post, err := s.app.Store().DeletePostAttachmentForWorkspace(
		r.Context(), access.UserID, access.WorkspaceID, postID, attachmentID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, scopeWorkspacePostMedia(post, access.WorkspaceID))
}

func openAndSaveWorkspaceAttachment(
	r *http.Request, s *Server, actorUserID, workspaceID, attachmentType string, header *multipart.FileHeader,
) (media.File, error) {
	if r.MultipartForm != nil {
		defer func() { _ = r.MultipartForm.RemoveAll() }()
	}
	upload, err := header.Open()
	if err != nil {
		return media.File{}, err
	}
	defer func() { _ = upload.Close() }()
	return s.app.SaveAttachmentMediaForWorkspace(
		r.Context(), actorUserID, workspaceID, attachmentType, header.Filename, upload)
}

func scopeWorkspaceAttachment(attachment store.PostAttachment, workspaceID string) store.PostAttachment {
	if attachment.StorageKey != "" {
		attachment.URL = workspaceMediaURL(workspaceID, attachment.StorageKey)
	}
	return attachment
}

func scopeWorkspacePostMedia(post store.Post, workspaceID string) store.Post {
	if post.ImagePath != "" {
		post.ImageURL = workspaceMediaURL(workspaceID, post.ImagePath)
	}
	post.Attachments = append([]store.PostAttachment(nil), post.Attachments...)
	for index := range post.Attachments {
		post.Attachments[index] = scopeWorkspaceAttachment(post.Attachments[index], workspaceID)
	}
	return post
}

func (s *Server) resolveWorkspaceImageURL(
	ctx context.Context, actorUserID, workspaceID, rawURL string,
) (string, error) {
	if strings.TrimSpace(rawURL) == "" {
		return "", nil
	}
	filename, err := s.app.Media().FilenameFromURL(rawURL)
	resolvedURL := rawURL
	if err != nil {
		parsed, parseErr := url.Parse(rawURL)
		if parseErr != nil {
			return "", errors.New("invalid image URL")
		}
		prefix := "/api/v1/workspaces/" + url.PathEscape(workspaceID) + "/media/"
		escapedPath := parsed.EscapedPath()
		if !strings.HasPrefix(escapedPath, prefix) {
			return "", errors.New("image URL is not a local workspace media URL")
		}
		escapedFilename := strings.TrimPrefix(escapedPath, prefix)
		if escapedFilename == "" || strings.Contains(escapedFilename, "/") {
			return "", errors.New("invalid workspace media filename")
		}
		decodedFilename, unescapeErr := url.PathUnescape(escapedFilename)
		if unescapeErr != nil {
			return "", errors.New("invalid workspace media filename")
		}
		parsed.Path, parsed.RawPath = "/media/"+decodedFilename, ""
		resolvedURL = parsed.String()
		filename, err = s.app.Media().FilenameFromURL(resolvedURL)
		if err != nil {
			return "", err
		}
	}
	owned, err := s.app.Store().WorkspaceOwnsMedia(ctx, actorUserID, workspaceID, filename)
	if err != nil {
		return "", err
	}
	if !owned {
		return "", store.ErrNotFound
	}
	return s.app.Media().ResolveURL(ctx, resolvedURL)
}
