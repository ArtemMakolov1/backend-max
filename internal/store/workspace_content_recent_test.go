package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestListRecentPublishedPostContentsIsBoundedOrderedAndTenantScoped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage := openWorkspaceTestStore(t, "recent-published-content")
	workspace, err := storage.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Direct context"})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := storage.CreateChannelForWorkspace(ctx, "test-owner", workspace.ID, Channel{
		MAXChatID: "direct-context-channel", VerifiedMAXOwnerID: "max-owner",
		Title: "Direct context", Active: true, IsChannel: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2045, time.January, 2, 12, 0, 0, 0, time.UTC)
	for index := 0; index < 12; index++ {
		publishedAt := base.Add(time.Duration(index) * time.Minute)
		if _, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{
			Title: "Published", Content: fmt.Sprintf("post-%02d", index),
			Status: PostStatusPublished, ChannelID: &channel.ID,
			MAXMessageID: fmt.Sprintf("mid-%02d", index), PublishedAt: &publishedAt,
		}); err != nil {
			t.Fatal(err)
		}
	}
	newest := base.Add(time.Hour)
	if _, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{
		Title: "Whitespace", Content: " \n\t ", Status: PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: "mid-empty", PublishedAt: &newest,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreatePostForWorkspace(ctx, "test-owner", workspace.ID, Post{
		Title: "Draft", Content: "private draft", Status: PostStatusDraft,
		ChannelID: &channel.ID,
	}); err != nil {
		t.Fatal(err)
	}

	foreignWorkspace, err := storage.CreateWorkspace(
		ctx, "test-owner", Workspace{Name: "Foreign Direct context"},
	)
	if err != nil {
		t.Fatal(err)
	}
	foreignChannel, err := storage.CreateChannelForWorkspace(
		ctx, "test-owner", foreignWorkspace.ID, Channel{
			MAXChatID: "foreign-direct-context", VerifiedMAXOwnerID: "max-owner",
			Title: "Foreign", Active: true, IsChannel: true,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreatePostForWorkspace(ctx, "test-owner", foreignWorkspace.ID, Post{
		Title: "Foreign", Content: "foreign private content", Status: PostStatusPublished,
		ChannelID: &foreignChannel.ID, MAXMessageID: "mid-foreign", PublishedAt: &newest,
	}); err != nil {
		t.Fatal(err)
	}

	contents, err := storage.ListRecentPublishedPostContentsForWorkspace(
		ctx, "test-owner", workspace.ID, channel.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) != 10 {
		t.Fatalf("recent content count = %d, want 10: %#v", len(contents), contents)
	}
	for index, content := range contents {
		expected := fmt.Sprintf("post-%02d", 11-index)
		if content != expected {
			t.Fatalf("recent content[%d] = %q, want %q", index, content, expected)
		}
	}
	foreignContents, err := storage.ListRecentPublishedPostContentsForWorkspace(
		ctx, "test-owner", workspace.ID, foreignChannel.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(foreignContents) != 0 {
		t.Fatalf("foreign channel leaked into workspace: %#v", foreignContents)
	}
	if err := storage.UpsertUser(ctx, User{
		ID: "direct-context-outsider", DisplayName: "Direct context outsider",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ListRecentPublishedPostContentsForWorkspace(
		ctx, "direct-context-outsider", workspace.ID, channel.ID,
	); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider query error = %v, want ErrNotFound", err)
	}
}
