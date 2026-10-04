package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/store"
)

type commentsMAXFake struct {
	*fakeMAX
	items                                                  []maxclient.CommentMessage
	sendResult                                             maxclient.CommentMessage
	sendErr, editCommentErr, deleteCommentErr, readbackErr error
	sends, edits, deletes, lists, gets                     int
	lastCommentRequest                                     maxclient.CommentRequest
	applyOnError                                           bool
	afterSend                                              func()
}

func (f *commentsMAXFake) GetComments(_ context.Context, _ string, q maxclient.CommentsQuery) (maxclient.CommentsPage, error) {
	f.lists++
	if q.Count != 100 {
		panic("unbounded comments page")
	}
	return maxclient.CommentsPage{Messages: append([]maxclient.CommentMessage{}, f.items...)}, nil
}
func (f *commentsMAXFake) GetComment(_ context.Context, _, id string) (maxclient.CommentMessage, error) {
	f.gets++
	if f.edits > 0 && f.readbackErr != nil {
		return maxclient.CommentMessage{}, f.readbackErr
	}
	for _, c := range f.items {
		if c.MessageID == id {
			return c, nil
		}
	}
	return maxclient.CommentMessage{}, &maxclient.Error{StatusCode: http.StatusNotFound, Code: "not.found"}
}
func (f *commentsMAXFake) SendComment(_ context.Context, _ string, r maxclient.CommentRequest) (maxclient.CommentMessage, error) {
	f.sends++
	f.lastCommentRequest = r
	if f.afterSend != nil {
		defer f.afterSend()
	}
	if f.sendErr == nil || f.applyOnError {
		c := f.sendResult
		c.Text = r.Text
		c.ReplyTo = r.ReplyTo
		f.items = append(f.items, c)
		return c, f.sendErr
	}
	return maxclient.CommentMessage{}, f.sendErr
}
func (f *commentsMAXFake) EditComment(_ context.Context, _, id string, r maxclient.CommentRequest) error {
	f.edits++
	f.lastCommentRequest = r
	if f.editCommentErr == nil || f.applyOnError {
		for i := range f.items {
			if f.items[i].MessageID == id {
				f.items[i].Text = r.Text
				f.items[i].ReplyTo = r.ReplyTo
			}
		}
	}
	return f.editCommentErr
}
func (f *commentsMAXFake) DeleteComment(context.Context, string, string) error {
	f.deletes++
	return f.deleteCommentErr
}

func maxCommentsAppFixture(t *testing.T) (*App, *commentsMAXFake, string, store.Post, time.Time) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := &commentsMAXFake{fakeMAX: &fakeMAX{chat: maxclient.ChatInfo{ChatID: "-1001", Type: "channel", Status: "active", OwnerID: "test-max-owner"}, membership: maxclient.Membership{UserID: 42, IsBot: true, IsAdmin: true, Permissions: []maxclient.Permission{maxclient.PermissionReadAllMessages, maxclient.PermissionWrite, maxclient.PermissionDelete}}}}
	a, s := newTestApp(t, f)
	a.now = func() time.Time { return now }
	workspaces, err := s.ListWorkspaces(t.Context(), "test-owner")
	if err != nil {
		t.Fatal(err)
	}
	w := workspaces[0].Workspace
	c, err := s.CreateChannelForWorkspace(t.Context(), "test-owner", w.ID, store.Channel{MAXChatID: "-1001", VerifiedMAXOwnerID: "test-max-owner", Title: "Channel", IsChannel: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	published := now.Add(-2 * time.Hour)
	p, err := s.CreatePostForWorkspace(t.Context(), "test-owner", w.ID, store.Post{Title: "Post", Content: "body", Format: store.FormatMarkdown, Status: store.PostStatusPublished, MAXMessageID: "mid.root", ChannelID: &c.ID, PublishedAt: &published})
	if err != nil {
		t.Fatal(err)
	}
	f.items = []maxclient.CommentMessage{{MessageID: "mid.reader", ChatID: c.MAXChatID, PostID: p.MAXMessageID, Text: "Question", TimestampMillis: now.Add(-time.Hour).UnixMilli(), SenderUserID: "71", SenderName: "Reader", Raw: json.RawMessage(`{"body":{"markup":[]}}`)}}
	f.sendResult = maxclient.CommentMessage{MessageID: "mid.botreply", ChatID: c.MAXChatID, PostID: p.MAXMessageID, TimestampMillis: now.UnixMilli(), SenderUserID: "42", SenderIsBot: true, Raw: json.RawMessage(`{"body":{"markup":[]}}`)}
	return a, f, w.ID, p, now
}
func maxCommentTestMutation(root, key string) MAXCommentMutation {
	return MAXCommentMutation{ExpectedRootMessageID: root, ClientRequestID: key, Text: "Answer", ReplyToMessageID: "mid.reader"}
}

func TestMAXCommentSuccessfulSendIsIdempotentAndSurvivesCancellation(t *testing.T) {
	t.Parallel()
	a, f, w, p, _ := maxCommentsAppFixture(t)
	if _, err := a.SyncMAXComments(t.Context(), "test-owner", w, p.ID, p.MAXMessageID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	f.afterSend = cancel
	r := maxCommentTestMutation(p.MAXMessageID, "send_request_0001")
	result, err := a.MutateMAXComment(ctx, "test-owner", w, p.ID, "send", "", r)
	if err != nil || result.Operation == nil || result.Operation.State != "succeeded" || f.sends != 1 || *result.Metrics.ObservedReplies != 1 || *result.Metrics.ResponseRate != 1 {
		t.Fatalf("lost durable reply after cancellation: %+v %v", result, err)
	}
	again, err := a.MutateMAXComment(t.Context(), "test-owner", w, p.ID, "send", "", r)
	if err != nil || again.Operation.MessageID != "mid.botreply" || f.sends != 1 {
		t.Fatalf("duplicate key re-sent: %+v %v", again, err)
	}
	r.Text = "Another payload"
	if _, err = a.MutateMAXComment(t.Context(), "test-owner", w, p.ID, "send", "", r); !errors.Is(err, store.ErrMAXCommentUncertain) || f.sends != 1 {
		t.Fatalf("same-key payload changed: %v sends%d", err, f.sends)
	}
}

func TestMAXCommentUncertainSendRequiresExplicitExactOwnReconciliation(t *testing.T) {
	t.Parallel()
	a, f, w, p, _ := maxCommentsAppFixture(t)
	if _, err := a.SyncMAXComments(t.Context(), "test-owner", w, p.ID, p.MAXMessageID); err != nil {
		t.Fatal(err)
	}
	f.sendErr = context.DeadlineExceeded
	f.applyOnError = true
	r := maxCommentTestMutation(p.MAXMessageID, "send_request_0001")
	if _, err := a.MutateMAXComment(t.Context(), "test-owner", w, p.ID, "send", "", r); !errors.Is(err, store.ErrMAXCommentUncertain) {
		t.Fatalf("unknown send accepted: %v", err)
	}
	r.ClientRequestID = "send_request_0002"
	if _, err := a.MutateMAXComment(t.Context(), "test-owner", w, p.ID, "send", "", r); !errors.Is(err, store.ErrMAXCommentUncertain) || f.sends != 1 {
		t.Fatalf("unknown send retried: %v", err)
	}
	view, err := a.GetMAXComments(t.Context(), "test-owner", w, p.ID, p.MAXMessageID)
	if err != nil || view.Operations[0].State != "uncertain" {
		t.Fatalf("uncertainty not visible: %+v %v", view, err)
	}
	if _, err = a.ReconcileMAXCommentSend(t.Context(), "test-owner", w, p.ID, p.MAXMessageID, view.Operations[0].OperationID, "mid.reader"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("reader mapped as own reply: %v", err)
	}
	settled, err := a.ReconcileMAXCommentSend(t.Context(), "test-owner", w, p.ID, p.MAXMessageID, view.Operations[0].OperationID, "mid.botreply")
	if err != nil || settled.Operation.State != "succeeded" || f.sends != 1 {
		t.Fatalf("manual read-only reconcile failed: %+v %v", settled, err)
	}
}

func TestMAXCommentKnownRejectNeverBlocksThreadAndNullableSenderSuccessIsKnown(t *testing.T) {
	t.Parallel()
	a, f, w, p, _ := maxCommentsAppFixture(t)
	if _, err := a.SyncMAXComments(t.Context(), "test-owner", w, p.ID, p.MAXMessageID); err != nil {
		t.Fatal(err)
	}
	f.sendErr = &maxclient.Error{StatusCode: http.StatusOK, Code: "comments.disabled"}
	r := maxCommentTestMutation(p.MAXMessageID, "send_request_0001")
	if _, err := a.MutateMAXComment(t.Context(), "test-owner", w, p.ID, "send", "", r); err == nil || errors.Is(err, store.ErrMAXCommentUncertain) {
		t.Fatalf("explicit reject became uncertain: %v", err)
	}
	f.sendErr = nil
	f.sendResult.SenderUserID = ""
	f.sendResult.SenderIsBot = false
	r.ClientRequestID = "send_request_0002"
	view, err := a.MutateMAXComment(t.Context(), "test-owner", w, p.ID, "send", "", r)
	if err != nil || view.Operation.State != "succeeded" || view.Operation.MessageID != "mid.botreply" || *view.Metrics.ObservedInbound != 1 || *view.Metrics.ObservedReplies != 1 || *view.Metrics.ResponseRate != 1 {
		t.Fatalf("nullable sender ack lost or counted inbound: %+v %v", view, err)
	}
	for _, c := range view.Comments {
		if c.MessageID == "mid.botreply" && (c.IsOwn || c.CanEdit) {
			t.Fatalf("unknown sender advertised editable: %+v", c)
		}
	}
}

func TestMAXCommentOwnEditWithoutWriteAndArchivedReadDelete(t *testing.T) {
	t.Parallel()
	a, f, w, p, now := maxCommentsAppFixture(t)
	own := f.sendResult
	own.TimestampMillis = now.Add(-time.Hour).UnixMilli()
	own.Text = "Old answer"
	own.ReplyTo = "mid.reader"
	f.items = append(f.items, own)
	f.membership.Permissions = []maxclient.Permission{maxclient.PermissionReadAllMessages, maxclient.PermissionDelete}
	f.chat.Status = "closed"
	view, err := a.SyncMAXComments(t.Context(), "test-owner", w, p.ID, p.MAXMessageID)
	if err != nil || view.Capabilities.CanReply || !view.Capabilities.CanDelete {
		t.Fatalf("archived old comments unavailable: %+v %v", view, err)
	}
	var version int64
	for _, c := range view.Comments {
		if c.MessageID == own.MessageID {
			if !c.CanEdit {
				t.Fatal("own edit incorrectly requires write")
			}
			version = c.Version
		}
	}
	r := MAXCommentMutation{ExpectedRootMessageID: p.MAXMessageID, ClientRequestID: "edit_request_0001", Text: "Corrected answer", ExpectedVersion: version}
	saved, err := a.MutateMAXComment(t.Context(), "test-owner", w, p.ID, "edit", own.MessageID, r)
	if err != nil || f.edits != 1 || f.lastCommentRequest.ReplyTo != "mid.reader" {
		t.Fatalf("own archived edit/link preservation: %+v %v", saved, err)
	}
	for _, c := range saved.Comments {
		if c.MessageID == own.MessageID {
			version = c.Version
		}
	}
	r = MAXCommentMutation{ExpectedRootMessageID: p.MAXMessageID, ClientRequestID: "delete_request_001", ExpectedVersion: version}
	deleted, err := a.MutateMAXComment(t.Context(), "test-owner", w, p.ID, "delete", own.MessageID, r)
	if err != nil || f.deletes != 1 {
		t.Fatalf("old archived delete: %+v %v", deleted, err)
	}
	r = maxCommentTestMutation(p.MAXMessageID, "send_request_0001")
	if _, err = a.MutateMAXComment(t.Context(), "test-owner", w, p.ID, "send", "", r); err == nil || f.sends != 0 {
		t.Fatalf("send to archived channel: %v", err)
	}
}

func TestMAXCommentAcceptedEditReadbackFailureStaysUncertainAndNeverDeletesOn404(t *testing.T) {
	t.Parallel()
	for _, status := range []int{403, 404, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			a, f, w, p, now := maxCommentsAppFixture(t)
			own := f.sendResult
			own.TimestampMillis = now.Add(-time.Hour).UnixMilli()
			own.Text = "Old answer"
			f.items = append(f.items, own)
			view, err := a.SyncMAXComments(t.Context(), "test-owner", w, p.ID, p.MAXMessageID)
			if err != nil {
				t.Fatal(err)
			}
			var version int64
			for _, c := range view.Comments {
				if c.MessageID == own.MessageID {
					version = c.Version
				}
			}
			f.readbackErr = &maxclient.Error{StatusCode: status}
			r := MAXCommentMutation{ExpectedRootMessageID: p.MAXMessageID, ClientRequestID: "edit_request_0001", Text: "New answer", ExpectedVersion: version}
			if _, err = a.MutateMAXComment(t.Context(), "test-owner", w, p.ID, "edit", own.MessageID, r); !errors.Is(err, store.ErrMAXCommentUncertain) {
				t.Fatalf("accepted edit readback %d misclassified: %v", status, err)
			}
			f.readbackErr = nil
			r.ClientRequestID = "edit_request_0002"
			if _, err = a.MutateMAXComment(t.Context(), "test-owner", w, p.ID, "edit", own.MessageID, r); err == nil || f.edits != 1 {
				t.Fatalf("accepted write repeated: %v", err)
			}
			snapshot, err := a.GetMAXComments(t.Context(), "test-owner", w, p.ID, p.MAXMessageID)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range snapshot.Comments {
				if c.MessageID == own.MessageID && c.DeletedAt != nil {
					t.Fatal("read error invented deletion")
				}
			}
		})
	}
}

func TestMAXCommentStaleRootAndInvalidInputCauseNoProviderCalls(t *testing.T) {
	t.Parallel()
	a, f, w, p, _ := maxCommentsAppFixture(t)
	r := maxCommentTestMutation("mid.old-root", "send_request_0001")
	if _, err := a.MutateMAXComment(t.Context(), "test-owner", w, p.ID, "send", "", r); !errors.Is(err, store.ErrConflict) || f.getChatCalls != 0 || f.sends != 0 {
		t.Fatalf("stale root reached provider: %v", err)
	}
	r.ExpectedRootMessageID = p.MAXMessageID
	r.ReplyToMessageID = ".bad:mid"
	if _, err := a.MutateMAXComment(t.Context(), "test-owner", w, p.ID, "send", "", r); !errors.Is(err, store.ErrMAXCommentValidation) || f.getChatCalls != 0 {
		t.Fatalf("invalid ID reached provider: %v", err)
	}
	if _, err := a.SyncMAXComments(t.Context(), "test-owner", w, p.ID, ""); !errors.Is(err, store.ErrMAXCommentValidation) || f.lists != 0 {
		t.Fatalf("unfenced sync accepted: %v", err)
	}
}

func TestMAXCommentAcceptedSendRootChangeCannotBecomeDefinitiveRejection(t *testing.T) {
	t.Parallel()
	a, f, w, p, _ := maxCommentsAppFixture(t)
	if _, err := a.SyncMAXComments(t.Context(), "test-owner", w, p.ID, p.MAXMessageID); err != nil {
		t.Fatal(err)
	}
	f.afterSend = func() {
		if _, err := a.store.ClaimPublishedForUpdate(t.Context(), p); err != nil {
			t.Error(err)
			return
		}
		if _, err := a.store.MarkPublished(t.Context(), p.ID, "mid.new-root", "https://max.ru/new-root"); err != nil {
			t.Error(err)
		}
	}
	r := maxCommentTestMutation(p.MAXMessageID, "send_request_0001")
	if _, err := a.MutateMAXComment(t.Context(), "test-owner", w, p.ID, "send", "", r); !errors.Is(err, store.ErrMAXCommentUncertain) || f.sends != 1 {
		t.Fatalf("post-ACK view conflict became safe rejection: %v sends%d", err, f.sends)
	}
	f.afterSend = nil
	if _, err := a.MutateMAXComment(t.Context(), "test-owner", w, p.ID, "send", "", r); !errors.Is(err, store.ErrMAXCommentUncertain) || f.sends != 1 {
		t.Fatalf("known old key became safe resend: %v sends%d", err, f.sends)
	}
	written, err := a.store.MAXCommentRequestWasWritten(t.Context(), w, p.ID, r.ClientRequestID)
	if err != nil || !written {
		t.Fatalf("old root ACK was lost: %v %v", written, err)
	}
}
