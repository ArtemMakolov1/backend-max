package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTenantIsolationAndCompositeChannelOwnership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "tenant-isolation.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	for _, id := range []string{"tenant-a", "tenant-b"} {
		if err := storage.UpsertUser(ctx, User{ID: id, DisplayName: id}); err != nil {
			t.Fatal(err)
		}
	}
	channelA, err := storage.CreateChannel(ctx, Channel{
		UserID: "tenant-a", VerifiedMAXOwnerID: "max-a", MAXChatID: "100", Title: "A", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	channelB, err := storage.CreateChannel(ctx, Channel{
		UserID: "tenant-b", VerifiedMAXOwnerID: "max-b", MAXChatID: "200", Title: "B", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetChannelForUser(ctx, "tenant-b", channelA.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign channel lookup error = %v, want ErrNotFound", err)
	}
	postA, err := storage.CreatePost(ctx, Post{
		UserID: "tenant-a", Title: "A post", Content: "body", Format: FormatMarkdown, ChannelID: &channelA.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetPostForUser(ctx, "tenant-b", postA.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign post lookup error = %v, want ErrNotFound", err)
	}
	if _, err := storage.CreatePost(ctx, Post{
		UserID: "tenant-a", Title: "cross tenant", Content: "body", Format: FormatMarkdown, ChannelID: &channelB.ID,
	}); err == nil {
		t.Fatal("database accepted a cross-tenant post/channel relation")
	}
	postsA, err := storage.ListPostsForUser(ctx, "tenant-a", "", nil)
	if err != nil || len(postsA) != 1 || postsA[0].ID != postA.ID {
		t.Fatalf("tenant A posts = %#v, %v", postsA, err)
	}
	postsB, err := storage.ListPostsForUser(ctx, "tenant-b", "", nil)
	if err != nil || len(postsB) != 0 {
		t.Fatalf("tenant B posts = %#v, %v", postsB, err)
	}
	if err := storage.RegisterMedia(ctx, "tenant-a", "asset.png", time.Now()); err != nil {
		t.Fatal(err)
	}
	if owned, err := storage.UserOwnsMedia(ctx, "tenant-a", "asset.png"); err != nil || !owned {
		t.Fatalf("owner media access = %v, %v", owned, err)
	}
	if owned, err := storage.UserOwnsMedia(ctx, "tenant-b", "asset.png"); err != nil || owned {
		t.Fatalf("foreign media access = %v, %v", owned, err)
	}
}

func TestChannelVisualMetadataRefreshIsTenantScoped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "channel-visual-tenant.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	for _, id := range []string{"tenant-a", "tenant-b"} {
		if err := storage.UpsertUser(ctx, User{ID: id, DisplayName: id}); err != nil {
			t.Fatal(err)
		}
	}
	channel, err := storage.CreateChannel(ctx, Channel{
		UserID: "tenant-a", VerifiedMAXOwnerID: "max-a", MAXChatID: "visual-tenant",
		Title: "A", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := storage.RefreshChannelVisualMetadataForUser(ctx, "tenant-b", channel.ID,
		"https://cdn.max.ru/foreign.png", 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign visual metadata refresh error = %v, want ErrNotFound", err)
	}
	stored, err := storage.GetChannelForUser(ctx, "tenant-a", channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.IconURL != "" || stored.ParticipantsCount != 0 {
		t.Fatalf("foreign tenant changed channel metadata: %#v", stored)
	}

	stored, err = storage.RefreshChannelVisualMetadataForUser(ctx, "tenant-a", channel.ID,
		"https://cdn.max.ru/owner.png", 25)
	if err != nil {
		t.Fatal(err)
	}
	if stored.IconURL != "https://cdn.max.ru/owner.png" || stored.ParticipantsCount != 25 {
		t.Fatalf("owner visual metadata was not refreshed: %#v", stored)
	}
}

func TestChannelClaimIsOneTimeOwnerBoundAndConflictsAcrossTenants(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "channel-claim.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	for _, id := range []string{"tenant-a", "tenant-b"} {
		if err := storage.UpsertUser(ctx, User{ID: id, DisplayName: id}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	claim := ChannelClaim{
		ID: "claim-a", TokenHash: strings.Repeat("a", 64), UserID: "tenant-a", MAXChatID: "777",
		PublicLink: "https://max.ru/channel", RequestedTitle: "Channel", RequesterLabel: "tenant-a",
		ComparisonCode: "123456", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := storage.CreateChannelClaim(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetChannelClaimForUser(ctx, "tenant-b", claim.ID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign claim lookup error = %v", err)
	}
	started, first, err := storage.StartChannelClaimConfirmation(ctx, claim.TokenHash, "max-owner",
		strings.Repeat("b", 64), strings.Repeat("c", 64), now.Add(time.Second))
	if err != nil || !first || started.Status != ChannelClaimAwaitingConfirmation {
		t.Fatalf("start confirmation = %#v, first=%v, err=%v", started, first, err)
	}
	_, first, err = storage.StartChannelClaimConfirmation(ctx, claim.TokenHash, "max-owner",
		strings.Repeat("d", 64), strings.Repeat("e", 64), now.Add(2*time.Second))
	if err != nil || first {
		t.Fatalf("replayed bot_started first=%v err=%v", first, err)
	}
	if _, err := storage.ConfirmChannelClaim(ctx, strings.Repeat("b", 64), "other-max-user", true, now.Add(3*time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign MAX user confirmation error = %v", err)
	}
	confirmed, err := storage.ConfirmChannelClaim(ctx, strings.Repeat("b", 64), "max-owner", true, now.Add(3*time.Second))
	if err != nil || confirmed.Status != ChannelClaimIdentityVerified {
		t.Fatalf("confirmed claim = %#v, %v", confirmed, err)
	}
	channel, err := storage.CompleteChannelClaim(ctx, confirmed, Channel{
		UserID: "tenant-a", VerifiedMAXOwnerID: "max-owner", MAXChatID: "777", Title: "Channel", IsChannel: true, Active: true,
	})
	if err != nil || channel.UserID != "tenant-a" || channel.VerifiedMAXOwnerID != "max-owner" {
		t.Fatalf("connected channel = %#v, %v", channel, err)
	}
	claimB := ChannelClaim{
		ID: "claim-b", TokenHash: strings.Repeat("1", 64), UserID: "tenant-b", MAXChatID: "777",
		RequestedTitle: "Channel", RequesterLabel: "tenant-b", ComparisonCode: "654321",
		CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := storage.CreateChannelClaim(ctx, claimB); err != nil {
		t.Fatal(err)
	}
	_, _, err = storage.StartChannelClaimConfirmation(ctx, claimB.TokenHash, "max-owner",
		strings.Repeat("2", 64), strings.Repeat("3", 64), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	confirmedB, err := storage.ConfirmChannelClaim(ctx, strings.Repeat("2", 64), "max-owner", true, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CompleteChannelClaim(ctx, confirmedB, Channel{
		UserID: "tenant-b", VerifiedMAXOwnerID: "max-owner", MAXChatID: "777", Title: "Channel", IsChannel: true, Active: true,
	}); !errors.Is(err, ErrChannelOwned) {
		t.Fatalf("second tenant completion error = %v, want ErrChannelOwned", err)
	}
}

func TestChannelClaimActiveLimitIsAtomic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "channel-claim-limit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	if err := storage.UpsertUser(ctx, User{ID: "tenant", DisplayName: "tenant"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			hash := fmt.Sprintf("%064x", i+1)
			err := storage.CreateChannelClaim(ctx, ChannelClaim{
				ID: fmt.Sprintf("claim-%d", i), TokenHash: hash, UserID: "tenant", MAXChatID: fmt.Sprintf("%d", 1000+i),
				RequestedTitle: "Channel", RequesterLabel: "tenant", ComparisonCode: fmt.Sprintf("%06d", i),
				CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
			})
			if err == nil {
				successes.Add(1)
				return
			}
			if !errors.Is(err, ErrConflict) {
				t.Errorf("claim %d error = %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if successes.Load() != maxActiveClaimsPerUser {
		t.Fatalf("successful active claims = %d, want %d", successes.Load(), maxActiveClaimsPerUser)
	}
}

func TestObservedBotChatRemovalWinsEqualAndNewerEventsOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "observed-ordering.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	eventAt := time.Now().UTC().Truncate(time.Microsecond)
	chat := ObservedBotChat{
		MAXChatID: "4242", PublicLink: "https://max.ru/order", Title: "Order",
		MAXOwnerID: "77", Active: true, LastSeenAt: eventAt,
	}
	if err := storage.UpsertObservedBotChat(ctx, chat); err != nil {
		t.Fatal(err)
	}
	if err := storage.MarkObservedBotChatRemoved(ctx, chat.MAXChatID, eventAt); err != nil {
		t.Fatal(err)
	}
	if err := storage.UpsertObservedBotChat(ctx, chat); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetActiveObservedBotChat(ctx, chat.PublicLink, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("equal-time add resurrected removed chat: %v", err)
	}
	chat.LastSeenAt = eventAt.Add(-time.Second)
	if err := storage.UpsertObservedBotChat(ctx, chat); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetActiveObservedBotChat(ctx, chat.PublicLink, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale add resurrected removed chat: %v", err)
	}
	chat.LastSeenAt = eventAt.Add(time.Second)
	if err := storage.UpsertObservedBotChat(ctx, chat); err != nil {
		t.Fatal(err)
	}
	active, err := storage.GetActiveObservedBotChat(ctx, chat.PublicLink, "")
	if err != nil || !active.Active || !active.LastSeenAt.Equal(chat.LastSeenAt) {
		t.Fatalf("newer add was not applied: %#v, %v", active, err)
	}
}
