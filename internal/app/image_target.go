package app

import (
	"context"
	"fmt"
	"strings"

	"maxpilot/backend/internal/openaiimg"
	"maxpilot/backend/internal/store"
)

// These hooks commit usage only after loading the authoritative target and
// rejecting its publishing state. Once the hook succeeds, generation starts
// without a second target read that could reject a concurrent publishing claim
// before any AI call. The final attachment CAS still protects concurrent edits;
// a conflict after generation does not refund AI work already performed.
func (a *App) GeneratePostImageWithBeforeGenerate(
	ctx context.Context, userID string, postID int64, request openaiimg.GenerateRequest, beforeGenerate func() error,
) (store.Post, error) {
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
	if beforeGenerate != nil {
		if err := beforeGenerate(); err != nil {
			return store.Post{}, err
		}
	}
	file, err := a.GenerateImageForUser(ctx, userID, request)
	if err != nil {
		return store.Post{}, err
	}
	return a.store.ReplaceFirstImageAttachmentAndPromptIfUnchanged(ctx, post, attachmentFromImage(file), request.Prompt)
}

func (a *App) GeneratePostImageForWorkspaceWithBeforeGenerate(
	ctx context.Context, actorUserID, workspaceID string, postID int64, request openaiimg.GenerateRequest, beforeGenerate func() error,
) (store.Post, error) {
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
	if beforeGenerate != nil {
		if err := beforeGenerate(); err != nil {
			return store.Post{}, err
		}
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
	return updated, err
}
