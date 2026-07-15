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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/media"
	"maxpilot/backend/internal/store"
)

type fakeMAX struct {
	chat               maxclient.ChatInfo
	admins             []maxclient.ChatMember
	membership         maxclient.Membership
	getChatFn          func(string) (maxclient.ChatInfo, error)
	getChatErr         error
	getAdminsErr       error
	getLinkErr         error
	getChatCalls       int
	getAdminsCalls     int
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
	deleteErr          error
	getMessageCalls    int
	getPinnedCalls     int
	pinCalls           int
	unpinCalls         int
}

type blockingRefreshMAX struct {
	*fakeMAX
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
	calls     atomic.Int32
}

func (f *blockingRefreshMAX) GetChat(ctx context.Context, chatID string) (maxclient.ChatInfo, error) {
	f.calls.Add(1)
	f.startOnce.Do(func() { close(f.started) })
	select {
	case <-ctx.Done():
		return maxclient.ChatInfo{}, context.Cause(ctx)
	case <-f.release:
	}
	chat := f.chat
	if chat.ChatID == "" {
		chat.ChatID = chatID
	}
	return chat, nil
}

type recordingMetrics struct {
	mu          sync.Mutex
	publication map[string]int
	jobs        map[string]int
	due         map[string]int
	cycles      int
}

func newRecordingMetrics() *recordingMetrics {
	return &recordingMetrics{publication: map[string]int{}, jobs: map[string]int{}, due: map[string]int{}}
}

func (m *recordingMetrics) ObservePublicationOperation(operation, outcome string, _ time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publication[operation+":"+outcome]++
}

func (m *recordingMetrics) ObserveSchedulerJob(job, outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[job+":"+outcome]++
}

func (m *recordingMetrics) SetSchedulerDue(job string, count int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.due[job] = count
}

func (m *recordingMetrics) ObserveSchedulerCycle(time.Duration, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cycles++
}

func (m *recordingMetrics) AddRecoveredPublications(int64) {}

func (f *fakeMAX) GetMe(context.Context) (maxclient.BotInfo, error) {
	return maxclient.BotInfo{UserID: 1, Username: "studio_bot", IsBot: true}, nil
}
func (f *fakeMAX) GetChat(_ context.Context, chatID string) (maxclient.ChatInfo, error) {
	f.getChatCalls++
	if f.getChatFn != nil {
		return f.getChatFn(chatID)
	}
	if f.getChatErr != nil {
		return maxclient.ChatInfo{}, f.getChatErr
	}
	chat := f.chat
	if chat.OwnerID == "" {
		chat.OwnerID = "test-max-owner"
	}
	return chat, nil
}
func (f *fakeMAX) GetChatAdmins(context.Context, string) ([]maxclient.ChatMember, error) {
	f.getAdminsCalls++
	return f.admins, f.getAdminsErr
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
	return f.deleteErr
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
	capturedAt := time.Date(2041, time.August, 12, 13, 14, 15, 0, time.UTC)
	application.now = func() time.Time { return capturedAt }

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
	history, err := storage.ListChannelParticipantSnapshotsForUser(context.Background(), connected.Channel.UserID,
		connected.Channel.ID, capturedAt, capturedAt)
	if err != nil || len(history) != 1 || history[0].ParticipantsCount != 73 || !history[0].CapturedAt.Equal(capturedAt) {
		t.Fatalf("manual channel participant history = %#v, %v", history, err)
	}

	fake.chat.ChatID = "another-channel"
	fake.chat.ParticipantsCount = 9000
	if _, err := application.TestChannelForUser(context.Background(), connected.Channel.UserID, connected.Channel.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatched manual participant refresh error = %v, want ErrConflict", err)
	}
	stored, err = storage.GetChannelForUser(context.Background(), connected.Channel.UserID, connected.Channel.ID)
	if err != nil || stored.ParticipantsCount != 73 {
		t.Fatalf("mismatched manual refresh changed channel = %#v, %v", stored, err)
	}
}

func TestChannelParticipantStatsWorkerRefreshesAndBacksOffWithoutMembershipLookup(t *testing.T) {
	t.Parallel()
	now := time.Date(2041, time.September, 2, 10, 0, 0, 0, time.UTC)
	fake := &fakeMAX{chat: maxclient.ChatInfo{
		ChatID: "-125", OwnerID: "test-max-owner", Type: "channel", Status: "active", Title: "Channel",
		Icon: maxclient.ChatIcon{URL: "https://cdn.max.ru/channels/worker.png"}, ParticipantsCount: 88,
	}}
	application, storage := newTestApp(t, fake)
	application.now = func() time.Time { return now }
	metrics := newRecordingMetrics()
	application.metrics = metrics
	channel, err := storage.CreateChannel(context.Background(), store.Channel{
		VerifiedMAXOwnerID: "test-max-owner", MAXChatID: fake.chat.ChatID, Title: "Channel",
		IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	application.syncDueChannelParticipantStats(context.Background(), now)
	stored, err := storage.GetChannel(context.Background(), channel.ID)
	if err != nil || stored.ParticipantsCount != 88 || stored.IconURL != fake.chat.Icon.URL {
		t.Fatalf("worker channel = %#v, %v", stored, err)
	}
	history, err := storage.ListChannelParticipantSnapshotsForUser(context.Background(), channel.UserID, channel.ID, now, now)
	if err != nil || len(history) != 1 || history[0].ParticipantsCount != 88 {
		t.Fatalf("worker participant history = %#v, %v", history, err)
	}
	if fake.getChatCalls != 1 || fake.memberCalls != 0 {
		t.Fatalf("worker MAX calls = %#v", fake)
	}
	if metrics.due["channel_participants_sync"] != 1 ||
		metrics.jobs["channel_participants_scan:success"] != 1 ||
		metrics.jobs["channel_participants_sync:success"] != 1 {
		t.Fatalf("participant worker metrics: due=%#v jobs=%#v", metrics.due, metrics.jobs)
	}

	application.now = func() time.Time { return now.Add(30 * time.Minute) }
	application.syncDueChannelParticipantStats(context.Background(), now.Add(30*time.Minute))
	if fake.getChatCalls != 1 {
		t.Fatal("participant worker synchronized a channel more than once per hour")
	}

	fake.chat.OwnerID = "another-owner"
	failedAt := now.Add(time.Hour)
	application.now = func() time.Time { return failedAt }
	application.syncDueChannelParticipantStats(context.Background(), failedAt)
	if fake.getChatCalls != 2 || metrics.jobs["channel_participants_sync:error"] != 1 {
		t.Fatalf("ownership mismatch was not rejected: fake=%#v jobs=%#v", fake, metrics.jobs)
	}
	application.now = func() time.Time { return failedAt.Add(time.Minute) }
	application.syncDueChannelParticipantStats(context.Background(), failedAt.Add(time.Minute))
	if fake.getChatCalls != 2 {
		t.Fatal("failed participant lookup was retried before the one-hour backoff")
	}
	stored, err = storage.GetChannel(context.Background(), channel.ID)
	if err != nil || stored.ParticipantsCount != 88 {
		t.Fatalf("ownership mismatch changed participant count: %#v, %v", stored, err)
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

func TestObserveMAXChatRejectsIncompleteOwnerAndMessageRetriesExistingIncompleteRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	eventAt := time.Now().UTC().Truncate(time.Microsecond)
	fake := &fakeMAX{getChatFn: func(chatID string) (maxclient.ChatInfo, error) {
		return maxclient.ChatInfo{ChatID: chatID, Type: "channel", Status: "active", Title: "Без владельца"}, nil
	}}
	application, storage := newTestApp(t, fake)
	if err := application.ObserveMAXChat(ctx, "100", true, eventAt); !errors.Is(err, ErrMAXChannelMetadataIncomplete) {
		t.Fatalf("ObserveMAXChat() error = %v, want incomplete metadata", err)
	}
	if _, err := storage.GetActiveObservedBotChat(ctx, "", "100"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("incomplete lifecycle event entered inventory: %v", err)
	}

	if err := storage.UpsertObservedBotChat(ctx, store.ObservedBotChat{
		MAXChatID: "100", Title: "Старое название", Active: true, LastSeenAt: eventAt,
	}); err != nil {
		t.Fatal(err)
	}
	fake.getChatFn = func(chatID string) (maxclient.ChatInfo, error) {
		return maxclient.ChatInfo{
			ChatID: chatID, OwnerID: "777", Type: "channel", Status: "active", Title: "Новое название",
			Link: "https://max.ru/new_channel", Icon: maxclient.ChatIcon{URL: "https://cdn.max.ru/new.png"}, ParticipantsCount: 15,
		}, nil
	}
	if err := application.DiscoverMAXChatFromMessage(ctx, "100", eventAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	observed, err := storage.GetActiveObservedBotChat(ctx, "", "100")
	if err != nil {
		t.Fatal(err)
	}
	if observed.MAXOwnerID != "777" || observed.Title != "Новое название" || observed.IconURL != "https://cdn.max.ru/new.png" {
		t.Fatalf("retried incomplete observation = %#v", observed)
	}
}

func TestRefreshDiscoverableChannelsReconcilesMetadataAndHidesForeignOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	fake := &fakeMAX{}
	application, storage := newTestApp(t, fake)
	application.now = func() time.Time { return now.Add(time.Minute) }
	linkMAXIdentityForAppTest(t, storage, "test-owner", "777", now.Add(-time.Hour))
	fake.admins = []maxclient.ChatMember{
		{UserID: 999, IsAdmin: true},
		{UserID: 777, IsOwner: true, IsAdmin: true},
	}
	for _, chat := range []store.ObservedBotChat{
		{MAXChatID: "100", Title: "Старое название", Active: true, LastSeenAt: now.Add(-time.Minute)},
		{MAXChatID: "101", Title: "Чужой канал", Active: true, LastSeenAt: now.Add(-time.Minute)},
		{MAXChatID: "102", MAXOwnerID: "777", Title: "Временно недоступен", Active: true, LastSeenAt: now.Add(-time.Minute)},
	} {
		if err := storage.UpsertObservedBotChat(ctx, chat); err != nil {
			t.Fatal(err)
		}
	}
	connected, err := storage.CreateChannel(ctx, store.Channel{
		UserID: "test-owner", VerifiedMAXOwnerID: "777", MAXChatID: "100", Title: "Старое название",
		PublicLink: "https://max.ru/old_channel", IconURL: "https://cdn.max.ru/old.png",
		IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	fake.getChatFn = func(chatID string) (maxclient.ChatInfo, error) {
		if chatID == "101" {
			return maxclient.ChatInfo{ChatID: chatID, OwnerID: "999", Type: "channel", Status: "active", Title: "Чужой канал"}, nil
		}
		if chatID == "102" {
			return maxclient.ChatInfo{}, errors.New("temporary MAX failure")
		}
		return maxclient.ChatInfo{
			ChatID: chatID, Type: "channel", Status: "active", Title: "Новое название",
			Link: "https://max.ru/new_channel", Icon: maxclient.ChatIcon{URL: "https://cdn.max.ru/new.png"}, ParticipantsCount: 42,
		}, nil
	}

	result, err := application.RefreshDiscoverableChannelsForUser(ctx, "test-owner")
	if err != nil {
		t.Fatal(err)
	}
	if result.Refreshed != 1 || result.Failed != 1 || len(result.Channels) != 2 {
		t.Fatalf("refresh result = %#v", result)
	}
	channelIDs := map[string]bool{}
	for _, channel := range result.Channels {
		channelIDs[channel.MAXChatID] = true
	}
	if !channelIDs["100"] || !channelIDs["102"] || channelIDs["101"] {
		t.Fatalf("refresh channels = %#v", result.Channels)
	}
	if fake.getAdminsCalls != 1 {
		t.Fatalf("GetChatAdmins calls = %d, want owner fallback for one channel", fake.getAdminsCalls)
	}
	updated, err := storage.GetChannel(ctx, connected.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Title != "Новое название" || updated.PublicLink != "https://max.ru/new_channel" ||
		updated.IconURL != "https://cdn.max.ru/new.png" || updated.ParticipantsCount != 42 {
		t.Fatalf("connected channel metadata = %#v", updated)
	}
	foreign, err := storage.GetActiveObservedBotChat(ctx, "", "101")
	if err != nil || foreign.MAXOwnerID != "999" {
		t.Fatalf("foreign owner was not authoritatively recorded: %#v err=%v", foreign, err)
	}
}

func TestRefreshDiscoverableChannelsCooldownCollapsesConcurrentMAXCalls(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	max := &blockingRefreshMAX{
		fakeMAX: &fakeMAX{chat: maxclient.ChatInfo{
			ChatID: "100", OwnerID: "777", Type: "channel", Status: "active", Title: "Owned",
		}},
		started: make(chan struct{}), release: make(chan struct{}),
	}
	released := false
	defer func() {
		if !released {
			close(max.release)
		}
	}()
	application, storage := newTestApp(t, max)
	var clock atomic.Int64
	clock.Store(now.UnixNano())
	application.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	linkMAXIdentityForAppTest(t, storage, "test-owner", "777", now.Add(-time.Hour))
	if err := storage.UpsertObservedBotChat(ctx, store.ObservedBotChat{
		MAXChatID: "100", MAXOwnerID: "777", Title: "Owned", Active: true, LastSeenAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	type refreshResult struct {
		value DiscoverableChannelRefresh
		err   error
	}
	firstDone := make(chan refreshResult, 1)
	go func() {
		value, err := application.RefreshDiscoverableChannelsForUser(ctx, "test-owner")
		firstDone <- refreshResult{value: value, err: err}
	}()
	select {
	case <-max.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first refresh did not reach MAX")
	}

	secondCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if _, err := application.RefreshDiscoverableChannelsForUser(secondCtx, "test-owner"); err == nil {
		t.Fatal("concurrent refresh was not rate limited")
	} else {
		var cooldownErr *DiscoverableRefreshCooldownError
		if !errors.As(err, &cooldownErr) || cooldownErr.RetryAfter != discoverableRefreshCooldown {
			t.Fatalf("concurrent refresh error = %#v", err)
		}
	}
	if calls := max.calls.Load(); calls != 1 {
		t.Fatalf("concurrent refresh MAX calls = %d, want 1", calls)
	}

	close(max.release)
	released = true
	first := <-firstDone
	if first.err != nil || first.value.Refreshed != 1 {
		t.Fatalf("first refresh = %#v err=%v", first.value, first.err)
	}
	if _, err := application.RefreshDiscoverableChannelsForUser(ctx, "test-owner"); err == nil {
		t.Fatal("immediate sequential refresh was not rate limited")
	}
	if calls := max.calls.Load(); calls != 1 {
		t.Fatalf("sequential cooldown MAX calls = %d, want 1", calls)
	}

	clock.Store(now.Add(discoverableRefreshCooldown).UnixNano())
	afterCooldown, err := application.RefreshDiscoverableChannelsForUser(ctx, "test-owner")
	if err != nil || afterCooldown.Refreshed != 1 || max.calls.Load() != 2 {
		t.Fatalf("refresh after cooldown = %#v calls=%d err=%v", afterCooldown, max.calls.Load(), err)
	}
}

func TestDiscoverableRefreshGateIsPerUserAndReportsRemainingCooldown(t *testing.T) {
	t.Parallel()
	application := &App{}
	now := time.Date(2026, time.July, 15, 13, 0, 0, 0, time.UTC)
	if err := application.beginDiscoverableRefresh("tenant-a", now); err != nil {
		t.Fatal(err)
	}
	if err := application.beginDiscoverableRefresh("tenant-b", now); err != nil {
		t.Fatalf("tenant B was blocked by tenant A: %v", err)
	}
	application.finishDiscoverableRefresh("tenant-a")
	application.finishDiscoverableRefresh("tenant-b")

	err := application.beginDiscoverableRefresh("tenant-a", now.Add(5*time.Second))
	var cooldownErr *DiscoverableRefreshCooldownError
	if !errors.As(err, &cooldownErr) || cooldownErr.RetryAfter != 10*time.Second {
		t.Fatalf("partial cooldown error = %#v", err)
	}
	if err := application.beginDiscoverableRefresh("tenant-a", now.Add(discoverableRefreshCooldown)); err != nil {
		t.Fatalf("refresh remained blocked after cooldown: %v", err)
	}
	application.finishDiscoverableRefresh("tenant-a")
}

func TestPrepareChannelClaimRefreshesExistingObservedMetadata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	fake := &fakeMAX{
		membership: maxclient.Membership{IsAdmin: true, Permissions: []maxclient.Permission{
			maxclient.PermissionReadAllMessages, maxclient.PermissionWrite,
		}},
	}
	application, storage := newTestApp(t, fake)
	fake.chat = maxclient.ChatInfo{
		ChatID: "100", OwnerID: "777", Type: "channel", Status: "active", Title: "Новое название",
		Link: "https://max.ru/new_channel", Icon: maxclient.ChatIcon{URL: "https://cdn.max.ru/new.png"}, ParticipantsCount: 22,
	}
	application.now = func() time.Time { return now.Add(time.Minute) }
	if err := storage.UpsertObservedBotChat(ctx, store.ObservedBotChat{
		MAXChatID: "100", PublicLink: "https://max.ru/old_channel", Title: "Старое название", MAXOwnerID: "777",
		IconURL: "https://cdn.max.ru/old.png", Active: true, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := application.PrepareChannelClaim(ctx, "", "100"); err != nil {
		t.Fatal(err)
	}
	observed, err := storage.GetActiveObservedBotChat(ctx, "", "100")
	if err != nil {
		t.Fatal(err)
	}
	if observed.Title != "Новое название" || observed.PublicLink != "https://max.ru/new_channel" ||
		observed.IconURL != "https://cdn.max.ru/new.png" || observed.ParticipantsCount != 22 {
		t.Fatalf("existing observation was not refreshed: %#v", observed)
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
	if _, err := application.DeletePublication(context.Background(), "test-owner", post.ID); err == nil {
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
	metrics := newRecordingMetrics()
	application.metrics = metrics
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
	if metrics.due["publish"] != 1 || metrics.jobs["publish:success"] != 1 || metrics.publication["publish:success"] != 1 {
		t.Fatalf("unexpected scheduler metrics: due=%#v jobs=%#v publications=%#v", metrics.due, metrics.jobs, metrics.publication)
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
		// Simulate a legacy draft saved before MAX stopped accepting silent
		// channel publications. The application must omit the unsupported field.
		ImageURL: "http://localhost:8080/media/post.png", ImagePath: imagePath, Notify: false,
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
	if fake.lastPublishRequest.Notify != nil {
		t.Fatalf("publish Notify = %#v, want omitted for channel publication", fake.lastPublishRequest.Notify)
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
	if fake.lastEditRequest.Notify != nil {
		t.Fatalf("edit Notify = %#v, want omitted for channel publication", fake.lastEditRequest.Notify)
	}
}

func TestUpdatePublishedPostAddsImageWithoutReplacingPublication(t *testing.T) {
	t.Parallel()
	fake := &fakeMAX{
		chat:    maxclient.ChatInfo{ChatID: "72", Type: "channel", Status: "active", Title: "Channel"},
		message: maxclient.Message{MessageID: "mid.add-image", ChatID: "72"},
		membership: maxclient.Membership{
			IsAdmin: true,
			Permissions: []maxclient.Permission{
				maxclient.PermissionReadAllMessages, maxclient.PermissionEdit,
			},
		},
	}
	application, storage := newTestApp(t, fake)
	ctx := context.Background()
	channel, err := storage.CreateChannel(ctx, store.Channel{
		MAXChatID: "72", Title: "Channel", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	publishedAt := time.Date(2039, time.May, 7, 8, 9, 10, 0, time.UTC)
	post, err := storage.CreatePost(ctx, store.Post{
		Title: "Add image", Content: "Body", Format: store.FormatMarkdown, Status: store.PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: "mid.add-image", MAXMessageURL: "https://max.ru/channel/add-image",
		PublishedAt: &publishedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(t.TempDir(), "added.png")
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	imageURL := "http://localhost:8080/media/added.png"
	if post, err = storage.UpdatePost(ctx, post.ID, store.PostChanges{ImageURL: &imageURL, ImagePath: &imagePath}); err != nil {
		t.Fatal(err)
	}
	post, err = application.UpdatePublishedPost(ctx, post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fake.uploadCalls != 1 || fake.editCalls != 1 || len(fake.lastEditRequest.ImageTokens) != 1 ||
		fake.lastEditRequest.ImageTokens[0] != "image-token" {
		t.Fatalf("image update request = %#v, fake = %#v", fake.lastEditRequest, fake)
	}
	if post.Status != store.PostStatusPublished || post.MAXMessageID != "mid.add-image" ||
		post.MAXMessageURL != "https://max.ru/channel/add-image" || post.PublishedAt == nil ||
		!post.PublishedAt.Equal(publishedAt) {
		t.Fatalf("image update replaced publication history: %#v", post)
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

func linkMAXIdentityForAppTest(t *testing.T, storage *store.Store, userID, maxUserID string, now time.Time) {
	t.Helper()
	attempt := store.MAXIdentityLinkAttempt{
		ID: "link-" + userID, TokenHash: strings.Repeat("a", 64), UserID: userID,
		RequesterLabel: userID, ComparisonCode: "123456", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := storage.CreateMAXIdentityLinkAttempt(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	confirmHash := strings.Repeat("b", 64)
	if _, _, err := storage.StartMAXIdentityLinkConfirmation(context.Background(), attempt.TokenHash, maxUserID,
		confirmHash, strings.Repeat("c", 64), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ConfirmMAXIdentityLink(context.Background(), confirmHash, maxUserID, true, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
