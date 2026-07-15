package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/store"
)

var maxChatIDPattern = regexp.MustCompile(`^-?[0-9]+$`)

const channelClaimTTL = 10 * time.Minute

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

type connectObservedChannelRequest struct {
	MAXChatID string `json:"max_chat_id"`
}

func (s *Server) listDiscoverableChannels(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	channels, err := s.app.Store().ListDiscoverableChannelsForUser(r.Context(), userID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"channels": channels})
}

func (s *Server) refreshDiscoverableChannels(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()
	result, err := s.app.RefreshDiscoverableChannelsForUser(ctx, userID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) connectObservedChannel(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	var request connectObservedChannelRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	request.MAXChatID = strings.TrimSpace(request.MAXChatID)
	if !validMAXChatID(request.MAXChatID) {
		s.problem(w, http.StatusBadRequest, "validation_error", "max_chat_id must be a numeric string", nil)
		return
	}
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	check, err := s.app.ConnectDiscoverableChannelForUser(ctx, userID, request.MAXChatID)
	cancel()
	if err != nil {
		if errors.Is(err, store.ErrChannelOwned) {
			s.problem(w, http.StatusConflict, "channel_already_connected", "Канал уже подключён к другому аккаунту", nil)
			return
		}
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"channel": check.Channel, "diagnostics": check.Diagnostics})
}

func (s *Server) listChannels(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	channels, err := s.app.Store().ListChannelsForUser(r.Context(), userID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, channels)
}

func (s *Server) startChannelConnect(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	var request createChannelRequest
	if !s.decodeJSON(w, r, &request) {
		return
	}
	request.PublicLink = strings.TrimRight(strings.TrimSpace(request.PublicLink), "/")
	request.MAXChatID = strings.TrimSpace(request.MAXChatID)
	request.Title = strings.TrimSpace(request.Title)
	if request.PublicLink == "" && !validMAXChatID(request.MAXChatID) {
		s.problem(w, http.StatusBadRequest, "validation_error", "public_link or numeric max_chat_id is required", nil)
		return
	}
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()
	candidate, err := s.app.PrepareChannelClaim(ctx, request.PublicLink, request.MAXChatID)
	if err != nil {
		if errors.Is(err, app.ErrMAXChannelEventRequired) {
			s.problem(w, http.StatusUnprocessableEntity, "max_channel_event_required",
				"MAX не передал ID этого уже подключённого канала. Опубликуйте в канале любой новый пост, затем нажмите „Обновить список“.",
				map[string]any{"action": "publish_post_and_refresh"})
			return
		}
		if errors.Is(err, store.ErrNotFound) || strings.Contains(err.Error(), "first add") {
			s.problem(w, http.StatusUnprocessableEntity, "bot_not_observed",
				"Сначала добавьте помощника MaxPosty администратором канала и повторите подключение", nil)
			return
		}
		s.writeError(w, err)
		return
	}
	// A previously linked MAX identity is already an explicit ownership proof.
	// The application method rechecks that identity, the observed channel owner,
	// current API metadata and bot permissions before the transactional connect.
	connected, connectErr := s.app.ConnectDiscoverableChannelForUser(ctx, userID, candidate.Info.ChatID)
	switch {
	case connectErr == nil:
		claimID, tokenErr := randomURLToken(32)
		if tokenErr != nil {
			s.writeError(w, tokenErr)
			return
		}
		s.writeJSON(w, http.StatusCreated, map[string]any{
			"claim_id": claimID, "status": store.ChannelClaimConnected,
			"channel": connected.Channel, "diagnostics": connected.Diagnostics,
			"message": "Канал подтверждён связанным профилем MAX и подключён",
		})
		return
	case errors.Is(connectErr, store.ErrChannelOwned):
		s.problem(w, http.StatusConflict, "channel_already_connected", "Канал уже подключён к другому аккаунту", nil)
		return
	case !errors.Is(connectErr, store.ErrNotFound):
		s.writeError(w, connectErr)
		return
	}
	if existing, getErr := s.app.Store().GetChannelByMAXChatID(r.Context(), candidate.Info.ChatID); getErr == nil {
		if existing.UserID != userID {
			s.problem(w, http.StatusConflict, "channel_already_connected", "Канал уже подключён к другому аккаунту", nil)
			return
		}
		s.problem(w, http.StatusConflict, "channel_already_connected", "Канал уже подключён к вашему аккаунту", map[string]any{"channel_id": existing.ID})
		return
	} else if !errors.Is(getErr, store.ErrNotFound) {
		s.writeError(w, getErr)
		return
	}
	claimID, err := randomURLToken(32)
	if err != nil {
		s.writeError(w, err)
		return
	}
	deepToken, err := randomURLToken(32)
	if err != nil {
		s.writeError(w, err)
		return
	}
	comparisonCode, err := randomComparisonCode()
	if err != nil {
		s.writeError(w, err)
		return
	}
	user, err := s.app.Store().GetUser(r.Context(), userID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	requesterLabel := safeRequesterLabel(firstNonEmpty(user.Login, user.Email, user.DisplayName, user.ID))
	now := s.now().UTC()
	claimTitle := firstNonEmpty(strings.TrimSpace(candidate.Info.Title), request.Title, "MAX "+candidate.Info.ChatID)
	claim := store.ChannelClaim{
		ID: claimID, TokenHash: sha256Hex(deepToken), UserID: userID, MAXChatID: candidate.Info.ChatID,
		PublicLink: candidate.Info.Link, RequestedTitle: claimTitle, Status: store.ChannelClaimPending,
		RequesterLabel: requesterLabel, ComparisonCode: comparisonCode,
		CreatedAt: now, ExpiresAt: now.Add(channelClaimTTL), UpdatedAt: now,
	}
	if err := s.app.Store().CreateChannelClaim(r.Context(), claim); err != nil {
		s.writeError(w, err)
		return
	}
	username := strings.TrimPrefix(strings.TrimSpace(candidate.Bot.Username), "@")
	botURL := "https://max.ru/" + url.PathEscape(username) + "?start=" + url.QueryEscape("claim_"+deepToken)
	s.writeJSON(w, http.StatusCreated, map[string]any{
		"claim_id": claim.ID, "status": claim.Status, "expires_at": claim.ExpiresAt, "bot_url": botURL,
		"comparison_code": claim.ComparisonCode, "requester_label": claim.RequesterLabel,
		"channel": map[string]any{"max_chat_id": candidate.Info.ChatID, "title": candidate.Info.Title,
			"public_link": candidate.Info.Link, "icon_url": maxclient.SafeAssetURL(candidate.Info.Icon.URL)},
		"message": "Откройте помощника в MAX и подтвердите подключение в личном сообщении",
	})
}

func safeRequesterLabel(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return -1
		}
		return r
	}, strings.TrimSpace(value))
	runes := []rune(value)
	if len(runes) > 120 {
		value = string(runes[:120])
	}
	return firstNonEmpty(value, "Пользователь MaxPosty")
}

func (s *Server) getChannelConnect(w http.ResponseWriter, r *http.Request) {
	userID, err := authenticatedUserID(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	claimID := strings.TrimSpace(chi.URLParam(r, "claim_id"))
	if claimID == "" || len(claimID) > 128 {
		s.problem(w, http.StatusNotFound, "not_found", "Connection attempt was not found", nil)
		return
	}
	claim, err := s.app.Store().GetChannelClaimForUser(r.Context(), userID, claimID, s.now().UTC())
	if err != nil {
		s.writeError(w, err)
		return
	}
	var channel any
	var diagnostics any
	if claim.Status == store.ChannelClaimIdentityVerified {
		ctx, cancel := contextWithTimeout(r, 15*time.Second)
		connected, checked, completeErr := s.app.CompleteChannelClaim(ctx, claim)
		cancel()
		if completeErr != nil {
			if errors.Is(completeErr, store.ErrChannelOwned) {
				_ = s.app.Store().FailChannelClaim(r.Context(), userID, claim.ID, "channel_already_connected", s.now().UTC())
				s.problem(w, http.StatusConflict, "channel_already_connected", "Канал уже подключён к другому аккаунту", nil)
				return
			}
			if strings.Contains(strings.ToLower(completeErr.Error()), "owner") {
				_ = s.app.Store().FailChannelClaim(r.Context(), userID, claim.ID, "owner_verification_failed", s.now().UTC())
			}
			s.writeError(w, completeErr)
			return
		}
		claim, err = s.app.Store().GetChannelClaimForUser(r.Context(), userID, claimID, s.now().UTC())
		if err != nil {
			s.writeError(w, err)
			return
		}
		channel, diagnostics = connected, checked
	} else if claim.Status == store.ChannelClaimConnected && claim.ChannelID != nil {
		connected, getErr := s.app.Store().GetChannelForUser(r.Context(), userID, *claim.ChannelID)
		if getErr == nil {
			channel = connected
		}
	}
	publicStatus := claim.Status
	step := claim.Status
	if publicStatus == store.ChannelClaimAwaitingConfirmation {
		publicStatus = store.ChannelClaimPending
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"claim_id": claim.ID, "status": publicStatus, "step": step, "expires_at": claim.ExpiresAt,
		"comparison_code": claim.ComparisonCode, "requester_label": claim.RequesterLabel,
		"error_code": claim.ErrorCode, "channel": channel, "diagnostics": diagnostics,
	})
}

func (s *Server) getChannel(w http.ResponseWriter, r *http.Request) {
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
	channel, err := s.app.Store().GetChannelForUser(r.Context(), userID, id)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, channel)
}

func (s *Server) updateChannel(w http.ResponseWriter, r *http.Request) {
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
		current, getErr := s.app.Store().GetChannelForUser(r.Context(), userID, id)
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
	channel, err := s.app.Store().UpdateChannelForUser(r.Context(), userID, id, request.Title, request.Active)
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
	if err := s.app.Store().DeleteChannelForUser(r.Context(), userID, id); err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) testChannel(w http.ResponseWriter, r *http.Request) {
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
	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()
	check, err := s.app.TestChannelForUser(ctx, userID, id)
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
