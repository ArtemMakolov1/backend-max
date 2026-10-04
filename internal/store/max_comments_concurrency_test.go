package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func maxCommentsConcurrencyFixture(t *testing.T, name string) (*Store, Workspace, Post, time.Time) {
	t.Helper()
	s, workspace, channel := openMAXHistoryStoreTest(t, name, 1)
	now := time.Now().UTC().Truncate(time.Microsecond)
	published := now.Add(-time.Hour)
	post, err := s.CreatePostForWorkspace(context.Background(), "test-owner", workspace.ID, Post{
		Title: "MAX comment concurrency", Content: "A published post", Status: PostStatusPublished,
		ChannelID: &channel.ID, MAXMessageID: "root-concurrency", PublishedAt: &published,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, workspace, post, now
}

func maxCommentsTestSendRequest(key string) MAXCommentWriteRequest {
	return MAXCommentWriteRequest{ClientRequestID: key, RequestHash: strings.Repeat("a", 64),
		Kind: "send", Text: "A reply", BotUserID: "42"}
}

// Wait for PostgreSQL's actual lock dependency, rather than assuming the
// worker reached a lock after an arbitrary sleep. Each fixture has its own
// schema and blocker connection, so unrelated tests cannot satisfy this gate.
func waitMAXCommentsBlockedBy(t *testing.T, s *Store, blockerPID, workers int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var blocked int
		// PostgreSQL can queue the second waiter behind the first one, making
		// it indirectly dependent on the held lock. Include that real chain.
		if err := s.db.QueryRowContext(ctx, `WITH RECURSIVE blocked(pid) AS (
SELECT pid FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))
UNION
SELECT activity.pid FROM pg_stat_activity activity JOIN blocked
ON blocked.pid=ANY(pg_blocking_pids(activity.pid))
) SELECT COUNT(*) FROM blocked`, blockerPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked >= workers {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("only %d workers reached the PostgreSQL barrier, wanted %d", blocked, workers)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestMAXCommentsParentBarrierRechecksDemotedAndRemovedWriter(t *testing.T) {
	for _, mutation := range []string{"demote", "remove"} {
		t.Run(mutation, func(t *testing.T) {
			t.Parallel()
			s, workspace, post, now := maxCommentsConcurrencyFixture(t, "max-comment-authority-"+mutation)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			operation, _, err := s.ClaimMAXCommentOperation(ctx, "history-editor", workspace.ID, post.ID, post.MAXMessageID,
				maxCommentsTestSendRequest("parent-barrier-request"), now)
			if err != nil {
				t.Fatal(err)
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
				result <- s.BeginMAXCommentWrite(ctx, "history-editor", workspace.ID, post.ID, post.MAXMessageID, operation.OperationID, now)
			}()
			waitMAXCommentsBlockedBy(t, s, pid, 1)
			if mutation == "demote" {
				_, err = blocker.ExecContext(ctx, `UPDATE workspace_members SET role='viewer',updated_at=$3
WHERE workspace_id=$1 AND user_id=$2`, workspace.ID, "history-editor", now)
			} else {
				_, err = blocker.ExecContext(ctx, `DELETE FROM workspace_members WHERE workspace_id=$1 AND user_id=$2`, workspace.ID, "history-editor")
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			if err = <-result; !errors.Is(err, ErrNotFound) {
				t.Fatalf("writer kept pre-revocation authority: %v", err)
			}
			var started bool
			if err = s.db.QueryRowContext(ctx, `SELECT write_started FROM max_post_comment_operations WHERE operation_id=$1`, operation.OperationID).Scan(&started); err != nil || started {
				t.Fatalf("unauthorized provider write intent persisted: started=%v error=%v", started, err)
			}
		})
	}
}

func TestMAXCommentsFinishAndDuplicateClaimUseConsistentPostLockOrder(t *testing.T) {
	t.Parallel()
	s, workspace, post, now := maxCommentsConcurrencyFixture(t, "max-comment-finish-lock-order")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request := maxCommentsTestSendRequest("same-permanent-request")
	operation, _, err := s.ClaimMAXCommentOperation(ctx, "test-owner", workspace.ID, post.ID, post.MAXMessageID, request, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BeginMAXCommentWrite(ctx, "test-owner", workspace.ID, post.ID, post.MAXMessageID, operation.OperationID, now); err != nil {
		t.Fatal(err)
	}
	blocker, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback() }()
	var postID int64
	if err = blocker.QueryRowContext(ctx, `SELECT id FROM posts WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace.ID, post.ID).Scan(&postID); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err = blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	item := MAXPostComment{MessageID: "accepted-response", Text: request.Text, SenderUserID: "42", SenderIsBot: true,
		TextEditable: true, CreatedAt: now, UpdatedAt: now, ObservedAt: now}
	finished := make(chan error, 1)
	go func() { finished <- s.FinishMAXCommentOperation(ctx, operation.OperationID, "succeeded", &item, now) }()
	waitMAXCommentsBlockedBy(t, s, pid, 1)
	// The finalizer must still be waiting BEFORE it locks the operation. With
	// the former inverse order, NOWAIT fails here and the real FK/claim cycle
	// can deadlock, losing an already accepted provider MID.
	probe, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = probe.Rollback() }()
	var operationID string
	if err = probe.QueryRowContext(ctx, `SELECT operation_id FROM max_post_comment_operations
WHERE operation_id=$1 FOR UPDATE NOWAIT`, operation.OperationID).Scan(&operationID); err != nil {
		t.Fatalf("finalizer locked operation before the blocked post: %v", err)
	}
	if err = probe.Rollback(); err != nil {
		t.Fatal(err)
	}
	type duplicateResult struct {
		operation MAXCommentOperation
		isNew     bool
		err       error
	}
	duplicate := make(chan duplicateResult, 1)
	go func() {
		op, isNew, claimErr := s.ClaimMAXCommentOperation(ctx, "test-owner", workspace.ID, post.ID, post.MAXMessageID, request, now)
		duplicate <- duplicateResult{op, isNew, claimErr}
	}()
	waitMAXCommentsBlockedBy(t, s, pid, 2)
	if err = blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-finished; err != nil {
		t.Fatalf("accepted provider result was not saved: %v", err)
	}
	repeated := <-duplicate
	if repeated.err != nil || repeated.isNew || repeated.operation.OperationID != operation.OperationID {
		t.Fatalf("duplicate claim=%+v", repeated)
	}
	saved, err := s.GetMAXCommentOperation(ctx, "test-owner", workspace.ID, post.ID, operation.OperationID)
	if err != nil || saved.State != "succeeded" || saved.MessageID != item.MessageID {
		t.Fatalf("known result lost: %+v, error=%v", saved, err)
	}
	var comments, operations int
	if err = s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM max_post_comments WHERE workspace_id=$1 AND post_id=$2),
(SELECT COUNT(*) FROM max_post_comment_operations WHERE workspace_id=$1 AND post_id=$2)`, workspace.ID, post.ID).Scan(&comments, &operations); err != nil || comments != 1 || operations != 1 {
		t.Fatalf("duplicate result rows: comments=%d operations=%d error=%v", comments, operations, err)
	}
}

func TestMAXCommentsPermanentSendUncertaintySurvivesFinishedEditHistory(t *testing.T) {
	t.Parallel()
	s, workspace, post, now := maxCommentsConcurrencyFixture(t, "max-comment-permanent-uncertainty")
	ctx := context.Background()
	request := maxCommentsTestSendRequest("uncertain-permanent-request")
	operation, _, err := s.ClaimMAXCommentOperation(ctx, "test-owner", workspace.ID, post.ID, post.MAXMessageID, request, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BeginMAXCommentWrite(ctx, "test-owner", workspace.ID, post.ID, post.MAXMessageID, operation.OperationID, now); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishMAXCommentOperation(ctx, operation.OperationID, "uncertain", nil, now); err != nil {
		t.Fatal(err)
	}
	item := MAXPostComment{MessageID: "known-edit-target", Text: "Original", SenderUserID: "42", SenderIsBot: true,
		TextEditable: true, CreatedAt: now.Add(-time.Minute), UpdatedAt: now, ObservedAt: now}
	if err = s.ObserveMAXComment(ctx, "test-owner", workspace.ID, post.ID, post.MAXMessageID, item); err != nil {
		t.Fatal(err)
	}
	for i := range 60 {
		at := now.Add(time.Duration(i+1) * time.Second)
		item.Text, item.UpdatedAt, item.ObservedAt = fmt.Sprintf("Edited %d", i), at, at
		edit := MAXCommentWriteRequest{ClientRequestID: fmt.Sprintf("completed-edit-request-%02d", i), RequestHash: fmt.Sprintf("%064x", i+1),
			Kind: "edit", MessageID: item.MessageID, ExpectedVersion: int64(i + 1), Text: item.Text, BotUserID: "42"}
		claimed, _, claimErr := s.ClaimMAXCommentOperation(ctx, "test-owner", workspace.ID, post.ID, post.MAXMessageID, edit, at)
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		if err = s.BeginMAXCommentWrite(ctx, "test-owner", workspace.ID, post.ID, post.MAXMessageID, claimed.OperationID, at); err != nil {
			t.Fatal(err)
		}
		if err = s.FinishMAXCommentOperation(ctx, claimed.OperationID, "succeeded", &item, at); err != nil {
			t.Fatal(err)
		}
	}
	later := now.Add(24 * time.Hour)
	snapshot, err := s.GetMAXCommentsSnapshot(ctx, "test-owner", workspace.ID, post.ID, "42", later)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, pending := range snapshot.Operations {
		if pending.OperationID == operation.OperationID {
			found = pending.State == "uncertain" && pending.WriteStarted
		}
	}
	if !found {
		t.Fatal("permanent send uncertainty was hidden by finished-operation history or lease expiry")
	}
	request.ClientRequestID = "new-key-must-not-resend"
	if _, _, err = s.ClaimMAXCommentOperation(ctx, "test-owner", workspace.ID, post.ID, post.MAXMessageID, request, later); !errors.Is(err, ErrMAXCommentUncertain) {
		t.Fatalf("new key could repeat an unknown accepted SEND: %v", err)
	}
}
