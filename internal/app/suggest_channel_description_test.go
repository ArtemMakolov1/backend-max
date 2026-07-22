package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/openairesearch"
	"maxpilot/backend/internal/store"
)

type fakeChannelDescriptionSuggester struct {
	mu       sync.Mutex
	requests []openairesearch.SuggestChannelDescriptionRequest
	result   openairesearch.SuggestChannelDescriptionResult
}

func TestChannelDiagnosticsReportsChangeChatInfoSeparately(t *testing.T) {
	t.Parallel()
	info := maxclient.ChatInfo{ChatID: "-1", Type: "channel", Status: "active"}
	membership := maxclient.Membership{IsAdmin: true, Permissions: []maxclient.Permission{
		maxclient.PermissionReadAllMessages, maxclient.PermissionWrite,
	}}
	diagnostics := channelDiagnostics(info, membership)
	if diagnostics.CanChangeInfo || !diagnostics.CanPublish {
		t.Fatalf("diagnostics without change_chat_info = %#v", diagnostics)
	}
	membership.Permissions = append(membership.Permissions, maxclient.PermissionChangeChatInfo)
	if diagnostics = channelDiagnostics(info, membership); !diagnostics.CanChangeInfo {
		t.Fatalf("diagnostics with change_chat_info = %#v", diagnostics)
	}
}

func (*fakeChannelDescriptionSuggester) Generate(context.Context, openairesearch.Request) (openairesearch.Result, error) {
	return openairesearch.Result{}, errors.New("research generation is not expected")
}

func (f *fakeChannelDescriptionSuggester) SuggestChannelDescription(
	_ context.Context, request openairesearch.SuggestChannelDescriptionRequest,
) (openairesearch.SuggestChannelDescriptionResult, error) {
	f.mu.Lock()
	f.requests = append(f.requests, request)
	f.mu.Unlock()
	return f.result, nil
}

func newChannelDescriptionFixture(t *testing.T, research ResearchClient) (*App, *store.Store, store.Workspace) {
	t.Helper()
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "channel-description.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	if err := storage.UpsertUser(ctx, store.User{ID: "description-owner", DisplayName: "Owner"}); err != nil {
		t.Fatal(err)
	}
	workspaces, err := storage.ListWorkspaces(ctx, "description-owner")
	if err != nil {
		t.Fatal(err)
	}
	var personal store.Workspace
	for _, access := range workspaces {
		if access.Workspace.IsPersonal {
			personal = access.Workspace
			break
		}
	}
	if personal.ID == "" {
		t.Fatal("personal workspace was not created")
	}
	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(storage, mediaStore, nil, nil, research, logger), storage, personal
}

func TestSuggestChannelDescriptionUsesAuthoritativeChannelAndEightNewestPosts(t *testing.T) {
	fake := &fakeChannelDescriptionSuggester{result: openairesearch.SuggestChannelDescriptionResult{
		Suggestions: []openairesearch.ChannelDescriptionSuggestion{
			{Style: "concise", Label: "Кратко", Text: "Описание."},
		},
	}}
	application, storage, workspace := newChannelDescriptionFixture(t, fake)
	channel, err := storage.CreateChannel(context.Background(), store.Channel{
		UserID: "description-owner", WorkspaceID: workspace.ID, VerifiedMAXOwnerID: "100",
		MAXChatID: "-100", Title: "Авторитетное название", Description: "Описание из MAX",
		IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := storage.CreateChannel(context.Background(), store.Channel{
		UserID: "description-owner", WorkspaceID: workspace.ID, VerifiedMAXOwnerID: "100",
		MAXChatID: "-101", Title: "Другой канал", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreatePost(context.Background(), store.Post{
		UserID: "description-owner", WorkspaceID: workspace.ID, ChannelID: &foreign.ID,
		Title: "Чужой", Content: "Этот текст не должен попасть в запрос.", Format: store.FormatMarkdown,
		Status: store.PostStatusPublished,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		content := fmt.Sprintf("Пост %02d", i)
		if i == 8 {
			content = "   "
		}
		if _, err := storage.CreatePost(context.Background(), store.Post{
			UserID: "description-owner", WorkspaceID: workspace.ID, ChannelID: &channel.ID,
			Title: content, Content: content, Format: store.FormatMarkdown, Status: store.PostStatusPublished,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := storage.CreatePost(context.Background(), store.Post{
		UserID: "description-owner", WorkspaceID: workspace.ID, ChannelID: &channel.ID,
		Title: "Черновик", Content: "Секретный черновик не отправлять.", Format: store.FormatMarkdown,
		Status: store.PostStatusDraft,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = application.SuggestChannelDescriptionForUser(
		context.Background(), "description-owner", channel.ID,
		openairesearch.SuggestChannelDescriptionRequest{Context: "Новый контекст", CurrentDescription: "Черновик пользователя"})
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 1 {
		t.Fatalf("requests = %#v", fake.requests)
	}
	request := fake.requests[0]
	if request.ChannelTitle != "Авторитетное название" || request.ChannelDescription != "Описание из MAX" ||
		request.Context != "Новый контекст" || request.CurrentDescription != "Черновик пользователя" {
		t.Fatalf("request metadata = %#v", request)
	}
	if len(request.Posts) != openairesearch.MaxSuggestChannelDescriptionPosts {
		t.Fatalf("posts = %#v", request.Posts)
	}
	if request.Posts[0].Text != "Пост 09" || request.Posts[1].Text != "Пост 07" {
		t.Fatalf("newest post order = %#v", request.Posts)
	}
	for _, post := range request.Posts {
		if post.Text == "Этот текст не должен попасть в запрос." || post.Text == "Секретный черновик не отправлять." {
			t.Fatal("foreign or unpublished post leaked into AI material")
		}
	}
}

func TestSuggestChannelDescriptionRejectsForeignTenantBeforeAI(t *testing.T) {
	fake := &fakeChannelDescriptionSuggester{}
	application, storage, workspace := newChannelDescriptionFixture(t, fake)
	channel, err := storage.CreateChannel(context.Background(), store.Channel{
		UserID: "description-owner", WorkspaceID: workspace.ID, VerifiedMAXOwnerID: "100",
		MAXChatID: "-200", Title: "Канал", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.SuggestChannelDescriptionForUser(
		context.Background(), "foreign-user", channel.ID, openairesearch.SuggestChannelDescriptionRequest{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign request error = %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 0 {
		t.Fatalf("foreign request reached AI: %#v", fake.requests)
	}
}
