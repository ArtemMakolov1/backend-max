package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/store"
)

type maxVideoClient interface {
	GetVideo(context.Context, string) (maxclient.VideoInfo, error)
}

type MAXVideoPreview struct {
	AttachmentID int64                `json:"attachment_id"`
	Available    bool                 `json:"available"`
	URLs         *maxclient.VideoURLs `json:"urls"`
	ThumbnailURL string               `json:"thumbnail_url,omitempty"`
	Width        *int                 `json:"width,omitempty"`
	Height       *int                 `json:"height,omitempty"`
	DurationMS   *int64               `json:"duration_ms,omitempty"`
}

func (a *App) GetMAXVideoPreviewForWorkspace(ctx context.Context, actorID, workspaceID string, postID, attachmentID int64) (MAXVideoPreview, error) {
	access, err := a.store.ResolveWorkspaceAccess(ctx, actorID, workspaceID)
	if err != nil {
		return MAXVideoPreview{}, err
	}
	if !AccessContextForWorkspace(access.Workspace, actorID, access.Member.Role).Can(CapabilityPostsRead) {
		return MAXVideoPreview{}, store.ErrNotFound
	}
	post, err := a.store.GetPostForWorkspace(ctx, actorID, workspaceID, postID)
	if err != nil {
		return MAXVideoPreview{}, err
	}
	attachment, err := maxVideoAttachment(post, attachmentID)
	if err != nil {
		return MAXVideoPreview{}, err
	}
	client, ok := a.max.(maxVideoClient)
	if !ok {
		return MAXVideoPreview{}, ErrMAXNotConfigured
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	channel, err := a.store.GetChannelForWorkspace(ctx, actorID, workspaceID, *post.ChannelID)
	if err != nil {
		return MAXVideoPreview{}, err
	}
	info, membership, err := a.inspectChannel(ctx, channel)
	if err != nil {
		return MAXVideoPreview{}, err
	}
	if err := validateChannelParticipantInfo(channel, info); err != nil {
		return MAXVideoPreview{}, err
	}
	if membership.UserID <= 0 || !membership.IsBot || !membership.IsAdmin || !membership.HasPermission(maxclient.PermissionReadAllMessages) {
		return MAXVideoPreview{}, &ChannelAccessError{Diagnostics: channelDiagnostics(info, membership), Message: "MAX video preview requires a bot administrator with read_all_messages permission"}
	}
	video, err := client.GetVideo(ctx, attachment.ProviderToken)
	if err != nil {
		return MAXVideoPreview{}, err
	}
	if video.Token != attachment.ProviderToken || video.Width < 0 || video.Height < 0 || video.DurationMS < 0 {
		return MAXVideoPreview{}, errors.New("MAX video metadata response is inconsistent")
	}
	// A membership revocation, republish or attachment replacement during the
	// provider request must not return URLs from the previous tenant snapshot.
	current, err := a.store.GetPostForWorkspace(ctx, actorID, workspaceID, postID)
	if err != nil {
		return MAXVideoPreview{}, err
	}
	currentAttachment, err := maxVideoAttachment(current, attachmentID)
	if err != nil {
		return MAXVideoPreview{}, err
	}
	if current.MAXMessageID != post.MAXMessageID || *current.ChannelID != *post.ChannelID ||
		currentAttachment.Source != attachment.Source || currentAttachment.ProviderToken != attachment.ProviderToken {
		return MAXVideoPreview{}, store.ErrConflict
	}
	urls := maxclient.SafeVideoURLs(video.URLs)
	return MAXVideoPreview{AttachmentID: attachmentID, Available: urls != nil, URLs: urls,
		ThumbnailURL: maxclient.SafeAssetURL(video.ThumbnailURL), Width: &video.Width,
		Height: &video.Height, DurationMS: &video.DurationMS}, nil
}

func maxVideoAttachment(post store.Post, attachmentID int64) (store.PostAttachment, error) {
	if post.Status != store.PostStatusPublished || strings.TrimSpace(post.MAXMessageID) == "" || post.ChannelID == nil {
		return store.PostAttachment{}, store.ErrNotFound
	}
	var result store.PostAttachment
	for _, attachment := range post.Attachments {
		if attachment.ID != attachmentID {
			continue
		}
		if result.ID != 0 {
			return store.PostAttachment{}, store.ErrConflict
		}
		result = attachment
	}
	if result.ID <= 0 || result.Type != store.PostAttachmentVideo || strings.TrimSpace(result.ProviderToken) == "" ||
		(result.Source != store.PostAttachmentSourceMAXHistory && result.Source != store.PostAttachmentSourceUpload) {
		return store.PostAttachment{}, store.ErrNotFound
	}
	return result, nil
}
