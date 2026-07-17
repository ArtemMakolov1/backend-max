package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/store"
)

func (s *Server) listWorkspacePosts(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityPostsRead)
	if !ok {
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status != "" && !validPostStatus(status) {
		s.problem(w, http.StatusBadRequest, "validation_error", "Unknown post status", nil)
		return
	}
	var channelID *int64
	if raw := strings.TrimSpace(r.URL.Query().Get("channel_id")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			s.problem(w, http.StatusBadRequest, "validation_error", "channel_id must be a positive integer", nil)
			return
		}
		channelID = &parsed
		if _, err := s.app.Store().GetChannelForWorkspace(r.Context(), access.UserID, access.WorkspaceID, parsed); err != nil {
			s.writeError(w, err)
			return
		}
	}
	posts, err := s.app.Store().ListPostsForWorkspace(r.Context(), access.UserID, access.WorkspaceID, status, channelID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	for index := range posts {
		posts[index] = scopeWorkspacePostMedia(posts[index], access.WorkspaceID)
	}
	s.writeJSON(w, http.StatusOK, posts)
}

func (s *Server) createWorkspacePost(w http.ResponseWriter, r *http.Request) {
	workspace, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	var request createPostRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	if request.Format == "" {
		request.Format = store.FormatMarkdown
	}
	if err := validatePostFields(request.Title, request.Content, request.Format); err != nil {
		s.writeError(w, err)
		return
	}
	if err := store.ValidateLinkButtonsDraft(request.LinkButtons); err != nil {
		s.writeError(w, err)
		return
	}
	if request.ChannelID != nil {
		if *request.ChannelID <= 0 {
			s.problem(w, http.StatusBadRequest, "validation_error", "channel_id must be positive", nil)
			return
		}
		if _, err := s.app.Store().GetChannelForWorkspace(
			r.Context(), access.UserID, access.WorkspaceID, *request.ChannelID); err != nil {
			s.writeError(w, err)
			return
		}
	}
	imagePath, err := s.resolveWorkspaceImageURL(
		r.Context(), access.UserID, access.WorkspaceID, request.ImageURL)
	if err != nil {
		s.writeError(w, err)
		return
	}
	post := store.Post{
		UserID: access.UserID, WorkspaceID: access.WorkspaceID,
		Title: strings.TrimSpace(request.Title), Content: request.Content, Format: request.Format,
		Status: store.PostStatusDraft, ChannelID: request.ChannelID, ImageURL: request.ImageURL,
		ImagePath: imagePath, ImagePrompt: request.ImagePrompt, Notify: true,
		LinkButtons: request.LinkButtons, DisableLinkPreview: request.DisableLinkPreview,
	}
	if request.Notify != nil {
		post.Notify = true // MAX channel publications always notify.
	}
	if len(request.ScheduledAt) > 0 {
		scheduledAt, err := decodeScheduledAt(request.ScheduledAt)
		if err != nil {
			s.writeError(w, err)
			return
		}
		if scheduledAt != nil {
			if workspace.ApprovalRequired {
				s.writeError(w, app.ErrApprovalRequired)
				return
			}
			if !scheduledAt.After(s.now().UTC()) {
				s.writeError(w, validationError("scheduled_at must be in the future"))
				return
			}
			post.Status, post.ScheduledAt = store.PostStatusScheduled, scheduledAt
			if err := s.app.ValidatePostForScheduling(r.Context(), post); err != nil {
				s.writeError(w, err)
				return
			}
		}
	}
	created, err := s.app.Store().CreatePostForWorkspace(r.Context(), access.UserID, access.WorkspaceID, post)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, scopeWorkspacePostMedia(created, access.WorkspaceID))
}

func (s *Server) getWorkspacePost(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsRead)
	if !ok {
		return
	}
	post, err := s.app.Store().GetPostForWorkspace(r.Context(), access.UserID, access.WorkspaceID, postID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, scopeWorkspacePostMedia(post, access.WorkspaceID))
}

func (s *Server) updateWorkspacePost(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	current, err := s.app.Store().GetPostForWorkspace(r.Context(), access.UserID, access.WorkspaceID, postID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	var request updatePostRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	if len(request.ScheduledAt) > 0 {
		s.problem(w, http.StatusBadRequest, "schedule_endpoint_required",
			"Use the workspace post schedule endpoint after content is approved", nil)
		return
	}
	if request.ExpectedUpdatedAt != nil {
		expected, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(*request.ExpectedUpdatedAt))
		if parseErr != nil {
			s.writeError(w, validationError("expected_updated_at must be an RFC3339 timestamp"))
			return
		}
		if !current.UpdatedAt.Equal(expected) {
			s.writeError(w, fmt.Errorf("%w: post changed in another session; reload before saving", store.ErrConflict))
			return
		}
	}
	candidate := current
	if request.Title != nil {
		candidate.Title = *request.Title
	}
	if request.Content != nil {
		candidate.Content = *request.Content
	}
	if request.Format != nil {
		candidate.Format = *request.Format
	}
	if err := validatePostFields(candidate.Title, candidate.Content, candidate.Format); err != nil {
		s.writeError(w, err)
		return
	}
	changes := store.PostChanges{
		Title: request.Title, Content: request.Content, Format: request.Format,
		ImagePrompt: request.ImagePrompt, LinkButtons: request.LinkButtons,
		DisableLinkPreview: request.DisableLinkPreview,
	}
	if request.Notify != nil {
		notify := true
		changes.Notify, candidate.Notify = &notify, true
	}
	if request.LinkButtons != nil {
		if err := store.ValidateLinkButtonsDraft(*request.LinkButtons); err != nil {
			s.writeError(w, err)
			return
		}
		candidate.LinkButtons = *request.LinkButtons
	}
	if len(request.ChannelID) > 0 {
		channelID, err := decodeNullableInt64(request.ChannelID)
		if err != nil {
			s.writeError(w, err)
			return
		}
		if channelID != nil {
			if *channelID <= 0 {
				s.problem(w, http.StatusBadRequest, "validation_error", "channel_id must be positive", nil)
				return
			}
			if _, err := s.app.Store().GetChannelForWorkspace(
				r.Context(), access.UserID, access.WorkspaceID, *channelID); err != nil {
				s.writeError(w, err)
				return
			}
		}
		changes.ChannelID, candidate.ChannelID = &channelID, channelID
	}
	if len(request.ImageURL) > 0 {
		imageURL := ""
		if string(request.ImageURL) != "null" {
			if err := json.Unmarshal(request.ImageURL, &imageURL); err != nil {
				s.writeError(w, validationError("image_url must be a local media URL or null"))
				return
			}
		}
		resolved, err := s.resolveWorkspaceImageURL(
			r.Context(), access.UserID, access.WorkspaceID, imageURL)
		if err != nil {
			s.writeError(w, err)
			return
		}
		changes.ImageURL, changes.ImagePath = &imageURL, &resolved
		candidate.ImageURL, candidate.ImagePath = imageURL, resolved
	}
	if request.ImagePrompt != nil {
		candidate.ImagePrompt = *request.ImagePrompt
	}
	if request.DisableLinkPreview != nil {
		candidate.DisableLinkPreview = *request.DisableLinkPreview
	}
	post, err := s.app.Store().UpdatePostForWorkspaceIfUnchanged(
		r.Context(), access.UserID, access.WorkspaceID, current, changes)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, scopeWorkspacePostMedia(post, access.WorkspaceID))
}

func (s *Server) deleteWorkspacePost(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsDelete)
	if !ok {
		return
	}
	if err := s.app.Store().DeletePostForWorkspace(r.Context(), access.UserID, access.WorkspaceID, postID); errors.Is(err, store.ErrPublicationExists) {
		s.problem(w, http.StatusConflict, "publication_exists", "Delete the MAX publication before deleting the post", nil)
		return
	} else if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) duplicateWorkspacePost(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsWrite)
	if !ok {
		return
	}
	post, err := s.app.Store().DuplicatePostForWorkspace(r.Context(), access.UserID, access.WorkspaceID, postID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, scopeWorkspacePostMedia(post, access.WorkspaceID))
}

func (s *Server) scheduleWorkspacePost(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsPublish)
	if !ok {
		return
	}
	var request scheduleRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	scheduledAt, err := parseFutureTimeAt(request.ScheduledAt, s.now().UTC())
	if err != nil {
		s.writeError(w, err)
		return
	}
	if _, err := s.app.Store().GetPostForWorkspace(r.Context(), access.UserID, access.WorkspaceID, postID); err != nil {
		s.writeError(w, err)
		return
	}
	post, err := s.app.SchedulePost(r.Context(), postID, scheduledAt)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, scopeWorkspacePostMedia(post, access.WorkspaceID))
}

func (s *Server) cancelWorkspaceSchedule(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsPublish)
	if !ok {
		return
	}
	if _, err := s.app.Store().GetPostForWorkspace(r.Context(), access.UserID, access.WorkspaceID, postID); err != nil {
		s.writeError(w, err)
		return
	}
	post, err := s.app.Store().CancelSchedule(r.Context(), postID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, scopeWorkspacePostMedia(post, access.WorkspaceID))
}

func (s *Server) publishWorkspacePost(w http.ResponseWriter, r *http.Request) {
	_, access, postID, ok := s.requireWorkspacePostCapability(w, r, app.CapabilityPostsPublish)
	if !ok {
		return
	}
	if _, err := s.app.Store().GetPostForWorkspace(r.Context(), access.UserID, access.WorkspaceID, postID); err != nil {
		s.writeError(w, err)
		return
	}
	ctx, cancel := contextWithTimeout(r, 3*time.Minute)
	defer cancel()
	post, err := s.app.PublishPost(ctx, postID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, scopeWorkspacePostMedia(post, access.WorkspaceID))
}

func (s *Server) listWorkspaceChannels(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityChannelsRead)
	if !ok {
		return
	}
	channels, err := s.app.Store().ListChannelsForWorkspace(r.Context(), access.UserID, access.WorkspaceID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, channels)
}

func (s *Server) getWorkspaceChannel(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityChannelsRead)
	if !ok {
		return
	}
	channelID, err := parsePositivePathID(r, "channel_id")
	if err != nil {
		s.writeError(w, err)
		return
	}
	channel, err := s.app.Store().GetChannelForWorkspace(r.Context(), access.UserID, access.WorkspaceID, channelID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, channel)
}

func (s *Server) updateWorkspaceChannel(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityChannelsManage)
	if !ok {
		return
	}
	channelID, err := parsePositivePathID(r, "channel_id")
	if err != nil {
		s.writeError(w, err)
		return
	}
	var request updateChannelRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	if request.MAXChatID != nil {
		current, getErr := s.app.Store().GetChannelForWorkspace(r.Context(), access.UserID, access.WorkspaceID, channelID)
		if getErr != nil {
			s.writeError(w, getErr)
			return
		}
		if strings.TrimSpace(*request.MAXChatID) != current.MAXChatID {
			s.problem(w, http.StatusBadRequest, "reconnect_required", "Reconnect to change the MAX channel", nil)
			return
		}
	}
	if request.Title != nil {
		value := strings.TrimSpace(*request.Title)
		if value == "" || utf8.RuneCountInString(value) > 200 {
			s.problem(w, http.StatusBadRequest, "validation_error", "title must contain 1 to 200 characters", nil)
			return
		}
		request.Title = &value
	}
	channel, err := s.app.Store().UpdateChannelForWorkspace(
		r.Context(), access.UserID, access.WorkspaceID, channelID, request.Title, request.Active)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, channel)
}

func (s *Server) deleteWorkspaceChannel(w http.ResponseWriter, r *http.Request) {
	_, access, ok := s.requireWorkspaceCapability(w, r, app.CapabilityChannelsManage)
	if !ok {
		return
	}
	channelID, err := parsePositivePathID(r, "channel_id")
	if err != nil {
		s.writeError(w, err)
		return
	}
	if err := s.app.Store().DeleteChannelForWorkspace(r.Context(), access.UserID, access.WorkspaceID, channelID); err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusNoContent, nil)
}
