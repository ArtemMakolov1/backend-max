package store

import (
	"encoding/json"
	"time"
)

const (
	FormatMarkdown = "markdown"
	FormatHTML     = "html"

	PostStatusDraft      = "draft"
	PostStatusScheduled  = "scheduled"
	PostStatusPublishing = "publishing"
	PostStatusPublished  = "published"
	PostStatusFailed     = "failed"

	PostAttachmentImage = "image"
	PostAttachmentVideo = "video"

	AttachmentStatusUploading  = "uploading"
	AttachmentStatusProcessing = "processing"
	AttachmentStatusReady      = "ready"
	AttachmentStatusFailed     = "failed"

	// MAX accepts up to twelve message attachments. An inline keyboard is an
	// attachment too, so a post with link buttons has one fewer media slot.
	MaxPostAttachments             = 12
	MaxPostAttachmentsWithKeyboard = 11

	// MAXPublicationMissingLastError is persisted when a publication that was
	// previously sent successfully no longer exists in MAX. Keeping a stable,
	// user-facing marker lets API clients distinguish this recoverable state
	// from an ordinary publication failure without adding another lifecycle
	// status or discarding the original publication timestamp.
	MAXPublicationMissingLastError = "Публикация удалена из MAX"
)

// PostAttachment is the public attachment DTO embedded in Post responses.
// Storage and provider fields are deliberately hidden from JSON: storage keys
// are implementation details and MAX upload tokens are opaque provider credentials.
type PostAttachment struct {
	ID               int64           `json:"id"`
	OwnerID          string          `json:"-"`
	PostID           int64           `json:"-"`
	Type             string          `json:"type"`
	Position         int             `json:"position"`
	URL              string          `json:"url"`
	StorageKey       string          `json:"-"`
	ProcessingStatus string          `json:"processing_status"`
	SizeBytes        int64           `json:"size_bytes"`
	MIMEType         string          `json:"mime_type"`
	Width            *int            `json:"width,omitempty"`
	Height           *int            `json:"height,omitempty"`
	DurationMS       *int64          `json:"duration_ms,omitempty"`
	ProviderToken    string          `json:"-"`
	ProviderExpires  *time.Time      `json:"-"`
	ProviderMeta     json.RawMessage `json:"-"`
	ErrorCode        string          `json:"-"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

type Channel struct {
	ID                 int64     `json:"id"`
	UserID             string    `json:"-"`
	WorkspaceID        string    `json:"workspace_id,omitempty"`
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

// DiscoverableChannelRefreshCandidates keeps tenant-associated inventory
// separate from the small global fallback needed to recover legacy rows whose
// MAX owner was temporarily missing. Unknown rows must never consume the
// tenant-owned quota or be exposed directly to the requester.
type DiscoverableChannelRefreshCandidates struct {
	Owned   []ObservedBotChat
	Unknown []ObservedBotChat
}

type Post struct {
	ID                  int64            `json:"id"`
	UserID              string           `json:"-"`
	WorkspaceID         string           `json:"workspace_id,omitempty"`
	Title               string           `json:"title"`
	Content             string           `json:"content"`
	Format              string           `json:"format"`
	Status              string           `json:"status"`
	ChannelID           *int64           `json:"channel_id,omitempty"`
	ImageURL            string           `json:"image_url,omitempty"`
	ImagePath           string           `json:"-"`
	ImagePrompt         string           `json:"image_prompt,omitempty"`
	Attachments         []PostAttachment `json:"attachments"`
	LinkButtons         []LinkButton     `json:"link_buttons"`
	Notify              bool             `json:"notify"`
	DisableLinkPreview  bool             `json:"disable_link_preview"`
	ScheduledAt         *time.Time       `json:"scheduled_at,omitempty"`
	MAXMessageID        string           `json:"max_message_id,omitempty"`
	MAXMessageURL       string           `json:"max_message_url"`
	MAXViews            *int64           `json:"max_views"`
	MAXStatsSyncedAt    *time.Time       `json:"max_stats_synced_at"`
	MAXStatsAttemptedAt *time.Time       `json:"-"`
	MAXIsPinned         bool             `json:"max_is_pinned"`
	LastError           string           `json:"last_error,omitempty"`
	CreatedAt           time.Time        `json:"created_at"`
	UpdatedAt           time.Time        `json:"updated_at"`
	PublishedAt         *time.Time       `json:"published_at,omitempty"`
	ReviewStatus        string           `json:"review_status,omitempty"`
	CurrentRevisionID   *int64           `json:"current_revision_id,omitempty"`
}

const (
	WorkspaceRoleOwner    = "owner"
	WorkspaceRoleEditor   = "editor"
	WorkspaceRoleApprover = "approver"
	WorkspaceRoleViewer   = "viewer"

	InvitationStatusPending  = "pending"
	InvitationStatusAccepted = "accepted"
	InvitationStatusRevoked  = "revoked"
	InvitationStatusExpired  = "expired"

	ReviewStatusDraft            = "draft"
	ReviewStatusInReview         = "in_review"
	ReviewStatusChangesRequested = "changes_requested"
	ReviewStatusApproved         = "approved"

	ReviewDecisionApproved         = "approved"
	ReviewDecisionChangesRequested = "changes_requested"
)

// Workspace is the tenant boundary for channels, posts and collaborative
// state. OwnerUserID is lifecycle ownership; authorization always comes from
// workspace_members rather than this field alone.
type Workspace struct {
	ID                      string     `json:"id"`
	Name                    string     `json:"name"`
	OwnerUserID             string     `json:"owner_user_id"`
	CompatOwnerUserID       string     `json:"-"`
	IsPersonal              bool       `json:"is_personal"`
	ApprovalRequired        bool       `json:"approval_required"`
	RequireDistinctApprover bool       `json:"require_distinct_approver"`
	CreatedBy               string     `json:"created_by,omitempty"`
	CreatedAt               time.Time  `json:"created_at"`
	UpdatedAt               time.Time  `json:"updated_at"`
	ArchivedAt              *time.Time `json:"archived_at,omitempty"`
}

type WorkspaceChanges struct {
	Name                    *string
	ApprovalRequired        *bool
	RequireDistinctApprover *bool
}

type WorkspaceMember struct {
	WorkspaceID string    `json:"workspace_id"`
	UserID      string    `json:"user_id"`
	Role        string    `json:"role"`
	CreatedBy   string    `json:"created_by,omitempty"`
	JoinedAt    time.Time `json:"joined_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	DisplayName string    `json:"display_name,omitempty"`
	Email       string    `json:"email,omitempty"`
	AvatarURL   string    `json:"avatar_url,omitempty"`
}

type WorkspaceCapabilities struct {
	ManageWorkspace bool `json:"manage_workspace"`
	ManageMembers   bool `json:"manage_members"`
	EditContent     bool `json:"edit_content"`
	SubmitReview    bool `json:"submit_review"`
	ApproveReview   bool `json:"approve_review"`
	Comment         bool `json:"comment"`
	ViewAudit       bool `json:"view_audit"`
}

type WorkspaceAccess struct {
	Workspace    Workspace             `json:"workspace"`
	Member       WorkspaceMember       `json:"member"`
	Capabilities WorkspaceCapabilities `json:"capabilities"`
}

type WorkspaceInvitation struct {
	ID           string     `json:"id"`
	WorkspaceID  string     `json:"workspace_id"`
	Email        string     `json:"email"`
	TargetUserID string     `json:"target_user_id,omitempty"`
	TokenHash    string     `json:"-"`
	Role         string     `json:"role"`
	Status       string     `json:"status"`
	InvitedBy    string     `json:"invited_by"`
	AcceptedBy   string     `json:"accepted_by,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	AcceptedAt   *time.Time `json:"accepted_at,omitempty"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
}

type PostRevision struct {
	ID                int64           `json:"id"`
	WorkspaceID       string          `json:"workspace_id"`
	PostID            int64           `json:"post_id"`
	Number            int             `json:"number"`
	AuthorUserID      string          `json:"author_user_id"`
	AuthorDisplayName string          `json:"author_display_name,omitempty"`
	Snapshot          json.RawMessage `json:"snapshot"`
	CreatedAt         time.Time       `json:"created_at"`
}

type PostReview struct {
	ID                  int64     `json:"id"`
	WorkspaceID         string    `json:"workspace_id"`
	PostID              int64     `json:"post_id"`
	RevisionID          int64     `json:"revision_id"`
	ReviewerUserID      string    `json:"reviewer_user_id"`
	ReviewerDisplayName string    `json:"reviewer_display_name,omitempty"`
	Decision            string    `json:"decision"`
	Comment             string    `json:"comment,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
}

type PostComment struct {
	ID                    int64      `json:"id"`
	WorkspaceID           string     `json:"workspace_id"`
	PostID                int64      `json:"post_id"`
	RevisionID            *int64     `json:"revision_id,omitempty"`
	ParentID              *int64     `json:"parent_id,omitempty"`
	AuthorUserID          string     `json:"author_user_id"`
	AuthorDisplayName     string     `json:"author_display_name,omitempty"`
	Body                  string     `json:"body"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
	DeletedAt             *time.Time `json:"deleted_at,omitempty"`
	ResolvedAt            *time.Time `json:"resolved_at,omitempty"`
	ResolvedByUserID      string     `json:"resolved_by_user_id,omitempty"`
	ResolvedByDisplayName string     `json:"resolved_by_display_name,omitempty"`
}

type AuditEvent struct {
	ID               int64           `json:"id"`
	WorkspaceID      string          `json:"workspace_id"`
	ActorUserID      string          `json:"actor_user_id,omitempty"`
	ActorDisplayName string          `json:"actor_display_name,omitempty"`
	Action           string          `json:"action"`
	EntityType       string          `json:"entity_type"`
	EntityID         string          `json:"entity_id,omitempty"`
	Metadata         json.RawMessage `json:"metadata"`
	CreatedAt        time.Time       `json:"created_at"`
}

type Notification struct {
	ID          int64           `json:"id"`
	WorkspaceID string          `json:"workspace_id"`
	UserID      string          `json:"user_id"`
	Kind        string          `json:"kind"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	EntityType  string          `json:"entity_type,omitempty"`
	EntityID    string          `json:"entity_id,omitempty"`
	Metadata    json.RawMessage `json:"metadata"`
	DedupeKey   string          `json:"-"`
	ReadAt      *time.Time      `json:"read_at,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

type WorkspaceMediaUsage struct {
	WorkspaceID string    `json:"workspace_id"`
	AssetCount  int64     `json:"asset_count"`
	TotalBytes  int64     `json:"total_bytes"`
	UpdatedAt   time.Time `json:"updated_at"`
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
	WorkspaceID      string     `json:"workspace_id,omitempty"`
	ActorUserID      string     `json:"-"`
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
