package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

func TestMAXIdentityLinkDiscoversAndConnectsOwnedObservedChannels(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "max-identity-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	if err := storage.UpsertUser(ctx, store.User{ID: "tenant-a", Login: "alice", DisplayName: "Alice"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	fake := &claimWebhookMAX{
		chat: maxclient.ChatInfo{
			ChatID: "-13549123", Type: "channel", Status: "active", Title: "Тестовый канал",
			Link: "https://max.ru/se13549123_biz", Icon: maxclient.ChatIcon{URL: "https://cdn.max.ru/channel.png"},
			ParticipantsCount: 42,
		},
		admins: []maxclient.ChatMember{
			{UserID: 888, IsAdmin: true},
			{UserID: 777, IsOwner: true, IsAdmin: true},
		},
		membership: maxclient.Membership{IsAdmin: true, Permissions: []maxclient.Permission{
			maxclient.PermissionReadAllMessages, maxclient.PermissionWrite, maxclient.PermissionEdit, maxclient.PermissionDelete,
		}},
	}
	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(app.New(storage, mediaStore, fake, nil, nil, logger), logger,
		"http://localhost:4321", "webhook-secret", AuthOptions{YandexClient: &fakeYandexOAuth{}})
	server.now = func() time.Time { return now }
	handler := withTestSession(t, storage, server.Handler(), "tenant-a")

	startResponse := performJSONRequest(handler, http.MethodPost, "/api/v1/integration/max/identity", "")
	if startResponse.Code != http.StatusCreated || startResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("identity start=%d cache=%q body=%s", startResponse.Code, startResponse.Header().Get("Cache-Control"), startResponse.Body.String())
	}
	var started struct {
		Identity maxIdentityPublicStatus `json:"identity"`
	}
	if err := json.Unmarshal(startResponse.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.Identity.Status != store.MAXIdentityAttemptPending || started.Identity.BotURL == "" || started.Identity.ComparisonCode == "" {
		t.Fatalf("identity start payload=%#v", started.Identity)
	}
	botURL, err := url.Parse(started.Identity.BotURL)
	if err != nil {
		t.Fatal(err)
	}
	startPayload := botURL.Query().Get("start")
	if !strings.HasPrefix(startPayload, "link_") {
		t.Fatalf("MAX deep-link payload=%q", startPayload)
	}
	deepToken := strings.TrimPrefix(startPayload, "link_")
	botStarted := fmt.Sprintf(`{"update_type":"bot_started","timestamp":%d,"chat_id":777,"payload":"link_%s","user":{"user_id":777}}`, now.UnixMilli(), deepToken)
	response := performMAXWebhook(handler, botStarted)
	if response.Code != http.StatusOK || fake.confirmationRuns != 1 {
		t.Fatalf("identity bot_started=%d sends=%d body=%s", response.Code, fake.confirmationRuns, response.Body.String())
	}
	response = performMAXWebhook(handler, botStarted)
	if response.Code != http.StatusOK || fake.confirmationRuns != 1 {
		t.Fatalf("identity deep-link replay=%d sends=%d", response.Code, fake.confirmationRuns)
	}
	if !strings.HasPrefix(fake.confirmPayload, "link_confirm_") || !strings.HasPrefix(fake.cancelPayload, "link_cancel_") ||
		fake.comparisonCode != started.Identity.ComparisonCode || fake.requesterLabel != "tenant-a" {
		t.Fatalf("identity confirmation context=%#v", fake)
	}
	poll := performJSONRequest(handler, http.MethodGet, "/api/v1/integration/max/identity", "")
	if poll.Code != http.StatusOK || !strings.Contains(poll.Body.String(), `"status":"awaiting_confirmation"`) || strings.Contains(poll.Body.String(), "bot_url") {
		t.Fatalf("identity poll=%d body=%s", poll.Code, poll.Body.String())
	}

	wrongUser := fmt.Sprintf(`{"update_type":"message_callback","timestamp":%d,"callback":{"callback_id":"wrong","payload":"%s","user":{"user_id":999}}}`, now.UnixMilli(), fake.confirmPayload)
	if response = performMAXWebhook(handler, wrongUser); response.Code != http.StatusOK {
		t.Fatalf("wrong identity callback=%d %s", response.Code, response.Body.String())
	}
	if _, err := storage.GetMAXIdentityLinkForUser(ctx, "tenant-a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("wrong MAX user linked identity: %v", err)
	}
	confirmation := fmt.Sprintf(`{"update_type":"message_callback","timestamp":%d,"callback":{"callback_id":"link-1","payload":"%s","user":{"user_id":777}}}`, now.UnixMilli(), fake.confirmPayload)
	if response = performMAXWebhook(handler, confirmation); response.Code != http.StatusOK {
		t.Fatalf("identity confirmation=%d %s", response.Code, response.Body.String())
	}
	if len(fake.callbackAnswers) != 1 || !strings.Contains(fake.callbackAnswers[0], "Профиль MAX связан") {
		t.Fatalf("identity callback answers=%#v", fake.callbackAnswers)
	}
	if len(fake.callbackMessages) != 1 ||
		fake.callbackMessages[0] != "✅ Готово! Профиль MAX связан с MaxPosty.\n\nВернитесь в MaxPosty — теперь можно подключить канал." {
		t.Fatalf("identity callback replacement messages=%#v", fake.callbackMessages)
	}
	linkedPoll := performJSONRequest(handler, http.MethodGet, "/api/v1/integration/max/identity", "")
	if linkedPoll.Code != http.StatusOK || !strings.Contains(linkedPoll.Body.String(), `"status":"linked"`) ||
		!strings.Contains(linkedPoll.Body.String(), `"max_user_id":"777"`) || strings.Contains(linkedPoll.Body.String(), "bot_url") {
		t.Fatalf("linked identity poll=%d body=%s", linkedPoll.Code, linkedPoll.Body.String())
	}
	restart := performJSONRequest(handler, http.MethodPost, "/api/v1/integration/max/identity", "")
	if restart.Code != http.StatusOK || strings.Contains(restart.Body.String(), "bot_url") {
		t.Fatalf("linked identity restart=%d body=%s", restart.Code, restart.Body.String())
	}

	// bot_added can arrive while MAX still returns an empty owner. The explicit
	// refresh route must reconcile that incomplete inventory row and immediately
	// return the fresh avatar/title to this tenant.
	if err := storage.UpsertObservedBotChat(ctx, store.ObservedBotChat{
		MAXChatID: fake.chat.ChatID, Title: "Старое название", IconURL: "https://cdn.max.ru/old.png",
		Active: true, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	refresh := performJSONRequest(handler, http.MethodPost, "/api/v1/channels/discoverable/refresh", "")
	if refresh.Code != http.StatusOK || refresh.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(refresh.Body.String(), `"refreshed":1`) ||
		!strings.Contains(refresh.Body.String(), `"icon_url":"https://cdn.max.ru/channel.png"`) {
		t.Fatalf("discoverable refresh=%d cache=%q body=%s", refresh.Code, refresh.Header().Get("Cache-Control"), refresh.Body.String())
	}
	maxCalls, adminCalls := len(fake.getChatIDs), len(fake.getAdminChatIDs)
	repeatedRefresh := performJSONRequest(handler, http.MethodPost, "/api/v1/channels/discoverable/refresh", "")
	if repeatedRefresh.Code != http.StatusTooManyRequests || repeatedRefresh.Header().Get("Cache-Control") != "no-store" ||
		repeatedRefresh.Header().Get("Retry-After") != "15" ||
		!strings.Contains(repeatedRefresh.Body.String(), `"code":"channels_refresh_cooldown"`) ||
		!strings.Contains(repeatedRefresh.Body.String(), `"retry_after_seconds":15`) {
		t.Fatalf("repeated refresh=%d retry=%q body=%s", repeatedRefresh.Code,
			repeatedRefresh.Header().Get("Retry-After"), repeatedRefresh.Body.String())
	}
	if len(fake.getChatIDs) != maxCalls || len(fake.getAdminChatIDs) != adminCalls {
		t.Fatalf("cooldown reached MAX: chats=%#v admins=%#v", fake.getChatIDs, fake.getAdminChatIDs)
	}
	discoverable := performJSONRequest(handler, http.MethodGet, "/api/v1/channels/discoverable", "")
	if discoverable.Code != http.StatusOK || discoverable.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(discoverable.Body.String(), fake.chat.ChatID) || !strings.Contains(discoverable.Body.String(), `"owner_verified":true`) {
		t.Fatalf("discoverable=%d cache=%q body=%s", discoverable.Code, discoverable.Header().Get("Cache-Control"), discoverable.Body.String())
	}
	connect := performJSONRequest(handler, http.MethodPost, "/api/v1/channels/connect/observed", `{"max_chat_id":"-13549123"}`)
	if connect.Code != http.StatusOK || !strings.Contains(connect.Body.String(), `"max_chat_id":"-13549123"`) ||
		!strings.Contains(connect.Body.String(), `"can_publish":true`) {
		t.Fatalf("connect observed=%d body=%s", connect.Code, connect.Body.String())
	}
	channels, err := storage.ListChannelsForUser(ctx, "tenant-a")
	if err != nil || len(channels) != 1 || channels[0].VerifiedMAXOwnerID != "777" {
		t.Fatalf("connected channels=%#v err=%v", channels, err)
	}
}
