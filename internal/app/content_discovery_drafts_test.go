package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/mediaanalysis"
	"maxpilot/backend/internal/mediafetch"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

type discoveryFetcherFake struct {
	data   []byte
	mime   string
	calls  int
	before func()
	err    error
}

func (f *discoveryFetcherFake) Fetch(_ context.Context, raw string, limits mediafetch.Limits) (*mediafetch.Download, error) {
	f.calls++
	if f.before != nil {
		f.before()
	}
	if f.err != nil {
		return nil, f.err
	}
	if int64(len(f.data)) > limits.MaxBytes {
		return nil, &mediafetch.Error{Code: "too_large"}
	}
	return &mediafetch.Download{Body: io.NopCloser(bytes.NewReader(f.data)), MIMEType: f.mime, FinalURL: raw, ContentLength: int64(len(f.data))}, nil
}

func discoveryPNG(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 4, 3))); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func discoveryCard(kind string, preview bool) openairesearch.ContentCard {
	source := openairesearch.Source{Title: "Verified source", URL: "https://example.com/source"}
	card := openairesearch.ContentCard{ID: "grounded", ContentKind: kind, Title: "Material", Source: source,
		Draft: openairesearch.Draft{Title: "Draft", Content: "Ready text and https://example.com/source", Format: "markdown"}}
	switch kind {
	case "meme":
		card.MediaCandidates = []openairesearch.DiscoveryMediaCandidate{{Type: "image", URL: "https://media.example.com/image?signature=private-candidate-secret", SourceURL: source.URL, PreviewOnly: preview}}
	case "video":
		card.MediaCandidates = []openairesearch.DiscoveryMediaCandidate{{Type: "video", SourceURL: source.URL}}
	}
	return card
}

func saveDiscoveryCard(t *testing.T, a *App, actor, workspace string, card openairesearch.ContentCard) string {
	t.Helper()
	result, err := a.SaveDiscoveryCandidatesForWorkspace(t.Context(), actor, workspace, nil, openairesearch.DiscoverContentResult{Topic: "Topic", Cards: []openairesearch.ContentCard{card}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil || strings.Contains(string(encoded), "private-candidate-secret") {
		t.Fatalf("private retrieval authority leaked into cards: %s err=%v", encoded, err)
	}
	return result.Cards[0].CandidateID
}

func TestDiscoveryDraftCopiesOnlySavedGroundedMediaAndReturnsSamePostOnRetry(t *testing.T) {
	for _, test := range []struct {
		name, kind, status, reason string
		preview, skip, direct      bool
		copied, requested, calls   int
	}{
		{name: "image", kind: "meme", status: "complete", copied: 1, requested: 1, calls: 1},
		{name: "thumbnail", kind: "meme", preview: true, status: "partial", copied: 1, requested: 1, calls: 1, reason: "preview_only"},
		{name: "watch page", kind: "video", status: "unavailable", requested: 1, reason: "video_url_unavailable"},
		{name: "direct video", kind: "video", direct: true, status: "complete", copied: 1, requested: 1, calls: 1},
		{name: "explicit skip", kind: "meme", skip: true, status: "skipped", requested: 1, reason: "media_not_requested"},
		{name: "text only", kind: "idea", status: "complete"},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, s, workspace := newChannelDescriptionFixture(t, nil)
			card := discoveryCard(test.kind, test.preview)
			fetcher := &discoveryFetcherFake{data: discoveryPNG(t), mime: "image/png"}
			if test.direct {
				card.Source.URL = "https://cdn.example.com/video.mp4"
				card.MediaCandidates = []openairesearch.DiscoveryMediaCandidate{{Type: "video", URL: card.Source.URL, SourceURL: card.Source.URL}}
				fetcher.data = []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'm', 'p', '4', '2', 0, 0, 0, 0, 'm', 'p', '4', '2', 'i', 's', 'o', 'm'}
				fetcher.mime = "video/mp4"
			}
			candidate := saveDiscoveryCard(t, a, "description-owner", workspace.ID, card)
			request := CreateDiscoveryDraftRequest{CandidateID: candidate, ClientRequestID: "01234567-89ab-4cde-8f01-23456789abcd"}
			if test.skip {
				include := false
				request.IncludeMedia = &include
			}
			result, err := a.createContentDiscoveryDraft(t.Context(), "description-owner", workspace.ID, request, fetcher)
			if err != nil || result.Post.ID <= 0 || result.Post.Status != store.PostStatusDraft || result.MediaTransfer.Status != test.status || result.MediaTransfer.CopiedCount != test.copied || result.MediaTransfer.RequestedCount != test.requested || len(result.Post.Attachments) != test.copied || fetcher.calls != test.calls {
				t.Fatalf("transfer=%#v err=%v fetches=%d", result, err, fetcher.calls)
			}
			if test.reason != "" && (len(result.MediaTransfer.Items) != 1 || result.MediaTransfer.Items[0].Reason != test.reason) {
				t.Fatalf("media outcome is not explicit: %#v", result.MediaTransfer)
			}
			if test.copied == 1 {
				attachment := result.Post.Attachments[0]
				object, err := a.media.Open(t.Context(), attachment.StorageKey)
				if err != nil {
					t.Fatalf("counted copy was not stored: %v", err)
				}
				actual, err := io.ReadAll(object.Body)
				_ = object.Body.Close()
				if err != nil || !bytes.Equal(actual, fetcher.data) {
					t.Fatal("stored draft media differs from verified download")
				}
			}
			replay, err := a.createContentDiscoveryDraft(t.Context(), "description-owner", workspace.ID, request, fetcher)
			if err != nil || replay.Post.ID != result.Post.ID || fetcher.calls != test.calls || len(replay.Post.Attachments) != test.copied {
				t.Fatalf("retry duplicated work: %#v err=%v calls=%d", replay, err, fetcher.calls)
			}
			posts, err := s.ListPostsForWorkspace(t.Context(), "description-owner", workspace.ID, "", nil)
			if err != nil || len(posts) != 1 {
				t.Fatal("request retry created a second draft")
			}
		})
	}
}

func TestDiscoveryDraftRejectsForeignCandidateAndChecksQuotaBeforeFetching(t *testing.T) {
	a, s, workspace := newChannelDescriptionFixture(t, nil)
	if err := s.UpsertUser(t.Context(), store.User{ID: "other-discovery-user", DisplayName: "Other"}); err != nil {
		t.Fatal(err)
	}
	candidate := saveDiscoveryCard(t, a, "description-owner", workspace.ID, discoveryCard("meme", false))
	fetcher := &discoveryFetcherFake{data: discoveryPNG(t), mime: "image/png"}
	request := CreateDiscoveryDraftRequest{CandidateID: candidate, ClientRequestID: "01234567-89ab-4cde-8f01-23456789abcd"}
	if _, err := a.createContentDiscoveryDraft(t.Context(), "other-discovery-user", workspace.ID, request, fetcher); !errors.Is(err, store.ErrNotFound) || fetcher.calls != 0 {
		t.Fatalf("foreign candidate reached network: err=%v calls=%d", err, fetcher.calls)
	}
	policy := a.currentMediaPolicy()
	policy.MaxFiles, policy.MaxBytes = 1, 1000
	if err := a.ConfigureMediaPolicy(policy); err != nil {
		t.Fatal(err)
	}
	reservation, err := s.ReserveDiscoveryMediaForWorkspace(t.Context(), "description-owner", workspace.ID, "quota.png", 10, store.MediaLimits{MaxFiles: 1, MaxBytes: 1000}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteMediaReservation(t.Context(), reservation, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	result, err := a.createContentDiscoveryDraft(t.Context(), "description-owner", workspace.ID, request, fetcher)
	if err != nil || result.MediaTransfer.Status != "unavailable" || result.MediaTransfer.Items[0].Reason != "media_quota_exceeded" || fetcher.calls != 0 || len(result.Post.Attachments) != 0 {
		t.Fatalf("quota did not preserve text and stop retrieval: %#v err=%v calls=%d", result, err, fetcher.calls)
	}
}

func TestDiscoveryDraftDoesNotClaimCopyWhenStorageFailsOrPostChanges(t *testing.T) {
	for _, change := range []string{"storage failure", "post changed during retrieval"} {
		t.Run(change, func(t *testing.T) {
			a, s, workspace := newChannelDescriptionFixture(t, nil)
			candidate := saveDiscoveryCard(t, a, "description-owner", workspace.ID, discoveryCard("meme", false))
			fetcher := &discoveryFetcherFake{data: discoveryPNG(t), mime: "image/png"}
			if change == "storage failure" {
				storagePath := filepath.Join(t.TempDir(), "blocked-media")
				var err error
				a.media, err = media.New(storagePath, "http://localhost:8080")
				if err != nil {
					t.Fatal(err)
				}
				if err = os.RemoveAll(storagePath); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(storagePath, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				fetcher.before = func() {
					posts, err := s.ListPostsForWorkspace(t.Context(), "description-owner", workspace.ID, "", nil)
					if err != nil || len(posts) != 1 {
						t.Fatal("initial draft unavailable during download")
					}
					text := "Edited while retrieval was running"
					if _, err = s.UpdatePostForWorkspaceIfUnchanged(t.Context(), "description-owner", workspace.ID, posts[0], store.PostChanges{Content: &text}); err != nil {
						t.Fatal(err)
					}
				}
			}
			result, err := a.createContentDiscoveryDraft(t.Context(), "description-owner", workspace.ID, CreateDiscoveryDraftRequest{CandidateID: candidate, ClientRequestID: "01234567-89ab-4cde-8f01-23456789abcd"}, fetcher)
			if err != nil || result.MediaTransfer.CopiedCount != 0 || len(result.Post.Attachments) != 0 || result.MediaTransfer.Status != "unavailable" {
				t.Fatalf("unconfirmed/mismatched storage copy counted: %#v err=%v", result, err)
			}
			if change == "post changed during retrieval" && (result.MediaTransfer.Items[0].Reason != "draft_changed" || result.Post.Content != "Edited while retrieval was running") {
				t.Fatal("concurrent user edit was lost")
			}
			if change == "storage failure" {
				usage, err := s.GetWorkspaceMediaUsage(t.Context(), workspace.ID)
				if err != nil || usage.AssetCount != 0 || usage.TotalBytes != 0 {
					t.Fatalf("failed storage write retained quota: %#v err=%v", usage, err)
				}
			}
		})
	}
}

func TestDiscoveryDraftConvertsTransparentWebPIntoActualPNGAndAccountsConvertedSize(t *testing.T) {
	if !mediaanalysis.Available() {
		t.Skip("actual local FFmpeg WebP conversion required")
	}
	raw, err := os.ReadFile(filepath.Join("..", "mediaanalysis", "testdata", "synthetic-alpha.webp"))
	if err != nil {
		t.Fatal(err)
	}
	a, s, workspace := newChannelDescriptionFixture(t, nil)
	candidate := saveDiscoveryCard(t, a, "description-owner", workspace.ID, discoveryCard("meme", false))
	fetcher := &discoveryFetcherFake{data: raw, mime: "image/webp"}
	result, err := a.createContentDiscoveryDraft(t.Context(), "description-owner", workspace.ID, CreateDiscoveryDraftRequest{CandidateID: candidate, ClientRequestID: "01234567-89ab-4cde-8f01-23456789abcd"}, fetcher)
	if err != nil || result.MediaTransfer.Status != "complete" || result.MediaTransfer.CopiedCount != 1 || len(result.Post.Attachments) != 1 {
		t.Fatalf("WebP not copied into draft: %#v err=%v", result, err)
	}
	attachment := result.Post.Attachments[0]
	if attachment.MIMEType != "image/png" || attachment.Width == nil || *attachment.Width != 7 || attachment.Height == nil || *attachment.Height != 5 {
		t.Fatalf("converted format/dimensions wrong: %#v", attachment)
	}
	object, err := a.media.Open(t.Context(), attachment.StorageKey)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := io.ReadAll(object.Body)
	_ = object.Body.Close()
	decoded, decodeErr := png.Decode(bytes.NewReader(encoded))
	if err != nil || decodeErr != nil {
		t.Fatalf("actual stored PNG unreadable: %v %v", err, decodeErr)
	}
	_, _, _, alpha := decoded.At(0, 0).RGBA()
	if alpha != 0 {
		t.Fatal("conversion lost source transparency")
	}
	usage, err := s.GetWorkspaceMediaUsage(t.Context(), workspace.ID)
	if err != nil || usage.AssetCount != 1 || usage.TotalBytes != int64(len(encoded)) || usage.TotalBytes == int64(len(raw)) {
		t.Fatalf("quota counted source bytes rather than actual PNG: %#v input=%d output=%d err=%v", usage, len(raw), len(encoded), err)
	}
}
