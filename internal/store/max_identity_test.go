package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMAXIdentityLinkIsExplicitOneTimeAndOneToOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "max-identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	for _, userID := range []string{"tenant-a", "tenant-b"} {
		if err := storage.UpsertUser(ctx, User{ID: userID, DisplayName: userID}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	attempt := MAXIdentityLinkAttempt{
		ID: "identity-a", TokenHash: strings.Repeat("a", 64), UserID: "tenant-a",
		RequesterLabel: "tenant-a", ComparisonCode: "123456", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := storage.CreateMAXIdentityLinkAttempt(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	started, first, err := storage.StartMAXIdentityLinkConfirmation(ctx, attempt.TokenHash, "777",
		strings.Repeat("b", 64), strings.Repeat("c", 64), now.Add(time.Second))
	if err != nil || !first || started.Status != MAXIdentityAttemptAwaitingConfirmation {
		t.Fatalf("start identity confirmation = %#v first=%v err=%v", started, first, err)
	}
	_, first, err = storage.StartMAXIdentityLinkConfirmation(ctx, attempt.TokenHash, "777",
		strings.Repeat("d", 64), strings.Repeat("e", 64), now.Add(2*time.Second))
	if err != nil || first {
		t.Fatalf("deep-link replay first=%v err=%v", first, err)
	}
	if _, err := storage.ConfirmMAXIdentityLink(ctx, strings.Repeat("b", 64), "999", true, now.Add(3*time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong MAX user callback error=%v", err)
	}
	linkedAttempt, err := storage.ConfirmMAXIdentityLink(ctx, strings.Repeat("b", 64), "777", true, now.Add(3*time.Second))
	if err != nil || linkedAttempt.Status != MAXIdentityAttemptLinked {
		t.Fatalf("confirmed attempt=%#v err=%v", linkedAttempt, err)
	}
	link, err := storage.GetMAXIdentityLinkForUser(ctx, "tenant-a")
	if err != nil || link.MAXUserID != "777" {
		t.Fatalf("durable identity link=%#v err=%v", link, err)
	}
	if _, err := storage.ConfirmMAXIdentityLink(ctx, strings.Repeat("b", 64), "777", true, now.Add(4*time.Second)); err != nil {
		t.Fatalf("callback replay failed: %v", err)
	}
	if err := storage.CreateMAXIdentityLinkAttempt(ctx, MAXIdentityLinkAttempt{
		ID: "relink", TokenHash: strings.Repeat("f", 64), UserID: "tenant-a", RequesterLabel: "tenant-a",
		ComparisonCode: "654321", CreatedAt: now.Add(5 * time.Second), ExpiresAt: now.Add(15 * time.Minute),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("linked owner started relink: %v", err)
	}

	attemptB := MAXIdentityLinkAttempt{
		ID: "identity-b", TokenHash: strings.Repeat("1", 64), UserID: "tenant-b",
		RequesterLabel: "tenant-b", ComparisonCode: "271828", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := storage.CreateMAXIdentityLinkAttempt(ctx, attemptB); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.StartMAXIdentityLinkConfirmation(ctx, attemptB.TokenHash, "777",
		strings.Repeat("2", 64), strings.Repeat("3", 64), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	rejected, err := storage.ConfirmMAXIdentityLink(ctx, strings.Repeat("2", 64), "777", true, now.Add(2*time.Second))
	if err != nil || rejected.Status != MAXIdentityAttemptFailed || rejected.ErrorCode != "max_identity_already_linked" {
		t.Fatalf("duplicate MAX identity result=%#v err=%v", rejected, err)
	}
	if _, err := storage.GetMAXIdentityLinkForUser(ctx, "tenant-b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("duplicate identity was persisted: %v", err)
	}
}

func TestDiscoverableChannelsAreIdentityAndTenantScoped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "discoverable-channels.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	for _, userID := range []string{"tenant-a", "tenant-b"} {
		if err := storage.UpsertUser(ctx, User{ID: userID, DisplayName: userID}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	linkIdentityForTest(t, storage, "tenant-a", "777", now)
	for _, chat := range []ObservedBotChat{
		{MAXChatID: "100", MAXOwnerID: "777", Title: "Owned", IconURL: "https://cdn.max.ru/a.png", ParticipantsCount: 12, Active: true, LastSeenAt: now},
		{MAXChatID: "200", MAXOwnerID: "999", Title: "Foreign MAX owner", Active: true, LastSeenAt: now},
		{MAXChatID: "300", MAXOwnerID: "777", Title: "Removed", Active: false, LastSeenAt: now},
		{MAXChatID: "400", MAXOwnerID: "777", Title: "Connected elsewhere", Active: true, LastSeenAt: now},
	} {
		if err := storage.UpsertObservedBotChat(ctx, chat); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := storage.CreateChannel(ctx, Channel{
		UserID: "tenant-b", VerifiedMAXOwnerID: "777", MAXChatID: "400", Title: "Legacy foreign", IsChannel: true, Active: true,
	}); err != nil {
		t.Fatal(err)
	}
	channels, err := storage.ListDiscoverableChannelsForUser(ctx, "tenant-a")
	if err != nil || len(channels) != 1 || channels[0].MAXChatID != "100" || channels[0].IconURL == "" || channels[0].ParticipantsCount != 12 {
		t.Fatalf("tenant A discoverable channels=%#v err=%v", channels, err)
	}
	foreign, err := storage.ListDiscoverableChannelsForUser(ctx, "tenant-b")
	if err != nil || len(foreign) != 0 {
		t.Fatalf("unlinked tenant discoverable channels=%#v err=%v", foreign, err)
	}
	connected, err := storage.ConnectDiscoverableChannelForUser(ctx, "tenant-a", "100", Channel{
		UserID: "tenant-a", VerifiedMAXOwnerID: "777", MAXChatID: "100", Title: "Owned", IsChannel: true, Active: true,
	})
	if err != nil || connected.UserID != "tenant-a" || connected.MAXChatID != "100" {
		t.Fatalf("connect discoverable channel=%#v err=%v", connected, err)
	}
	if _, err := storage.ConnectDiscoverableChannelForUser(ctx, "tenant-a", "200", Channel{
		UserID: "tenant-a", VerifiedMAXOwnerID: "999", MAXChatID: "200", Title: "Foreign", IsChannel: true, Active: true,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign owner connect error=%v", err)
	}
	if err := storage.MarkObservedBotChatRemoved(ctx, "100", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := storage.UpsertObservedBotChat(ctx, ObservedBotChat{
		MAXChatID: "100", MAXOwnerID: "777", Title: "Owned", Active: true, LastSeenAt: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	readded, err := storage.ListDiscoverableChannelsForUser(ctx, "tenant-a")
	if err != nil || len(readded) != 1 || readded[0].MAXChatID != "100" || readded[0].Connected {
		t.Fatalf("re-added inactive channel should be reconnectable: %#v err=%v", readded, err)
	}
}

func TestDiscoverableRefreshCandidatesAreBoundedAndTenantSafe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "discoverable-refresh-candidates.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	for _, userID := range []string{"tenant-a", "tenant-b"} {
		if err := storage.UpsertUser(ctx, User{ID: userID, DisplayName: userID}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	linkIdentityForTest(t, storage, "tenant-a", "777", now.Add(-time.Hour))
	for _, chat := range []ObservedBotChat{
		{MAXChatID: "100", MAXOwnerID: "777", Title: "Verified", Active: true, LastSeenAt: now.Add(-time.Minute)},
		{MAXChatID: "101", Title: "Recent incomplete", Active: true, LastSeenAt: now.Add(-time.Minute)},
		{MAXChatID: "102", Title: "Old incomplete", Active: true, LastSeenAt: now.Add(-8 * 24 * time.Hour)},
		{MAXChatID: "103", MAXOwnerID: "999", Title: "Foreign owner", Active: true, LastSeenAt: now},
		{MAXChatID: "104", Title: "Connected elsewhere", Active: true, LastSeenAt: now},
		{MAXChatID: "105", Title: "Another recent incomplete", Active: true, LastSeenAt: now},
		{MAXChatID: "106", Title: "Connected here with incomplete owner", Active: true, LastSeenAt: now.Add(-2 * time.Minute)},
	} {
		if err := storage.UpsertObservedBotChat(ctx, chat); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := storage.CreateChannel(ctx, Channel{
		UserID: "tenant-b", VerifiedMAXOwnerID: "888", MAXChatID: "104", Title: "Foreign tenant",
		IsChannel: true, Active: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateChannel(ctx, Channel{
		UserID: "tenant-a", VerifiedMAXOwnerID: "777", MAXChatID: "106", Title: "Current tenant",
		IsChannel: true, Active: true,
	}); err != nil {
		t.Fatal(err)
	}

	candidates, err := storage.ListDiscoverableChannelRefreshCandidatesForUser(ctx, "tenant-a", now.Add(-7*24*time.Hour), 20, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates.Owned) != 2 || candidates.Owned[0].MAXChatID != "100" || candidates.Owned[1].MAXChatID != "106" {
		t.Fatalf("owned refresh candidates = %#v", candidates.Owned)
	}
	if len(candidates.Unknown) != 2 || candidates.Unknown[0].MAXChatID != "105" || candidates.Unknown[1].MAXChatID != "101" {
		t.Fatalf("unknown refresh candidates = %#v", candidates.Unknown)
	}
	bounded, err := storage.ListDiscoverableChannelRefreshCandidatesForUser(ctx, "tenant-a", now.Add(-7*24*time.Hour), 1, 1)
	if err != nil || len(bounded.Owned) != 1 || bounded.Owned[0].MAXChatID != "100" ||
		len(bounded.Unknown) != 1 || bounded.Unknown[0].MAXChatID != "105" {
		t.Fatalf("bounded refresh candidates = %#v err=%v", bounded, err)
	}
	if unlinked, err := storage.ListDiscoverableChannelRefreshCandidatesForUser(ctx, "tenant-b", now.Add(-7*24*time.Hour), 20, 4); err != nil ||
		len(unlinked.Owned) != 0 || len(unlinked.Unknown) != 0 {
		t.Fatalf("unlinked tenant candidates = %#v err=%v", unlinked, err)
	}
}

func TestDiscoverableConnectedChannelUsesTenantMetadataDespiteStaleObservedOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "discoverable-connected-metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	for _, userID := range []string{"tenant-a", "tenant-b"} {
		if err := storage.UpsertUser(ctx, User{ID: userID, DisplayName: userID}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	linkIdentityForTest(t, storage, "tenant-a", "777", now.Add(-time.Hour))
	owned, err := storage.CreateChannel(ctx, Channel{
		UserID: "tenant-a", VerifiedMAXOwnerID: "777", MAXChatID: "100", Title: "Актуальное название",
		PublicLink: "https://max.ru/current", IconURL: "https://cdn.max.ru/current.png", ParticipantsCount: 55,
		IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateChannel(ctx, Channel{
		UserID: "tenant-b", VerifiedMAXOwnerID: "888", MAXChatID: "300", Title: "Другой кабинет",
		PublicLink: "https://max.ru/foreign", IconURL: "https://cdn.max.ru/foreign.png",
		IsChannel: true, Active: true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, observed := range []ObservedBotChat{
		{MAXChatID: "100", Title: "Устаревшее название", PublicLink: "https://max.ru/stale", IconURL: "https://cdn.max.ru/stale.png", ParticipantsCount: 1, Active: true, LastSeenAt: now},
		{MAXChatID: "200", MAXOwnerID: "999", Title: "Чужой неподключённый", Active: true, LastSeenAt: now},
		{MAXChatID: "300", MAXOwnerID: "777", Title: "Наблюдаемый, но чужой tenant", Active: true, LastSeenAt: now},
	} {
		if err := storage.UpsertObservedBotChat(ctx, observed); err != nil {
			t.Fatal(err)
		}
	}

	channels, err := storage.ListDiscoverableChannelsForUser(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 1 {
		t.Fatalf("discoverable channels = %#v, want only current tenant's connected channel", channels)
	}
	channel := channels[0]
	if channel.MAXChatID != "100" || !channel.Connected || channel.ConnectedChannelID == nil || *channel.ConnectedChannelID != owned.ID ||
		channel.Title != "Актуальное название" || channel.PublicLink != "https://max.ru/current" ||
		channel.IconURL != "https://cdn.max.ru/current.png" || channel.ParticipantsCount != 55 {
		t.Fatalf("connected discoverable channel = %#v", channel)
	}
	if err := storage.UpsertObservedBotChat(ctx, ObservedBotChat{
		MAXChatID: "100", MAXOwnerID: "999", Title: "Transferred", Active: true, LastSeenAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	channels, err = storage.ListDiscoverableChannelsForUser(ctx, "tenant-a")
	if err != nil || len(channels) != 0 {
		t.Fatalf("channel with an authoritative foreign owner remained discoverable: %#v err=%v", channels, err)
	}
}

func linkIdentityForTest(t *testing.T, storage *Store, userID, maxUserID string, now time.Time) {
	t.Helper()
	attempt := MAXIdentityLinkAttempt{
		ID: "link-" + userID, TokenHash: strings.Repeat(map[string]string{"tenant-a": "a", "tenant-b": "b"}[userID], 64),
		UserID: userID, RequesterLabel: userID, ComparisonCode: "123456", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := storage.CreateMAXIdentityLinkAttempt(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	confirmHash := strings.Repeat("c", 64)
	if _, _, err := storage.StartMAXIdentityLinkConfirmation(context.Background(), attempt.TokenHash, maxUserID,
		confirmHash, strings.Repeat("d", 64), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ConfirmMAXIdentityLink(context.Background(), confirmHash, maxUserID, true, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
}
