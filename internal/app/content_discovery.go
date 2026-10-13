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

const maxDiscoveryPublicationCandidates = 20
const maxDiscoveryPublicationComparisonReferences = 3

func (a *App) ContentDiscoveryConfigured() bool {
	_, ok := a.research.(ContentDiscoverer)
	return a.research != nil && ok && a.ValidateResearchSources() == nil
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
	request.ChannelPosts, request.ExamplePosts = nil, nil
	request.ExamplePostsOverride = false
	request.CTA, request.VisualStyle = "", ""
	request.Audience, request.Tone = "", ""
	request.MediaContext = ""
	request.PublishedMatcher = nil
	request.PublishedReferenceLookup = nil
	if channelID != nil {
		channel, err := a.store.GetChannelForWorkspace(ctx, actorUserID, workspaceID, *channelID)
		if err != nil {
			return openairesearch.DiscoverContentResult{}, err
		}
		if err := a.store.EnsurePublishedContentIndexForWorkspace(ctx, actorUserID, workspaceID, *channelID); err != nil {
			return openairesearch.DiscoverContentResult{}, err
		}
		ownedChannelID := *channelID
		request.PublishedReferenceLookup = func(matchCtx context.Context, candidates []openairesearch.DiscoveryPublishedCandidate) ([][]openairesearch.DiscoveryPublishedReference, error) {
			if len(candidates) > maxDiscoveryPublicationCandidates {
				return nil, errors.New("publication identity candidates exceed the bounded lookup")
			}
			comparisons := make([][]openairesearch.DiscoveryPublishedReference, len(candidates))
			for i, candidate := range candidates {
				references, err := a.store.FindPublishedDiscoveryReferences(matchCtx, actorUserID, workspaceID, ownedChannelID,
					candidate.Title, candidate.Text, candidate.SourceURL)
				if err != nil {
					return nil, err
				}
				strong := make([]openairesearch.DiscoveryPublishedReference, 0, maxDiscoveryPublicationComparisonReferences)
				other := make([]openairesearch.DiscoveryPublishedReference, 0, maxDiscoveryPublicationComparisonReferences)
				var definitive *openairesearch.DiscoveryPublishedReference
				for _, reference := range references {
					published := openairesearch.DiscoveryPublishedReference{
						Title: reference.Title, Text: reference.Text, SourceURLs: reference.SourceURLs,
					}
					// Check all local references before narrowing comparisons. A
					// strong event-relevance hint must remain available even when
					// it ranks below three less specific text matches; the service
					// distinguishes exact copies from new related milestones.
					if openairesearch.IsDefinitivePublishedDiscoveryDuplicate(candidate, published) {
						definitive = &published
						break
					}
					if openairesearch.IsPublishedDiscoveryDuplicate(candidate, published) {
						if len(strong) < maxDiscoveryPublicationComparisonReferences {
							strong = append(strong, published)
						}
					} else if len(other) < maxDiscoveryPublicationComparisonReferences {
						other = append(other, published)
					}
				}
				if definitive != nil {
					comparisons[i] = []openairesearch.DiscoveryPublishedReference{*definitive}
				} else {
					comparisons[i] = append(strong, other[:min(len(other), maxDiscoveryPublicationComparisonReferences-len(strong))]...)
				}
			}
			return comparisons, nil
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
			post := openairesearch.DiscoveryPublishedPost{Text: boundDiscoveryText(sample.Text, openairesearch.MaxDiscoverySampleRunes), Media: []openairesearch.DiscoveryMedia{}, Format: sample.Format, CharacterCount: sample.CharacterCount}
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
		styleSamples, err := a.store.ListChannelStyleSamplesForWorkspace(ctx, actorUserID, workspaceID, *channelID)
		if err != nil {
			return openairesearch.DiscoverContentResult{}, err
		}
		for _, sample := range styleSamples {
			request.ChannelPosts = append(request.ChannelPosts, openairesearch.ChannelPostStyle{Text: boundDiscoveryText(sample.Text, openairesearch.MaxChannelStyleSampleRunes), Format: sample.Format, CharacterCount: sample.CharacterCount})
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
		if strings.TrimSpace(template.CTA) != "" {
			profile.CTA = template.CTA
		}
		if strings.TrimSpace(template.VisualStyle) != "" {
			profile.VisualStyle = template.VisualStyle
		}
		if len(template.ExamplePosts) != 0 {
			profile.ExamplePosts = template.ExamplePosts
			request.ExamplePostsOverride = true
		}
	}
	request.Audience = boundDiscoveryText(profile.Audience, 500)
	request.Tone = boundDiscoveryText(profile.Tone, 100)
	request.ForbiddenWords = append([]string(nil), profile.ForbiddenWords...)
	request.CTA = boundDiscoveryText(profile.CTA, 500)
	request.VisualStyle = boundDiscoveryText(profile.VisualStyle, 1000)
	request.ExamplePosts = append([]string(nil), profile.ExamplePosts...)
	if request.Tone == "" {
		request.Tone = "Естественный и понятный"
	}
	if request.Topic == "" {
		return openairesearch.DiscoverContentResult{}, errors.New("selected channel has no topic context")
	}
	if err := openairesearch.ValidateDiscoverContentRequest(request); err != nil {
		return openairesearch.DiscoverContentResult{}, err
	}
	if err := a.ValidateResearchSources(); err != nil {
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
