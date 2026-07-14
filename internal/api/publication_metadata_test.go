package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

func TestPublicationMetadataPinAndBoundedHistoryAPI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "publication-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	channel, err := storage.CreateChannel(ctx, store.Channel{
		VerifiedMAXOwnerID: "test-max-owner", MAXChatID: "-301", Title: "Channel", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(ctx, store.Post{
		Title: "Published", Content: "body", Format: store.FormatMarkdown, Status: store.PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: "mid.api",
	})
	if err != nil {
		t.Fatal(err)
	}
	views := int64(25)
	fake := &claimWebhookMAX{
		chat: maxclient.ChatInfo{ChatID: "-301", OwnerID: "test-max-owner", Type: "channel", Status: "active", Title: "Channel"},
		membership: maxclient.Membership{IsAdmin: true, Permissions: []maxclient.Permission{
			maxclient.PermissionReadAllMessages, maxclient.PermissionWrite, maxclient.PermissionPinMessage,
		}},
		message: maxclient.Message{MessageID: post.MAXMessageID, ChatID: "-301", URL: "https://max.ru/channel/api", Views: &views},
	}
	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application := app.New(storage, mediaStore, fake, nil, nil, logger)
	handler := withTestSession(t, storage,
		New(application, logger, "http://localhost:4321", "", AuthOptions{YandexClient: &fakeYandexOAuth{}}).Handler(), "test-owner")

	post = performPostRequest(t, handler, http.MethodPost, "/api/v1/posts/"+postID(post.ID)+"/sync-max", "", http.StatusOK)
	if post.MAXMessageURL != fake.message.URL || post.MAXViews == nil || *post.MAXViews != views || post.MAXIsPinned || post.MAXStatsSyncedAt == nil {
		t.Fatalf("sync response = %#v", post)
	}
	repeated := performJSONRequest(handler, http.MethodPost, "/api/v1/posts/"+postID(post.ID)+"/sync-max", "")
	if repeated.Code != http.StatusConflict {
		t.Fatalf("repeated sync status = %d, body=%s", repeated.Code, repeated.Body.String())
	}

	post = performPostRequest(t, handler, http.MethodPost, "/api/v1/posts/"+postID(post.ID)+"/pin", "", http.StatusOK)
	if !post.MAXIsPinned || fake.pinRuns != 1 {
		t.Fatalf("pin response = %#v, calls=%d", post, fake.pinRuns)
	}
	fake.pinnedMessage = &maxclient.Message{MessageID: post.MAXMessageID, ChatID: "-301"}
	post = performPostRequest(t, handler, http.MethodDelete, "/api/v1/posts/"+postID(post.ID)+"/pin", "", http.StatusOK)
	if post.MAXIsPinned || fake.unpinRuns != 1 {
		t.Fatalf("unpin response = %#v, calls=%d", post, fake.unpinRuns)
	}

	historyResponse := performJSONRequest(handler, http.MethodGet,
		"/api/v1/posts/"+postID(post.ID)+"/view-history?limit=1", "")
	if historyResponse.Code != http.StatusOK {
		t.Fatalf("history status = %d, body=%s", historyResponse.Code, historyResponse.Body.String())
	}
	var history []store.PostViewSnapshot
	if err := json.Unmarshal(historyResponse.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].PostID != post.ID || history[0].MAXMessageID != post.MAXMessageID || history[0].Views != views {
		t.Fatalf("history = %#v", history)
	}
	invalidLimit := performJSONRequest(handler, http.MethodGet,
		"/api/v1/posts/"+postID(post.ID)+"/view-history?limit=1001", "")
	if invalidLimit.Code != http.StatusBadRequest {
		t.Fatalf("invalid history limit status = %d, body=%s", invalidLimit.Code, invalidLimit.Body.String())
	}
}
