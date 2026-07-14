package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"maxpilot/backend/internal/openaiimg"
	"maxpilot/backend/internal/store"
)

type createPostRequest struct {
	Title              string             `json:"title"`
	Content            string             `json:"content"`
	Format             string             `json:"format"`
	ChannelID          *int64             `json:"channel_id,omitempty"`
	ImageURL           string             `json:"image_url,omitempty"`
	ImagePrompt        string             `json:"image_prompt,omitempty"`
	LinkButtons        []store.LinkButton `json:"link_buttons,omitempty"`
	Notify             *bool              `json:"notify,omitempty"`
	DisableLinkPreview bool               `json:"disable_link_preview"`
	ScheduledAt        json.RawMessage    `json:"scheduled_at,omitempty"`
}

type updatePostRequest struct {
	Title              *string             `json:"title,omitempty"`
	Content            *string             `json:"content,omitempty"`
	Format             *string             `json:"format,omitempty"`
	ChannelID          json.RawMessage     `json:"channel_id,omitempty"`
	ImageURL           json.RawMessage     `json:"image_url,omitempty"`
	ImagePrompt        *string             `json:"image_prompt,omitempty"`
	LinkButtons        *[]store.LinkButton `json:"link_buttons,omitempty"`
	Notify             *bool               `json:"notify,omitempty"`
	DisableLinkPreview *bool               `json:"disable_link_preview,omitempty"`
	ScheduledAt        json.RawMessage     `json:"scheduled_at,omitempty"`
	ExpectedUpdatedAt  *string             `json:"expected_updated_at,omitempty"`
}

type scheduleRequest struct {
	ScheduledAt string `json:"scheduled_at"`
}

func (s *Server) listPosts(w http.ResponseWriter, r *http.Request) {
	userID, authErr := authenticatedUserID(r)
	if authErr != nil {
		s.writeError(w, authErr)
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
	}
	if channelID != nil {
		if _, err := s.app.Store().GetChannelForUser(r.Context(), userID, *channelID); err != nil {
			s.writeError(w, err)
			return
		}
	}
	posts, err := s.app.Store().ListPostsForUser(r.Context(), userID, status, channelID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, posts)
}

func (s *Server) createPost(w http.ResponseWriter, r *http.Request) {
	userID, authErr := authenticatedUserID(r)
	if authErr != nil {
		s.writeError(w, authErr)
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
		if _, err := s.app.Store().GetChannelForUser(r.Context(), userID, *request.ChannelID); err != nil {
			s.writeError(w, err)
			return
		}
	}
	imagePath, err := s.resolveImageURL(r.Context(), userID, request.ImageURL)
	if err != nil {
		s.writeError(w, err)
		return
	}
	notify := true
	if request.Notify != nil {
		notify = *request.Notify
	}
	post := store.Post{
		UserID: userID,
		Title:  strings.TrimSpace(request.Title), Content: request.Content, Format: request.Format,
		Status: store.PostStatusDraft, ChannelID: request.ChannelID, ImageURL: request.ImageURL,
		ImagePath: imagePath, ImagePrompt: request.ImagePrompt, Notify: notify,
		LinkButtons: request.LinkButtons, DisableLinkPreview: request.DisableLinkPreview,
	}
	if len(request.ScheduledAt) > 0 {
		scheduledAt, err := decodeScheduledAt(request.ScheduledAt)
		if err != nil {
			s.writeError(w, err)
			return
		}
		if scheduledAt != nil {
			if !scheduledAt.After(time.Now().UTC()) {
				s.writeError(w, errors.New("scheduled_at must be in the future"))
				return
			}
			post.Status = store.PostStatusScheduled
			post.ScheduledAt = scheduledAt
			if err := s.app.ValidatePostForScheduling(r.Context(), post); err != nil {
				s.writeError(w, err)
				return
			}
		}
	}
	created, err := s.app.Store().CreatePost(r.Context(), post)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, created)
}

func (s *Server) getPost(w http.ResponseWriter, r *http.Request) {
	userID, authErr := authenticatedUserID(r)
	if authErr != nil {
		s.writeError(w, authErr)
		return
	}
	id, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	post, err := s.app.Store().GetPostForUser(r.Context(), userID, id)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, post)
}

func (s *Server) updatePost(w http.ResponseWriter, r *http.Request) {
	userID, authErr := authenticatedUserID(r)
	if authErr != nil {
		s.writeError(w, authErr)
		return
	}
	id, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	current, err := s.app.Store().GetPostForUser(r.Context(), userID, id)
	if err != nil {
		s.writeError(w, err)
		return
	}
	var request updatePostRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	if request.ExpectedUpdatedAt != nil {
		expected, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(*request.ExpectedUpdatedAt))
		if parseErr != nil {
			s.writeError(w, errors.New("expected_updated_at must be an RFC3339 timestamp"))
			return
		}
		if !current.UpdatedAt.Equal(expected) {
			s.writeError(w, fmt.Errorf("%w: post changed in another session; reload before saving", store.ErrConflict))
			return
		}
	}

	title, content, format := current.Title, current.Content, current.Format
	candidate := current
	if request.Title != nil {
		title = *request.Title
	}
	if request.Content != nil {
		content = *request.Content
	}
	if request.Format != nil {
		format = *request.Format
	}
	candidate.Title, candidate.Content, candidate.Format = title, content, format
	if err := validatePostFields(title, content, format); err != nil {
		s.writeError(w, err)
		return
	}

	changes := store.PostChanges{
		Title: request.Title, Content: request.Content, Format: request.Format,
		ImagePrompt: request.ImagePrompt, LinkButtons: request.LinkButtons, Notify: request.Notify,
		DisableLinkPreview: request.DisableLinkPreview,
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
			if _, err := s.app.Store().GetChannelForUser(r.Context(), userID, *channelID); err != nil {
				s.writeError(w, err)
				return
			}
		}
		changes.ChannelID = &channelID
		candidate.ChannelID = channelID
	}
	if len(request.ImageURL) > 0 {
		imageURL := ""
		if string(request.ImageURL) != "null" {
			if err := json.Unmarshal(request.ImageURL, &imageURL); err != nil {
				s.writeError(w, errors.New("image_url must be a local media URL or null"))
				return
			}
		}
		resolved, err := s.resolveImageURL(r.Context(), userID, imageURL)
		if err != nil {
			s.writeError(w, err)
			return
		}
		changes.ImageURL = &imageURL
		changes.ImagePath = &resolved
		candidate.ImageURL = imageURL
		candidate.ImagePath = resolved
	}
	if len(request.ScheduledAt) > 0 {
		scheduledAt, err := decodeScheduledAt(request.ScheduledAt)
		if err != nil {
			s.writeError(w, err)
			return
		}
		if scheduledAt != nil && !sameInstant(current.ScheduledAt, scheduledAt) && !scheduledAt.After(time.Now().UTC()) {
			s.writeError(w, errors.New("scheduled_at must be in the future"))
			return
		}
		changes.ScheduledAt = &scheduledAt
		candidate.ScheduledAt = scheduledAt
	}
	if request.ImagePrompt != nil {
		candidate.ImagePrompt = *request.ImagePrompt
	}
	if request.Notify != nil {
		candidate.Notify = *request.Notify
	}
	if request.DisableLinkPreview != nil {
		candidate.DisableLinkPreview = *request.DisableLinkPreview
	}
	if candidate.ScheduledAt != nil {
		if err := s.app.ValidatePostForScheduling(r.Context(), candidate); err != nil {
			s.writeError(w, err)
			return
		}
	}

	post, err := s.app.Store().UpdatePostIfUnchanged(r.Context(), current, changes)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, post)
}

func (s *Server) deletePost(w http.ResponseWriter, r *http.Request) {
	userID, authErr := authenticatedUserID(r)
	if authErr != nil {
		s.writeError(w, authErr)
		return
	}
	id, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if err := s.app.Store().DeletePostForUser(r.Context(), userID, id); errors.Is(err, store.ErrPublicationExists) {
		s.problem(w, http.StatusConflict, "publication_exists",
			"Пост всё ещё опубликован в MAX. Сначала удалите публикацию из MAX.", nil)
		return
	} else if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) duplicatePost(w http.ResponseWriter, r *http.Request) {
	userID, authErr := authenticatedUserID(r)
	if authErr != nil {
		s.writeError(w, authErr)
		return
	}
	id, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	post, err := s.app.Store().DuplicatePostForUser(r.Context(), userID, id)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, post)
}

func (s *Server) schedulePost(w http.ResponseWriter, r *http.Request) {
	userID, authErr := authenticatedUserID(r)
	if authErr != nil {
		s.writeError(w, authErr)
		return
	}
	id, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	var request scheduleRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	scheduledAt, err := parseFutureTime(request.ScheduledAt)
	if err != nil {
		s.writeError(w, err)
		return
	}
	post, err := s.app.Store().GetPostForUser(r.Context(), userID, id)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if post.ChannelID == nil || (strings.TrimSpace(post.Content) == "" && post.ImageURL == "") {
		s.problem(w, http.StatusBadRequest, "validation_error", "A channel and post content or an image are required before scheduling", nil)
		return
	}
	post, err = s.app.SchedulePost(r.Context(), id, scheduledAt)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, post)
}

func (s *Server) cancelSchedule(w http.ResponseWriter, r *http.Request) {
	userID, authErr := authenticatedUserID(r)
	if authErr != nil {
		s.writeError(w, authErr)
		return
	}
	id, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if _, err := s.app.Store().GetPostForUser(r.Context(), userID, id); err != nil {
		s.writeError(w, err)
		return
	}
	post, err := s.app.Store().CancelSchedule(r.Context(), id)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, post)
}

func (s *Server) publishPost(w http.ResponseWriter, r *http.Request) {
	userID, authErr := authenticatedUserID(r)
	if authErr != nil {
		s.writeError(w, authErr)
		return
	}
	id, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if _, err := s.app.Store().GetPostForUser(r.Context(), userID, id); err != nil {
		s.writeError(w, err)
		return
	}
	ctx, cancel := contextWithTimeout(r, 3*time.Minute)
	defer cancel()
	post, err := s.app.PublishPost(ctx, id)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, post)
}

func (s *Server) updatePublishedPost(w http.ResponseWriter, r *http.Request) {
	userID, authErr := authenticatedUserID(r)
	if authErr != nil {
		s.writeError(w, authErr)
		return
	}
	id, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if _, err := s.app.Store().GetPostForUser(r.Context(), userID, id); err != nil {
		s.writeError(w, err)
		return
	}
	ctx, cancel := contextWithTimeout(r, 3*time.Minute)
	defer cancel()
	post, err := s.app.UpdatePublishedPost(ctx, id)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, post)
}

func (s *Server) deletePublication(w http.ResponseWriter, r *http.Request) {
	userID, authErr := authenticatedUserID(r)
	if authErr != nil {
		s.writeError(w, authErr)
		return
	}
	id, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	ctx, cancel := contextWithTimeout(r, time.Minute)
	defer cancel()
	post, err := s.app.DeletePublication(ctx, userID, id)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, post)
}

func (s *Server) generateImage(w http.ResponseWriter, r *http.Request) {
	userID, authErr := authenticatedUserID(r)
	if authErr != nil {
		s.writeError(w, authErr)
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
	release, err := s.aiLimiter.acquire(r.Context(), userID, store.AIOperationImage, s.now().UTC())
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer release()
	ctx, cancel := contextWithTimeout(r, AIHandlerTimeout)
	defer cancel()
	file, err := s.app.GenerateImage(ctx, request)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if err := s.app.Store().RegisterMedia(r.Context(), userID, file.Filename, s.now().UTC()); err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, file)
}

func (s *Server) generatePostImage(w http.ResponseWriter, r *http.Request) {
	userID, authErr := authenticatedUserID(r)
	if authErr != nil {
		s.writeError(w, authErr)
		return
	}
	id, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	ownedPost, err := s.app.Store().GetPostForUser(r.Context(), userID, id)
	if err != nil {
		s.writeError(w, err)
		return
	}
	var request openaiimg.GenerateRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.Prompt) == "" {
		request.Prompt = ownedPost.ImagePrompt
	}
	if err := s.app.ValidateImageRequest(request); err != nil {
		s.writeError(w, err)
		return
	}
	release, err := s.aiLimiter.acquire(r.Context(), userID, store.AIOperationImage, s.now().UTC())
	if err != nil {
		s.writeError(w, err)
		return
	}
	defer release()
	ctx, cancel := contextWithTimeout(r, AIHandlerTimeout)
	defer cancel()
	post, err := s.app.GeneratePostImage(ctx, id, request)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if post.ImageURL != "" {
		filename := path.Base(post.ImageURL)
		if err := s.app.Store().RegisterMedia(r.Context(), userID, filename, s.now().UTC()); err != nil {
			s.writeError(w, err)
			return
		}
	}
	s.writeJSON(w, http.StatusOK, post)
}

func (s *Server) resolveImageURL(ctx context.Context, userID, imageURL string) (string, error) {
	if strings.TrimSpace(imageURL) == "" {
		return "", nil
	}
	filename, err := s.app.Media().FilenameFromURL(imageURL)
	if err != nil {
		return "", err
	}
	owned, err := s.app.Store().UserOwnsMedia(ctx, userID, filename)
	if err != nil {
		return "", err
	}
	if !owned {
		return "", store.ErrNotFound
	}
	return s.app.Media().ResolveURL(imageURL)
}

func validatePostFields(title, content, format string) error {
	if utf8.RuneCountInString(strings.TrimSpace(title)) > 200 {
		return errors.New("title must not exceed 200 characters")
	}
	if !store.ValidFormat(format) {
		return errors.New("format must be markdown or html")
	}
	if utf8.RuneCountInString(content) > 100000 {
		return errors.New("draft content must not exceed 100000 characters")
	}
	return nil
}

func parseFutureTime(value string) (time.Time, error) {
	return parseFutureTimeAt(value, time.Now().UTC())
}

func parseFutureTimeAt(value string, now time.Time) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, errors.New("scheduled_at must be an RFC3339 timestamp")
	}
	parsed = parsed.UTC()
	if !parsed.After(now.UTC()) {
		return time.Time{}, errors.New("scheduled_at must be in the future")
	}
	return parsed, nil
}

func decodeScheduledAt(raw json.RawMessage) (*time.Time, error) {
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
		return nil, errors.New("scheduled_at must be an RFC3339 string or null")
	}
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return nil, errors.New("scheduled_at must be an RFC3339 timestamp")
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

func sameInstant(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func decodeNullableInt64(raw json.RawMessage) (*int64, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, errors.New("channel_id must be an integer or null")
	}
	return &value, nil
}

func validPostStatus(status string) bool {
	switch status {
	case store.PostStatusDraft, store.PostStatusScheduled, store.PostStatusPublishing, store.PostStatusPublished, store.PostStatusFailed:
		return true
	default:
		return false
	}
}
