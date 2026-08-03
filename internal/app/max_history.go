package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/store"
)

const (
	maxHistoryPageSize   = 100
	maxHistoryLease      = 2 * time.Minute
	maxHistoryTitleRunes = 200
)

// maxHistoryClient is an optional capability of the production MAX client.
// Keeping it out of MAXClient avoids widening every publication-only test
// double while still failing closed when history is unavailable.
type maxHistoryClient interface {
	GetMessagesPage(context.Context, string, int64, int) ([]maxclient.HistoryMessage, error)
}

// ChannelHistoryImportResult is the progress snapshot returned after one
// durable provider page. Clients may immediately request the next page while
// HasMore is true; a later retry resumes from the stored cursor.
type ChannelHistoryImportResult struct {
	ChannelID      int64  `json:"channel_id"`
	Status         string `json:"status"`
	ProcessedCount int64  `json:"processed_count"`
	ImportedCount  int64  `json:"imported_count"`
	ExistingCount  int64  `json:"existing_count"`
	SkippedCount   int64  `json:"skipped_count"`
	ExpectedCount  int64  `json:"expected_count"`
	HasMore        bool   `json:"has_more"`
	Partial        bool   `json:"partial"`
}

// ImportMAXChannelHistoryPage imports at most one MAX API page. The store owns
// the resumable cursor, idempotency keys and fencing token, so a browser may
// safely close, retry, or race another tab without duplicating publications.
func (a *App) ImportMAXChannelHistoryPage(
	ctx context.Context,
	actorUserID, workspaceID string,
	channelID int64,
) (ChannelHistoryImportResult, error) {
	if a.max == nil {
		return ChannelHistoryImportResult{}, ErrMAXNotConfigured
	}
	history, ok := a.max.(maxHistoryClient)
	if !ok {
		return ChannelHistoryImportResult{}, fmt.Errorf("%w: MAX history API is unavailable", ErrMAXNotConfigured)
	}

	now := a.now().UTC()
	channel, err := a.store.GetChannelForWorkspace(ctx, actorUserID, workspaceID, channelID)
	if err != nil {
		return ChannelHistoryImportResult{}, err
	}
	info, membership, err := a.inspectChannel(ctx, channel)
	if err != nil {
		return ChannelHistoryImportResult{}, err
	}
	if err := validateChannelParticipantInfo(channel, info); err != nil {
		return ChannelHistoryImportResult{}, err
	}
	diagnostics := channelDiagnostics(info, membership)
	if membership.UserID <= 0 || !membership.IsBot || !membership.IsAdmin ||
		!membership.HasPermission(maxclient.PermissionReadAllMessages) {
		return ChannelHistoryImportResult{}, &ChannelAccessError{
			Diagnostics: diagnostics,
			Message:     "MAX history access requires a valid bot administrator with read_all_messages permission",
		}
	}
	// Refresh messages_count before the first claim so partial-completion
	// diagnostics compare against the same provider snapshot as this import.
	channel, err = a.syncChannelMAXInfoForUser(ctx, channel.UserID, channel.ID, channel.MAXChatID, info, now)
	if err != nil {
		return ChannelHistoryImportResult{}, err
	}
	claim, err := a.store.ClaimMAXHistoryImport(
		ctx, actorUserID, workspaceID, channelID, now, maxHistoryLease,
	)
	if err != nil {
		return ChannelHistoryImportResult{}, err
	}
	fail := func(cause error) (ChannelHistoryImportResult, error) {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if releaseErr := a.store.ReleaseMAXHistoryImport(
			releaseCtx, actorUserID, workspaceID, claim.Generation, maxHistoryFailureCode(cause), a.now().UTC(),
		); releaseErr != nil {
			return ChannelHistoryImportResult{}, errors.Join(cause, releaseErr)
		}
		return ChannelHistoryImportResult{}, cause
	}

	before := int64(0)
	if claim.CursorFrom != nil {
		before = *claim.CursorFrom
	}
	messages, err := history.GetMessagesPage(ctx, claim.Channel.MAXChatID, before, maxHistoryPageSize)
	if err != nil {
		return fail(err)
	}
	items := make([]store.MAXHistoryItem, 0, len(messages))
	oldest := int64(0)
	for _, message := range messages {
		item, normalizeErr := maxHistoryItem(message, membership.UserID)
		if normalizeErr != nil {
			return fail(normalizeErr)
		}
		items = append(items, item)
		if oldest == 0 || message.TimestampMillis < oldest {
			oldest = message.TimestampMillis
		}
	}

	complete, nextFrom, stalledBoundary, err := maxHistoryPageCursor(len(messages), oldest, before)
	if err != nil {
		return fail(err)
	}
	progress, err := a.store.ApplyMAXHistoryPage(
		ctx, actorUserID, workspaceID, claim.Generation, items, nextFrom, complete, a.now().UTC(),
	)
	if err != nil {
		return fail(err)
	}
	result := channelHistoryImportResult(progress)
	result.Partial = result.Partial || stalledBoundary
	return result, nil
}

func maxHistoryPageCursor(count int, oldest, before int64) (bool, *int64, bool, error) {
	if count < 0 || count > maxHistoryPageSize {
		return false, nil, false, errors.New("MAX history page size is invalid")
	}
	if count < maxHistoryPageSize {
		return true, nil, false, nil
	}
	if oldest <= 0 {
		return false, nil, false, errors.New("MAX history cursor is invalid")
	}
	if before > 0 && oldest >= before {
		// The provider offers no secondary cursor for more than one full page at
		// an identical millisecond. The caller applies every newly observed ID
		// from this page and then reports an explicit partial completion.
		return true, nil, true, nil
	}
	// MAX documents from as inclusive. Repeating the oldest boundary avoids
	// silently skipping messages that share its timestamp; the store removes
	// repeated message IDs transactionally.
	cursor := oldest
	return false, &cursor, false, nil
}

func channelHistoryImportResult(progress store.MAXHistoryImport) ChannelHistoryImportResult {
	complete := progress.Status == "complete"
	return ChannelHistoryImportResult{
		ChannelID: progress.ChannelID, Status: progress.Status,
		ProcessedCount: progress.ProcessedCount, ImportedCount: progress.ImportedCount,
		ExistingCount: progress.ExistingCount, SkippedCount: progress.SkippedCount,
		ExpectedCount: progress.ExpectedCount, HasMore: !complete,
		Partial: complete && progress.ExpectedCount > 0 && progress.ProcessedCount < progress.ExpectedCount,
	}
}

func maxHistoryItem(message maxclient.HistoryMessage, currentBotUserID int64) (store.MAXHistoryItem, error) {
	messageID := strings.TrimSpace(message.MessageID)
	if messageID == "" {
		return store.MAXHistoryItem{}, errors.New("MAX history message has no id")
	}
	if message.TimestampMillis <= 0 {
		return store.MAXHistoryItem{}, fmt.Errorf("MAX history message %s has no timestamp", messageID)
	}
	publishedAt := time.UnixMilli(message.TimestampMillis).UTC()
	content := message.Text
	buttons := make([]store.LinkButton, 0)
	attachments := make([]store.MAXHistoryAttachment, 0, len(message.Attachments))
	roundTrip := maxHistoryTextRoundTripComplete(message.Raw)
	for _, attachment := range message.Attachments {
		if !attachment.Complete {
			roundTrip = false
		}
		for _, button := range attachment.LinkButtons {
			buttons = append(buttons, store.LinkButton{Text: button.Text, URL: button.URL})
		}
		switch attachment.Type {
		case string(maxclient.MediaTypeImage), string(maxclient.MediaTypeVideo):
			if strings.TrimSpace(attachment.Token) == "" || strings.TrimSpace(attachment.URL) == "" {
				roundTrip = false
			}
			attachments = append(attachments, store.MAXHistoryAttachment{
				Type: attachment.Type, ProviderToken: strings.TrimSpace(attachment.Token),
				RemoteURL: strings.TrimSpace(attachment.URL), Width: attachment.Width,
				Height: attachment.Height, DurationMS: attachment.DurationMS,
				ProviderMeta: json.RawMessage(`{}`),
			})
		case "inline_keyboard":
			// Link buttons are persisted on the post rather than as media.
		default:
			roundTrip = false
		}
	}
	if err := store.ValidateLinkButtonsForPublish(buttons); err != nil {
		roundTrip = false
	}
	mediaLimit := store.MaxPostAttachments
	if len(buttons) > 0 {
		mediaLimit = store.MaxPostAttachmentsWithKeyboard
	}
	if len(attachments) > mediaLimit {
		roundTrip = false
	}
	// Never expose a normalized subset as directly editable: an edit would
	// otherwise erase unsupported MAX attachments or keyboard controls.
	if !roundTrip {
		attachments = nil
		buttons = nil
		if strings.TrimSpace(content) == "" {
			content = maxHistoryReferenceContent(strings.TrimSpace(message.URL))
		}
	}

	var senderIsBot *bool
	if senderID := strings.TrimSpace(message.SenderUserID); senderID != "" {
		matches := message.SenderIsBot && senderID == strconv.FormatInt(currentBotUserID, 10)
		senderIsBot = &matches
	}
	return store.MAXHistoryItem{
		Title: maxHistoryTitle(content, publishedAt), Content: content,
		URL: strings.TrimSpace(message.URL), MessageID: messageID, PublishedAt: publishedAt,
		Views: message.Views, SenderIsBot: senderIsBot, RoundTrip: roundTrip,
		// The provider payload may contain contact, callback or other unrelated
		// secrets. Normalized fields above are sufficient for the product, so the
		// durable mapping deliberately stores no raw provider body.
		LinkButtons: buttons, Raw: json.RawMessage(`{}`), Attachments: attachments,
	}, nil
}

func maxHistoryTextRoundTripComplete(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var message struct {
		Body json.RawMessage `json:"body"`
		Link json.RawMessage `json:"link"`
	}
	if err := json.Unmarshal(raw, &message); err != nil || len(message.Body) == 0 || string(message.Body) == "null" {
		return false
	}
	if len(message.Link) > 0 && string(message.Link) != "null" {
		// Reply/forward metadata is not represented by the edit request. Treat
		// any linked message as read-only so an edit cannot silently discard it.
		return false
	}
	var body struct {
		Markup json.RawMessage `json:"markup"`
	}
	if err := json.Unmarshal(message.Body, &body); err != nil {
		return false
	}
	if len(body.Markup) == 0 || string(body.Markup) == "null" {
		return true
	}
	var markup []json.RawMessage
	if err := json.Unmarshal(body.Markup, &markup); err != nil {
		// Unknown markup must never be normalized into a directly editable post.
		return false
	}
	return len(markup) == 0
}

func maxHistoryReferenceContent(messageURL string) string {
	if messageURL == "" {
		return "Публикация содержит вложение, которое можно посмотреть в MAX."
	}
	return "Публикация содержит вложение, которое можно посмотреть в MAX: " + messageURL
}

func maxHistoryTitle(content string, publishedAt time.Time) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			continue
		}
		if utf8.RuneCountInString(line) <= maxHistoryTitleRunes {
			return line
		}
		runes := []rune(line)
		return strings.TrimSpace(string(runes[:maxHistoryTitleRunes-1])) + "…"
	}
	return "Публикация из MAX · " + publishedAt.Format("02.01.2006")
}

func maxHistoryFailureCode(err error) string {
	var accessErr *ChannelAccessError
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "request_interrupted"
	case errors.As(err, &accessErr):
		return "channel_access_denied"
	case errors.Is(err, store.ErrConflict):
		return "state_conflict"
	default:
		return "history_import_failed"
	}
}

func requireMAXHistoryAuthoredByCurrentBot(post store.Post) error {
	if post.Origin != store.PostOriginMAXHistory {
		return nil
	}
	if post.MAXSenderIsBot == nil || !*post.MAXSenderIsBot {
		return fmt.Errorf("%w: imported publication was not authored by the connected MaxPosty bot; create an editable copy instead", ErrConflict)
	}
	return nil
}

func requireMAXHistoryEditable(post store.Post) error {
	if err := requireMAXHistoryAuthoredByCurrentBot(post); err != nil {
		return err
	}
	if post.Origin == store.PostOriginMAXHistory && !post.MAXHistoryAttachmentsComplete {
		return fmt.Errorf("%w: imported publication contains MAX attachments that cannot be edited safely; create an editable copy instead", ErrConflict)
	}
	return nil
}

func requireMAXHistoryCurrentBotMessage(
	post store.Post,
	message maxclient.Message,
	currentBotUserID int64,
) error {
	if post.Origin != store.PostOriginMAXHistory {
		return nil
	}
	if currentBotUserID <= 0 || !message.SenderIsBot ||
		message.SenderUserID != strconv.FormatInt(currentBotUserID, 10) {
		return fmt.Errorf("%w: imported publication was not authored by the currently connected MaxPosty bot; create an editable copy instead", ErrConflict)
	}
	return nil
}
