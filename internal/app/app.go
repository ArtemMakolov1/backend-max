package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/singleflight"

	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/openaiimg"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

var (
	ErrMAXNotConfigured             = errors.New("MAX integration is not configured")
	ErrMAXChannelEventRequired      = errors.New("MAX channel event is required to discover the channel id")
	ErrMAXChannelMetadataIncomplete = errors.New("MAX channel metadata is incomplete")
	ErrOpenAINotConfigured          = errors.New("OpenAI integration is not configured")
	ErrResearchNotConfigured        = errors.New("OpenAI research integration is not configured")
	ErrConflict                     = errors.New("resource state conflict")
	ErrApprovalRequired             = errors.New("the current post revision must be approved before scheduling or publishing")
	ErrNotEnoughPostsForBrandKit    = errors.New("not enough posts with text to suggest a brand kit")
	ErrBillingNotConfigured         = errors.New("billing integration is not configured")
	ErrPaymentProvider              = errors.New("payment provider request failed")
)

const (
	manualMAXStatsCooldown          = 15 * time.Second
	channelParticipantStatsInterval = time.Hour
	discoverableRefreshCooldown     = 15 * time.Second
	discoverableOwnedRefreshLimit   = 8
	discoverableUnknownRefreshLimit = 2
	incompleteObservedChatWindow    = 7 * 24 * time.Hour
	defaultMediaMaxFiles            = int64(500)
	defaultMediaMaxBytes            = int64(10 << 30)
	defaultMediaOrphanGrace         = 24 * time.Hour
	defaultMediaCleanupInterval     = 15 * time.Minute
	defaultMediaCleanupBatch        = 50
)

type MAXClient interface {
	GetMe(context.Context) (maxclient.BotInfo, error)
	EditChat(context.Context, string, maxclient.ChatPatch) (maxclient.ChatInfo, error)
	GetChat(context.Context, string) (maxclient.ChatInfo, error)
	GetChatAdmins(context.Context, string) ([]maxclient.ChatMember, error)
	GetChatByLink(context.Context, string) (maxclient.ChatInfo, error)
	GetMembership(context.Context, string) (maxclient.Membership, error)
	GetMessage(context.Context, string) (maxclient.Message, error)
	GetPinnedMessage(context.Context, string) (*maxclient.Message, error)
	PinMessage(context.Context, string, string) error
	UnpinMessage(context.Context, string) error
	SendClaimConfirmation(context.Context, string, string, string, string, string, string, string) error
	AnswerCallback(context.Context, string, string, string) error
	UploadImage(context.Context, string, io.Reader) (maxclient.UploadResult, error)
	Publish(context.Context, maxclient.PublishRequest) (maxclient.Message, error)
	Edit(context.Context, maxclient.EditRequest) error
	Delete(context.Context, string) error
}

type ChannelDiagnostics struct {
	ChatID                     string   `json:"chat_id"`
	Type                       string   `json:"type"`
	Status                     string   `json:"status"`
	IsAdmin                    bool     `json:"is_admin"`
	Permissions                []string `json:"permissions"`
	CanPublish                 bool     `json:"can_publish"`
	CanEdit                    bool     `json:"can_edit"`
	CanDelete                  bool     `json:"can_delete"`
	CanPin                     bool     `json:"can_pin"`
	CanChangeInfo              bool     `json:"can_change_info"`
	MissingRequiredPermissions []string `json:"missing_required_permissions"`
}

type ChannelCheck struct {
	Channel     store.Channel      `json:"channel"`
	Diagnostics ChannelDiagnostics `json:"diagnostics"`
}

type ChannelAccessError struct {
	Diagnostics ChannelDiagnostics
	Message     string
}

// MAXStatsCooldownError tells API clients exactly when another manual MAX
// metadata refresh is allowed. It still unwraps to ErrConflict for callers
// that only need the broader state-conflict classification.
type MAXStatsCooldownError struct {
	RetryAfter time.Duration
}

func (e *MAXStatsCooldownError) Error() string {
	return "MAX statistics were refreshed recently"
}

func (e *MAXStatsCooldownError) Unwrap() error {
	return ErrConflict
}

// DiscoverableRefreshCooldownError prevents repeated browser clicks and
// concurrent requests from multiplying calls made with the shared MAX bot
// token. RetryAfter is suitable for both the HTTP Retry-After header and UI.
type DiscoverableRefreshCooldownError struct {
	RetryAfter time.Duration
}

func (e *DiscoverableRefreshCooldownError) Error() string {
	return "MAX channel inventory was refreshed recently"
}

func (e *DiscoverableRefreshCooldownError) Unwrap() error {
	return ErrConflict
}

func (e *ChannelAccessError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "MAX channel access requirements are not satisfied"
}

type ImageClient interface {
	Generate(context.Context, openaiimg.GenerateRequest) (openaiimg.Result, error)
}

type imageRequestValidator interface {
	Validate(openaiimg.GenerateRequest) error
}

type ResearchClient interface {
	Generate(context.Context, openairesearch.Request) (openairesearch.Result, error)
}

type ContentFormatter interface {
	FormatContent(context.Context, openairesearch.FormatRequest) (openairesearch.FormatResult, error)
}

type ImagePromptSuggester interface {
	SuggestImagePrompt(context.Context, openairesearch.SuggestImagePromptRequest) (openairesearch.SuggestImagePromptResult, error)
}

type BrandKitSuggester interface {
	SuggestBrandKit(context.Context, openairesearch.SuggestBrandKitRequest) (openairesearch.SuggestBrandKitResult, error)
}

type ChannelDescriptionSuggester interface {
	SuggestChannelDescription(context.Context, openairesearch.SuggestChannelDescriptionRequest) (openairesearch.SuggestChannelDescriptionResult, error)
}

// Metrics receives only bounded operational dimensions. Implementations must
// never attach post, channel or user identifiers as metric labels.
type Metrics interface {
	ObservePublicationOperation(operation, outcome string, elapsed time.Duration)
	ObserveSchedulerJob(job, outcome string)
	SetSchedulerDue(job string, count int)
	ObserveSchedulerCycle(elapsed time.Duration, completedAt time.Time)
	AddRecoveredPublications(count int64)
	ObserveMediaOperation(operation, outcome string)
}

type noopMetrics struct{}

func (noopMetrics) ObservePublicationOperation(string, string, time.Duration) {}
func (noopMetrics) ObserveSchedulerJob(string, string)                        {}
func (noopMetrics) SetSchedulerDue(string, int)                               {}
func (noopMetrics) ObserveSchedulerCycle(time.Duration, time.Time)            {}
func (noopMetrics) AddRecoveredPublications(int64)                            {}
func (noopMetrics) ObserveMediaOperation(string, string)                      {}

type MediaPolicy struct {
	MaxFiles        int64
	MaxBytes        int64
	OrphanGrace     time.Duration
	CleanupInterval time.Duration
	CleanupBatch    int
}

type discoverableRefreshState struct {
	inFlight bool
	retryAt  time.Time
}

type App struct {
	store    *store.Store
	media    *media.Store
	max      MAXClient
	images   ImageClient
	research ResearchClient
	logger   *slog.Logger
	metrics  Metrics
	now      func() time.Time
	// messageChatDiscovery collapses webhook retries for a channel that has not
	// entered the authenticated inventory yet. Lifecycle events intentionally do
	// not use it because bot_added must refresh the stored channel metadata.
	messageChatDiscovery         singleflight.Group
	discoverableRefreshMu        sync.Mutex
	discoverableRefreshes        map[string]discoverableRefreshState
	discoverableRefreshLastSweep time.Time
	mediaPolicy                  MediaPolicy
	mediaCleanupMu               sync.Mutex
	lastMediaCleanup             time.Time
	billing                      BillingClient
	billingReturnURL             string
	billingCipher                *billingMethodCipher
	billingLiveEnabled           bool
	billingManualReviewMu        sync.Mutex
	billingManualReviewLastCount int
	billingManualReviewLastLog   time.Time
}

func New(storage *store.Store, mediaStore *media.Store, max MAXClient, images ImageClient, research ResearchClient, logger *slog.Logger) *App {
	return NewWithMetrics(storage, mediaStore, max, images, research, logger, noopMetrics{})
}

func NewWithMetrics(storage *store.Store, mediaStore *media.Store, max MAXClient, images ImageClient, research ResearchClient, logger *slog.Logger, metrics Metrics) *App {
	if logger == nil {
		logger = slog.Default()
	}
	if metrics == nil {
		metrics = noopMetrics{}
	}
	return &App{
		store: storage, media: mediaStore, max: max, images: images, research: research,
		logger: logger, metrics: metrics, now: time.Now,
		discoverableRefreshes: make(map[string]discoverableRefreshState),
		mediaPolicy: MediaPolicy{
			MaxFiles: defaultMediaMaxFiles, MaxBytes: defaultMediaMaxBytes,
			OrphanGrace: defaultMediaOrphanGrace, CleanupInterval: defaultMediaCleanupInterval,
			CleanupBatch: defaultMediaCleanupBatch,
		},
	}
}

func (a *App) ConfigureMediaPolicy(policy MediaPolicy) error {
	if policy.MaxFiles <= 0 || policy.MaxBytes <= 0 || policy.OrphanGrace <= 0 ||
		policy.CleanupInterval <= 0 || policy.CleanupBatch <= 0 {
		return errors.New("media policy values must be positive")
	}
	a.mediaCleanupMu.Lock()
	a.mediaPolicy = policy
	a.lastMediaCleanup = time.Time{}
	a.mediaCleanupMu.Unlock()
	return nil
}

func (a *App) Store() *store.Store { return a.store }

func (a *App) Media() *media.Store { return a.media }

func (a *App) MAXConfigured() bool { return a.max != nil }

func (a *App) OpenAIConfigured() bool { return a.images != nil || a.research != nil }

func (a *App) ResearchConfigured() bool { return a.research != nil }

func (a *App) ContentFormattingConfigured() bool {
	_, ok := a.research.(ContentFormatter)
	return a.research != nil && ok
}

func (a *App) ImagePromptSuggestionConfigured() bool {
	_, ok := a.research.(ImagePromptSuggester)
	return a.research != nil && ok
}

func (a *App) BrandKitSuggestionConfigured() bool {
	_, ok := a.research.(BrandKitSuggester)
	return a.research != nil && ok
}

func (a *App) ChannelDescriptionSuggestionConfigured() bool {
	_, ok := a.research.(ChannelDescriptionSuggester)
	return a.research != nil && ok
}

// ValidateImageRequest applies the public API policy before an API handler
// reserves quota, then lets the configured client enforce its model's exact
// upstream rules. Alternative clients still receive the common validation.
func (a *App) ValidateImageRequest(request openaiimg.GenerateRequest) error {
	if a.images == nil {
		return ErrOpenAINotConfigured
	}
	if err := openaiimg.ValidateAPIRequest(request); err != nil {
		return err
	}
	if validator, ok := a.images.(imageRequestValidator); ok {
		return validator.Validate(request)
	}
	return openaiimg.ValidateRequest(request)
}

func (a *App) TestMAX(ctx context.Context) (maxclient.BotInfo, error) {
	if a.max == nil {
		return maxclient.BotInfo{}, ErrMAXNotConfigured
	}
	return a.max.GetMe(ctx)
}

// normalizeObservedMAXChat validates the ownership and lifecycle fields that
// are required before an upstream chat can enter the authenticated inventory.
// MAX may briefly return an empty owner immediately after bot_added; treating
// that response as an error lets the webhook delivery be retried instead of
// persisting an undiscoverable channel indefinitely.
func normalizeObservedMAXChat(requestedID string, info maxclient.ChatInfo, fallbackLink string,
	observedAt time.Time,
) (maxclient.ChatInfo, store.ObservedBotChat, error) {
	requestedID = strings.TrimSpace(requestedID)
	info.ChatID = strings.TrimSpace(info.ChatID)
	info.OwnerID = strings.TrimSpace(info.OwnerID)
	info.Type = strings.TrimSpace(info.Type)
	info.Status = strings.TrimSpace(info.Status)
	info.Title = strings.TrimSpace(info.Title)
	info.Description = strings.TrimSpace(info.Description)
	if info.ChatID == "" || (requestedID != "" && info.ChatID != requestedID) {
		return maxclient.ChatInfo{}, store.ObservedBotChat{}, fmt.Errorf("%w: chat id is missing or mismatched", ErrMAXChannelMetadataIncomplete)
	}
	if info.OwnerID == "" {
		return maxclient.ChatInfo{}, store.ObservedBotChat{}, fmt.Errorf("%w: owner id is missing", ErrMAXChannelMetadataIncomplete)
	}
	if info.Type != "channel" || info.Status != "active" {
		return maxclient.ChatInfo{}, store.ObservedBotChat{}, fmt.Errorf("%w: chat is not an active channel", ErrMAXChannelMetadataIncomplete)
	}
	if info.ParticipantsCount < 0 {
		return maxclient.ChatInfo{}, store.ObservedBotChat{}, fmt.Errorf("%w: participants count is invalid", ErrMAXChannelMetadataIncomplete)
	}
	if info.MessagesCount < 0 {
		return maxclient.ChatInfo{}, store.ObservedBotChat{}, fmt.Errorf("%w: messages count is invalid", ErrMAXChannelMetadataIncomplete)
	}

	canonicalLink := strings.TrimRight(strings.TrimSpace(info.Link), "/")
	if canonicalLink == "" {
		canonicalLink = strings.TrimRight(strings.TrimSpace(fallbackLink), "/")
	}
	if canonicalLink != "" {
		slug, err := maxclient.NormalizeChatLink(canonicalLink)
		if err != nil {
			return maxclient.ChatInfo{}, store.ObservedBotChat{}, fmt.Errorf("%w: public link is invalid", ErrMAXChannelMetadataIncomplete)
		}
		canonicalLink = "https://max.ru/" + strings.TrimPrefix(slug, "@")
	}
	iconURL := maxclient.SafeAssetURL(info.Icon.URL)
	if strings.TrimSpace(info.Icon.URL) != "" && iconURL == "" {
		return maxclient.ChatInfo{}, store.ObservedBotChat{}, fmt.Errorf("%w: icon URL is invalid", ErrMAXChannelMetadataIncomplete)
	}
	info.Link = canonicalLink
	info.Icon.URL = iconURL
	return info, store.ObservedBotChat{
		MAXChatID: info.ChatID, PublicLink: canonicalLink, Title: info.Title, Description: info.Description,
		MAXOwnerID: info.OwnerID, IconURL: iconURL, ParticipantsCount: info.ParticipantsCount,
		IsPublic: info.IsPublic, MessagesCount: info.MessagesCount, HasPinnedMessage: info.HasPinnedMessage,
		MAXLastEventTime: maxLastEventTime(info.LastEventTime), MAXInfoSyncedAt: timePointer(observedAt),
		Active: true, LastSeenAt: observedAt.UTC(),
	}, nil
}

func timePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

func maxLastEventTime(timestamp int64) *time.Time {
	if timestamp <= 0 {
		return nil
	}
	value := time.UnixMilli(timestamp).UTC()
	return &value
}

func channelMetadataFromMAX(info maxclient.ChatInfo, syncedAt time.Time) store.Channel {
	return store.Channel{
		MAXChatID: info.ChatID, VerifiedMAXOwnerID: info.OwnerID,
		Title: strings.TrimSpace(info.Title), Description: strings.TrimSpace(info.Description),
		PublicLink: strings.TrimSpace(info.Link), IconURL: maxclient.SafeAssetURL(info.Icon.URL),
		ParticipantsCount: info.ParticipantsCount, IsPublic: info.IsPublic, MessagesCount: info.MessagesCount,
		HasPinnedMessage: info.HasPinnedMessage, MAXLastEventTime: maxLastEventTime(info.LastEventTime),
		MAXInfoSyncedAt: timePointer(syncedAt),
		IsChannel:       info.Type == "channel", Active: info.Status == "active",
	}
}

func (a *App) syncChannelMAXInfoForUser(
	ctx context.Context, userID string, channelID int64, expectedMAXChatID string, info maxclient.ChatInfo,
	capturedAt time.Time,
) (store.Channel, error) {
	return a.store.SyncChannelMAXInfoForUser(
		ctx, userID, channelID, expectedMAXChatID,
		channelMetadataFromMAX(info, capturedAt), capturedAt)
}

// resolveMAXChatOwner fills the nullable owner_id from the official
// administrators endpoint. An expected owner is always matched by exact MAX
// user id and must carry the owner flag; an ordinary administrator can never
// become a verified owner. Webhook discovery additionally requires the owner
// profile to have been linked to a MaxPosty tenant already.
func (a *App) resolveMAXChatOwner(ctx context.Context, info maxclient.ChatInfo, expectedOwnerID string,
	requireLinkedOwner bool,
) (maxclient.ChatInfo, error) {
	info.OwnerID = strings.TrimSpace(info.OwnerID)
	if info.OwnerID != "" {
		return info, nil
	}
	admins, err := a.max.GetChatAdmins(ctx, strings.TrimSpace(info.ChatID))
	if err != nil {
		return maxclient.ChatInfo{}, err
	}
	expectedOwnerID = strings.TrimSpace(expectedOwnerID)
	if expectedOwnerID != "" {
		expected, parseErr := strconv.ParseInt(expectedOwnerID, 10, 64)
		if parseErr != nil || expected <= 0 {
			return maxclient.ChatInfo{}, fmt.Errorf("%w: linked owner id is invalid", ErrMAXChannelMetadataIncomplete)
		}
		for _, admin := range admins {
			if admin.UserID == expected && admin.IsOwner && admin.IsAdmin && !admin.IsBot {
				info.OwnerID = expectedOwnerID
				return info, nil
			}
		}
		return maxclient.ChatInfo{}, fmt.Errorf("%w: linked MAX profile is not the current channel owner", ErrMAXChannelMetadataIncomplete)
	}

	owners := make(map[string]struct{}, 1)
	for _, admin := range admins {
		if !admin.IsOwner || !admin.IsAdmin || admin.IsBot || admin.UserID <= 0 {
			continue
		}
		ownerID := strconv.FormatInt(admin.UserID, 10)
		if requireLinkedOwner {
			if _, linkErr := a.store.GetMAXIdentityLinkForMAXUser(ctx, ownerID); linkErr != nil {
				if errors.Is(linkErr, store.ErrNotFound) {
					continue
				}
				return maxclient.ChatInfo{}, linkErr
			}
		}
		owners[ownerID] = struct{}{}
	}
	if len(owners) != 1 {
		return maxclient.ChatInfo{}, fmt.Errorf("%w: current channel owner was not uniquely verified", ErrMAXChannelMetadataIncomplete)
	}
	for ownerID := range owners {
		info.OwnerID = ownerID
	}
	return info, nil
}

func (a *App) ObserveMAXChat(ctx context.Context, maxChatID string, active bool, eventAt time.Time) error {
	if a.max == nil {
		return ErrMAXNotConfigured
	}
	now := eventAt.UTC()
	if !active {
		return a.store.MarkObservedBotChatRemoved(ctx, maxChatID, now)
	}
	// Persist the authenticated bot_added/message_created lifecycle fact before
	// enriching it. MAX may omit owner_id and refuse the administrators lookup
	// until the helper is promoted from subscriber to administrator. Keeping the
	// chat id lets a later refresh recover that normal two-step setup flow.
	if err := a.store.TouchObservedBotChat(ctx, maxChatID, now); err != nil {
		return err
	}
	info, err := a.max.GetChat(ctx, maxChatID)
	if err != nil {
		return err
	}
	info, err = a.resolveMAXChatOwner(ctx, info, "", true)
	if err != nil {
		return err
	}
	_, observed, err := normalizeObservedMAXChat(maxChatID, info, "", now)
	if err != nil {
		return err
	}
	return a.store.UpsertObservedBotChat(ctx, observed)
}

// DiscoverMAXChatFromMessage learns a channel from message_created only when
// it is absent from the active inventory. The second lookup inside singleflight
// closes the race between concurrent webhook deliveries and later retries.
func (a *App) DiscoverMAXChatFromMessage(ctx context.Context, maxChatID string, eventAt time.Time) error {
	if a.max == nil {
		return ErrMAXNotConfigured
	}
	if observed, err := a.store.GetActiveObservedBotChat(ctx, "", maxChatID); err == nil {
		if strings.TrimSpace(observed.MAXOwnerID) != "" {
			return nil
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	_, err, _ := a.messageChatDiscovery.Do(maxChatID, func() (any, error) {
		if observed, lookupErr := a.store.GetActiveObservedBotChat(ctx, "", maxChatID); lookupErr == nil {
			if strings.TrimSpace(observed.MAXOwnerID) != "" {
				return nil, nil
			}
		} else if !errors.Is(lookupErr, store.ErrNotFound) {
			return nil, lookupErr
		}
		return nil, a.ObserveMAXChat(ctx, maxChatID, true, eventAt)
	})
	return err
}

type DiscoverableChannelRefresh struct {
	Channels  []store.DiscoverableChannel `json:"channels"`
	Refreshed int                         `json:"refreshed"`
	Failed    int                         `json:"failed"`
}

func (a *App) beginDiscoverableRefresh(userID string, now time.Time) error {
	a.discoverableRefreshMu.Lock()
	defer a.discoverableRefreshMu.Unlock()
	if a.discoverableRefreshes == nil {
		a.discoverableRefreshes = make(map[string]discoverableRefreshState)
	}
	if a.discoverableRefreshLastSweep.IsZero() || now.Before(a.discoverableRefreshLastSweep) ||
		now.Sub(a.discoverableRefreshLastSweep) >= discoverableRefreshCooldown {
		for ownerID, state := range a.discoverableRefreshes {
			if !state.inFlight && !now.Before(state.retryAt) {
				delete(a.discoverableRefreshes, ownerID)
			}
		}
		a.discoverableRefreshLastSweep = now
	}
	if state, ok := a.discoverableRefreshes[userID]; ok && (state.inFlight || now.Before(state.retryAt)) {
		retryAfter := state.retryAt.Sub(now)
		if retryAfter <= 0 {
			retryAfter = time.Second
		}
		return &DiscoverableRefreshCooldownError{RetryAfter: retryAfter}
	}
	a.discoverableRefreshes[userID] = discoverableRefreshState{
		inFlight: true, retryAt: now.Add(discoverableRefreshCooldown),
	}
	return nil
}

func (a *App) finishDiscoverableRefresh(userID string) {
	a.discoverableRefreshMu.Lock()
	defer a.discoverableRefreshMu.Unlock()
	state, ok := a.discoverableRefreshes[userID]
	if !ok {
		return
	}
	state.inFlight = false
	a.discoverableRefreshes[userID] = state
}

// RefreshDiscoverableChannelsForUser reconciles only a bounded, tenant-safe
// set derived from the authenticated inventory. The request never accepts a
// chat id, so it cannot be used to probe arbitrary channels through the shared
// bot. Authoritative owner metadata is persisted before the normal tenant
// query decides which rows may be returned.
func (a *App) RefreshDiscoverableChannelsForUser(ctx context.Context, userID string) (DiscoverableChannelRefresh, error) {
	if a.max == nil {
		return DiscoverableChannelRefresh{}, ErrMAXNotConfigured
	}
	link, err := a.store.GetMAXIdentityLinkForUser(ctx, userID)
	if err != nil {
		return DiscoverableChannelRefresh{}, err
	}
	gateNow := a.now()
	if err := a.beginDiscoverableRefresh(userID, gateNow); err != nil {
		return DiscoverableChannelRefresh{}, err
	}
	defer a.finishDiscoverableRefresh(userID)
	now := gateNow.UTC()
	candidates, err := a.store.ListDiscoverableChannelRefreshCandidatesForUser(
		ctx, userID, now.Add(-incompleteObservedChatWindow),
		discoverableOwnedRefreshLimit, discoverableUnknownRefreshLimit,
	)
	if err != nil {
		return DiscoverableChannelRefresh{}, err
	}
	result := DiscoverableChannelRefresh{}
	var firstOwnedErr error
	refreshCandidate := func(candidate store.ObservedBotChat, tenantAssociated bool) {
		info, getErr := a.max.GetChat(ctx, candidate.MAXChatID)
		if getErr != nil {
			if tenantAssociated {
				result.Failed++
				if firstOwnedErr == nil {
					firstOwnedErr = getErr
				}
			}
			return
		}
		// Resolve the authoritative owner before deciding whether any outcome is
		// visible in this tenant's counters. Persisting a verified foreign owner
		// only repairs the shared bot inventory; the tenant query still hides it.
		info, getErr = a.resolveMAXChatOwner(ctx, info, "", false)
		if getErr != nil {
			if tenantAssociated {
				result.Failed++
				if firstOwnedErr == nil {
					firstOwnedErr = getErr
				}
			}
			return
		}
		requesterOwned := info.OwnerID == link.MAXUserID
		_, observed, normalizeErr := normalizeObservedMAXChat(candidate.MAXChatID, info, candidate.PublicLink, now)
		if normalizeErr != nil {
			if requesterOwned {
				result.Failed++
				if firstOwnedErr == nil {
					firstOwnedErr = normalizeErr
				}
			}
			return
		}
		if upsertErr := a.store.RefreshObservedBotChatMetadata(ctx, observed); upsertErr != nil {
			if requesterOwned {
				result.Failed++
				if firstOwnedErr == nil {
					firstOwnedErr = upsertErr
				}
			}
			return
		}
		if requesterOwned {
			result.Refreshed++
		}
	}
	for _, candidate := range candidates.Owned {
		refreshCandidate(candidate, true)
	}
	for _, candidate := range candidates.Unknown {
		refreshCandidate(candidate, false)
	}
	if result.Refreshed == 0 && firstOwnedErr != nil {
		return DiscoverableChannelRefresh{}, firstOwnedErr
	}
	result.Channels, err = a.store.ListDiscoverableChannelsForUser(ctx, userID)
	if err != nil {
		return DiscoverableChannelRefresh{}, err
	}
	return result, nil
}

func (a *App) ConnectChannel(ctx context.Context, publicLink, maxChatID, requestedTitle string) (ChannelCheck, error) {
	if a.max == nil {
		return ChannelCheck{}, ErrMAXNotConfigured
	}
	publicLink = strings.TrimRight(strings.TrimSpace(publicLink), "/")
	maxChatID = strings.TrimSpace(maxChatID)
	observed, err := a.store.GetActiveObservedBotChat(ctx, publicLink, maxChatID)
	if err != nil {
		return ChannelCheck{}, errors.New("the shared bot must be added to the channel as an administrator before connecting")
	}
	info, err := a.max.GetChat(ctx, observed.MAXChatID)
	if err != nil {
		return ChannelCheck{}, err
	}
	info, err = a.resolveMAXChatOwner(ctx, info, observed.MAXOwnerID, false)
	if err != nil {
		return ChannelCheck{}, err
	}
	membership, err := a.max.GetMembership(ctx, info.ChatID)
	if err != nil {
		return ChannelCheck{}, err
	}
	diagnostics := channelDiagnostics(info, membership)
	if !diagnostics.CanPublish || !diagnostics.CanEdit || !diagnostics.CanDelete {
		return ChannelCheck{}, &ChannelAccessError{
			Diagnostics: diagnostics,
			Message:     "The bot must be an active channel administrator with read_all_messages and write permissions",
		}
	}
	title := strings.TrimSpace(info.Title)
	if title == "" {
		title = strings.TrimSpace(requestedTitle)
	}
	if title == "" {
		title = "MAX " + info.ChatID
	}
	canonicalLink := strings.TrimSpace(info.Link)
	if canonicalLink == "" && publicLink != "" {
		slug, normalizeErr := maxclient.NormalizeChatLink(publicLink)
		if normalizeErr == nil {
			canonicalLink = "https://max.ru/" + strings.TrimPrefix(slug, "@")
		}
	}
	metadata := channelMetadataFromMAX(info, a.now().UTC())
	metadata.Title = title
	metadata.PublicLink = canonicalLink
	channel, err := a.store.UpsertConnectedChannel(ctx, metadata)
	if err != nil {
		return ChannelCheck{}, err
	}
	return ChannelCheck{Channel: channel, Diagnostics: diagnostics}, nil
}

type ChannelClaimCandidate struct {
	Info        maxclient.ChatInfo
	Bot         maxclient.BotInfo
	Diagnostics ChannelDiagnostics
}

func (a *App) PrepareChannelClaim(ctx context.Context, publicLink, maxChatID string) (ChannelClaimCandidate, error) {
	if a.max == nil {
		return ChannelClaimCandidate{}, ErrMAXNotConfigured
	}
	publicLink = strings.TrimRight(strings.TrimSpace(publicLink), "/")
	maxChatID = strings.TrimSpace(maxChatID)
	var slug string
	if publicLink != "" {
		var normalizeErr error
		slug, normalizeErr = maxclient.NormalizeChatLink(publicLink)
		if normalizeErr != nil {
			return ChannelClaimCandidate{}, normalizeErr
		}
		publicLink = "https://max.ru/" + strings.TrimPrefix(slug, "@")
	}

	observed, observedErr := a.store.GetActiveObservedBotChat(ctx, publicLink, maxChatID)
	var info maxclient.ChatInfo
	var err error
	fallbackLink := publicLink
	requestedChatID := maxChatID
	expectedOwnerID := ""
	if observedErr == nil {
		fallbackLink = observed.PublicLink
		requestedChatID = observed.MAXChatID
		expectedOwnerID = observed.MAXOwnerID
		info, err = a.max.GetChat(ctx, observed.MAXChatID)
		if err != nil {
			return ChannelClaimCandidate{}, err
		}
	} else {
		if !errors.Is(observedErr, store.ErrNotFound) {
			return ChannelClaimCandidate{}, observedErr
		}
		// Numeric IDs remain registry-only. The public-link fallback supports a
		// channel where the shared bot was already an administrator before the
		// webhook inventory was enabled.
		if publicLink == "" || maxChatID != "" {
			return ChannelClaimCandidate{}, errors.New("first add the MaxPosty bot to the channel as an administrator, then retry")
		}
		info, err = a.max.GetChatByLink(ctx, slug)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				if cause := context.Cause(ctx); cause != nil {
					return ChannelClaimCandidate{}, cause
				}
				if errors.Is(err, context.DeadlineExceeded) {
					return ChannelClaimCandidate{}, context.DeadlineExceeded
				}
				return ChannelClaimCandidate{}, context.Canceled
			}
			var maxErr *maxclient.Error
			if errors.As(err, &maxErr) && maxErr.StatusCode == 404 && maxErr.Code == "chat.not.found" {
				return ChannelClaimCandidate{}, ErrMAXChannelEventRequired
			}
			return ChannelClaimCandidate{}, err
		}
	}
	info, err = a.resolveMAXChatOwner(ctx, info, expectedOwnerID, false)
	if err != nil {
		return ChannelClaimCandidate{}, err
	}
	info, freshObserved, err := normalizeObservedMAXChat(requestedChatID, info, fallbackLink, a.now().UTC())
	if err != nil {
		return ChannelClaimCandidate{}, err
	}
	membership, err := a.max.GetMembership(ctx, info.ChatID)
	if err != nil {
		return ChannelClaimCandidate{}, err
	}
	diagnostics := channelDiagnostics(info, membership)
	if !diagnostics.CanPublish || !diagnostics.CanEdit || !diagnostics.CanDelete {
		return ChannelClaimCandidate{}, &ChannelAccessError{Diagnostics: diagnostics,
			Message: "The shared bot must be an active channel administrator with read_all_messages and write permissions"}
	}
	// Refresh both link-discovered and already observed rows. Without this,
	// claims could keep serving an old channel avatar/title indefinitely.
	if err := a.store.UpsertObservedBotChat(ctx, freshObserved); err != nil {
		return ChannelClaimCandidate{}, err
	}
	bot, err := a.max.GetMe(ctx)
	if err != nil {
		return ChannelClaimCandidate{}, err
	}
	if strings.TrimSpace(bot.Username) == "" {
		return ChannelClaimCandidate{}, errors.New("MAX bot username is missing")
	}
	return ChannelClaimCandidate{Info: info, Bot: bot, Diagnostics: diagnostics}, nil
}

func (a *App) CompleteChannelClaim(ctx context.Context, claim store.ChannelClaim) (store.Channel, ChannelDiagnostics, error) {
	if a.max == nil {
		return store.Channel{}, ChannelDiagnostics{}, ErrMAXNotConfigured
	}
	info, err := a.max.GetChat(ctx, claim.MAXChatID)
	if err != nil {
		return store.Channel{}, ChannelDiagnostics{}, err
	}
	info, err = a.resolveMAXChatOwner(ctx, info, claim.MAXUserID, false)
	if err != nil {
		return store.Channel{}, ChannelDiagnostics{}, err
	}
	info, freshObserved, err := normalizeObservedMAXChat(claim.MAXChatID, info, claim.PublicLink, a.now().UTC())
	if err != nil {
		return store.Channel{}, ChannelDiagnostics{}, err
	}
	if err := a.store.UpsertObservedBotChat(ctx, freshObserved); err != nil {
		return store.Channel{}, ChannelDiagnostics{}, err
	}
	membership, err := a.max.GetMembership(ctx, claim.MAXChatID)
	if err != nil {
		return store.Channel{}, ChannelDiagnostics{}, err
	}
	diagnostics := channelDiagnostics(info, membership)
	if info.OwnerID == "" || info.OwnerID != claim.MAXUserID {
		return store.Channel{}, diagnostics, &ChannelAccessError{Diagnostics: diagnostics,
			Message: "Channel connection must be confirmed by its current MAX owner"}
	}
	if !diagnostics.CanPublish || !diagnostics.CanEdit || !diagnostics.CanDelete {
		return store.Channel{}, diagnostics, &ChannelAccessError{Diagnostics: diagnostics,
			Message: "The shared bot needs read, publish, edit, and delete permissions"}
	}
	title := strings.TrimSpace(info.Title)
	if title == "" {
		title = strings.TrimSpace(claim.RequestedTitle)
	}
	metadata := channelMetadataFromMAX(info, a.now().UTC())
	metadata.UserID = claim.UserID
	metadata.Title = title
	channel, err := a.store.CompleteChannelClaim(ctx, claim, metadata)
	return channel, diagnostics, err
}

func (a *App) SendChannelClaimConfirmation(ctx context.Context, maxUserID, title, link, requesterLabel, comparisonCode, confirmPayload, cancelPayload string) error {
	if a.max == nil {
		return ErrMAXNotConfigured
	}
	return a.max.SendClaimConfirmation(ctx, maxUserID, title, link, requesterLabel, comparisonCode, confirmPayload, cancelPayload)
}

type maxIdentityConfirmationSender interface {
	SendIdentityLinkConfirmation(context.Context, string, string, string, string, string) error
}

type maxAuthContactClient interface {
	SendAuthContactRequest(context.Context, string, string, string) error
	VerifyContactHMAC(string, string) bool
}

func (a *App) SendMAXIdentityLinkConfirmation(ctx context.Context, maxUserID, requesterLabel, comparisonCode, confirmPayload, cancelPayload string) error {
	if a.max == nil {
		return ErrMAXNotConfigured
	}
	if sender, ok := a.max.(maxIdentityConfirmationSender); ok {
		return sender.SendIdentityLinkConfirmation(ctx, maxUserID, requesterLabel, comparisonCode, confirmPayload, cancelPayload)
	}
	// Compatibility for alternative clients implementing the older interface.
	return a.max.SendClaimConfirmation(ctx, maxUserID, "профиль MAX", "", requesterLabel, comparisonCode, confirmPayload, cancelPayload)
}

func (a *App) SendMAXAuthContactRequest(ctx context.Context, maxUserID, comparisonCode, confirmPayload string) error {
	if a.max == nil {
		return ErrMAXNotConfigured
	}
	client, ok := a.max.(maxAuthContactClient)
	if !ok {
		return ErrMAXNotConfigured
	}
	return client.SendAuthContactRequest(ctx, maxUserID, comparisonCode, confirmPayload)
}

func (a *App) VerifyMAXAuthContact(vcfInfo, proof string) bool {
	if a.max == nil {
		return false
	}
	client, ok := a.max.(maxAuthContactClient)
	return ok && client.VerifyContactHMAC(vcfInfo, proof)
}

func (a *App) ConnectDiscoverableChannelForUser(ctx context.Context, userID, maxChatID string) (ChannelCheck, error) {
	if a.max == nil {
		return ChannelCheck{}, ErrMAXNotConfigured
	}
	link, err := a.store.GetMAXIdentityLinkForUser(ctx, userID)
	if err != nil {
		return ChannelCheck{}, err
	}
	// Authorize against the webhook inventory before contacting MAX. Besides
	// reducing upstream load, this prevents a tenant from probing arbitrary
	// chat IDs and learning whether the shared bot can access another tenant's
	// channel through response differences.
	observed, err := a.store.GetActiveObservedBotChat(ctx, "", maxChatID)
	if err != nil {
		return ChannelCheck{}, err
	}
	if observed.MAXOwnerID == "" || observed.MAXOwnerID != link.MAXUserID {
		return ChannelCheck{}, store.ErrNotFound
	}
	info, err := a.max.GetChat(ctx, maxChatID)
	if err != nil {
		return ChannelCheck{}, err
	}
	info, err = a.resolveMAXChatOwner(ctx, info, link.MAXUserID, false)
	if err != nil {
		return ChannelCheck{}, err
	}
	info, freshObserved, err := normalizeObservedMAXChat(maxChatID, info, observed.PublicLink, a.now().UTC())
	if err != nil {
		return ChannelCheck{}, err
	}
	if err := a.store.UpsertObservedBotChat(ctx, freshObserved); err != nil {
		return ChannelCheck{}, err
	}
	membership, err := a.max.GetMembership(ctx, maxChatID)
	if err != nil {
		return ChannelCheck{}, err
	}
	diagnostics := channelDiagnostics(info, membership)
	if info.ChatID != maxChatID || info.OwnerID == "" || info.OwnerID != link.MAXUserID {
		return ChannelCheck{}, &ChannelAccessError{Diagnostics: diagnostics, Message: "MAX channel owner does not match the linked MAX profile"}
	}
	if !diagnostics.CanPublish || !diagnostics.CanEdit || !diagnostics.CanDelete {
		return ChannelCheck{}, &ChannelAccessError{Diagnostics: diagnostics,
			Message: "The shared bot needs read, publish, edit, and delete permissions"}
	}
	title := strings.TrimSpace(info.Title)
	if title == "" {
		title = "MAX " + info.ChatID
	}
	metadata := channelMetadataFromMAX(info, a.now().UTC())
	metadata.UserID = userID
	metadata.Title = title
	channel, err := a.store.ConnectDiscoverableChannelForUser(ctx, userID, maxChatID, metadata)
	if err != nil {
		return ChannelCheck{}, err
	}
	return ChannelCheck{Channel: channel, Diagnostics: diagnostics}, nil
}

func (a *App) AnswerMAXCallback(ctx context.Context, callbackID, notification, messageText string) error {
	if a.max == nil {
		return ErrMAXNotConfigured
	}
	return a.max.AnswerCallback(ctx, callbackID, notification, messageText)
}

func (a *App) TestChannel(ctx context.Context, channelID int64) (ChannelCheck, error) {
	if a.max == nil {
		return ChannelCheck{}, ErrMAXNotConfigured
	}
	channel, err := a.store.GetChannel(ctx, channelID)
	if err != nil {
		return ChannelCheck{}, err
	}
	// Date the snapshot when its upstream read starts. Otherwise an older,
	// slower GET can finish after a newer one and overwrite fresher metadata.
	capturedAt := a.now().UTC()
	info, membership, err := a.inspectChannel(ctx, channel)
	if err != nil {
		return ChannelCheck{}, err
	}
	channel, err = a.syncChannelMAXInfoForUser(
		ctx, channel.UserID, channel.ID, channel.MAXChatID, info, capturedAt)
	if err != nil {
		return ChannelCheck{}, err
	}
	diagnostics := channelDiagnostics(info, membership)
	if !channel.Active {
		diagnostics.CanPublish = false
		diagnostics.CanEdit = false
		diagnostics.CanDelete = false
		diagnostics.CanPin = false
		diagnostics.CanChangeInfo = false
	}
	return ChannelCheck{Channel: channel, Diagnostics: diagnostics}, nil
}

func (a *App) TestChannelForUser(ctx context.Context, userID string, channelID int64) (ChannelCheck, error) {
	channel, err := a.store.GetChannelForUser(ctx, userID, channelID)
	if err != nil {
		return ChannelCheck{}, err
	}
	if a.max == nil {
		return ChannelCheck{}, ErrMAXNotConfigured
	}
	capturedAt := a.now().UTC()
	info, membership, err := a.inspectChannel(ctx, channel)
	if err != nil {
		return ChannelCheck{}, err
	}
	if err := validateChannelParticipantInfo(channel, info); err != nil {
		return ChannelCheck{}, err
	}
	channel, err = a.syncChannelMAXInfoForUser(
		ctx, userID, channel.ID, channel.MAXChatID, info, capturedAt)
	if err != nil {
		return ChannelCheck{}, err
	}
	diagnostics := channelDiagnostics(info, membership)
	if !channel.Active {
		diagnostics.CanPublish, diagnostics.CanEdit, diagnostics.CanDelete, diagnostics.CanPin, diagnostics.CanChangeInfo = false, false, false, false, false
	}
	return ChannelCheck{Channel: channel, Diagnostics: diagnostics}, nil
}

func (a *App) TestChannelForWorkspace(
	ctx context.Context, actorUserID, workspaceID string, channelID int64,
) (ChannelCheck, error) {
	channel, err := a.store.GetChannelForWorkspace(ctx, actorUserID, workspaceID, channelID)
	if err != nil {
		return ChannelCheck{}, err
	}
	if a.max == nil {
		return ChannelCheck{}, ErrMAXNotConfigured
	}
	capturedAt := a.now().UTC()
	info, membership, err := a.inspectChannel(ctx, channel)
	if err != nil {
		return ChannelCheck{}, err
	}
	if err := validateChannelParticipantInfo(channel, info); err != nil {
		return ChannelCheck{}, err
	}
	channel, err = a.syncChannelMAXInfoForUser(
		ctx, channel.UserID, channel.ID, channel.MAXChatID, info, capturedAt)
	if err != nil {
		return ChannelCheck{}, err
	}
	diagnostics := channelDiagnostics(info, membership)
	if !channel.Active {
		diagnostics.CanPublish, diagnostics.CanEdit, diagnostics.CanDelete, diagnostics.CanPin, diagnostics.CanChangeInfo = false, false, false, false, false
	}
	return ChannelCheck{Channel: channel, Diagnostics: diagnostics}, nil
}

// ChannelMAXInfoUpdate carries channel metadata pushed to MAX itself. Icon is
// streamed to the MAX upload endpoint first; Title changes both the MAX chat
// and the cached channel record. The MAX Bot API cannot change descriptions.
type ChannelMAXInfoUpdate struct {
	Title        *string
	IconFilename string
	Icon         io.Reader
	Notify       bool
}

// pushChannelMAXInfo re-verifies channel ownership and bot admin rights, then
// applies the patch in MAX and returns the fresh chat metadata for syncing.
func (a *App) pushChannelMAXInfo(ctx context.Context, channel store.Channel, update ChannelMAXInfoUpdate) (maxclient.ChatInfo, error) {
	if a.max == nil {
		return maxclient.ChatInfo{}, ErrMAXNotConfigured
	}
	if update.Title == nil && update.Icon == nil {
		return maxclient.ChatInfo{}, errors.New("channel title or icon is required")
	}
	info, membership, err := a.inspectChannel(ctx, channel)
	if err != nil {
		return maxclient.ChatInfo{}, err
	}
	diagnostics := channelDiagnostics(info, membership)
	if !channel.Active || !diagnostics.CanChangeInfo {
		return maxclient.ChatInfo{}, &ChannelAccessError{Diagnostics: diagnostics,
			Message: "The shared bot needs change_chat_info permission to change the channel title or photo"}
	}
	notify := update.Notify
	patch := maxclient.ChatPatch{Title: update.Title, Notify: &notify}
	if update.Icon != nil {
		upload, uploadErr := a.max.UploadImage(ctx, update.IconFilename, update.Icon)
		if uploadErr != nil {
			return maxclient.ChatInfo{}, uploadErr
		}
		patch.IconToken = upload.Token
	}
	return a.max.EditChat(ctx, channel.MAXChatID, patch)
}

func (a *App) UpdateChannelMAXInfoForUser(ctx context.Context, userID string, channelID int64, update ChannelMAXInfoUpdate) (store.Channel, error) {
	channel, err := a.store.GetChannelForUser(ctx, userID, channelID)
	if err != nil {
		return store.Channel{}, err
	}
	info, err := a.pushChannelMAXInfo(ctx, channel, update)
	if err != nil {
		return store.Channel{}, err
	}
	if update.Title != nil {
		if _, err := a.store.UpdateChannelForUser(ctx, userID, channelID, update.Title, nil); err != nil {
			return store.Channel{}, err
		}
	}
	return a.syncChannelMAXInfoForUser(
		ctx, userID, channelID, channel.MAXChatID, info, a.now().UTC())
}

func (a *App) UpdateChannelMAXInfoForWorkspace(ctx context.Context, actorUserID, workspaceID string, channelID int64, update ChannelMAXInfoUpdate) (store.Channel, error) {
	channel, err := a.store.GetChannelForWorkspace(ctx, actorUserID, workspaceID, channelID)
	if err != nil {
		return store.Channel{}, err
	}
	info, err := a.pushChannelMAXInfo(ctx, channel, update)
	if err != nil {
		return store.Channel{}, err
	}
	if update.Title != nil {
		// UpdateChannelForWorkspace также записывает событие channel.updated.
		if _, err := a.store.UpdateChannelForWorkspace(ctx, actorUserID, workspaceID, channelID, update.Title, nil); err != nil {
			return store.Channel{}, err
		}
	} else if _, err := a.store.CreateAuditEvent(ctx, actorUserID, store.AuditEvent{
		WorkspaceID: workspaceID, Action: "channel.updated", EntityType: "channel", EntityID: fmt.Sprint(channelID),
	}); err != nil {
		return store.Channel{}, err
	}
	if _, err := a.syncChannelMAXInfoForUser(
		ctx, channel.UserID, channelID, channel.MAXChatID, info, a.now().UTC(),
	); err != nil {
		return store.Channel{}, err
	}
	return a.store.GetChannelForWorkspace(ctx, actorUserID, workspaceID, channelID)
}

func (a *App) GenerateImageForUser(ctx context.Context, userID string, request openaiimg.GenerateRequest) (media.File, error) {
	if a.images == nil {
		return media.File{}, ErrOpenAINotConfigured
	}
	result, err := a.images.Generate(ctx, request)
	if err != nil {
		return media.File{}, err
	}
	return a.SaveMediaForUser(ctx, userID, "openai.png", bytes.NewReader(result.Bytes))
}

func (a *App) GenerateImageForWorkspace(ctx context.Context, actorUserID, workspaceID string, request openaiimg.GenerateRequest) (media.File, error) {
	if a.images == nil {
		return media.File{}, ErrOpenAINotConfigured
	}
	result, err := a.images.Generate(ctx, request)
	if err != nil {
		return media.File{}, err
	}
	return a.SaveAttachmentMediaForWorkspace(ctx, actorUserID, workspaceID,
		media.AttachmentTypeImage, "openai.png", bytes.NewReader(result.Bytes))
}

// SaveMediaForUser reserves quota in PostgreSQL before writing the private S3
// object. A failed write releases the reservation; stale reservations are also
// reclaimed by the periodic orphan job after the configured grace period.
func (a *App) SaveMediaForUser(ctx context.Context, userID, filename string, reader io.Reader) (result media.File, resultErr error) {
	return a.SaveAttachmentMediaForUser(ctx, userID, media.AttachmentTypeImage, filename, reader)
}

// SaveAttachmentMediaForUser validates an attachment into a bounded temporary
// file, reserves tenant quota, and only then streams it to object storage.
func (a *App) SaveAttachmentMediaForUser(ctx context.Context, userID, attachmentType, filename string, reader io.Reader) (result media.File, resultErr error) {
	defer func() { a.metrics.ObserveMediaOperation("upload", metricOutcome(resultErr)) }()
	upload, err := a.media.PrepareAttachment(attachmentType, filename, reader)
	if err != nil {
		return media.File{}, err
	}
	defer func() { _ = upload.Close() }()
	file := upload.File()
	policy := a.currentMediaPolicy()
	reservation, err := a.store.ReserveMedia(ctx, userID, file.Filename, file.Size, store.MediaLimits{
		MaxFiles: policy.MaxFiles, MaxBytes: policy.MaxBytes,
	}, a.now().UTC())
	if err != nil {
		return media.File{}, err
	}
	if err := upload.Store(ctx); err != nil {
		a.releaseMediaReservation(reservation)
		return media.File{}, err
	}
	if err := a.store.CompleteMediaReservation(ctx, reservation, a.now().UTC()); err != nil {
		a.releaseMediaReservation(reservation)
		return media.File{}, err
	}
	return file, nil
}

func (a *App) SaveAttachmentMediaForWorkspace(ctx context.Context, actorUserID, workspaceID, attachmentType, filename string, reader io.Reader) (result media.File, resultErr error) {
	workspace, err := a.store.GetWorkspaceForUser(ctx, actorUserID, workspaceID)
	if err != nil {
		return media.File{}, err
	}
	// Personal workspace routes are aliases of the legacy personal API. Keep
	// one authoritative quota ledger so alternating URL families cannot double
	// the user's storage allowance.
	if workspace.IsPersonal {
		return a.SaveAttachmentMediaForUser(ctx, actorUserID, attachmentType, filename, reader)
	}
	defer func() { a.metrics.ObserveMediaOperation("upload", metricOutcome(resultErr)) }()
	upload, err := a.media.PrepareAttachment(attachmentType, filename, reader)
	if err != nil {
		return media.File{}, err
	}
	defer func() { _ = upload.Close() }()
	file := upload.File()
	policy := a.currentMediaPolicy()
	reservation, err := a.store.ReserveMediaForWorkspace(ctx, actorUserID, workspaceID, file.Filename, file.Size, store.MediaLimits{
		MaxFiles: policy.MaxFiles, MaxBytes: policy.MaxBytes,
	}, a.now().UTC())
	if err != nil {
		return media.File{}, err
	}
	if err := upload.Store(ctx); err != nil {
		a.releaseMediaReservation(reservation)
		return media.File{}, err
	}
	if err := a.store.CompleteMediaReservation(ctx, reservation, a.now().UTC()); err != nil {
		a.releaseMediaReservation(reservation)
		return media.File{}, err
	}
	return file, nil
}

func (a *App) releaseMediaReservation(reservation store.MediaReservation) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.store.ReleaseMediaReservation(ctx, reservation, a.now().UTC()); err != nil {
		a.logger.Error("could not release failed media reservation", "error", err)
	}
}

func (a *App) currentMediaPolicy() MediaPolicy {
	a.mediaCleanupMu.Lock()
	defer a.mediaCleanupMu.Unlock()
	return a.mediaPolicy
}

func (a *App) GenerateResearch(ctx context.Context, request openairesearch.Request) (openairesearch.Result, error) {
	if err := openairesearch.ValidateRequest(request); err != nil {
		return openairesearch.Result{}, err
	}
	if a.research == nil {
		return openairesearch.Result{}, ErrResearchNotConfigured
	}
	return a.research.Generate(ctx, request)
}

func (a *App) FormatPostContent(ctx context.Context, request openairesearch.FormatRequest) (openairesearch.FormatResult, error) {
	if err := openairesearch.ValidateFormatRequest(request); err != nil {
		return openairesearch.FormatResult{}, err
	}
	formatter, ok := a.research.(ContentFormatter)
	if a.research == nil || !ok {
		return openairesearch.FormatResult{}, ErrResearchNotConfigured
	}
	return formatter.FormatContent(ctx, request)
}

func (a *App) SuggestImagePrompt(ctx context.Context, request openairesearch.SuggestImagePromptRequest) (openairesearch.SuggestImagePromptResult, error) {
	if err := openairesearch.ValidateSuggestImagePromptRequest(request); err != nil {
		return openairesearch.SuggestImagePromptResult{}, err
	}
	suggester, ok := a.research.(ImagePromptSuggester)
	if a.research == nil || !ok {
		return openairesearch.SuggestImagePromptResult{}, ErrResearchNotConfigured
	}
	return suggester.SuggestImagePrompt(ctx, request)
}

// SuggestBrandKit assembles untrusted editorial material from the workspace's
// own recent posts and asks the model for a Brand Kit draft. Nothing is
// persisted: the caller shows the suggestion and the user edits and saves it
// through the regular brand kit update flow.
func (a *App) SuggestBrandKit(ctx context.Context, actorUserID, workspaceID string) (openairesearch.SuggestBrandKitResult, error) {
	suggester, ok := a.research.(BrandKitSuggester)
	if a.research == nil || !ok {
		return openairesearch.SuggestBrandKitResult{}, ErrResearchNotConfigured
	}
	posts, err := a.store.ListPostsForWorkspace(ctx, actorUserID, workspaceID, "", nil)
	if err != nil {
		return openairesearch.SuggestBrandKitResult{}, err
	}
	candidates := make([]store.Post, 0, len(posts))
	for _, post := range posts {
		if strings.TrimSpace(post.Content) != "" {
			candidates = append(candidates, post)
		}
	}
	if len(candidates) < openairesearch.MinSuggestBrandKitPosts {
		return openairesearch.SuggestBrandKitResult{}, ErrNotEnoughPostsForBrandKit
	}
	// Published posts represent the workspace's real voice best, so they come
	// first; drafts only pad the sample. Each group keeps the store's
	// newest-first ordering.
	ordered := make([]store.Post, 0, len(candidates))
	for _, post := range candidates {
		if post.Status == store.PostStatusPublished {
			ordered = append(ordered, post)
		}
	}
	for _, post := range candidates {
		if post.Status != store.PostStatusPublished {
			ordered = append(ordered, post)
		}
	}
	samples := make([]openairesearch.PostSample, 0, openairesearch.MaxSuggestBrandKitPosts)
	selected := make([]store.Post, 0, openairesearch.MaxSuggestBrandKitPosts)
	remaining := openairesearch.MaxSuggestBrandKitTotalRunes
	for _, post := range ordered {
		if len(samples) == openairesearch.MaxSuggestBrandKitPosts || remaining <= 0 {
			break
		}
		text := strings.TrimSpace(post.Content)
		if runes := []rune(text); len(runes) > remaining {
			text = strings.TrimSpace(string(runes[:remaining]))
			if text == "" {
				break
			}
		}
		remaining -= utf8.RuneCountInString(text)
		samples = append(samples, openairesearch.PostSample{Text: text, Format: post.Format})
		selected = append(selected, post)
	}
	if len(samples) < openairesearch.MinSuggestBrandKitPosts {
		return openairesearch.SuggestBrandKitResult{}, ErrNotEnoughPostsForBrandKit
	}
	return suggester.SuggestBrandKit(ctx, openairesearch.SuggestBrandKitRequest{
		Posts:  samples,
		Images: a.collectBrandKitImages(ctx, selected),
	})
}

// SuggestChannelDescriptionForUser builds an AI request exclusively from the
// authenticated user's channel and its newest non-empty posts. The caller's
// context and draft stay separate from authoritative MAX metadata so neither
// can replace the saved title or description.
func (a *App) SuggestChannelDescriptionForUser(
	ctx context.Context, userID string, channelID int64, input openairesearch.SuggestChannelDescriptionRequest,
) (openairesearch.SuggestChannelDescriptionResult, error) {
	if err := openairesearch.ValidateSuggestChannelDescriptionInput(input); err != nil {
		return openairesearch.SuggestChannelDescriptionResult{}, err
	}
	channel, err := a.store.GetChannelForUser(ctx, userID, channelID)
	if err != nil {
		return openairesearch.SuggestChannelDescriptionResult{}, err
	}
	posts, err := a.store.ListPostsForUser(ctx, userID, store.PostStatusPublished, &channelID)
	if err != nil {
		return openairesearch.SuggestChannelDescriptionResult{}, err
	}
	return a.suggestChannelDescription(ctx, channel, posts, input)
}

func (a *App) SuggestChannelDescriptionForWorkspace(
	ctx context.Context, actorUserID, workspaceID string, channelID int64,
	input openairesearch.SuggestChannelDescriptionRequest,
) (openairesearch.SuggestChannelDescriptionResult, error) {
	if err := openairesearch.ValidateSuggestChannelDescriptionInput(input); err != nil {
		return openairesearch.SuggestChannelDescriptionResult{}, err
	}
	channel, err := a.store.GetChannelForWorkspace(ctx, actorUserID, workspaceID, channelID)
	if err != nil {
		return openairesearch.SuggestChannelDescriptionResult{}, err
	}
	posts, err := a.store.ListPostsForWorkspace(ctx, actorUserID, workspaceID, store.PostStatusPublished, &channelID)
	if err != nil {
		return openairesearch.SuggestChannelDescriptionResult{}, err
	}
	return a.suggestChannelDescription(ctx, channel, posts, input)
}

func (a *App) suggestChannelDescription(
	ctx context.Context, channel store.Channel, posts []store.Post,
	input openairesearch.SuggestChannelDescriptionRequest,
) (openairesearch.SuggestChannelDescriptionResult, error) {
	suggester, ok := a.research.(ChannelDescriptionSuggester)
	if a.research == nil || !ok {
		return openairesearch.SuggestChannelDescriptionResult{}, ErrResearchNotConfigured
	}
	input.ChannelTitle = channel.Title
	input.ChannelDescription = channel.Description
	input.Posts = make([]openairesearch.PostSample, 0, openairesearch.MaxSuggestChannelDescriptionPosts)
	remaining := openairesearch.MaxSuggestChannelDescriptionTotalRunes
	for _, post := range posts {
		if len(input.Posts) == openairesearch.MaxSuggestChannelDescriptionPosts || remaining <= 0 {
			break
		}
		text := strings.TrimSpace(post.Content)
		if text == "" {
			continue
		}
		if runes := []rune(text); len(runes) > remaining {
			text = strings.TrimSpace(string(runes[:remaining]))
			if text == "" {
				break
			}
		}
		remaining -= utf8.RuneCountInString(text)
		input.Posts = append(input.Posts, openairesearch.PostSample{Text: text, Format: post.Format})
	}
	if err := openairesearch.ValidateSuggestChannelDescriptionRequest(input); err != nil {
		return openairesearch.SuggestChannelDescriptionResult{}, err
	}
	return suggester.SuggestChannelDescription(ctx, input)
}

// collectBrandKitImages loads up to MaxSuggestBrandKitImages cover images from
// the sampled posts. Missing, oversized or unsupported files are skipped
// silently: the suggestion then simply proceeds without a visual style.
// Content-addressed keys are deduplicated so one reused cover is sent once.
func (a *App) collectBrandKitImages(ctx context.Context, posts []store.Post) []openairesearch.ImageInput {
	if a.media == nil {
		return nil
	}
	images := make([]openairesearch.ImageInput, 0, openairesearch.MaxSuggestBrandKitImages)
	seen := make(map[string]struct{}, openairesearch.MaxSuggestBrandKitImages)
	for _, post := range posts {
		if len(images) == openairesearch.MaxSuggestBrandKitImages {
			break
		}
		key := a.brandKitImageKey(post)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		if image, ok := a.loadBrandKitImage(ctx, key); ok {
			images = append(images, image)
		}
	}
	return images
}

func (a *App) brandKitImageKey(post store.Post) string {
	for _, attachment := range post.Attachments {
		if attachment.Type == store.PostAttachmentImage &&
			attachment.ProcessingStatus == store.AttachmentStatusReady && attachment.StorageKey != "" {
			return attachment.StorageKey
		}
	}
	if post.ImagePath != "" {
		return post.ImagePath
	}
	if post.ImageURL != "" {
		if filename, err := a.media.FilenameFromURL(post.ImageURL); err == nil {
			return filename
		}
	}
	return ""
}

func (a *App) loadBrandKitImage(ctx context.Context, key string) (openairesearch.ImageInput, bool) {
	object, err := a.media.Open(ctx, key)
	if err != nil {
		return openairesearch.ImageInput{}, false
	}
	defer func() { _ = object.Body.Close() }()
	if !openairesearch.SupportedBrandKitImageMIME(object.MIMEType) {
		return openairesearch.ImageInput{}, false
	}
	data, err := io.ReadAll(io.LimitReader(object.Body, openairesearch.MaxSuggestBrandKitImageBytes+1))
	if err != nil || len(data) == 0 || len(data) > openairesearch.MaxSuggestBrandKitImageBytes {
		return openairesearch.ImageInput{}, false
	}
	return openairesearch.ImageInput{MIME: object.MIMEType, Data: data}, true
}

func (a *App) GeneratePostImage(ctx context.Context, userID string, postID int64, request openaiimg.GenerateRequest) (store.Post, error) {
	post, err := a.store.GetPostForUser(ctx, userID, postID)
	if err != nil {
		return store.Post{}, err
	}
	if post.Status == store.PostStatusPublishing {
		return store.Post{}, fmt.Errorf("%w: post is currently publishing", ErrConflict)
	}
	if strings.TrimSpace(request.Prompt) == "" {
		request.Prompt = post.ImagePrompt
	}
	file, err := a.GenerateImageForUser(ctx, userID, request)
	if err != nil {
		return store.Post{}, err
	}
	prompt := request.Prompt
	return a.store.ReplaceFirstImageAttachmentAndPromptIfUnchanged(ctx, post, attachmentFromImage(file), prompt)
}

func (a *App) GeneratePostImageForWorkspace(ctx context.Context, actorUserID, workspaceID string, postID int64, request openaiimg.GenerateRequest) (store.Post, error) {
	post, err := a.store.GetPostForWorkspace(ctx, actorUserID, workspaceID, postID)
	if err != nil {
		return store.Post{}, err
	}
	if post.Status == store.PostStatusPublishing {
		return store.Post{}, fmt.Errorf("%w: post is currently publishing", ErrConflict)
	}
	if strings.TrimSpace(request.Prompt) == "" {
		request.Prompt = post.ImagePrompt
	}
	file, err := a.GenerateImageForWorkspace(ctx, actorUserID, workspaceID, request)
	if err != nil {
		return store.Post{}, err
	}
	updated, err := a.store.ReplaceFirstImageAttachmentAndPromptIfUnchanged(
		ctx, post, attachmentFromImage(file), request.Prompt)
	if err != nil {
		return store.Post{}, err
	}
	if updated.WorkspaceID != workspaceID {
		return store.Post{}, store.ErrNotFound
	}
	_, err = a.store.CreateAuditEvent(ctx, actorUserID, store.AuditEvent{
		WorkspaceID: workspaceID, Action: "post.image_generated", EntityType: "post", EntityID: fmt.Sprint(postID),
	})
	if err != nil {
		return store.Post{}, err
	}
	return updated, nil
}

func (a *App) SavePostImage(ctx context.Context, postID int64, filename string, reader io.Reader) (store.Post, error) {
	post, err := a.store.GetPost(ctx, postID)
	if err != nil {
		return store.Post{}, err
	}
	return a.SavePostImageForUser(ctx, post.UserID, postID, filename, reader)
}

func (a *App) SavePostImageForUser(ctx context.Context, userID string, postID int64, filename string, reader io.Reader) (store.Post, error) {
	post, err := a.store.GetPostForUser(ctx, userID, postID)
	if err != nil {
		return store.Post{}, err
	}
	if post.Status == store.PostStatusPublishing {
		return store.Post{}, fmt.Errorf("%w: post is currently publishing", ErrConflict)
	}
	file, err := a.SaveMediaForUser(ctx, userID, filename, reader)
	if err != nil {
		return store.Post{}, err
	}
	return a.store.ReplaceFirstImageAttachmentAndPromptIfUnchanged(ctx, post, attachmentFromImage(file), "")
}

func (a *App) SavePostImageForWorkspace(ctx context.Context, actorUserID, workspaceID string, postID int64, filename string, reader io.Reader) (store.Post, error) {
	post, err := a.store.GetPostForWorkspace(ctx, actorUserID, workspaceID, postID)
	if err != nil {
		return store.Post{}, err
	}
	if post.Status == store.PostStatusPublishing {
		return store.Post{}, fmt.Errorf("%w: post is currently publishing", ErrConflict)
	}
	file, err := a.SaveAttachmentMediaForWorkspace(
		ctx, actorUserID, workspaceID, media.AttachmentTypeImage, filename, reader)
	if err != nil {
		return store.Post{}, err
	}
	updated, err := a.store.ReplaceFirstImageAttachmentAndPromptIfUnchanged(ctx, post, attachmentFromImage(file), "")
	if err != nil {
		return store.Post{}, err
	}
	if updated.WorkspaceID != workspaceID {
		return store.Post{}, store.ErrNotFound
	}
	_, err = a.store.CreateAuditEvent(ctx, actorUserID, store.AuditEvent{
		WorkspaceID: workspaceID, Action: "post.image_uploaded", EntityType: "post", EntityID: fmt.Sprint(postID),
	})
	if err != nil {
		return store.Post{}, err
	}
	return updated, nil
}

func (a *App) PublishPost(ctx context.Context, postID int64) (store.Post, error) {
	post, err := a.store.GetPost(ctx, postID)
	if err != nil {
		return store.Post{}, err
	}
	if err := a.requireApprovedCurrentRevision(ctx, post); err != nil {
		return store.Post{}, err
	}
	if a.max == nil {
		return store.Post{}, ErrMAXNotConfigured
	}
	post, err = a.store.ClaimForPublishing(ctx, postID)
	if err != nil {
		return store.Post{}, fmt.Errorf("%w: %w", ErrConflict, err)
	}
	return a.publishClaimedPost(ctx, post, nil)
}

// publishClaimedPost publishes a post that already holds the publishing claim.
// sendStarted, when non-nil, is set to true immediately before the message
// request is handed to MAX: past that point the message may have reached the
// channel even if the call returns an error, so callers must not retry
// automatically.
func (a *App) publishClaimedPost(ctx context.Context, post store.Post, sendStarted *bool) (result store.Post, resultErr error) {
	startedAt := time.Now()
	defer func() {
		a.metrics.ObservePublicationOperation("publish", metricOutcome(resultErr), time.Since(startedAt))
	}()
	postID := post.ID
	// The first approval check gives callers a fast failure, but the post may
	// change before its publishing claim is acquired. Once claimed, content and
	// attachment writes are blocked, so this second check closes that TOCTOU
	// window before any request reaches MAX.
	if err := a.requireApprovedCurrentRevision(ctx, post); err != nil {
		return a.fail(postID, err)
	}
	channel, err := a.validateForPublish(ctx, post)
	if err != nil {
		return a.fail(postID, err)
	}
	info, membership, err := a.inspectChannel(ctx, channel)
	if err != nil {
		return a.fail(postID, err)
	}
	diagnostics := channelDiagnostics(info, membership)
	if !diagnostics.CanPublish {
		return a.fail(postID, &ChannelAccessError{
			Diagnostics: diagnostics,
			Message:     "MAX publish permission check failed; read_all_messages and write are required",
		})
	}

	mediaTokens, imageTokens, err := a.postMediaTokens(ctx, post)
	if err != nil {
		return a.fail(postID, err)
	}
	if sendStarted != nil {
		*sendStarted = true
	}
	message, err := a.max.Publish(ctx, maxclient.PublishRequest{
		ChatID: channel.MAXChatID, Text: post.Content, Format: maxclient.Format(post.Format),
		MediaTokens: mediaTokens, ImageTokens: imageTokens, LinkButtons: maxLinkButtons(post.LinkButtons),
		DisableLinkPreview: post.DisableLinkPreview,
	})
	if err != nil {
		return a.fail(postID, err)
	}
	if message.MessageID == "" {
		return a.fail(postID, errors.New("MAX published the post but returned no message ID; check the channel before retrying"))
	}
	return a.store.MarkPublished(ctx, postID, message.MessageID, message.URL)
}

func (a *App) UpdatePublishedPost(ctx context.Context, postID int64) (result store.Post, resultErr error) {
	startedAt := time.Now()
	defer func() {
		a.metrics.ObservePublicationOperation("edit", metricOutcome(resultErr), time.Since(startedAt))
	}()
	if a.max == nil {
		return store.Post{}, ErrMAXNotConfigured
	}
	post, err := a.store.GetPost(ctx, postID)
	if err != nil {
		return store.Post{}, err
	}
	if isStoredMAXPublicationMissing(post) {
		return post, nil
	}
	if post.Status != store.PostStatusPublished || post.MAXMessageID == "" {
		return store.Post{}, fmt.Errorf("%w: post has no active MAX publication", ErrConflict)
	}
	if err := a.requireApprovedCurrentRevision(ctx, post); err != nil {
		return store.Post{}, err
	}
	channel, err := a.validateForPublish(ctx, post)
	if err != nil {
		return store.Post{}, err
	}
	info, membership, err := a.inspectChannel(ctx, channel)
	if err != nil {
		return store.Post{}, err
	}
	diagnostics := channelDiagnostics(info, membership)
	if !diagnostics.CanEdit {
		return store.Post{}, &ChannelAccessError{Diagnostics: diagnostics, Message: "MAX edit permission is required"}
	}
	message, err := a.max.GetMessage(ctx, post.MAXMessageID)
	if err != nil {
		if isMAXMessageNotFound(err) {
			return a.markMAXPublicationMissing(ctx, post)
		}
		return store.Post{}, err
	}
	if err := validateMAXMessageOwnership(message, post.MAXMessageID, channel.MAXChatID); err != nil {
		return store.Post{}, err
	}
	mediaTokens, imageTokens, err := a.postMediaTokens(ctx, post)
	if err != nil {
		return store.Post{}, err
	}
	if len(post.Attachments) == 0 && post.ImageURL == "" {
		// A non-nil empty slice tells MAX to remove media that existed in the
		// previously published version instead of leaving attachments unchanged.
		mediaTokens = []maxclient.MediaToken{}
	}
	claimed, err := a.store.ClaimPublishedForUpdate(ctx, post)
	if err != nil {
		return store.Post{}, fmt.Errorf("%w: %w", ErrConflict, err)
	}
	if err := a.requireApprovedCurrentRevision(ctx, claimed); err != nil {
		if _, releaseErr := a.releasePublishedUpdate(claimed, publicationFailureMessage(err)); releaseErr != nil {
			return store.Post{}, errors.Join(err, releaseErr)
		}
		return store.Post{}, err
	}
	err = a.max.Edit(ctx, maxclient.EditRequest{
		MessageID: claimed.MAXMessageID, Text: claimed.Content, Format: maxclient.Format(claimed.Format),
		MediaTokens: mediaTokens, ImageTokens: imageTokens, LinkButtons: maxLinkButtons(claimed.LinkButtons),
	})
	if err != nil {
		// MAX edit operations can return HTTP 200 with success=false and no
		// machine-readable reason. Re-read the message to distinguish an
		// external deletion (including the small race after our preflight)
		// from a genuine edit failure.
		missing := false
		if isMAXOperationFailed(err) {
			if _, getErr := a.max.GetMessage(ctx, claimed.MAXMessageID); isMAXMessageNotFound(getErr) {
				missing = true
			}
		}
		released, releaseErr := a.releasePublishedUpdate(claimed, publicationFailureMessage(err))
		if releaseErr != nil {
			return store.Post{}, errors.Join(err, releaseErr)
		}
		if missing {
			return a.markMAXPublicationMissing(ctx, released)
		}
		return store.Post{}, err
	}
	return a.releasePublishedUpdate(claimed, "")
}

func (a *App) DeletePublication(ctx context.Context, userID string, postID int64) (result store.Post, resultErr error) {
	startedAt := time.Now()
	defer func() {
		a.metrics.ObservePublicationOperation("delete", metricOutcome(resultErr), time.Since(startedAt))
	}()
	post, err := a.store.GetPostForUser(ctx, userID, postID)
	if err != nil {
		return store.Post{}, err
	}
	if a.max == nil {
		return store.Post{}, ErrMAXNotConfigured
	}
	if post.MAXMessageID == "" {
		return store.Post{}, fmt.Errorf("%w: post has no MAX publication", ErrConflict)
	}
	if post.Status == store.PostStatusPublishing {
		return store.Post{}, fmt.Errorf("%w: post is currently publishing", ErrConflict)
	}
	if post.ChannelID == nil {
		return store.Post{}, errors.New("published post has no channel_id")
	}
	channel, err := a.store.GetChannelForUser(ctx, userID, *post.ChannelID)
	if err != nil {
		return store.Post{}, err
	}
	info, membership, err := a.inspectChannel(ctx, channel)
	if err != nil {
		return store.Post{}, err
	}
	diagnostics := channelDiagnostics(info, membership)
	if !diagnostics.CanDelete {
		return store.Post{}, &ChannelAccessError{Diagnostics: diagnostics, Message: "MAX delete permission is required"}
	}
	if err := a.max.Delete(ctx, post.MAXMessageID); err != nil {
		// Deletion is idempotent from the user's perspective. If MAX reports
		// that the message is already gone, clear the matching live metadata
		// with the same CAS used after a successful explicit deletion.
		if isMAXMessageNotFound(err) {
			return a.store.ClearPublicationForUser(ctx, userID, postID, channel.ID, post.MAXMessageID)
		}
		// MAX delete operations can return HTTP 200 with success=false and no
		// machine-readable reason. Re-read the message to distinguish an
		// already completed deletion from a genuine delete failure.
		if isMAXOperationFailed(err) {
			if _, getErr := a.max.GetMessage(ctx, post.MAXMessageID); isMAXMessageNotFound(getErr) {
				return a.store.ClearPublicationForUser(ctx, userID, postID, channel.ID, post.MAXMessageID)
			}
		}
		return store.Post{}, err
	}
	return a.store.ClearPublicationForUser(ctx, userID, postID, channel.ID, post.MAXMessageID)
}

// SyncMAXPublication refreshes the canonical MAX URL, latest view count and
// actual pin state for one tenant-owned published post. View observations are
// appended transactionally by the store for future reports.
func (a *App) SyncMAXPublication(ctx context.Context, userID string, postID int64) (store.Post, error) {
	current, err := a.store.GetPostForUser(ctx, userID, postID)
	if err != nil {
		return store.Post{}, err
	}
	if isStoredMAXPublicationMissing(current) {
		return current, nil
	}
	post, channel, err := a.publishedPostForUser(ctx, userID, postID)
	if err != nil {
		return store.Post{}, err
	}
	if a.max == nil {
		return store.Post{}, ErrMAXNotConfigured
	}
	now := a.now().UTC()
	claimed, err := a.store.ClaimPostStatsAttemptForUser(ctx, userID, post.ID, channel.ID, post.MAXMessageID, now, manualMAXStatsCooldown)
	if err != nil {
		return store.Post{}, err
	}
	if !claimed {
		current, getErr := a.store.GetPostForUser(ctx, userID, post.ID)
		if getErr != nil {
			return store.Post{}, getErr
		}
		if isStoredMAXPublicationMissing(current) {
			return current, nil
		}
		retryAfter := manualMAXStatsCooldown
		if current.MAXStatsAttemptedAt != nil {
			remaining := current.MAXStatsAttemptedAt.UTC().Add(manualMAXStatsCooldown).Sub(now)
			if remaining > 0 && remaining < retryAfter {
				retryAfter = remaining
			}
		}
		return store.Post{}, &MAXStatsCooldownError{RetryAfter: retryAfter}
	}
	return a.syncClaimedMAXPublication(ctx, userID, post, channel, now)
}

// syncClaimedMAXPublicationForWorker consumes only a row selected by the
// cross-tenant scheduler and then revalidates its immutable workspace/compat
// owner boundary. It must not call personal-only HTTP authorization getters.
func (a *App) syncClaimedMAXPublicationForWorker(ctx context.Context, expected store.Post, syncedAt time.Time) (store.Post, error) {
	post, err := a.store.GetPost(ctx, expected.ID)
	if err != nil {
		return store.Post{}, err
	}
	if post.WorkspaceID != expected.WorkspaceID || post.UserID != expected.UserID {
		return store.Post{}, fmt.Errorf("%w: scheduled stats post changed workspace", ErrConflict)
	}
	workspace, err := a.store.GetWorkspace(ctx, post.WorkspaceID)
	if err != nil {
		return store.Post{}, err
	}
	if workspace.ArchivedAt != nil || workspace.CompatOwnerUserID != post.UserID {
		return store.Post{}, fmt.Errorf("%w: publication workspace is no longer active", ErrConflict)
	}
	if post.Status != store.PostStatusPublished || strings.TrimSpace(post.MAXMessageID) == "" || post.ChannelID == nil {
		return store.Post{}, fmt.Errorf("%w: post has no active MAX publication", ErrConflict)
	}
	channel, err := a.store.GetChannel(ctx, *post.ChannelID)
	if err != nil {
		return store.Post{}, err
	}
	if channel.WorkspaceID != post.WorkspaceID || channel.UserID != post.UserID || !channel.Active {
		return store.Post{}, fmt.Errorf("%w: publication channel changed workspace or is inactive", ErrConflict)
	}
	return a.syncClaimedMAXPublication(ctx, post.UserID, post, channel, syncedAt)
}

func (a *App) syncClaimedMAXPublication(ctx context.Context, userID string, post store.Post, channel store.Channel, syncedAt time.Time) (result store.Post, resultErr error) {
	startedAt := time.Now()
	defer func() {
		a.metrics.ObservePublicationOperation("sync", metricOutcome(resultErr), time.Since(startedAt))
	}()
	message, err := a.max.GetMessage(ctx, post.MAXMessageID)
	if err != nil {
		if isMAXMessageNotFound(err) {
			return a.store.MarkMAXPublicationMissingForUser(ctx, userID, post.ID, channel.ID, post.MAXMessageID)
		}
		return store.Post{}, err
	}
	if err := validateMAXMessageOwnership(message, post.MAXMessageID, channel.MAXChatID); err != nil {
		return store.Post{}, err
	}
	pinnedMessage, err := a.max.GetPinnedMessage(ctx, channel.MAXChatID)
	if err != nil {
		// A MAX 404 is meaningful (missing/inaccessible chat or message) and
		// must not be converted into a false local pin state.
		return store.Post{}, err
	}
	pinned := false
	if pinnedMessage != nil {
		if err := validateMAXMessageChannel(*pinnedMessage, channel.MAXChatID); err != nil {
			return store.Post{}, err
		}
		pinned = pinnedMessage.MessageID == post.MAXMessageID
	}
	return a.store.SyncPublicationMetadataForUser(ctx, userID, post.ID, channel.ID, post.MAXMessageID,
		message.URL, message.Views, syncedAt.UTC(), pinned)
}

func (a *App) PinPost(ctx context.Context, userID string, postID int64) (result store.Post, resultErr error) {
	startedAt := time.Now()
	defer func() {
		a.metrics.ObservePublicationOperation("pin", metricOutcome(resultErr), time.Since(startedAt))
	}()
	post, channel, err := a.publishedPostForUser(ctx, userID, postID)
	if err != nil {
		return store.Post{}, err
	}
	if a.max == nil {
		return store.Post{}, ErrMAXNotConfigured
	}
	if err := a.requirePinAccess(ctx, channel); err != nil {
		return store.Post{}, err
	}
	if err := a.max.PinMessage(ctx, channel.MAXChatID, post.MAXMessageID); err != nil {
		return store.Post{}, err
	}
	return a.store.SetPublicationPinnedForUser(ctx, userID, post.ID, channel.ID, post.MAXMessageID, true)
}

func (a *App) UnpinPost(ctx context.Context, userID string, postID int64) (result store.Post, resultErr error) {
	startedAt := time.Now()
	defer func() {
		a.metrics.ObservePublicationOperation("unpin", metricOutcome(resultErr), time.Since(startedAt))
	}()
	post, channel, err := a.publishedPostForUser(ctx, userID, postID)
	if err != nil {
		return store.Post{}, err
	}
	if a.max == nil {
		return store.Post{}, ErrMAXNotConfigured
	}
	if err := a.requirePinAccess(ctx, channel); err != nil {
		return store.Post{}, err
	}
	current, err := a.max.GetPinnedMessage(ctx, channel.MAXChatID)
	if err != nil {
		return store.Post{}, err
	}
	if current == nil {
		// DELETE is intentionally idempotent. The pin may have been removed in
		// MAX directly, or the pinned publication may already have been deleted.
		// In both cases the requested end state is reached, so reconcile the
		// stale local flag instead of returning a conflict.
		return a.store.SetPublicationPinnedForUser(ctx, userID, post.ID, channel.ID, post.MAXMessageID, false)
	}
	if err := validateMAXMessageChannel(*current, channel.MAXChatID); err != nil {
		return store.Post{}, err
	}
	if current.MessageID != post.MAXMessageID {
		// Another post is pinned now. Do not remove that pin; only reconcile this
		// post, which is already unpinned in MAX.
		return a.store.SetPublicationPinnedForUser(ctx, userID, post.ID, channel.ID, post.MAXMessageID, false)
	}
	if err := a.max.UnpinMessage(ctx, channel.MAXChatID); err != nil {
		return store.Post{}, err
	}
	return a.store.SetPublicationPinnedForUser(ctx, userID, post.ID, channel.ID, post.MAXMessageID, false)
}

func (a *App) publishedPostForUser(ctx context.Context, userID string, postID int64) (store.Post, store.Channel, error) {
	post, err := a.store.GetPostForUser(ctx, userID, postID)
	if err != nil {
		return store.Post{}, store.Channel{}, err
	}
	if post.Status != store.PostStatusPublished || strings.TrimSpace(post.MAXMessageID) == "" || post.ChannelID == nil {
		return store.Post{}, store.Channel{}, fmt.Errorf("%w: post has no active MAX publication", ErrConflict)
	}
	channel, err := a.store.GetChannelForUser(ctx, userID, *post.ChannelID)
	if err != nil {
		return store.Post{}, store.Channel{}, err
	}
	if !channel.Active {
		return store.Post{}, store.Channel{}, errors.New("selected MAX channel is inactive")
	}
	return post, channel, nil
}

func (a *App) requirePinAccess(ctx context.Context, channel store.Channel) error {
	info, membership, err := a.inspectChannel(ctx, channel)
	if err != nil {
		return err
	}
	diagnostics := channelDiagnostics(info, membership)
	if !diagnostics.CanPin {
		return &ChannelAccessError{Diagnostics: diagnostics,
			Message: "MAX pin_message permission is required to pin or unpin posts"}
	}
	return nil
}

func validateMAXMessageOwnership(message maxclient.Message, messageID, chatID string) error {
	if message.MessageID != messageID {
		return fmt.Errorf("%w: MAX returned a different publication", ErrConflict)
	}
	return validateMAXMessageChannel(message, chatID)
}

func (a *App) markMAXPublicationMissing(ctx context.Context, post store.Post) (store.Post, error) {
	if post.ChannelID == nil {
		return store.Post{}, errors.New("published post has no channel_id")
	}
	return a.store.MarkMAXPublicationMissingForUser(ctx, post.UserID, post.ID, *post.ChannelID, post.MAXMessageID)
}

func isMAXMessageNotFound(err error) bool {
	var apiErr *maxclient.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

func isMAXOperationFailed(err error) bool {
	var apiErr *maxclient.Error
	return errors.As(err, &apiErr) && apiErr.Code == "operation_failed"
}

func isStoredMAXPublicationMissing(post store.Post) bool {
	return post.Status == store.PostStatusFailed &&
		post.LastError == store.MAXPublicationMissingLastError &&
		strings.TrimSpace(post.MAXMessageID) == ""
}

func metricOutcome(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, store.ErrMediaQuotaExceeded):
		return "quota_exceeded"
	case errors.Is(err, store.ErrMediaUploadBusy):
		return "busy"
	default:
		return "error"
	}
}

func validateMAXMessageChannel(message maxclient.Message, chatID string) error {
	if strings.TrimSpace(message.MessageID) == "" || message.ChatID == "" || message.ChatID != chatID {
		return fmt.Errorf("%w: MAX message does not belong to the post channel", ErrConflict)
	}
	return nil
}

func (a *App) RunScheduler(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		a.logger.Error("scheduler interval must be positive", "interval", interval)
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	a.runSchedulerCycle(ctx, a.now().UTC())
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.runSchedulerCycle(ctx, a.now().UTC())
		}
	}
}

func (a *App) runSchedulerCycle(ctx context.Context, now time.Time) {
	startedAt := time.Now()
	defer func() {
		a.metrics.ObserveSchedulerCycle(time.Since(startedAt), a.now().UTC())
	}()
	if a.max != nil {
		a.publishDueAt(ctx, now)
		a.syncDueMAXStats(ctx, now)
		a.syncDueChannelParticipantStats(ctx, now)
	}
	a.cleanupDueMedia(ctx, now)
	a.runBillingCycle(ctx, now)
	if err := a.store.PurgeExpiredMAXAuthAttempts(ctx, now.UTC()); err != nil {
		a.logger.Error("scheduler could not purge expired MAX auth attempts", "error", err)
	}
}

func (a *App) cleanupDueMedia(ctx context.Context, now time.Time) {
	a.mediaCleanupMu.Lock()
	policy := a.mediaPolicy
	if !a.lastMediaCleanup.IsZero() && now.Before(a.lastMediaCleanup.Add(policy.CleanupInterval)) {
		a.mediaCleanupMu.Unlock()
		return
	}
	a.lastMediaCleanup = now
	a.mediaCleanupMu.Unlock()

	cleanupCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	result, err := a.store.CleanupOrphanMedia(cleanupCtx, now.Add(-policy.OrphanGrace), policy.CleanupBatch, a.media.Delete)
	if err != nil {
		a.metrics.ObserveSchedulerJob("media_cleanup", "error")
		a.metrics.ObserveMediaOperation("cleanup", "error")
		a.logger.Error("scheduler could not clean orphan media", "error", err)
		return
	}
	a.metrics.ObserveSchedulerJob("media_cleanup", "success")
	a.metrics.SetSchedulerDue("media_cleanup", int(result.AssetsRemoved+result.ObjectsDeleted))
	a.metrics.ObserveMediaOperation("cleanup", "success")
	if result.AssetsRemoved > 0 || result.ObjectsDeleted > 0 {
		a.logger.Info("orphan media cleanup completed", "assets_removed", result.AssetsRemoved,
			"objects_deleted", result.ObjectsDeleted, "bytes_released", result.BytesReleased)
	}
}

func (a *App) syncDueMAXStats(ctx context.Context, now time.Time) {
	if a.max == nil {
		return
	}
	posts, err := a.store.ListPostsDueForStats(ctx, now.UTC(), time.Hour, 10)
	if err != nil {
		a.metrics.ObserveSchedulerJob("stats_sync_scan", "error")
		a.logger.Error("scheduler could not list posts due for MAX stats", "error", err)
		return
	}
	a.metrics.ObserveSchedulerJob("stats_sync_scan", "success")
	a.metrics.SetSchedulerDue("stats_sync", len(posts))
	const parallelism = 2
	workerCount := min(parallelism, len(posts))
	jobs := make(chan store.Post)
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for post := range jobs {
				if post.ChannelID == nil {
					a.metrics.ObserveSchedulerJob("stats_sync", "skipped")
					continue
				}
				claimed, claimErr := a.store.ClaimPostStatsAttemptForUser(ctx, post.UserID, post.ID, *post.ChannelID,
					post.MAXMessageID, now.UTC(), time.Hour)
				if claimErr != nil {
					a.metrics.ObserveSchedulerJob("stats_sync", "error")
					a.logger.Warn("could not claim MAX post statistics synchronization", "post_id", post.ID, "error", claimErr)
					continue
				}
				if !claimed {
					a.metrics.ObserveSchedulerJob("stats_sync", "skipped")
					continue
				}
				syncCtx, cancel := context.WithTimeout(ctx, time.Minute)
				_, syncErr := a.syncClaimedMAXPublicationForWorker(syncCtx, post, now.UTC())
				cancel()
				if syncErr != nil {
					a.metrics.ObserveSchedulerJob("stats_sync", "error")
					// A confirmed GET-message 404 is reconciled inside the sync and
					// returns success. Other upstream failures (including a failed
					// pin lookup) leave the publication intact for a later retry.
					a.logger.Warn("could not synchronize MAX post statistics", "post_id", post.ID, "error", syncErr)
				} else {
					a.metrics.ObserveSchedulerJob("stats_sync", "success")
				}
			}
		}()
	}
	for _, post := range posts {
		select {
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return
		case jobs <- post:
		}
	}
	close(jobs)
	workers.Wait()
}

func (a *App) syncDueChannelParticipantStats(ctx context.Context, now time.Time) {
	if a.max == nil {
		return
	}
	channels, err := a.store.ListChannelsDueForParticipantStats(ctx, now.UTC(), channelParticipantStatsInterval, 10)
	if err != nil {
		a.metrics.ObserveSchedulerJob("channel_participants_scan", "error")
		a.logger.Error("scheduler could not list channels due for MAX participant stats", "error", err)
		return
	}
	a.metrics.ObserveSchedulerJob("channel_participants_scan", "success")
	a.metrics.SetSchedulerDue("channel_participants_sync", len(channels))
	const parallelism = 2
	workerCount := min(parallelism, len(channels))
	jobs := make(chan store.Channel)
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for channel := range jobs {
				claimed, claimErr := a.store.ClaimChannelParticipantStatsAttemptForUser(ctx, channel.UserID, channel.ID,
					channel.MAXChatID, now.UTC(), channelParticipantStatsInterval)
				if claimErr != nil {
					a.metrics.ObserveSchedulerJob("channel_participants_sync", "error")
					a.logger.Warn("could not claim MAX channel participant statistics synchronization", "channel_id", channel.ID, "error", claimErr)
					continue
				}
				if !claimed {
					a.metrics.ObserveSchedulerJob("channel_participants_sync", "skipped")
					continue
				}
				syncCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				_, syncErr := a.syncClaimedChannelParticipantStats(syncCtx, channel, a.now().UTC())
				cancel()
				if syncErr != nil {
					a.metrics.ObserveSchedulerJob("channel_participants_sync", "error")
					a.logger.Warn("could not synchronize MAX channel participant statistics", "channel_id", channel.ID, "error", syncErr)
				} else {
					a.metrics.ObserveSchedulerJob("channel_participants_sync", "success")
				}
			}
		}()
	}
	for _, channel := range channels {
		select {
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return
		case jobs <- channel:
		}
	}
	close(jobs)
	workers.Wait()
}

func (a *App) syncClaimedChannelParticipantStats(ctx context.Context, channel store.Channel, capturedAt time.Time) (store.Channel, error) {
	workspace, err := a.store.GetWorkspace(ctx, channel.WorkspaceID)
	if err != nil {
		return store.Channel{}, err
	}
	if workspace.ArchivedAt != nil || workspace.CompatOwnerUserID != channel.UserID {
		return store.Channel{}, fmt.Errorf("%w: workspace channel is no longer active", ErrConflict)
	}
	info, err := a.max.GetChat(ctx, channel.MAXChatID)
	if err != nil {
		return store.Channel{}, err
	}
	if err := validateChannelParticipantInfo(channel, info); err != nil {
		return store.Channel{}, err
	}
	return a.store.SyncChannelParticipantStatsForUser(ctx, channel.UserID, channel.ID, channel.MAXChatID,
		maxclient.SafeAssetURL(info.Icon.URL), info.ParticipantsCount, capturedAt.UTC())
}

func validateChannelParticipantInfo(channel store.Channel, info maxclient.ChatInfo) error {
	if info.ChatID != channel.MAXChatID {
		return fmt.Errorf("%w: MAX participant stats returned another channel", ErrConflict)
	}
	if channel.VerifiedMAXOwnerID == "" || info.OwnerID == "" || info.OwnerID != channel.VerifiedMAXOwnerID {
		return fmt.Errorf("%w: MAX channel ownership changed before participant statistics were synchronized", ErrConflict)
	}
	if info.Type != "channel" || info.Status != "active" {
		return fmt.Errorf("%w: MAX participant stats require an active channel", ErrConflict)
	}
	return nil
}

func (a *App) publishDueAt(ctx context.Context, now time.Time) {
	if a.max == nil {
		a.logger.Warn("scheduler skipped because MAX is not configured")
		return
	}
	now = now.UTC()
	recovered, err := a.store.RecoverStalePublishing(ctx, now.Add(-10*time.Minute))
	if err != nil {
		a.metrics.ObserveSchedulerJob("publication_recovery", "error")
		a.logger.Error("scheduler could not recover stale publishing posts", "error", err)
		return
	}
	a.metrics.ObserveSchedulerJob("publication_recovery", "success")
	a.metrics.AddRecoveredPublications(recovered)
	if recovered > 0 {
		a.logger.Warn("recovered interrupted publishing posts", "count", recovered)
	}
	ids, err := a.store.DuePostIDs(ctx, now, 25)
	if err != nil {
		a.metrics.ObserveSchedulerJob("publish_scan", "error")
		a.logger.Error("scheduler could not list due posts", "error", err)
		return
	}
	a.metrics.ObserveSchedulerJob("publish_scan", "success")
	a.metrics.SetSchedulerDue("publish", len(ids))
	const parallelism = 3
	workerCount := min(parallelism, len(ids))
	jobs := make(chan int64)
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for id := range jobs {
				// A stop signal cancels ctx immediately, but aborting a post
				// mid-publication can turn a message that already reached MAX
				// into a terminal failed post (and a duplicate after a manual
				// retry). Publications therefore run on a context detached from
				// scheduler cancellation, bounded by their own timeout; main
				// waits for the scheduler goroutine before exiting.
				publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
				published, publishErr := a.publishScheduledPost(publishCtx, id, now)
				cancel()
				if publishErr != nil {
					a.metrics.ObserveSchedulerJob("publish", "error")
					a.logger.Error("scheduled post failed", "post_id", id, "error", publishErr)
				} else if published {
					a.metrics.ObserveSchedulerJob("publish", "success")
					a.logger.Info("scheduled post published", "post_id", id)
				} else {
					a.metrics.ObserveSchedulerJob("publish", "skipped")
				}
			}
		}()
	}

dispatch:
	for _, id := range ids {
		select {
		case jobs <- id:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(jobs)
	workers.Wait()
}

func (a *App) publishScheduledPost(ctx context.Context, postID int64, now time.Time) (bool, error) {
	queued, err := a.store.GetPost(ctx, postID)
	if err != nil {
		return false, err
	}
	if err := a.requireApprovedCurrentRevision(ctx, queued); err != nil {
		if errors.Is(err, ErrApprovalRequired) {
			// A revoked approval is not a transient failure: left scheduled, the
			// post would be retried every cycle forever and could crowd fresh due
			// posts out of the DuePostIDs batch. Take it off the calendar with a
			// clear last_error; re-approving and rescheduling brings it back.
			claimed, claimErr := a.store.ClaimScheduledForPublishing(ctx, postID, now.UTC())
			if errors.Is(claimErr, store.ErrScheduleNotDue) {
				return false, nil
			}
			if claimErr != nil {
				return false, claimErr
			}
			_, failErr := a.fail(claimed.ID, err)
			return false, failErr
		}
		return false, err
	}
	post, err := a.store.ClaimScheduledForPublishing(ctx, postID, now.UTC())
	if errors.Is(err, store.ErrScheduleNotDue) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	sendStarted := false
	_, err = a.publishClaimedPost(ctx, post, &sendStarted)
	if err != nil && errors.Is(err, context.Canceled) && !sendStarted && queued.ScheduledAt != nil {
		// The publication was interrupted by a canceled context (typically a
		// service stop) before anything was sent to MAX, so retrying cannot
		// duplicate the message. Return the post to the calendar with its
		// original slot so the next cycle retries it instead of leaving a
		// terminal failure behind. Once the send has started the message may
		// already be in the channel, so the post stays failed with last_error
		// for a human to resolve.
		a.restoreInterruptedSchedule(postID, *queued.ScheduledAt)
	}
	return err == nil, err
}

// restoreInterruptedSchedule undoes the failed/publishing state of a
// scheduled publication that was cut short by a canceled context. It runs on
// its own short-lived context because the interrupting cancellation must not
// prevent the state from being written.
func (a *App) restoreInterruptedSchedule(postID int64, scheduledAt time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := a.store.RestoreInterruptedSchedule(ctx, postID, scheduledAt); err != nil {
		a.logger.Error("could not restore the schedule of an interrupted publication",
			"post_id", postID, "error", err)
	}
}

func (a *App) SchedulePost(ctx context.Context, postID int64, scheduledAt time.Time) (store.Post, error) {
	scheduledAt = scheduledAt.UTC()
	if scheduledAt.IsZero() || !scheduledAt.After(a.now().UTC()) {
		return store.Post{}, errors.New("scheduled_at must be in the future")
	}
	post, err := a.store.GetPost(ctx, postID)
	if err != nil {
		return store.Post{}, err
	}
	if _, err := a.validateForPublish(ctx, post); err != nil {
		return store.Post{}, err
	}
	if err := a.requireApprovedCurrentRevision(ctx, post); err != nil {
		return store.Post{}, err
	}
	return a.store.SetPostScheduledIfUnchanged(ctx, post, scheduledAt)
}

// requireApprovedCurrentRevision is shared by manual and scheduled publishing.
// Personal workspaces preserve the legacy flow because their approval policy
// is disabled by default. Team workspaces fail closed on missing/stale review.
func (a *App) requireApprovedCurrentRevision(ctx context.Context, post store.Post) error {
	if strings.TrimSpace(post.WorkspaceID) == "" {
		// Compatibility for callers constructing transient posts in tests. Every
		// persisted post receives a workspace_id from migration 016.
		return nil
	}
	workspace, err := a.store.GetWorkspace(ctx, post.WorkspaceID)
	if err != nil {
		return err
	}
	if workspace.ArchivedAt != nil {
		return fmt.Errorf("%w: workspace is archived", ErrConflict)
	}
	if !workspace.ApprovalRequired {
		return nil
	}
	approved, err := a.store.IsCurrentRevisionApproved(ctx, post.WorkspaceID, post.ID)
	if err != nil {
		return err
	}
	if !approved {
		return ErrApprovalRequired
	}
	return nil
}

// ValidatePostForScheduling performs all local checks required before a post
// enters the calendar. It does not call MAX; the worker rechecks remote access
// immediately before the actual publication.
func (a *App) ValidatePostForScheduling(ctx context.Context, post store.Post) error {
	_, err := a.validateForPublish(ctx, post)
	return err
}

func (a *App) validateForPublish(ctx context.Context, post store.Post) (store.Channel, error) {
	if !store.ValidFormat(post.Format) {
		return store.Channel{}, errors.New("post format must be markdown or html")
	}
	if strings.TrimSpace(post.Content) == "" && len(post.Attachments) == 0 && post.ImageURL == "" {
		return store.Channel{}, errors.New("post content or a media attachment is required")
	}
	if utf8.RuneCountInString(post.Content) > 4000 {
		return store.Channel{}, errors.New("MAX post content must not exceed 4000 characters")
	}
	if err := store.ValidateLinkButtonsForPublish(post.LinkButtons); err != nil {
		return store.Channel{}, err
	}
	if err := validatePostAttachments(post); err != nil {
		return store.Channel{}, err
	}
	if post.ChannelID == nil {
		return store.Channel{}, errors.New("post channel_id is required")
	}
	channel, err := a.store.GetChannel(ctx, *post.ChannelID)
	if err != nil {
		return store.Channel{}, err
	}
	if !channel.Active {
		return store.Channel{}, errors.New("selected MAX channel is inactive")
	}
	if post.WorkspaceID != "" {
		if channel.WorkspaceID == "" || channel.WorkspaceID != post.WorkspaceID {
			return store.Channel{}, errors.New("post and channel workspace do not match")
		}
	} else if post.UserID == "" || channel.UserID != post.UserID {
		return store.Channel{}, errors.New("post and channel ownership do not match")
	}
	return channel, nil
}

func maxLinkButtons(buttons []store.LinkButton) []maxclient.LinkButton {
	if buttons == nil {
		return nil
	}
	result := make([]maxclient.LinkButton, len(buttons))
	for i, button := range buttons {
		result[i] = maxclient.LinkButton{Text: strings.TrimSpace(button.Text), URL: strings.TrimSpace(button.URL)}
	}
	return result
}

func (a *App) inspectChannel(ctx context.Context, channel store.Channel) (maxclient.ChatInfo, maxclient.Membership, error) {
	info, err := a.max.GetChat(ctx, channel.MAXChatID)
	if err != nil {
		return maxclient.ChatInfo{}, maxclient.Membership{}, err
	}
	info, err = a.resolveMAXChatOwner(ctx, info, channel.VerifiedMAXOwnerID, false)
	if err != nil {
		return maxclient.ChatInfo{}, maxclient.Membership{}, err
	}
	if channel.VerifiedMAXOwnerID == "" || info.OwnerID == "" || info.OwnerID != channel.VerifiedMAXOwnerID {
		return maxclient.ChatInfo{}, maxclient.Membership{}, &ChannelAccessError{
			Diagnostics: ChannelDiagnostics{ChatID: info.ChatID, Type: info.Type, Status: info.Status},
			Message:     "MAX channel ownership changed; reconnect the channel before publishing",
		}
	}
	membership, err := a.max.GetMembership(ctx, channel.MAXChatID)
	if err != nil {
		return maxclient.ChatInfo{}, maxclient.Membership{}, err
	}
	return info, membership, nil
}

func channelDiagnostics(info maxclient.ChatInfo, membership maxclient.Membership) ChannelDiagnostics {
	permissions := make([]string, len(membership.Permissions))
	for i, permission := range membership.Permissions {
		permissions[i] = string(permission)
	}
	missing := make([]string, 0, 5)
	if !membership.IsAdmin {
		missing = append(missing, "admin")
	}
	hasRead := membership.HasPermission(maxclient.PermissionReadAllMessages)
	hasWrite := membership.HasPermission(maxclient.PermissionWrite)
	if !hasRead {
		missing = append(missing, string(maxclient.PermissionReadAllMessages))
	}
	if !hasWrite {
		missing = append(missing, string(maxclient.PermissionWrite))
	}
	hasEdit := membership.HasPermission(maxclient.PermissionEdit)
	hasDelete := membership.HasPermission(maxclient.PermissionDelete)
	hasPin := membership.HasPermission(maxclient.PermissionPinMessage)
	hasChangeInfo := membership.HasPermission(maxclient.PermissionChangeChatInfo)
	if !hasEdit {
		missing = append(missing, string(maxclient.PermissionEdit))
	}
	if !hasDelete {
		missing = append(missing, string(maxclient.PermissionDelete))
	}
	activeChannel := info.Type == "channel" && info.Status == "active"
	return ChannelDiagnostics{
		ChatID: info.ChatID, Type: info.Type, Status: info.Status, IsAdmin: membership.IsAdmin,
		Permissions:                permissions,
		CanPublish:                 activeChannel && membership.IsAdmin && hasRead && hasWrite,
		CanEdit:                    activeChannel && membership.IsAdmin && hasRead && hasEdit,
		CanDelete:                  activeChannel && membership.IsAdmin && hasRead && hasDelete,
		CanPin:                     activeChannel && membership.IsAdmin && hasPin,
		CanChangeInfo:              activeChannel && membership.IsAdmin && hasChangeInfo,
		MissingRequiredPermissions: missing,
	}
}

func (a *App) imageTokens(ctx context.Context, post store.Post) ([]string, error) {
	if post.ImageURL == "" {
		return nil, nil
	}
	imagePath := post.ImagePath
	if imagePath == "" {
		resolved, err := a.media.ResolveURL(ctx, post.ImageURL)
		if err != nil {
			return nil, err
		}
		imagePath = resolved
	}
	object, err := a.media.Open(ctx, imagePath)
	if err != nil {
		return nil, fmt.Errorf("open post image: %w", err)
	}
	defer func() {
		_ = object.Body.Close()
	}()
	upload, err := a.max.UploadImage(ctx, object.Filename, object.Body)
	if err != nil {
		return nil, err
	}
	return []string{upload.Token}, nil
}

func (a *App) releasePublishedUpdate(claimed store.Post, lastError string) (store.Post, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return a.store.ReleasePublishedUpdate(ctx, claimed, lastError)
}

func (a *App) fail(postID int64, cause error) (store.Post, error) {
	a.logger.Warn("MAX publication failed", "post_id", postID, "error", cause)
	// The failure is recorded on a context detached from the caller (which may
	// already be canceled) but bounded, so a hung database write cannot block a
	// scheduler worker forever.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := a.store.MarkPublishFailed(ctx, postID, publicationFailureMessage(cause)); err != nil {
		a.logger.Error("could not persist publication failure", "post_id", postID, "error", err)
	}
	return store.Post{}, cause
}

func publicationFailureMessage(cause error) string {
	if errors.Is(cause, context.DeadlineExceeded) {
		return "MAX не ответил вовремя. Проверьте канал и попробуйте опубликовать ещё раз."
	}
	if errors.Is(cause, ErrApprovalRequired) {
		return "Согласование текущей версии поста отозвано. Отправьте пост на согласование и опубликуйте или запланируйте его заново."
	}
	var channelErr *ChannelAccessError
	if errors.As(cause, &channelErr) {
		return "Помощнику MaxPosty не хватает прав для публикации. Проверьте его права администратора в канале."
	}
	var maxErr *maxclient.Error
	if errors.As(cause, &maxErr) {
		if maxErr.Code == "errors.send-message.channel-notify" ||
			maxErr.Message == "errors.send-message.channel-notify" {
			return "MAX требует уведомить подписчиков о публикации. Уведомления включены автоматически — попробуйте ещё раз."
		}
		return "MAX не смог опубликовать пост. Проверьте подключение канала и попробуйте ещё раз."
	}
	if strings.Contains(strings.ToLower(cause.Error()), "returned no message id") {
		return "MAX принял публикацию, но не подтвердил её. Проверьте канал перед повторной попыткой."
	}
	return "Не удалось опубликовать пост. Проверьте канал и попробуйте ещё раз."
}
