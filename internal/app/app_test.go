package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

type fakeMAX struct {
	chat               maxclient.ChatInfo
	membership         maxclient.Membership
	getChatErr         error
	getLinkErr         error
	getChatCalls       int
	getLinkCalls       int
	lastChatLink       string
	memberCalls        int
	resolveCalls       int
	publishCalls       int
	editCalls          int
	deleteCalls        int
	uploadCalls        int
	lastPublishRequest maxclient.PublishRequest
	lastEditRequest    maxclient.EditRequest
	publishMessage     maxclient.Message
	message            maxclient.Message
	pinnedMessage      *maxclient.Message
	getMessageErr      error
	getMessageErrs     []error
	getPinnedErr       error
	editErr            error
	getMessageCalls    int
	getPinnedCalls     int
	pinCalls           int
	unpinCalls         int
}

func (f *fakeMAX) GetMe(context.Context) (maxclient.BotInfo, error) {
	return maxclient.BotInfo{UserID: 1, Username: "studio_bot", IsBot: true}, nil
}
func (f *fakeMAX) GetChat(context.Context, string) (maxclient.ChatInfo, error) {
	f.getChatCalls++
	if f.getChatErr != nil {
		return maxclient.ChatInfo{}, f.getChatErr
	}
	chat := f.chat
	if chat.OwnerID == "" {
		chat.OwnerID = "test-max-owner"
	}
	return chat, nil
}
func (f *fakeMAX) GetChatByLink(_ context.Context, link string) (maxclient.ChatInfo, error) {
	f.getLinkCalls++
	f.lastChatLink = link
	if f.getLinkErr != nil {
		return maxclient.ChatInfo{}, f.getLinkErr
	}
	chat := f.chat
	if chat.OwnerID == "" {
		chat.OwnerID = "test-max-owner"
	}
	return chat, nil
}
func (f *fakeMAX) ResolveChat(context.Context, string) (maxclient.ChatInfo, error) {
	f.resolveCalls++
	return f.chat, nil
}
func (f *fakeMAX) GetMembership(context.Context, string) (maxclient.Membership, error) {
	f.memberCalls++
	return f.membership, nil
}
func (f *fakeMAX) GetMessage(context.Context, string) (maxclient.Message, error) {
	f.getMessageCalls++
	if f.getMessageCalls <= len(f.getMessageErrs) {
		return f.message, f.getMessageErrs[f.getMessageCalls-1]
	}
	return f.message, f.getMessageErr
}
func (f *fakeMAX) GetPinnedMessage(context.Context, string) (*maxclient.Message, error) {
	f.getPinnedCalls++
	return f.pinnedMessage, f.getPinnedErr
}
func (f *fakeMAX) PinMessage(context.Context, string, string) error {
	f.pinCalls++
	return nil
}
func (f *fakeMAX) UnpinMessage(context.Context, string) error {
	f.unpinCalls++
	return nil
}
func (f *fakeMAX) SendClaimConfirmation(context.Context, string, string, string, string, string, string, string) error {
	return nil
}
func (f *fakeMAX) AnswerCallback(context.Context, string, string, string) error { return nil }
func (f *fakeMAX) UploadImage(context.Context, string, io.Reader) (maxclient.UploadResult, error) {
	f.uploadCalls++
	return maxclient.UploadResult{Token: "image-token"}, nil
}
func (f *fakeMAX) Publish(_ context.Context, request maxclient.PublishRequest) (maxclient.Message, error) {
	f.publishCalls++
	f.lastPublishRequest = request
	if f.publishMessage.MessageID != "" {
		return f.publishMessage, nil
	}
	return maxclient.Message{MessageID: "mid-1"}, nil
}
func (f *fakeMAX) Edit(_ context.Context, request maxclient.EditRequest) error {
	f.editCalls++
	f.lastEditRequest = request
	return f.editErr
}
func (f *fakeMAX) Delete(context.Context, string) error {
	f.deleteCalls++
	return nil
}

func TestConnectChannelAndDiagnosticsAreReadOnly(t *testing.T) {
	t.Parallel()
	fake := &fakeMAX{
		chat: maxclient.ChatInfo{
			ChatID: "-123", Type: "channel", Status: "active", Title: "Official", Link: "https://max.ru/official",
			Icon: maxclient.ChatIcon{URL: "https://cdn.max.ru/official.png"}, ParticipantsCount: 3210,
		},
		membership: maxclient.Membership{
			IsAdmin: true,
			Permissions: []maxclient.Permission{
				maxclient.PermissionReadAllMessages, maxclient.PermissionWrite, maxclient.PermissionEdit, maxclient.PermissionDelete,
			},
		},
	}
	application, storage := newTestApp(t, fake)

	check, err := application.ConnectChannel(context.Background(), "https://max.ru/official", "", "Ignored title")
	if err != nil {
		t.Fatal(err)
	}
	if check.Channel.MAXChatID != "-123" || check.Channel.Title != "Official" || check.Channel.PublicLink != "https://max.ru/official" ||
		check.Channel.IconURL != "https://cdn.max.ru/official.png" || check.Channel.ParticipantsCount != 3210 {
		t.Fatalf("unexpected channel: %#v", check.Channel)
	}
	if !check.Diagnostics.CanPublish || !check.Diagnostics.CanEdit || !check.Diagnostics.CanDelete {
		t.Fatalf("unexpected diagnostics: %#v", check.Diagnostics)
	}
	if fake.resolveCalls != 0 || fake.getChatCalls != 1 || fake.memberCalls != 1 || fake.publishCalls != 0 {
		t.Fatalf("unexpected MAX calls: %#v", fake)
	}

	readOnlyCheck, err := application.TestChannel(context.Background(), check.Channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !readOnlyCheck.Diagnostics.CanPublish || fake.publishCalls != 0 || fake.getChatCalls != 2 {
		t.Fatalf("test channel was not read-only: check=%#v fake=%#v", readOnlyCheck, fake)
	}
	stored, err := storage.GetChannel(context.Background(), check.Channel.ID)
	if err != nil || stored.PublicLink == "" || stored.IconURL != "https://cdn.max.ru/official.png" || stored.ParticipantsCount != 3210 {
		t.Fatalf("stored channel = %#v, %v", stored, err)
	}
}

func TestConnectedChannelIconIsSanitizedAndRefreshedForItsTenant(t *testing.T) {
	t.Parallel()
	fake := &fakeMAX{
		chat: maxclient.ChatInfo{
			ChatID: "-124", Type: "channel", Status: "active", Title: "Official",
			Icon: maxclient.ChatIcon{URL: "https://tracker.example/channel.png"},
		},
		membership: maxclient.Membership{
			IsAdmin: true,
			Permissions: []maxclient.Permission{
				maxclient.PermissionReadAllMessages, maxclient.PermissionWrite,
				maxclient.PermissionEdit, maxclient.PermissionDelete,
			},
		},
	}
	application, storage := newTestApp(t, fake)

	connected, err := application.ConnectChannel(context.Background(), "", "-124", "")
	if err != nil {
		t.Fatal(err)
	}
	if connected.Channel.IconURL != "" {
		t.Fatalf("untrusted MAX icon was persisted: %q", connected.Channel.IconURL)
	}

	fake.chat.Icon.URL = "https://cdn.max.ru/channels/official.png?size=256"
	fake.chat.ParticipantsCount = 73
	checked, err := application.TestChannelForUser(context.Background(), connected.Channel.UserID, connected.Channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if checked.Channel.IconURL != fake.chat.Icon.URL || checked.Channel.ParticipantsCount != 73 {
		t.Fatalf("checked channel visual metadata = %#v", checked.Channel)
	}
	stored, err := storage.GetChannelForUser(context.Background(), connected.Channel.UserID, connected.Channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.IconURL != fake.chat.Icon.URL || stored.ParticipantsCount != 73 {
		t.Fatalf("stored channel visual metadata = %#v", stored)
	}
}

func TestConnectChannelRejectsMissingRequiredPermissions(t *testing.T) {
	t.Parallel()
	fake := &fakeMAX{
		chat: maxclient.ChatInfo{ChatID: "10", Type: "channel", Status: "active", Title: "Channel"},
		membership: maxclient.Membership{
			IsAdmin: true, Permissions: []maxclient.Permission{maxclient.PermissionReadAllMessages},
		},
	}
	application, storage := newTestApp(t, fake)
	_, err := application.ConnectChannel(context.Background(), "", "10", "")
	var accessErr *ChannelAccessError
	if !errors.As(err, &accessErr) || accessErr.Diagnostics.CanPublish ||
		!contains(accessErr.Diagnostics.MissingRequiredPermissions, "write") {
		t.Fatalf("unexpected access error: %#v, %v", accessErr, err)
	}
	channels, listErr := storage.ListChannels(context.Background())
	if listErr != nil || len(channels) != 0 {
		t.Fatalf("channel was stored despite failed permissions: %#v, %v", channels, listErr)
	}
}

func TestPrepareChannelClaimDiscoversPublicLinkAndCachesObservedChat(t *testing.T) {
	t.Parallel()
	fake := &fakeMAX{
		membership: maxclient.Membership{
			IsAdmin: true,
			Permissions: []maxclient.Permission{
				maxclient.PermissionReadAllMessages, maxclient.PermissionWrite,
			},
		},
	}
	application, storage := newTestApp(t, fake)
	fake.chat = maxclient.ChatInfo{
		ChatID: "-13549123", OwnerID: "777", Type: "channel", Status: "active", Title: "Тестовый канал",
		Link: "https://max.ru/se13549123_biz", Icon: maxclient.ChatIcon{URL: "https://cdn.max.ru/icon.png"},
	}

	candidate, err := application.PrepareChannelClaim(context.Background(),
		"https://max.ru/se13549123_biz?from=studio#channel", "")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Info.ChatID != "-13549123" || candidate.Bot.Username != "studio_bot" ||
		!candidate.Diagnostics.CanPublish || !candidate.Diagnostics.CanEdit || !candidate.Diagnostics.CanDelete {
		t.Fatalf("unexpected claim candidate: %#v", candidate)
	}
	if fake.getLinkCalls != 1 || fake.lastChatLink != "se13549123_biz" || fake.getChatCalls != 0 || fake.memberCalls != 1 {
		t.Fatalf("unexpected discovery calls: %#v", fake)
	}
	observed, err := storage.GetActiveObservedBotChat(context.Background(), "https://max.ru/se13549123_biz", "")
	if err != nil || observed.MAXChatID != "-13549123" || observed.MAXOwnerID != "777" {
		t.Fatalf("discovered chat was not cached: %#v, %v", observed, err)
	}

	if _, err := application.PrepareChannelClaim(context.Background(), "https://max.ru/se13549123_biz", ""); err != nil {
		t.Fatal(err)
	}
	if fake.getLinkCalls != 1 || fake.getChatCalls != 1 || fake.memberCalls != 2 {
		t.Fatalf("cached discovery was not reused: %#v", fake)
	}
}

func TestPrepareChannelClaimKeepsNumericIDRegistryOnly(t *testing.T) {
	t.Parallel()
	fake := &fakeMAX{
		membership: maxclient.Membership{IsAdmin: true, Permissions: []maxclient.Permission{
			maxclient.PermissionReadAllMessages, maxclient.PermissionWrite,
		}},
	}
	application, _ := newTestApp(t, fake)
	fake.chat = maxclient.ChatInfo{ChatID: "123", Type: "channel", Status: "active"}
	if _, err := application.PrepareChannelClaim(context.Background(), "", "123"); err == nil {
		t.Fatal("numeric MAX ID bypassed the observed-chat registry")
	}
	if fake.getLinkCalls != 0 || fake.getChatCalls != 0 || fake.memberCalls != 0 {
		t.Fatalf("numeric registry miss reached MAX API: %#v", fake)
	}
}

func TestPrepareChannelClaimRequiresChannelEventWhenMAXCannotResolvePublicLink(t *testing.T) {
	t.Parallel()
	fake := &fakeMAX{getLinkErr: &maxclient.Error{
		StatusCode: 404,
		Code:       "chat.not.found",
		Message:    "Chat not found by link: se13549123_biz",
	}}
	application, _ := newTestApp(t, fake)

	_, err := application.PrepareChannelClaim(context.Background(), "https://max.ru/se13549123_biz", "")
	if !errors.Is(err, ErrMAXChannelEventRequired) {
		t.Fatalf("PrepareChannelClaim() error = %v, want ErrMAXChannelEventRequired", err)
	}
	if fake.getLinkCalls != 1 || fake.lastChatLink != "se13549123_biz" || fake.getChatCalls != 0 || fake.memberCalls != 0 {
		t.Fatalf("unexpected MAX calls after chat.not.found: %#v", fake)
	}
}

func TestPrepareChannelClaimPreservesOtherMAXErrors(t *testing.T) {
	t.Parallel()
	upstream := &maxclient.Error{StatusCode: 500, Code: "chat.not.found", Message: "temporary failure"}
	fake := &fakeMAX{getLinkErr: upstream}
	application, _ := newTestApp(t, fake)

	_, err := application.PrepareChannelClaim(context.Background(), "https://max.ru/se13549123_biz", "")
	if !errors.Is(err, upstream) || errors.Is(err, ErrMAXChannelEventRequired) {
		t.Fatalf("PrepareChannelClaim() error = %v, want original upstream error", err)
	}
}

func TestPrepareChannelClaimPreservesPublicFallbackDeadline(t *testing.T) {
	t.Parallel()
	upstream := &maxclient.Error{
		StatusCode: 404,
		Code:       "chat.not.found",
		Message:    "Chat not found by link: se13549123_biz",
	}
	fake := &fakeMAX{getLinkErr: errors.Join(context.DeadlineExceeded, upstream)}
	application, _ := newTestApp(t, fake)

	_, err := application.PrepareChannelClaim(context.Background(), "https://max.ru/se13549123_biz", "")
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrMAXChannelEventRequired) {
		t.Fatalf("PrepareChannelClaim() error = %v, want preserved deadline", err)
	}
}

func TestDiscoverMAXChatFromMessageSkipsKnownChannelRetries(t *testing.T) {
	t.Parallel()
	fake := &fakeMAX{}
	application, storage := newTestApp(t, fake)
	fake.chat = maxclient.ChatInfo{
		ChatID: "-70801090403050", OwnerID: "123456789", Type: "channel", Status: "active",
		Title: "Канал из события", Link: "https://max.ru/official_channel",
	}
	eventAt := time.Now().UTC().Truncate(time.Microsecond)

	if err := application.DiscoverMAXChatFromMessage(context.Background(), fake.chat.ChatID, eventAt); err != nil {
		t.Fatal(err)
	}
	if err := application.DiscoverMAXChatFromMessage(context.Background(), fake.chat.ChatID, eventAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if fake.getChatCalls != 1 {
		t.Fatalf("GetChat calls = %d, want one discovery call for webhook retries", fake.getChatCalls)
	}
	observed, err := storage.GetActiveObservedBotChat(context.Background(), "", fake.chat.ChatID)
	if err != nil || observed.MAXChatID != fake.chat.ChatID || !observed.Active {
		t.Fatalf("observed channel = %#v, %v", observed, err)
	}
}

func TestConnectDiscoverableChannelRejectsForeignInventoryBeforeMAX(t *testing.T) {
	t.Parallel()
	fake := &fakeMAX{
		chat: maxclient.ChatInfo{ChatID: "200", OwnerID: "777", Type: "channel", Status: "active"},
		membership: maxclient.Membership{IsAdmin: true, Permissions: []maxclient.Permission{
			maxclient.PermissionReadAllMessages, maxclient.PermissionWrite,
			maxclient.PermissionEdit, maxclient.PermissionDelete,
		}},
	}
	application, storage := newTestApp(t, fake)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	attempt := store.MAXIdentityLinkAttempt{
		ID: "discoverable-owner-link", TokenHash: strings.Repeat("a", 64), UserID: "test-owner",
		RequesterLabel: "Test Owner", ComparisonCode: "123456", CreatedAt: now,
		ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := storage.CreateMAXIdentityLinkAttempt(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	confirmHash := strings.Repeat("b", 64)
	if _, _, err := storage.StartMAXIdentityLinkConfirmation(ctx, attempt.TokenHash, "777",
		confirmHash, strings.Repeat("c", 64), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ConfirmMAXIdentityLink(ctx, confirmHash, "777", true, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := storage.UpsertObservedBotChat(ctx, store.ObservedBotChat{
		MAXChatID: "200", MAXOwnerID: "999", Title: "Foreign", Active: true, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := application.ConnectDiscoverableChannelForUser(ctx, "test-owner", "200"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign discoverable channel error = %v, want ErrNotFound", err)
	}
	if fake.getChatCalls != 0 || fake.memberCalls != 0 {
		t.Fatalf("foreign inventory reached MAX API: %#v", fake)
	}
	if _, err := application.ConnectDiscoverableChannelForUser(ctx, "test-owner", "201"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown discoverable channel error = %v, want ErrNotFound", err)
	}
	if fake.getChatCalls != 0 || fake.memberCalls != 0 {
		t.Fatalf("unknown inventory reached MAX API: %#v", fake)
	}
}

func TestPrepareChannelClaimRejectsNonChannelBeforeCaching(t *testing.T) {
	t.Parallel()
	fake := &fakeMAX{}
	application, storage := newTestApp(t, fake)
	fake.chat = maxclient.ChatInfo{
		ChatID: "321", OwnerID: "777", Type: "chat", Status: "active", Title: "Not a channel",
		Link: "https://max.ru/not_channel",
	}
	if _, err := application.PrepareChannelClaim(context.Background(), "https://max.ru/not_channel", ""); err == nil {
		t.Fatal("public group chat was accepted as a channel")
	}
	if fake.getLinkCalls != 1 || fake.memberCalls != 0 {
		t.Fatalf("non-channel discovery continued to membership: %#v", fake)
	}
	if _, err := storage.GetActiveObservedBotChat(context.Background(), "https://max.ru/not_channel", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rejected non-channel was cached: %v", err)
	}
}

func TestScheduleValidatesLocallyAndPublishRechecksPermissions(t *testing.T) {
	t.Parallel()
	fake := &fakeMAX{
		chat: maxclient.ChatInfo{ChatID: "20", Type: "channel", Status: "active", Title: "Channel"},
		membership: maxclient.Membership{
			IsAdmin: true, Permissions: []maxclient.Permission{maxclient.PermissionReadAllMessages},
		},
	}
	application, storage := newTestApp(t, fake)
	channel, err := storage.CreateChannel(context.Background(), store.Channel{
		MAXChatID: "20", Title: "Channel", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(context.Background(), store.Post{
		Title: "Post", Content: "body", Format: store.FormatMarkdown, Status: store.PostStatusDraft,
		ChannelID: &channel.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err = application.SchedulePost(context.Background(), post.ID, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if post.Status != store.PostStatusScheduled || fake.getChatCalls != 0 || fake.memberCalls != 0 {
		t.Fatalf("schedule unexpectedly called MAX: post=%#v fake=%#v", post, fake)
	}
	_, err = application.PublishPost(context.Background(), post.ID)
	var accessErr *ChannelAccessError
	if !errors.As(err, &accessErr) {
		t.Fatalf("PublishPost() error = %v, want ChannelAccessError", err)
	}
	post, err = storage.GetPost(context.Background(), post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if post.Status != store.PostStatusFailed || post.ScheduledAt != nil || fake.publishCalls != 0 {
		t.Fatalf("permission failure state = %#v, fake=%#v", post, fake)
	}

	tooLong, err := storage.CreatePost(context.Background(), store.Post{
		Title: "Invalid", Content: strings.Repeat("я", 4001), Format: store.FormatMarkdown,
		Status: store.PostStatusDraft, ChannelID: &channel.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.SchedulePost(context.Background(), tooLong.ID, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("SchedulePost accepted content longer than MAX limit")
	}
	tooLong, _ = storage.GetPost(context.Background(), tooLong.ID)
	if tooLong.Status != store.PostStatusDraft {
		t.Fatalf("invalid post status = %q, want draft", tooLong.Status)
	}
}

func TestPublishedMutationsRecheckEditAndDeletePermissions(t *testing.T) {
	t.Parallel()
	fake := &fakeMAX{
		chat: maxclient.ChatInfo{ChatID: "30", Type: "channel", Status: "active", Title: "Channel"},
		membership: maxclient.Membership{
			IsAdmin:     true,
			Permissions: []maxclient.Permission{maxclient.PermissionReadAllMessages},
		},
	}
	application, storage := newTestApp(t, fake)
	channel, err := storage.CreateChannel(context.Background(), store.Channel{
		MAXChatID: "30", Title: "Channel", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(context.Background(), store.Post{
		Title: "Published", Content: "body", Format: store.FormatMarkdown, Status: store.PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: "mid-30",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.UpdatePublishedPost(context.Background(), post.ID); err == nil {
		t.Fatal("UpdatePublishedPost accepted missing edit permission")
	}
	if _, err := application.DeletePublication(context.Background(), post.ID); err == nil {
		t.Fatal("DeletePublication accepted missing delete permission")
	}
	if fake.editCalls != 0 || fake.deleteCalls != 0 {
		t.Fatalf("mutation reached MAX without permission: %#v", fake)
	}
}

func TestUpdatePublishedPostReconcilesMessageDeletedInMAX(t *testing.T) {
	t.Parallel()
	notFound := &maxclient.Error{StatusCode: http.StatusNotFound, Code: "message.not.found", Message: "Message not found"}
	tests := []struct {
		name               string
		getMessageErrs     []error
		getMessageErr      error
		editErr            error
		wantGetMessageCall int
		wantEditCall       int
	}{
		{
			name: "deleted before edit", getMessageErr: notFound,
			wantGetMessageCall: 1, wantEditCall: 0,
		},
		{
			name: "deleted during edit", getMessageErrs: []error{nil, notFound},
			editErr:            &maxclient.Error{StatusCode: http.StatusOK, Code: "operation_failed", Message: "Error on message edit"},
			wantGetMessageCall: 2, wantEditCall: 1,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeMAX{
				chat: maxclient.ChatInfo{ChatID: "31", Type: "channel", Status: "active", Title: "Channel"},
				membership: maxclient.Membership{IsAdmin: true, Permissions: []maxclient.Permission{
					maxclient.PermissionReadAllMessages, maxclient.PermissionWrite, maxclient.PermissionEdit,
				}},
				message:       maxclient.Message{MessageID: "mid-31", ChatID: "31"},
				getMessageErr: test.getMessageErr, getMessageErrs: test.getMessageErrs, editErr: test.editErr,
			}
			application, storage := newTestApp(t, fake)
			channel, err := storage.CreateChannel(context.Background(), store.Channel{
				MAXChatID: "31", Title: "Channel", IsChannel: true, Active: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			publishedAt := time.Date(2039, time.May, 6, 7, 8, 9, 0, time.UTC)
			post, err := storage.CreatePost(context.Background(), store.Post{
				Title: "Published", Content: "body", Format: store.FormatMarkdown, Status: store.PostStatusPublished,
				ChannelID: &channel.ID, MAXMessageID: "mid-31", MAXMessageURL: "https://max.ru/channel/message",
				PublishedAt: &publishedAt,
			})
			if err != nil {
				t.Fatal(err)
			}
			post, err = application.UpdatePublishedPost(context.Background(), post.ID)
			if err != nil {
				t.Fatal(err)
			}
			if post.Status != store.PostStatusFailed || post.LastError != store.MAXPublicationMissingLastError ||
				post.MAXMessageID != "" || post.MAXMessageURL != "" || post.PublishedAt == nil || !post.PublishedAt.Equal(publishedAt) {
				t.Fatalf("reconciled post = %#v", post)
			}
			if fake.getMessageCalls != test.wantGetMessageCall || fake.editCalls != test.wantEditCall {
				t.Fatalf("MAX calls = get %d edit %d, want get %d edit %d", fake.getMessageCalls, fake.editCalls,
					test.wantGetMessageCall, test.wantEditCall)
			}
			post, err = application.UpdatePublishedPost(context.Background(), post.ID)
			if err != nil || !isStoredMAXPublicationMissing(post) ||
				fake.getMessageCalls != test.wantGetMessageCall || fake.editCalls != test.wantEditCall {
				t.Fatalf("idempotent update = %#v, err=%v, MAX=%#v", post, err, fake)
			}
		})
	}
}

func TestScheduleRejectsPastAndNormalizesOffsetWithoutCallingMAX(t *testing.T) {
	t.Parallel()
	fake := &fakeMAX{
		chat: maxclient.ChatInfo{ChatID: "40", Type: "channel", Status: "active", Title: "Channel"},
		membership: maxclient.Membership{
			IsAdmin:     true,
			Permissions: []maxclient.Permission{maxclient.PermissionReadAllMessages, maxclient.PermissionWrite},
		},
	}
	application, storage := newTestApp(t, fake)
	now := time.Date(2032, time.May, 1, 10, 0, 0, 0, time.UTC)
	application.now = func() time.Time { return now }
	channel, err := storage.CreateChannel(context.Background(), store.Channel{
		MAXChatID: "40", Title: "Channel", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(context.Background(), store.Post{
		Title: "Calendar", Content: "body", Format: store.FormatMarkdown, ChannelID: &channel.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.SchedulePost(context.Background(), post.ID, now); err == nil {
		t.Fatal("SchedulePost accepted a non-future timestamp")
	}
	moscowTime := now.Add(2 * time.Hour).In(time.FixedZone("MSK", 3*60*60))
	post, err = application.SchedulePost(context.Background(), post.ID, moscowTime)
	if err != nil {
		t.Fatal(err)
	}
	if post.Status != store.PostStatusScheduled || post.ScheduledAt == nil ||
		post.ScheduledAt.Location() != time.UTC || !post.ScheduledAt.Equal(moscowTime) {
		t.Fatalf("scheduled post = %#v", post)
	}
	if fake.getChatCalls != 0 || fake.memberCalls != 0 || fake.publishCalls != 0 {
		t.Fatalf("scheduling made remote MAX calls: %#v", fake)
	}
}

func TestSchedulerPublishesOnlyPostsStillDueAtAtomicClaim(t *testing.T) {
	t.Parallel()
	fake := &fakeMAX{
		chat: maxclient.ChatInfo{ChatID: "50", Type: "channel", Status: "active", Title: "Channel"},
		membership: maxclient.Membership{
			IsAdmin:     true,
			Permissions: []maxclient.Permission{maxclient.PermissionReadAllMessages, maxclient.PermissionWrite},
		},
	}
	application, storage := newTestApp(t, fake)
	now := time.Date(2033, time.June, 2, 12, 0, 0, 0, time.UTC)
	application.now = func() time.Time { return now }
	channel, err := storage.CreateChannel(context.Background(), store.Channel{
		MAXChatID: "50", Title: "Channel", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	newPost := func(title string) store.Post {
		created, createErr := storage.CreatePost(context.Background(), store.Post{
			Title: title, Content: "body", Format: store.FormatMarkdown, ChannelID: &channel.ID, Notify: true,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return created
	}
	due := newPost("Due")
	if due, err = storage.SetPostScheduled(context.Background(), due.ID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	future := newPost("Future")
	if future, err = storage.SetPostScheduled(context.Background(), future.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	canceled := newPost("Canceled")
	if canceled, err = storage.SetPostScheduled(context.Background(), canceled.ID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if canceled, err = storage.CancelSchedule(context.Background(), canceled.ID); err != nil {
		t.Fatal(err)
	}

	application.publishDueAt(context.Background(), now)
	due, _ = storage.GetPost(context.Background(), due.ID)
	future, _ = storage.GetPost(context.Background(), future.ID)
	canceled, _ = storage.GetPost(context.Background(), canceled.ID)
	if due.Status != store.PostStatusPublished || due.ScheduledAt != nil || due.MAXMessageID == "" {
		t.Fatalf("due post = %#v", due)
	}
	if future.Status != store.PostStatusScheduled || future.ScheduledAt == nil {
		t.Fatalf("future post = %#v", future)
	}
	if canceled.Status != store.PostStatusDraft || canceled.ScheduledAt != nil {
		t.Fatalf("canceled post = %#v", canceled)
	}
	if fake.publishCalls != 1 {
		t.Fatalf("MAX publish calls = %d, want 1", fake.publishCalls)
	}
}

func TestSelectedCalendarPostSkippedAfterPostponeOrCancel(t *testing.T) {
	t.Parallel()
	fake := &fakeMAX{
		chat: maxclient.ChatInfo{ChatID: "60", Type: "channel", Status: "active", Title: "Channel"},
		membership: maxclient.Membership{
			IsAdmin:     true,
			Permissions: []maxclient.Permission{maxclient.PermissionReadAllMessages, maxclient.PermissionWrite},
		},
	}
	application, storage := newTestApp(t, fake)
	now := time.Date(2034, time.July, 3, 9, 0, 0, 0, time.UTC)
	channel, err := storage.CreateChannel(context.Background(), store.Channel{
		MAXChatID: "60", Title: "Channel", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	newDuePost := func(title string) store.Post {
		post, createErr := storage.CreatePost(context.Background(), store.Post{
			Title: title, Content: "body", Format: store.FormatMarkdown, ChannelID: &channel.ID,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		post, createErr = storage.SetPostScheduled(context.Background(), post.ID, now.Add(-time.Minute))
		if createErr != nil {
			t.Fatal(createErr)
		}
		return post
	}

	postponed := newDuePost("Postponed")
	selected, err := storage.DuePostIDs(context.Background(), now, 10)
	if err != nil || len(selected) != 1 || selected[0] != postponed.ID {
		t.Fatalf("selected postponed IDs = %v, error = %v", selected, err)
	}
	if _, err := storage.SetPostScheduled(context.Background(), postponed.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if published, err := application.publishScheduledPost(context.Background(), selected[0], now); err != nil || published {
		t.Fatal(err)
	}

	canceled := newDuePost("Canceled")
	selected, err = storage.DuePostIDs(context.Background(), now, 10)
	if err != nil || len(selected) != 1 || selected[0] != canceled.ID {
		t.Fatalf("selected canceled IDs = %v, error = %v", selected, err)
	}
	if _, err := storage.CancelSchedule(context.Background(), canceled.ID); err != nil {
		t.Fatal(err)
	}
	if published, err := application.publishScheduledPost(context.Background(), selected[0], now); err != nil || published {
		t.Fatal(err)
	}
	if fake.publishCalls != 0 || fake.getChatCalls != 0 || fake.memberCalls != 0 {
		t.Fatalf("postponed/canceled selection reached MAX: %#v", fake)
	}
}

func TestPublishAndEditCarryLinkButtonsWithReuploadedImage(t *testing.T) {
	t.Parallel()
	fake := &fakeMAX{
		chat:    maxclient.ChatInfo{ChatID: "70", Type: "channel", Status: "active", Title: "Channel"},
		message: maxclient.Message{MessageID: "mid.buttons", ChatID: "70"},
		membership: maxclient.Membership{
			IsAdmin: true,
			Permissions: []maxclient.Permission{
				maxclient.PermissionReadAllMessages, maxclient.PermissionWrite, maxclient.PermissionEdit,
			},
		},
		publishMessage: maxclient.Message{MessageID: "mid.buttons", URL: "https://max.ru/channel/buttons"},
	}
	application, storage := newTestApp(t, fake)
	channel, err := storage.CreateChannel(context.Background(), store.Channel{
		MAXChatID: "70", Title: "Channel", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(t.TempDir(), "post.png")
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(context.Background(), store.Post{
		Title: "Buttons", Content: "Body", Format: store.FormatMarkdown, ChannelID: &channel.ID,
		ImageURL: "http://localhost:8080/media/post.png", ImagePath: imagePath, Notify: true,
		LinkButtons: []store.LinkButton{
			{Text: "  Сайт ", URL: " https://example.com "},
			{Text: "Каталог", URL: "https://example.com/catalog"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err = application.PublishPost(context.Background(), post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if post.Status != store.PostStatusPublished || post.MAXMessageURL != fake.publishMessage.URL || fake.publishCalls != 1 || fake.uploadCalls != 1 {
		t.Fatalf("publish state = %#v, fake = %#v", post, fake)
	}
	if len(fake.lastPublishRequest.ImageTokens) != 1 || len(fake.lastPublishRequest.LinkButtons) != 2 ||
		fake.lastPublishRequest.LinkButtons[0].Text != "Сайт" || fake.lastPublishRequest.LinkButtons[0].URL != "https://example.com" {
		t.Fatalf("publish request = %#v", fake.lastPublishRequest)
	}

	cleared := []store.LinkButton{}
	if _, err := storage.UpdatePost(context.Background(), post.ID, store.PostChanges{LinkButtons: &cleared}); err != nil {
		t.Fatal(err)
	}
	if _, err := application.UpdatePublishedPost(context.Background(), post.ID); err != nil {
		t.Fatal(err)
	}
	if fake.editCalls != 1 || fake.uploadCalls != 2 || len(fake.lastEditRequest.ImageTokens) != 1 {
		t.Fatalf("edit request = %#v, fake = %#v", fake.lastEditRequest, fake)
	}
	if fake.lastEditRequest.LinkButtons == nil || len(fake.lastEditRequest.LinkButtons) != 0 {
		t.Fatalf("edit LinkButtons = %#v, want explicit empty slice", fake.lastEditRequest.LinkButtons)
	}
}

func TestScheduleRejectsIncompleteLinkButtons(t *testing.T) {
	t.Parallel()
	application, storage := newTestApp(t, &fakeMAX{})
	channel, err := storage.CreateChannel(context.Background(), store.Channel{
		MAXChatID: "71", Title: "Channel", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	post, err := storage.CreatePost(context.Background(), store.Post{
		Title: "Draft", Content: "Body", Format: store.FormatMarkdown, ChannelID: &channel.ID,
		LinkButtons: []store.LinkButton{{Text: "Подробнее", URL: "https://"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.SchedulePost(context.Background(), post.ID, time.Now().UTC().Add(time.Hour)); err == nil {
		t.Fatal("SchedulePost accepted an incomplete HTTPS URL")
	}
	stored, err := storage.GetPost(context.Background(), post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != store.PostStatusDraft {
		t.Fatalf("invalid post status = %q, want draft", stored.Status)
	}
}

func newTestApp(t *testing.T, maxClient MAXClient) (*App, *store.Store) {
	t.Helper()
	ctx := context.Background()
	storage, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	if fake, ok := maxClient.(*fakeMAX); ok && fake.chat.ChatID != "" {
		if err := storage.UpsertObservedBotChat(ctx, store.ObservedBotChat{
			MAXChatID: fake.chat.ChatID, PublicLink: fake.chat.Link, Title: fake.chat.Title,
			MAXOwnerID: "test-max-owner", Active: true, LastSeenAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	mediaStore, err := media.New(t.TempDir(), "http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(storage, mediaStore, maxClient, nil, nil, logger), storage
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
