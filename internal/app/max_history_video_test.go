package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/store"
)

type historyVideoMetadataFake struct {
	MAXClient
	calls []string
	read  func(string) (maxclient.VideoInfo, error)
}

func (f *historyVideoMetadataFake) GetVideo(_ context.Context, token string) (maxclient.VideoInfo, error) {
	f.calls = append(f.calls, token)
	if f.read != nil {
		return f.read(token)
	}
	return maxclient.VideoInfo{Token: token, URLs: &maxclient.VideoURLs{MP4480: "https://media.max.ru/" + token + ".mp4"}, Width: 640, Height: 480, DurationMS: 37000}, nil
}

func tokenOnlyHistoryVideo(index int) maxclient.HistoryMessage {
	return maxclient.HistoryMessage{
		MessageID: fmt.Sprintf("mid.video.%d", index), Text: "Видео", TimestampMillis: 1_785_700_000_123 - int64(index),
		Attachments: []maxclient.HistoryAttachment{{Type: "video", Token: fmt.Sprintf("video_%d", index), Complete: true}},
	}
}

func TestMAXHistoryVideoMetadataBudgetPreservesPrefixAndCachesTokens(t *testing.T) {
	fake := &historyVideoMetadataFake{}
	messages := make([]maxclient.HistoryMessage, 30)
	for index := range messages {
		messages[index] = tokenOnlyHistoryVideo(index)
	}
	prepared, limited, err := prepareMAXHistoryVideoMedia(t.Context(), fake, messages)
	if err != nil || !limited || len(prepared) != 20 || len(fake.calls) != 20 {
		t.Fatalf("budget result len=%d limited=%v calls=%d error=%v", len(prepared), limited, len(fake.calls), err)
	}
	for index, message := range prepared {
		attachment := message.Attachments[0]
		if message.MessageID != messages[index].MessageID || attachment.URL == "" || attachment.DurationMS == nil || *attachment.DurationMS != 37000 || messages[index].Attachments[0].URL != "" {
			t.Fatal("prefix lost source order, metadata or mutated the provider page")
		}
	}
	complete, cursor, stalled, err := maxHistoryPageCursor(maxHistoryPageSize, prepared[19].TimestampMillis, 0)
	if err != nil || complete || stalled || cursor == nil || *cursor != messages[19].TimestampMillis {
		t.Fatal("short provider prefix did not retain the inclusive resume boundary")
	}

	// Repeated tokens consume one read, including multiple occurrences inside a
	// single gallery. A legal gallery always fits on an otherwise empty page.
	fake.calls = nil
	gallery := tokenOnlyHistoryVideo(0)
	for len(gallery.Attachments) < store.MaxPostAttachments {
		gallery.Attachments = append(gallery.Attachments, gallery.Attachments[0])
	}
	prepared, limited, err = prepareMAXHistoryVideoMedia(t.Context(), fake, []maxclient.HistoryMessage{gallery, gallery})
	if err != nil || limited || len(prepared) != 2 || len(fake.calls) != 1 || len(prepared[0].Attachments) != store.MaxPostAttachments {
		t.Fatal("repeated token cache or legal first-page progress failed")
	}
	fake.calls = nil
	gallery.Attachments = append(gallery.Attachments, gallery.Attachments[0])
	prepared, limited, err = prepareMAXHistoryVideoMedia(t.Context(), fake, []maxclient.HistoryMessage{gallery})
	if err != nil || limited || len(prepared) != 1 || len(fake.calls) != 0 {
		t.Fatal("unsupported oversized gallery caused metadata fan-out")
	}
}

func TestMAXHistoryVideoMetadataUnavailableDoesNotInventMP4(t *testing.T) {
	for _, test := range []struct {
		name string
		urls *maxclient.VideoURLs
		err  error
	}{
		{name: "null"},
		{name: "forbidden", err: &maxclient.Error{StatusCode: http.StatusForbidden}},
		{name: "missing", err: &maxclient.Error{StatusCode: http.StatusNotFound}},
		{name: "HLS only", urls: &maxclient.VideoURLs{HLS: "https://media.max.ru/live.m3u8"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &historyVideoMetadataFake{read: func(token string) (maxclient.VideoInfo, error) {
				return maxclient.VideoInfo{Token: token, URLs: test.urls}, test.err
			}}
			prepared, limited, err := prepareMAXHistoryVideoMedia(t.Context(), fake, []maxclient.HistoryMessage{tokenOnlyHistoryVideo(0)})
			if err != nil || limited || len(prepared) != 1 || prepared[0].Attachments[0].URL != "" {
				t.Fatal("unavailable or HLS-only source became a fabricated MP4")
			}
			item, err := maxHistoryItem(prepared[0], 42)
			if err != nil || item.RoundTrip || len(item.Attachments) != 0 {
				t.Fatal("unavailable media became an editable imported gallery")
			}
		})
	}
}

func TestMAXHistoryVideoMetadataTransientFailureRejectsWholePage(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, &maxclient.Error{StatusCode: 429}, &maxclient.Error{StatusCode: 500}} {
		fake := &historyVideoMetadataFake{read: func(token string) (maxclient.VideoInfo, error) {
			if token == "video_0" {
				return maxclient.VideoInfo{Token: token, URLs: &maxclient.VideoURLs{MP4480: "https://media.max.ru/video.mp4"}}, nil
			}
			return maxclient.VideoInfo{}, cause
		}}
		prepared, limited, err := prepareMAXHistoryVideoMedia(t.Context(), fake, []maxclient.HistoryMessage{tokenOnlyHistoryVideo(0), tokenOnlyHistoryVideo(1)})
		if !errors.Is(err, cause) || limited || prepared != nil || len(fake.calls) != 2 {
			t.Fatal("transient metadata failure silently accepted a partial provider page")
		}
	}
}

func TestMAXHistoryIncompleteMessageRetainsSafeImagePreview(t *testing.T) {
	message := tokenOnlyHistoryVideo(0)
	message.Attachments = []maxclient.HistoryAttachment{
		{Type: "image", Token: "photo-token", URL: "https://media.max.ru/photo.jpg", Complete: true},
		{Type: "image", Token: "unsafe-photo", URL: "https://127.0.0.1/private.jpg", Complete: true},
		{Type: "audio", Complete: false},
	}
	item, err := maxHistoryItem(message, 42)
	if err != nil || item.RoundTrip || len(item.Attachments) != 1 || item.Attachments[0].Type != store.PostAttachmentImage || item.Attachments[0].RemoteURL != message.Attachments[0].URL {
		t.Fatal("safe photo preview was discarded because another attachment was unsupported")
	}
}
