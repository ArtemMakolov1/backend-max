package api

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var maxChatIDPattern = regexp.MustCompile(`^-?[0-9]+$`)

type createChannelRequest struct {
	PublicLink string `json:"public_link,omitempty"`
	MAXChatID  string `json:"max_chat_id,omitempty"`
	Title      string `json:"title,omitempty"`
}

type updateChannelRequest struct {
	MAXChatID *string `json:"max_chat_id,omitempty"`
	Title     *string `json:"title,omitempty"`
	Active    *bool   `json:"active,omitempty"`
}

func (s *Server) listChannels(w http.ResponseWriter, r *http.Request) {
	channels, err := s.app.Store().ListChannels(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, channels)
}

func (s *Server) createChannel(w http.ResponseWriter, r *http.Request) {
	var request createChannelRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	request.MAXChatID = strings.TrimSpace(request.MAXChatID)
	request.PublicLink = strings.TrimSpace(request.PublicLink)
	request.Title = strings.TrimSpace(request.Title)
	if request.PublicLink == "" && !validMAXChatID(request.MAXChatID) {
		s.problem(w, http.StatusBadRequest, "validation_error", "max_chat_id must be a numeric string", nil)
		return
	}
	if request.MAXChatID != "" && !validMAXChatID(request.MAXChatID) {
		s.problem(w, http.StatusBadRequest, "validation_error", "max_chat_id must be a numeric string", nil)
		return
	}
	if utf8.RuneCountInString(request.Title) > 200 {
		s.problem(w, http.StatusBadRequest, "validation_error", "title must not exceed 200 characters", nil)
		return
	}
	ctx, cancel := contextWithTimeout(r, 20*time.Second)
	defer cancel()
	check, err := s.app.ConnectChannel(ctx, request.PublicLink, request.MAXChatID, request.Title)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, check.Channel)
}

func (s *Server) getChannel(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	channel, err := s.app.Store().GetChannel(r.Context(), id)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, channel)
}

func (s *Server) updateChannel(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	var request updateChannelRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	if request.MAXChatID != nil {
		trimmed := strings.TrimSpace(*request.MAXChatID)
		if !validMAXChatID(trimmed) {
			s.problem(w, http.StatusBadRequest, "validation_error", "max_chat_id must be a numeric string", nil)
			return
		}
		current, getErr := s.app.Store().GetChannel(r.Context(), id)
		if getErr != nil {
			s.writeError(w, getErr)
			return
		}
		if trimmed != current.MAXChatID {
			s.problem(w, http.StatusBadRequest, "reconnect_required", "Use POST /channels to connect a different MAX channel", nil)
			return
		}
		request.MAXChatID = nil
	}
	if request.Title != nil {
		trimmed := strings.TrimSpace(*request.Title)
		if trimmed == "" || utf8.RuneCountInString(trimmed) > 200 {
			s.problem(w, http.StatusBadRequest, "validation_error", "title must contain 1 to 200 characters", nil)
			return
		}
		request.Title = &trimmed
	}
	channel, err := s.app.Store().UpdateChannel(r.Context(), id, request.MAXChatID, request.Title, request.Active)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			s.problem(w, http.StatusConflict, "channel_exists", "A channel with this max_chat_id already exists", nil)
			return
		}
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, channel)
}

func (s *Server) deleteChannel(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if err := s.app.Store().DeleteChannel(r.Context(), id); err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) testChannel(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()
	check, err := s.app.TestChannel(ctx, id)
	if err != nil {
		s.writeError(w, err)
		return
	}
	ok := check.Diagnostics.CanPublish
	message := "Channel is ready for publishing"
	if !ok {
		message = "Channel or bot permissions require attention"
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": ok, "message": message, "channel": check.Channel, "diagnostics": check.Diagnostics,
	})
}

func contextWithTimeout(r *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(r.Context())
	}
	return context.WithTimeout(r.Context(), timeout)
}

func validMAXChatID(value string) bool {
	if !maxChatIDPattern.MatchString(value) {
		return false
	}
	_, err := strconv.ParseInt(value, 10, 64)
	return err == nil
}
