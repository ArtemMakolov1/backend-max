package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
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
		if _, err := storage.CreatePost(t.Context(), store.Post{UserID: "description-owner", WorkspaceID: workspace.ID, ChannelID: &channel.ID,
			Title: fmt.Sprint(i), Content: fmt.Sprint(i) + strings.Repeat("я", 900), Format: store.FormatMarkdown, Status: store.PostStatusPublished}); err != nil {
			t.Fatal(err)
		}
	}
	for _, post := range []store.Post{
		{ChannelID: &other.ID, Content: "Не передавать чужой канал", Status: store.PostStatusPublished},
		{ChannelID: &channel.ID, Content: "Не передавать секретный черновик", Status: store.PostStatusDraft},
	} {
		post.UserID, post.WorkspaceID, post.Title, post.Format = "description-owner", workspace.ID, "Тест", store.FormatMarkdown
		if _, err := storage.CreatePost(t.Context(), post); err != nil {
			t.Fatal(err)
		}
	}
	kit, err := storage.GetWorkspaceBrandKit(t.Context(), "description-owner", workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.UpdateWorkspaceBrandKit(t.Context(), "description-owner", workspace.ID, store.WorkspaceBrandKitUpdate{
		ExpectedVersion: kit.Version, BrandProfile: store.BrandProfile{Tone: "Простой", Audience: "Любители котов", ForbiddenWords: []string{"хайп"}},
	}); err != nil {
		t.Fatal(err)
	}
	quotaCalls := 0
	_, err = application.DiscoverContentForWorkspaceWithBeforeGenerate(t.Context(), "description-owner", workspace.ID, &channel.ID,
		openairesearch.DiscoverContentRequest{ContentKind: "meme", ChannelTitle: "Injected", RecentPosts: []string{"Injected private data"}}, func() error {
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
	if utf8.RuneCountInString(request.ChannelDescription) != openairesearch.MaxDiscoveryDescriptionRunes || !utf8.ValidString(request.ChannelDescription) || len(request.RecentPosts) != openairesearch.MaxDiscoverySamples {
		t.Fatalf("unbounded channel context: %#v", request)
	}
	for index, text := range request.RecentPosts {
		if utf8.RuneCountInString(text) != openairesearch.MaxDiscoverySampleRunes || !strings.HasPrefix(text, fmt.Sprint(4-index)) || strings.Contains(text, "Не передавать") || strings.Contains(text, "Injected") {
			t.Fatalf("unscoped/unbounded sample %d: %s", index, text)
		}
	}
	saved, err := storage.GetChannelForWorkspace(t.Context(), "description-owner", workspace.ID, channel.ID)
	if err != nil || saved.Description != channel.Description {
		t.Fatal("discovery changed stored channel metadata")
	}
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
