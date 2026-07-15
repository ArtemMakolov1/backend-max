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
	"maxpilot/backend/internal/maxclient"
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

type maxWebhookUser struct {
	UserID        json.RawMessage `json:"user_id"`
	FirstName     string          `json:"first_name,omitempty"`
	LastName      string          `json:"last_name,omitempty"`
	Username      string          `json:"username,omitempty"`
	AvatarURL     string          `json:"avatar_url,omitempty"`
	FullAvatarURL string          `json:"full_avatar_url,omitempty"`
}

type maxWebhookAttachment struct {
	Type    string `json:"type"`
	Payload struct {
		VCFInfo string          `json:"vcf_info"`
		Hash    string          `json:"hash"`
		MAXInfo *maxWebhookUser `json:"max_info,omitempty"`
	} `json:"payload"`
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
	Payload  string          `json:"payload,omitempty"`
	User     *maxWebhookUser `json:"user,omitempty"`
	Callback *struct {
		CallbackID string          `json:"callback_id"`
		Payload    string          `json:"payload"`
		User       *maxWebhookUser `json:"user,omitempty"`
	} `json:"callback,omitempty"`
	Message *struct {
		Sender    *maxWebhookUser `json:"sender,omitempty"`
		Recipient struct {
			ChatID   json.RawMessage `json:"chat_id"`
			ChatType string          `json:"chat_type"`
		} `json:"recipient"`
		Body struct {
			MID         string                 `json:"mid"`
			Attachments []maxWebhookAttachment `json:"attachments,omitempty"`
		} `json:"body"`
	} `json:"message,omitempty"`
}

const maxAuthConfirmPayloadPrefix = "auth_contact_confirm_"

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
	if update.UpdateType == "message_created" {
		s.handleMessageCreated(w, r, update)
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

func (s *Server) handleMessageCreated(w http.ResponseWriter, r *http.Request, update maxUpdate) {
	if update.Message != nil && update.Message.Recipient.ChatType == "dialog" {
		s.handleMAXAuthContactMessage(w, r, update)
		return
	}
	// Unlike bot lifecycle updates, message_created carries the chat identity
	// inside message.recipient. Trust the authenticated MAX recipient type, not
	// optional top-level compatibility fields, so dialogs and group chats never
	// enter the channel inventory.
	if update.Message == nil || update.Message.Recipient.ChatType != "channel" {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	chatID, err := webhookChatID(update.Message.Recipient.ChatID)
	if err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	eventAt, valid := maxEventTime(update.Timestamp, s.now().UTC())
	if !valid {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	ctx, cancel := contextWithTimeout(r, 8*time.Second)
	err = s.app.DiscoverMAXChatFromMessage(ctx, chatID, eventAt)
	cancel()
	if err != nil {
		s.writeError(w, err)
		return
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
	if err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	if strings.HasPrefix(update.Payload, "auth_") {
		s.handleMAXAuthBotStarted(w, r, update, maxUserID, strings.TrimPrefix(update.Payload, "auth_"))
		return
	}
	if strings.HasPrefix(update.Payload, "link_") {
		s.handleMAXIdentityBotStarted(w, r, maxUserID, strings.TrimPrefix(update.Payload, "link_"))
		return
	}
	if !strings.HasPrefix(update.Payload, "claim_") {
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

func (s *Server) handleMAXAuthBotStarted(w http.ResponseWriter, r *http.Request, update maxUpdate, maxUserID, deepToken string) {
	if len(deepToken) < 32 || len(deepToken) > 96 {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	eventAt, valid := maxEventTime(update.Timestamp, s.now().UTC())
	if !valid {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	now := s.now().UTC()
	attempt, first, err := s.app.Store().StartMAXAuthContact(r.Context(), sha256Hex(deepToken), maxUserID, eventAt, now)
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
		err = s.app.SendMAXAuthContactRequest(ctx, maxUserID, attempt.ComparisonCode,
			maxAuthConfirmPayloadPrefix+deepToken)
		cancel()
		if err != nil {
			_ = s.app.Store().ResetMAXAuthContactStart(r.Context(), attempt.ID, maxUserID, now)
			s.writeError(w, err)
			return
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMAXAuthContactMessage(w http.ResponseWriter, r *http.Request, update maxUpdate) {
	if update.Message == nil || update.Message.Sender == nil || strings.TrimSpace(update.Message.Body.MID) == "" {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	maxUserID, err := webhookUserID(update.Message.Sender)
	if err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	var contact *maxWebhookAttachment
	for index := range update.Message.Body.Attachments {
		attachment := &update.Message.Body.Attachments[index]
		if attachment.Type != "contact" {
			continue
		}
		if contact != nil {
			s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
			return
		}
		contact = attachment
	}
	if contact == nil || contact.Payload.MAXInfo == nil || contact.Payload.Hash == "" || contact.Payload.VCFInfo == "" {
		// Manually shared/forwarded contacts deliberately have no hash and are
		// never accepted as authentication proof.
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	contactMAXUserID, err := webhookUserID(contact.Payload.MAXInfo)
	if err != nil || contactMAXUserID != maxUserID || !s.app.VerifyMAXAuthContact(contact.Payload.VCFInfo, contact.Payload.Hash) {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	// Validate and normalize transiently, then discard. Neither the number nor
	// its source vCard/hash is passed to storage or logging.
	if _, ok := maxclient.NormalizeVerifiedContactPhone(contact.Payload.VCFInfo); !ok {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	eventAt, valid := maxEventTime(update.Timestamp, s.now().UTC())
	if !valid {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	profile := store.MAXAuthProfile{
		MAXUserID: maxUserID,
		FirstName: safeMAXProfileValue(contact.Payload.MAXInfo.FirstName),
		LastName:  safeMAXProfileValue(contact.Payload.MAXInfo.LastName),
		Username:  safeMAXProfileValue(contact.Payload.MAXInfo.Username),
		AvatarURL: maxclient.SafeAssetURL(firstNonEmpty(contact.Payload.MAXInfo.FullAvatarURL, contact.Payload.MAXInfo.AvatarURL)),
	}
	_, _, err = s.app.Store().RecordMAXAuthContact(r.Context(), maxUserID, update.Message.Body.MID,
		eventAt, s.now().UTC(), profile)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
			return
		}
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func safeMAXProfileValue(value string) string {
	value = strings.Map(func(char rune) rune {
		if char < 0x20 || char == 0x7f {
			return -1
		}
		return char
	}, strings.TrimSpace(value))
	runes := []rune(value)
	if len(runes) > 255 {
		value = string(runes[:255])
	}
	return value
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
	if strings.HasPrefix(payload, maxAuthConfirmPayloadPrefix) {
		s.handleMAXAuthContactCallback(w, r, update, maxUserID)
		return
	}
	if strings.HasPrefix(payload, "link_confirm_") || strings.HasPrefix(payload, "link_cancel_") {
		s.handleMAXIdentityCallback(w, r, update, maxUserID)
		return
	}
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
		messageText := notification
		switch claim.Status {
		case store.ChannelClaimConnected:
			messageText = "✅ Готово! Канал подключён к MaxPosty.\n\nВернитесь в MaxPosty — теперь в него можно публиковать посты."
		case store.ChannelClaimFailed:
			messageText = notification + "\n\nВернитесь в MaxPosty, чтобы проверить настройки или начать заново."
		}
		if answerErr := s.app.AnswerMAXCallback(ctx, update.Callback.CallbackID, notification, messageText); answerErr != nil {
			s.logger.Warn("could not answer MAX callback", "error", answerErr)
		}
		cancel()
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMAXAuthContactCallback(w http.ResponseWriter, r *http.Request, update maxUpdate, maxUserID string) {
	token := strings.TrimPrefix(update.Callback.Payload, maxAuthConfirmPayloadPrefix)
	if len(token) < 32 || len(token) > 96 || strings.TrimSpace(update.Callback.CallbackID) == "" {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	now := s.now().UTC()
	eventAt, valid := maxEventTime(update.Timestamp, now)
	if !valid {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	attempt, _, err := s.app.Store().ConfirmMAXAuthContact(r.Context(), sha256Hex(token), maxUserID, eventAt, now)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			ctx, cancel := contextWithTimeout(r, 5*time.Second)
			answerErr := s.app.AnswerMAXCallback(ctx, update.Callback.CallbackID,
				"Запрос входа устарел. Начните вход заново на сайте MaxPosty.",
				"⏱ Этот запрос входа больше не действует.\n\nНачните новый вход на сайте MaxPosty.")
			cancel()
			if answerErr != nil {
				s.logger.Warn("could not answer expired MAX auth callback", "error", answerErr)
			}
			s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
			return
		}
		s.writeError(w, err)
		return
	}
	if attempt.Status != store.MAXAuthAttemptVerified {
		// Keep the keyboard: the user can share the signed contact and retry this
		// exact callback without restarting the browser flow.
		ctx, cancel := contextWithTimeout(r, 5*time.Second)
		answerErr := s.app.AnswerMAXCallback(ctx, update.Callback.CallbackID,
			"Сначала нажмите «1. Поделиться контактом», затем повторите подтверждение.", "")
		cancel()
		if answerErr != nil {
			s.logger.Warn("could not answer early MAX auth callback", "error", answerErr)
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "awaiting_contact": true})
		return
	}
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	answerErr := s.app.AnswerMAXCallback(ctx, update.Callback.CallbackID,
		"Вход подтверждён. Вернитесь в MaxPosty.",
		"✅ Вход подтверждён.\n\nВернитесь в открытое окно MaxPosty — вход завершится автоматически.")
	cancel()
	if answerErr != nil {
		s.logger.Warn("could not answer MAX auth callback", "error", answerErr)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMAXIdentityBotStarted(w http.ResponseWriter, r *http.Request, maxUserID, deepToken string) {
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
	attempt, first, err := s.app.Store().StartMAXIdentityLinkConfirmation(r.Context(), sha256Hex(deepToken), maxUserID,
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
		err = s.app.SendMAXIdentityLinkConfirmation(ctx, maxUserID, attempt.RequesterLabel, attempt.ComparisonCode,
			"link_confirm_"+confirmToken, "link_cancel_"+cancelToken)
		cancel()
		if err != nil {
			_ = s.app.Store().FailMAXIdentityLinkAttempt(r.Context(), attempt.UserID, attempt.ID, "confirmation_send_failed", now)
			s.writeError(w, err)
			return
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMAXIdentityCallback(w http.ResponseWriter, r *http.Request, update maxUpdate, maxUserID string) {
	payload := update.Callback.Payload
	confirm := strings.HasPrefix(payload, "link_confirm_")
	token := strings.TrimPrefix(payload, "link_confirm_")
	if !confirm {
		token = strings.TrimPrefix(payload, "link_cancel_")
	}
	if len(token) < 32 || len(token) > 96 {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	attempt, err := s.app.Store().ConfirmMAXIdentityLink(r.Context(), sha256Hex(token), maxUserID, confirm, s.now().UTC())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
			return
		}
		s.writeError(w, err)
		return
	}
	notification := "Профиль MAX связан с MaxPosty. Вернитесь в личный кабинет."
	if attempt.Status == store.MAXIdentityAttemptFailed {
		switch attempt.ErrorCode {
		case "canceled":
			notification = "Связывание профиля отменено."
		case "max_identity_already_linked":
			notification = "Этот профиль MAX уже связан с другим аккаунтом MaxPosty."
		case "owner_identity_already_linked":
			notification = "К этому аккаунту MaxPosty уже привязан другой профиль MAX."
		default:
			notification = "Не удалось связать профиль MAX."
		}
	}
	if update.Callback.CallbackID != "" {
		ctx, cancel := contextWithTimeout(r, 5*time.Second)
		messageText := "✅ Готово! Профиль MAX связан с MaxPosty.\n\nВернитесь в MaxPosty — теперь можно подключить канал."
		if attempt.Status == store.MAXIdentityAttemptFailed {
			messageText = notification + "\n\nВернитесь в MaxPosty, чтобы проверить данные или начать заново."
		}
		if answerErr := s.app.AnswerMAXCallback(ctx, update.Callback.CallbackID, notification, messageText); answerErr != nil {
			s.logger.Warn("could not answer MAX identity callback", "error", answerErr)
		}
		cancel()
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func webhookUserID(user *maxWebhookUser) (string, error) {
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
