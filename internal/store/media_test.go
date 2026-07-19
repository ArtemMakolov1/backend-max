package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestReserveMediaQuotaIsAtomic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "media-quota.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	const userID = "media-quota-owner"
	if err := storage.UpsertUser(ctx, User{ID: userID, DisplayName: "Media quota owner"}); err != nil {
		t.Fatal(err)
	}

	const attempts = 12
	start := make(chan struct{})
	results := make(chan struct {
		reservation MediaReservation
		err         error
	}, attempts)
	var workers sync.WaitGroup
	for index := 0; index < attempts; index++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			<-start
			reservation, err := storage.ReserveMedia(ctx, userID, fmt.Sprintf("asset-%02d.png", index), 10,
				MediaLimits{MaxFiles: 1, MaxBytes: 100}, time.Now().UTC())
			results <- struct {
				reservation MediaReservation
				err         error
			}{reservation: reservation, err: err}
		}(index)
	}
	close(start)
	workers.Wait()
	close(results)

	var winner MediaReservation
	successes := 0
	quotaErrors := 0
	for result := range results {
		switch {
		case result.err == nil:
			successes++
			winner = result.reservation
		case errors.Is(result.err, ErrMediaQuotaExceeded):
			quotaErrors++
		default:
			t.Fatalf("unexpected reservation error: %v", result.err)
		}
	}
	if successes != 1 || quotaErrors != attempts-1 {
		t.Fatalf("successes=%d quota_errors=%d, want 1 and %d", successes, quotaErrors, attempts-1)
	}
	if err := storage.CompleteMediaReservation(ctx, winner, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	var usedFiles, usedBytes int64
	if err := storage.db.QueryRowContext(ctx, `SELECT asset_count, total_bytes FROM media_usage WHERE owner_id=$1`, userID).
		Scan(&usedFiles, &usedBytes); err != nil {
		t.Fatal(err)
	}
	if usedFiles != 1 || usedBytes != 10 {
		t.Fatalf("usage=(%d files, %d bytes), want (1, 10)", usedFiles, usedBytes)
	}

	existing, err := storage.ReserveMedia(ctx, userID, winner.Filename, 10,
		MediaLimits{MaxFiles: 1, MaxBytes: 100}, time.Now().UTC())
	if err != nil {
		t.Fatalf("reserve existing media: %v", err)
	}
	if !existing.Existing {
		t.Fatalf("existing reservation = %#v, want Existing", existing)
	}
	if err := storage.db.QueryRowContext(ctx, `SELECT asset_count, total_bytes FROM media_usage WHERE owner_id=$1`, userID).
		Scan(&usedFiles, &usedBytes); err != nil {
		t.Fatal(err)
	}
	if usedFiles != 1 || usedBytes != 10 {
		t.Fatalf("usage after duplicate=(%d files, %d bytes), want (1, 10)", usedFiles, usedBytes)
	}

	workspaceFiles, workspaceBytes := personalWorkspaceMediaUsage(t, storage, userID)
	if workspaceFiles != 1 || workspaceBytes != 10 {
		t.Fatalf("personal workspace usage=(%d files, %d bytes), want (1, 10)", workspaceFiles, workspaceBytes)
	}
}

// personalWorkspaceMediaUsage reads the workspace_media_usage ledger of the
// owner's personal workspace. Reservation must keep it in sync with
// media_usage because GC and release drain both ledgers.
func personalWorkspaceMediaUsage(t *testing.T, storage *Store, ownerID string) (int64, int64) {
	t.Helper()
	var files, bytes int64
	if err := storage.db.QueryRowContext(context.Background(), `SELECT wmu.asset_count, wmu.total_bytes
FROM workspace_media_usage wmu
JOIN workspaces w ON w.id=wmu.workspace_id
WHERE w.owner_user_id=$1 AND w.is_personal`, ownerID).Scan(&files, &bytes); err != nil {
		t.Fatal(err)
	}
	return files, bytes
}

func TestCleanupOrphanMediaIsTenantSafe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "media-cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	const (
		ownerA       = "media-owner-a"
		ownerB       = "media-owner-b"
		shared       = "shared-object.png"
		orphan       = "orphan-object.png"
		stalePending = "stale-pending.png"
	)
	for _, userID := range []string{ownerA, ownerB} {
		if err := storage.UpsertUser(ctx, User{ID: userID, DisplayName: userID}); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	limits := MediaLimits{MaxFiles: 10, MaxBytes: 1 << 20}
	reserveReady := func(userID, filename string, size int64) {
		t.Helper()
		reservation, err := storage.ReserveMedia(ctx, userID, filename, size, limits, old)
		if err != nil {
			t.Fatal(err)
		}
		if err := storage.CompleteMediaReservation(ctx, reservation, old); err != nil {
			t.Fatal(err)
		}
	}
	reserveReady(ownerA, shared, 100)
	reserveReady(ownerB, shared, 100)
	reserveReady(ownerB, orphan, 50)
	if _, err := storage.ReserveMedia(ctx, ownerB, stalePending, 25, limits, old); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreatePost(ctx, Post{
		UserID: ownerA, Title: "Uses shared media", Content: "body", ImagePath: shared,
	}); err != nil {
		t.Fatal(err)
	}

	var deleted []string
	cleanup, err := storage.CleanupOrphanMedia(ctx, time.Now().UTC().Add(-24*time.Hour), 20,
		func(_ context.Context, filename string) error {
			deleted = append(deleted, filename)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(deleted)
	wantDeleted := []string{orphan, stalePending}
	sort.Strings(wantDeleted)
	if fmt.Sprint(deleted) != fmt.Sprint(wantDeleted) {
		t.Fatalf("deleted objects=%v, want %v", deleted, wantDeleted)
	}
	if cleanup.AssetsRemoved != 3 || cleanup.ObjectsDeleted != 2 || cleanup.BytesReleased != 175 {
		t.Fatalf("cleanup result=%#v, want 3 assets, 2 objects, 175 bytes", cleanup)
	}

	owned, err := storage.UserOwnsMedia(ctx, ownerA, shared)
	if err != nil {
		t.Fatal(err)
	}
	if !owned {
		t.Fatal("referenced ownership for tenant A was removed")
	}
	owned, err = storage.UserOwnsMedia(ctx, ownerB, shared)
	if err != nil {
		t.Fatal(err)
	}
	if owned {
		t.Fatal("unreferenced ownership for tenant B was retained")
	}

	var filesA, bytesA, filesB, bytesB int64
	if err := storage.db.QueryRowContext(ctx, `SELECT asset_count, total_bytes FROM media_usage WHERE owner_id=$1`, ownerA).
		Scan(&filesA, &bytesA); err != nil {
		t.Fatal(err)
	}
	if err := storage.db.QueryRowContext(ctx, `SELECT asset_count, total_bytes FROM media_usage WHERE owner_id=$1`, ownerB).
		Scan(&filesB, &bytesB); err != nil {
		t.Fatal(err)
	}
	if filesA != 1 || bytesA != 100 || filesB != 0 || bytesB != 0 {
		t.Fatalf("usage A=(%d,%d) B=(%d,%d), want A=(1,100) B=(0,0)", filesA, bytesA, filesB, bytesB)
	}

	// GC drains both ledgers for personal workspaces, so the workspace ledger
	// must track exactly the same bytes as media_usage after cleanup.
	wsFilesA, wsBytesA := personalWorkspaceMediaUsage(t, storage, ownerA)
	wsFilesB, wsBytesB := personalWorkspaceMediaUsage(t, storage, ownerB)
	if wsFilesA != 1 || wsBytesA != 100 || wsFilesB != 0 || wsBytesB != 0 {
		t.Fatalf("workspace usage A=(%d,%d) B=(%d,%d), want A=(1,100) B=(0,0)",
			wsFilesA, wsBytesA, wsFilesB, wsBytesB)
	}
}

func TestCleanupOrphanWorkspaceMediaAfterArchive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "workspace-media-archive-cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	const (
		owner    = "workspace-media-archive-owner"
		filename = "archived-workspace-orphan.png"
	)
	if err := storage.UpsertUser(ctx, User{ID: owner, DisplayName: owner}); err != nil {
		t.Fatal(err)
	}
	workspace, err := storage.CreateWorkspace(ctx, owner, Workspace{Name: "Archived media cleanup"})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	reservation, err := storage.ReserveMediaForWorkspace(ctx, owner, workspace.ID, filename, 64,
		MediaLimits{MaxFiles: 10, MaxBytes: 1 << 20}, old)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.CompleteMediaReservation(ctx, reservation, old); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteWorkspace(ctx, owner, workspace.ID); err != nil {
		t.Fatal(err)
	}

	var deleted []string
	cleanup, err := storage.CleanupOrphanMedia(ctx, time.Now().UTC().Add(-24*time.Hour), 10,
		func(_ context.Context, got string) error {
			deleted = append(deleted, got)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(deleted) != fmt.Sprint([]string{filename}) {
		t.Fatalf("deleted objects=%v, want [%s]", deleted, filename)
	}
	if cleanup.AssetsRemoved != 1 || cleanup.ObjectsDeleted != 1 || cleanup.BytesReleased != 64 {
		t.Fatalf("cleanup result=%#v, want 1 asset, 1 object, 64 bytes", cleanup)
	}
	usage, err := storage.GetWorkspaceMediaUsage(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if usage.AssetCount != 0 || usage.TotalBytes != 0 {
		t.Fatalf("workspace usage=(%d files, %d bytes), want zero", usage.AssetCount, usage.TotalBytes)
	}
}

func TestCleanupOrphanMediaCommitsOwnershipBeforeObjectDeletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "media-cleanup-commit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	const (
		owner    = "media-cleanup-commit-owner"
		filename = "committed-before-delete.png"
	)
	if err := storage.UpsertUser(ctx, User{ID: owner, DisplayName: owner}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	reservation, err := storage.ReserveMedia(ctx, owner, filename, 42,
		MediaLimits{MaxFiles: 10, MaxBytes: 1 << 20}, old)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.CompleteMediaReservation(ctx, reservation, old); err != nil {
		t.Fatal(err)
	}

	deleteCalls := 0
	result, err := storage.CleanupOrphanMedia(ctx, time.Now().UTC().Add(-24*time.Hour), 10,
		func(callbackCtx context.Context, callbackFilename string) error {
			deleteCalls++
			if callbackFilename != filename {
				t.Fatalf("delete filename = %q, want %q", callbackFilename, filename)
			}
			var assets int64
			if err := storage.db.QueryRowContext(callbackCtx,
				`SELECT COUNT(*) FROM media_assets WHERE owner_id=$1 AND filename=$2`, owner, filename).Scan(&assets); err != nil {
				return err
			}
			if assets != 0 {
				return fmt.Errorf("object deletion observed %d uncommitted ownership rows", assets)
			}
			var files, bytes int64
			if err := storage.db.QueryRowContext(callbackCtx,
				`SELECT asset_count, total_bytes FROM media_usage WHERE owner_id=$1`, owner).Scan(&files, &bytes); err != nil {
				return err
			}
			if files != 0 || bytes != 0 {
				return fmt.Errorf("object deletion observed uncommitted usage (%d,%d)", files, bytes)
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if deleteCalls != 1 || result.AssetsRemoved != 1 || result.ObjectsDeleted != 1 || result.BytesReleased != 42 {
		t.Fatalf("delete calls=%d cleanup=%#v", deleteCalls, result)
	}
	var queued int64
	if err := storage.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_gc_queue WHERE filename=$1`, filename).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Fatalf("garbage collection queue still has %d rows", queued)
	}
}

func TestCleanupOrphanMediaRetriesFailedObjectDeletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "media-cleanup-retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	const (
		owner    = "media-cleanup-retry-owner"
		filename = "retry-object-delete.png"
	)
	if err := storage.UpsertUser(ctx, User{ID: owner, DisplayName: owner}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	reservation, err := storage.ReserveMedia(ctx, owner, filename, 17,
		MediaLimits{MaxFiles: 10, MaxBytes: 1 << 20}, old)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.CompleteMediaReservation(ctx, reservation, old); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC().Add(-24 * time.Hour)
	deleteErr := errors.New("temporary s3 delete failure")
	if _, err := storage.CleanupOrphanMedia(ctx, before, 10,
		func(context.Context, string) error { return deleteErr }); !errors.Is(err, deleteErr) {
		t.Fatalf("cleanup error = %v, want %v", err, deleteErr)
	}

	var assets, queued int64
	if err := storage.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_assets WHERE filename=$1`, filename).Scan(&assets); err != nil {
		t.Fatal(err)
	}
	if err := storage.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_gc_queue WHERE filename=$1`, filename).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if assets != 0 || queued != 1 {
		t.Fatalf("after failed object delete assets=%d queued=%d, want 0 and 1", assets, queued)
	}

	retryCalls := 0
	result, err := storage.CleanupOrphanMedia(ctx, before, 10, func(_ context.Context, got string) error {
		retryCalls++
		if got != filename {
			t.Fatalf("retry filename = %q, want %q", got, filename)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if retryCalls != 1 || result.ObjectsDeleted != 1 {
		t.Fatalf("retry calls=%d cleanup=%#v", retryCalls, result)
	}
}

func TestMediaLedgersStaySymmetricAcrossReserveAndRelease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "media-ledger-symmetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	const owner = "media-ledger-owner"
	if err := storage.UpsertUser(ctx, User{ID: owner, DisplayName: owner}); err != nil {
		t.Fatal(err)
	}
	limits := MediaLimits{MaxFiles: 10, MaxBytes: 1 << 20}
	now := time.Now().UTC()
	assertPersonalLedgers := func(step string, files, bytes int64) {
		t.Helper()
		var ownerFiles, ownerBytes int64
		if err := storage.db.QueryRowContext(ctx,
			`SELECT COALESCE((SELECT asset_count FROM media_usage WHERE owner_id=$1),0),
COALESCE((SELECT total_bytes FROM media_usage WHERE owner_id=$1),0)`, owner).
			Scan(&ownerFiles, &ownerBytes); err != nil {
			t.Fatal(err)
		}
		workspaceFiles, workspaceBytes := personalWorkspaceMediaUsage(t, storage, owner)
		if ownerFiles != files || ownerBytes != bytes || workspaceFiles != files || workspaceBytes != bytes {
			t.Fatalf("%s: media_usage=(%d,%d) workspace=(%d,%d), want both (%d,%d)",
				step, ownerFiles, ownerBytes, workspaceFiles, workspaceBytes, files, bytes)
		}
	}

	reservation, err := storage.ReserveMedia(ctx, owner, "personal-path.png", 30, limits, now)
	if err != nil {
		t.Fatal(err)
	}
	assertPersonalLedgers("after personal reserve", 1, 30)
	if err := storage.ReleaseMediaReservation(ctx, reservation, now); err != nil {
		t.Fatal(err)
	}
	assertPersonalLedgers("after personal release", 0, 0)

	var personalWorkspaceID string
	workspaces, err := storage.ListWorkspaces(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	for _, access := range workspaces {
		if access.Workspace.IsPersonal {
			personalWorkspaceID = access.Workspace.ID
		}
	}
	if personalWorkspaceID == "" {
		t.Fatal("personal workspace is missing")
	}
	reservation, err = storage.ReserveMediaForWorkspace(ctx, owner, personalWorkspaceID,
		"workspace-path.png", 50, limits, now)
	if err != nil {
		t.Fatal(err)
	}
	assertPersonalLedgers("after personal workspace reserve", 1, 50)
	if err := storage.ReleaseMediaReservation(ctx, reservation, now); err != nil {
		t.Fatal(err)
	}
	assertPersonalLedgers("after personal workspace release", 0, 0)

	team, err := storage.CreateWorkspace(ctx, owner, Workspace{Name: "Ledger symmetry team"})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err = storage.ReserveMediaForWorkspace(ctx, owner, team.ID, "team-path.png", 70, limits, now)
	if err != nil {
		t.Fatal(err)
	}
	assertPersonalLedgers("after team reserve", 0, 0)
	teamUsage, err := storage.GetWorkspaceMediaUsage(ctx, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if teamUsage.AssetCount != 1 || teamUsage.TotalBytes != 70 {
		t.Fatalf("team usage=(%d,%d), want (1,70)", teamUsage.AssetCount, teamUsage.TotalBytes)
	}
	if err := storage.ReleaseMediaReservation(ctx, reservation, now); err != nil {
		t.Fatal(err)
	}
	teamUsage, err = storage.GetWorkspaceMediaUsage(ctx, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if teamUsage.AssetCount != 0 || teamUsage.TotalBytes != 0 {
		t.Fatalf("team usage after release=(%d,%d), want (0,0)", teamUsage.AssetCount, teamUsage.TotalBytes)
	}
	var compatRows int64
	if err := storage.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM media_usage WHERE owner_id=$1`, team.CompatOwnerUserID).Scan(&compatRows); err != nil {
		t.Fatal(err)
	}
	if compatRows != 0 {
		t.Fatalf("team reservation touched the personal media_usage ledger (%d rows)", compatRows)
	}
}
