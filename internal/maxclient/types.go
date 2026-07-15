package maxclient

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Format controls how MAX interprets message text.
type Format string

const (
	FormatMarkdown Format = "markdown"
	FormatHTML     Format = "html"
)

// BotInfo is the subset of GET /me returned by MAX that is useful to the
// application. Zero values also cover nullable fields from the API.
type BotInfo struct {
	UserID           int64        `json:"user_id"`
	FirstName        string       `json:"first_name"`
	LastName         string       `json:"last_name,omitempty"`
	Username         string       `json:"username,omitempty"`
	IsBot            bool         `json:"is_bot"`
	LastActivityTime int64        `json:"last_activity_time,omitempty"`
	Name             string       `json:"name,omitempty"`
	Description      string       `json:"description,omitempty"`
	AvatarURL        string       `json:"avatar_url,omitempty"`
	FullAvatarURL    string       `json:"full_avatar_url,omitempty"`
	Commands         []BotCommand `json:"commands,omitempty"`
}

type BotCommand struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ChatIcon is the channel image returned by MAX chat metadata endpoints.
type ChatIcon struct {
	URL string `json:"url"`
}

// ChatInfo is the subset of GET /chats/{chatId} used to verify a configured
// channel without creating a visible test message.
type ChatInfo struct {
	ChatID            string   `json:"chat_id"`
	OwnerID           string   `json:"owner_id"`
	Type              string   `json:"type"`
	Status            string   `json:"status"`
	Title             string   `json:"title"`
	Link              string   `json:"link,omitempty"`
	Icon              ChatIcon `json:"icon,omitempty"`
	ParticipantsCount int      `json:"participants_count,omitempty"`
}

type Permission string

const (
	PermissionReadAllMessages Permission = "read_all_messages"
	PermissionWrite           Permission = "write"
	PermissionEdit            Permission = "edit"
	PermissionDelete          Permission = "delete"
	PermissionPinMessage      Permission = "pin_message"
)

// Membership is the read-only result of GET /chats/{chatId}/members/me.
type Membership struct {
	UserID      int64        `json:"user_id"`
	FirstName   string       `json:"first_name"`
	Username    string       `json:"username,omitempty"`
	IsBot       bool         `json:"is_bot"`
	IsOwner     bool         `json:"is_owner"`
	IsAdmin     bool         `json:"is_admin"`
	Permissions []Permission `json:"permissions,omitempty"`
}

// ChatMember is the subset of the official ChatMember object needed when the
// chat metadata omits owner_id. GET /chats/{chatId}/members/admins is the
// authoritative fallback for distinguishing the owner from ordinary admins.
type ChatMember struct {
	UserID      int64        `json:"user_id"`
	FirstName   string       `json:"first_name"`
	LastName    string       `json:"last_name,omitempty"`
	Username    string       `json:"username,omitempty"`
	IsBot       bool         `json:"is_bot"`
	IsOwner     bool         `json:"is_owner"`
	IsAdmin     bool         `json:"is_admin"`
	Permissions []Permission `json:"permissions,omitempty"`
}

// HasPermission accepts both current permission names and legacy names that
// MAX can still return for existing channel administrators.
func (m Membership) HasPermission(required Permission) bool {
	for _, permission := range m.Permissions {
		if permission == required {
			return true
		}
		switch required {
		case PermissionWrite:
			if permission == "post_edit_delete_message" {
				return true
			}
		case PermissionEdit:
			if permission == PermissionWrite || permission == "post_edit_delete_message" || permission == "edit_message" {
				return true
			}
		case PermissionDelete:
			if permission == PermissionWrite || permission == "post_edit_delete_message" || permission == "delete_message" {
				return true
			}
		}
	}
	return false
}

// UploadResult contains the token accepted by attachments.payload.token.
type UploadResult struct {
	Token string
}

// Message identifies a post created through Publish.
type Message struct {
	MessageID string
	URL       string
	Text      string
	ChatID    string
	Views     *int64
}

// PublishRequest describes a new channel post. ImageTokens are tokens returned
// by UploadImage. DisableLinkPreview is always sent as a query parameter.
type PublishRequest struct {
	ChatID             string
	Text               string
	Format             Format
	ImageTokens        []string
	LinkButtons        []LinkButton
	DisableLinkPreview bool
	Notify             *bool
}

// EditRequest replaces the editable fields of an existing post. When both
// attachment slices are nil, MAX attachments are left unchanged. Otherwise the
// two slices describe the complete desired image + keyboard state.
type EditRequest struct {
	MessageID   string
	Text        string
	Format      Format
	ImageTokens []string
	LinkButtons []LinkButton
	Notify      *bool
}

// Error is an HTTP/API error returned by MAX. In particular, rate-limit and
// server errors keep their original status instead of being retried or hidden.
type Error struct {
	StatusCode int
	Code       string
	Message    string
	RetryAfter time.Duration
	RequestID  string
	Body       string
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}

	detail := e.Message
	if detail == "" {
		detail = e.Body
	}
	if detail == "" {
		detail = "request failed"
	}

	if e.Code != "" {
		return fmt.Sprintf("MAX API error (status %d, code %s): %s", e.StatusCode, e.Code, detail)
	}
	return fmt.Sprintf("MAX API error (status %d): %s", e.StatusCode, detail)
}

// Temporary reports errors for which a caller may choose to retry.
func (e *Error) Temporary() bool {
	return e != nil && (e.StatusCode == 429 || e.StatusCode >= 500)
}

type attachment struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

type attachmentPayload struct {
	Token string `json:"token"`
}

type messageBody struct {
	Text        string        `json:"text"`
	Attachments *[]attachment `json:"attachments,omitempty"`
	Notify      *bool         `json:"notify,omitempty"`
	Format      Format        `json:"format,omitempty"`
}

type apiMessage struct {
	MessageID string          `json:"message_id,omitempty"`
	Mid       string          `json:"mid,omitempty"`
	URL       string          `json:"url,omitempty"`
	ChatID    json.RawMessage `json:"chat_id,omitempty"`
	Recipient *struct {
		ChatID json.RawMessage `json:"chat_id,omitempty"`
	} `json:"recipient,omitempty"`
	Stat *struct {
		Views *int64 `json:"views,omitempty"`
	} `json:"stat,omitempty"`
	Body *struct {
		Mid  string `json:"mid,omitempty"`
		Text string `json:"text,omitempty"`
	} `json:"body,omitempty"`
}

func (m apiMessage) publicMessage() Message {
	id := m.MessageID
	if id == "" {
		id = m.Mid
	}

	var text, chatID string
	if m.Body != nil {
		if id == "" {
			id = m.Body.Mid
		}
		text = m.Body.Text
	}
	if len(m.ChatID) != 0 {
		chatID = jsonCode(m.ChatID)
	}
	if m.Recipient != nil && len(m.Recipient.ChatID) != 0 {
		chatID = jsonCode(m.Recipient.ChatID)
	}
	var views *int64
	if m.Stat != nil {
		views = m.Stat.Views
	}

	return Message{MessageID: id, URL: m.URL, Text: text, ChatID: chatID, Views: views}
}

func imageAttachments(tokens []string) (*[]attachment, error) {
	if tokens == nil {
		return nil, nil
	}

	result := make([]attachment, len(tokens))
	for i, token := range tokens {
		if token == "" {
			return nil, fmt.Errorf("image token %d is empty", i)
		}
		result[i] = attachment{
			Type:    "image",
			Payload: attachmentPayload{Token: token},
		}
	}
	return &result, nil
}

func validFormat(format Format) bool {
	return format == "" || format == FormatMarkdown || format == FormatHTML
}

func jsonCode(raw []byte) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}

	if raw[0] == '"' {
		if value, err := strconv.Unquote(string(raw)); err == nil {
			return value
		}
	}
	return string(raw)
}
