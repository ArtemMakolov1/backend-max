package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/store"
)

// Optional capability keeps publication-only MAX test doubles narrow.
type maxCommentsClient interface {
	GetComments(context.Context, string, maxclient.CommentsQuery) (maxclient.CommentsPage, error)
	GetComment(context.Context, string, string) (maxclient.CommentMessage, error)
	SendComment(context.Context, string, maxclient.CommentRequest) (maxclient.CommentMessage, error)
	EditComment(context.Context, string, string, maxclient.CommentRequest) error
	DeleteComment(context.Context, string, string) error
}

type MAXCommentCapabilities struct {
	CanRead                bool   `json:"can_read"`
	CanReply               bool   `json:"can_reply"`
	CanDelete              bool   `json:"can_delete"`
	ReadOnlyReason         string `json:"read_only_reason,omitempty"`
	ReplyUnavailableReason string `json:"reply_unavailable_reason,omitempty"`
}
type MAXCommentView struct {
	store.MAXPostComment
	IsOwn     bool `json:"is_own"`
	CanEdit   bool `json:"can_edit"`
	CanDelete bool `json:"can_delete"`
}
type MAXCommentsView struct {
	PostID        int64                       `json:"post_id"`
	RootMessageID string                      `json:"root_message_id"`
	Comments      []MAXCommentView            `json:"comments"`
	Operations    []store.MAXCommentOperation `json:"operations"`
	Capabilities  MAXCommentCapabilities      `json:"capabilities"`
	Coverage      store.MAXCommentCoverage    `json:"coverage"`
	Metrics       store.MAXCommentMetrics     `json:"metrics"`
	Operation     *store.MAXCommentOperation  `json:"operation,omitempty"`
}
type MAXCommentMutation struct {
	ExpectedRootMessageID string           `json:"expected_root_message_id"`
	ClientRequestID       string           `json:"client_request_id"`
	Text                  string           `json:"text"`
	Format                maxclient.Format `json:"format,omitempty"`
	ReplyToMessageID      string           `json:"reply_to_message_id,omitempty"`
	ExpectedVersion       int64            `json:"expected_version,omitempty"`
}

type MAXCommentNotWrittenError struct{ Cause error }

func (e *MAXCommentNotWrittenError) Error() string {
	return "MAX comment was not written: " + e.Cause.Error()
}
func (e *MAXCommentNotWrittenError) Unwrap() error { return e.Cause }

type maxCommentScope struct {
	store.MAXCommentContext
	ProviderActive bool
}

func (a *App) MAXCommentsConfigured() bool {
	if a.max == nil {
		return false
	}
	_, ok := a.max.(maxCommentsClient)
	return ok
}

func (a *App) maxCommentProviderContext(ctx context.Context, actor, workspace string, postID int64, write bool, expectedRoot string) (maxCommentScope, maxCommentsClient, maxclient.Membership, error) {
	stored, err := a.store.GetMAXCommentContext(ctx, actor, workspace, postID, write)
	c := maxCommentScope{MAXCommentContext: stored}
	if err != nil {
		return c, nil, maxclient.Membership{}, err
	}
	if expectedRoot != "" && stored.Post.MAXMessageID != expectedRoot {
		return c, nil, maxclient.Membership{}, store.ErrConflict
	}
	provider, ok := a.max.(maxCommentsClient)
	if !ok || a.max == nil {
		return c, nil, maxclient.Membership{}, ErrMAXNotConfigured
	}
	info, membership, err := a.inspectChannel(ctx, c.Channel)
	if err != nil {
		return c, provider, membership, err
	}
	if info.ChatID != c.Channel.MAXChatID || info.Type != "channel" || !membership.IsBot || membership.UserID <= 0 || !membership.IsAdmin || !membership.HasCommentPermission(maxclient.PermissionReadAllMessages) {
		return c, provider, membership, &ChannelAccessError{Diagnostics: channelDiagnostics(info, membership), Message: "MAX comments require a bot administrator with read_all_messages"}
	}
	c.ProviderActive = stored.Channel.Active && info.Status == "active"
	return c, provider, membership, nil
}

func (a *App) maxCommentView(ctx context.Context, actor, workspace string, postID int64, membership maxclient.Membership, scope maxCommentScope, requestedIDs ...string) (MAXCommentsView, error) {
	now := a.now().UTC()
	bot := strconv.FormatInt(membership.UserID, 10)
	snapshot, err := a.store.GetMAXCommentsSnapshot(ctx, actor, workspace, postID, bot, now, requestedIDs...)
	if err != nil {
		return MAXCommentsView{}, err
	}
	if snapshot.RootMessageID != scope.Post.MAXMessageID || snapshot.ChannelID != scope.Channel.ID {
		return MAXCommentsView{}, store.ErrConflict
	}
	access, err := a.store.ResolveWorkspaceAccess(ctx, actor, workspace)
	if err != nil {
		return MAXCommentsView{}, err
	}
	editor := access.Member.Role == store.WorkspaceRoleOwner || access.Member.Role == store.WorkspaceRoleEditor
	canReply := editor && scope.ProviderActive && membership.IsAdmin && membership.HasCommentPermission(maxclient.PermissionWrite)
	canDelete := editor && membership.IsAdmin && membership.HasCommentPermission(maxclient.PermissionDelete)
	capabilities := MAXCommentCapabilities{CanRead: true, CanReply: canReply, CanDelete: canDelete}
	if !editor {
		capabilities.ReadOnlyReason = "workspace_read_only"
		capabilities.ReplyUnavailableReason = "workspace_read_only"
	} else if !scope.ProviderActive {
		capabilities.ReplyUnavailableReason = "channel_not_active"
	} else if !canReply {
		capabilities.ReplyUnavailableReason = "missing_write_permission"
	}
	result := MAXCommentsView{PostID: postID, RootMessageID: snapshot.RootMessageID, Comments: []MAXCommentView{}, Operations: snapshot.Operations, Capabilities: capabilities, Coverage: snapshot.Coverage, Metrics: snapshot.Metrics}
	for _, comment := range snapshot.Comments {
		own := comment.SenderIsBot && comment.SenderUserID == bot
		age := now.Sub(comment.CreatedAt)
		editable := editor && own && comment.TextEditable && comment.DeletedAt == nil && age >= 0 && age < 26*time.Hour
		result.Comments = append(result.Comments, MAXCommentView{MAXPostComment: comment, IsOwn: own, CanEdit: editable, CanDelete: canDelete && comment.DeletedAt == nil})
	}
	return result, nil
}

func (a *App) GetMAXComments(ctx context.Context, actor, workspace string, postID int64, expectedRoot string, requestedIDs ...string) (MAXCommentsView, error) {
	if len(requestedIDs) > 20 {
		return MAXCommentsView{}, store.ErrMAXCommentValidation
	}
	for _, id := range requestedIDs {
		if !store.ValidMAXCommentRequestID(id) {
			return MAXCommentsView{}, store.ErrMAXCommentValidation
		}
	}
	c, _, membership, err := a.maxCommentProviderContext(ctx, actor, workspace, postID, false, expectedRoot)
	if err != nil {
		return MAXCommentsView{}, err
	}
	return a.maxCommentView(ctx, actor, workspace, postID, membership, c, requestedIDs...)
}

func (a *App) SyncMAXComments(ctx context.Context, actor, workspace string, postID int64, expectedRoot string) (MAXCommentsView, error) {
	if !store.ValidMAXCommentID(expectedRoot) {
		return MAXCommentsView{}, store.ErrMAXCommentValidation
	}
	c, provider, membership, err := a.maxCommentProviderContext(ctx, actor, workspace, postID, false, expectedRoot)
	if err != nil {
		return MAXCommentsView{}, err
	}
	claim, err := a.store.ClaimMAXCommentSync(ctx, actor, workspace, postID, a.now().UTC())
	if err != nil {
		return MAXCommentsView{}, err
	}
	release := func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.store.ReleaseMAXCommentSync(releaseCtx, workspace, claim)
	}
	if claim.Post.MAXMessageID != c.Post.MAXMessageID || claim.Channel.MAXChatID != c.Channel.MAXChatID {
		release()
		return MAXCommentsView{}, store.ErrConflict
	}
	page, err := provider.GetComments(ctx, c.Post.MAXMessageID, maxclient.CommentsQuery{Count: store.MAXCommentsPageLimit})
	if err != nil {
		release()
		return MAXCommentsView{}, err
	}
	items := make([]store.MAXPostComment, 0, len(page.Messages))
	for _, message := range page.Messages {
		item, e := normalizeMAXComment(message, c.MAXCommentContext, a.now().UTC(), claim.ClaimedAt)
		if e != nil {
			release()
			return MAXCommentsView{}, e
		}
		items = append(items, item)
	}
	if err = a.store.ApplyMAXCommentSync(ctx, actor, workspace, claim, items, a.now().UTC()); err != nil {
		release()
		return MAXCommentsView{}, err
	}
	// Only an exact known comment ID can reconcile an ambiguous edit. MAX
	// explicitly has no outgoing-action webhooks; text equality cannot identify
	// an ambiguous SEND, and a failed GET must not manufacture a deletion.
	if err = a.reconcileMAXCommentEdits(ctx, actor, workspace, c.MAXCommentContext, provider); err != nil {
		return MAXCommentsView{}, err
	}
	return a.maxCommentView(ctx, actor, workspace, postID, membership, c)
}

func normalizeMAXComment(message maxclient.CommentMessage, c store.MAXCommentContext, now, observed time.Time) (store.MAXPostComment, error) {
	if message.ChatID != c.Channel.MAXChatID || message.PostID != "" && message.PostID != c.Post.MAXMessageID || message.TimestampMillis <= 0 {
		return store.MAXPostComment{}, fmt.Errorf("%w: MAX comment belongs to another publication", store.ErrConflict)
	}
	created := time.UnixMilli(message.TimestampMillis).UTC()
	if created.After(now.Add(5 * time.Minute)) {
		return store.MAXPostComment{}, fmt.Errorf("%w: invalid provider timestamp", store.ErrMAXCommentValidation)
	}
	name := message.SenderName
	if name == "" {
		name = message.SenderUsername
	}
	return store.MAXPostComment{MessageID: message.MessageID, Text: message.Text, SenderUserID: message.SenderUserID, SenderName: name, SenderIsBot: message.SenderIsBot, TextEditable: maxCommentPlainText(message.Raw), ReplyToMessageID: message.ReplyTo, CreatedAt: created, UpdatedAt: created, ObservedAt: observed}, nil
}

func maxCommentPlainText(raw json.RawMessage) bool {
	// An edit of a rendered subset could discard existing markup. Preserve
	// formatted provider comments as readable until lossless editing exists.
	var message struct {
		Body *struct {
			Markup json.RawMessage `json:"markup"`
		} `json:"body"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &message) != nil || message.Body == nil {
		return false
	}
	markup := message.Body.Markup
	if len(markup) == 0 || string(markup) == "null" {
		return true
	}
	var entities []json.RawMessage
	return json.Unmarshal(markup, &entities) == nil && len(entities) == 0
}

func validateMAXCommentMutation(kind, messageID string, r MAXCommentMutation) error {
	if kind != "send" && kind != "edit" && kind != "delete" {
		return store.ErrMAXCommentValidation
	}
	if !store.ValidMAXCommentRequestID(r.ClientRequestID) || !store.ValidMAXCommentID(r.ExpectedRootMessageID) {
		return fmt.Errorf("%w: client_request_id and publication root are required", store.ErrMAXCommentValidation)
	}
	if kind != "send" && (!store.ValidMAXCommentID(messageID) || r.ExpectedVersion <= 0) {
		return fmt.Errorf("%w: current comment version is required", store.ErrMAXCommentValidation)
	}
	if kind == "delete" {
		if r.Text != "" || r.Format != "" || r.ReplyToMessageID != "" {
			return store.ErrMAXCommentValidation
		}
		return nil
	}
	if !utf8.ValidString(r.Text) || strings.TrimSpace(r.Text) == "" || utf8.RuneCountInString(r.Text) > 4000 || r.Format != "" && r.Format != maxclient.FormatMarkdown && r.Format != maxclient.FormatHTML || r.ReplyToMessageID != "" && !store.ValidMAXCommentID(r.ReplyToMessageID) {
		return store.ErrMAXCommentValidation
	}
	if kind == "edit" && r.ReplyToMessageID != "" {
		return fmt.Errorf("%w: reply association cannot be changed", store.ErrMAXCommentValidation)
	}
	return nil
}

func maxCommentRequestHash(root, kind, messageID string, r MAXCommentMutation) string {
	body, _ := json.Marshal(struct {
		Root, Kind, Message, Text, Format, Reply string
		Version                                  int64
	}{root, kind, messageID, r.Text, string(r.Format), r.ReplyToMessageID, r.ExpectedVersion})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func (a *App) MutateMAXComment(ctx context.Context, actor, workspace string, postID int64, kind, messageID string, r MAXCommentMutation) (view MAXCommentsView, returnErr error) {
	mayHaveWritten := false
	if store.ValidMAXCommentRequestID(r.ClientRequestID) {
		written, lookupErr := a.store.MAXCommentRequestWasWritten(ctx, workspace, postID, r.ClientRequestID)
		if lookupErr != nil {
			return MAXCommentsView{}, store.ErrMAXCommentUncertain
		}
		mayHaveWritten = written
	}
	defer func() {
		if returnErr == nil {
			return
		}
		if store.ValidMAXCommentRequestID(r.ClientRequestID) {
			phaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			written, lookupErr := a.store.MAXCommentRequestWasWritten(phaseCtx, workspace, postID, r.ClientRequestID)
			cancel()
			if lookupErr != nil || written {
				mayHaveWritten = true
			}
		}
		if mayHaveWritten {
			returnErr = store.ErrMAXCommentUncertain
			return
		}
		if errors.Is(returnErr, store.ErrMAXCommentUncertain) || errors.Is(returnErr, store.ErrMAXCommentBusy) {
			return
		}
		returnErr = &MAXCommentNotWrittenError{Cause: returnErr}
	}()
	if err := validateMAXCommentMutation(kind, messageID, r); err != nil {
		return MAXCommentsView{}, err
	}
	c, provider, membership, err := a.maxCommentProviderContext(ctx, actor, workspace, postID, true, r.ExpectedRootMessageID)
	if err != nil {
		return MAXCommentsView{}, err
	}
	required := maxclient.PermissionWrite
	if kind == "delete" {
		required = maxclient.PermissionDelete
	}
	if kind != "edit" && !membership.HasCommentPermission(required) {
		return MAXCommentsView{}, &ChannelAccessError{Message: "MAX comment operation requires an explicit administrator permission"}
	}
	if kind == "send" && !c.ProviderActive {
		return MAXCommentsView{}, store.ErrMAXCommentsUnavailable
	}
	request := store.MAXCommentWriteRequest{ClientRequestID: r.ClientRequestID, RequestHash: maxCommentRequestHash(c.Post.MAXMessageID, kind, messageID, r), Kind: kind, MessageID: messageID, ExpectedVersion: r.ExpectedVersion, Text: r.Text, Format: string(r.Format), ReplyToMessageID: r.ReplyToMessageID, BotUserID: strconv.FormatInt(membership.UserID, 10)}
	// Consult an existing permanent request key before fetching a possibly
	// removed/edited target, so a successful duplicate returns its known result.
	existing, found, err := a.store.FindMAXCommentOperation(ctx, actor, workspace, postID, r.ClientRequestID)
	if err != nil {
		return MAXCommentsView{}, err
	}
	if found {
		mayHaveWritten = mayHaveWritten || existing.State != "rejected"
		if existing.RequestHash != request.RequestHash {
			return MAXCommentsView{}, store.ErrConflict
		}
		if err = store.MAXCommentStateError(existing); err != nil {
			return MAXCommentsView{}, err
		}
		view, e := a.maxCommentView(ctx, actor, workspace, postID, membership, c)
		view.Operation = &existing
		return view, e
	}
	providerRequest := maxclient.CommentRequest{Text: r.Text, Format: r.Format, ReplyTo: r.ReplyToMessageID}
	if kind != "send" || r.ReplyToMessageID != "" {
		target := messageID
		if kind == "send" {
			target = r.ReplyToMessageID
		}
		fresh, e := provider.GetComment(ctx, c.Post.MAXMessageID, target)
		if e != nil {
			return MAXCommentsView{}, e
		}
		item, e := normalizeMAXComment(fresh, c.MAXCommentContext, a.now().UTC(), a.now().UTC())
		if e != nil {
			return MAXCommentsView{}, e
		}
		if item.MessageID != target {
			return MAXCommentsView{}, store.ErrConflict
		}
		if kind == "edit" {
			age := a.now().UTC().Sub(item.CreatedAt)
			if !item.SenderIsBot || item.SenderUserID != strconv.FormatInt(membership.UserID, 10) || !item.TextEditable || age < 0 || age >= 26*time.Hour {
				return MAXCommentsView{}, fmt.Errorf("%w: only this bot's plain comments younger than 26 hours can be edited", store.ErrConflict)
			}
			providerRequest.ReplyTo = item.ReplyToMessageID
		}
		if e = a.store.ObserveMAXComment(ctx, actor, workspace, postID, c.Post.MAXMessageID, item); e != nil {
			return MAXCommentsView{}, e
		}
	}
	operation, isNew, err := a.store.ClaimMAXCommentOperation(ctx, actor, workspace, postID, c.Post.MAXMessageID, request, a.now().UTC())
	if !isNew && operation.OperationID != "" && operation.State != "rejected" {
		mayHaveWritten = true
	}
	if err != nil {
		return MAXCommentsView{}, err
	}
	if !isNew {
		if err = store.MAXCommentStateError(operation); err != nil {
			return MAXCommentsView{}, err
		}
		view, e := a.maxCommentView(ctx, actor, workspace, postID, membership, c)
		view.Operation = &operation
		return view, e
	}
	finish := func(state string, item *store.MAXPostComment) error {
		finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return a.store.FinishMAXCommentOperation(finishCtx, operation.OperationID, state, item, a.now().UTC())
	}
	// Recheck authority immediately before the durable begin-write marker. A
	// member or channel change while waiting cannot authorize a stale call.
	latest, _, latestMembership, e := a.maxCommentProviderContext(ctx, actor, workspace, postID, true, r.ExpectedRootMessageID)
	if e != nil || latest.Post.MAXMessageID != c.Post.MAXMessageID || latest.Channel.MAXChatID != c.Channel.MAXChatID || latestMembership.UserID != membership.UserID || (kind != "edit" && !latestMembership.HasCommentPermission(required)) || (kind == "send" && !latest.ProviderActive) {
		if e == nil {
			e = store.ErrConflict
		}
		return MAXCommentsView{}, errors.Join(e, finish("rejected", nil))
	}
	if err = a.store.BeginMAXCommentWrite(ctx, actor, workspace, postID, c.Post.MAXMessageID, operation.OperationID, a.now().UTC()); err != nil {
		return MAXCommentsView{}, errors.Join(err, finish("rejected", nil))
	}
	mayHaveWritten = true
	var result *store.MAXPostComment
	writeAccepted := false
	switch kind {
	case "send":
		var message maxclient.CommentMessage
		message, err = provider.SendComment(ctx, c.Post.MAXMessageID, providerRequest)
		if err == nil {
			writeAccepted = true
			var item store.MAXPostComment
			item, err = normalizeMAXComment(message, c.MAXCommentContext, a.now().UTC(), a.now().UTC())
			if err == nil {
				result = &item
				if item.SenderUserID != "" && (item.SenderUserID != strconv.FormatInt(membership.UserID, 10) || !item.SenderIsBot) {
					err = store.ErrConflict
				}
			}
		}
	case "edit":
		err = provider.EditComment(ctx, c.Post.MAXMessageID, messageID, providerRequest)
		if err == nil {
			writeAccepted = true
			var message maxclient.CommentMessage
			message, err = provider.GetComment(ctx, c.Post.MAXMessageID, messageID)
			if err == nil {
				var item store.MAXPostComment
				item, err = normalizeMAXComment(message, c.MAXCommentContext, a.now().UTC(), a.now().UTC())
				if err == nil {
					item.UpdatedAt = a.now().UTC()
					result = &item
				}
			}
		}
	case "delete":
		err = provider.DeleteComment(ctx, c.Post.MAXMessageID, messageID)
	}
	if err != nil {
		state := "uncertain"
		if !writeAccepted && maxCommentDefinitiveRejection(err) {
			state = "rejected"
			mayHaveWritten = false
		}
		if e = finish(state, result); e != nil {
			return MAXCommentsView{}, errors.Join(err, e)
		}
		if state == "uncertain" {
			return MAXCommentsView{}, store.ErrMAXCommentUncertain
		}
		return MAXCommentsView{}, err
	}
	if err = finish("succeeded", result); err != nil {
		_ = finish("uncertain", nil)
		return MAXCommentsView{}, errors.Join(store.ErrMAXCommentUncertain, err)
	}
	viewCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	view, err = a.maxCommentView(viewCtx, actor, workspace, postID, membership, c)
	operation.State = "succeeded"
	if result != nil {
		operation.MessageID = result.MessageID
	}
	view.Operation = &operation
	return view, err
}

func maxCommentDefinitiveRejection(err error) bool {
	var upstream *maxclient.Error
	if !errors.As(err, &upstream) {
		return false
	}
	switch upstream.StatusCode {
	case http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUnprocessableEntity, http.StatusTooManyRequests:
		return true
	}
	return false
}

func (a *App) reconcileMAXCommentEdits(ctx context.Context, actor, workspace string, c store.MAXCommentContext, provider maxCommentsClient) error {
	snapshot, err := a.store.GetMAXCommentsSnapshot(ctx, actor, workspace, c.Post.ID, "", a.now().UTC())
	if err != nil {
		return err
	}
	if snapshot.RootMessageID != c.Post.MAXMessageID || snapshot.ChannelID != c.Channel.ID {
		return store.ErrConflict
	}
	checked := 0
	for _, operation := range snapshot.Operations {
		if operation.State != "uncertain" || operation.Kind != "edit" || operation.DesiredFormat != "" {
			continue
		}
		if checked >= 5 {
			break
		}
		checked++
		fresh, e := provider.GetComment(ctx, c.Post.MAXMessageID, operation.MessageID)
		if e != nil {
			continue
		}
		item, e := normalizeMAXComment(fresh, c, a.now().UTC(), a.now().UTC())
		if e != nil || item.Text != operation.DesiredText || !item.TextEditable || item.SenderUserID != operation.BotUserID || !item.SenderIsBot {
			continue
		}
		finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		e = a.store.FinishMAXCommentOperation(finishCtx, operation.OperationID, "succeeded", &item, a.now().UTC())
		cancel()
		if e != nil {
			return e
		}
	}
	return nil
}

// Only authenticated MAX webhook handlers call this, never browser clients.
func (a *App) ObserveMAXCommentMessage(ctx context.Context, message maxclient.CommentMessage, eventAt time.Time) error {
	if message.PostID != "" {
		return a.store.ObserveMAXCommentForRoot(ctx, message.ChatID, message.PostID, store.MAXPostComment{MessageID: message.MessageID, Text: message.Text, SenderUserID: message.SenderUserID, SenderName: message.SenderName, SenderIsBot: message.SenderIsBot, TextEditable: maxCommentPlainText(message.Raw), ReplyToMessageID: message.ReplyTo, CreatedAt: time.UnixMilli(message.TimestampMillis).UTC(), UpdatedAt: eventAt, ObservedAt: eventAt})
	}
	return a.store.ObserveKnownMAXCommentEvent(ctx, message.ChatID, store.MAXPostComment{MessageID: message.MessageID, Text: message.Text, SenderUserID: message.SenderUserID, SenderName: message.SenderName, SenderIsBot: message.SenderIsBot, TextEditable: maxCommentPlainText(message.Raw), ReplyToMessageID: message.ReplyTo, CreatedAt: time.UnixMilli(message.TimestampMillis).UTC(), UpdatedAt: eventAt, ObservedAt: eventAt})
}

// A person explicitly identifies the received response. This is a read-only
// provider lookup and a local link, never a send/retry or heuristic auto-match.
func (a *App) ReconcileMAXCommentSend(ctx context.Context, actor, workspace string, postID int64, expectedRoot, operationID, messageID string) (MAXCommentsView, error) {
	if !store.ValidMAXCommentID(messageID) || !store.ValidMAXCommentID(expectedRoot) {
		return MAXCommentsView{}, store.ErrMAXCommentValidation
	}
	c, provider, membership, err := a.maxCommentProviderContext(ctx, actor, workspace, postID, true, expectedRoot)
	if err != nil {
		return MAXCommentsView{}, err
	}
	operation, err := a.store.GetMAXCommentOperation(ctx, actor, workspace, postID, operationID)
	if err != nil {
		return MAXCommentsView{}, err
	}
	if operation.State != "uncertain" || operation.Kind != "send" || operation.DesiredFormat != "" || operation.BotUserID != strconv.FormatInt(membership.UserID, 10) {
		return MAXCommentsView{}, store.ErrConflict
	}
	message, err := provider.GetComment(ctx, c.Post.MAXMessageID, messageID)
	if err != nil {
		return MAXCommentsView{}, err
	}
	item, err := normalizeMAXComment(message, c.MAXCommentContext, a.now().UTC(), a.now().UTC())
	if err != nil {
		return MAXCommentsView{}, err
	}
	// Provider creation may differ by a few seconds from the VPS clock. This
	// bound admits clock skew while excluding older identical comments.
	if !item.SenderIsBot || item.SenderUserID != operation.BotUserID || item.Text != operation.DesiredText || item.ReplyToMessageID != operation.DesiredReplyTo || item.CreatedAt.Before(operation.ClaimedAt.Add(-5*time.Second)) {
		return MAXCommentsView{}, store.ErrConflict
	}
	if err = a.store.ReconcileMAXCommentOperation(ctx, actor, workspace, postID, c.Post.MAXMessageID, operation.OperationID, &item, a.now().UTC()); err != nil {
		return MAXCommentsView{}, err
	}
	view, err := a.maxCommentView(ctx, actor, workspace, postID, membership, c)
	operation.State = "succeeded"
	operation.MessageID = item.MessageID
	view.Operation = &operation
	return view, err
}
