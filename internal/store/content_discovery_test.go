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
	if samples[2].CharacterCount != 600 || samples[2].Format != FormatMarkdown {
		t.Fatalf("discovery lost full-length or format metadata: %#v", samples[2])
	}
	styleSamples, err := storage.ListChannelStyleSamplesForWorkspace(ctx, "history-editor", workspace.ID, channel.ID)
	if err != nil || len(styleSamples) != 2 || styleSamples[0].Text != strings.Repeat("я", 600) ||
		styleSamples[0].CharacterCount != 600 || styleSamples[0].Format != FormatMarkdown || styleSamples[1].Text != "За пределами трёх примеров" {
		t.Fatalf("writing style included media-only/draft context or lost full text: %#v err=%v", styleSamples, err)
	}
	encoded, err := json.Marshal(samples)
	if err != nil || strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "raw-only") || strings.Contains(string(encoded), "https://") || strings.Contains(string(encoded), "черновик") {
		t.Fatalf("private media metadata/draft leaked into editorial projection: %s err=%v", encoded, err)
	}
	if _, err = storage.ListContentDiscoverySamplesForWorkspace(ctx, "foreign-user", workspace.ID, channel.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign user accessed published context: %v", err)
	}
	if _, err = storage.ListChannelStyleSamplesForWorkspace(ctx, "foreign-user", workspace.ID, channel.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign user accessed writing style: %v", err)
	}
	foreignWorkspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Foreign context"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storage.ListContentDiscoverySamplesForWorkspace(ctx, "test-owner", foreignWorkspace.ID, channel.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("channel crossed workspace context boundary: %v", err)
	}
	if _, err = storage.ListChannelStyleSamplesForWorkspace(ctx, "test-owner", foreignWorkspace.ID, channel.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("channel crossed workspace writing-style boundary: %v", err)
	}
}

func TestChannelStyleSamplesKeepThreeRecentFullLengthPublishedTexts(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	storage, workspace, channel := openMAXHistoryStoreTest(t, "channel-style-samples", 4)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for i, input := range []struct{ title, text, format string }{
		{"Свежий", strings.Repeat("🙂", 1800), FormatMarkdown},
		{"HTML", "<b>Текст канала</b>", FormatHTML},
		{"Третий", "Третий пример", FormatMarkdown},
		{"Старый", "Не входит в три примера", FormatMarkdown},
	} {
		publishedAt := now.Add(-time.Duration(i+1) * time.Minute)
		if _, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{
			Title: input.title, Content: input.text, Format: input.format, ChannelID: &channel.ID,
			Status: PostStatusPublished, PublishedAt: &publishedAt, MAXMessageID: "style-" + input.title,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// These fixture rows represent confirmed historical MAXPosty deliveries.
	rows, err := storage.ListPostsForWorkspace(ctx, "test-owner", workspace.ID, PostStatusPublished, &channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, post := range rows {
		if post.Status == PostStatusPublished {
			if _, err := storage.db.ExecContext(ctx, `UPDATE posts SET max_published_fingerprint=? WHERE workspace_id=? AND id=?`,
				publicationFingerprint(post), workspace.ID, post.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	samples, err := storage.ListChannelStyleSamplesForWorkspace(ctx, "test-owner", workspace.ID, channel.ID)
	if err != nil || len(samples) != 3 {
		t.Fatalf("style sample limit/order failed: %#v err=%v", samples, err)
	}
	if samples[0].Text != strings.Repeat("🙂", 1800) || samples[0].CharacterCount != 1800 ||
		samples[1].Text != "<b>Текст канала</b>" || samples[1].Format != FormatHTML ||
		samples[1].CharacterCount != utf8.RuneCountInString(samples[1].Text) || samples[2].Text != "Третий пример" {
		t.Fatalf("style samples lost Unicode length, full text, format or chronology: %#v", samples)
	}
}

func TestContentDiscoveryImportedDeliveredTextProofExcludesLocalEditsAndUnknownRows(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	storage, workspace, channel := openMAXHistoryStoreTest(t, "discovery-delivered-history", 5)
	now := time.Now().UTC().Truncate(time.Microsecond)
	sender := true
	claim, err := storage.ClaimMAXHistoryImport(ctx, "test-owner", workspace.ID, channel.ID, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	items := []MAXHistoryItem{
		{Title: "Точный оригинал", Content: strings.Repeat("я", 1800), MessageID: "known-clean", PublishedAt: now.Add(-time.Minute), RoundTrip: true, SenderIsBot: &sender},
		{Title: "Изменённый известный", Content: "Исходный текст известной публикации", MessageID: "known-dirty", PublishedAt: now.Add(-2 * time.Minute), RoundTrip: true, SenderIsBot: &sender},
		{Title: "Старый оригинал", Content: strings.Repeat("🙂", 901), MessageID: "legacy-clean", PublishedAt: now.Add(-3 * time.Minute), RoundTrip: true, SenderIsBot: &sender},
		{Title: "Изменённый старый", Content: "Исходный текст старой публикации", MessageID: "legacy-dirty", PublishedAt: now.Add(-4 * time.Minute), RoundTrip: true, SenderIsBot: &sender},
		{Title: "Оригинал без доказательства", Content: "История без исходной ревизии", MessageID: "legacy-unproven", PublishedAt: now.Add(-5 * time.Minute), RoundTrip: true, SenderIsBot: &sender},
	}
	if _, err := storage.ApplyMAXHistoryPage(ctx, "test-owner", workspace.ID, claim.Generation, items, nil, true, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, messageID := range []string{"legacy-clean", "legacy-dirty", "legacy-unproven"} {
		post := maxHistoryPostByMessageID(t, storage, workspace.ID, messageID)
		if _, err := storage.db.ExecContext(ctx, `UPDATE posts SET max_published_fingerprint='' WHERE workspace_id=? AND id=?`, workspace.ID, post.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, messageID := range []string{"known-dirty", "legacy-dirty"} {
		post := maxHistoryPostByMessageID(t, storage, workspace.ID, messageID)
		localText := "Секретный локальный текст, ещё не отправленный в MAX"
		if _, err := storage.UpdatePostForWorkspaceIfUnchanged(ctx, "history-editor", workspace.ID, post, PostChanges{Content: &localText}); err != nil {
			t.Fatal(err)
		}
	}
	unproven := maxHistoryPostByMessageID(t, storage, workspace.ID, "legacy-unproven")
	if _, err := storage.db.ExecContext(ctx, `UPDATE posts SET current_revision_id=NULL WHERE workspace_id=? AND id=?`, workspace.ID, unproven.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.ExecContext(ctx, `DELETE FROM post_revisions WHERE workspace_id=? AND post_id=?`, workspace.ID, unproven.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{Title: "Неизвестная локальная версия", Content: "Недоказанный локальный текст", ChannelID: &channel.ID, Status: PostStatusPublished}); err != nil {
		t.Fatal(err)
	}
	style, err := storage.ListChannelStyleSamplesForWorkspace(ctx, "history-editor", workspace.ID, channel.ID)
	if err != nil || len(style) != 2 || style[0].Text != items[0].Content || style[0].CharacterCount != 1800 ||
		style[1].Text != items[2].Content || style[1].CharacterCount != 901 {
		t.Fatalf("delivered style proof accepted unpublished text or dropped clean original imports: %#v err=%v", style, err)
	}
	discovery, err := storage.ListContentDiscoverySamplesForWorkspace(ctx, "history-editor", workspace.ID, channel.ID)
	if err != nil || len(discovery) != 2 || discovery[0].CharacterCount != 1800 || discovery[1].CharacterCount != 901 ||
		utf8.RuneCountInString(discovery[0].Text) != 400 || utf8.RuneCountInString(discovery[1].Text) != 400 {
		t.Fatalf("discovery proof/short snippets lost original lengths: %#v err=%v", discovery, err)
	}
}
