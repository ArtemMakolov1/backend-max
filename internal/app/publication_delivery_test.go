package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/store"
)

func publicationTestTarget(t *testing.T, fake *fakeMAX) (*App, *store.Store, store.Post) {
	t.Helper()
	fake.chat = maxclient.ChatInfo{ChatID: "delivery-channel", OwnerID: "test-max-owner", Type: "channel", Status: "active"}
	fake.membership = maxclient.Membership{IsAdmin: true, Permissions: []maxclient.Permission{
		maxclient.PermissionReadAllMessages, maxclient.PermissionWrite, maxclient.PermissionEdit,
	}}
	application, storage := newTestApp(t, fake)
	channel, err := storage.CreateChannel(context.Background(), store.Channel{
		MAXChatID: fake.chat.ChatID, VerifiedMAXOwnerID: fake.chat.OwnerID, Title: "Delivery", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(context.Background(), store.Post{
		Title: "Delivery", Content: "body", Format: store.FormatMarkdown, ChannelID: &channel.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return application, storage, post
}

func TestConfirmedPublicationPersistsAfterCallerCancellation(t *testing.T) {
	t.Parallel()
	fake := &fakeMAX{}
	application, storage, post := publicationTestTarget(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake.publishFn = func(context.Context, maxclient.PublishRequest) (maxclient.Message, error) {
		cancel() // MAX has accepted the message just as the browser disconnects.
		return maxclient.Message{MessageID: "confirmed-mid", URL: "https://max.ru/confirmed"}, nil
	}
	post, err := application.PublishPost(ctx, post.ID)
	if err != nil || post.Status != store.PostStatusPublished || post.MAXMessageID != "confirmed-mid" || post.PublicationHasChanges {
		t.Fatalf("confirmed publication = %#v, %v", post, err)
	}
	stored, err := storage.GetPost(context.Background(), post.ID)
	if err != nil || stored.MAXMessageID != "confirmed-mid" || stored.MAXMessageURL != "https://max.ru/confirmed" {
		t.Fatalf("durable publication = %#v, %v", stored, err)
	}
	if recovered, err := storage.RecoverStalePublishing(context.Background(), time.Now().Add(time.Hour)); err != nil || recovered != 0 {
		t.Fatalf("confirmed publication was recovered as failed: %d, %v", recovered, err)
	}
	if _, err := application.PublishPost(context.Background(), post.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("republish = %v, want conflict", err)
	}
	if fake.publishCalls != 1 {
		t.Fatalf("MAX message requests = %d, want exactly one", fake.publishCalls)
	}
}

func TestPublicationRetriesTransientPreflightBeforeSending(t *testing.T) {
	t.Parallel()
	fake := &fakeMAX{}
	application, _, post := publicationTestTarget(t, fake)
	fake.getChatFn = func(string) (maxclient.ChatInfo, error) {
		if fake.getChatCalls < 3 {
			return maxclient.ChatInfo{}, &maxclient.Error{StatusCode: http.StatusServiceUnavailable}
		}
		return fake.chat, nil
	}
	post, err := application.PublishPost(context.Background(), post.ID)
	if err != nil || post.Status != store.PostStatusPublished || fake.getChatCalls != 3 || fake.publishCalls != 1 {
		t.Fatalf("retried publication = %#v, %v; preflight=%d send=%d", post, err, fake.getChatCalls, fake.publishCalls)
	}
}

func TestExhaustedPublicationPreflightStopsAndNotifies(t *testing.T) {
	t.Parallel()
	fake := &fakeMAX{}
	application, storage, post := publicationTestTarget(t, fake)
	fake.getChatErr = &maxclient.Error{StatusCode: http.StatusServiceUnavailable, Message: "temporary unavailable"}
	now := time.Now().UTC()
	if _, err := storage.SetPostScheduled(context.Background(), post.ID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	application.publishDueAt(context.Background(), now)
	post, err := storage.GetPost(context.Background(), post.ID)
	if err != nil || post.Status != store.PostStatusFailed || post.ScheduledAt != nil || !strings.Contains(post.LastError, "Пост не отправлен в MAX") {
		t.Fatalf("exhausted preflight = %#v, %v", post, err)
	}
	if fake.getChatCalls != 3 || fake.publishCalls != 0 {
		t.Fatalf("MAX calls: preflight=%d send=%d", fake.getChatCalls, fake.publishCalls)
	}
	notifications, err := storage.ListNotifications(context.Background(), post.UserID, post.WorkspaceID, false, 20, 0)
	if err != nil || len(notifications) != 1 || notifications[0].Kind != "publication.failed" || notifications[0].Body != post.LastError {
		t.Fatalf("failure notification = %#v, %v", notifications, err)
	}
	// A later healthy tick cannot restart an exhausted failed attempt forever.
	fake.getChatErr = nil
	application.publishDueAt(context.Background(), now.Add(time.Minute))
	if fake.getChatCalls != 3 || fake.publishCalls != 0 {
		t.Fatal("failed preflight was automatically sent by another scheduler tick")
	}
	if err := storage.NotifyPublicationFailure(context.Background(), post); err != nil {
		t.Fatal(err)
	}
	notifications, err = storage.ListNotifications(context.Background(), post.UserID, post.WorkspaceID, false, 20, 0)
	if err != nil || len(notifications) != 1 {
		t.Fatalf("re-observing failure duplicated the notification: %#v, %v", notifications, err)
	}
}

func TestPublicationDoesNotRetryPermanentPreflightOrMessageRequests(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"preflight", "send"} {
		t.Run(phase, func(t *testing.T) {
			fake := &fakeMAX{}
			application, storage, post := publicationTestTarget(t, fake)
			wantSend := 0
			if phase == "preflight" {
				fake.getChatErr = &maxclient.Error{StatusCode: http.StatusForbidden}
			} else {
				wantSend = 1
				fake.publishFn = func(context.Context, maxclient.PublishRequest) (maxclient.Message, error) {
					return maxclient.Message{}, &maxclient.Error{StatusCode: http.StatusServiceUnavailable}
				}
			}
			if _, err := application.PublishPost(context.Background(), post.ID); err == nil {
				t.Fatal("expected MAX failure")
			}
			post, err := storage.GetPost(context.Background(), post.ID)
			if err != nil || post.Status != store.PostStatusFailed || fake.getChatCalls != 1 || fake.publishCalls != wantSend {
				t.Fatalf("unsafe retry: %#v, %v; preflight=%d send=%d", post, err, fake.getChatCalls, fake.publishCalls)
			}
		})
	}
}

func TestPublicationRespectsLongPreflightProviderCooldown(t *testing.T) {
	t.Parallel()
	fake := &fakeMAX{}
	application, storage, post := publicationTestTarget(t, fake)
	fake.getChatErr = &maxclient.Error{StatusCode: http.StatusTooManyRequests, RetryAfter: time.Hour}
	if _, err := application.PublishPost(context.Background(), post.ID); err == nil {
		t.Fatal("expected provider cooldown")
	}
	post, err := storage.GetPost(context.Background(), post.ID)
	if err != nil || post.Status != store.PostStatusFailed || fake.getChatCalls != 1 || fake.publishCalls != 0 {
		t.Fatalf("long cooldown was shortened or sent a message: %#v, %v; preflight=%d send=%d", post, err, fake.getChatCalls, fake.publishCalls)
	}
}

func TestParticipantStatsResolveNullableOwnerAndRejectOrdinaryAdmin(t *testing.T) {
	t.Parallel()
	for _, verifiedOwner := range []bool{true, false} {
		t.Run(map[bool]string{true: "owner", false: "ordinary-admin"}[verifiedOwner], func(t *testing.T) {
			fake := &fakeMAX{chat: maxclient.ChatInfo{ChatID: "nullable-owner-stats", Type: "channel", Status: "active", ParticipantsCount: 42},
				admins: []maxclient.ChatMember{{UserID: 123, IsAdmin: true, IsOwner: verifiedOwner}}}
			fake.getChatFn = func(string) (maxclient.ChatInfo, error) { return fake.chat, nil }
			application, storage := newTestApp(t, fake)
			channel, err := storage.CreateChannel(context.Background(), store.Channel{
				MAXChatID: fake.chat.ChatID, VerifiedMAXOwnerID: "123", Title: "Stats", IsChannel: true, Active: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			application.now = func() time.Time { return now }
			application.syncDueChannelParticipantStats(context.Background(), now)
			channel, err = storage.GetChannel(context.Background(), channel.ID)
			wantCount := 0
			if verifiedOwner {
				wantCount = 42
			}
			if err != nil || channel.ParticipantsCount != wantCount || fake.getAdminsCalls != 1 || fake.memberCalls != 0 {
				t.Fatalf("nullable owner participant stats = %#v, %v; admins=%d membership=%d", channel, err, fake.getAdminsCalls, fake.memberCalls)
			}
		})
	}
}
