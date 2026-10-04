package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestContentAnalysisCacheFencesTenantLeaseMembershipAndSnapshot(t *testing.T) {
	t.Parallel()
	s, workspace, channel := openMAXHistoryStoreTest(t, "semantic-cache", 4)
	ctx := t.Context()
	now := time.Now().UTC()
	key := strings.Repeat("a", 64)
	result, claim, err := s.ClaimContentAnalysis(ctx, "history-editor", workspace.ID, channel.ID, key, now)
	if err != nil || result != "" || claim == "" {
		t.Fatalf("claim failed: %q %q %v", result, claim, err)
	}
	if result, second, err := s.ClaimContentAnalysis(ctx, "test-owner", workspace.ID, channel.ID, key, now); err != nil || result != "" || second != "" {
		t.Fatalf("duplicate paid fill allowed: %q %q %v", result, second, err)
	}
	if _, _, err := s.ClaimContentAnalysis(ctx, "foreign-user", workspace.ID, channel.ID, key, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign claim: %v", err)
	}
	foreign, err := s.CreateWorkspace(ctx, "test-owner", Workspace{Name: "Foreign cache"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClaimContentAnalysis(ctx, "test-owner", foreign.ID, channel.ID, key, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace channel: %v", err)
	}
	raw := `{"status":"ready","images_analyzed":1,"videos_analyzed":0,"audio_analyzed":0,"cached":false,"summary":"Кошки и мягкий юмор","warnings":[]}`
	if err := s.CompleteContentAnalysis(ctx, "test-owner", workspace.ID, channel.ID, key, strings.Repeat("b", 32), raw, now.Add(time.Hour)); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong lease completed: %v", err)
	}
	if err := s.CompleteContentAnalysis(ctx, "history-editor", workspace.ID, channel.ID, key, claim, raw, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if cached, newClaim, err := s.ClaimContentAnalysis(ctx, "test-owner", workspace.ID, channel.ID, key, now); err != nil || cached != raw || newClaim != "" {
		t.Fatalf("cache missed: %q %q %v", cached, newClaim, err)
	}
	newKey := strings.Repeat("c", 64)
	_, newClaim, err := s.ClaimContentAnalysis(ctx, "history-editor", workspace.ID, channel.ID, newKey, now)
	if err != nil || newClaim == "" {
		t.Fatalf("changed snapshot retained old result: %q %v", newClaim, err)
	}
	if err := s.CompleteContentAnalysis(ctx, "test-owner", workspace.ID, channel.ID, key, claim, raw, now.Add(time.Hour)); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale snapshot completed: %v", err)
	}
	if err := s.RemoveWorkspaceMember(ctx, "test-owner", workspace.ID, "history-editor"); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteContentAnalysis(ctx, "history-editor", workspace.ID, channel.ID, newKey, newClaim, raw, now.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked actor finalized cache: %v", err)
	}
	if _, _, err := s.ClaimContentAnalysis(ctx, "history-editor", workspace.ID, channel.ID, key, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked actor read cache: %v", err)
	}
}

func TestContentAnalysisSnapshotChangesWhenPrivateSourceOrModelChanges(t *testing.T) {
	posts := []Post{{ID: 1, Content: "Caption", Attachments: []PostAttachment{{ID: 2, Type: PostAttachmentVideo, ProviderToken: "private-one", RemoteURL: "https://cdn.example.com/one"}}}}
	first := ContentAnalysisSnapshotKey(posts, "luna|audio|v1")
	posts[0].Attachments[0].ProviderToken = "private-two"
	second := ContentAnalysisSnapshotKey(posts, "luna|audio|v1")
	if first == second || second == ContentAnalysisSnapshotKey(posts, "luna|audio|v2") || len(first) != 64 {
		t.Fatal("media/model changes failed to invalidate semantic cache")
	}
}
