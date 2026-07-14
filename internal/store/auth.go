package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

const (
	authSessionColumns   = `token_hash, yandex_user_id, login, email, display_name, allowlist_identity, created_at, expires_at`
	oauthStateColumns    = `state_hash, pkce_verifier, return_to, created_at, expires_at`
	maxActiveOAuthStates = 1024
)

// CreateAuthSession persists a session using a SHA-256 hex digest supplied by
// the caller. It deliberately never accepts or derives the opaque browser
// token itself.
func (s *Store) CreateAuthSession(ctx context.Context, session AuthSession) error {
	if err := validateSHA256Hex("token hash", session.TokenHash); err != nil {
		return err
	}
	if session.YandexUserID == "" {
		return errors.New("yandex user id is required")
	}
	if session.AllowlistIdentity == "" {
		return errors.New("allowlist identity is required")
	}
	if err := validateLifetime(session.CreatedAt, session.ExpiresAt); err != nil {
		return fmt.Errorf("auth session: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin auth session transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_sessions WHERE expires_at <= ?`, timeText(session.CreatedAt)); err != nil {
		return fmt.Errorf("delete expired auth sessions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO auth_sessions(token_hash, yandex_user_id, login, email, display_name, allowlist_identity, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		session.TokenHash, session.YandexUserID, session.Login, session.Email, session.DisplayName,
		session.AllowlistIdentity, timeText(session.CreatedAt), timeText(session.ExpiresAt)); err != nil {
		return fmt.Errorf("create auth session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit auth session: %w", err)
	}
	return nil
}

// GetAuthSession returns a live session. An expired session is removed and
// reported as ErrNotFound, so callers cannot accidentally authenticate it.
func (s *Store) GetAuthSession(ctx context.Context, tokenHash string, now time.Time) (AuthSession, error) {
	if err := validateSHA256Hex("token hash", tokenHash); err != nil {
		return AuthSession{}, err
	}
	if now.IsZero() {
		return AuthSession{}, errors.New("current time is required")
	}

	session, err := scanAuthSession(s.db.QueryRowContext(ctx,
		`SELECT `+authSessionColumns+` FROM auth_sessions WHERE token_hash = ?`, tokenHash))
	if err != nil {
		return AuthSession{}, err
	}
	if session.ExpiresAt.After(now) {
		return session, nil
	}

	// Match the exact expired revision so a hypothetical replacement created
	// between SELECT and DELETE is not removed.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM auth_sessions WHERE token_hash = ? AND expires_at = ?`,
		tokenHash, timeText(session.ExpiresAt)); err != nil {
		return AuthSession{}, fmt.Errorf("delete expired auth session: %w", err)
	}
	return AuthSession{}, ErrNotFound
}

// DeleteAuthSession is idempotent, which makes repeated logout requests safe.
func (s *Store) DeleteAuthSession(ctx context.Context, tokenHash string) error {
	if err := validateSHA256Hex("token hash", tokenHash); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("delete auth session: %w", err)
	}
	return nil
}

// CreateOAuthState persists a one-time OAuth state using a SHA-256 hex digest
// supplied by the caller. The opaque browser state is never stored.
func (s *Store) CreateOAuthState(ctx context.Context, state OAuthState) error {
	if err := validateSHA256Hex("state hash", state.StateHash); err != nil {
		return err
	}
	if state.PKCEVerifier == "" {
		return errors.New("PKCE verifier is required")
	}
	if err := validateLifetime(state.CreatedAt, state.ExpiresAt); err != nil {
		return fmt.Errorf("oauth state: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin oauth state transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_states WHERE expires_at <= ?`, timeText(state.CreatedAt)); err != nil {
		return fmt.Errorf("delete expired oauth states: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO oauth_states(state_hash, pkce_verifier, return_to, created_at, expires_at)
VALUES (?, ?, ?, ?, ?)`, state.StateHash, state.PKCEVerifier, state.ReturnTo,
		timeText(state.CreatedAt), timeText(state.ExpiresAt)); err != nil {
		return fmt.Errorf("create oauth state: %w", err)
	}
	// The start endpoint is public. Keep the live-state table bounded even if a
	// client repeatedly starts OAuth without ever returning from Yandex.
	if _, err := tx.ExecContext(ctx, `
DELETE FROM oauth_states
WHERE state_hash IN (
	SELECT state_hash
	FROM oauth_states
	ORDER BY created_at DESC, state_hash DESC
	LIMIT -1 OFFSET ?
)`, maxActiveOAuthStates); err != nil {
		return fmt.Errorf("trim oauth states: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit oauth state: %w", err)
	}
	return nil
}

// ConsumeOAuthState atomically deletes and returns a state. DELETE RETURNING
// makes concurrent callbacks race on the deletion itself: exactly one can
// receive the row. Expired states are also deleted but reported as ErrNotFound.
func (s *Store) ConsumeOAuthState(ctx context.Context, stateHash string, now time.Time) (OAuthState, error) {
	if err := validateSHA256Hex("state hash", stateHash); err != nil {
		return OAuthState{}, err
	}
	if now.IsZero() {
		return OAuthState{}, errors.New("current time is required")
	}

	state, err := scanOAuthState(s.db.QueryRowContext(ctx,
		`DELETE FROM oauth_states WHERE state_hash = ? RETURNING `+oauthStateColumns, stateHash))
	if err != nil {
		return OAuthState{}, err
	}
	if !state.ExpiresAt.After(now) {
		return OAuthState{}, ErrNotFound
	}
	return state, nil
}

func scanAuthSession(row scanner) (AuthSession, error) {
	var session AuthSession
	var createdAt, expiresAt string
	if err := row.Scan(&session.TokenHash, &session.YandexUserID, &session.Login, &session.Email,
		&session.DisplayName, &session.AllowlistIdentity, &createdAt, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AuthSession{}, ErrNotFound
		}
		return AuthSession{}, fmt.Errorf("scan auth session: %w", err)
	}

	var err error
	session.CreatedAt, err = parseStoredTime("auth session created_at", createdAt)
	if err != nil {
		return AuthSession{}, err
	}
	session.ExpiresAt, err = parseStoredTime("auth session expires_at", expiresAt)
	if err != nil {
		return AuthSession{}, err
	}
	return session, nil
}

func scanOAuthState(row scanner) (OAuthState, error) {
	var state OAuthState
	var createdAt, expiresAt string
	if err := row.Scan(&state.StateHash, &state.PKCEVerifier, &state.ReturnTo, &createdAt, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return OAuthState{}, ErrNotFound
		}
		return OAuthState{}, fmt.Errorf("scan oauth state: %w", err)
	}

	var err error
	state.CreatedAt, err = parseStoredTime("oauth state created_at", createdAt)
	if err != nil {
		return OAuthState{}, err
	}
	state.ExpiresAt, err = parseStoredTime("oauth state expires_at", expiresAt)
	if err != nil {
		return OAuthState{}, err
	}
	return state, nil
}

func validateSHA256Hex(name, value string) error {
	if len(value) != 64 {
		return fmt.Errorf("%s must be a 64-character SHA-256 hex digest", name)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("%s must be a SHA-256 hex digest: %w", name, err)
	}
	return nil
}

func validateLifetime(createdAt, expiresAt time.Time) error {
	if createdAt.IsZero() {
		return errors.New("created_at is required")
	}
	if expiresAt.IsZero() {
		return errors.New("expires_at is required")
	}
	if !expiresAt.After(createdAt) {
		return errors.New("expires_at must be after created_at")
	}
	return nil
}

func timeText(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseStoredTime(name, value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse %s: %w", name, err)
	}
	return parsed.UTC(), nil
}
