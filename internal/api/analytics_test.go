package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

func TestAnalyticsAPIIsPrivateBoundedAndTenantScoped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "analytics-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	for _, userID := range []string{"analytics-api-owner", "analytics-api-foreign"} {
		if err := storage.UpsertUser(ctx, store.User{ID: userID, Login: userID, DisplayName: userID}); err != nil {
			t.Fatal(err)
		}
	}
	channel, err := storage.CreateChannel(ctx, store.Channel{
		UserID: "analytics-api-owner", VerifiedMAXOwnerID: "max-owner", MAXChatID: "analytics-api-channel",
		Title: "Analytics", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	foreignChannel, err := storage.CreateChannel(ctx, store.Channel{
		UserID: "analytics-api-foreign", VerifiedMAXOwnerID: "max-foreign", MAXChatID: "analytics-api-foreign-channel",
		Title: "Foreign", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	today := utcAPIDate(time.Now())
	publishedAt := today.Add(9 * time.Hour)
	views := int64(8)
	post, err := storage.CreatePost(ctx, store.Post{
		UserID: "analytics-api-owner", Title: "Published", Content: "body", Status: store.PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: "analytics-api-message", MAXMessageURL: "https://max.ru/analytics/message",
		MAXViews: &views, MAXStatsSyncedAt: &publishedAt, PublishedAt: &publishedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if post.ID <= 0 {
		t.Fatal("post was not created")
	}

	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application := app.New(storage, mediaStore, nil, nil, nil, logger)
	server := New(application, logger, "http://localhost:4321", "", AuthOptions{YandexClient: &fakeYandexOAuth{}})
	server.now = func() time.Time { return today.Add(12 * time.Hour) }
	rawHandler := server.Handler()
	handler := withTestSession(t, storage, rawHandler, "analytics-api-owner")
	from := today.AddDate(0, 0, -2).Format(time.DateOnly)
	to := today.Format(time.DateOnly)

	response := performJSONRequest(handler, http.MethodGet, "/api/v1/analytics?"+url.Values{
		"channel_id": []string{strconv.FormatInt(channel.ID, 10)}, "from": []string{from}, "to": []string{to},
	}.Encode(), "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("analytics response = %d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var payload struct {
		Analytics store.ChannelAnalytics `json:"analytics"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Analytics.Channel.ID != channel.ID || payload.Analytics.Summary.PublishedPosts != 1 ||
		payload.Analytics.Summary.TotalViews == nil || *payload.Analytics.Summary.TotalViews != views ||
		len(payload.Analytics.Posts) != 1 || payload.Analytics.Posts[0].ID != post.ID {
		t.Fatalf("analytics payload = %#v", payload.Analytics)
	}

	invalidPaths := []string{
		"/api/v1/analytics",
		"/api/v1/analytics?channel_id=nope",
		"/api/v1/analytics?channel_id=" + strconv.FormatInt(channel.ID, 10) + "&from=15-07-2026",
		"/api/v1/analytics?channel_id=" + strconv.FormatInt(channel.ID, 10) + "&to=15-07-2026",
		"/api/v1/analytics?channel_id=" + strconv.FormatInt(channel.ID, 10) + "&from=" + to + "&to=" + from,
		"/api/v1/analytics?channel_id=" + strconv.FormatInt(channel.ID, 10) + "&from=" +
			today.AddDate(0, 0, -store.MaxChannelAnalyticsDays).Format(time.DateOnly) + "&to=" + to,
	}
	for _, path := range invalidPaths {
		invalid := performJSONRequest(handler, http.MethodGet, path, "")
		if invalid.Code != http.StatusBadRequest {
			t.Errorf("invalid analytics %q status = %d, body=%s", path, invalid.Code, invalid.Body.String())
		}
	}

	foreign := performJSONRequest(handler, http.MethodGet,
		"/api/v1/analytics?channel_id="+strconv.FormatInt(foreignChannel.ID, 10)+"&from="+from+"&to="+to, "")
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("foreign analytics status = %d, body=%s", foreign.Code, foreign.Body.String())
	}
	unauthenticated := performJSONRequest(rawHandler, http.MethodGet,
		"/api/v1/analytics?channel_id="+strconv.FormatInt(channel.ID, 10)+"&from="+from+"&to="+to, "")
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("analytics without session = %d, body=%s", unauthenticated.Code, unauthenticated.Body.String())
	}
}
