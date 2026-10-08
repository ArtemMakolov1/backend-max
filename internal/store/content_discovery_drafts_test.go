package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

const discoveryRequestID = "01234567-89ab-4cde-8f01-23456789abcd"

func TestDiscoveryDraftIsTenantScopedDurableAndIdempotentAfterCandidateExpiry(t *testing.T) {
	s, workspace, channel := openMAXHistoryStoreTest(t, "discovery-durable", 0)
	now := time.Now().UTC().Truncate(time.Microsecond)
	candidates, err := s.SaveDiscoveryCandidates(t.Context(), "history-editor", workspace.ID, &channel.ID, []json.RawMessage{json.RawMessage(`{"card":{"draft":{"content":"body"}},"media":[]}`)}, now)
	if err != nil {
		t.Fatal(err)
	}
	candidate := candidates[0]
	for _, actor := range []string{"foreign-user", "history-viewer", "test-owner"} {
		if _, err = s.GetDiscoveryCandidate(t.Context(), actor, workspace.ID, candidate.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("candidate leaked to other actor %s: %v", actor, err)
		}
	}
	draft := Post{Title: "Title", Content: "Body", Format: FormatMarkdown}
	op, err := s.ClaimDiscoveryDraft(t.Context(), "history-editor", workspace.ID, candidate.ID, discoveryRequestID, &channel.ID, true, draft, now)
	if err != nil || op.PostID <= 0 {
		t.Fatalf("claim=%#v err=%v", op, err)
	}
	if _, err = s.ClaimDiscoveryDraft(t.Context(), "history-editor", workspace.ID, candidate.ID, discoveryRequestID, &channel.ID, true, draft, now.Add(time.Second)); !errors.Is(err, ErrDiscoveryDraftBusy) {
		t.Fatalf("concurrent claim was not fenced: %v", err)
	}
	if _, err = s.ClaimDiscoveryDraft(t.Context(), "history-editor", workspace.ID, candidate.ID, discoveryRequestID, &channel.ID, false, draft, now.Add(time.Second)); !errors.Is(err, ErrDiscoveryRequestConflict) {
		t.Fatalf("changed request reused idempotency id: %v", err)
	}
	report := DiscoveryMediaTransfer{Status: "complete", Items: []DiscoveryTransferItem{}, Warnings: []string{}}
	if err = s.FinishDiscoveryDraft(t.Context(), "history-editor", workspace.ID, discoveryRequestID, op, report); err != nil {
		t.Fatal(err)
	}
	replayed, err := s.ClaimDiscoveryDraft(t.Context(), "history-editor", workspace.ID, candidate.ID, discoveryRequestID, &channel.ID, true, draft, now.Add(25*time.Hour))
	if err != nil || !replayed.Complete || replayed.PostID != op.PostID {
		t.Fatalf("completed request lost after card TTL: %#v err=%v", replayed, err)
	}
	unused, err := s.SaveDiscoveryCandidates(t.Context(), "history-editor", workspace.ID, nil, []json.RawMessage{json.RawMessage(`{"card":{},"media":[]}`)}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveDiscoveryCandidates(t.Context(), "history-editor", workspace.ID, nil, nil, now.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetDiscoveryCandidate(t.Context(), "history-editor", workspace.ID, unused[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired unused candidate survived cleanup: %v", err)
	}
	if retained, err := s.ClaimDiscoveryDraft(t.Context(), "history-editor", workspace.ID, candidate.ID, discoveryRequestID, &channel.ID, true, draft, now.Add(25*time.Hour)); err != nil || retained.PostID != op.PostID || !retained.Complete {
		t.Fatalf("cleanup broke durable accepted replay: %#v err=%v", retained, err)
	}
	if _, err = s.ClaimDiscoveryDraft(t.Context(), "history-editor", workspace.ID, candidate.ID, "11234567-89ab-4cde-8f01-23456789abcd", &channel.ID, true, draft, now.Add(25*time.Hour)); !errors.Is(err, ErrDiscoveryCandidateExpired) {
		t.Fatalf("expired card allowed new draft: %v", err)
	}
	posts, err := s.ListPostsForWorkspace(t.Context(), "history-editor", workspace.ID, "", nil)
	if err != nil || len(posts) != 1 {
		t.Fatalf("idempotent requests created duplicates: %#v err=%v", posts, err)
	}
}

func TestDiscoveryDraftTakeoverPreservesAcceptedAttachmentAndRejectsStaleWriters(t *testing.T) {
	s, workspace, _ := openMAXHistoryStoreTest(t, "discovery-takeover", 0)
	now := time.Now().UTC().Truncate(time.Microsecond)
	candidates, err := s.SaveDiscoveryCandidates(t.Context(), "history-editor", workspace.ID, nil, []json.RawMessage{json.RawMessage(`{"card":{},"media":[]}`)}, now)
	if err != nil {
		t.Fatal(err)
	}
	draft := Post{Title: "Title", Content: "Body", Format: FormatMarkdown}
	op, err := s.ClaimDiscoveryDraft(t.Context(), "history-editor", workspace.ID, candidates[0].ID, discoveryRequestID, nil, true, draft, now)
	if err != nil {
		t.Fatal(err)
	}
	limits := MediaLimits{MaxFiles: 5, MaxBytes: 1000}
	reservation, err := s.ReserveDiscoveryMediaForWorkspace(t.Context(), "history-editor", workspace.ID, "durable.png", 100, limits, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteMediaReservation(t.Context(), reservation, now); err != nil {
		t.Fatal(err)
	}
	attachment := PostAttachment{Type: PostAttachmentImage, StorageKey: "durable.png", MIMEType: "image/png", SizeBytes: 100, ProviderMeta: json.RawMessage(`{"preview_only":true}`)}
	id, err := s.AddDiscoveryDraftAttachment(t.Context(), "history-editor", workspace.ID, discoveryRequestID, op, attachment)
	if err != nil {
		t.Fatal(err)
	}
	takeover, err := s.ClaimDiscoveryDraft(t.Context(), "history-editor", workspace.ID, candidates[0].ID, discoveryRequestID, nil, true, draft, now.Add(4*time.Minute))
	if err != nil || takeover.PostID != op.PostID || takeover.Generation <= op.Generation || len(takeover.Report.Items) != 1 || takeover.Report.Items[0].AttachmentID != id || takeover.Report.Items[0].Reason != "preview_only" {
		t.Fatalf("durable attachment outcome lost on takeover: %#v err=%v", takeover, err)
	}
	if _, err = s.AddDiscoveryDraftAttachment(t.Context(), "history-editor", workspace.ID, discoveryRequestID, op, attachment); !errors.Is(err, ErrConflict) {
		t.Fatalf("old lease attached media: %v", err)
	}
	if err = s.FinishDiscoveryDraft(t.Context(), "history-editor", workspace.ID, discoveryRequestID, op, DiscoveryMediaTransfer{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("old lease replaced outcome: %v", err)
	}
	post, err := s.GetPostForWorkspace(t.Context(), "history-editor", workspace.ID, op.PostID)
	if err != nil {
		t.Fatal(err)
	}
	updatedText := "User edit during media copy"
	if _, err = s.UpdatePostForWorkspaceIfUnchanged(t.Context(), "history-editor", workspace.ID, post, PostChanges{Content: &updatedText}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddDiscoveryDraftAttachment(t.Context(), "history-editor", workspace.ID, discoveryRequestID, takeover, attachment); !errors.Is(err, ErrConflict) {
		t.Fatalf("media attached to user-modified post: %v", err)
	}
	if err = s.RemoveWorkspaceMember(t.Context(), "test-owner", workspace.ID, "history-editor"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReserveDiscoveryMediaForWorkspace(t.Context(), "history-editor", workspace.ID, "other.png", 1, limits, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked membership reserved media: %v", err)
	}
	if _, err = s.AddDiscoveryDraftAttachment(t.Context(), "history-editor", workspace.ID, discoveryRequestID, takeover, attachment); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked membership attached media: %v", err)
	}
}

func TestDiscoveryDraftWritersRecheckMembershipAfterWaitingForParentFence(t *testing.T) {
	for _, operation := range []string{"claim", "reserve", "attach"} {
		t.Run(operation, func(t *testing.T) {
			s, workspace, _ := openMAXHistoryStoreTest(t, "discovery-revoke-"+operation, 0)
			now := time.Now().UTC().Truncate(time.Microsecond)
			candidates, err := s.SaveDiscoveryCandidates(t.Context(), "history-editor", workspace.ID, nil, []json.RawMessage{json.RawMessage(`{"card":{},"media":[]}`)}, now)
			if err != nil {
				t.Fatal(err)
			}
			draft := Post{Title: "Title", Content: "Body", Format: FormatMarkdown}
			var op DiscoveryDraftOperation
			if operation == "attach" {
				op, err = s.ClaimDiscoveryDraft(t.Context(), "history-editor", workspace.ID, candidates[0].ID, discoveryRequestID, nil, true, draft, now)
				if err != nil {
					t.Fatal(err)
				}
				reservation, err := s.ReserveDiscoveryMediaForWorkspace(t.Context(), "history-editor", workspace.ID, "revoke.png", 1, MediaLimits{MaxFiles: 5, MaxBytes: 1000}, now)
				if err != nil {
					t.Fatal(err)
				}
				if err = s.CompleteMediaReservation(t.Context(), reservation, now); err != nil {
					t.Fatal(err)
				}
			}
			blocker, err := s.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = blocker.Rollback() }()
			var pid int
			if err = blocker.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			if _, err = blocker.ExecContext(t.Context(), `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace.ID); err != nil {
				t.Fatal(err)
			}
			finished := make(chan error, 1)
			go func() {
				var callErr error
				switch operation {
				case "claim":
					_, callErr = s.ClaimDiscoveryDraft(t.Context(), "history-editor", workspace.ID, candidates[0].ID, discoveryRequestID, nil, true, draft, now)
				case "reserve":
					_, callErr = s.ReserveDiscoveryMediaForWorkspace(t.Context(), "history-editor", workspace.ID, "revoke.png", 1, MediaLimits{MaxFiles: 5, MaxBytes: 1000}, now)
				case "attach":
					_, callErr = s.AddDiscoveryDraftAttachment(t.Context(), "history-editor", workspace.ID, discoveryRequestID, op, PostAttachment{Type: PostAttachmentImage, StorageKey: "revoke.png", MIMEType: "image/png", SizeBytes: 1})
				}
				finished <- callErr
			}()
			waitMAXCommentsBlockedBy(t, s, pid, 1)
			if _, err = blocker.ExecContext(t.Context(), `DELETE FROM workspace_members WHERE workspace_id=$1 AND user_id='history-editor'`, workspace.ID); err != nil {
				t.Fatal(err)
			}
			if err = blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-finished:
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("%s used access checked before waiting: %v", operation, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("revocation fence did not release the writer")
			}
		})
	}
}
