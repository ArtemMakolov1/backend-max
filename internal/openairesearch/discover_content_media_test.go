package openairesearch

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiscoveryRetrievalAuthorityComesOnlyFromGroundedOriginalImage(t *testing.T) {
	envelope := discoveryFixtureEnvelope(nil)
	envelope.Output[0].Results = []webResult{
		{Type: "text_result", SourceWebsiteURL: "https://example.com/one", ImageURL: "https://cdn.example.com/not-an-image-result.jpg"},
		{Type: "image_result", SourceWebsiteURL: "https://example.com/one", ThumbnailURL: "https://cdn.example.com/thumbnail.jpg"},
		{Type: "image_result", SourceWebsiteURL: "https://example.com/one", ImageURL: "https://cdn.example.com/original.jpg", ThumbnailURL: "https://cdn.example.com/other-thumb.jpg"},
		{Type: "image_result", SourceWebsiteURL: "https://example.com/one", ThumbnailURL: "https://cdn.example.com/later-thumb.jpg"},
		{Type: "image_result", SourceWebsiteURL: "https://example.com/private", ImageURL: "https://127.0.0.1/secret.jpg"},
	}
	media := discoveryMediaSources(envelope)
	if len(media) != 1 || media["https://example.com/one"].URL != "https://cdn.example.com/original.jpg" || media["https://example.com/one"].PreviewOnly {
		t.Fatalf("grounded original source was replaced by a thumbnail or untrusted result: %#v", media)
	}
	envelope.Output[0].Results = []webResult{{Type: "image_result", SourceWebsiteURL: "https://example.com/one", ImageURL: "http://cdn.example.com/unsafe.jpg", ThumbnailURL: "https://cdn.example.com/thumb.jpg"}}
	media = discoveryMediaSources(envelope)
	if len(media) != 1 || !media["https://example.com/one"].PreviewOnly {
		t.Fatal("thumbnail fallback was represented as original media")
	}
}

func TestDiscoveryDoesNotTreatVideoPostersAsDownloadableVideos(t *testing.T) {
	for _, source := range []string{"https://example.com/video/abc123", "https://cdn.example.com/video.mp4", "https://cdn.example.com/video.WEBM?download=1"} {
		t.Run(source, func(t *testing.T) {
			envelope := discoveryFixtureEnvelope([]any{discoveryFixtureCard(source, "video")})
			envelope.Output[0].Action.Sources[0].URL = source
			envelope.Output[0].Results = []webResult{{Type: "image_result", SourceWebsiteURL: source, ImageURL: "https://cdn.example.com/poster.jpg"}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(envelope) }))
			defer server.Close()
			client, err := New(server.URL, "mock-media-key", "gpt-6-luna", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.DiscoverContent(t.Context(), DiscoverContentRequest{Topic: "Коты", ContentKind: "video"})
			if err != nil || len(result.Cards) != 1 || len(result.Cards[0].MediaCandidates) != 1 {
				t.Fatalf("video source missing: %#v err=%v", result, err)
			}
			candidate := result.Cards[0].MediaCandidates[0]
			if candidate.Type != "video" || strings.Contains(candidate.URL, "poster") || (isDirectDiscoveryVideoSource(source) && candidate.URL != source) || (!isDirectDiscoveryVideoSource(source) && candidate.URL != "") {
				t.Fatalf("watch-page thumbnail became video authority: %#v", candidate)
			}
		})
	}
}
