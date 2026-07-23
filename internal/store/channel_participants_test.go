package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestChannelParticipantStatsAreTenantScopedDailyAndClaimed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "channel-participant-stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	for _, userID := range []string{"tenant-a", "tenant-b"} {
		if err := storage.UpsertUser(ctx, User{ID: userID, DisplayName: userID}); err != nil {
			t.Fatal(err)
		}
	}
	channel, err := storage.CreateChannel(ctx, Channel{
		UserID: "tenant-a", VerifiedMAXOwnerID: "max-owner-a", MAXChatID: "1001",
		Title: "Channel", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2042, time.March, 10, 9, 0, 0, 0, time.UTC)
	if _, err := storage.SyncChannelParticipantStatsForUser(ctx, "tenant-b", channel.ID, channel.MAXChatID,
		"https://cdn.max.ru/foreign.png", 999, first); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign participant sync error = %v, want ErrNotFound", err)
	}
	channel, err = storage.SyncChannelParticipantStatsForUser(ctx, "tenant-a", channel.ID, channel.MAXChatID,
		"https://cdn.max.ru/channel.png", 25, first)
	if err != nil {
		t.Fatal(err)
	}
	if channel.ParticipantsCount != 25 || channel.IconURL != "https://cdn.max.ru/channel.png" {
		t.Fatalf("first participant sync = %#v", channel)
	}
	laterSameDay := first.Add(9 * time.Hour)
	if _, err := storage.SyncChannelParticipantStatsForUser(ctx, "tenant-a", channel.ID, channel.MAXChatID,
		channel.IconURL, 30, laterSameDay); err != nil {
		t.Fatal(err)
	}
	history, err := storage.ListChannelParticipantSnapshotsForUser(ctx, "tenant-a", channel.ID,
		first.AddDate(0, 0, -1), first.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].ObservedOn != "2042-03-10" || history[0].ParticipantsCount != 30 ||
		!history[0].CapturedAt.Equal(laterSameDay) {
		t.Fatalf("same-day participant history = %#v", history)
	}
	if _, err := storage.ListChannelParticipantSnapshotsForUser(ctx, "tenant-b", channel.ID,
		first, first.AddDate(0, 0, 1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign participant history error = %v, want ErrNotFound", err)
	}
	nextDay := first.AddDate(0, 0, 1).Add(time.Hour)
	if _, err := storage.SyncChannelParticipantStatsForUser(ctx, "tenant-a", channel.ID, channel.MAXChatID,
		channel.IconURL, 31, nextDay); err != nil {
		t.Fatal(err)
	}
	history, err = storage.ListChannelParticipantSnapshotsForUser(ctx, "tenant-a", channel.ID, first, nextDay)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].ParticipantsCount != 30 || history[1].ParticipantsCount != 31 ||
		history[1].ObservedOn != "2042-03-11" {
		t.Fatalf("multi-day participant history = %#v", history)
	}
	due, err := storage.ListChannelsDueForParticipantStats(ctx, nextDay.Add(30*time.Minute), time.Hour, 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("early due channels = %#v, %v", due, err)
	}
	claimAt := nextDay.Add(time.Hour)
	due, err = storage.ListChannelsDueForParticipantStats(ctx, claimAt, time.Hour, 10)
	if err != nil || len(due) != 1 || due[0].ID != channel.ID {
		t.Fatalf("due channels = %#v, %v", due, err)
	}
	claimed, err := storage.ClaimChannelParticipantStatsAttemptForUser(ctx, "tenant-a", channel.ID,
		channel.MAXChatID, claimAt, time.Hour)
	if err != nil || !claimed {
		t.Fatalf("first participant claim = %v, %v", claimed, err)
	}
	claimed, err = storage.ClaimChannelParticipantStatsAttemptForUser(ctx, "tenant-a", channel.ID,
		channel.MAXChatID, claimAt, time.Hour)
	if err != nil || claimed {
		t.Fatalf("repeated participant claim = %v, %v", claimed, err)
	}
	if _, err := storage.SyncChannelParticipantStatsForUser(ctx, "tenant-a", channel.ID, "9999",
		channel.IconURL, 1000, claimAt); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale MAX chat participant sync error = %v, want ErrConflict", err)
	}

	var forbiddenColumns int
	if err := storage.db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns
WHERE table_schema=current_schema() AND table_name='channel_participant_snapshots'
  AND column_name IN ('owner_id','max_chat_id','title','public_link')`).Scan(&forbiddenColumns); err != nil {
		t.Fatal(err)
	}
	if forbiddenColumns != 0 {
		t.Fatalf("participant snapshots contain %d tenant/channel identity columns", forbiddenColumns)
	}
}

func TestObservedChatRefreshRequiresVerifiedOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "observed-owner-refresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	channel, err := storage.CreateChannel(ctx, Channel{
		VerifiedMAXOwnerID: "max-owner-a", MAXChatID: "2001", Title: "Channel",
		ParticipantsCount: 5, IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := storage.UpsertObservedBotChat(ctx, ObservedBotChat{
		MAXChatID: channel.MAXChatID, MAXOwnerID: "max-owner-b", ParticipantsCount: 99,
		Active: true, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := storage.GetChannel(ctx, channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ParticipantsCount != 5 {
		t.Fatalf("foreign MAX owner refreshed connected channel: %#v", stored)
	}
	if err := storage.UpsertObservedBotChat(ctx, ObservedBotChat{
		MAXChatID: channel.MAXChatID, MAXOwnerID: "max-owner-a", ParticipantsCount: 7,
		Active: true, LastSeenAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	stored, err = storage.GetChannel(ctx, channel.ID)
	if err != nil || stored.ParticipantsCount != 7 {
		t.Fatalf("verified MAX owner refresh = %#v, %v", stored, err)
	}
}

func TestSyncChannelMAXInfoStoresFullProfileAndRejectsStaleResponse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "channel-max-info.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	channel, err := storage.CreateChannel(ctx, Channel{
		MAXChatID: "3001", Title: "Old", Description: "Old description", IsChannel: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	eventAt := time.Date(2044, time.June, 1, 10, 0, 0, 0, time.UTC)
	syncedAt := eventAt.Add(time.Minute)
	channel, err = storage.SyncChannelMAXInfoForUser(ctx, channel.UserID, channel.ID, channel.MAXChatID, Channel{
		MAXChatID: channel.MAXChatID, Title: "Fresh", Description: "Fresh description",
		PublicLink: "https://max.ru/fresh", IconURL: "https://cdn.max.ru/fresh.png",
		ParticipantsCount: 77, IsPublic: true, MessagesCount: 1234, HasPinnedMessage: true,
		MAXLastEventTime: &eventAt,
	}, syncedAt)
	if err != nil {
		t.Fatal(err)
	}
	if channel.Title != "Fresh" || channel.Description != "Fresh description" || !channel.IsPublic ||
		channel.MessagesCount != 1234 || !channel.HasPinnedMessage || channel.ParticipantsCount != 77 ||
		channel.MAXLastEventTime == nil || !channel.MAXLastEventTime.Equal(eventAt) ||
		channel.MAXInfoSyncedAt == nil || !channel.MAXInfoSyncedAt.Equal(syncedAt) {
		t.Fatalf("full MAX profile sync = %#v", channel)
	}
	participantOnlyAt := syncedAt.Add(time.Second)
	channel, err = storage.SyncChannelParticipantStatsForUser(ctx, channel.UserID, channel.ID, channel.MAXChatID,
		"https://cdn.max.ru/fresh-v2.png", 78, participantOnlyAt)
	if err != nil {
		t.Fatal(err)
	}
	if channel.Description != "Fresh description" || !channel.IsPublic || channel.MessagesCount != 1234 ||
		!channel.HasPinnedMessage || channel.MAXLastEventTime == nil || !channel.MAXLastEventTime.Equal(eventAt) ||
		channel.MAXInfoSyncedAt == nil || !channel.MAXInfoSyncedAt.Equal(syncedAt) {
		t.Fatalf("participant-only sync lost MAX profile: %#v", channel)
	}

	staleAt := syncedAt.Add(-time.Second)
	staleEvent := eventAt.Add(-time.Hour)
	channel, err = storage.SyncChannelMAXInfoForUser(ctx, channel.UserID, channel.ID, channel.MAXChatID, Channel{
		MAXChatID: channel.MAXChatID, Title: "Stale", Description: "Stale description",
		ParticipantsCount: 1, MessagesCount: 1, MAXLastEventTime: &staleEvent,
	}, staleAt)
	if err != nil {
		t.Fatal(err)
	}
	if channel.Title != "Fresh" || channel.Description != "Fresh description" || channel.MessagesCount != 1234 ||
		channel.MAXInfoSyncedAt == nil || !channel.MAXInfoSyncedAt.Equal(syncedAt) {
		t.Fatalf("stale MAX profile replaced current data: %#v", channel)
	}
}
