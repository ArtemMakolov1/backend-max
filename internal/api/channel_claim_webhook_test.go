package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

type claimWebhookMAX struct {
	chat              maxclient.ChatInfo
	membership        maxclient.Membership
	confirmationRuns  int
	confirmationUser  string
	confirmationTitle string
	confirmationLink  string
	requesterLabel    string
	comparisonCode    string
	confirmPayload    string
	cancelPayload     string
	callbackAnswers   []string
	callbackMessages  []string
	publishRuns       int
	getChatIDs        []string
	getLinkErr        error
}

func (f *claimWebhookMAX) GetMe(context.Context) (maxclient.BotInfo, error) {
	return maxclient.BotInfo{UserID: 42, Username: "maxstudio_helper_bot", IsBot: true}, nil
}

func (f *claimWebhookMAX) GetChat(_ context.Context, chatID string) (maxclient.ChatInfo, error) {
	f.getChatIDs = append(f.getChatIDs, chatID)
	return f.chat, nil
}

func (f *claimWebhookMAX) GetChatByLink(context.Context, string) (maxclient.ChatInfo, error) {
	if f.getLinkErr != nil {
		return maxclient.ChatInfo{}, f.getLinkErr
	}
	return f.chat, nil
}

func (f *claimWebhookMAX) GetMembership(context.Context, string) (maxclient.Membership, error) {
	return f.membership, nil
}

func (f *claimWebhookMAX) SendClaimConfirmation(_ context.Context, maxUserID, title, link, requesterLabel,
	comparisonCode, confirmPayload, cancelPayload string) error {
	f.confirmationRuns++
	f.confirmationUser = maxUserID
	f.confirmationTitle = title
	f.confirmationLink = link
	f.requesterLabel = requesterLabel
	f.comparisonCode = comparisonCode
	f.confirmPayload = confirmPayload
	f.cancelPayload = cancelPayload
	return nil
}

func (f *claimWebhookMAX) AnswerCallback(_ context.Context, callbackID, notification, messageText string) error {
	f.callbackAnswers = append(f.callbackAnswers, callbackID+":"+notification)
	f.callbackMessages = append(f.callbackMessages, messageText)
	return nil
}

func (f *claimWebhookMAX) UploadImage(context.Context, string, io.Reader) (maxclient.UploadResult, error) {
	return maxclient.UploadResult{}, nil
}

func (f *claimWebhookMAX) Publish(context.Context, maxclient.PublishRequest) (maxclient.Message, error) {
	f.publishRuns++
	return maxclient.Message{}, nil
}

func (f *claimWebhookMAX) Edit(context.Context, maxclient.EditRequest) error { return nil }
func (f *claimWebhookMAX) Delete(context.Context, string) error              { return nil }

func TestMAXCallbackCompletesChannelClaimWithoutBrowserPoll(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "claim-webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	if err := storage.UpsertUser(ctx, store.User{ID: "tenant-a", Login: "alice", DisplayName: "Alice"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	deepToken := strings.Repeat("deep-token-", 4)
	claim := store.ChannelClaim{
		ID: "claim-browser-closed", TokenHash: sha256Hex(deepToken), UserID: "tenant-a", MAXChatID: "-13549123",
		PublicLink: "https://max.ru/se13549123_biz", RequestedTitle: "Тестовый канал",
		RequesterLabel: "alice", ComparisonCode: "271828", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := storage.CreateChannelClaim(ctx, claim); err != nil {
		t.Fatal(err)
	}
	fake := &claimWebhookMAX{
		chat: maxclient.ChatInfo{
			ChatID: "-13549123", OwnerID: "777", Type: "channel", Status: "active", Title: "Тестовый канал",
			Link: "https://max.ru/se13549123_biz", Icon: maxclient.ChatIcon{URL: "https://cdn.max.ru/channel.png"},
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
		"http://localhost:4321", "webhook-secret")
	server.now = func() time.Time { return now }
	handler := server.Handler()

	botStarted := fmt.Sprintf(`{"update_type":"bot_started","timestamp":%d,"chat_id":777,"payload":"claim_%s","user":{"user_id":777,"name":"Владелец","username":"owner"}}`,
		now.UnixMilli(), deepToken)
	response := performMAXWebhook(handler, botStarted)
	if response.Code != http.StatusOK {
		t.Fatalf("bot_started = %d %s", response.Code, response.Body.String())
	}
	response = performMAXWebhook(handler, botStarted)
	if response.Code != http.StatusOK || fake.confirmationRuns != 1 {
		t.Fatalf("bot_started replay status=%d sends=%d body=%s", response.Code, fake.confirmationRuns, response.Body.String())
	}
	if fake.confirmationUser != "777" || fake.confirmationTitle != claim.RequestedTitle ||
		fake.confirmationLink != claim.PublicLink || fake.requesterLabel != claim.RequesterLabel ||
		fake.comparisonCode != claim.ComparisonCode {
		t.Fatalf("confirmation context was changed or omitted: %#v", fake)
	}
	if !strings.HasPrefix(fake.confirmPayload, "claim_confirm_") || !strings.HasPrefix(fake.cancelPayload, "claim_cancel_") {
		t.Fatalf("callback payloads = %q, %q", fake.confirmPayload, fake.cancelPayload)
	}

	wrongUser := fmt.Sprintf(`{"update_type":"message_callback","timestamp":%d,"callback":{"timestamp":%d,"callback_id":"wrong-user","payload":"%s","user":{"user_id":999,"name":"Другой"}},"message":null}`,
		now.UnixMilli(), now.UnixMilli(), fake.confirmPayload)
	response = performMAXWebhook(handler, wrongUser)
	if response.Code != http.StatusOK {
		t.Fatalf("wrong-user callback = %d %s", response.Code, response.Body.String())
	}
	pending, err := storage.GetChannelClaimForUser(ctx, "tenant-a", claim.ID, now)
	if err != nil || pending.Status != store.ChannelClaimAwaitingConfirmation {
		t.Fatalf("wrong MAX user changed claim: %#v, %v", pending, err)
	}

	// No GET /channels/connect/{id} poll happens here. The authenticated MAX
	// owner callback itself must complete the claim for a closed browser.
	confirmation := fmt.Sprintf(`{"update_type":"message_callback","timestamp":%d,"callback":{"timestamp":%d,"callback_id":"callback-1","payload":"%s","user":{"user_id":777,"name":"Владелец"}},"message":null}`,
		now.UnixMilli(), now.UnixMilli(), fake.confirmPayload)
	response = performMAXWebhook(handler, confirmation)
	if response.Code != http.StatusOK {
		t.Fatalf("confirmation callback = %d %s", response.Code, response.Body.String())
	}
	connected, err := storage.GetChannelClaimForUser(ctx, "tenant-a", claim.ID, now)
	if err != nil || connected.Status != store.ChannelClaimConnected || connected.ChannelID == nil {
		t.Fatalf("callback did not complete claim: %#v, %v", connected, err)
	}
	channel, err := storage.GetChannelForUser(ctx, "tenant-a", *connected.ChannelID)
	if err != nil || channel.MAXChatID != claim.MAXChatID || channel.VerifiedMAXOwnerID != "777" {
		t.Fatalf("connected channel = %#v, %v", channel, err)
	}
	if fake.publishRuns != 0 {
		t.Fatalf("ownership verification published a visible message %d time(s)", fake.publishRuns)
	}
	if len(fake.callbackAnswers) != 1 || !strings.Contains(fake.callbackAnswers[0], "Канал подключён") {
		t.Fatalf("callback answers = %#v", fake.callbackAnswers)
	}
	if len(fake.callbackMessages) != 1 || !strings.Contains(fake.callbackMessages[0], "✅ Готово!") ||
		!strings.Contains(fake.callbackMessages[0], "публиковать посты") {
		t.Fatalf("callback replacement messages = %#v", fake.callbackMessages)
	}

	response = performMAXWebhook(handler, confirmation)
	if response.Code != http.StatusOK {
		t.Fatalf("callback replay = %d %s", response.Code, response.Body.String())
	}
	channels, err := storage.ListChannelsForUser(ctx, "tenant-a")
	if err != nil || len(channels) != 1 {
		t.Fatalf("callback replay created duplicate channels: %#v, %v", channels, err)
	}
}

func TestMAXMessageCreatedObservesChannelFromNestedRecipient(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "message-created.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	fake := &claimWebhookMAX{chat: maxclient.ChatInfo{
		ChatID: "-70801090403050", OwnerID: "123456789", Type: "channel", Status: "active",
		Title: "Канал из события", Link: "https://max.ru/official_channel",
		Icon: maxclient.ChatIcon{URL: "https://cdn.max.ru/channel.png"}, ParticipantsCount: 15,
	}}
	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(app.New(storage, mediaStore, fake, nil, nil, logger), logger,
		"http://localhost:4321", "webhook-secret")
	server.now = func() time.Time { return time.UnixMilli(1775025604499).UTC() }

	// This is the message_created fixture shape from the official MAX Go SDK.
	// A channel delivery differs from its group-chat sample only by chat_type.
	body := `{
		"message": {
			"recipient": {"chat_id": -70801090403050, "chat_type": "channel"},
			"timestamp": 1775053255737,
			"body": {"mid": "mid.ffffbdb48e6c3775019d496b34394b84", "seq": 116327994376978687, "text": "..."},
			"sender": {"user_id": 123456789, "first_name": "John", "last_name": "Doe", "is_bot": false, "last_activity_time": 1775053249000, "name": "John Doe"},
			"link": {"type": "forward", "message": {"mid": "mid.sha-more", "seq": 116327994376978687, "text": "Лада седан - баклажан"}, "sender": {"user_id": 398398398, "first_name": "Tod", "last_name": "V", "is_bot": false, "last_activity_time": 1775755269000, "name": "Tod V"}, "chat_id": -695695695695}
		},
		"timestamp": 1775025604499,
		"update_type": "message_created"
	}`
	response := performMAXWebhook(server.Handler(), body)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ok":true`) {
		t.Fatalf("message_created = %d %s", response.Code, response.Body.String())
	}
	if len(fake.getChatIDs) != 1 || fake.getChatIDs[0] != "-70801090403050" {
		t.Fatalf("GetChat ids = %#v, want nested recipient id", fake.getChatIDs)
	}
	observed, err := storage.GetActiveObservedBotChat(ctx, "https://max.ru/official_channel", "")
	if err != nil || observed.MAXChatID != "-70801090403050" || observed.Title != "Канал из события" || !observed.Active {
		t.Fatalf("observed channel = %#v, %v", observed, err)
	}
}

func TestMAXMessageCreatedIgnoresNonChannelAndInvalidNestedRecipient(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "message-created-ignored.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	fake := &claimWebhookMAX{chat: maxclient.ChatInfo{ChatID: "-1", Type: "channel", Status: "active"}}
	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(app.New(storage, mediaStore, fake, nil, nil, logger), logger,
		"http://localhost:4321", "webhook-secret")
	now := time.UnixMilli(1775025604499).UTC()
	server.now = func() time.Time { return now }

	for name, body := range map[string]string{
		"spoofed top-level channel": `{"update_type":"message_created","timestamp":1775025604499,"chat_id":-1,"is_channel":true,"message":{"recipient":{"chat_id":-1,"chat_type":"chat"}}}`,
		"invalid nested chat id":    `{"update_type":"message_created","timestamp":1775025604499,"message":{"recipient":{"chat_id":"not-a-number","chat_type":"channel"}}}`,
		"missing message":           `{"update_type":"message_created","timestamp":1775025604499}`,
	} {
		t.Run(name, func(t *testing.T) {
			response := performMAXWebhook(server.Handler(), body)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ignored":true`) {
				t.Fatalf("message_created = %d %s", response.Code, response.Body.String())
			}
		})
	}
	if len(fake.getChatIDs) != 0 {
		t.Fatalf("ignored events called GetChat: %#v", fake.getChatIDs)
	}
	if _, err := storage.GetActiveObservedBotChat(ctx, "", "-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ignored event entered inventory: %v", err)
	}
}

func TestStartChannelConnectMapsMAXChatNotFoundToActionableEventError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "channel-event-required.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	fake := &claimWebhookMAX{getLinkErr: &maxclient.Error{
		StatusCode: http.StatusNotFound,
		Code:       "chat.not.found",
		Message:    "Chat not found by link: se13549123_biz",
	}}
	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(app.New(storage, mediaStore, fake, nil, nil, logger), logger,
		"http://localhost:4321", "webhook-secret", AuthOptions{YandexClient: &fakeYandexOAuth{}})
	handler := withTestSession(t, storage, server.Handler(), "tenant-a")

	response := performJSONRequest(handler, http.MethodPost, "/api/v1/channels/connect/start",
		`{"public_link":"https://max.ru/se13549123_biz"}`)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("connect start = %d %s", response.Code, response.Body.String())
	}
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				Action string `json:"action"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	const wantMessage = "MAX не передал ID этого уже подключённого канала. Опубликуйте в канале любой новый пост, затем нажмите „Обновить список“."
	if payload.Error.Code != "max_channel_event_required" || payload.Error.Message != wantMessage ||
		payload.Error.Details.Action != "publish_post_and_refresh" {
		t.Fatalf("problem contract = %#v", payload.Error)
	}
}

func TestStartChannelConnectUsesLinkedMAXIdentityWithoutRepeatedBotConfirmation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "linked-identity-connect.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	if err := storage.UpsertUser(ctx, store.User{ID: "tenant-a", Login: "alice", DisplayName: "Alice"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	linkMAXIdentityForChannelTest(t, storage, "tenant-a", "32202189", now)

	fake := &claimWebhookMAX{
		chat: maxclient.ChatInfo{
			ChatID: "-76868796016845", OwnerID: "32202189", Type: "channel", Status: "active",
			Title: "Тестовый канал", Link: "https://max.ru/se13549123_biz",
			Icon: maxclient.ChatIcon{URL: "https://cdn.max.ru/channel.png"}, ParticipantsCount: 42,
		},
		membership: maxclient.Membership{IsAdmin: true, Permissions: []maxclient.Permission{
			maxclient.PermissionReadAllMessages, maxclient.PermissionWrite,
			maxclient.PermissionEdit, maxclient.PermissionDelete,
		}},
	}
	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(app.New(storage, mediaStore, fake, nil, nil, logger), logger,
		"http://localhost:4321", "webhook-secret", AuthOptions{YandexClient: &fakeYandexOAuth{}})
	handler := withTestSession(t, storage, server.Handler(), "tenant-a")

	response := performJSONRequest(handler, http.MethodPost, "/api/v1/channels/connect/start",
		`{"public_link":"https://max.ru/se13549123_biz"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("connect start = %d %s", response.Code, response.Body.String())
	}
	var payload struct {
		ClaimID     string                 `json:"claim_id"`
		Status      string                 `json:"status"`
		Channel     store.Channel          `json:"channel"`
		Diagnostics app.ChannelDiagnostics `json:"diagnostics"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ClaimID == "" || payload.Status != store.ChannelClaimConnected ||
		payload.Channel.MAXChatID != fake.chat.ChatID ||
		!payload.Diagnostics.CanPublish {
		t.Fatalf("immediate connection payload = %#v", payload)
	}
	if fake.confirmationRuns != 0 {
		t.Fatalf("linked identity triggered %d repeated bot confirmation(s)", fake.confirmationRuns)
	}
	channels, err := storage.ListChannelsForUser(ctx, "tenant-a")
	if err != nil || len(channels) != 1 || channels[0].VerifiedMAXOwnerID != "32202189" {
		t.Fatalf("connected channels = %#v, %v", channels, err)
	}
}

func linkMAXIdentityForChannelTest(t *testing.T, storage *store.Store, userID, maxUserID string, now time.Time) {
	t.Helper()
	attempt := store.MAXIdentityLinkAttempt{
		ID: "identity-" + userID, TokenHash: strings.Repeat("a", 64), UserID: userID,
		RequesterLabel: userID, ComparisonCode: "123456", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := storage.CreateMAXIdentityLinkAttempt(t.Context(), attempt); err != nil {
		t.Fatal(err)
	}
	confirmHash := strings.Repeat("b", 64)
	if _, _, err := storage.StartMAXIdentityLinkConfirmation(t.Context(), attempt.TokenHash, maxUserID,
		confirmHash, strings.Repeat("c", 64), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ConfirmMAXIdentityLink(t.Context(), confirmHash, maxUserID, true, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func performMAXWebhook(handler http.Handler, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/max", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Max-Bot-Api-Secret", "webhook-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
