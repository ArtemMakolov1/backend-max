package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

// maxMediaUploader is implemented by the production MAX client. Keeping this
// capability separate from the legacy MAXClient interface preserves the
// image-only test doubles while making video support explicit.
type maxMediaUploader interface {
	UploadMedia(context.Context, maxclient.MediaType, string, io.Reader) (maxclient.MediaToken, error)
}

func attachmentFromImage(file media.File) store.PostAttachment {
	width, height := file.Width, file.Height
	return store.PostAttachment{
		Type:             store.PostAttachmentImage,
		Position:         -1,
		URL:              file.URL,
		StorageKey:       file.Path,
		ProcessingStatus: store.AttachmentStatusReady,
		SizeBytes:        file.Size,
		MIMEType:         file.MIMEType,
		Width:            &width,
		Height:           &height,
	}
}

// postMediaTokens returns the ordered MAX token set for the post. MAX upload
// tokens are reusable and have no documented expiry, so the first successful
// upload is cached with an attachment-object CAS and later text edits do not
// re-upload unchanged media. The S3 object remains the source of truth.
func (a *App) postMediaTokens(ctx context.Context, post store.Post) ([]maxclient.MediaToken, []string, error) {
	if len(post.Attachments) == 0 {
		images, err := a.imageTokens(ctx, post)
		return nil, images, err
	}

	uploader, supportsMixedMedia := a.max.(maxMediaUploader)
	tokens := make([]maxclient.MediaToken, 0, len(post.Attachments))
	legacyImages := make([]string, 0, len(post.Attachments))
	for _, attachment := range post.Attachments {
		if attachment.ProcessingStatus != store.AttachmentStatusReady {
			return nil, nil, fmt.Errorf("attachment %d is not ready", attachment.ID)
		}

		mediaType := maxclient.MediaType(attachment.Type)
		if mediaType != maxclient.MediaTypeImage && mediaType != maxclient.MediaTypeVideo {
			return nil, nil, fmt.Errorf("attachment %d has unsupported type %q", attachment.ID, attachment.Type)
		}
		if mediaType == maxclient.MediaTypeVideo && !supportsMixedMedia {
			return nil, nil, errors.New("the configured MAX client does not support video uploads")
		}
		if cached := strings.TrimSpace(attachment.ProviderToken); cached != "" {
			if supportsMixedMedia {
				tokens = append(tokens, maxclient.MediaToken{Type: mediaType, Token: cached})
			} else {
				legacyImages = append(legacyImages, cached)
			}
			continue
		}

		storageKey := strings.TrimSpace(attachment.StorageKey)
		if storageKey == "" {
			resolved, err := a.media.ResolveURL(ctx, attachment.URL)
			if err != nil {
				return nil, nil, fmt.Errorf("resolve attachment %d: %w", attachment.ID, err)
			}
			storageKey = resolved
		}
		object, err := a.media.Open(ctx, storageKey)
		if err != nil {
			return nil, nil, fmt.Errorf("open attachment %d: %w", attachment.ID, err)
		}

		var token maxclient.MediaToken
		if supportsMixedMedia {
			token, err = uploader.UploadMedia(ctx, mediaType, object.Filename, object.Body)
		} else {
			var legacy maxclient.UploadResult
			legacy, err = a.max.UploadImage(ctx, object.Filename, object.Body)
			token = maxclient.MediaToken{Type: maxclient.MediaTypeImage, Token: legacy.Token}
		}
		closeErr := object.Body.Close()
		if err != nil {
			return nil, nil, err
		}
		if closeErr != nil {
			return nil, nil, fmt.Errorf("close attachment %d: %w", attachment.ID, closeErr)
		}
		if err := a.store.CachePostAttachmentProviderToken(ctx, post.UserID, post.ID, attachment.ID,
			storageKey, attachment.UpdatedAt, token.Token); err != nil {
			return nil, nil, err
		}
		if supportsMixedMedia {
			tokens = append(tokens, token)
		} else {
			legacyImages = append(legacyImages, token.Token)
		}
	}
	if !supportsMixedMedia {
		return nil, legacyImages, nil
	}
	return tokens, nil, nil
}

func validatePostAttachments(post store.Post) error {
	if len(post.Attachments) == 0 {
		return nil
	}
	limit := store.MaxPostAttachments
	if len(post.LinkButtons) > 0 {
		limit = store.MaxPostAttachmentsWithKeyboard
	}
	if len(post.Attachments) > limit {
		return fmt.Errorf("MAX allows no more than %d media attachments for this post", limit)
	}
	for index, attachment := range post.Attachments {
		if attachment.Position != index {
			return errors.New("post attachments have an invalid order; reorder them and try again")
		}
		if attachment.Type != store.PostAttachmentImage && attachment.Type != store.PostAttachmentVideo {
			return fmt.Errorf("attachment %d has unsupported type %q", attachment.ID, attachment.Type)
		}
		if attachment.ProcessingStatus != store.AttachmentStatusReady {
			return fmt.Errorf("attachment %d is still processing", attachment.ID)
		}
		if strings.TrimSpace(attachment.StorageKey) == "" && strings.TrimSpace(attachment.URL) == "" {
			return fmt.Errorf("attachment %d has no stored media", attachment.ID)
		}
	}
	return nil
}
