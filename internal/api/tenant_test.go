package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

func TestTenantAPIReturnsNotFoundForForeignResources(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "api-tenant.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	for _, userID := range []string{"owner-a", "owner-b"} {
		if err := storage.UpsertUser(ctx, store.User{ID: userID, Login: userID, DisplayName: userID}); err != nil {
			t.Fatal(err)
		}
	}
	channel, err := storage.CreateChannel(ctx, store.Channel{
		UserID: "owner-a", VerifiedMAXOwnerID: "max-owner-a", MAXChatID: "81001",
		Title: "Private A", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(ctx, store.Post{
		UserID: "owner-a", Title: "Private post A", Content: "secret", Format: store.FormatMarkdown,
		Status: store.PostStatusDraft, ChannelID: &channel.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	claim := store.ChannelClaim{
		ID: "foreign-claim", TokenHash: strings.Repeat("a", 64), UserID: "owner-a", MAXChatID: "81001",
		RequestedTitle: "Private A", RequesterLabel: "owner-a", ComparisonCode: "314159",
		CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := storage.CreateChannelClaim(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if err := storage.RegisterMedia(ctx, "owner-a", "private-a.png", now); err != nil {
		t.Fatal(err)
	}

	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application := app.New(storage, mediaStore, nil, nil, nil, logger)
	handler := withTestSession(t, storage,
		New(application, logger, "http://localhost:4321", "webhook-secret", AuthOptions{
			YandexClient: &fakeYandexOAuth{},
		}).Handler(), "owner-b")

	foreignRequests := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, fmt.Sprintf("/api/v1/channels/%d", channel.ID), ""},
		{http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", channel.ID), `{"active":false}`},
		{http.MethodDelete, fmt.Sprintf("/api/v1/channels/%d", channel.ID), ""},
		{http.MethodPost, fmt.Sprintf("/api/v1/channels/%d/test", channel.ID), `{}`},
		{http.MethodGet, fmt.Sprintf("/api/v1/posts/%d", post.ID), ""},
		{http.MethodPatch, fmt.Sprintf("/api/v1/posts/%d", post.ID), `{"title":"stolen"}`},
		{http.MethodDelete, fmt.Sprintf("/api/v1/posts/%d", post.ID), ""},
		{http.MethodPost, fmt.Sprintf("/api/v1/posts/%d/duplicate", post.ID), `{}`},
		{http.MethodPost, fmt.Sprintf("/api/v1/posts/%d/schedule", post.ID), `{"scheduled_at":"2099-01-01T00:00:00Z"}`},
		{http.MethodPost, fmt.Sprintf("/api/v1/posts/%d/cancel-schedule", post.ID), `{}`},
		{http.MethodPost, fmt.Sprintf("/api/v1/posts/%d/publish", post.ID), `{}`},
		{http.MethodPost, fmt.Sprintf("/api/v1/posts/%d/sync", post.ID), `{}`},
		{http.MethodPost, fmt.Sprintf("/api/v1/posts/%d/update-published", post.ID), `{}`},
		{http.MethodPost, fmt.Sprintf("/api/v1/posts/%d/sync-max", post.ID), `{}`},
		{http.MethodPost, fmt.Sprintf("/api/v1/posts/%d/pin", post.ID), `{}`},
		{http.MethodDelete, fmt.Sprintf("/api/v1/posts/%d/pin", post.ID), ""},
		{http.MethodGet, fmt.Sprintf("/api/v1/posts/%d/view-history", post.ID), ""},
		{http.MethodPost, fmt.Sprintf("/api/v1/posts/%d/delete-publication", post.ID), `{}`},
		{http.MethodDelete, fmt.Sprintf("/api/v1/posts/%d/publication", post.ID), ""},
		{http.MethodPost, fmt.Sprintf("/api/v1/posts/%d/generate-image", post.ID), `{"prompt":"stolen"}`},
		{http.MethodGet, "/api/v1/channels/connect/" + claim.ID, ""},
		{http.MethodGet, "/media/private-a.png", ""},
	}
	for _, test := range foreignRequests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			response := performJSONRequest(handler, test.method, test.path, test.body)
			if response.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body=%s", response.Code, response.Body.String())
			}
		})
	}

	for _, path := range []string{
		fmt.Sprintf("/api/v1/posts/%d/image", post.ID),
		fmt.Sprintf("/api/v1/media?post_id=%d", post.ID),
	} {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, path, &body)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("POST %s status = %d, want 404; body=%s", path, response.Code, response.Body.String())
		}
	}

	response := performJSONRequest(handler, http.MethodPost, "/api/v1/posts",
		fmt.Sprintf(`{"title":"cross tenant","content":"body","format":"markdown","channel_id":%d}`, channel.ID))
	if response.Code != http.StatusNotFound {
		t.Fatalf("foreign channel assignment status = %d, want 404; body=%s", response.Code, response.Body.String())
	}
	response = performJSONRequest(handler, http.MethodPost, "/api/v1/posts",
		`{"title":"cross media","content":"body","format":"markdown","image_url":"http://localhost:8080/media/private-a.png"}`)
	if response.Code != http.StatusNotFound {
		t.Fatalf("foreign media assignment status = %d, want 404; body=%s", response.Code, response.Body.String())
	}

	for _, path := range []string{"/api/v1/channels", "/api/v1/posts"} {
		response = performJSONRequest(handler, http.MethodGet, path, "")
		if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "[]" {
			t.Fatalf("GET %s = %d %s, want empty tenant list", path, response.Code, response.Body.String())
		}
	}
	if _, err := storage.GetChannelForUser(ctx, "owner-a", channel.ID); err != nil {
		t.Fatalf("foreign requests mutated owner channel: %v", err)
	}
	storedPost, err := storage.GetPostForUser(ctx, "owner-a", post.ID)
	if err != nil || storedPost.Title != "Private post A" || storedPost.Status != store.PostStatusDraft {
		t.Fatalf("foreign requests mutated owner post: %#v, %v", storedPost, err)
	}
}

func TestSafeRequesterLabelRemovesControlsAndCapsRunes(t *testing.T) {
	t.Parallel()
	value := "  alice\n\t\u202e\u2028" + strings.Repeat("я", 200) + "  "
	label := safeRequesterLabel(value)
	if strings.ContainsAny(label, "\r\n\t\u202e\u2028") {
		t.Fatalf("requester label still contains control characters: %q", label)
	}
	if got := len([]rune(label)); got != 120 {
		t.Fatalf("requester label rune length = %d, want 120", got)
	}
	if got := safeRequesterLabel("\n\t"); got != "Пользователь MaxPosty" {
		t.Fatalf("empty sanitized label = %q", got)
	}
}

func performJSONRequest(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
