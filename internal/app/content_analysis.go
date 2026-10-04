package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"maxpilot/backend/internal/mediaanalysis"
	"maxpilot/backend/internal/mediafetch"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

type contentMediaAnalyzer interface {
	AnalyzeContentMedia(context.Context, openairesearch.ContentAnalysisRequest) (string, error)
	AnalyzeContentAudio(context.Context, []byte) (string, error)
	ContentAnalysisModelKey() string
}

func textContentAnalysis() *openairesearch.ContentContextAnalysis {
	return &openairesearch.ContentContextAnalysis{Status: "text_only", Warnings: []string{}}
}

func (a *App) analyzeChannelContent(ctx context.Context, actor, workspaceID string, channelID *int64) (*openairesearch.ContentContextAnalysis, error) {
	result := textContentAnalysis()
	analyzer, ok := a.research.(contentMediaAnalyzer)
	if channelID == nil || !ok {
		return result, nil
	}
	access, err := a.store.ResolveWorkspaceAccess(ctx, actor, workspaceID)
	if err != nil {
		return nil, err
	}
	if !AccessContextForWorkspace(access.Workspace, actor, access.Member.Role).Can(CapabilityAIUse) {
		return nil, store.ErrNotFound
	}
	posts, err := a.store.ListContentAnalysisPostsForWorkspace(ctx, actor, workspaceID, *channelID)
	if err != nil {
		return nil, err
	}
	if len(posts) == 0 {
		return result, nil
	}
	key := store.ContentAnalysisSnapshotKey(posts, analyzer.ContentAnalysisModelKey())
	cached, claim, err := a.store.ClaimContentAnalysis(ctx, actor, workspaceID, *channelID, key, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if cached != "" {
		if json.Unmarshal([]byte(cached), result) != nil {
			return nil, errors.New("invalid saved content analysis")
		}
		result.Cached = true
		return result, nil
	}
	result.Status = "unavailable"
	if claim == "" {
		result.Warnings = append(result.Warnings, "analysis_in_progress")
		return result, nil
	}
	analysisCtx, cancel := context.WithTimeout(ctx, 85*time.Second)
	defer cancel()
	if !mediaanalysis.Available() {
		result.Warnings = append(result.Warnings, "media_decoder_unavailable")
	} else {
		request := openairesearch.ContentAnalysisRequest{}
		images, videos, audios := 0, 0, 0
		selected := 0
		for _, post := range posts {
			attachments := post.Attachments
			if len(attachments) == 0 && (post.ImagePath != "" || post.ImageURL != "") {
				attachments = []store.PostAttachment{{Type: store.PostAttachmentImage, StorageKey: post.ImagePath, URL: post.ImageURL, OwnerID: post.UserID, Source: store.PostAttachmentSourceUpload}}
			}
			for _, att := range attachments {
				if selected >= 3 || (att.Type == store.PostAttachmentVideo && videos >= 1) {
					continue
				}
				if att.Type != store.PostAttachmentImage && att.Type != store.PostAttachmentVideo {
					continue
				}
				selected++
				prepared, err := a.prepareAnalysisAttachment(analysisCtx, actor, workspaceID, post, att)
				if err != nil {
					result.Warnings = append(result.Warnings, "media_unavailable")
					continue
				}
				for _, frame := range prepared.Frames {
					if len(request.Images) >= 6 {
						break
					}
					label := "Изображение публикации"
					if att.Type == store.PostAttachmentVideo {
						label = "Кадр короткого фрагмента видео"
					}
					request.Images = append(request.Images, openairesearch.ContentAnalysisImage{JPEG: frame, Label: label})
				}
				if att.Type == store.PostAttachmentVideo {
					videos++
					result.SampleLimitSeconds = mediaanalysis.SampleSeconds
					if prepared.Sampled {
						result.Warnings = append(result.Warnings, "video_fragment_only")
					}
					if prepared.HasAudio {
						if len(prepared.Audio) == 0 {
							result.Warnings = append(result.Warnings, "audio_unavailable")
						} else {
							audioCtx, audioCancel := context.WithTimeout(analysisCtx, 25*time.Second)
							summary, audioErr := analyzer.AnalyzeContentAudio(audioCtx, prepared.Audio)
							audioCancel()
							if audioErr != nil {
								result.Warnings = append(result.Warnings, "audio_unavailable")
							} else {
								request.AudioSummary = summary
								audios++
							}
						}
					}
				} else {
					images++
				}
			}
		}
		if len(request.Images) != 0 || request.AudioSummary != "" {
			modelCtx, modelCancel := context.WithTimeout(analysisCtx, 25*time.Second)
			summary, modelErr := analyzer.AnalyzeContentMedia(modelCtx, request)
			modelCancel()
			if modelErr != nil {
				result.Warnings = append(result.Warnings, "visual_analysis_unavailable")
				if audios > 0 {
					result.Summary = request.AudioSummary
					result.AudioAnalyzed = audios
					result.Status = "partial"
				}
			} else {
				result.Summary = summary
				result.ImagesAnalyzed = images
				result.VideosAnalyzed = videos
				result.AudioAnalyzed = audios
				result.Status = "ready"
				if len(result.Warnings) > 0 {
					result.Status = "partial"
				}
			}
		}
	}
	// Re-read membership and the published media snapshot after the external
	// work. A moved, edited or revoked channel cannot use the previous result.
	current, err := a.store.ListContentAnalysisPostsForWorkspace(ctx, actor, workspaceID, *channelID)
	if err != nil {
		return nil, err
	}
	if store.ContentAnalysisSnapshotKey(current, analyzer.ContentAnalysisModelKey()) != key {
		return nil, store.ErrConflict
	}
	result.Warnings = uniqueAnalysisWarnings(result.Warnings)
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	expires := time.Now().UTC().Add(7 * 24 * time.Hour)
	if result.Status == "unavailable" || result.Status == "partial" {
		expires = time.Now().UTC().Add(10 * time.Minute)
	}
	if err := a.store.CompleteContentAnalysis(ctx, actor, workspaceID, *channelID, key, claim, string(raw), expires); err != nil {
		return nil, err
	}
	return result, nil
}

func uniqueAnalysisWarnings(input []string) []string {
	result := []string{}
	seen := map[string]bool{}
	for _, warning := range input {
		if !seen[warning] {
			seen[warning] = true
			result = append(result, warning)
		}
	}
	return result
}

func (a *App) prepareAnalysisAttachment(ctx context.Context, actor, workspaceID string, post store.Post, att store.PostAttachment) (mediaanalysis.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	video := att.Type == store.PostAttachmentVideo
	var body io.ReadCloser
	key := att.StorageKey
	if key == "" && a.media != nil && att.Source == store.PostAttachmentSourceUpload {
		key, _ = a.media.FilenameFromURL(att.URL)
	}
	if key != "" && a.media != nil {
		owned, err := a.store.OwnsContentAnalysisMedia(ctx, actor, workspaceID, key)
		if err != nil {
			return mediaanalysis.Result{}, err
		}
		if !owned {
			return mediaanalysis.Result{}, store.ErrNotFound
		}
		object, err := a.media.Open(ctx, key)
		if err != nil {
			return mediaanalysis.Result{}, err
		}
		if object.Size > mediaanalysis.MaxInputBytes {
			_ = object.Body.Close()
			return mediaanalysis.Result{}, errors.New("analysis media too large")
		}
		body = object.Body
	} else {
		remote := att.RemoteURL
		if video {
			preview, err := a.GetMAXVideoPreviewForWorkspace(ctx, actor, workspaceID, post.ID, att.ID)
			if err != nil {
				return mediaanalysis.Result{}, err
			}
			if preview.URLs == nil {
				return mediaanalysis.Result{}, errors.New("analysis video unavailable")
			}
			// Use a low resolution direct MP4. Never hand HLS or a network URL
			// to a decoder, and never interpret a thumbnail as the video itself.
			for _, candidate := range []string{preview.URLs.MP4240, preview.URLs.MP4360, preview.URLs.MP4144, preview.URLs.MP4480, preview.URLs.MP4720, preview.URLs.MP41080} {
				if candidate != "" {
					remote = candidate
					break
				}
			}
		} else if remote == "" {
			remote = att.URL
		}
		if strings.TrimSpace(remote) == "" {
			return mediaanalysis.Result{}, errors.New("analysis media unavailable")
		}
		limit := int64(8 << 20)
		if video {
			limit = mediaanalysis.MaxInputBytes
		}
		download, err := mediafetch.New().Fetch(ctx, remote, mediafetch.Limits{MaxBytes: limit, Timeout: 15 * time.Second, MaxRedirects: 3})
		if err != nil {
			return mediaanalysis.Result{}, errors.New("analysis media download unavailable")
		}
		body = download.Body
	}
	defer func() { _ = body.Close() }()
	return mediaanalysis.Prepare(ctx, body, video)
}
