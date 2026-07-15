package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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
	ErrMAXNotConfigured        = errors.New("MAX integration is not configured")
	ErrMAXChannelEventRequired = errors.New("MAX channel event is required to discover the channel id")
	ErrOpenAINotConfigured     = errors.New("OpenAI integration is not configured")
	ErrResearchNotConfigured   = errors.New("OpenAI research integration is not configured")
	ErrConflict                = errors.New("resource state conflict")
)

const (
	manualMAXStatsCooldown          = 15 * time.Second
	channelParticipantStatsInterval = time.Hour
)

type MAXClient interface {
	GetMe(context.Context) (maxclient.BotInfo, error)
	GetChat(context.Context, string) (maxclient.ChatInfo, error)
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

// Metrics receives only bounded operational dimensions. Implementations must
// never attach post, channel or user identifiers as metric labels.
type Metrics interface {
	ObservePublicationOperation(operation, outcome string, elapsed time.Duration)
	ObserveSchedulerJob(job, outcome string)
	SetSchedulerDue(job string, count int)
	ObserveSchedulerCycle(elapsed time.Duration, completedAt time.Time)
	AddRecoveredPublications(count int64)
}

type noopMetrics struct{}

func (noopMetrics) ObservePublicationOperation(string, string, time.Duration) {}
func (noopMetrics) ObserveSchedulerJob(string, string)                        {}
func (noopMetrics) SetSchedulerDue(string, int)                               {}
func (noopMetrics) ObserveSchedulerCycle(time.Duration, time.Time)            {}
func (noopMetrics) AddRecoveredPublications(int64)                            {}

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
	messageChatDiscovery singleflight.Group
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
	}
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

// ValidateImageRequest performs every available local check before an API
// handler reserves quota. The real OpenAI client validates its configured
// model's size rules; test or alternative clients still receive the common
// prompt and quality checks.
func (a *App) ValidateImageRequest(request openaiimg.GenerateRequest) error {
	if a.images == nil {
		return ErrOpenAINotConfigured
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

func (a *App) ObserveMAXChat(ctx context.Context, maxChatID string, active bool, eventAt time.Time) error {
	if a.max == nil {
		return ErrMAXNotConfigured
	}
	now := eventAt.UTC()
	if !active {
		return a.store.MarkObservedBotChatRemoved(ctx, maxChatID, now)
	}
	info, err := a.max.GetChat(ctx, maxChatID)
	if err != nil {
		return err
	}
	return a.store.UpsertObservedBotChat(ctx, store.ObservedBotChat{
		MAXChatID: info.ChatID, PublicLink: strings.TrimRight(strings.TrimSpace(info.Link), "/"),
		Title: info.Title, MAXOwnerID: info.OwnerID, IconURL: maxclient.SafeAssetURL(info.Icon.URL),
		ParticipantsCount: info.ParticipantsCount, Active: true, LastSeenAt: now,
	})
}

// DiscoverMAXChatFromMessage learns a channel from message_created only when
// it is absent from the active inventory. The second lookup inside singleflight
// closes the race between concurrent webhook deliveries and later retries.
func (a *App) DiscoverMAXChatFromMessage(ctx context.Context, maxChatID string, eventAt time.Time) error {
	if a.max == nil {
		return ErrMAXNotConfigured
	}
	if _, err := a.store.GetActiveObservedBotChat(ctx, "", maxChatID); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	_, err, _ := a.messageChatDiscovery.Do(maxChatID, func() (any, error) {
		if _, lookupErr := a.store.GetActiveObservedBotChat(ctx, "", maxChatID); lookupErr == nil {
			return nil, nil
		} else if !errors.Is(lookupErr, store.ErrNotFound) {
			return nil, lookupErr
		}
		return nil, a.ObserveMAXChat(ctx, maxChatID, true, eventAt)
	})
	return err
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
	channel, err := a.store.UpsertConnectedChannel(ctx, store.Channel{
		VerifiedMAXOwnerID: info.OwnerID, MAXChatID: info.ChatID, Title: title, PublicLink: canonicalLink, IconURL: maxclient.SafeAssetURL(info.Icon.URL),
		ParticipantsCount: info.ParticipantsCount, IsChannel: true, Active: true,
	})
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
	discoveredByLink := false
	if observedErr == nil {
		var err error
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
		var err error
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
		if info.Type != "channel" || info.Status != "active" {
			return ChannelClaimCandidate{}, &ChannelAccessError{
				Diagnostics: ChannelDiagnostics{ChatID: info.ChatID, Type: info.Type, Status: info.Status},
				Message:     "The public link must point to an active MAX channel",
			}
		}
		discoveredByLink = true
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
	if discoveredByLink {
		canonicalLink := strings.TrimRight(strings.TrimSpace(info.Link), "/")
		if canonicalLink == "" {
			canonicalLink = publicLink
		} else if normalized, normalizeErr := maxclient.NormalizeChatLink(canonicalLink); normalizeErr == nil {
			canonicalLink = "https://max.ru/" + strings.TrimPrefix(normalized, "@")
		} else {
			// Never persist an unexpected URL returned by the upstream API.
			canonicalLink = publicLink
		}
		info.Link = canonicalLink
		if err := a.store.UpsertObservedBotChat(ctx, store.ObservedBotChat{
			MAXChatID: info.ChatID, PublicLink: canonicalLink, Title: info.Title, MAXOwnerID: info.OwnerID,
			IconURL: maxclient.SafeAssetURL(info.Icon.URL), ParticipantsCount: info.ParticipantsCount, Active: true, LastSeenAt: a.now().UTC(),
		}); err != nil {
			return ChannelClaimCandidate{}, err
		}
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
	channel, err := a.store.CompleteChannelClaim(ctx, claim, store.Channel{
		UserID: claim.UserID, VerifiedMAXOwnerID: info.OwnerID, MAXChatID: info.ChatID, Title: title,
		PublicLink: strings.TrimSpace(info.Link), IconURL: maxclient.SafeAssetURL(info.Icon.URL), ParticipantsCount: info.ParticipantsCount,
		IsChannel: true, Active: true,
	})
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
	channel, err := a.store.ConnectDiscoverableChannelForUser(ctx, userID, maxChatID, store.Channel{
		UserID: userID, VerifiedMAXOwnerID: info.OwnerID, MAXChatID: info.ChatID, Title: title,
		PublicLink: strings.TrimSpace(info.Link), IconURL: maxclient.SafeAssetURL(info.Icon.URL), ParticipantsCount: info.ParticipantsCount,
		IsChannel: true, Active: true,
	})
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
	info, membership, err := a.inspectChannel(ctx, channel)
	if err != nil {
		return ChannelCheck{}, err
	}
	diagnostics := channelDiagnostics(info, membership)
	if !channel.Active {
		diagnostics.CanPublish = false
		diagnostics.CanEdit = false
		diagnostics.CanDelete = false
		diagnostics.CanPin = false
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
	info, membership, err := a.inspectChannel(ctx, channel)
	if err != nil {
		return ChannelCheck{}, err
	}
	if err := validateChannelParticipantInfo(channel, info); err != nil {
		return ChannelCheck{}, err
	}
	channel, err = a.store.SyncChannelParticipantStatsForUser(ctx, userID, channel.ID, channel.MAXChatID,
		maxclient.SafeAssetURL(info.Icon.URL), info.ParticipantsCount, a.now().UTC())
	if err != nil {
		return ChannelCheck{}, err
	}
	diagnostics := channelDiagnostics(info, membership)
	if !channel.Active {
		diagnostics.CanPublish, diagnostics.CanEdit, diagnostics.CanDelete, diagnostics.CanPin = false, false, false, false
	}
	return ChannelCheck{Channel: channel, Diagnostics: diagnostics}, nil
}

func (a *App) GenerateImage(ctx context.Context, request openaiimg.GenerateRequest) (media.File, error) {
	if a.images == nil {
		return media.File{}, ErrOpenAINotConfigured
	}
	result, err := a.images.Generate(ctx, request)
	if err != nil {
		return media.File{}, err
	}
	return a.media.Save("openai.png", bytes.NewReader(result.Bytes))
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

func (a *App) GeneratePostImage(ctx context.Context, postID int64, request openaiimg.GenerateRequest) (store.Post, error) {
	post, err := a.store.GetPost(ctx, postID)
	if err != nil {
		return store.Post{}, err
	}
	if post.Status == store.PostStatusPublishing {
		return store.Post{}, fmt.Errorf("%w: post is currently publishing", ErrConflict)
	}
	if strings.TrimSpace(request.Prompt) == "" {
		request.Prompt = post.ImagePrompt
	}
	file, err := a.GenerateImage(ctx, request)
	if err != nil {
		return store.Post{}, err
	}
	prompt := request.Prompt
	return a.store.UpdatePost(ctx, postID, store.PostChanges{
		ImageURL: &file.URL, ImagePath: &file.Path, ImagePrompt: &prompt,
	})
}

func (a *App) SavePostImage(ctx context.Context, postID int64, filename string, reader io.Reader) (store.Post, error) {
	post, err := a.store.GetPost(ctx, postID)
	if err != nil {
		return store.Post{}, err
	}
	if post.Status == store.PostStatusPublishing {
		return store.Post{}, fmt.Errorf("%w: post is currently publishing", ErrConflict)
	}
	file, err := a.media.Save(filename, reader)
	if err != nil {
		return store.Post{}, err
	}
	emptyPrompt := ""
	return a.store.UpdatePost(ctx, postID, store.PostChanges{
		ImageURL: &file.URL, ImagePath: &file.Path, ImagePrompt: &emptyPrompt,
	})
}

func (a *App) PublishPost(ctx context.Context, postID int64) (store.Post, error) {
	if a.max == nil {
		return store.Post{}, ErrMAXNotConfigured
	}
	post, err := a.store.ClaimForPublishing(ctx, postID)
	if err != nil {
		return store.Post{}, fmt.Errorf("%w: %w", ErrConflict, err)
	}
	return a.publishClaimedPost(ctx, post)
}

func (a *App) publishClaimedPost(ctx context.Context, post store.Post) (result store.Post, resultErr error) {
	startedAt := time.Now()
	defer func() {
		a.metrics.ObservePublicationOperation("publish", metricOutcome(resultErr), time.Since(startedAt))
	}()
	postID := post.ID
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

	tokens, err := a.imageTokens(ctx, post)
	if err != nil {
		return a.fail(postID, err)
	}
	notify := post.Notify
	message, err := a.publishWithAttachmentRetry(ctx, maxclient.PublishRequest{
		ChatID: channel.MAXChatID, Text: post.Content, Format: maxclient.Format(post.Format),
		ImageTokens: tokens, LinkButtons: maxLinkButtons(post.LinkButtons),
		DisableLinkPreview: post.DisableLinkPreview, Notify: &notify,
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
	tokens := make([]string, 0)
	if post.ImageURL != "" {
		tokens, err = a.imageTokens(ctx, post)
		if err != nil {
			return store.Post{}, err
		}
	}
	notify := post.Notify
	err = a.editWithAttachmentRetry(ctx, maxclient.EditRequest{
		MessageID: post.MAXMessageID, Text: post.Content, Format: maxclient.Format(post.Format),
		ImageTokens: tokens, LinkButtons: maxLinkButtons(post.LinkButtons), Notify: &notify,
	})
	if err != nil {
		// MAX edit operations can return HTTP 200 with success=false and no
		// machine-readable reason. Re-read the message to distinguish an
		// external deletion (including the small race after our preflight)
		// from a genuine edit failure.
		if isMAXOperationFailed(err) {
			if _, getErr := a.max.GetMessage(ctx, post.MAXMessageID); isMAXMessageNotFound(getErr) {
				return a.markMAXPublicationMissing(ctx, post)
			}
		}
		return store.Post{}, err
	}
	return a.store.GetPost(ctx, postID)
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

func (a *App) syncClaimedMAXPublicationForUser(ctx context.Context, userID string, postID int64, syncedAt time.Time) (store.Post, error) {
	post, channel, err := a.publishedPostForUser(ctx, userID, postID)
	if err != nil {
		return store.Post{}, err
	}
	return a.syncClaimedMAXPublication(ctx, userID, post, channel, syncedAt)
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
	if err != nil {
		return "error"
	}
	return "success"
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
	a.publishDueAt(ctx, now)
	a.syncDueMAXStats(ctx, now)
	a.syncDueChannelParticipantStats(ctx, now)
	if err := a.store.PurgeExpiredMAXAuthAttempts(ctx, now.UTC()); err != nil {
		a.logger.Error("scheduler could not purge expired MAX auth attempts", "error", err)
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
				_, syncErr := a.syncClaimedMAXPublicationForUser(syncCtx, post.UserID, post.ID, now.UTC())
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
				publishCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
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
	post, err := a.store.ClaimScheduledForPublishing(ctx, postID, now.UTC())
	if errors.Is(err, store.ErrScheduleNotDue) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = a.publishClaimedPost(ctx, post)
	return err == nil, err
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
	return a.store.SetPostScheduledIfUnchanged(ctx, post, scheduledAt)
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
	if strings.TrimSpace(post.Content) == "" && post.ImageURL == "" {
		return store.Channel{}, errors.New("post content or an image is required")
	}
	if utf8.RuneCountInString(post.Content) > 4000 {
		return store.Channel{}, errors.New("MAX post content must not exceed 4000 characters")
	}
	if err := store.ValidateLinkButtonsForPublish(post.LinkButtons); err != nil {
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
	if post.UserID == "" || channel.UserID != post.UserID {
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
		MissingRequiredPermissions: missing,
	}
}

func (a *App) imageTokens(ctx context.Context, post store.Post) ([]string, error) {
	if post.ImageURL == "" {
		return nil, nil
	}
	imagePath := post.ImagePath
	if imagePath == "" {
		resolved, err := a.media.ResolveURL(post.ImageURL)
		if err != nil {
			return nil, err
		}
		imagePath = resolved
	}
	file, err := os.Open(imagePath)
	if err != nil {
		return nil, fmt.Errorf("open post image: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()
	upload, err := a.max.UploadImage(ctx, filepath.Base(imagePath), file)
	if err != nil {
		return nil, err
	}
	return []string{upload.Token}, nil
}

func (a *App) publishWithAttachmentRetry(ctx context.Context, request maxclient.PublishRequest) (maxclient.Message, error) {
	delays := []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second}
	var lastErr error
	for _, delay := range delays {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return maxclient.Message{}, ctx.Err()
			case <-timer.C:
			}
		}
		message, err := a.max.Publish(ctx, request)
		if err == nil {
			return message, nil
		}
		lastErr = err
		var apiErr *maxclient.Error
		if !errors.As(err, &apiErr) || apiErr.Code != "attachment.not.ready" {
			return maxclient.Message{}, err
		}
	}
	return maxclient.Message{}, lastErr
}

func (a *App) editWithAttachmentRetry(ctx context.Context, request maxclient.EditRequest) error {
	delays := []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second}
	var lastErr error
	for _, delay := range delays {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		if err := a.max.Edit(ctx, request); err != nil {
			lastErr = err
			var apiErr *maxclient.Error
			if !errors.As(err, &apiErr) || apiErr.Code != "attachment.not.ready" {
				return err
			}
			continue
		}
		return nil
	}
	return lastErr
}

func (a *App) fail(postID int64, cause error) (store.Post, error) {
	if _, err := a.store.MarkPublishFailed(context.Background(), postID, cause.Error()); err != nil {
		a.logger.Error("could not persist publication failure", "post_id", postID, "error", err)
	}
	return store.Post{}, cause
}
