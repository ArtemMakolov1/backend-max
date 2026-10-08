package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func externalOperationFixture(t *testing.T) (*Store, string, Workspace, DirectConnection, DirectExternalEditFence, time.Time) {
	t.Helper()
	ctx := t.Context()
	s, owner, ws := newDirectStoreFixture(t, ctx)
	connection := connectDirectTestAccount(t, ctx, s, owner, ws.ID)
	now := time.Now().UTC()
	seedExternalEditTestSnapshot(t, s, owner, ws.ID, connection.ID, now)
	control, err := s.ObserveDirectExternalEdit(ctx, owner, ws.ID, connection.ID, 77, strings.Repeat("a", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	return s, owner, ws, connection, DirectExternalEditFence{ConnectionID: connection.ID, Version: control.Version, ObservedHash: control.ObservedHash, RevisionID: control.RevisionID}, now
}
func claimExternalOperation(t *testing.T, s *Store, owner string, ws Workspace, fence DirectExternalEditFence, now time.Time) DirectExternalOperation {
	t.Helper()
	o, isNew, err := s.ClaimDirectExternalOperation(t.Context(), owner, ws.ID, 77, fence, "11111111-1111-4111-8111-111111111111", "create_keyword", strings.Repeat("b", 64), strings.Repeat("c", 64), json.RawMessage(`{"keyword":"курс английского"}`), now)
	if err != nil || !isNew {
		t.Fatal("claim failed", err)
	}
	return o
}
func TestExternalOperationJournalConcurrentClaimAndImmutableRequest(t *testing.T) {
	t.Parallel()
	s, owner, ws, connection, fence, now := externalOperationFixture(t)
	var wg sync.WaitGroup
	count := 0
	var mutex sync.Mutex
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, fresh, err := s.ClaimDirectExternalOperation(t.Context(), owner, ws.ID, 77, fence, "11111111-1111-4111-8111-111111111111", "create_keyword", strings.Repeat("b", 64), strings.Repeat("c", 64), json.RawMessage(`{"keyword":"курс английского"}`), now)
			if err != nil {
				t.Error(err)
			}
			if fresh {
				mutex.Lock()
				count++
				mutex.Unlock()
			}
		}()
	}
	wg.Wait()
	if count != 1 {
		t.Fatalf("provider operation may replay: %d", count)
	}
	if _, _, err := s.ClaimDirectExternalOperation(t.Context(), owner, ws.ID, 77, fence, "11111111-1111-4111-8111-111111111111", "create_keyword", strings.Repeat("d", 64), strings.Repeat("c", 64), json.RawMessage(`{}`), now); !errors.Is(err, ErrConflict) {
		t.Fatal("UUID reused for different mutation", err)
	}
	if _, err := s.GetDirectExternalOperation(t.Context(), "outsider", ws.ID, connection.ID, 77, "11111111-1111-4111-8111-111111111111"); !errors.Is(err, ErrNotFound) {
		t.Fatal("journal crossed tenant boundary", err)
	}
}

func TestExternalOperationJournalPreservesProviderSizedGroupPayload(t *testing.T) {
	t.Parallel()
	s, owner, ws, connection, fence, now := externalOperationFixture(t)
	regions := make([]int64, 1000)
	for i := range regions {
		regions[i] = 1000000000000000 + int64(i)
	}
	phrases := make([]string, 4096)
	for i := range phrases {
		phrases[i] = "я"
	}
	payload, err := json.Marshal(map[string]any{"region_ids": regions, "negative_keywords": phrases})
	if err != nil || len(payload) <= 32768 || len(payload) > 65536 {
		t.Fatal("invalid large group fixture", len(payload), err)
	}
	o, fresh, err := s.ClaimDirectExternalOperation(t.Context(), owner, ws.ID, 77, fence, "11111111-1111-4111-8111-111111111111", "group", strings.Repeat("b", 64), strings.Repeat("c", 64), payload, now)
	if err != nil || !fresh {
		t.Fatal("valid group payload could not be durably claimed", err)
	}
	saved, err := s.GetDirectExternalOperation(t.Context(), owner, ws.ID, connection.ID, 77, o.ClientRequestID)
	var decoded struct {
		Regions []int64  `json:"region_ids"`
		Phrases []string `json:"negative_keywords"`
	}
	if err != nil || json.Unmarshal(saved.Payload, &decoded) != nil || len(decoded.Phrases) != 4096 || len(decoded.Regions) != 1000 || decoded.Regions[999] != regions[999] {
		t.Fatal("large provider payload lost exact targeting or phrases", err)
	}
	oversized, err := json.Marshal(map[string]string{"name": strings.Repeat("x", 65536)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClaimDirectExternalOperation(t.Context(), owner, ws.ID, 77, fence, "22222222-2222-4222-8222-222222222222", "group", strings.Repeat("b", 64), strings.Repeat("c", 64), oversized, now); !errors.Is(err, ErrDirectValidation) {
		t.Fatal("unbounded operation payload accepted", err)
	}
}
func TestExternalOperationAcknowledgmentImmutableAndFinalizerCannotRelockSuccess(t *testing.T) {
	t.Parallel()
	s, owner, ws, connection, fence, now := externalOperationFixture(t)
	o := claimExternalOperation(t, s, owner, ws, fence, now)
	if err := s.BeginDirectExternalOperation(t.Context(), owner, ws.ID, connection.ID, 77, o.OperationID, "create_keyword", now); err != nil {
		t.Fatal(err)
	}
	if err := s.AcknowledgeDirectExternalOperation(t.Context(), o.OperationID, 99, now); err != nil {
		t.Fatal(err)
	}
	if err := s.AcknowledgeDirectExternalOperation(t.Context(), o.OperationID, 100, now); !errors.Is(err, ErrConflict) {
		t.Fatal("provider acknowledgement overwritten", err)
	}
	if err := s.FinishDirectExternalOperation(t.Context(), ws.ID, connection.ID, 77, o.OperationID, "succeeded", now); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDirectExternalOperation(t.Context(), ws.ID, connection.ID, 77, o.OperationID, "uncertain", now); err != nil {
		t.Fatal(err)
	}
	view, err := s.GetDirectExternalOperation(t.Context(), owner, ws.ID, connection.ID, 77, o.ClientRequestID)
	if err != nil || view.State != "succeeded" || *view.ProviderResultID != 99 {
		t.Fatal("completed journal state changed", err)
	}
	c, err := s.ObserveDirectExternalEdit(t.Context(), owner, ws.ID, connection.ID, 77, fence.ObservedHash, now)
	if err != nil || c.EditState != "idle" {
		t.Fatal("stale finalizer relocked control", err)
	}
}
func TestExternalOperationExpiredBeforeWriteIsRejectedWithoutReleasingAmbiguousWrite(t *testing.T) {
	t.Parallel()
	for _, started := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsent", true: "sent"}[started], func(t *testing.T) {
			s, owner, ws, connection, fence, now := externalOperationFixture(t)
			o := claimExternalOperation(t, s, owner, ws, fence, now)
			if started {
				if err := s.BeginDirectExternalOperation(t.Context(), owner, ws.ID, connection.ID, 77, o.OperationID, "create_keyword", now); err != nil {
					t.Fatal(err)
				}
			}
			expired := now.Add(3 * time.Minute)
			if err := s.RejectExpiredUnstartedDirectExternalOperation(t.Context(), ws.ID, connection.ID, 77, o.OperationID, expired); err != nil {
				t.Fatal(err)
			}
			view, err := s.GetDirectExternalOperation(t.Context(), owner, ws.ID, connection.ID, 77, o.ClientRequestID)
			if err != nil {
				t.Fatal(err)
			}
			if !started {
				if view.State != "rejected" {
					t.Fatal("unsent journal did not expire")
				}
				if err = s.BeginDirectExternalOperation(t.Context(), owner, ws.ID, connection.ID, 77, o.OperationID, "create_keyword", expired); !errors.Is(err, ErrConflict) {
					t.Fatal("expired worker could contact provider", err)
				}
			} else if view.State != "updating" {
				t.Fatal("ambiguous started operation unlocked")
			}
		})
	}
}
func TestExternalOperationBeginRechecksRoleAfterClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, owner, personal := newDirectStoreFixture(t, ctx)
	_ = personal
	ws, err := s.CreateWorkspace(ctx, owner, Workspace{Name: "External controls team"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seedBillingContract(t, s, ws.ID, "pro", now.Add(-time.Hour), now.AddDate(0, 1, 0), "team-method")
	editor := "control-editor"
	if err = s.UpsertUser(ctx, User{ID: editor, DisplayName: editor}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddWorkspaceMember(ctx, owner, WorkspaceMember{WorkspaceID: ws.ID, UserID: editor, Role: WorkspaceRoleEditor}); err != nil {
		t.Fatal(err)
	}
	connection := connectDirectTestAccount(t, ctx, s, owner, ws.ID)
	seedExternalEditTestSnapshot(t, s, owner, ws.ID, connection.ID, now)
	control, err := s.ObserveDirectExternalEdit(ctx, owner, ws.ID, connection.ID, 77, strings.Repeat("a", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	fence := DirectExternalEditFence{ConnectionID: connection.ID, Version: control.Version, ObservedHash: control.ObservedHash, RevisionID: control.RevisionID}
	if _, _, err = s.ClaimDirectExternalOperation(ctx, editor, ws.ID, 77, fence, "11111111-1111-4111-8111-111111111111", "budget", strings.Repeat("b", 64), strings.Repeat("c", 64), json.RawMessage(`{}`), now); !errors.Is(err, ErrNotFound) {
		t.Fatal("editor changed real provider budget", err)
	}
	o, isNew, err := s.ClaimDirectExternalOperation(ctx, editor, ws.ID, 77, fence, "22222222-2222-4222-8222-222222222222", "group", strings.Repeat("b", 64), strings.Repeat("c", 64), json.RawMessage(`{}`), now)
	if err != nil || !isNew {
		t.Fatal(err)
	}
	if err = s.RemoveWorkspaceMember(ctx, owner, ws.ID, editor); err != nil {
		t.Fatal(err)
	}
	if err = s.BeginDirectExternalOperation(ctx, editor, ws.ID, connection.ID, 77, o.OperationID, "group", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("revoked editor wrote Direct", err)
	}
}
