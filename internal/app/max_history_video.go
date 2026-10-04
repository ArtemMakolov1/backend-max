package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/store"
)

const maxHistoryVideoMetadataBudget = 20

// prepareMAXHistoryVideoMedia resolves only token-only video attachments.
// A per-page cache and bounded prefix avoid unbounded provider fan-out. The
// caller persists the inclusive cursor of that prefix and resumes the rest.
func prepareMAXHistoryVideoMedia(ctx context.Context, provider MAXClient, messages []maxclient.HistoryMessage) ([]maxclient.HistoryMessage, bool, error) {
	client, supported := provider.(maxVideoClient)
	if !supported {
		return messages, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cache := make(map[string]maxclient.VideoInfo)
	prepared := make([]maxclient.HistoryMessage, 0, len(messages))
	for _, original := range messages {
		// MAX's legal gallery fits in the budget on an otherwise empty page.
		// Oversized unsupported galleries remain read-only without fan-out.
		if len(original.Attachments) > store.MaxPostAttachments {
			prepared = append(prepared, original)
			continue
		}
		needed := make(map[string]struct{})
		for _, attachment := range original.Attachments {
			if attachment.Type == "video" && strings.TrimSpace(attachment.Token) != "" && attachment.URL == "" {
				if _, cached := cache[attachment.Token]; !cached {
					needed[attachment.Token] = struct{}{}
				}
			}
		}
		if len(cache)+len(needed) > maxHistoryVideoMetadataBudget {
			return prepared, true, nil
		}
		message := original
		message.Attachments = append([]maxclient.HistoryAttachment(nil), original.Attachments...)
		for index := range message.Attachments {
			attachment := &message.Attachments[index]
			if attachment.Type != "video" || strings.TrimSpace(attachment.Token) == "" || attachment.URL != "" {
				continue
			}
			video, cached := cache[attachment.Token]
			if !cached {
				var err error
				video, err = client.GetVideo(ctx, attachment.Token)
				if err != nil {
					var providerErr *maxclient.Error
					if !errors.As(err, &providerErr) || (providerErr.StatusCode != http.StatusForbidden && providerErr.StatusCode != http.StatusNotFound) {
						return nil, false, err
					}
					video = maxclient.VideoInfo{Token: attachment.Token}
				}
				if video.Token != attachment.Token || video.Width < 0 || video.Height < 0 || video.DurationMS < 0 {
					return nil, false, errors.New("MAX video metadata response is inconsistent")
				}
				cache[attachment.Token] = video
			}
			attachment.URL = preferredMAXVideoURL(maxclient.SafeVideoURLs(video.URLs))
			if attachment.URL == "" {
				attachment.Complete = false
				continue
			}
			if video.Width > 0 {
				value := video.Width
				attachment.Width = &value
			}
			if video.Height > 0 {
				value := video.Height
				attachment.Height = &value
			}
			value := video.DurationMS
			attachment.DurationMS = &value
		}
		prepared = append(prepared, message)
	}
	return prepared, false, nil
}

func preferredMAXVideoURL(urls *maxclient.VideoURLs) string {
	if urls == nil {
		return ""
	}
	// Imported gallery rows use video/mp4. HLS remains available from the
	// metadata endpoint, but cannot be stored as a falsely labelled MP4.
	for _, candidate := range []string{urls.MP4480, urls.MP4720, urls.MP4360, urls.MP41080, urls.MP4240, urls.MP4144} {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}
