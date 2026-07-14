package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/store"
)

type integrationStatus struct {
	Configured   bool   `json:"configured"`
	Connected    bool   `json:"connected"`
	BotName      string `json:"bot_name,omitempty"`
	BotUsername  string `json:"bot_username,omitempty"`
	ChannelCount int    `json:"channel_count"`
	LastError    string `json:"last_error,omitempty"`
}

func (s *Server) maxIntegrationStatus(w http.ResponseWriter, r *http.Request) {
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
	status := integrationStatus{Configured: s.app.MAXConfigured(), ChannelCount: len(channels)}
	if !status.Configured {
		s.writeJSON(w, http.StatusOK, status)
		return
	}
	ctx, cancel := contextWithTimeout(r, 8*time.Second)
	defer cancel()
	bot, err := s.app.TestMAX(ctx)
	if err != nil {
		status.LastError = "Could not connect to MAX"
		s.writeJSON(w, http.StatusOK, status)
		return
	}
	status.Connected = true
	status.BotName = bot.Name
	if status.BotName == "" {
		status.BotName = strings.TrimSpace(bot.FirstName + " " + bot.LastName)
	}
	status.BotUsername = bot.Username
	s.writeJSON(w, http.StatusOK, status)
}

func (s *Server) testMAXIntegration(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()
	bot, err := s.app.TestMAX(ctx)
	if err != nil {
		s.writeError(w, err)
		return
	}
	name := bot.Name
	if name == "" {
		name = strings.TrimSpace(bot.FirstName + " " + bot.LastName)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": "MAX connection is ready", "bot_name": name, "bot_username": bot.Username,
	})
}

type maxUpdate struct {
	UpdateType string          `json:"update_type"`
	Timestamp  int64           `json:"timestamp"`
	ChatID     json.RawMessage `json:"chat_id"`
	IsChannel  bool            `json:"is_channel"`
	Title      string          `json:"title,omitempty"`
	Chat       *struct {
		Title string `json:"title"`
	} `json:"chat,omitempty"`
	Payload string `json:"payload,omitempty"`
	User    *struct {
		UserID json.RawMessage `json:"user_id"`
	} `json:"user,omitempty"`
	Callback *struct {
		CallbackID string `json:"callback_id"`
		Payload    string `json:"payload"`
		User       *struct {
			UserID json.RawMessage `json:"user_id"`
		} `json:"user,omitempty"`
	} `json:"callback,omitempty"`
}

func (s *Server) maxWebhook(w http.ResponseWriter, r *http.Request) {
	if s.webhookSecret == "" {
		s.problem(w, http.StatusServiceUnavailable, "webhook_not_configured", "MAX_WEBHOOK_SECRET is not configured", nil)
		return
	}
	provided := r.Header.Get("X-Max-Bot-Api-Secret")
	if len(provided) != len(s.webhookSecret) || subtle.ConstantTimeCompare([]byte(provided), []byte(s.webhookSecret)) != 1 {
		s.problem(w, http.StatusUnauthorized, "invalid_webhook_secret", "Invalid MAX webhook secret", nil)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	var update maxUpdate
	if err := decoder.Decode(&update); err != nil {
		if errors.Is(err, io.EOF) {
			s.problem(w, http.StatusBadRequest, "invalid_webhook", "Webhook body is empty", nil)
		} else {
			s.problem(w, http.StatusBadRequest, "invalid_webhook", "Webhook body is invalid", nil)
		}
		return
	}
	if update.UpdateType == "bot_started" {
		s.handleBotStarted(w, r, update)
		return
	}
	if update.UpdateType == "message_callback" {
		s.handleMessageCallback(w, r, update)
		return
	}
	chatID, err := webhookChatID(update.ChatID)
	if err != nil {
		// Some subscribed event types do not carry a channel id. They are safe to acknowledge.
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	if !update.IsChannel && update.UpdateType != "bot_removed" {
		// The application manages channels, not private dialogs or group chats.
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	switch update.UpdateType {
	case "bot_added":
		eventAt, valid := maxEventTime(update.Timestamp, s.now().UTC())
		if !valid {
			s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
			return
		}
		ctx, cancel := contextWithTimeout(r, 8*time.Second)
		err = s.app.ObserveMAXChat(ctx, chatID, true, eventAt)
		cancel()
		if err != nil {
			s.writeError(w, err)
			return
		}
	case "bot_removed":
		eventAt, valid := maxEventTime(update.Timestamp, s.now().UTC())
		if !valid {
			s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
			return
		}
		if err := s.app.ObserveMAXChat(r.Context(), chatID, false, eventAt); err != nil {
			s.writeError(w, err)
			return
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func maxEventTime(timestamp int64, now time.Time) (time.Time, bool) {
	if timestamp <= 0 {
		return time.Time{}, false
	}
	var eventAt time.Time
	if timestamp > 10_000_000_000 {
		eventAt = time.UnixMilli(timestamp)
	} else {
		eventAt = time.Unix(timestamp, 0)
	}
	eventAt = eventAt.UTC()
	if eventAt.After(now.Add(5*time.Minute)) || eventAt.Before(now.Add(-30*24*time.Hour)) {
		return time.Time{}, false
	}
	return eventAt, true
}

func (s *Server) handleBotStarted(w http.ResponseWriter, r *http.Request, update maxUpdate) {
	maxUserID, err := webhookUserID(update.User)
	if err != nil || !strings.HasPrefix(update.Payload, "claim_") {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	deepToken := strings.TrimPrefix(update.Payload, "claim_")
	if len(deepToken) < 32 || len(deepToken) > 96 {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	confirmToken, err := randomURLToken(32)
	if err != nil {
		s.writeError(w, err)
		return
	}
	cancelToken, err := randomURLToken(32)
	if err != nil {
		s.writeError(w, err)
		return
	}
	now := s.now().UTC()
	claim, first, err := s.app.Store().StartChannelClaimConfirmation(r.Context(), sha256Hex(deepToken), maxUserID,
		sha256Hex(confirmToken), sha256Hex(cancelToken), now)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
			return
		}
		s.writeError(w, err)
		return
	}
	if first {
		ctx, cancel := contextWithTimeout(r, 8*time.Second)
		err = s.app.SendChannelClaimConfirmation(ctx, maxUserID, firstNonEmpty(claim.RequestedTitle, "канал MAX"),
			firstNonEmpty(claim.PublicLink, "MAX ID: "+claim.MAXChatID), claim.RequesterLabel, claim.ComparisonCode,
			"claim_confirm_"+confirmToken, "claim_cancel_"+cancelToken)
		cancel()
		if err != nil {
			_ = s.app.Store().FailChannelClaim(r.Context(), claim.UserID, claim.ID, "confirmation_send_failed", now)
			s.writeError(w, err)
			return
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMessageCallback(w http.ResponseWriter, r *http.Request, update maxUpdate) {
	if update.Callback == nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	maxUserID, err := webhookUserID(update.Callback.User)
	if err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	payload := update.Callback.Payload
	confirm := strings.HasPrefix(payload, "claim_confirm_")
	cancelled := strings.HasPrefix(payload, "claim_cancel_")
	if !confirm && !cancelled {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	token := strings.TrimPrefix(payload, "claim_confirm_")
	if cancelled {
		token = strings.TrimPrefix(payload, "claim_cancel_")
	}
	claim, err := s.app.Store().ConfirmChannelClaim(r.Context(), sha256Hex(token), maxUserID, confirm, s.now().UTC())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
			return
		}
		s.writeError(w, err)
		return
	}
	notification := "Подключение подтверждено. Вернитесь в MaxPosty."
	if confirm && claim.Status == store.ChannelClaimIdentityVerified {
		ctx, cancel := contextWithTimeout(r, 10*time.Second)
		_, _, completeErr := s.app.CompleteChannelClaim(ctx, claim)
		cancel()
		if completeErr != nil {
			var accessErr *app.ChannelAccessError
			if errors.Is(completeErr, store.ErrChannelOwned) || errors.As(completeErr, &accessErr) {
				code := "owner_or_permissions_verification_failed"
				if errors.Is(completeErr, store.ErrChannelOwned) {
					code = "channel_already_connected"
				}
				_ = s.app.Store().FailChannelClaim(r.Context(), claim.UserID, claim.ID, code, s.now().UTC())
				notification = "Не удалось подключить канал: проверьте владельца и права помощника."
				claim.Status = store.ChannelClaimFailed
			} else {
				// Keep identity_verified so MAX can safely retry the same callback.
				s.writeError(w, completeErr)
				return
			}
		} else {
			notification = "Канал подключён к MaxPosty."
			claim.Status = store.ChannelClaimConnected
		}
	}
	if claim.Status == store.ChannelClaimFailed {
		if !confirm {
			notification = "Подключение отменено."
		}
	}
	if update.Callback.CallbackID != "" {
		ctx, cancel := contextWithTimeout(r, 5*time.Second)
		if answerErr := s.app.AnswerMAXCallback(ctx, update.Callback.CallbackID, notification); answerErr != nil {
			s.logger.Warn("could not answer MAX callback", "error", answerErr)
		}
		cancel()
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func webhookUserID(user *struct {
	UserID json.RawMessage `json:"user_id"`
}) (string, error) {
	if user == nil {
		return "", errors.New("MAX user is missing")
	}
	return webhookChatID(user.UserID)
}

func webhookChatID(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", errors.New("chat_id is missing")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		text = strings.TrimSpace(text)
		if !validMAXChatID(text) {
			return "", errors.New("chat_id is invalid")
		}
		return text, nil
	}
	text = strings.TrimSpace(string(raw))
	if !validMAXChatID(text) {
		return "", fmt.Errorf("chat_id is invalid: %q", text)
	}
	return text, nil
}
