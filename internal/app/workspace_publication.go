package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"maxpilot/backend/internal/store"
)

func (a *App) UpdatePublishedPostForWorkspace(ctx context.Context, actorUserID, workspaceID string, postID int64) (store.Post, error) {
	if _, _, err := a.publishedPostForWorkspace(ctx, actorUserID, workspaceID, postID); err != nil {
		return store.Post{}, err
	}
	post, err := a.UpdatePublishedPost(ctx, postID)
	if err == nil {
		a.recordWorkspacePublicationEvent(actorUserID, workspaceID, "post.publication_updated", postID)
	}
	return post, err
}

func (a *App) DeletePublicationForWorkspace(ctx context.Context, actorUserID, workspaceID string, postID int64) (result store.Post, resultErr error) {
	startedAt := time.Now()
	defer func() {
		a.metrics.ObservePublicationOperation("delete", metricOutcome(resultErr), time.Since(startedAt))
	}()
	post, err := a.store.GetPostForWorkspace(ctx, actorUserID, workspaceID, postID)
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
	channel, err := a.store.GetChannelForWorkspace(ctx, actorUserID, workspaceID, *post.ChannelID)
	if err != nil {
		return store.Post{}, err
	}
	info, membership, err := a.inspectChannel(ctx, channel)
	if err != nil {
		return store.Post{}, err
	}
	if diagnostics := channelDiagnostics(info, membership); !diagnostics.CanDelete {
		return store.Post{}, &ChannelAccessError{Diagnostics: diagnostics, Message: "MAX delete permission is required"}
	}
	clear := func() (store.Post, error) {
		return a.store.ClearPublicationForUser(ctx, post.UserID, post.ID, channel.ID, post.MAXMessageID)
	}
	if err := a.max.Delete(ctx, post.MAXMessageID); err != nil {
		if isMAXMessageNotFound(err) {
			result, resultErr = clear()
		} else if isMAXOperationFailed(err) {
			if _, getErr := a.max.GetMessage(ctx, post.MAXMessageID); isMAXMessageNotFound(getErr) {
				result, resultErr = clear()
			} else {
				return store.Post{}, err
			}
		} else {
			return store.Post{}, err
		}
	} else {
		result, resultErr = clear()
	}
	if resultErr == nil {
		a.recordWorkspacePublicationEvent(actorUserID, workspaceID, "post.publication_deleted", postID)
	}
	return result, resultErr
}

func (a *App) SyncMAXPublicationForWorkspace(ctx context.Context, actorUserID, workspaceID string, postID int64) (store.Post, error) {
	current, err := a.store.GetPostForWorkspace(ctx, actorUserID, workspaceID, postID)
	if err != nil {
		return store.Post{}, err
	}
	if isStoredMAXPublicationMissing(current) {
		return current, nil
	}
	post, channel, err := a.publishedPostForWorkspace(ctx, actorUserID, workspaceID, postID)
	if err != nil {
		return store.Post{}, err
	}
	if a.max == nil {
		return store.Post{}, ErrMAXNotConfigured
	}
	now := a.now().UTC()
	claimed, err := a.store.ClaimPostStatsAttemptForUser(
		ctx, post.UserID, post.ID, channel.ID, post.MAXMessageID, now, manualMAXStatsCooldown)
	if err != nil {
		return store.Post{}, err
	}
	if !claimed {
		current, getErr := a.store.GetPostForWorkspace(ctx, actorUserID, workspaceID, post.ID)
		if getErr != nil {
			return store.Post{}, getErr
		}
		if isStoredMAXPublicationMissing(current) {
			return current, nil
		}
		retryAfter := manualMAXStatsCooldown
		if current.MAXStatsAttemptedAt != nil {
			if remaining := current.MAXStatsAttemptedAt.UTC().Add(manualMAXStatsCooldown).Sub(now); remaining > 0 && remaining < retryAfter {
				retryAfter = remaining
			}
		}
		return store.Post{}, &MAXStatsCooldownError{RetryAfter: retryAfter}
	}
	return a.syncClaimedMAXPublication(ctx, post.UserID, post, channel, now)
}

func (a *App) PinPostForWorkspace(ctx context.Context, actorUserID, workspaceID string, postID int64) (result store.Post, resultErr error) {
	startedAt := time.Now()
	defer func() { a.metrics.ObservePublicationOperation("pin", metricOutcome(resultErr), time.Since(startedAt)) }()
	post, channel, err := a.publishedPostForWorkspace(ctx, actorUserID, workspaceID, postID)
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
	result, resultErr = a.store.SetPublicationPinnedForUser(ctx, post.UserID, post.ID, channel.ID, post.MAXMessageID, true)
	if resultErr == nil {
		a.recordWorkspacePublicationEvent(actorUserID, workspaceID, "post.pinned", postID)
	}
	return result, resultErr
}

func (a *App) UnpinPostForWorkspace(ctx context.Context, actorUserID, workspaceID string, postID int64) (result store.Post, resultErr error) {
	startedAt := time.Now()
	defer func() {
		a.metrics.ObservePublicationOperation("unpin", metricOutcome(resultErr), time.Since(startedAt))
	}()
	post, channel, err := a.publishedPostForWorkspace(ctx, actorUserID, workspaceID, postID)
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
	if current != nil {
		if err := validateMAXMessageChannel(*current, channel.MAXChatID); err != nil {
			return store.Post{}, err
		}
		if current.MessageID == post.MAXMessageID {
			if err := a.max.UnpinMessage(ctx, channel.MAXChatID); err != nil {
				return store.Post{}, err
			}
		}
	}
	result, resultErr = a.store.SetPublicationPinnedForUser(ctx, post.UserID, post.ID, channel.ID, post.MAXMessageID, false)
	if resultErr == nil {
		a.recordWorkspacePublicationEvent(actorUserID, workspaceID, "post.unpinned", postID)
	}
	return result, resultErr
}

func (a *App) publishedPostForWorkspace(ctx context.Context, actorUserID, workspaceID string, postID int64) (store.Post, store.Channel, error) {
	post, err := a.store.GetPostForWorkspace(ctx, actorUserID, workspaceID, postID)
	if err != nil {
		return store.Post{}, store.Channel{}, err
	}
	if post.Status != store.PostStatusPublished || strings.TrimSpace(post.MAXMessageID) == "" || post.ChannelID == nil {
		return store.Post{}, store.Channel{}, fmt.Errorf("%w: post has no active MAX publication", ErrConflict)
	}
	channel, err := a.store.GetChannelForWorkspace(ctx, actorUserID, workspaceID, *post.ChannelID)
	if err != nil {
		return store.Post{}, store.Channel{}, err
	}
	if post.UserID != channel.UserID || post.WorkspaceID != channel.WorkspaceID || !channel.Active {
		return store.Post{}, store.Channel{}, fmt.Errorf("%w: publication channel is outside the workspace or inactive", ErrConflict)
	}
	return post, channel, nil
}

func (a *App) recordWorkspacePublicationEvent(actorUserID, workspaceID, action string, postID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := a.store.CreateAuditEvent(ctx, actorUserID, store.AuditEvent{
		WorkspaceID: workspaceID, Action: action, EntityType: "post", EntityID: fmt.Sprint(postID),
	}); err != nil {
		a.logger.Error("could not append workspace publication audit event", "workspace_id", workspaceID,
			"post_id", postID, "action", action, "error", err)
	}
}
