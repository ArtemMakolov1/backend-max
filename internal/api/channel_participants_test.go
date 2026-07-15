package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

func TestChannelParticipantHistoryAPIIsPrivateBoundedAndTenantScoped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "channel-participant-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	if err := storage.UpsertUser(ctx, store.User{ID: "owner-a", Login: "owner-a", DisplayName: "Owner A"}); err != nil {
		t.Fatal(err)
	}
	channel, err := storage.CreateChannel(ctx, store.Channel{
		UserID: "owner-a", VerifiedMAXOwnerID: "max-owner-a", MAXChatID: "901",
		Title: "Channel", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2043, time.January, 2, 10, 0, 0, 0, time.UTC)
	second := first.AddDate(0, 0, 1)
	if _, err := storage.SyncChannelParticipantStatsForUser(ctx, "owner-a", channel.ID, channel.MAXChatID, "", 10, first); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.SyncChannelParticipantStatsForUser(ctx, "owner-a", channel.ID, channel.MAXChatID, "", 12, second); err != nil {
		t.Fatal(err)
	}
	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application := app.New(storage, mediaStore, nil, nil, nil, logger)
	server := New(application, logger, "http://localhost:4321", "", AuthOptions{YandexClient: &fakeYandexOAuth{}})
	handler := withTestSession(t, storage, server.Handler(), "owner-a")

	response := performJSONRequest(handler, http.MethodGet,
		"/api/v1/channels/"+postID(channel.ID)+"/participant-history?from=2043-01-02&to=2043-01-03", "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("participant history response = %d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var history []store.ChannelParticipantSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].ObservedOn != "2043-01-02" || history[0].ParticipantsCount != 10 ||
		history[1].ObservedOn != "2043-01-03" || history[1].ParticipantsCount != 12 {
		t.Fatalf("participant history = %#v", history)
	}
	invalid := performJSONRequest(handler, http.MethodGet,
		"/api/v1/channels/"+postID(channel.ID)+"/participant-history?from=2043-01-03&to=2043-01-02", "")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid participant range status = %d, body=%s", invalid.Code, invalid.Body.String())
	}
	unauthorized := New(application, logger, "http://localhost:4321", "").Handler()
	missingSession := performJSONRequest(unauthorized, http.MethodGet,
		"/api/v1/channels/"+postID(channel.ID)+"/participant-history", "")
	if missingSession.Code != http.StatusUnauthorized {
		t.Fatalf("participant history without session = %d, body=%s", missingSession.Code, missingSession.Body.String())
	}
}
