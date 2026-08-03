package maxclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// HistoryMessage is one message returned by the MAX chat-history endpoint.
// Numeric provider identifiers remain strings so callers can pass them through
// browser clients without losing int64 precision.
type HistoryMessage struct {
	MessageID       string
	ChatID          string
	Text            string
	TimestampMillis int64
	URL             string
	Views           *int64
	SenderUserID    string
	SenderIsBot     bool
	Attachments     []HistoryAttachment
	Raw             json.RawMessage `json:"-"`
}

// HistoryAttachment is the lossless-enough, normalized subset needed to
// decide whether an existing MAX attachment can safely be sent back on edit.
// Raw and Token are provider-internal values and must not be serialized to a
// browser response directly.
type HistoryAttachment struct {
	Type        string
	Token       string `json:"-"`
	URL         string
	Width       *int
	Height      *int
	DurationMS  *int64
	LinkButtons []LinkButton
	Complete    bool
	Raw         json.RawMessage `json:"-"`
}

type historyMessageWire struct {
	MessageID string          `json:"message_id,omitempty"`
	MID       string          `json:"mid,omitempty"`
	URL       string          `json:"url,omitempty"`
	ChatID    json.RawMessage `json:"chat_id,omitempty"`
	Timestamp int64           `json:"timestamp"`
	Sender    *struct {
		UserID json.RawMessage `json:"user_id,omitempty"`
		IsBot  bool            `json:"is_bot,omitempty"`
	} `json:"sender,omitempty"`
	Recipient *struct {
		ChatID json.RawMessage `json:"chat_id,omitempty"`
	} `json:"recipient,omitempty"`
	Stat *struct {
		Views *int64 `json:"views,omitempty"`
	} `json:"stat,omitempty"`
	Body *struct {
		MID         string            `json:"mid,omitempty"`
		Text        string            `json:"text,omitempty"`
		Attachments []json.RawMessage `json:"attachments,omitempty"`
	} `json:"body,omitempty"`
}

type historyAttachmentWire struct {
	Type     string          `json:"type"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	Width    *int            `json:"width,omitempty"`
	Height   *int            `json:"height,omitempty"`
	Duration *int64          `json:"duration,omitempty"`
}

// GetMessagesPage returns one MAX history page in provider order (newest
// first). before is the inclusive upper Unix-millisecond boundary documented
// by MAX; zero requests the newest page.
func (c *Client) GetMessagesPage(
	ctx context.Context,
	chatID string,
	before int64,
	count int,
) ([]HistoryMessage, error) {
	if !numericID(chatID) {
		return nil, errors.New("get MAX message history: chat ID must be numeric")
	}
	if before < 0 {
		return nil, errors.New("get MAX message history: before must not be negative")
	}
	if count < 1 || count > 100 {
		return nil, errors.New("get MAX message history: count must be between 1 and 100")
	}

	query := url.Values{
		"chat_id": []string{chatID},
		"count":   []string{strconv.Itoa(count)},
	}
	if before > 0 {
		query.Set("from", strconv.FormatInt(before, 10))
	}

	var envelope struct {
		Messages json.RawMessage `json:"messages"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/messages", query, nil, &envelope); err != nil {
		return nil, err
	}
	if rawJSONNull(envelope.Messages) {
		return nil, errors.New("MAX message history response does not contain a messages array")
	}
	var rawMessages []json.RawMessage
	if err := json.Unmarshal(envelope.Messages, &rawMessages); err != nil {
		return nil, fmt.Errorf("decode MAX message history messages: %w", err)
	}
	if len(rawMessages) > count {
		return nil, fmt.Errorf("MAX message history response contains %d messages, requested at most %d", len(rawMessages), count)
	}

	messages := make([]HistoryMessage, 0, len(rawMessages))
	seen := make(map[string]struct{}, len(rawMessages))
	for index, raw := range rawMessages {
		message, err := decodeHistoryMessage(raw, chatID)
		if err != nil {
			return nil, fmt.Errorf("decode MAX message history item %d: %w", index, err)
		}
		if _, duplicate := seen[message.MessageID]; duplicate {
			return nil, fmt.Errorf("MAX message history response contains duplicate message ID %q", message.MessageID)
		}
		seen[message.MessageID] = struct{}{}
		messages = append(messages, message)
	}
	return messages, nil
}

func decodeHistoryMessage(raw json.RawMessage, requestedChatID string) (HistoryMessage, error) {
	var wire historyMessageWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return HistoryMessage{}, err
	}

	messageID, err := matchingHistoryValue("message ID", wire.MessageID, wire.MID, historyBodyMID(wire.Body))
	if err != nil {
		return HistoryMessage{}, err
	}
	if !validMessageID(messageID) {
		return HistoryMessage{}, errors.New("MAX message history item does not contain a valid message ID")
	}

	chatID, err := historyMessageChatID(wire)
	if err != nil {
		return HistoryMessage{}, err
	}
	if chatID != requestedChatID {
		return HistoryMessage{}, errors.New("MAX message history item does not belong to the requested chat")
	}
	if wire.Timestamp <= 0 {
		return HistoryMessage{}, errors.New("MAX message history item contains a non-positive timestamp")
	}
	if wire.Stat != nil && wire.Stat.Views != nil && *wire.Stat.Views < 0 {
		return HistoryMessage{}, errors.New("MAX message history item contains a negative view count")
	}

	senderUserID := ""
	senderIsBot := false
	if wire.Sender != nil {
		senderUserID = jsonCode(wire.Sender.UserID)
		if senderUserID != "" {
			parsedSenderUserID, parseErr := strconv.ParseInt(senderUserID, 10, 64)
			if parseErr != nil || parsedSenderUserID <= 0 {
				return HistoryMessage{}, errors.New("MAX message history item contains an invalid sender user ID")
			}
		}
		senderIsBot = wire.Sender.IsBot
		if senderIsBot && senderUserID == "" {
			return HistoryMessage{}, errors.New("MAX message history item identifies a bot sender without a user ID")
		}
	}

	attachments := make([]HistoryAttachment, 0)
	text := ""
	if wire.Body != nil {
		text = wire.Body.Text
		attachments = make([]HistoryAttachment, 0, len(wire.Body.Attachments))
		for index, rawAttachment := range wire.Body.Attachments {
			attachment, decodeErr := decodeHistoryAttachment(rawAttachment)
			if decodeErr != nil {
				return HistoryMessage{}, fmt.Errorf("decode attachment %d: %w", index, decodeErr)
			}
			attachments = append(attachments, attachment)
		}
	}

	return HistoryMessage{
		MessageID: messageID, ChatID: chatID, Text: text,
		TimestampMillis: wire.Timestamp, URL: SafeAssetURL(wire.URL),
		Views: historyViews(wire.Stat), SenderUserID: senderUserID, SenderIsBot: senderIsBot,
		Attachments: attachments, Raw: append(json.RawMessage(nil), raw...),
	}, nil
}

func historyBodyMID(body *struct {
	MID         string            `json:"mid,omitempty"`
	Text        string            `json:"text,omitempty"`
	Attachments []json.RawMessage `json:"attachments,omitempty"`
}) string {
	if body == nil {
		return ""
	}
	return body.MID
}

func historyViews(stat *struct {
	Views *int64 `json:"views,omitempty"`
}) *int64 {
	if stat == nil {
		return nil
	}
	return stat.Views
}

func historyMessageChatID(wire historyMessageWire) (string, error) {
	topLevel := jsonCode(wire.ChatID)
	recipient := ""
	if wire.Recipient != nil {
		recipient = jsonCode(wire.Recipient.ChatID)
	}
	chatID, err := matchingHistoryValue("chat ID", topLevel, recipient)
	if err != nil {
		return "", err
	}
	if !numericID(chatID) {
		return "", errors.New("MAX message history item does not contain a valid chat ID")
	}
	return chatID, nil
}

func matchingHistoryValue(label string, candidates ...string) (string, error) {
	result := ""
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if result == "" {
			result = candidate
			continue
		}
		if candidate != result {
			return "", fmt.Errorf("MAX message history item contains mismatched %s fields", label)
		}
	}
	return result, nil
}

func decodeHistoryAttachment(raw json.RawMessage) (HistoryAttachment, error) {
	var wire historyAttachmentWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return HistoryAttachment{}, err
	}
	attachment := HistoryAttachment{
		Type: wire.Type, Width: wire.Width, Height: wire.Height,
		Raw: append(json.RawMessage(nil), raw...),
	}
	if wire.Duration != nil {
		if *wire.Duration < 0 {
			return HistoryAttachment{}, errors.New("MAX attachment contains a negative duration")
		}
		if *wire.Duration > math.MaxInt64/1000 {
			return HistoryAttachment{}, errors.New("MAX attachment duration overflows milliseconds")
		}
		durationMillis := *wire.Duration * 1000
		attachment.DurationMS = &durationMillis
	}

	switch wire.Type {
	case string(MediaTypeImage), string(MediaTypeVideo):
		var payload struct {
			Token string `json:"token,omitempty"`
			URL   string `json:"url,omitempty"`
		}
		if !rawJSONNull(wire.Payload) {
			if err := json.Unmarshal(wire.Payload, &payload); err != nil {
				return HistoryAttachment{}, fmt.Errorf("decode %s attachment payload: %w", wire.Type, err)
			}
		}
		attachment.Token = payload.Token
		attachment.URL = SafeAssetURL(payload.URL)
		attachment.Complete = strings.TrimSpace(payload.Token) != ""
	case "inline_keyboard":
		attachment.LinkButtons, attachment.Complete = decodeHistoryLinkButtons(wire.Payload)
	}
	return attachment, nil
}

func decodeHistoryLinkButtons(raw json.RawMessage) ([]LinkButton, bool) {
	if rawJSONNull(raw) {
		return nil, false
	}
	var payload struct {
		Buttons json.RawMessage `json:"buttons"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || rawJSONNull(payload.Buttons) {
		return nil, false
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(payload.Buttons, &rows); err != nil || len(rows) == 0 {
		return nil, false
	}

	buttons := make([]LinkButton, 0)
	complete := true
	for _, rawRow := range rows {
		var row []json.RawMessage
		if err := json.Unmarshal(rawRow, &row); err != nil || len(row) == 0 {
			complete = false
			continue
		}
		for _, rawButton := range row {
			var button inlineKeyboardButton
			if err := json.Unmarshal(rawButton, &button); err != nil || button.Type != "link" {
				complete = false
				continue
			}
			candidate := LinkButton{Text: strings.TrimSpace(button.Text), URL: strings.TrimSpace(button.URL)}
			if err := validateLinkButtons([]LinkButton{candidate}); err != nil {
				complete = false
				continue
			}
			buttons = append(buttons, candidate)
		}
	}
	if len(buttons) == 0 || len(buttons) > maxLinkButtons {
		complete = false
	}
	return buttons, complete
}

func rawJSONNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}
