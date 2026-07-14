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
	channels, err := s.app.Store().ListChannels(r.Context())
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
	title := strings.TrimSpace(update.Title)
	if title == "" && update.Chat != nil {
		title = strings.TrimSpace(update.Chat.Title)
	}
	active := update.UpdateType != "bot_removed"
	if _, err := s.app.Store().UpsertDiscoveredChannel(r.Context(), chatID, title, update.IsChannel, active); err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
