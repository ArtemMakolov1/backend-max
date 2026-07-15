package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

func TestCreateRescheduleAndCancelCalendarPost(t *testing.T) {
	t.Parallel()
	handler, storage, channel := newCalendarTestHandler(t)
	moscow := time.FixedZone("MSK", 3*60*60)
	firstAt := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second).In(moscow)
	createBody, _ := json.Marshal(map[string]any{
		"title": "Calendar post", "content": "body", "format": "markdown", "channel_id": channel.ID,
		"notify": true, "disable_link_preview": false, "scheduled_at": firstAt.Format(time.RFC3339),
	})
	created := performPostRequest(t, handler, http.MethodPost, "/api/v1/posts", string(createBody), http.StatusCreated)
	if created.Status != store.PostStatusScheduled || created.ScheduledAt == nil ||
		created.ScheduledAt.Location() != time.UTC || !created.ScheduledAt.Equal(firstAt) {
		t.Fatalf("created scheduled post = %#v", created)
	}
	stored, err := storage.GetPost(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != store.PostStatusScheduled || stored.ScheduledAt == nil || stored.ScheduledAt.Location() != time.UTC {
		t.Fatalf("stored scheduled post = %#v", stored)
	}

	secondAt := firstAt.Add(3 * time.Hour).In(time.FixedZone("UTC-4", -4*60*60))
	patchBody, _ := json.Marshal(map[string]any{"scheduled_at": secondAt.Format(time.RFC3339)})
	rescheduled := performPostRequest(t, handler, http.MethodPatch, "/api/v1/posts/"+postID(created.ID), string(patchBody), http.StatusOK)
	if rescheduled.Status != store.PostStatusScheduled || rescheduled.ScheduledAt == nil || !rescheduled.ScheduledAt.Equal(secondAt) {
		t.Fatalf("rescheduled post = %#v", rescheduled)
	}

	canceled := performPostRequest(t, handler, http.MethodPatch, "/api/v1/posts/"+postID(created.ID), `{"scheduled_at":null}`, http.StatusOK)
	if canceled.Status != store.PostStatusDraft || canceled.ScheduledAt != nil {
		t.Fatalf("PATCH canceled post = %#v", canceled)
	}

	thirdAt := time.Now().UTC().Add(6 * time.Hour).Truncate(time.Second)
	patchBody, _ = json.Marshal(map[string]string{"scheduled_at": thirdAt.Format(time.RFC3339)})
	rescheduled = performPostRequest(t, handler, http.MethodPatch, "/api/v1/posts/"+postID(created.ID), string(patchBody), http.StatusOK)
	if rescheduled.Status != store.PostStatusScheduled || rescheduled.ScheduledAt == nil || !rescheduled.ScheduledAt.Equal(thirdAt) {
		t.Fatalf("PATCH scheduled draft = %#v", rescheduled)
	}

	fourthAt := thirdAt.Add(2 * time.Hour)
	scheduleBody, _ := json.Marshal(map[string]string{"scheduled_at": fourthAt.Format(time.RFC3339)})
	rescheduled = performPostRequest(t, handler, http.MethodPost, "/api/v1/posts/"+postID(created.ID)+"/schedule", string(scheduleBody), http.StatusOK)
	if rescheduled.Status != store.PostStatusScheduled || rescheduled.ScheduledAt == nil || !rescheduled.ScheduledAt.Equal(fourthAt) {
		t.Fatalf("dedicated reschedule = %#v", rescheduled)
	}
	pastBody, _ := json.Marshal(map[string]string{"scheduled_at": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)})
	pastResponse := httptest.NewRecorder()
	handler.ServeHTTP(pastResponse, httptest.NewRequest(http.MethodPatch, "/api/v1/posts/"+postID(created.ID), strings.NewReader(string(pastBody))))
	if pastResponse.Code != http.StatusBadRequest {
		t.Fatalf("past reschedule status = %d, body = %s", pastResponse.Code, pastResponse.Body.String())
	}
	stored, err = storage.GetPost(context.Background(), created.ID)
	if err != nil || stored.Status != store.PostStatusScheduled || stored.ScheduledAt == nil || !stored.ScheduledAt.Equal(fourthAt) {
		t.Fatalf("past reschedule changed calendar state: post=%#v error=%v", stored, err)
	}
	canceled = performPostRequest(t, handler, http.MethodPost, "/api/v1/posts/"+postID(created.ID)+"/cancel-schedule", "", http.StatusOK)
	if canceled.Status != store.PostStatusDraft || canceled.ScheduledAt != nil {
		t.Fatalf("dedicated cancel = %#v", canceled)
	}
}

func TestCalendarAPIRejectsPastMalformedAndIncompleteSchedules(t *testing.T) {
	t.Parallel()
	handler, storage, channel := newCalendarTestHandler(t)
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	tests := []struct {
		name string
		body map[string]any
	}{
		{name: "past", body: map[string]any{"title": "Past", "content": "body", "format": "markdown", "channel_id": channel.ID, "scheduled_at": past}},
		{name: "malformed", body: map[string]any{"title": "Malformed", "content": "body", "format": "markdown", "channel_id": channel.ID, "scheduled_at": "2030-01-01 10:00"}},
		{name: "missing channel", body: map[string]any{"title": "No channel", "content": "body", "format": "markdown", "scheduled_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339)}},
		{name: "missing content", body: map[string]any{"title": "No content", "format": "markdown", "channel_id": channel.ID, "scheduled_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339)}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			body, _ := json.Marshal(test.body)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/posts", strings.NewReader(string(body))))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
	posts, err := storage.ListPosts(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 0 {
		t.Fatalf("invalid calendar requests persisted posts: %#v", posts)
	}
}

func TestParseFutureTimeUsesRFC3339OffsetAndUTC(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, time.January, 1, 10, 0, 0, 0, time.UTC)
	parsed, err := parseFutureTimeAt("2030-01-01T14:30:00+03:00", now)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2030, time.January, 1, 11, 30, 0, 0, time.UTC)
	if parsed.Location() != time.UTC || !parsed.Equal(want) {
		t.Fatalf("parsed = %s (%s), want %s UTC", parsed, parsed.Location(), want)
	}
	if _, err := parseFutureTimeAt("2030-01-01T09:59:59Z", now); err == nil {
		t.Fatal("parseFutureTimeAt accepted a past timestamp")
	}
	if _, err := parseFutureTimeAt("2030-01-01T14:30:00", now); err == nil {
		t.Fatal("parseFutureTimeAt accepted a timestamp without an offset")
	}
}

func TestUpdatePostRejectsStaleClientRevision(t *testing.T) {
	t.Parallel()
	handler, storage, channel := newCalendarTestHandler(t)
	createBody, _ := json.Marshal(map[string]any{
		"title": "Revision", "content": "original", "format": "markdown",
		"channel_id": channel.ID, "notify": true,
	})
	created := performPostRequest(t, handler, http.MethodPost, "/api/v1/posts", string(createBody), http.StatusCreated)

	staleBody, _ := json.Marshal(map[string]any{
		"content":             "must not win",
		"expected_updated_at": created.UpdatedAt.Add(-time.Second).Format(time.RFC3339Nano),
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodPatch,
		"/api/v1/posts/"+postID(created.ID),
		strings.NewReader(string(staleBody)),
	))
	if response.Code != http.StatusConflict {
		t.Fatalf("stale update status = %d, want %d; body = %s", response.Code, http.StatusConflict, response.Body.String())
	}
	stored, err := storage.GetPost(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Content != "original" || !stored.Notify {
		t.Fatalf("stale client overwrote post: %#v", stored)
	}
}

func TestPostAPINormalizesUnsupportedSilentChannelDelivery(t *testing.T) {
	t.Parallel()
	handler, storage, channel := newCalendarTestHandler(t)
	createBody, _ := json.Marshal(map[string]any{
		"title": "Channel notification", "content": "body", "format": "markdown",
		"channel_id": channel.ID, "notify": false,
	})
	created := performPostRequest(t, handler, http.MethodPost, "/api/v1/posts", string(createBody), http.StatusCreated)
	if !created.Notify {
		t.Fatal("create API kept unsupported notify=false for a MAX channel")
	}

	updated := performPostRequest(t, handler, http.MethodPatch, "/api/v1/posts/"+postID(created.ID), `{"notify":false}`, http.StatusOK)
	if !updated.Notify {
		t.Fatal("update API kept unsupported notify=false for a MAX channel")
	}
	stored, err := storage.GetPost(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Notify {
		t.Fatal("unsupported notify=false was persisted for a MAX channel")
	}
}

func newCalendarTestHandler(t *testing.T) (http.Handler, *store.Store, store.Channel) {
	t.Helper()
	storage, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "calendar-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	channel, err := storage.CreateChannel(context.Background(), store.Channel{
		MAXChatID: "calendar-api", Title: "Calendar", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application := app.New(storage, mediaStore, nil, nil, nil, logger)
	server := New(application, logger, "http://localhost:4321", "", AuthOptions{YandexClient: &fakeYandexOAuth{}})
	return withTestSession(t, storage, server.Handler(), "test-owner"), storage, channel
}

func performPostRequest(t *testing.T, handler http.Handler, method, path, body string, wantStatus int) store.Post {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(method, path, strings.NewReader(body)))
	if response.Code != wantStatus {
		t.Fatalf("%s %s status = %d, want %d; body = %s", method, path, response.Code, wantStatus, response.Body.String())
	}
	var post store.Post
	if err := json.Unmarshal(response.Body.Bytes(), &post); err != nil {
		t.Fatalf("decode post response: %v; body = %s", err, response.Body.String())
	}
	return post
}

func postID(id int64) string {
	return strconv.FormatInt(id, 10)
}
