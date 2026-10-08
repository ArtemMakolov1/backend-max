package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/mediaanalysis"
	"maxpilot/backend/internal/mediafetch"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

type discoveryCandidatePayload struct {
	Card  openairesearch.ContentCard               `json:"card"`
	Media []openairesearch.DiscoveryMediaCandidate `json:"media"`
}

func (a *App) SaveDiscoveryCandidatesForWorkspace(ctx context.Context, actorUserID, workspaceID string, channelID *int64, result openairesearch.DiscoverContentResult) (openairesearch.DiscoverContentResult, error) {
	payloads := make([]json.RawMessage, 0, len(result.Cards))
	for i := range result.Cards {
		card := result.Cards[i]
		card.CandidateID = ""
		// Private provider metadata was constructed from actual web-search results.
		// Revalidate it against the card's exact authoritative source before saving.
		candidates := make([]openairesearch.DiscoveryMediaCandidate, 0, 1)
		for _, candidate := range card.MediaCandidates {
			if candidate.SourceURL != card.Source.URL || (candidate.Type != "image" && candidate.Type != "video") {
				continue
			}
			candidates = append(candidates, candidate)
			break
		}
		raw, err := json.Marshal(discoveryCandidatePayload{Card: card, Media: candidates})
		if err != nil {
			return result, err
		}
		payloads = append(payloads, raw)
	}
	saved, err := a.store.SaveDiscoveryCandidates(ctx, actorUserID, workspaceID, channelID, payloads, a.now().UTC())
	if err != nil {
		return result, err
	}
	for i := range result.Cards {
		result.Cards[i].CandidateID = saved[i].ID
	}
	return result, nil
}

type CreateDiscoveryDraftRequest struct {
	CandidateID     string `json:"candidate_id"`
	ClientRequestID string `json:"client_request_id"`
	ChannelID       *int64 `json:"channel_id,omitempty"`
	IncludeMedia    *bool  `json:"include_media,omitempty"`
}
type CreateDiscoveryDraftResult struct {
	Post          store.Post                   `json:"post"`
	MediaTransfer store.DiscoveryMediaTransfer `json:"media_transfer"`
}

var discoveryUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)
var errDiscoveryMediaStorage = errors.New("discovery media could not be stored")

func ValidateCreateDiscoveryDraftRequest(request CreateDiscoveryDraftRequest) error {
	if !strings.HasPrefix(request.CandidateID, "cdc_") || len(request.CandidateID) > 80 || len(request.CandidateID) < 8 || !discoveryUUID.MatchString(request.ClientRequestID) {
		return errors.New("a saved candidate and UUID client request id are required")
	}
	if request.ChannelID != nil && *request.ChannelID <= 0 {
		return errors.New("channel id must be positive")
	}
	return nil
}

type discoveryMediaFetcher interface {
	Fetch(context.Context, string, mediafetch.Limits) (*mediafetch.Download, error)
}

func (a *App) CreateContentDiscoveryDraftForWorkspace(ctx context.Context, actor, workspace string, request CreateDiscoveryDraftRequest) (CreateDiscoveryDraftResult, error) {
	return a.createContentDiscoveryDraft(ctx, actor, workspace, request, mediafetch.New())
}

func (a *App) createContentDiscoveryDraft(ctx context.Context, actor, workspace string, request CreateDiscoveryDraftRequest, fetcher discoveryMediaFetcher) (CreateDiscoveryDraftResult, error) {
	if err := ValidateCreateDiscoveryDraftRequest(request); err != nil {
		return CreateDiscoveryDraftResult{}, err
	}
	request.ClientRequestID = strings.ToLower(request.ClientRequestID)
	candidate, err := a.store.GetDiscoveryCandidate(ctx, actor, workspace, request.CandidateID)
	if err != nil {
		return CreateDiscoveryDraftResult{}, err
	}
	var payload discoveryCandidatePayload
	if err = json.Unmarshal(candidate.Payload, &payload); err != nil {
		return CreateDiscoveryDraftResult{}, errors.New("saved discovery candidate is invalid")
	}
	include := request.IncludeMedia == nil || *request.IncludeMedia
	channel := request.ChannelID
	if channel == nil {
		channel = candidate.ChannelID
	}
	op, err := a.store.ClaimDiscoveryDraft(ctx, actor, workspace, request.CandidateID, request.ClientRequestID, channel, include, store.Post{Title: payload.Card.Draft.Title, Content: payload.Card.Draft.Content, Format: payload.Card.Draft.Format, ImagePrompt: payload.Card.Draft.ImagePrompt}, a.now().UTC())
	if err != nil {
		return CreateDiscoveryDraftResult{}, err
	}
	if op.Complete {
		return a.discoveryDraftResult(ctx, actor, workspace, op.PostID, op.Report)
	}
	report := op.Report
	report.Items = append([]store.DiscoveryTransferItem{}, report.Items...)
	report.Warnings = []string{}
	report.RequestedCount = len(payload.Media)
	if len(payload.Media) == 0 && payload.Card.ContentKind == "video" {
		payload.Media = []openairesearch.DiscoveryMediaCandidate{{Type: "video", SourceURL: payload.Card.Source.URL}}
		report.RequestedCount = 1
	}
	for index, candidate := range payload.Media {
		if index < len(report.Items) {
			continue
		} // Durable copied entries survive a lease takeover.
		item := store.DiscoveryTransferItem{Type: candidate.Type, Status: "unavailable"}
		if !include {
			item.Status = "skipped"
			item.Reason = "media_not_requested"
			report.Items = append(report.Items, item)
			continue
		}
		if candidate.URL == "" {
			item.Reason = "video_url_unavailable"
			report.Items = append(report.Items, item)
			continue
		}
		attachmentID, copyErr := a.copyDiscoveryMedia(ctx, actor, workspace, request.ClientRequestID, op, candidate, payload.Card.Source, fetcher)
		if copyErr != nil {
			item.Reason = discoveryTransferReason(copyErr)
		} else {
			item.Status = "copied"
			item.AttachmentID = attachmentID
			if candidate.PreviewOnly {
				item.Reason = "preview_only"
			}
		}
		report.Items = append(report.Items, item)
	}
	report.CopiedCount = 0
	previewOnly := false
	for _, item := range report.Items {
		if item.Status == "copied" {
			report.CopiedCount++
		}
		if item.Reason != "" && item.Reason != "media_not_requested" {
			report.Warnings = append(report.Warnings, item.Reason)
		}
		previewOnly = previewOnly || item.Reason == "preview_only"
	}
	switch {
	case !include:
		report.Status = "skipped"
	case report.CopiedCount == report.RequestedCount && !previewOnly:
		report.Status = "complete"
	case report.CopiedCount > 0:
		report.Status = "partial"
	default:
		report.Status = "unavailable"
	}
	// Saving an accepted attachment/result must survive the HTTP cancellation.
	finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = a.store.FinishDiscoveryDraft(finishCtx, actor, workspace, request.ClientRequestID, op, report); err != nil {
		return CreateDiscoveryDraftResult{}, err
	}
	return a.discoveryDraftResult(ctx, actor, workspace, op.PostID, report)
}

func (a *App) discoveryDraftResult(ctx context.Context, actor, workspace string, postID int64, report store.DiscoveryMediaTransfer) (CreateDiscoveryDraftResult, error) {
	post, err := a.store.GetPostForWorkspace(ctx, actor, workspace, postID)
	if err != nil {
		return CreateDiscoveryDraftResult{}, err
	}
	return CreateDiscoveryDraftResult{Post: post, MediaTransfer: report}, nil
}

func (a *App) copyDiscoveryMedia(ctx context.Context, actor, workspace, requestID string, op store.DiscoveryDraftOperation, candidate openairesearch.DiscoveryMediaCandidate, source openairesearch.Source, fetcher discoveryMediaFetcher) (int64, error) {
	policy := a.currentMediaPolicy()
	limits := store.MediaLimits{MaxFiles: policy.MaxFiles, MaxBytes: policy.MaxBytes}
	capacity, err := a.store.DiscoveryMediaCapacity(ctx, actor, workspace, limits)
	if err != nil {
		return 0, err
	}
	maxBytes := int64(media.MaxImageBytes)
	if candidate.Type == "video" {
		maxBytes = media.MaxVideoBytes
	}
	maxBytes = min(maxBytes, capacity)
	download, err := fetcher.Fetch(ctx, candidate.URL, mediafetch.Limits{MaxBytes: maxBytes, Timeout: 45 * time.Second, MaxRedirects: 2})
	if err != nil {
		return 0, err
	}
	defer func() { _ = download.Close() }()
	// Actual sniffed MIME, not an arbitrary provider filename, determines format.
	var body io.Reader = download.Body
	mimeType := download.MIMEType
	if candidate.Type == "image" && mimeType == "image/webp" {
		if download.ContentLength > mediaanalysis.MaxStaticWebPBytes {
			return 0, &mediafetch.Error{Code: "too_large"}
		}
		converted, err := mediaanalysis.ConvertStaticWebP(ctx, body)
		if err != nil {
			return 0, err
		}
		body, mimeType = bytes.NewReader(converted), "image/png"
	}
	filename, err := discoveryMediaFilename(candidate.Type, mimeType, download.FinalURL)
	if err != nil {
		return 0, err
	}
	upload, err := a.media.PrepareAttachment(candidate.Type, filename, body)
	if err != nil {
		return 0, err
	}
	defer func() { _ = upload.Close() }()
	file := upload.File()
	reservation, err := a.store.ReserveDiscoveryMediaForWorkspace(ctx, actor, workspace, file.Filename, file.Size, limits, a.now().UTC())
	if err != nil {
		return 0, err
	}
	if !reservation.Existing {
		if err = upload.Store(ctx); err != nil {
			a.releaseMediaReservation(reservation)
			return 0, errors.Join(errDiscoveryMediaStorage, err)
		}
		if err = a.store.CompleteMediaReservation(ctx, reservation, a.now().UTC()); err != nil {
			a.releaseMediaReservation(reservation)
			return 0, err
		}
	}
	metadata, _ := json.Marshal(map[string]any{"origin": "content_discovery", "source_title": source.Title, "source_url": source.URL, "candidate_id": op.Candidate.ID, "preview_only": candidate.PreviewOnly})
	attachment := store.PostAttachment{Type: file.Type, StorageKey: file.Path, SizeBytes: file.Size, MIMEType: file.MIMEType, ProviderMeta: metadata, Position: -1}
	if file.Width > 0 {
		width := file.Width
		attachment.Width = &width
	}
	if file.Height > 0 {
		height := file.Height
		attachment.Height = &height
	}
	if file.DurationMS > 0 {
		duration := file.DurationMS
		attachment.DurationMS = &duration
	}
	// Fence after external retrieval/storage: a changed role, post or lease
	// cannot attach media to a different revision. Unreferenced quota-accounted
	// files follow normal media GC instead of deleting shared content hashes.
	return a.store.AddDiscoveryDraftAttachment(ctx, actor, workspace, requestID, op, attachment)
}

func discoveryMediaFilename(kind, mime, rawURL string) (string, error) {
	switch {
	case kind == "image" && mime == "image/png":
		return "discovery.png", nil
	case kind == "image" && mime == "image/jpeg":
		return "discovery.jpg", nil
	case kind == "image" && mime == "image/gif":
		return "discovery.gif", nil
	case kind == "video" && (mime == "video/mp4" || mime == "video/quicktime"):
		if mime == "video/quicktime" {
			return "discovery.mov", nil
		}
		return "discovery.mp4", nil
	case kind == "video" && (mime == "video/webm" || mime == "application/octet-stream"):
		parsed, _ := url.Parse(rawURL)
		if parsed != nil && strings.EqualFold(path.Ext(parsed.Path), ".webm") {
			return "discovery.webm", nil
		}
	}
	return "", errors.New("unsupported discovery media format")
}

func discoveryTransferReason(err error) string {
	var fetchErr *mediafetch.Error
	if errors.As(err, &fetchErr) {
		switch fetchErr.Code {
		case "too_large":
			return "media_too_large"
		case "timeout":
			return "media_timeout"
		case "unsafe_url", "unsafe_address", "redirect_limit":
			return "media_source_unsafe"
		case "invalid_mime":
			return "media_format_unsupported"
		default:
			return "media_source_unavailable"
		}
	}
	switch {
	case errors.Is(err, errDiscoveryMediaStorage):
		return "media_storage_unavailable"
	case errors.Is(err, store.ErrMediaQuotaExceeded):
		return "media_quota_exceeded"
	case errors.Is(err, store.ErrMediaUploadBusy):
		return "media_upload_busy"
	case errors.Is(err, store.ErrNotFound):
		return "media_access_changed"
	case errors.Is(err, store.ErrConflict):
		return "draft_changed"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "media_timeout"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "media_source_unavailable"
	default:
		return "media_format_unsupported"
	}
}
