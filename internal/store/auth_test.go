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

func TestAuthSessionLifecycleAndExpiry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "auth-session.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	createdAt := time.Date(2026, time.July, 14, 8, 30, 0, 123000, time.FixedZone("test", 3*60*60))
	expiresAt := createdAt.Add(24 * time.Hour)
	tokenHash := strings.Repeat("a", 64)
	want := AuthSession{
		TokenHash: tokenHash, YandexUserID: "123456789", Login: "writer",
		Email: "writer@example.test", DisplayName: "Тестовый автор", AllowlistIdentity: "123456789",
		CreatedAt: createdAt, ExpiresAt: expiresAt,
	}
	if err := storage.CreateAuthSession(ctx, want); err != nil {
		t.Fatal(err)
	}

	got, err := storage.GetAuthSession(ctx, tokenHash, expiresAt.Add(-time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	if got.TokenHash != want.TokenHash || got.YandexUserID != want.YandexUserID || got.Login != want.Login ||
		got.Email != want.Email || got.DisplayName != want.DisplayName || got.AllowlistIdentity != want.AllowlistIdentity {
		t.Fatalf("GetAuthSession() = %#v, want %#v", got, want)
	}
	if !got.CreatedAt.Equal(createdAt) || !got.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("stored times = (%s, %s), want (%s, %s)", got.CreatedAt, got.ExpiresAt, createdAt, expiresAt)
	}
	if got.CreatedAt.Location() != time.UTC || got.ExpiresAt.Location() != time.UTC {
		t.Fatalf("stored times must be normalized to UTC: %#v", got)
	}

	if _, err := storage.GetAuthSession(ctx, tokenHash, expiresAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetAuthSession(at expiry) error = %v, want ErrNotFound", err)
	}
	var count int
	if err := storage.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM auth_sessions WHERE token_hash = ?`, tokenHash).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expired session row count = %d, want 0", count)
	}

	secondHash := strings.Repeat("b", 64)
	want.TokenHash = secondHash
	if err := storage.CreateAuthSession(ctx, want); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteAuthSession(ctx, secondHash); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteAuthSession(ctx, secondHash); err != nil {
		t.Fatalf("repeated DeleteAuthSession() error = %v, want nil", err)
	}
	if _, err := storage.GetAuthSession(ctx, secondHash, createdAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetAuthSession(after delete) error = %v, want ErrNotFound", err)
	}
}

func TestOAuthStateConsumeIsOneTimeAndAtomic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "oauth-state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	now := time.Date(2026, time.July, 14, 9, 0, 0, 0, time.UTC)
	stateHash := strings.Repeat("c", 64)
	want := OAuthState{
		StateHash: stateHash, PKCEVerifier: "pkce-verifier-secret", ReturnTo: "/app/#/calendar",
		CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := storage.CreateOAuthState(ctx, want); err != nil {
		t.Fatal(err)
	}

	const consumers = 8
	var successes atomic.Int32
	errCh := make(chan error, consumers)
	var wg sync.WaitGroup
	for range consumers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, consumeErr := storage.ConsumeOAuthState(ctx, stateHash, now.Add(time.Minute))
			switch {
			case consumeErr == nil:
				successes.Add(1)
				if got.StateHash != want.StateHash || got.PKCEVerifier != want.PKCEVerifier ||
					got.ReturnTo != want.ReturnTo || !got.CreatedAt.Equal(want.CreatedAt) || !got.ExpiresAt.Equal(want.ExpiresAt) {
					errCh <- errors.New("consumed OAuth state did not match the created state")
				}
			case !errors.Is(consumeErr, ErrNotFound):
				errCh <- consumeErr
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for consumeErr := range errCh {
		t.Errorf("ConsumeOAuthState() error = %v", consumeErr)
	}
	if successes.Load() != 1 {
		t.Fatalf("successful consumes = %d, want exactly 1", successes.Load())
	}
	if _, err := storage.ConsumeOAuthState(ctx, stateHash, now.Add(time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replay ConsumeOAuthState() error = %v, want ErrNotFound", err)
	}
}

func TestExpiredOAuthStateIsDeletedAndRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "expired-oauth-state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	now := time.Date(2026, time.July, 14, 9, 0, 0, 0, time.UTC)
	stateHash := strings.Repeat("d", 64)
	if err := storage.CreateOAuthState(ctx, OAuthState{
		StateHash: stateHash, PKCEVerifier: "verifier", CreatedAt: now.Add(-time.Hour), ExpiresAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ConsumeOAuthState(ctx, stateHash, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ConsumeOAuthState(expired) error = %v, want ErrNotFound", err)
	}
	var count int
	if err := storage.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM oauth_states WHERE state_hash = ?`, stateHash).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expired OAuth state row count = %d, want 0", count)
	}
}

func TestAuthStoreRejectsOpaqueValuesInsteadOfHashes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "hash-validation.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	now := time.Now().UTC()
	invalidTokenHash := "not-a-sha256-digest-" + t.Name()
	if err := storage.CreateAuthSession(ctx, AuthSession{
		TokenHash: invalidTokenHash, YandexUserID: "1", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err == nil {
		t.Fatal("CreateAuthSession(raw token) error = nil, want validation error")
	}
	if err := storage.CreateOAuthState(ctx, OAuthState{
		StateHash: "raw-oauth-state", PKCEVerifier: "verifier", CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}); err == nil {
		t.Fatal("CreateOAuthState(raw state) error = nil, want validation error")
	}

	for _, table := range []string{"auth_sessions", "oauth_states"} {
		var count int
		if err := storage.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("%s row count = %d after invalid insert, want 0", table, count)
		}
	}
}

func TestCreateAuthRecordsPrunesExpiredRowsAndBoundsOAuthStates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storage, err := Open(ctx, filepath.Join(t.TempDir(), "auth-pruning.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	now := time.Date(2026, time.July, 14, 9, 0, 0, 0, time.UTC)
	expiredSessionHash := strings.Repeat("e", 64)
	if err := storage.UpsertUser(ctx, User{ID: "expired-user", DisplayName: "Expired"}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.db.ExecContext(ctx, `
INSERT INTO auth_sessions(token_hash, yandex_user_id, created_at, expires_at)
VALUES (?, 'expired-user', ?, ?)`, expiredSessionHash, timeText(now.Add(-2*time.Hour)), timeText(now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	expiredStateHash := strings.Repeat("f", 64)
	if _, err := storage.db.ExecContext(ctx, `
INSERT INTO oauth_states(state_hash, pkce_verifier, terms_version, personal_data_version, consent_at, created_at, expires_at)
VALUES (?, 'expired-verifier', 'test', 'test', ?, ?, ?)`, expiredStateHash, now.Add(-2*time.Hour), now.Add(-2*time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	if err := storage.CreateAuthSession(ctx, AuthSession{
		TokenHash: strings.Repeat("1", 64), YandexUserID: "live-user",
		AllowlistIdentity: "live-user",
		CreatedAt:         now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := storage.CreateOAuthState(ctx, OAuthState{
		StateHash: strings.Repeat("2", 64), PKCEVerifier: "live-verifier",
		CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	for table, hash := range map[string]string{
		"auth_sessions": expiredSessionHash,
		"oauth_states":  expiredStateHash,
	} {
		var count int
		if err := storage.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+table+` WHERE `+map[string]string{
				"auth_sessions": "token_hash", "oauth_states": "state_hash",
			}[table]+` = ?`, hash).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("expired row in %s was not pruned", table)
		}
	}

	if _, err := storage.db.ExecContext(ctx, `DELETE FROM oauth_states`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxActiveOAuthStates+5; i++ {
		hash := fmt.Sprintf("%064x", i+1)
		if err := storage.CreateOAuthState(ctx, OAuthState{
			StateHash: hash, PKCEVerifier: "verifier",
			CreatedAt: now.Add(time.Duration(i) * time.Nanosecond),
			ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := storage.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_states`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != maxActiveOAuthStates {
		t.Fatalf("live OAuth state row count = %d, want %d", count, maxActiveOAuthStates)
	}
}
