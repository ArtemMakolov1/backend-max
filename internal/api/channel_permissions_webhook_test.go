package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"maxpilot/backend/internal/app"
	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

func TestPermissionsWebhookRefreshesChannelAndNotifiesOncePerLoss(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "permissions-webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	fake := &claimWebhookMAX{
		chat:       maxclient.ChatInfo{ChatID: "-123", OwnerID: "123", Type: "channel", Status: "active", Title: "New MAX title", ParticipantsCount: 42},
		membership: maxclient.Membership{IsAdmin: true, Permissions: []maxclient.Permission{maxclient.PermissionReadAllMessages}},
	}
	channel, err := storage.CreateChannel(ctx, store.Channel{
		MAXChatID: fake.chat.ChatID, VerifiedMAXOwnerID: fake.chat.OwnerID, Title: "Old title", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(app.New(storage, mediaStore, fake, nil, nil, logger), logger, "http://localhost:4321", "webhook-secret")
	base := time.Now().UTC().Add(time.Minute).Truncate(time.Millisecond)
	server.now = func() time.Time { return base.Add(time.Minute) }
	handler := server.Handler()
	event := func(at time.Time) string {
		// The event body claims write access. The authoritative membership does
		// not: notification must reflect the fresh provider read instead.
		return fmt.Sprintf(`{"update_type":"bot_admin_permissions_changed","timestamp":%d,"chat_id":-123,"user_id":123,"bot_id":42,"is_channel":true,"is_admin":true,"permissions":["write"]}`, at.UnixMilli())
	}
	for _, at := range []time.Time{base, base, base.Add(time.Second)} {
		response := performMAXWebhook(handler, event(at))
		if response.Code != http.StatusOK {
			t.Fatalf("permissions webhook = %d %s", response.Code, response.Body.String())
		}
	}
	channel, err = storage.GetChannel(ctx, channel.ID)
	if err != nil || channel.Title != fake.chat.Title || channel.ParticipantsCount != 42 || !channel.Active {
		t.Fatalf("permissions refresh changed manual active state or missed metadata: %#v, %v", channel, err)
	}
	notifications, err := storage.ListNotifications(ctx, channel.UserID, channel.WorkspaceID, false, 20, 0)
	if err != nil || len(notifications) != 1 || notifications[0].Kind != "channel.permissions_lost" {
		t.Fatalf("permissions loss inbox = %#v, %v", notifications, err)
	}
	// A fresh restoration permits another later loss notification. A delayed
	// restoration event cannot overwrite the newer stored loss observation.
	fake.membership.Permissions = append(fake.membership.Permissions, maxclient.PermissionWrite)
	if response := performMAXWebhook(handler, event(base.Add(-time.Second))); response.Code != http.StatusOK {
		t.Fatalf("older event = %d %s", response.Code, response.Body.String())
	}
	fake.membership.Permissions = fake.membership.Permissions[:1]
	if response := performMAXWebhook(handler, event(base.Add(2*time.Second))); response.Code != http.StatusOK {
		t.Fatalf("repeated loss = %d %s", response.Code, response.Body.String())
	}
	notifications, err = storage.ListNotifications(ctx, channel.UserID, channel.WorkspaceID, false, 20, 0)
	if err != nil || len(notifications) != 1 {
		t.Fatalf("old restoration reset dedupe state: %#v, %v", notifications, err)
	}
	fake.membership.Permissions = append(fake.membership.Permissions, maxclient.PermissionWrite)
	if response := performMAXWebhook(handler, event(base.Add(3*time.Second))); response.Code != http.StatusOK {
		t.Fatalf("restoration = %d %s", response.Code, response.Body.String())
	}
	fake.membership.Permissions = fake.membership.Permissions[:1]
	if response := performMAXWebhook(handler, event(base.Add(4*time.Second))); response.Code != http.StatusOK {
		t.Fatalf("new loss = %d %s", response.Code, response.Body.String())
	}
	notifications, err = storage.ListNotifications(ctx, channel.UserID, channel.WorkspaceID, false, 20, 0)
	if err != nil || len(notifications) != 2 {
		t.Fatalf("new loss was not notified: %#v, %v", notifications, err)
	}
	// Lifecycle removal wins over a delayed permissions event even when MAX's
	// metadata fake still looks active. The user cannot be silently reactivated.
	removedAt := base.Add(5 * time.Second)
	if response := performMAXWebhook(handler, fmt.Sprintf(`{"update_type":"bot_removed","timestamp":%d,"chat_id":-123}`, removedAt.UnixMilli())); response.Code != http.StatusOK {
		t.Fatalf("bot_removed = %d %s", response.Code, response.Body.String())
	}
	if response := performMAXWebhook(handler, event(base.Add(4*time.Second))); response.Code != http.StatusOK {
		t.Fatalf("delayed permissions = %d %s", response.Code, response.Body.String())
	}
	channel, err = storage.GetChannel(ctx, channel.ID)
	if err != nil || channel.Active {
		t.Fatalf("old permissions event resurrected removed channel: %#v, %v", channel, err)
	}
	if _, err := storage.GetActiveObservedBotChat(ctx, "", channel.MAXChatID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("removed lifecycle inventory was revived: %v", err)
	}
}

func TestPermissionsWebhookDoesNotEnterGroupsIntoChannelInventory(t *testing.T) {
	t.Parallel()
	fixture := newWorkspaceAPIFixture(t)
	fake := &claimWebhookMAX{chat: maxclient.ChatInfo{ChatID: "-123", Type: "chat", Status: "active", OwnerID: "123"}}
	server := New(app.New(fixture.storage, fixture.app.Media(), fake, nil, nil, fixture.logger), fixture.logger,
		"http://localhost:4321", "webhook-secret")
	response := performMAXWebhook(server.Handler(), fmt.Sprintf(`{"update_type":"bot_admin_permissions_changed","timestamp":%d,"chat_id":-123,"is_channel":false}`, time.Now().UnixMilli()))
	if response.Code != http.StatusOK || len(fake.getChatIDs) != 0 {
		t.Fatalf("group permissions webhook = %d %s, MAX reads=%d", response.Code, response.Body.String(), len(fake.getChatIDs))
	}
}
