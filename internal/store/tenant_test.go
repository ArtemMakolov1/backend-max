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

func TestChannelClaimTransfersActorsPersonalChannelIntoTeam(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "channel-claim-personal-transfer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	const actor = "transfer-actor"
	if err := storage.UpsertUser(ctx, User{ID: actor, DisplayName: "Transfer Actor"}); err != nil {
		t.Fatal(err)
	}
	personal := requirePersonalWorkspace(t, ctx, storage, actor)
	team, err := storage.CreateWorkspace(ctx, actor, Workspace{Name: "Transfer Team"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	channel, err := storage.CreateChannel(ctx, Channel{
		UserID: actor, VerifiedMAXOwnerID: "max-transfer-owner", MAXChatID: "transfer-chat",
		Title: "Personal channel", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.ExecContext(ctx, `INSERT INTO channel_participant_snapshots(
channel_id,observed_on,captured_at,participants_count) VALUES($1,$2,$3,$4)`,
		channel.ID, now.Format("2006-01-02"), now, 42); err != nil {
		t.Fatal(err)
	}

	// Keep a historical connected personal claim to exercise its composite
	// (owner_id, channel_id) foreign key during the transfer.
	personalClaim := verifyChannelClaimForTest(t, ctx, storage, ChannelClaim{
		ID: "personal-connected-claim", TokenHash: strings.Repeat("a", 64), UserID: actor,
		WorkspaceID: personal.ID, MAXChatID: channel.MAXChatID, RequestedTitle: channel.Title,
		RequesterLabel: actor, ComparisonCode: "123456", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}, "max-transfer-owner", "b", "c")
	if _, err := storage.CompleteChannelClaim(ctx, personalClaim, Channel{
		VerifiedMAXOwnerID: "max-transfer-owner", MAXChatID: channel.MAXChatID,
		Title: channel.Title, IsChannel: true, Active: true,
	}); err != nil {
		t.Fatal(err)
	}

	teamClaim := verifyChannelClaimForTest(t, ctx, storage, ChannelClaim{
		ID: "team-transfer-claim", TokenHash: strings.Repeat("d", 64), UserID: actor,
		WorkspaceID: team.ID, MAXChatID: channel.MAXChatID, RequestedTitle: "Team channel",
		RequesterLabel: actor, ComparisonCode: "654321", CreatedAt: now.Add(time.Minute),
		ExpiresAt: now.Add(11 * time.Minute),
	}, "max-transfer-owner", "e", "f")
	transferred, err := storage.CompleteChannelClaim(ctx, teamClaim, Channel{
		VerifiedMAXOwnerID: "max-transfer-owner", MAXChatID: channel.MAXChatID,
		Title: "Team channel", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if transferred.ID != channel.ID || transferred.WorkspaceID != team.ID ||
		transferred.UserID != team.CompatOwnerUserID {
		t.Fatalf("transferred channel = %#v, team = %#v", transferred, team)
	}
	if _, err := storage.GetChannelForUser(ctx, actor, channel.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("personal lookup after transfer error = %v, want ErrNotFound", err)
	}
	personalClaim, err = storage.GetChannelClaimForUser(ctx, actor, personalClaim.ID, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if personalClaim.Status != ChannelClaimConnected || personalClaim.ChannelID != nil ||
		personalClaim.WorkspaceID != personal.ID {
		t.Fatalf("historical personal claim = %#v", personalClaim)
	}
	teamClaim, err = storage.GetChannelClaimForUser(ctx, actor, teamClaim.ID, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if teamClaim.Status != ChannelClaimConnected || teamClaim.ChannelID == nil || *teamClaim.ChannelID != channel.ID {
		t.Fatalf("connected team claim = %#v", teamClaim)
	}
	var snapshotCount int
	if err := storage.db.QueryRowContext(ctx, `SELECT count(*) FROM channel_participant_snapshots
WHERE channel_id=$1`, channel.ID).Scan(&snapshotCount); err != nil || snapshotCount != 1 {
		t.Fatalf("participant snapshots after transfer = %d, err=%v", snapshotCount, err)
	}
	events, err := storage.ListAuditEvents(ctx, actor, team.ID, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	foundTransfer := false
	for _, event := range events {
		if event.Action == "channel.transferred" && event.EntityID == fmt.Sprint(channel.ID) &&
			strings.Contains(string(event.Metadata), personal.ID) {
			foundTransfer = true
			break
		}
	}
	if !foundTransfer {
		t.Fatalf("team audit does not contain channel.transferred from %q: %#v", personal.ID, events)
	}
}

func TestChannelClaimPersonalTransferSafetyChecks(t *testing.T) {
	t.Parallel()
	t.Run("linked posts", func(t *testing.T) {
		ctx := context.Background()
		storage, err := Open(ctx, filepath.Join(t.TempDir(), "channel-transfer-linked.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = storage.Close() })
		const actor = "linked-transfer-owner"
		if err := storage.UpsertUser(ctx, User{ID: actor, DisplayName: actor}); err != nil {
			t.Fatal(err)
		}
		personal := requirePersonalWorkspace(t, ctx, storage, actor)
		team, err := storage.CreateWorkspace(ctx, actor, Workspace{Name: "Linked Team"})
		if err != nil {
			t.Fatal(err)
		}
		channel, err := storage.CreateChannel(ctx, Channel{
			UserID: actor, VerifiedMAXOwnerID: "max-linked", MAXChatID: "linked-transfer-chat",
			Title: "Linked", IsChannel: true, Active: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.CreatePost(ctx, Post{
			UserID: actor, Title: "Linked post", Content: "Body", Format: FormatMarkdown, ChannelID: &channel.ID,
		}); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		claim := verifyChannelClaimForTest(t, ctx, storage, ChannelClaim{
			ID: "linked-transfer-claim", TokenHash: strings.Repeat("a", 64), UserID: actor,
			WorkspaceID: team.ID, MAXChatID: channel.MAXChatID, RequestedTitle: channel.Title,
			RequesterLabel: actor, ComparisonCode: "123456", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
		}, "max-linked", "b", "c")
		if _, err := storage.CompleteChannelClaim(ctx, claim, Channel{
			VerifiedMAXOwnerID: "max-linked", MAXChatID: channel.MAXChatID,
			Title: channel.Title, IsChannel: true, Active: true,
		}); !errors.Is(err, ErrConflict) {
			t.Fatalf("linked channel transfer error = %v, want ErrConflict", err)
		}
		stored, err := storage.GetChannel(ctx, channel.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.UserID != actor || stored.WorkspaceID != personal.ID {
			t.Fatalf("linked channel moved despite conflict: %#v", stored)
		}
	})

	t.Run("archived source", func(t *testing.T) {
		ctx := context.Background()
		storage, err := Open(ctx, filepath.Join(t.TempDir(), "channel-transfer-archived.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = storage.Close() })
		const actor = "archived-transfer-owner"
		if err := storage.UpsertUser(ctx, User{ID: actor, DisplayName: actor}); err != nil {
			t.Fatal(err)
		}
		personal := requirePersonalWorkspace(t, ctx, storage, actor)
		team, err := storage.CreateWorkspace(ctx, actor, Workspace{Name: "Archive Team"})
		if err != nil {
			t.Fatal(err)
		}
		channel, err := storage.CreateChannel(ctx, Channel{
			UserID: actor, VerifiedMAXOwnerID: "max-archived", MAXChatID: "archived-transfer-chat",
			Title: "Archived source", IsChannel: true, Active: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		claim := verifyChannelClaimForTest(t, ctx, storage, ChannelClaim{
			ID: "archived-transfer-claim", TokenHash: strings.Repeat("a", 64), UserID: actor,
			WorkspaceID: team.ID, MAXChatID: channel.MAXChatID, RequestedTitle: channel.Title,
			RequesterLabel: actor, ComparisonCode: "123456", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
		}, "max-archived", "b", "c")
		if _, err := storage.db.ExecContext(ctx, `UPDATE workspaces SET archived_at=$1 WHERE id=$2`,
			now.Add(time.Minute), personal.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := storage.CompleteChannelClaim(ctx, claim, Channel{
			VerifiedMAXOwnerID: "max-archived", MAXChatID: channel.MAXChatID,
			Title: channel.Title, IsChannel: true, Active: true,
		}); !errors.Is(err, ErrChannelOwned) {
			t.Fatalf("archived source transfer error = %v, want ErrChannelOwned", err)
		}
		stored, err := storage.GetChannel(ctx, channel.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.UserID != actor || stored.WorkspaceID != personal.ID {
			t.Fatalf("archived source channel moved: %#v", stored)
		}
	})

	t.Run("foreign personal owner", func(t *testing.T) {
		ctx := context.Background()
		storage, err := Open(ctx, filepath.Join(t.TempDir(), "channel-transfer-foreign.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = storage.Close() })
		for _, actor := range []string{"foreign-channel-owner", "foreign-team-owner"} {
			if err := storage.UpsertUser(ctx, User{ID: actor, DisplayName: actor}); err != nil {
				t.Fatal(err)
			}
		}
		source := requirePersonalWorkspace(t, ctx, storage, "foreign-channel-owner")
		team, err := storage.CreateWorkspace(ctx, "foreign-team-owner", Workspace{Name: "Foreign Team"})
		if err != nil {
			t.Fatal(err)
		}
		channel, err := storage.CreateChannel(ctx, Channel{
			UserID: "foreign-channel-owner", VerifiedMAXOwnerID: "max-foreign", MAXChatID: "foreign-transfer-chat",
			Title: "Foreign", IsChannel: true, Active: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		claim := verifyChannelClaimForTest(t, ctx, storage, ChannelClaim{
			ID: "foreign-transfer-claim", TokenHash: strings.Repeat("a", 64), UserID: "foreign-team-owner",
			WorkspaceID: team.ID, MAXChatID: channel.MAXChatID, RequestedTitle: channel.Title,
			RequesterLabel: "foreign-team-owner", ComparisonCode: "123456", CreatedAt: now,
			ExpiresAt: now.Add(10 * time.Minute),
		}, "max-foreign", "b", "c")
		if _, err := storage.CompleteChannelClaim(ctx, claim, Channel{
			VerifiedMAXOwnerID: "max-foreign", MAXChatID: channel.MAXChatID,
			Title: channel.Title, IsChannel: true, Active: true,
		}); !errors.Is(err, ErrChannelOwned) {
			t.Fatalf("foreign personal transfer error = %v, want ErrChannelOwned", err)
		}
		stored, err := storage.GetChannel(ctx, channel.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.UserID != "foreign-channel-owner" || stored.WorkspaceID != source.ID {
			t.Fatalf("foreign personal channel moved: %#v", stored)
		}
	})
}

func requirePersonalWorkspace(t *testing.T, ctx context.Context, storage *Store, userID string) Workspace {
	t.Helper()
	workspaces, err := storage.ListWorkspaces(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	for _, access := range workspaces {
		if access.Workspace.IsPersonal {
			return access.Workspace
		}
	}
	t.Fatalf("personal workspace for %q was not found", userID)
	return Workspace{}
}

func verifyChannelClaimForTest(
	t *testing.T,
	ctx context.Context,
	storage *Store,
	claim ChannelClaim,
	maxUserID, confirmHashDigit, cancelHashDigit string,
) ChannelClaim {
	t.Helper()
	if err := storage.CreateChannelClaim(ctx, claim); err != nil {
		t.Fatal(err)
	}
	confirmHash := strings.Repeat(confirmHashDigit, 64)
	cancelHash := strings.Repeat(cancelHashDigit, 64)
	if _, first, err := storage.StartChannelClaimConfirmation(
		ctx, claim.TokenHash, maxUserID, confirmHash, cancelHash, claim.CreatedAt.Add(time.Second)); err != nil || !first {
		t.Fatalf("start channel claim confirmation first=%v err=%v", first, err)
	}
	confirmed, err := storage.ConfirmChannelClaim(
		ctx, confirmHash, maxUserID, true, claim.CreatedAt.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return confirmed
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

func TestTouchObservedBotChatPreservesMetadataAndRemovalOrdering(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "observed-touch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	eventAt := time.Now().UTC().Truncate(time.Microsecond)
	chat := ObservedBotChat{
		MAXChatID: "5252", PublicLink: "https://max.ru/preserved", Title: "Preserved",
		MAXOwnerID: "88", IconURL: "https://cdn.max.ru/preserved.png", ParticipantsCount: 12,
		Active: true, LastSeenAt: eventAt,
	}
	if err := storage.UpsertObservedBotChat(ctx, chat); err != nil {
		t.Fatal(err)
	}
	if err := storage.TouchObservedBotChat(ctx, chat.MAXChatID, eventAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	preserved, err := storage.GetActiveObservedBotChat(ctx, chat.PublicLink, "")
	if err != nil || preserved.Title != chat.Title || preserved.MAXOwnerID != chat.MAXOwnerID ||
		preserved.IconURL != chat.IconURL || preserved.ParticipantsCount != chat.ParticipantsCount {
		t.Fatalf("touch wiped enriched metadata: %#v, %v", preserved, err)
	}
	if err := storage.RefreshObservedBotChatMetadata(ctx, ObservedBotChat{
		MAXChatID: chat.MAXChatID, Active: true, LastSeenAt: eventAt.Add(1500 * time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	preserved, err = storage.GetActiveObservedBotChat(ctx, chat.PublicLink, "")
	if err != nil || preserved.Title != chat.Title || preserved.MAXOwnerID != chat.MAXOwnerID ||
		preserved.IconURL != chat.IconURL || preserved.ParticipantsCount != chat.ParticipantsCount {
		t.Fatalf("partial metadata refresh wiped verified fields: %#v, %v", preserved, err)
	}
	removedAt := eventAt.Add(2 * time.Second)
	if err := storage.MarkObservedBotChatRemoved(ctx, chat.MAXChatID, removedAt); err != nil {
		t.Fatal(err)
	}
	if err := storage.TouchObservedBotChat(ctx, chat.MAXChatID, removedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetActiveObservedBotChat(ctx, "", chat.MAXChatID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("equal-time touch resurrected removed chat: %v", err)
	}
	chat.Title = "Stale refresh"
	chat.LastSeenAt = removedAt.Add(time.Second)
	if err := storage.RefreshObservedBotChatMetadata(ctx, chat); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetActiveObservedBotChat(ctx, "", chat.MAXChatID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("metadata refresh resurrected removed chat: %v", err)
	}
	if err := storage.TouchObservedBotChat(ctx, chat.MAXChatID, removedAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	// A metadata request started before the newer bot_added can finish after it.
	// Its owner proof must not be applied to the new lifecycle.
	chat.MAXOwnerID = "stale-owner"
	if err := storage.RefreshObservedBotChatMetadata(ctx, chat); err != nil {
		t.Fatal(err)
	}
	readded, err := storage.GetActiveObservedBotChat(ctx, chat.PublicLink, "")
	if err != nil || !readded.Active || readded.MAXOwnerID != "" || readded.Title != "Preserved" {
		t.Fatalf("stale refresh changed safely reactivated inventory: %#v, %v", readded, err)
	}
}
