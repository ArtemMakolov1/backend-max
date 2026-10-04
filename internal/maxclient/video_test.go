package maxclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGetVideoReturnsDocumentedSourcesWithoutTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/videos/video_test-123" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "bot-secret" {
			t.Error("unexpected video metadata request")
		}
		_, _ = io.WriteString(w, `{"token":"video_test-123","urls":{"mp4_1080":"https://v.oneme.ru/full.mp4","mp4_720":null,"mp4_480":"https://media.max.ru/clip.mp4","hls":"https://v.oneme.ru/index.m3u8","mp4_144":"http://127.0.0.1/unsafe"},"thumbnail":{"url":"https://media.max.ru/thumb.jpg","token":"private-thumbnail-token"},"width":1920,"height":1080,"duration":37}`)
	}))
	defer server.Close()
	client := mustClient(t, server.URL, "bot-secret", server.Client())
	video, err := client.GetVideo(t.Context(), "video_test-123")
	if err != nil || video.URLs == nil || video.URLs.MP4480 != "https://media.max.ru/clip.mp4" || video.URLs.MP4144 != "" || video.Width != 1920 || video.DurationMS != 37000 || video.ThumbnailURL != "https://media.max.ru/thumb.jpg" {
		t.Fatalf("video metadata = %#v, error = %v", video, err)
	}
	encoded, err := json.Marshal(video)
	if err != nil || strings.Contains(string(encoded), "video_test-123") || strings.Contains(string(encoded), "private-thumbnail-token") {
		t.Fatal("video JSON exposed an opaque provider token")
	}
}

func TestGetVideoMissingNullableAndMalformedMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		bad  bool
	}{
		{"null playback", `{"token":"v","urls":null,"thumbnail":null,"width":0,"height":0,"duration":0}`, false},
		{"omitted playback", `{"token":"v","width":1,"height":1,"duration":1}`, false},
		{"unsafe playback omitted", `{"token":"v","urls":{"mp4_720":"https://localhost/x","hls":"https://max.ru.evil.test/x"},"thumbnail":{"url":"https://u:p@media.max.ru/x"},"width":1,"height":1,"duration":1}`, false},
		{"missing token", `{"width":1,"height":1,"duration":1}`, true},
		{"wrong token", `{"token":"other","width":1,"height":1,"duration":1}`, true},
		{"missing duration", `{"token":"v","width":1,"height":1}`, true},
		{"negative dimensions", `{"token":"v","width":-1,"height":1,"duration":1}`, true},
		{"duration overflow", `{"token":"v","width":1,"height":1,"duration":9223372036854776}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tc.body) }))
			defer server.Close()
			video, err := mustClient(t, server.URL, "bot", server.Client()).GetVideo(t.Context(), "v")
			if (err != nil) != tc.bad {
				t.Fatalf("metadata error = %v, want error %v", err, tc.bad)
			}
			if !tc.bad && (video.URLs != nil || video.ThumbnailURL != "") {
				t.Fatal("unavailable/unsafe metadata was exposed as a playable URL")
			}
		})
	}
}

func TestGetVideoRejectsArbitraryTokensBeforeRequestAndNeverRetries(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"code":"private-video-token","message":"private-video-token"}`)
	}))
	defer server.Close()
	client := mustClient(t, server.URL, "bot", server.Client())
	for _, token := range []string{"", "../v", "v?token=x", "v/v", "v+", strings.Repeat("a", 4097)} {
		if _, err := client.GetVideo(t.Context(), token); err == nil {
			t.Fatal("invalid token accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid token reached the provider")
	}
	_, err := client.GetVideo(t.Context(), "private-video-token")
	var provider *Error
	if !errors.As(err, &provider) || provider.StatusCode != http.StatusForbidden || calls.Load() != 1 || strings.Contains(err.Error(), "private-video-token") || provider.Body != "" {
		t.Fatal("video failure lost status, retried, or exposed the token")
	}
	client = mustClient(t, "https://platform-api2.max.ru", "bot", &http.Client{Transport: fallbackRoundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("transport down") })})
	_, err = client.GetVideo(context.Background(), "private-video-token")
	if err == nil || strings.Contains(err.Error(), "private-video-token") {
		t.Fatal("transport error exposed token-bearing request URL")
	}
}
