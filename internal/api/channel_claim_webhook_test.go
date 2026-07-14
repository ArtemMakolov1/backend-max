package api

import (
	"context"
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
	publishRuns       int
}

func (f *claimWebhookMAX) GetMe(context.Context) (maxclient.BotInfo, error) {
	return maxclient.BotInfo{UserID: 42, Username: "maxstudio_helper_bot", IsBot: true}, nil
}

func (f *claimWebhookMAX) GetChat(context.Context, string) (maxclient.ChatInfo, error) {
	return f.chat, nil
}

func (f *claimWebhookMAX) GetChatByLink(context.Context, string) (maxclient.ChatInfo, error) {
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

func (f *claimWebhookMAX) AnswerCallback(_ context.Context, callbackID, notification string) error {
	f.callbackAnswers = append(f.callbackAnswers, callbackID+":"+notification)
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

	response = performMAXWebhook(handler, confirmation)
	if response.Code != http.StatusOK {
		t.Fatalf("callback replay = %d %s", response.Code, response.Body.String())
	}
	channels, err := storage.ListChannelsForUser(ctx, "tenant-a")
	if err != nil || len(channels) != 1 {
		t.Fatalf("callback replay created duplicate channels: %#v, %v", channels, err)
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
