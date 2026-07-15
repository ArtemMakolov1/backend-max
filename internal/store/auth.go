package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	authSessionColumns   = `token_hash, yandex_user_id, COALESCE(provider, 'yandex'), login, email, display_name, avatar_url, allowlist_identity, created_at, expires_at`
	oauthStateColumns    = `state_hash, pkce_verifier, return_to, terms_version, personal_data_version, consent_at, created_at, expires_at`
	maxActiveOAuthStates = 1024
)

// CreateAuthenticatedSession atomically upserts the local account, records
// versioned consent evidence, and creates the browser session. A callback can
// therefore never expose an authenticated session without its audit evidence.
func (s *Store) CreateAuthenticatedSession(ctx context.Context, user User, consents []Consent, session AuthSession) error {
	ownerID := authSessionOwnerID(session)
	provider := authSessionProvider(session)
	providerSubject := strings.TrimSpace(session.ProviderSubject)
	if providerSubject == "" {
		providerSubject = ownerID
	}
	if strings.TrimSpace(user.ID) == "" || user.ID != ownerID {
		return errors.New("session user id must match the local user")
	}
	if err := validateAuthProvider(provider); err != nil {
		return err
	}
	if err := validateSHA256Hex("token hash", session.TokenHash); err != nil {
		return err
	}
	if err := validateLifetime(session.CreatedAt, session.ExpiresAt); err != nil {
		return fmt.Errorf("auth session: %w", err)
	}
	if len(consents) != 2 {
		return errors.New("terms and personal data consent evidence are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin authenticated session transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := session.CreatedAt.UTC()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO users(id, login, email, display_name, avatar_url, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $6)
ON CONFLICT(id) DO UPDATE SET login = excluded.login, email = excluded.email,
display_name = excluded.display_name, avatar_url = excluded.avatar_url, updated_at = excluded.updated_at`,
		user.ID, user.Login, user.Email, user.DisplayName, user.AvatarURL, now); err != nil {
		return fmt.Errorf("upsert authenticated user: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO auth_identities(provider, subject, owner_id, created_at, updated_at)
VALUES ($1, $2, $3, $4, $4)
ON CONFLICT(provider, subject) DO UPDATE SET updated_at=excluded.updated_at
WHERE auth_identities.owner_id=excluded.owner_id`, provider, providerSubject, ownerID, now); err != nil {
		return fmt.Errorf("upsert authenticated identity: %w", err)
	}
	for _, consent := range consents {
		if consent.Document != "terms" && consent.Document != "personal_data" {
			return fmt.Errorf("unsupported consent document %q", consent.Document)
		}
		if strings.TrimSpace(consent.Version) == "" || consent.AcceptedAt.IsZero() {
			return errors.New("consent version and accepted_at are required")
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO user_consents(owner_id, document, version, accepted_at, source)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT(owner_id, document, version) DO NOTHING`, user.ID, consent.Document,
			consent.Version, consent.AcceptedAt.UTC(), consent.Source); err != nil {
			return fmt.Errorf("record %s consent: %w", consent.Document, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_sessions WHERE expires_at <= $1`, now); err != nil {
		return fmt.Errorf("delete expired auth sessions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO auth_sessions(token_hash, yandex_user_id, provider, login, email, display_name, avatar_url, allowlist_identity, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, session.TokenHash, ownerID, provider,
		session.Login, session.Email, session.DisplayName, session.AvatarURL, session.AllowlistIdentity, now, session.ExpiresAt.UTC()); err != nil {
		return fmt.Errorf("create auth session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit authenticated session: %w", err)
	}
	return nil
}

func (s *Store) UpsertUser(ctx context.Context, user User) error {
	if strings.TrimSpace(user.ID) == "" {
		return errors.New("user id is required")
	}
	now := time.Now().UTC()
	if !user.CreatedAt.IsZero() {
		now = user.CreatedAt.UTC()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO users(id, login, email, display_name, avatar_url, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET login = excluded.login, email = excluded.email,
display_name = excluded.display_name, avatar_url = excluded.avatar_url, updated_at = excluded.updated_at`, user.ID, user.Login,
		user.Email, user.DisplayName, user.AvatarURL, now, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("upsert user: %w", err)
	}
	return nil
}

func (s *Store) GetUser(ctx context.Context, userID string) (User, error) {
	var user User
	if err := s.db.QueryRowContext(ctx, `SELECT id, login, email, display_name, avatar_url, created_at, updated_at
FROM users WHERE id = ?`, userID).Scan(&user.ID, &user.Login, &user.Email, &user.DisplayName,
		&user.AvatarURL, &user.CreatedAt, &user.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("get user: %w", err)
	}
	user.CreatedAt = user.CreatedAt.UTC()
	user.UpdatedAt = user.UpdatedAt.UTC()
	return user, nil
}

// CreateAuthSession persists a session using a SHA-256 hex digest supplied by
// the caller. It deliberately never accepts or derives the opaque browser
// token itself.
func (s *Store) CreateAuthSession(ctx context.Context, session AuthSession) error {
	if err := validateSHA256Hex("token hash", session.TokenHash); err != nil {
		return err
	}
	ownerID := authSessionOwnerID(session)
	provider := authSessionProvider(session)
	providerSubject := strings.TrimSpace(session.ProviderSubject)
	if providerSubject == "" {
		providerSubject = ownerID
	}
	if ownerID == "" {
		return errors.New("session owner id is required")
	}
	if err := validateAuthProvider(provider); err != nil {
		return err
	}
	if err := validateLifetime(session.CreatedAt, session.ExpiresAt); err != nil {
		return fmt.Errorf("auth session: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin auth session transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO users(id, login, email, display_name, avatar_url, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $6)
ON CONFLICT(id) DO UPDATE SET login = excluded.login, email = excluded.email,
	display_name = excluded.display_name, avatar_url = excluded.avatar_url, updated_at = excluded.updated_at`, ownerID,
		session.Login, session.Email, session.DisplayName, session.AvatarURL, session.CreatedAt.UTC()); err != nil {
		return fmt.Errorf("upsert auth session user: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO auth_identities(provider, subject, owner_id, created_at, updated_at)
VALUES ($1,$2,$3,$4,$4)
ON CONFLICT(provider, subject) DO UPDATE SET updated_at=excluded.updated_at
WHERE auth_identities.owner_id=excluded.owner_id`, provider, providerSubject, ownerID, session.CreatedAt.UTC()); err != nil {
		return fmt.Errorf("upsert auth session identity: %w", err)
	}

	if _, err := tx.ExecContext(ctx, bindSQL(`DELETE FROM auth_sessions WHERE expires_at <= ?`), session.CreatedAt.UTC()); err != nil {
		return fmt.Errorf("delete expired auth sessions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO auth_sessions(token_hash, yandex_user_id, provider, login, email, display_name, avatar_url, allowlist_identity, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		session.TokenHash, ownerID, provider, session.Login, session.Email, session.DisplayName,
		session.AvatarURL, session.AllowlistIdentity, session.CreatedAt.UTC(), session.ExpiresAt.UTC()); err != nil {
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
		tokenHash, session.ExpiresAt.UTC()); err != nil {
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

	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_states WHERE expires_at <= $1`, state.CreatedAt.UTC()); err != nil {
		return fmt.Errorf("delete expired oauth states: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO oauth_states(state_hash, pkce_verifier, return_to, terms_version, personal_data_version, consent_at, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, state.StateHash, state.PKCEVerifier, state.ReturnTo,
		state.TermsVersion, state.PersonalDataVersion, state.ConsentAt.UTC(), state.CreatedAt.UTC(), state.ExpiresAt.UTC()); err != nil {
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
	OFFSET $1
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
	if err := row.Scan(&session.TokenHash, &session.OwnerID, &session.Provider, &session.Login, &session.Email,
		&session.DisplayName, &session.AvatarURL, &session.AllowlistIdentity, &session.CreatedAt, &session.ExpiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AuthSession{}, ErrNotFound
		}
		return AuthSession{}, fmt.Errorf("scan auth session: %w", err)
	}

	session.YandexUserID = session.OwnerID
	session.CreatedAt = session.CreatedAt.UTC()
	session.ExpiresAt = session.ExpiresAt.UTC()
	return session, nil
}

func authSessionOwnerID(session AuthSession) string {
	if ownerID := strings.TrimSpace(session.OwnerID); ownerID != "" {
		return ownerID
	}
	return strings.TrimSpace(session.YandexUserID)
}

func authSessionProvider(session AuthSession) string {
	provider := strings.ToLower(strings.TrimSpace(session.Provider))
	if provider == "" {
		return "yandex"
	}
	return provider
}

func validateAuthProvider(provider string) error {
	if provider != "yandex" && provider != "max" {
		return errors.New("auth provider must be yandex or max")
	}
	return nil
}

func scanOAuthState(row scanner) (OAuthState, error) {
	var state OAuthState
	if err := row.Scan(&state.StateHash, &state.PKCEVerifier, &state.ReturnTo, &state.TermsVersion,
		&state.PersonalDataVersion, &state.ConsentAt, &state.CreatedAt, &state.ExpiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return OAuthState{}, ErrNotFound
		}
		return OAuthState{}, fmt.Errorf("scan oauth state: %w", err)
	}

	state.ConsentAt = state.ConsentAt.UTC()
	state.CreatedAt = state.CreatedAt.UTC()
	state.ExpiresAt = state.ExpiresAt.UTC()
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
