package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func maxCommentsStoreFixture(t *testing.T) (*Store, Workspace, Channel, Post, time.Time) {
	t.Helper()
	ctx := t.Context()
	s := openWorkspaceTestStore(t, "max-comments")
	w, err := s.CreateWorkspace(ctx, "test-owner", Workspace{Name: "MAX comments"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	seedBillingContract(t, s, w.ID, "pro", now.AddDate(0, -1, 0), now.AddDate(0, 1, 0), "synthetic-comment-method")
	for _, v := range []struct{ id, role string }{{"comment-editor", WorkspaceRoleEditor}, {"comment-viewer", WorkspaceRoleViewer}} {
		upsertWorkspaceUser(t, s, v.id, "")
		if _, err = s.AddWorkspaceMember(ctx, "test-owner", WorkspaceMember{WorkspaceID: w.ID, UserID: v.id, Role: v.role}); err != nil {
			t.Fatal(err)
		}
	}
	c, err := s.CreateChannelForWorkspace(ctx, "test-owner", w.ID, Channel{MAXChatID: "-90101", VerifiedMAXOwnerID: "max-owner", Title: "Comments", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	published := now.Add(-2 * time.Hour)
	p, err := s.CreatePostForWorkspace(ctx, "test-owner", w.ID, Post{Title: "Root", Content: "Text", Format: FormatMarkdown, Status: PostStatusPublished, ChannelID: &c.ID, MAXMessageID: "mid.root", PublishedAt: &published})
	if err != nil {
		t.Fatal(err)
	}
	return s, w, c, p, now
}

func maxCommentsTestItem(id string, created, observed time.Time) MAXPostComment {
	return MAXPostComment{MessageID: id, Text: "Question", SenderUserID: "71", SenderName: "Reader", CreatedAt: created, UpdatedAt: created, ObservedAt: observed, TextEditable: true}
}

func TestMAXCommentsObservedCoverageScopedSyncAndPermanentTombstones(t *testing.T) {
	t.Parallel()
	s, w, c, p, now := maxCommentsStoreFixture(t)
	ctx := t.Context()
	before, err := s.GetMAXCommentsSnapshot(ctx, "comment-viewer", w.ID, p.ID, "42", now)
	if err != nil || before.Metrics.ObservedTotal != nil || before.Coverage.LastSyncedAt != nil {
		t.Fatalf("unknown count became zero: %+v %v", before, err)
	}
	claim, err := s.ClaimMAXCommentSync(ctx, "comment-viewer", w.ID, p.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	items := make([]MAXPostComment, 100)
	for i := range items {
		items[i] = maxCommentsTestItem(fmt.Sprintf("mid.comment.%d", i), now.Add(-time.Hour+time.Duration(i)*time.Second), now)
	}
	if err = s.ApplyMAXCommentSync(ctx, "comment-viewer", w.ID, claim, items, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	view, err := s.GetMAXCommentsSnapshot(ctx, "comment-viewer", w.ID, p.ID, "42", now.Add(time.Second))
	if err != nil || len(view.Comments) != 100 || view.Metrics.ObservedTotal == nil || *view.Metrics.ObservedTotal != 100 || view.Metrics.Total != nil || view.Coverage.Complete || !view.Coverage.Truncated {
		t.Fatalf("wrong partial snapshot: %+v %v", view, err)
	}
	if _, err = s.ClaimMAXCommentSync(ctx, "comment-viewer", w.ID, p.ID, now.Add(2*time.Second)); !errors.Is(err, ErrMAXCommentBusy) {
		t.Fatalf("cooldown missing: %v", err)
	}
	if _, err = s.GetMAXCommentsSnapshot(ctx, "missing-user", w.ID, p.ID, "42", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign inbox readable: %v", err)
	}
	removedAt := now.Add(5 * time.Second)
	if err = s.ObserveMAXCommentRemoval(ctx, c.MAXChatID, p.MAXMessageID, "mid.comment.0", removedAt); err != nil {
		t.Fatal(err)
	}
	if err = s.ObserveMAXCommentRemoval(ctx, c.MAXChatID, p.MAXMessageID, "mid.comment.0", removedAt); err != nil {
		t.Fatal(err)
	}
	late := items[0]
	late.ObservedAt = now.Add(6 * time.Second)
	late.Text = "Late create must not resurrect"
	if err = s.ObserveMAXCommentForRoot(ctx, c.MAXChatID, p.MAXMessageID, late); err != nil {
		t.Fatal(err)
	}
	if err = s.ObserveKnownMAXCommentEvent(ctx, c.MAXChatID, maxCommentsTestItem("mid.unmapped", now, now)); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetMAXCommentsSnapshot(ctx, "test-owner", w.ID, p.ID, "42", now.Add(7*time.Second))
	if err != nil || *after.Metrics.ObservedTotal != 99 || *after.Metrics.ObservedDeleted != 1 || len(after.Comments) != 100 {
		t.Fatalf("deletion/unknown mapping lost: %+v %v", after, err)
	}
	for _, comment := range after.Comments {
		if comment.MessageID == "mid.comment.0" && (comment.DeletedAt == nil || comment.Text == late.Text || comment.Version != 2) {
			t.Fatalf("resurrected/duplicate deletion: %+v", comment)
		}
	}
}

func TestMAXCommentsSendKeysUncertaintyAndExactManualReconciliation(t *testing.T) {
	t.Parallel()
	s, w, c, p, now := maxCommentsStoreFixture(t)
	ctx := t.Context()
	r := MAXCommentWriteRequest{ClientRequestID: "request_send_00001", RequestHash: strings.Repeat("a", 64), Kind: "send", Text: "Answer", BotUserID: "42"}
	o, isNew, err := s.ClaimMAXCommentOperation(ctx, "comment-editor", w.ID, p.ID, p.MAXMessageID, r, now)
	if err != nil || !isNew {
		t.Fatalf("claim: %+v %v", o, err)
	}
	if err = s.BeginMAXCommentWrite(ctx, "comment-editor", w.ID, p.ID, p.MAXMessageID, o.OperationID, now); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishMAXCommentOperation(ctx, o.OperationID, "uncertain", nil, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	duplicate, newClaim, err := s.ClaimMAXCommentOperation(ctx, "comment-editor", w.ID, p.ID, p.MAXMessageID, r, now.Add(time.Second))
	if err != nil || newClaim || duplicate.State != "uncertain" {
		t.Fatalf("duplicate send repeated: %+v %v", duplicate, err)
	}
	r.ClientRequestID = "request_send_00002"
	if _, _, err = s.ClaimMAXCommentOperation(ctx, "test-owner", w.ID, p.ID, p.MAXMessageID, r, now.Add(2*time.Second)); !errors.Is(err, ErrMAXCommentUncertain) {
		t.Fatalf("new key bypassed uncertainty: %v", err)
	}
	item := maxCommentsTestItem("mid.received", now.Add(time.Second), now.Add(3*time.Second))
	item.Text = "Answer"
	item.SenderIsBot = true
	item.SenderUserID = "99"
	if err = s.ReconcileMAXCommentOperation(ctx, "comment-editor", w.ID, p.ID, p.MAXMessageID, o.OperationID, &item, now.Add(3*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("other bot accepted: %v", err)
	}
	item.SenderUserID = "42"
	item.CreatedAt = now.Add(-time.Minute)
	if err = s.ReconcileMAXCommentOperation(ctx, "comment-editor", w.ID, p.ID, p.MAXMessageID, o.OperationID, &item, now.Add(3*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("old same text accepted: %v", err)
	}
	item.CreatedAt = now.Add(time.Second)
	if err = s.ReconcileMAXCommentOperation(ctx, "comment-editor", w.ID, p.ID, p.MAXMessageID, o.OperationID, &item, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetMAXCommentOperation(ctx, "test-owner", w.ID, p.ID, o.OperationID)
	if err != nil || after.State != "succeeded" || after.MessageID != item.MessageID {
		t.Fatalf("reconcile not durable: %+v %v", after, err)
	}
	if _, _, err = s.ClaimMAXCommentOperation(ctx, "comment-viewer", w.ID, p.ID, p.MAXMessageID, r, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("viewer claimed: %v", err)
	}
	if _, _, err = s.ClaimMAXCommentOperation(ctx, "test-owner", w.ID, p.ID, "mid.other-root", r, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale root claimed: %v", err)
	}
	if err = s.ObserveMAXCommentRemoval(ctx, "-wrong-chat", p.MAXMessageID, item.MessageID, now.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.GetMAXCommentsSnapshot(ctx, "test-owner", w.ID, p.ID, "42", now)
	if err != nil || len(fresh.Comments) != 1 || fresh.Comments[0].DeletedAt != nil || fresh.RootMessageID != p.MAXMessageID || fresh.ChannelID != c.ID {
		t.Fatalf("wrong chat mutated thread: %+v %v", fresh, err)
	}
}

func TestMAXCommentsAnalyticsAreObservedPublicationScopedAndAsOfBounded(t *testing.T) {
	t.Parallel()
	s, w, c, p, now := maxCommentsStoreFixture(t)
	ctx := t.Context()
	claim, err := s.ClaimMAXCommentSync(ctx, "test-owner", w.ID, p.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	items := []MAXPostComment{maxCommentsTestItem("mid.inbound", now.Add(-time.Hour), now)}
	if err = s.ApplyMAXCommentSync(ctx, "test-owner", w.ID, claim, items, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	day := utcDate(now)
	report, err := s.GetWorkspaceAnalyticsContent(ctx, "test-owner", w.ID, &c.ID, day.AddDate(0, 0, -1), day, now.Add(2*time.Second), 0)
	if err != nil || report.Summary.ObservedComments == nil || *report.Summary.ObservedComments != 1 || report.Summary.CommentsSyncedPosts != 1 || report.Summary.CommentsCoverageComplete {
		t.Fatalf("observed report: %+v %v", report, err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE max_post_comment_sync SET truncated=TRUE WHERE workspace_id=$1 AND post_id=$2`, w.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetWorkspaceAnalyticsContent(ctx, "test-owner", w.ID, &c.ID, day.AddDate(0, 0, -1), day, now.Add(-time.Second), 0)
	if err != nil || before.Summary.ObservedComments != nil || len(before.Posts) != 1 || before.Posts[0].CommentsTruncated || before.Posts[0].CommentsLastSyncedAt != nil {
		t.Fatalf("future sync borrowed into asOf: %+v %v", before, err)
	}
	postBefore, err := s.maxCommentAnalyticsForPost(ctx, w.ID, p.ID, p.MAXMessageID, now.Add(-time.Second))
	if err != nil || postBefore.ObservedComments != nil || postBefore.CommentsTruncated || postBefore.CommentsLastSyncedAt != nil {
		t.Fatalf("future post sync borrowed into asOf: %+v %v", postBefore, err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE posts SET max_message_id='mid.republished' WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	oldReport := AnalyticsContentReport{Posts: []AnalyticsContentPost{{ID: p.ID, RootMessageID: p.MAXMessageID}}}
	if err = s.applyWorkspaceMAXCommentAnalytics(ctx, w.ID, now.Add(2*time.Second), &oldReport); err != nil || oldReport.Posts[0].ObservedComments == nil || *oldReport.Posts[0].ObservedComments != 1 {
		t.Fatalf("retained requested root changed: %+v %v", oldReport, err)
	}
	newReport := AnalyticsContentReport{Posts: []AnalyticsContentPost{{ID: p.ID, RootMessageID: "mid.republished"}}}
	if err = s.applyWorkspaceMAXCommentAnalytics(ctx, w.ID, now.Add(2*time.Second), &newReport); err != nil || newReport.Posts[0].ObservedComments != nil {
		t.Fatalf("old thread reused for new root: %+v %v", newReport, err)
	}
}

func TestMAXCommentsRequestedFinishedKeySurvivesRecentHistoryAndStaysRootScoped(t *testing.T) {
	t.Parallel()
	s, w, _, p, now := maxCommentsStoreFixture(t)
	ctx := t.Context()
	r := MAXCommentWriteRequest{ClientRequestID: "lost_response_request_01", RequestHash: strings.Repeat("a", 64), Kind: "send", Text: "Reply", BotUserID: "42"}
	op, _, err := s.ClaimMAXCommentOperation(ctx, "test-owner", w.ID, p.ID, p.MAXMessageID, r, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BeginMAXCommentWrite(ctx, "test-owner", w.ID, p.ID, p.MAXMessageID, op.OperationID, now); err != nil {
		t.Fatal(err)
	}
	item := maxCommentsTestItem("mid.confirmed", now, now)
	if err = s.FinishMAXCommentOperation(ctx, op.OperationID, "succeeded", &item, now); err != nil {
		t.Fatal(err)
	}
	// Other members' later completed operations can push a lost-response SEND
	// outside the normal ten-operation window; the browser still owns its key.
	_, err = s.db.ExecContext(ctx, `INSERT INTO max_post_comment_operations(operation_id,workspace_id,post_id,channel_id,root_message_id,client_request_id,request_hash,bot_user_id,kind,state,write_started,message_id,claimed_at,finished_at)
SELECT 'mco_'||md5(i::text),$1,$2,$3,$4,'later-finished-request-'||i,repeat('b',64),'42','edit','succeeded',TRUE,'mid.confirmed',$5::timestamptz+i*INTERVAL '1 second',$5::timestamptz+i*INTERVAL '1 second' FROM generate_series(1,60) i`, w.ID, p.ID, *p.ChannelID, p.MAXMessageID, now)
	if err != nil {
		t.Fatal(err)
	}
	view, err := s.GetMAXCommentsSnapshot(ctx, "comment-viewer", w.ID, p.ID, "42", now.Add(time.Minute), r.ClientRequestID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, saved := range view.Operations {
		if saved.ClientRequestID == r.ClientRequestID {
			found = saved.State == "succeeded" && saved.MessageID == item.MessageID
		}
	}
	if !found || len(view.Operations) != 11 {
		t.Fatalf("known finished key not recovered: %+v", view.Operations)
	}
	if _, err = s.GetMAXCommentsSnapshot(ctx, "comment-viewer", w.ID, p.ID, "42", now, "invalid"); !errors.Is(err, ErrMAXCommentValidation) {
		t.Fatalf("invalid request key accepted: %v", err)
	}
	if _, err = s.GetMAXCommentsSnapshot(ctx, "comment-viewer", w.ID, p.ID, "42", now, make([]string, 21)...); !errors.Is(err, ErrMAXCommentValidation) {
		t.Fatalf("unbounded request keys accepted: %v", err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE posts SET max_message_id='mid.new-root' WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	view, err = s.GetMAXCommentsSnapshot(ctx, "comment-viewer", w.ID, p.ID, "42", now.Add(time.Minute), r.ClientRequestID)
	if err != nil || len(view.Operations) != 0 {
		t.Fatalf("old-root operation exposed in new thread: %+v %v", view, err)
	}
}

func TestMAXCommentsLongProviderNamesDoNotRejectSyncOrWebhook(t *testing.T) {
	t.Parallel()
	s, w, c, p, now := maxCommentsStoreFixture(t)
	claim, err := s.ClaimMAXCommentSync(t.Context(), "test-owner", w.ID, p.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	item := maxCommentsTestItem("mid.long-name", now, now)
	item.SenderName = strings.Repeat("🙂", 250)
	if err = s.ApplyMAXCommentSync(t.Context(), "test-owner", w.ID, claim, []MAXPostComment{item}, now.Add(time.Second)); err != nil {
		t.Fatalf("valid long name rejected entire page: %v", err)
	}
	item.SenderName = strings.Repeat("Ж", 250)
	item.ObservedAt = now.Add(2 * time.Second)
	if err = s.ObserveMAXCommentForRoot(t.Context(), c.MAXChatID, p.MAXMessageID, item); err != nil {
		t.Fatalf("valid long webhook name rejected: %v", err)
	}
	view, err := s.GetMAXCommentsSnapshot(t.Context(), "comment-viewer", w.ID, p.ID, "42", now.Add(3*time.Second))
	if err != nil || len(view.Comments) != 1 || view.Comments[0].SenderName != strings.Repeat("Ж", 200) || view.Comments[0].SenderUserID != item.SenderUserID {
		t.Fatalf("display name or exact sender damaged: %+v %v", view, err)
	}
}
