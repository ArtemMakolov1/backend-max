package store

import "time"

const (
	FormatMarkdown = "markdown"
	FormatHTML     = "html"

	PostStatusDraft      = "draft"
	PostStatusScheduled  = "scheduled"
	PostStatusPublishing = "publishing"
	PostStatusPublished  = "published"
	PostStatusFailed     = "failed"
)

type Channel struct {
	ID                int64     `json:"id"`
	MAXChatID         string    `json:"max_chat_id"`
	Title             string    `json:"title"`
	PublicLink        string    `json:"public_link,omitempty"`
	IconURL           string    `json:"icon_url,omitempty"`
	ParticipantsCount int       `json:"participants_count"`
	IsChannel         bool      `json:"is_channel"`
	Active            bool      `json:"active"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type Post struct {
	ID                 int64      `json:"id"`
	Title              string     `json:"title"`
	Content            string     `json:"content"`
	Format             string     `json:"format"`
	Status             string     `json:"status"`
	ChannelID          *int64     `json:"channel_id,omitempty"`
	ImageURL           string     `json:"image_url,omitempty"`
	ImagePath          string     `json:"-"`
	ImagePrompt        string     `json:"image_prompt,omitempty"`
	Notify             bool       `json:"notify"`
	DisableLinkPreview bool       `json:"disable_link_preview"`
	ScheduledAt        *time.Time `json:"scheduled_at,omitempty"`
	MAXMessageID       string     `json:"max_message_id,omitempty"`
	LastError          string     `json:"last_error,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	PublishedAt        *time.Time `json:"published_at,omitempty"`
}

type PostChanges struct {
	Title              *string
	Content            *string
	Format             *string
	ChannelID          **int64
	ImageURL           *string
	ImagePath          *string
	ImagePrompt        *string
	Notify             *bool
	DisableLinkPreview *bool
	ScheduledAt        **time.Time
}

// AuthSession is a server-side Yandex OAuth session. TokenHash contains the
// SHA-256 hex digest of the opaque browser token; the token itself must never
// be persisted.
type AuthSession struct {
	TokenHash         string
	YandexUserID      string
	Login             string
	Email             string
	DisplayName       string
	AllowlistIdentity string
	CreatedAt         time.Time
	ExpiresAt         time.Time
}

// OAuthState stores the short-lived data needed to finish one Yandex OAuth
// authorization. StateHash contains the SHA-256 hex digest of the opaque state
// sent to the browser; the state itself must never be persisted.
type OAuthState struct {
	StateHash    string
	PKCEVerifier string
	ReturnTo     string
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

func ValidFormat(format string) bool {
	return format == FormatMarkdown || format == FormatHTML
}
