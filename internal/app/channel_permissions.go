package app

import (
	"context"
	"time"
)

// RefreshMAXChatPermissions handles the October 2026 webhook. Read the current
// membership rather than replaying event permissions: delayed deliveries must
// not replace current MAX access with an obsolete event body.
func (a *App) RefreshMAXChatPermissions(ctx context.Context, maxChatID string, eventAt time.Time) error {
	if a.max == nil {
		return ErrMAXNotConfigured
	}
	info, err := a.max.GetChat(ctx, maxChatID)
	if err != nil {
		return err
	}
	if info.Type != "channel" {
		return nil
	}
	membership, err := a.max.GetMembership(ctx, maxChatID)
	if err != nil {
		return err
	}
	info, err = a.resolveMAXChatOwner(ctx, info, "", true)
	if err != nil {
		return err
	}
	_, observed, err := normalizeObservedMAXChat(maxChatID, info, "", eventAt)
	if err != nil {
		return err
	}
	// The inventory upsert compares lifecycle timestamps, so an older rights
	// event cannot resurrect a bot that has since been removed from the channel.
	if err := a.store.UpsertObservedBotChat(ctx, observed); err != nil {
		return err
	}
	return a.store.SyncMAXChannelPermissions(ctx, info.ChatID, info.OwnerID,
		channelDiagnostics(info, membership).CanPublish, eventAt, a.now().UTC())
}
