package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestContentDiscoverySamplesIncludeMediaOnlyHistoryAndExcludeLocalDraftEdits(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	storage, workspace, channel := openMAXHistoryStoreTest(t, "discovery-media-samples", 4)
	now := time.Now().UTC().Truncate(time.Microsecond)
	claim, err := storage.ClaimMAXHistoryImport(ctx, "test-owner", workspace.ID, channel.ID, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, err = storage.ApplyMAXHistoryPage(ctx, "test-owner", workspace.ID, claim.Generation, []MAXHistoryItem{
		{Title: "Видео без подписи", MessageID: "video-only", PublishedAt: now.Add(-time.Minute), RoundTrip: true,
			Raw: json.RawMessage(`{"private":"raw-only"}`), Attachments: []MAXHistoryAttachment{{Type: PostAttachmentVideo, ProviderToken: "private-video-token", RemoteURL: "https://cdn.example.com/private-video.mp4"}}},
		{Title: "Фото без подписи", MessageID: "images-only", PublishedAt: now.Add(-2 * time.Minute), RoundTrip: true,
			Attachments: []MAXHistoryAttachment{{Type: PostAttachmentImage, ProviderToken: "private-image-token-1", RemoteURL: "https://cdn.example.com/private-image-1.jpg"}, {Type: PostAttachmentImage, ProviderToken: "private-image-token-2", RemoteURL: "https://cdn.example.com/private-image-2.jpg"}}},
		{Title: "Подпись", Content: strings.Repeat("я", 600), MessageID: "caption", PublishedAt: now.Add(-3 * time.Minute), RoundTrip: true},
		{Title: "Старый", Content: "За пределами трёх примеров", MessageID: "older", PublishedAt: now.Add(-4 * time.Minute), RoundTrip: true},
	}, nil, true, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	dirty, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{Title: "Published", Content: "Реально отправленный текст", ChannelID: &channel.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storage.ClaimForPublishing(ctx, dirty.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = storage.MarkPublished(ctx, dirty.ID, "dirty-publication", ""); err != nil {
		t.Fatal(err)
	}
	local := "Секретный новый черновик ещё не отправлен"
	if _, err = storage.UpdatePost(ctx, dirty.ID, PostChanges{Content: &local}); err != nil {
		t.Fatal(err)
	}
	if _, err = storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{Title: "Draft", Content: "Обычный черновик", Status: PostStatusDraft, ChannelID: &channel.ID}); err != nil {
		t.Fatal(err)
	}
	samples, err := storage.ListContentDiscoverySamplesForWorkspace(ctx, "history-editor", workspace.ID, channel.ID)
	if err != nil || len(samples) != 3 {
		t.Fatalf("media-only samples not bounded/included: samples=%#v err=%v", samples, err)
	}
	if samples[0].Text != "" || samples[0].VideoCount != 1 || samples[0].ImageCount != 0 || samples[1].Text != "" || samples[1].ImageCount != 2 || samples[1].VideoCount != 0 || utf8.RuneCountInString(samples[2].Text) != 400 || !utf8.ValidString(samples[2].Text) {
		t.Fatalf("published captions, media counts or legacy projection deduplication lost: %#v", samples)
	}
	encoded, err := json.Marshal(samples)
	if err != nil || strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "raw-only") || strings.Contains(string(encoded), "https://") || strings.Contains(string(encoded), "черновик") {
		t.Fatalf("private media metadata/draft leaked into editorial projection: %s err=%v", encoded, err)
	}
	if _, err = storage.ListContentDiscoverySamplesForWorkspace(ctx, "foreign-user", workspace.ID, channel.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign user accessed published context: %v", err)
	}
	foreignWorkspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Foreign context"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storage.ListContentDiscoverySamplesForWorkspace(ctx, "test-owner", foreignWorkspace.ID, channel.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("channel crossed workspace context boundary: %v", err)
	}
}
