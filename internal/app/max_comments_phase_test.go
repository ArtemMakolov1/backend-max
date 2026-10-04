package app

import (
	"context"
	"errors"
	"testing"

	"maxpilot/backend/internal/maxclient"
	"maxpilot/backend/internal/store"
)

// The outer request has already read an empty operation state when it reaches
// GetChat. Run another request here, then fail the outer provider inspection.
// This deterministically exercises the request-phase race without timing waits.
type earlyPhaseCommentsMAX struct {
	*commentsMAXFake
	onInspect func()
	fired     bool
}

func (f *earlyPhaseCommentsMAX) GetChat(ctx context.Context, id string) (maxclient.ChatInfo, error) {
	if !f.fired {
		f.fired = true
		f.onInspect()
		return maxclient.ChatInfo{}, context.DeadlineExceeded
	}
	return f.fakeMAX.GetChat(ctx, id)
}

func TestMAXCommentEarlyProviderFailureRechecksConcurrentSameKeyPhase(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"succeeded", "sending", "rejected", "absent"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			a, f, workspace, post, now := maxCommentsAppFixture(t)
			request := maxCommentTestMutation(post.MAXMessageID, "same_request_00001")
			request.ReplyToMessageID = ""
			wrapped := &earlyPhaseCommentsMAX{commentsMAXFake: f}
			a.max = wrapped
			wrapped.onInspect = func() {
				if state == "absent" {
					return
				}
				if state == "succeeded" {
					if _, err := a.MutateMAXComment(t.Context(), "test-owner", workspace, post.ID, "send", "", request); err != nil {
						t.Fatalf("concurrent accepted request: %v", err)
					}
					return
				}
				operation, isNew, err := a.store.ClaimMAXCommentOperation(t.Context(), "test-owner", workspace, post.ID, post.MAXMessageID, store.MAXCommentWriteRequest{
					ClientRequestID: request.ClientRequestID,
					RequestHash:     maxCommentRequestHash(post.MAXMessageID, "send", "", request),
					Kind:            "send",
					Text:            request.Text,
					BotUserID:       "42",
				}, now)
				if err != nil || !isNew {
					t.Fatalf("concurrent claim: %+v new=%v err=%v", operation, isNew, err)
				}
				if state == "rejected" {
					if err = a.store.FinishMAXCommentOperation(t.Context(), operation.OperationID, "rejected", nil, now); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, err := a.MutateMAXComment(t.Context(), "test-owner", workspace, post.ID, "send", "", request)
			var notWritten *MAXCommentNotWrittenError
			if state == "succeeded" || state == "sending" {
				if !errors.Is(err, store.ErrMAXCommentUncertain) || errors.As(err, &notWritten) {
					t.Fatalf("same-key %s operation mislabeled as safe rejection: %v", state, err)
				}
			} else if !errors.As(err, &notWritten) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("safe prewrite failure lost its phase: %v", err)
			}
			wantSends := 0
			if state == "succeeded" {
				wantSends = 1
			}
			if f.sends != wantSends {
				t.Fatalf("provider action count=%d, want=%d", f.sends, wantSends)
			}
		})
	}
}
