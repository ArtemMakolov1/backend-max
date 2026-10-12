package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

type discoveryTestClient struct {
	requests []openairesearch.DiscoverContentRequest
}

func (*discoveryTestClient) Generate(context.Context, openairesearch.Request) (openairesearch.Result, error) {
	return openairesearch.Result{}, errors.New("advanced research was not expected")
}

func (f *discoveryTestClient) DiscoverContent(_ context.Context, request openairesearch.DiscoverContentRequest) (openairesearch.DiscoverContentResult, error) {
	f.requests = append(f.requests, request)
	return openairesearch.DiscoverContentResult{Topic: request.Topic, ContentKind: request.ContentKind, Cards: []openairesearch.ContentCard{}}, nil
}

func TestContentDiscoveryUsesOwnedBoundedChannelAndPublishedContextBeforeQuota(t *testing.T) {
	fake := &discoveryTestClient{}
	application, storage, workspace := newChannelDescriptionFixture(t, fake)
	activatePaidWorkspaceForTest(t, storage, "description-owner", workspace.ID)
	channel, err := storage.CreateChannel(t.Context(), store.Channel{UserID: "description-owner", WorkspaceID: workspace.ID,
		VerifiedMAXOwnerID: "100", MAXChatID: "-100", Title: "Весёлые коты", Description: strings.Repeat("я", 16000), IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	other, err := storage.CreateChannel(t.Context(), store.Channel{UserID: "description-owner", WorkspaceID: workspace.ID,
		VerifiedMAXOwnerID: "100", MAXChatID: "-101", Title: "Другой канал", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		createDeliveredDiscoveryTestPost(t, storage, store.Post{UserID: "description-owner", WorkspaceID: workspace.ID, ChannelID: &channel.ID,
			Title: fmt.Sprint(i), Content: fmt.Sprint(i) + strings.Repeat("я", 900), Format: store.FormatMarkdown})
	}
	for _, post := range []store.Post{
		{ChannelID: &other.ID, Content: "Не передавать чужой канал", Status: store.PostStatusPublished},
		{ChannelID: &channel.ID, Content: "Не передавать секретный черновик", Status: store.PostStatusDraft},
	} {
		post.UserID, post.WorkspaceID, post.Title, post.Format = "description-owner", workspace.ID, "Тест", store.FormatMarkdown
		if post.Status == store.PostStatusPublished {
			createDeliveredDiscoveryTestPost(t, storage, post)
			continue
		}
		if _, err := storage.CreatePost(t.Context(), post); err != nil {
			t.Fatal(err)
		}
	}
	dirty, err := storage.CreatePost(t.Context(), store.Post{UserID: "description-owner", WorkspaceID: workspace.ID, ChannelID: &channel.ID,
		Title: "Опубликованный", Content: "Реально отправленный текст", Format: store.FormatMarkdown})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ClaimForPublishing(t.Context(), dirty.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.MarkPublished(t.Context(), dirty.ID, "dirty-discovery-style", ""); err != nil {
		t.Fatal(err)
	}
	unpublished := "Не передавать неотправленные локальные изменения"
	if _, err := storage.UpdatePost(t.Context(), dirty.ID, store.PostChanges{Content: &unpublished}); err != nil {
		t.Fatal(err)
	}
	kit, err := storage.GetWorkspaceBrandKit(t.Context(), "description-owner", workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.UpdateWorkspaceBrandKit(t.Context(), "description-owner", workspace.ID, store.WorkspaceBrandKitUpdate{
		ExpectedVersion: kit.Version, BrandProfile: store.BrandProfile{
			Tone: "Простой", Audience: "Любители котов", ForbiddenWords: []string{"хайп"},
			CTA: "Подписывайтесь", VisualStyle: "Синяя графика", ExamplePosts: []string{"Сохранённый пример бренда"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	quotaCalls := 0
	_, err = application.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, &channel.ID,
		openairesearch.DiscoverContentRequest{
			ContentKind: "meme", ChannelTitle: "Injected", RecentPosts: []openairesearch.DiscoveryPublishedPost{{Text: "Injected private data"}},
			ChannelPosts: []openairesearch.ChannelPostStyle{{Text: "Injected writing style", Format: store.FormatHTML, CharacterCount: 3000}},
			ExamplePosts: []string{"Injected style example"}, ExamplePostsOverride: true, CTA: "Injected CTA", VisualStyle: "Injected visuals",
			Tone: "Injected tone", Audience: "Injected audience", ForbiddenWords: []string{"Injected forbidden word"},
		}, func() error {
			quotaCalls++
			if len(fake.requests) != 0 {
				t.Fatal("provider started before the quota reservation")
			}
			return nil
		})
	if err != nil || quotaCalls != 1 || len(fake.requests) != 1 {
		t.Fatalf("discovery/quota calls: quota=%d requests=%#v err=%v", quotaCalls, fake.requests, err)
	}
	request := fake.requests[0]
	if request.Topic != channel.Title || request.ChannelTitle != channel.Title || request.Tone != "Простой" || request.Audience != "Любители котов" || len(request.ForbiddenWords) != 1 || request.ForbiddenWords[0] != "хайп" {
		t.Fatalf("owned channel/brand context not applied: %#v", request)
	}
	if request.CTA != "Подписывайтесь" || request.VisualStyle != "Синяя графика" || request.ExamplePostsOverride ||
		len(request.ExamplePosts) != 1 || request.ExamplePosts[0] != "Сохранённый пример бренда" {
		t.Fatalf("workspace policy/example defaults were overwritten or got style priority: %#v", request)
	}
	if utf8.RuneCountInString(request.ChannelDescription) != openairesearch.MaxDiscoveryDescriptionRunes || !utf8.ValidString(request.ChannelDescription) || len(request.RecentPosts) != openairesearch.MaxDiscoverySamples {
		t.Fatalf("unbounded channel context: %#v", request)
	}
	for index, sample := range request.RecentPosts {
		if utf8.RuneCountInString(sample.Text) != openairesearch.MaxDiscoverySampleRunes || !strings.HasPrefix(sample.Text, fmt.Sprint(4-index)) || strings.Contains(sample.Text, "Не передавать") || strings.Contains(sample.Text, "Injected") || len(sample.Media) != 0 || sample.Format != store.FormatMarkdown || sample.CharacterCount != 901 {
			t.Fatalf("unscoped/unbounded sample %d: %#v", index, sample)
		}
	}
	if len(request.ChannelPosts) != openairesearch.MaxChannelStylePosts {
		t.Fatalf("writing style sample count = %d", len(request.ChannelPosts))
	}
	for index, sample := range request.ChannelPosts {
		if sample.Text != fmt.Sprint(4-index)+strings.Repeat("я", 900) || sample.CharacterCount != 901 || sample.Format != store.FormatMarkdown {
			t.Fatalf("writing style used truncated discovery text or unowned/unpublished context: %#v", sample)
		}
	}
	saved, err := storage.GetChannelForWorkspace(t.Context(), "description-owner", workspace.ID, channel.ID)
	if err != nil || saved.Description != channel.Description {
		t.Fatal("discovery changed stored channel metadata")
	}
	_, err = application.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, &other.ID,
		openairesearch.DiscoverContentRequest{}, nil)
	if err != nil || len(fake.requests) != 2 {
		t.Fatalf("second channel discovery failed: %#v err=%v", fake.requests, err)
	}
	otherRequest := fake.requests[1]
	if otherRequest.ChannelTitle != other.Title || len(otherRequest.ChannelPosts) != 1 || len(otherRequest.RecentPosts) != 1 ||
		otherRequest.ChannelPosts[0].Text != "Не передавать чужой канал" || otherRequest.RecentPosts[0].Text != "Не передавать чужой канал" {
		t.Fatalf("channel A writing style crossed into channel B: %#v", otherRequest)
	}
}

func TestContentDiscoveryChannelTemplatePreservesEditorialPolicyAndStylePriority(t *testing.T) {
	fake := &discoveryTestClient{}
	application, storage, workspace := newChannelDescriptionFixture(t, fake)
	activatePaidWorkspaceForTest(t, storage, "description-owner", workspace.ID)
	channel, err := storage.CreateChannel(t.Context(), store.Channel{UserID: "description-owner", WorkspaceID: workspace.ID,
		VerifiedMAXOwnerID: "100", MAXChatID: "-140", Title: "Авторский канал", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	longText := "<b>Авторский пример</b>\n\n" + strings.Repeat("Длинный авторский текст. ", 90)
	createDeliveredDiscoveryTestPost(t, storage, store.Post{UserID: "description-owner", WorkspaceID: workspace.ID, ChannelID: &channel.ID,
		Title: "Текст канала", Content: longText, Format: store.FormatHTML})
	kit, err := storage.GetWorkspaceBrandKit(t.Context(), "description-owner", workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.UpdateWorkspaceBrandKit(t.Context(), "description-owner", workspace.ID, store.WorkspaceBrandKitUpdate{
		ExpectedVersion: kit.Version, BrandProfile: store.BrandProfile{Tone: "Базовый", Audience: "Базовая аудитория", CTA: "Базовый CTA",
			VisualStyle: "Базовая графика", ForbiddenWords: []string{"Базовое слово"}, ExamplePosts: []string{"Базовый пример"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateChannelTemplate(t.Context(), "description-owner", workspace.ID, store.ChannelTemplateCreate{
		ChannelID: &channel.ID, Name: "Авторский", BrandProfile: store.BrandProfile{Tone: "Авторский спокойный", Audience: "Постоянные читатели",
			CTA: "Обсудите в комментариях", VisualStyle: "Карандашные рисунки", ForbiddenWords: []string{"кликбейт"}, ExamplePosts: []string{"Образец автора"}},
	}); err != nil {
		t.Fatal(err)
	}
	_, err = application.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, &channel.ID,
		openairesearch.DiscoverContentRequest{}, nil)
	if err != nil || len(fake.requests) != 1 {
		t.Fatalf("template discovery failed: %#v err=%v", fake.requests, err)
	}
	request := fake.requests[0]
	if request.Tone != "Авторский спокойный" || request.Audience != "Постоянные читатели" || request.CTA != "Обсудите в комментариях" ||
		request.VisualStyle != "Карандашные рисунки" || strings.Join(request.ForbiddenWords, ",") != "кликбейт" ||
		strings.Join(request.ExamplePosts, ",") != "Образец автора" || !request.ExamplePostsOverride {
		t.Fatalf("channel template lost policy fields or explicit style priority: %#v", request)
	}
	if len(request.ChannelPosts) != 1 || request.ChannelPosts[0].Format != store.FormatHTML ||
		utf8.RuneCountInString(request.ChannelPosts[0].Text) > openairesearch.MaxChannelStyleSampleRunes ||
		!strings.HasPrefix(longText, request.ChannelPosts[0].Text) || request.ChannelPosts[0].CharacterCount != utf8.RuneCountInString(strings.TrimSpace(longText)) ||
		request.ChannelPosts[0].CharacterCount <= utf8.RuneCountInString(request.ChannelPosts[0].Text) {
		t.Fatalf("template style lost full original length, bounded text or HTML format: %#v", request.ChannelPosts)
	}
}

func createDeliveredDiscoveryTestPost(t *testing.T, storage *store.Store, post store.Post) store.Post {
	t.Helper()
	post.Status = store.PostStatusDraft
	created, err := storage.CreatePost(t.Context(), post)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ClaimForPublishing(t.Context(), created.ID); err != nil {
		t.Fatal(err)
	}
	delivered, err := storage.MarkPublished(t.Context(), created.ID, fmt.Sprintf("delivered-discovery-%d", created.ID), "")
	if err != nil {
		t.Fatal(err)
	}
	return delivered
}

func TestContentDiscoveryRejectsInvalidAndForeignTargetsBeforeQuota(t *testing.T) {
	fake := &discoveryTestClient{}
	application, _, workspace := newChannelDescriptionFixture(t, fake)
	missingID := int64(999999)
	for _, test := range []struct {
		request   openairesearch.DiscoverContentRequest
		channelID *int64
	}{
		{request: openairesearch.DiscoverContentRequest{}},
		{request: openairesearch.DiscoverContentRequest{Topic: "Коты", ContentKind: "unsupported"}},
		{request: openairesearch.DiscoverContentRequest{Topic: "Коты"}, channelID: &missingID},
	} {
		charged := false
		_, err := application.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, test.channelID,
			test.request, func() error { charged = true; return nil })
		if err == nil || charged || len(fake.requests) != 0 {
			t.Fatalf("invalid/foreign target consumed upstream/quota: charged=%v requests=%d err=%v", charged, len(fake.requests), err)
		}
	}
	quotaErr := errors.New("quota rejected")
	_, err := application.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, nil,
		openairesearch.DiscoverContentRequest{Topic: "Коты"}, func() error { return quotaErr })
	if !errors.Is(err, quotaErr) || len(fake.requests) != 0 {
		t.Fatalf("quota denial reached provider: requests=%d err=%v", len(fake.requests), err)
	}
}

func TestContentDiscoverySupportsChannelOnlyWithOneCharacterTitle(t *testing.T) {
	fake := &discoveryTestClient{}
	application, storage, workspace := newChannelDescriptionFixture(t, fake)
	activatePaidWorkspaceForTest(t, storage, "description-owner", workspace.ID)
	channel, err := storage.CreateChannel(t.Context(), store.Channel{UserID: "description-owner", WorkspaceID: workspace.ID,
		VerifiedMAXOwnerID: "100", MAXChatID: "-100", Title: "Я", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = application.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, &channel.ID,
		openairesearch.DiscoverContentRequest{}, nil)
	if err != nil || len(fake.requests) != 1 || fake.requests[0].Topic != "Канал Я" || fake.requests[0].ChannelTitle != "Я" {
		t.Fatalf("legal channel-only request rejected: requests=%#v err=%v", fake.requests, err)
	}
}

func TestContentDiscoveryAutoPassesOnlyOwnedMediaTypesForPostsWithoutCaptions(t *testing.T) {
	fake := &discoveryTestClient{}
	application, storage, workspace := newChannelDescriptionFixture(t, fake)
	activatePaidWorkspaceForTest(t, storage, "description-owner", workspace.ID)
	channel, err := storage.CreateChannel(t.Context(), store.Channel{UserID: "description-owner", WorkspaceID: workspace.ID,
		VerifiedMAXOwnerID: "100", MAXChatID: "-100", Title: "Канал", Description: "Юмор и короткие ролики", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(t.Context(), store.Post{UserID: "description-owner", WorkspaceID: workspace.ID, ChannelID: &channel.ID, Title: "Без подписи"})
	if err != nil {
		t.Fatal(err)
	}
	for _, media := range []struct{ kind, key, mime string }{{store.PostAttachmentVideo, "private-video-key.mp4", "video/mp4"}, {store.PostAttachmentImage, "private-image-key.jpg", "image/jpeg"}} {
		reservation, err := storage.ReserveMedia(t.Context(), "description-owner", media.key, 100, store.MediaLimits{MaxFiles: 10, MaxBytes: 10000}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if err = storage.CompleteMediaReservation(t.Context(), reservation, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		post, err = storage.AddPostAttachmentForUser(t.Context(), "description-owner", post.ID, store.PostAttachment{Type: media.kind, StorageKey: media.key, MIMEType: media.mime, SizeBytes: 100, ProviderToken: "private-upload-token", Position: -1})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = storage.ClaimForPublishing(t.Context(), post.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = storage.MarkPublished(t.Context(), post.ID, "media-only-message", ""); err != nil {
		t.Fatal(err)
	}
	_, err = application.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, &channel.ID, openairesearch.DiscoverContentRequest{}, nil)
	if err != nil || len(fake.requests) != 1 {
		t.Fatalf("media-only channel discovery failed: requests=%#v err=%v", fake.requests, err)
	}
	request := fake.requests[0]
	if request.ContentKind != "auto" || request.ChannelDescription != channel.Description || len(request.RecentPosts) != 1 || request.RecentPosts[0].Text != "" || len(request.RecentPosts[0].Media) != 2 {
		t.Fatalf("auto/media-only context lost: %#v", request)
	}
	encoded, err := json.Marshal(request.RecentPosts)
	if err != nil || strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "/media/") || strings.Contains(string(encoded), "http") || !strings.Contains(string(encoded), `"type":"video","count":1`) || !strings.Contains(string(encoded), `"type":"image","count":1`) {
		t.Fatalf("media credentials leaked or real type/count lost: %s err=%v", encoded, err)
	}
}
