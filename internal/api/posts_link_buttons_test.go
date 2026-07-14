package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"maxpilot/backend/internal/store"
)

func TestPostAPIAutosavesAndClearsLinkButtons(t *testing.T) {
	t.Parallel()
	handler, storage, _ := newCalendarTestHandler(t)
	created := performPostRequest(t, handler, http.MethodPost, "/api/v1/posts", `{
		"title":"Кнопки","content":"Черновик","format":"markdown",
		"link_buttons":[{"text":"Подробнее","url":"https://"}]
	}`, http.StatusCreated)
	if len(created.LinkButtons) != 1 || created.LinkButtons[0].URL != "https://" {
		t.Fatalf("created link_buttons = %#v", created.LinkButtons)
	}

	cleared := performPostRequest(t, handler, http.MethodPatch, "/api/v1/posts/"+postID(created.ID),
		`{"link_buttons":[]}`, http.StatusOK)
	if cleared.LinkButtons == nil || len(cleared.LinkButtons) != 0 {
		t.Fatalf("cleared link_buttons = %#v", cleared.LinkButtons)
	}
	stored, err := storage.GetPost(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LinkButtons == nil || len(stored.LinkButtons) != 0 {
		t.Fatalf("stored link_buttons = %#v", stored.LinkButtons)
	}
}

func TestPostAPIRequiresStrictButtonsWhenScheduling(t *testing.T) {
	t.Parallel()
	handler, _, channel := newCalendarTestHandler(t)
	scheduledAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)

	invalidBody, _ := json.Marshal(map[string]any{
		"title": "Invalid", "content": "Post", "format": "markdown", "channel_id": channel.ID,
		"scheduled_at": scheduledAt,
		"link_buttons": []map[string]string{{"text": "Подробнее", "url": "https://"}},
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/posts", strings.NewReader(string(invalidBody))))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid scheduled buttons status = %d, body = %s", response.Code, response.Body.String())
	}

	validBody, _ := json.Marshal(map[string]any{
		"title": "Valid", "content": "Post", "format": "markdown", "channel_id": channel.ID,
		"scheduled_at": scheduledAt,
		"link_buttons": []store.LinkButton{{Text: "Подробнее", URL: "https://example.com/post"}},
	})
	created := performPostRequest(t, handler, http.MethodPost, "/api/v1/posts", string(validBody), http.StatusCreated)
	if created.Status != store.PostStatusScheduled || len(created.LinkButtons) != 1 {
		t.Fatalf("scheduled post = %#v", created)
	}
}

func TestPostAPIRejectsMalformedOrOversizedButtonCollections(t *testing.T) {
	t.Parallel()
	handler, _, _ := newCalendarTestHandler(t)
	tests := []string{
		`{"title":"Missing URL","link_buttons":[{"text":"Сайт"}]}`,
		`{"title":"Unknown field","link_buttons":[{"text":"Сайт","url":"https://example.com","kind":"link"}]}`,
		`{"title":"Null button","link_buttons":[null]}`,
		`{"title":"Too many","link_buttons":[{"text":"1","url":""},{"text":"2","url":""},{"text":"3","url":""},{"text":"4","url":""}]}`,
	}
	for _, body := range tests {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/posts", strings.NewReader(body)))
		if response.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400; response = %s", body, response.Code, response.Body.String())
		}
	}
}
