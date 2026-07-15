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

	// MAXPublicationMissingLastError is persisted when a publication that was
	// previously sent successfully no longer exists in MAX. Keeping a stable,
	// user-facing marker lets API clients distinguish this recoverable state
	// from an ordinary publication failure without adding another lifecycle
	// status or discarding the original publication timestamp.
	MAXPublicationMissingLastError = "Публикация удалена из MAX"
)

type Channel struct {
	ID                 int64     `json:"id"`
	UserID             string    `json:"-"`
	VerifiedMAXOwnerID string    `json:"-"`
	MAXChatID          string    `json:"max_chat_id"`
	Title              string    `json:"title"`
	PublicLink         string    `json:"public_link,omitempty"`
	IconURL            string    `json:"icon_url,omitempty"`
	ParticipantsCount  int       `json:"participants_count"`
	IsChannel          bool      `json:"is_channel"`
	Active             bool      `json:"active"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// ObservedBotChat is inventory learned from authenticated MAX bot lifecycle
// and channel-message webhooks. It never grants a tenant ownership by itself.
type ObservedBotChat struct {
	MAXChatID         string
	PublicLink        string
	Title             string
	MAXOwnerID        string
	IconURL           string
	ParticipantsCount int
	Active            bool
	LastSeenAt        time.Time
	RemovedAt         *time.Time
}

const (
	MAXIdentityAttemptPending              = "pending"
	MAXIdentityAttemptAwaitingConfirmation = "awaiting_confirmation"
	MAXIdentityAttemptLinked               = "linked"
	MAXIdentityAttemptFailed               = "failed"
	MAXIdentityAttemptExpired              = "expired"
)

// MAXIdentityLink is the durable, one-to-one association established after a
// signed-in Yandex user explicitly confirms a one-time proof in MAX.
type MAXIdentityLink struct {
	UserID    string    `json:"-"`
	MAXUserID string    `json:"max_user_id"`
	LinkedAt  time.Time `json:"linked_at"`
	UpdatedAt time.Time `json:"-"`
}

// MAXIdentityLinkAttempt contains only hashes of the deep-link and callback
// bearer tokens. Raw tokens are returned once to the initiating browser or
// sent to MAX and must never be persisted.
type MAXIdentityLinkAttempt struct {
	ID               string     `json:"request_id"`
	TokenHash        string     `json:"-"`
	ConfirmTokenHash string     `json:"-"`
	CancelTokenHash  string     `json:"-"`
	UserID           string     `json:"-"`
	RequesterLabel   string     `json:"requester_label"`
	ComparisonCode   string     `json:"comparison_code"`
	Status           string     `json:"status"`
	MAXUserID        string     `json:"max_user_id,omitempty"`
	ErrorCode        string     `json:"error_code,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	ExpiresAt        time.Time  `json:"expires_at"`
	ConsumedAt       *time.Time `json:"-"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

type DiscoverableChannel struct {
	MAXChatID          string `json:"max_chat_id"`
	Title              string `json:"title"`
	PublicLink         string `json:"public_link,omitempty"`
	IconURL            string `json:"icon_url,omitempty"`
	ParticipantsCount  int    `json:"participants_count"`
	OwnerVerified      bool   `json:"owner_verified"`
	Connected          bool   `json:"connected"`
	ConnectedChannelID *int64 `json:"connected_channel_id,omitempty"`
}

type Post struct {
	ID                  int64        `json:"id"`
	UserID              string       `json:"-"`
	Title               string       `json:"title"`
	Content             string       `json:"content"`
	Format              string       `json:"format"`
	Status              string       `json:"status"`
	ChannelID           *int64       `json:"channel_id,omitempty"`
	ImageURL            string       `json:"image_url,omitempty"`
	ImagePath           string       `json:"-"`
	ImagePrompt         string       `json:"image_prompt,omitempty"`
	LinkButtons         []LinkButton `json:"link_buttons"`
	Notify              bool         `json:"notify"`
	DisableLinkPreview  bool         `json:"disable_link_preview"`
	ScheduledAt         *time.Time   `json:"scheduled_at,omitempty"`
	MAXMessageID        string       `json:"max_message_id,omitempty"`
	MAXMessageURL       string       `json:"max_message_url"`
	MAXViews            *int64       `json:"max_views"`
	MAXStatsSyncedAt    *time.Time   `json:"max_stats_synced_at"`
	MAXStatsAttemptedAt *time.Time   `json:"-"`
	MAXIsPinned         bool         `json:"max_is_pinned"`
	LastError           string       `json:"last_error,omitempty"`
	CreatedAt           time.Time    `json:"created_at"`
	UpdatedAt           time.Time    `json:"updated_at"`
	PublishedAt         *time.Time   `json:"published_at,omitempty"`
}

type PostChanges struct {
	Title              *string
	Content            *string
	Format             *string
	ChannelID          **int64
	ImageURL           *string
	ImagePath          *string
	ImagePrompt        *string
	LinkButtons        *[]LinkButton
	Notify             *bool
	DisableLinkPreview *bool
	ScheduledAt        **time.Time
}

// AuthSession is a provider-neutral server-side login session. TokenHash contains the
// SHA-256 hex digest of the opaque browser token; the token itself must never
// be persisted.
type AuthSession struct {
	TokenHash       string
	OwnerID         string
	Provider        string
	ProviderSubject string
	// YandexUserID is kept as a source-compatibility alias for older callers.
	// New code must use OwnerID; it is populated when sessions are read.
	YandexUserID      string
	Login             string
	Email             string
	DisplayName       string
	AvatarURL         string
	AllowlistIdentity string
	CreatedAt         time.Time
	ExpiresAt         time.Time
}

const (
	MAXAuthAttemptPending         = "pending"
	MAXAuthAttemptAwaitingContact = "awaiting_contact"
	MAXAuthAttemptVerified        = "verified"
	MAXAuthAttemptAuthenticated   = "authenticated"
	MAXAuthAttemptCancelled       = "canceled"
	MAXAuthAttemptFailed          = "failed"
	MAXAuthAttemptExpired         = "expired"
)

// MAXAuthAttempt is a browser-bound device-flow attempt. Both browser and
// deep-link secrets are persisted only as SHA-256 hashes.
type MAXAuthAttempt struct {
	ID                  string
	BrowserTokenHash    string
	DeepTokenHash       string
	ReturnTo            string
	ComparisonCode      string
	Status              string
	MAXUserID           string
	TermsVersion        string
	PersonalDataVersion string
	ConsentAt           time.Time
	ContactMessageID    string
	ContactEventAt      *time.Time
	ErrorCode           string
	CreatedAt           time.Time
	ExpiresAt           time.Time
	AuthenticatedAt     *time.Time
	UpdatedAt           time.Time
}

// MAXAuthProfile contains only the signed MAX identity data needed by the
// application. The verified phone number, vCard and contact hash are never
// stored.
type MAXAuthProfile struct {
	MAXUserID         string
	FirstName         string
	LastName          string
	Username          string
	AvatarURL         string
	ContactVerifiedAt time.Time
	UpdatedAt         time.Time
}

// User is the local account attached to a Yandex identity. ID is the stable
// app-scoped Yandex identifier and is the tenant key used by all user data.
type User struct {
	ID          string    `json:"id"`
	Login       string    `json:"login,omitempty"`
	Email       string    `json:"email,omitempty"`
	DisplayName string    `json:"display_name"`
	AvatarURL   string    `json:"avatar_url,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Consent struct {
	UserID     string    `json:"-"`
	Document   string    `json:"document"`
	Version    string    `json:"version"`
	AcceptedAt time.Time `json:"accepted_at"`
	Source     string    `json:"source"`
}

// OAuthState stores the short-lived data needed to finish one Yandex OAuth
// authorization. StateHash contains the SHA-256 hex digest of the opaque state
// sent to the browser; the state itself must never be persisted.
type OAuthState struct {
	StateHash           string
	PKCEVerifier        string
	ReturnTo            string
	TermsVersion        string
	PersonalDataVersion string
	ConsentAt           time.Time
	CreatedAt           time.Time
	ExpiresAt           time.Time
}

const (
	ChannelClaimPending              = "pending"
	ChannelClaimAwaitingConfirmation = "awaiting_confirmation"
	ChannelClaimIdentityVerified     = "identity_verified"
	ChannelClaimConnected            = "connected"
	ChannelClaimFailed               = "failed"
	ChannelClaimExpired              = "expired"
)

// ChannelClaim binds one opaque MAX deep-link proof to one authenticated
// tenant and one resolved MAX channel. TokenHash is persisted instead of the
// raw deep-link token.
type ChannelClaim struct {
	ID               string     `json:"claim_id"`
	TokenHash        string     `json:"-"`
	ConfirmTokenHash string     `json:"-"`
	CancelTokenHash  string     `json:"-"`
	UserID           string     `json:"-"`
	MAXChatID        string     `json:"max_chat_id"`
	PublicLink       string     `json:"public_link,omitempty"`
	RequestedTitle   string     `json:"-"`
	RequesterLabel   string     `json:"requester_label"`
	ComparisonCode   string     `json:"comparison_code"`
	Status           string     `json:"status"`
	MAXUserID        string     `json:"-"`
	ChannelID        *int64     `json:"channel_id,omitempty"`
	ErrorCode        string     `json:"error_code,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	ExpiresAt        time.Time  `json:"expires_at"`
	ConsumedAt       *time.Time `json:"-"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

func ValidFormat(format string) bool {
	return format == FormatMarkdown || format == FormatHTML
}
