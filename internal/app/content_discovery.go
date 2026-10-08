package app

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"maxpilot/backend/internal/openairesearch"
)

type ContentDiscoverer interface {
	DiscoverContent(context.Context, openairesearch.DiscoverContentRequest) (openairesearch.DiscoverContentResult, error)
}

func (a *App) ContentDiscoveryConfigured() bool {
	_, ok := a.research.(ContentDiscoverer)
	return a.research != nil && ok
}

func (a *App) ContentSearchConfigured() bool {
	client, ok := a.research.(interface{ ContentSearchConfigured() bool })
	return ok && client.ContentSearchConfigured()
}

// Finish all owned channel/context lookups before reserving a paid AI request.
// Reserve one bounded discovery operation before any paid media/model calls.
func (a *App) DiscoverContentForWorkspaceWithBeforeGenerate(
	ctx context.Context, actorUserID, workspaceID string, channelID *int64,
	request openairesearch.DiscoverContentRequest, beforeGenerate func() error,
) (openairesearch.DiscoverContentResult, error) {
	request = openairesearch.NormalizeDiscoverContentRequest(request)
	if err := openairesearch.ValidateDiscoverContentInput(request); err != nil {
		return openairesearch.DiscoverContentResult{}, err
	}
	if channelID != nil && *channelID <= 0 {
		return openairesearch.DiscoverContentResult{}, errors.New("channel id must be positive")
	}
	if request.Topic == "" && channelID == nil {
		return openairesearch.DiscoverContentResult{}, errors.New("topic or an owned channel is required")
	}
	discoverer, ok := a.research.(ContentDiscoverer)
	if a.research == nil || !ok {
		return openairesearch.DiscoverContentResult{}, ErrResearchNotConfigured
	}
	// Never retain injected context, even for internal callers.
	request.ChannelTitle, request.ChannelDescription = "", ""
	request.RecentPosts, request.ForbiddenWords = nil, nil
	request.Audience, request.Tone = "", ""
	request.MediaContext = ""
	if channelID != nil {
		channel, err := a.store.GetChannelForWorkspace(ctx, actorUserID, workspaceID, *channelID)
		if err != nil {
			return openairesearch.DiscoverContentResult{}, err
		}
		request.ChannelTitle = boundDiscoveryText(channel.Title, 200)
		request.ChannelDescription = boundDiscoveryText(channel.Description, openairesearch.MaxDiscoveryDescriptionRunes)
		if request.Topic == "" {
			request.Topic = request.ChannelTitle
			if utf8.RuneCountInString(request.Topic) < 2 {
				request.Topic = request.ChannelDescription
			}
			if utf8.RuneCountInString(request.Topic) < 2 && request.ChannelTitle != "" {
				request.Topic = "Канал " + request.ChannelTitle
			}
			request.Topic = boundDiscoveryText(request.Topic, openairesearch.MaxDiscoveryTopicRunes)
		}
		samples, err := a.store.ListContentDiscoverySamplesForWorkspace(ctx, actorUserID, workspaceID, *channelID)
		if err != nil {
			return openairesearch.DiscoverContentResult{}, err
		}
		for _, sample := range samples {
			post := openairesearch.DiscoveryPublishedPost{Text: boundDiscoveryText(sample.Text, openairesearch.MaxDiscoverySampleRunes), Media: []openairesearch.DiscoveryMedia{}}
			if sample.ImageCount > 0 {
				post.Media = append(post.Media, openairesearch.DiscoveryMedia{Type: "image", Count: sample.ImageCount})
			}
			if sample.VideoCount > 0 {
				post.Media = append(post.Media, openairesearch.DiscoveryMedia{Type: "video", Count: sample.VideoCount})
			}
			request.RecentPosts = append(request.RecentPosts, post)
			if len(request.RecentPosts) == openairesearch.MaxDiscoverySamples {
				break
			}
		}
	}
	brand, err := a.store.ResolveWorkspaceBrandContext(ctx, actorUserID, workspaceID, nil, channelID)
	if err != nil {
		return openairesearch.DiscoverContentResult{}, err
	}
	profile := brand.BrandKit.BrandProfile
	if brand.Template != nil {
		template := brand.Template.BrandProfile
		if strings.TrimSpace(template.Audience) != "" {
			profile.Audience = template.Audience
		}
		if strings.TrimSpace(template.Tone) != "" {
			profile.Tone = template.Tone
		}
		if len(template.ForbiddenWords) != 0 {
			profile.ForbiddenWords = template.ForbiddenWords
		}
	}
	request.Audience = boundDiscoveryText(profile.Audience, 500)
	request.Tone = boundDiscoveryText(profile.Tone, 100)
	request.ForbiddenWords = append([]string(nil), profile.ForbiddenWords...)
	if request.Tone == "" {
		request.Tone = "Естественный и понятный"
	}
	if request.Topic == "" {
		return openairesearch.DiscoverContentResult{}, errors.New("selected channel has no topic context")
	}
	if err := openairesearch.ValidateDiscoverContentRequest(request); err != nil {
		return openairesearch.DiscoverContentResult{}, err
	}
	if beforeGenerate != nil {
		if err := beforeGenerate(); err != nil {
			return openairesearch.DiscoverContentResult{}, err
		}
	}
	analysis, err := a.analyzeChannelContent(ctx, actorUserID, workspaceID, channelID)
	if err != nil {
		return openairesearch.DiscoverContentResult{}, err
	}
	request.MediaContext = analysis.Summary
	result, err := discoverer.DiscoverContent(ctx, request)
	if err != nil {
		return openairesearch.DiscoverContentResult{}, err
	}
	if channelID != nil {
		if _, err := a.store.GetChannelForWorkspace(ctx, actorUserID, workspaceID, *channelID); err != nil {
			return openairesearch.DiscoverContentResult{}, err
		}
	}
	result, err = a.SaveDiscoveryCandidatesForWorkspace(ctx, actorUserID, workspaceID, channelID, result)
	if err != nil {
		return openairesearch.DiscoverContentResult{}, err
	}
	result.ContextAnalysis = analysis
	return result, nil
}

func boundDiscoveryText(value string, limit int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > limit {
		value = string(runes[:limit])
	}
	return strings.TrimSpace(value)
}
