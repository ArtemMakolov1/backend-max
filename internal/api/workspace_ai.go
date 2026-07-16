package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/openaiimg"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

func (s *Server) generateWorkspaceImage(w http.ResponseWriter, r *http.Request) {
	workspace, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityAIUse)
	if !ok {
		return
	}
	if !access.Can(app.CapabilityMediaWrite) {
		s.problem(w, http.StatusForbidden, "workspace_forbidden", "Workspace media write access is required", nil)
		return
	}
	var request openaiimg.GenerateRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	if err := s.app.ValidateImageRequest(request); err != nil {
		s.writeError(w, err)
		return
	}
	release, err := s.aiLimiter.acquireForWorkspaceAmount(
		r.Context(), access.UserID, workspace, store.AIOperationImage,
		imageUsageCredits(request.Quality), s.now().UTC())
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer release()
	ctx, cancel := contextWithTimeout(r, AIHandlerTimeout)
	defer cancel()
	file, err := s.app.GenerateImageForWorkspace(ctx, access.UserID, access.WorkspaceID, request)
	if err != nil {
		s.writeError(w, err)
		return
	}
	file.URL = workspaceMediaURL(access.WorkspaceID, file.Filename)
	s.writeJSON(w, http.StatusCreated, file)
}

func (s *Server) generateWorkspacePostImage(w http.ResponseWriter, r *http.Request) {
	workspace, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	if !access.Can(app.CapabilityAIUse) || !access.Can(app.CapabilityMediaWrite) {
		s.problem(w, http.StatusForbidden, "workspace_forbidden", "Workspace AI and media write access are required", nil)
		return
	}
	post, err := s.app.Store().GetPostForWorkspace(r.Context(), access.UserID, access.WorkspaceID, postID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	var request openaiimg.GenerateRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.Prompt) == "" {
		request.Prompt = post.ImagePrompt
	}
	if err := s.app.ValidateImageRequest(request); err != nil {
		s.writeError(w, err)
		return
	}
	release, err := s.aiLimiter.acquireForWorkspaceAmount(
		r.Context(), access.UserID, workspace, store.AIOperationImage,
		imageUsageCredits(request.Quality), s.now().UTC())
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer release()
	ctx, cancel := contextWithTimeout(r, AIHandlerTimeout)
	defer cancel()
	updated, err := s.app.GeneratePostImageForWorkspace(ctx, access.UserID, access.WorkspaceID, postID, request)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, scopeWorkspacePostMedia(updated, access.WorkspaceID))
}

func (s *Server) uploadWorkspaceMedia(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityMediaWrite)
	if !ok {
		return
	}
	releaseUpload, acquired := s.mediaUploads.tryAcquire(access.UserID)
	if !acquired {
		s.writeError(w, errMediaUploadRateLimited)
		return
	}
	defer releaseUpload()
	r.Body = http.MaxBytesReader(w, r.Body, media.MaxImageBytes+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		s.writeError(w, errors.New("invalid multipart upload or image is too large"))
		return
	}
	if r.MultipartForm != nil {
		defer func() { _ = r.MultipartForm.RemoveAll() }()
	}
	fileHeader, err := firstFile(r, "file", "image")
	if err != nil {
		s.writeError(w, err)
		return
	}
	upload, err := fileHeader.Open()
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer func() { _ = upload.Close() }()
	file, err := s.app.SaveAttachmentMediaForWorkspace(r.Context(), access.UserID, access.WorkspaceID,
		media.AttachmentTypeImage, strings.TrimSpace(fileHeader.Filename), upload)
	if err != nil {
		s.writeError(w, err)
		return
	}
	file.URL = workspaceMediaURL(access.WorkspaceID, file.Filename)
	s.writeJSON(w, http.StatusCreated, file)
}

func workspaceMediaURL(workspaceID, filename string) string {
	return "/api/v1/workspaces/" + url.PathEscape(workspaceID) + "/media/" + url.PathEscape(filename)
}

func (s *Server) generateWorkspaceResearch(w http.ResponseWriter, r *http.Request) {
	workspace, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityAIUse)
	if !ok {
		return
	}
	request, ok := s.decodeBrandedWorkspaceResearchRequest(w, r, access)
	if !ok {
		return
	}
	if !s.app.ResearchConfigured() {
		s.writeError(w, app.ErrResearchNotConfigured)
		return
	}
	release, err := s.aiLimiter.acquireForWorkspace(
		r.Context(), access.UserID, workspace, store.AIOperationResearch, s.now().UTC())
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer release()
	ctx, cancel := contextWithTimeout(r, AIHandlerTimeout)
	defer cancel()
	result, err := s.app.GenerateResearch(ctx, request)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) formatWorkspacePostContent(w http.ResponseWriter, r *http.Request) {
	workspace, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityAIUse)
	if !ok {
		return
	}
	var request openairesearch.FormatRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	if err := openairesearch.ValidateFormatRequest(request); err != nil {
		s.writeError(w, err)
		return
	}
	if !s.app.ContentFormattingConfigured() {
		s.writeError(w, app.ErrResearchNotConfigured)
		return
	}
	release, err := s.aiLimiter.acquireForWorkspaceMetric(
		r.Context(), access.UserID, workspace, store.AIOperationResearch,
		store.UsageMetricAIFormatRequests, 1, s.now().UTC())
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer release()
	ctx, cancel := contextWithTimeout(r, AIHandlerTimeout)
	defer cancel()
	result, err := s.app.FormatPostContent(ctx, request)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}
