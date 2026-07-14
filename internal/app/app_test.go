package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

type fakeMAX struct {
	chat         maxclient.ChatInfo
	membership   maxclient.Membership
	getChatCalls int
	getLinkCalls int
	lastChatLink string
	memberCalls  int
	resolveCalls int
	publishCalls int
	editCalls    int
	deleteCalls  int
}

func (f *fakeMAX) GetMe(context.Context) (maxclient.BotInfo, error) {
	return maxclient.BotInfo{UserID: 1, Username: "studio_bot", IsBot: true}, nil
}
func (f *fakeMAX) GetChat(context.Context, string) (maxclient.ChatInfo, error) {
	f.getChatCalls++
	chat := f.chat
	if chat.OwnerID == "" {
		chat.OwnerID = "test-max-owner"
	}
	return chat, nil
}
func (f *fakeMAX) GetChatByLink(_ context.Context, link string) (maxclient.ChatInfo, error) {
	f.getLinkCalls++
	f.lastChatLink = link
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
func (f *fakeMAX) SendClaimConfirmation(context.Context, string, string, string, string, string, string, string) error {
	return nil
}
func (f *fakeMAX) AnswerCallback(context.Context, string, string) error { return nil }
func (f *fakeMAX) UploadImage(context.Context, string, io.Reader) (maxclient.UploadResult, error) {
	return maxclient.UploadResult{Token: "image-token"}, nil
}
func (f *fakeMAX) Publish(context.Context, maxclient.PublishRequest) (maxclient.Message, error) {
	f.publishCalls++
	return maxclient.Message{MessageID: "mid-1"}, nil
}
func (f *fakeMAX) Edit(context.Context, maxclient.EditRequest) error {
	f.editCalls++
	return nil
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
