package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMAXAuthDeviceFlowIsBrowserBoundReplaySafeAndReusesLinkedTenant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "max-auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	now := time.Date(2026, time.July, 15, 9, 0, 0, 0, time.UTC)
	if err := storage.UpsertUser(ctx, User{ID: "existing-owner", Login: "yandex-user", DisplayName: "Yandex User"}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.ExecContext(ctx, `INSERT INTO max_identity_links(owner_id,max_user_id,linked_at,updated_at)
VALUES ('existing-owner','777',?,?)`, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	attempt := MAXAuthAttempt{
		ID: "request-1", BrowserTokenHash: strings.Repeat("a", 64), DeepTokenHash: strings.Repeat("b", 64),
		ReturnTo: "/app/#/posts", ComparisonCode: "314159", TermsVersion: "terms-v1", PersonalDataVersion: "privacy-v1",
		ConsentAt: now, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute), UpdatedAt: now,
	}
	if err := storage.CreateMAXAuthAttempt(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetMAXAuthAttemptForBrowser(ctx, attempt.ID, strings.Repeat("c", 64), now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong browser token error = %v", err)
	}
	started, first, err := storage.StartMAXAuthContact(ctx, attempt.DeepTokenHash, "777", now.Add(time.Second), now.Add(time.Second))
	if err != nil || !first || started.Status != MAXAuthAttemptAwaitingContact {
		t.Fatalf("start = %#v first=%v err=%v", started, first, err)
	}
	if _, first, err := storage.StartMAXAuthContact(ctx, attempt.DeepTokenHash, "777", now.Add(time.Second), now.Add(2*time.Second)); err != nil || first {
		t.Fatalf("bot_started replay first=%v err=%v", first, err)
	}
	profile := MAXAuthProfile{MAXUserID: "777", FirstName: "Артем", LastName: "Маколов", Username: "makolov99"}
	recorded, first, err := storage.RecordMAXAuthContact(ctx, "777", "mid-1", now.Add(2*time.Second), now.Add(2*time.Second), profile)
	if err != nil || !first || recorded.Status != MAXAuthAttemptAwaitingContact || recorded.ContactMessageID != "mid-1" {
		t.Fatalf("record contact = %#v first=%v err=%v", recorded, first, err)
	}
	if _, first, err := storage.RecordMAXAuthContact(ctx, "777", "mid-1", now.Add(2*time.Second), now.Add(3*time.Second), profile); err != nil || first {
		t.Fatalf("contact replay first=%v err=%v", first, err)
	}
	verified, first, err := storage.ConfirmMAXAuthContact(ctx, attempt.DeepTokenHash, "777", now.Add(3*time.Second), now.Add(3*time.Second))
	if err != nil || !first || verified.Status != MAXAuthAttemptVerified {
		t.Fatalf("confirm contact = %#v first=%v err=%v", verified, first, err)
	}
	if replayed, first, err := storage.ConfirmMAXAuthContact(ctx, attempt.DeepTokenHash, "777", now.Add(3*time.Second), now.Add(3*time.Second)); err != nil || first || replayed.Status != MAXAuthAttemptVerified {
		t.Fatalf("confirmation replay = %#v first=%v err=%v", replayed, first, err)
	}
	session, err := storage.CompleteMAXAuthAttempt(ctx, attempt.ID, attempt.BrowserTokenHash, "max-new-owner", AuthSession{
		TokenHash: strings.Repeat("d", 64), CreatedAt: now.Add(3 * time.Second), ExpiresAt: now.Add(time.Hour),
	}, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if session.OwnerID != "existing-owner" || session.Provider != "max" || session.Login != "makolov99" {
		t.Fatalf("MAX session = %#v", session)
	}
	stored, err := storage.GetAuthSession(ctx, session.TokenHash, now.Add(4*time.Second))
	if err != nil || stored.OwnerID != "existing-owner" || stored.Provider != "max" {
		t.Fatalf("stored session = %#v err=%v", stored, err)
	}
	if _, err := storage.CompleteMAXAuthAttempt(ctx, attempt.ID, attempt.BrowserTokenHash, "another-owner", AuthSession{
		TokenHash: strings.Repeat("e", 64), CreatedAt: now.Add(4 * time.Second), ExpiresAt: now.Add(time.Hour),
	}, now.Add(4*time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("complete replay error = %v", err)
	}
	var attemptCount, identityCount int
	if err := storage.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM max_auth_attempts WHERE id=?`, attempt.ID).Scan(&attemptCount); err != nil {
		t.Fatal(err)
	}
	if err := storage.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_identities
WHERE provider='max' AND subject='777' AND owner_id='existing-owner'`).Scan(&identityCount); err != nil {
		t.Fatal(err)
	}
	if attemptCount != 0 || identityCount != 1 {
		t.Fatalf("retention/mapping counts attempt=%d identity=%d", attemptCount, identityCount)
	}
	var profileColumns string
	if err := storage.db.QueryRowContext(ctx, `SELECT string_agg(column_name, ',') FROM information_schema.columns
WHERE table_schema=current_schema() AND table_name='max_auth_profiles'`).Scan(&profileColumns); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"phone", "vcf", "hash", "message"} {
		if strings.Contains(profileColumns, forbidden) {
			t.Fatalf("max_auth_profiles unexpectedly persists %s: %s", forbidden, profileColumns)
		}
	}
}

func TestMAXAuthOldAttemptContactAndCallbackCannotVerifyNewAttempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "max-auth-attempt-binding.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	now := time.Date(2026, time.July, 15, 9, 30, 0, 0, time.UTC)
	newAttempt := func(id, browserHash, deepHash, code string) MAXAuthAttempt {
		return MAXAuthAttempt{
			ID: id, BrowserTokenHash: browserHash, DeepTokenHash: deepHash, ReturnTo: "/app/",
			ComparisonCode: code, TermsVersion: "terms", PersonalDataVersion: "privacy",
			ConsentAt: now, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute), UpdatedAt: now,
		}
	}
	attemptA := newAttempt("request-a", strings.Repeat("a", 64), strings.Repeat("b", 64), "111111")
	attemptB := newAttempt("request-b", strings.Repeat("c", 64), strings.Repeat("d", 64), "222222")
	if err := storage.CreateMAXAuthAttempt(ctx, attemptA); err != nil {
		t.Fatal(err)
	}
	if err := storage.CreateMAXAuthAttempt(ctx, attemptB); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.StartMAXAuthContact(ctx, attemptA.DeepTokenHash, "777", now.Add(time.Second), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.StartMAXAuthContact(ctx, attemptB.DeepTokenHash, "777", now.Add(2*time.Second), now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	// Pressing the new attempt's callback too early is safe and retryable.
	early, transitioned, err := storage.ConfirmMAXAuthContact(ctx, attemptB.DeepTokenHash, "777", now.Add(3*time.Second), now.Add(3*time.Second))
	if err != nil || transitioned || early.Status != MAXAuthAttemptAwaitingContact || early.ContactMessageID != "" {
		t.Fatalf("early confirmation = %#v transitioned=%v err=%v", early, transitioned, err)
	}
	// request_contact has no attempt payload in MAX. Even if a delayed contact
	// from message A is delivered while B is current, it remains only the first
	// proof and cannot complete B by itself.
	if recorded, first, err := storage.RecordMAXAuthContact(ctx, "777", "mid-from-old-message-a",
		now.Add(4*time.Second), now.Add(4*time.Second), MAXAuthProfile{MAXUserID: "777", FirstName: "User"}); err != nil || !first || recorded.Status != MAXAuthAttemptAwaitingContact || recorded.ID != attemptB.ID {
		t.Fatalf("record delayed contact = %#v first=%v err=%v", recorded, first, err)
	}
	if _, _, err := storage.ConfirmMAXAuthContact(ctx, attemptA.DeepTokenHash, "777", now.Add(5*time.Second), now.Add(5*time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old attempt callback error = %v, want not found", err)
	}
	stillAwaiting, err := storage.GetMAXAuthAttemptForBrowser(ctx, attemptB.ID, attemptB.BrowserTokenHash, now.Add(5*time.Second))
	if err != nil || stillAwaiting.Status != MAXAuthAttemptAwaitingContact {
		t.Fatalf("new attempt after old callback = %#v err=%v", stillAwaiting, err)
	}
	verified, transitioned, err := storage.ConfirmMAXAuthContact(ctx, attemptB.DeepTokenHash, "777", now.Add(6*time.Second), now.Add(6*time.Second))
	if err != nil || !transitioned || verified.Status != MAXAuthAttemptVerified {
		t.Fatalf("new attempt confirmation = %#v transitioned=%v err=%v", verified, transitioned, err)
	}
}

func TestMAXAuthRejectsEventsOutsideAttemptTTLAndPurgesExpiredAttempts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "max-auth-expiry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	now := time.Date(2026, time.July, 15, 9, 0, 0, 0, time.UTC)
	attempt := MAXAuthAttempt{
		ID: "request-expiry", BrowserTokenHash: strings.Repeat("1", 64), DeepTokenHash: strings.Repeat("2", 64),
		ReturnTo: "/app/", ComparisonCode: "271828", TermsVersion: "terms", PersonalDataVersion: "privacy",
		ConsentAt: now, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute), UpdatedAt: now,
	}
	if err := storage.CreateMAXAuthAttempt(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.StartMAXAuthContact(ctx, attempt.DeepTokenHash, "888", now.Add(-time.Second), now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pre-attempt bot event error = %v", err)
	}
	if _, _, err := storage.StartMAXAuthContact(ctx, attempt.DeepTokenHash, "888", now.Add(time.Second), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	profile := MAXAuthProfile{MAXUserID: "888", FirstName: "User"}
	if _, _, err := storage.RecordMAXAuthContact(ctx, "888", "mid-before-attempt", now.Add(-time.Second), now.Add(2*time.Second), profile); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pre-attempt contact error = %v", err)
	}
	if _, _, err := storage.RecordMAXAuthContact(ctx, "888", "mid-expired", attempt.ExpiresAt, attempt.ExpiresAt.Add(-time.Second), profile); !errors.Is(err, ErrNotFound) {
		t.Fatalf("contact at expiry error = %v", err)
	}
	abandoned := MAXAuthAttempt{
		ID: "request-abandoned", BrowserTokenHash: strings.Repeat("3", 64), DeepTokenHash: strings.Repeat("4", 64),
		ReturnTo: "/app/", ComparisonCode: "161803", TermsVersion: "terms", PersonalDataVersion: "privacy",
		ConsentAt: now, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute), UpdatedAt: now,
	}
	if err := storage.CreateMAXAuthAttempt(ctx, abandoned); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.StartMAXAuthContact(ctx, abandoned.DeepTokenHash, "999", now.Add(time.Second), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, first, err := storage.RecordMAXAuthContact(ctx, "999", "mid-abandoned", now.Add(2*time.Second), now.Add(2*time.Second),
		MAXAuthProfile{MAXUserID: "999", FirstName: "Abandoned"}); err != nil || !first {
		t.Fatalf("record abandoned profile first=%v err=%v", first, err)
	}
	if err := storage.PurgeExpiredMAXAuthAttempts(ctx, attempt.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.GetMAXAuthAttemptForBrowser(ctx, attempt.ID, attempt.BrowserTokenHash, attempt.ExpiresAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("purged attempt error = %v", err)
	}
	var orphanProfiles int
	if err := storage.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM max_auth_profiles WHERE max_user_id='999'`).Scan(&orphanProfiles); err != nil {
		t.Fatal(err)
	}
	if orphanProfiles != 0 {
		t.Fatalf("orphan verified MAX profiles = %d, want 0", orphanProfiles)
	}
}

func TestMAXAuthCompleteIsAtomicUnderConcurrency(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "max-auth-concurrent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	now := time.Date(2026, time.July, 15, 10, 0, 0, 0, time.UTC)
	attempt := MAXAuthAttempt{
		ID: "request-concurrent", BrowserTokenHash: strings.Repeat("5", 64), DeepTokenHash: strings.Repeat("6", 64),
		ReturnTo: "/app/", ComparisonCode: "112358", TermsVersion: "terms", PersonalDataVersion: "privacy",
		ConsentAt: now, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute), UpdatedAt: now,
	}
	if err := storage.CreateMAXAuthAttempt(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.StartMAXAuthContact(ctx, attempt.DeepTokenHash, "12345", now.Add(time.Second), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.RecordMAXAuthContact(ctx, "12345", "mid-concurrent", now.Add(2*time.Second), now.Add(2*time.Second),
		MAXAuthProfile{MAXUserID: "12345", FirstName: "Concurrent"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.ConfirmMAXAuthContact(ctx, attempt.DeepTokenHash, "12345", now.Add(3*time.Second), now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	const workers = 8
	var successes atomic.Int32
	var unexpected atomic.Int32
	var wg sync.WaitGroup
	for index := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, completeErr := storage.CompleteMAXAuthAttempt(ctx, attempt.ID, attempt.BrowserTokenHash,
				fmt.Sprintf("max-owner-%d", index), AuthSession{
					TokenHash: fmt.Sprintf("%064x", index+100), CreatedAt: now.Add(3 * time.Second), ExpiresAt: now.Add(time.Hour),
				}, now.Add(3*time.Second))
			if completeErr == nil {
				successes.Add(1)
			} else if !errors.Is(completeErr, ErrNotFound) {
				unexpected.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 || unexpected.Load() != 0 {
		t.Fatalf("concurrent completion successes=%d unexpected_errors=%d", successes.Load(), unexpected.Load())
	}
	var sessions, identities int
	if err := storage.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_sessions WHERE provider='max'`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := storage.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_identities WHERE provider='max' AND subject='12345'`).Scan(&identities); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 || identities != 1 {
		t.Fatalf("sessions=%d identities=%d, want 1/1", sessions, identities)
	}
}
