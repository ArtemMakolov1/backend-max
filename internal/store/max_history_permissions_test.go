package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestMAXHistoryWritesRecheckWriterAfterParentBarrier(t *testing.T) {
	for _, operation := range []string{"claim", "apply", "release"} {
		t.Run(operation, func(t *testing.T) {
			s, workspace, channel := openMAXHistoryStoreTest(t, "video-history-role-barrier-"+operation, 1)
			now := time.Now().UTC().Truncate(time.Microsecond)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var claim MAXHistoryImport
			var err error
			if operation != "claim" {
				claim, err = s.ClaimMAXHistoryImport(ctx, "history-editor", workspace.ID, channel.ID, now, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
			}
			blocker, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = blocker.Rollback() }()
			var parent string
			if err = blocker.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace.ID).Scan(&parent); err != nil {
				t.Fatal(err)
			}
			var pid int
			if err = blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() {
				var writeErr error
				switch operation {
				case "claim":
					_, writeErr = s.ClaimMAXHistoryImport(ctx, "history-editor", workspace.ID, channel.ID, now, time.Minute)
				case "apply":
					_, writeErr = s.ApplyMAXHistoryPage(ctx, "history-editor", workspace.ID, claim.Generation, []MAXHistoryItem{{
						Title: "Scoped history", Content: "Original content", MessageID: "mid.barrier", PublishedAt: now.Add(-time.Hour),
						Raw: json.RawMessage(`{}`), RoundTrip: false,
						Attachments: []MAXHistoryAttachment{{Type: "image", ProviderToken: "private-token", RemoteURL: "https://media.max.ru/preview.jpg", ProviderMeta: json.RawMessage(`{}`)}},
					}}, nil, true, now.Add(time.Second))
				case "release":
					writeErr = s.ReleaseMAXHistoryImport(ctx, "history-editor", workspace.ID, claim.Generation, "upstream", now.Add(time.Second))
				}
				result <- writeErr
			}()
			waitMAXCommentsBlockedBy(t, s, pid, 1)
			if _, err = blocker.ExecContext(ctx, `UPDATE workspace_members SET role='viewer' WHERE workspace_id=$1 AND user_id=$2`, workspace.ID, "history-editor"); err != nil {
				t.Fatal(err)
			}
			if err = blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			if err = <-result; !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s retained stale editor authority after demotion: %v", operation, err)
			}
			posts, err := s.ListPostsForWorkspace(ctx, "test-owner", workspace.ID, "", nil)
			if err != nil || len(posts) != 0 {
				t.Fatalf("unauthorized history writes created %d posts, error = %v", len(posts), err)
			}
		})
	}
}
