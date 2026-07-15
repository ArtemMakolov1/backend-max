package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const maxAuthAttemptColumns = `id, browser_token_hash, deep_token_hash, return_to,
comparison_code, status, max_user_id, terms_version, personal_data_version,
consent_at, COALESCE(contact_message_id, ''), contact_event_at, error_code,
created_at, expires_at, authenticated_at, updated_at`

func (s *Store) CreateMAXAuthAttempt(ctx context.Context, attempt MAXAuthAttempt) error {
	if strings.TrimSpace(attempt.ID) == "" || len(attempt.ComparisonCode) != 6 ||
		strings.TrimSpace(attempt.TermsVersion) == "" || strings.TrimSpace(attempt.PersonalDataVersion) == "" ||
		attempt.ConsentAt.IsZero() {
		return errors.New("MAX auth attempt id, comparison code and consent evidence are required")
	}
	if err := validateSHA256Hex("MAX auth browser token hash", attempt.BrowserTokenHash); err != nil {
		return err
	}
	if err := validateSHA256Hex("MAX auth deep-link token hash", attempt.DeepTokenHash); err != nil {
		return err
	}
	if err := validateLifetime(attempt.CreatedAt, attempt.ExpiresAt); err != nil {
		return fmt.Errorf("MAX auth attempt: %w", err)
	}
	now := attempt.CreatedAt.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin MAX auth attempt: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE max_auth_attempts
SET status='expired', error_code='expired', updated_at=$1
WHERE expires_at <= $1 AND status IN ('pending','awaiting_contact','verified')`, now); err != nil {
		return fmt.Errorf("expire MAX auth attempts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO max_auth_attempts(
id, browser_token_hash, deep_token_hash, return_to, comparison_code, status,
terms_version, personal_data_version, consent_at, created_at, expires_at, updated_at)
VALUES ($1,$2,$3,$4,$5,'pending',$6,$7,$8,$9,$10,$9)`, attempt.ID,
		attempt.BrowserTokenHash, attempt.DeepTokenHash, attempt.ReturnTo, attempt.ComparisonCode,
		attempt.TermsVersion, attempt.PersonalDataVersion, attempt.ConsentAt.UTC(), now, attempt.ExpiresAt.UTC()); err != nil {
		return fmt.Errorf("create MAX auth attempt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit MAX auth attempt: %w", err)
	}
	return nil
}

// StartMAXAuthContact atomically consumes the deep-link token and binds the
// browser attempt to the MAX account that opened it. Repeated bot_started
// deliveries for the same account are idempotent.
func (s *Store) StartMAXAuthContact(ctx context.Context, deepTokenHash, maxUserID string, eventAt, now time.Time) (MAXAuthAttempt, bool, error) {
	if err := validateSHA256Hex("MAX auth deep-link token hash", deepTokenHash); err != nil {
		return MAXAuthAttempt{}, false, err
	}
	if !validMAXUserID(maxUserID) || eventAt.IsZero() || now.IsZero() {
		return MAXAuthAttempt{}, false, errors.New("MAX user id and event/current times are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MAXAuthAttempt{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	attempt, err := scanMAXAuthAttempt(tx.QueryRowContext(ctx, bindSQL(`SELECT `+maxAuthAttemptColumns+
		` FROM max_auth_attempts WHERE deep_token_hash=? FOR UPDATE`), deepTokenHash))
	if err != nil {
		return MAXAuthAttempt{}, false, err
	}
	if attempt.Status == MAXAuthAttemptAwaitingContact && attempt.MAXUserID == maxUserID && attempt.ExpiresAt.After(now) {
		return attempt, false, tx.Commit()
	}
	if attempt.Status != MAXAuthAttemptPending || !maxAuthEventWithinAttempt(attempt, eventAt, now) {
		if !attempt.ExpiresAt.After(now) && attempt.Status == MAXAuthAttemptPending {
			_, _ = tx.ExecContext(ctx, `DELETE FROM max_auth_attempts WHERE id=$1`, attempt.ID)
			_ = tx.Commit()
		}
		return MAXAuthAttempt{}, false, ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "max-auth-user:"+maxUserID); err != nil {
		return MAXAuthAttempt{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE max_auth_attempts
SET status='failed', error_code='superseded', updated_at=$1
WHERE max_user_id=$2 AND id<>$3 AND status='awaiting_contact'`, now.UTC(), maxUserID, attempt.ID); err != nil {
		return MAXAuthAttempt{}, false, fmt.Errorf("supersede MAX auth attempt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE max_auth_attempts
SET status='awaiting_contact', max_user_id=$1, updated_at=$2 WHERE id=$3`, maxUserID, now.UTC(), attempt.ID); err != nil {
		return MAXAuthAttempt{}, false, fmt.Errorf("bind MAX auth attempt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return MAXAuthAttempt{}, false, err
	}
	attempt.Status, attempt.MAXUserID, attempt.UpdatedAt = MAXAuthAttemptAwaitingContact, maxUserID, now.UTC()
	return attempt, true, nil
}

// ResetMAXAuthContactStart makes a failed outbound request retryable by the
// same authenticated bot_started delivery. It never changes verified state.
func (s *Store) ResetMAXAuthContactStart(ctx context.Context, requestID, maxUserID string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE max_auth_attempts SET status='pending', max_user_id='', updated_at=?
WHERE id=? AND max_user_id=? AND status='awaiting_contact'`, now.UTC(), requestID, maxUserID)
	if err != nil {
		return fmt.Errorf("reset MAX auth contact request: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetMAXAuthAttemptForBrowser(ctx context.Context, requestID, browserTokenHash string, now time.Time) (MAXAuthAttempt, error) {
	if strings.TrimSpace(requestID) == "" || now.IsZero() {
		return MAXAuthAttempt{}, errors.New("MAX auth request id and current time are required")
	}
	if err := validateSHA256Hex("MAX auth browser token hash", browserTokenHash); err != nil {
		return MAXAuthAttempt{}, err
	}
	attempt, err := scanMAXAuthAttempt(s.db.QueryRowContext(ctx, `SELECT `+maxAuthAttemptColumns+
		` FROM max_auth_attempts WHERE id=? AND browser_token_hash=?`, requestID, browserTokenHash))
	if err != nil {
		return MAXAuthAttempt{}, err
	}
	if (attempt.Status == MAXAuthAttemptPending || attempt.Status == MAXAuthAttemptAwaitingContact || attempt.Status == MAXAuthAttemptVerified) &&
		!attempt.ExpiresAt.After(now) {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM max_auth_attempts WHERE id=? AND browser_token_hash=?`, requestID, browserTokenHash)
		if attempt.MAXUserID != "" {
			_, _ = s.db.ExecContext(ctx, deleteOrphanMAXAuthProfileSQL, attempt.MAXUserID)
		}
		attempt.Status, attempt.ErrorCode, attempt.UpdatedAt = MAXAuthAttemptExpired, "expired", now.UTC()
	} else if attempt.Status == MAXAuthAttemptCancelled || attempt.Status == MAXAuthAttemptFailed {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM max_auth_attempts WHERE id=? AND browser_token_hash=?`, requestID, browserTokenHash)
		if attempt.MAXUserID != "" {
			_, _ = s.db.ExecContext(ctx, deleteOrphanMAXAuthProfileSQL, attempt.MAXUserID)
		}
	}
	return attempt, nil
}

// RecordMAXAuthContact records replay-safe proof that MAX delivered a signed
// request_contact response. It deliberately leaves the attempt awaiting: only
// the attempt-specific callback can move it to verified. Phone/vCard/hash never
// cross this store boundary.
func (s *Store) RecordMAXAuthContact(ctx context.Context, maxUserID, messageID string, eventAt, now time.Time, profile MAXAuthProfile) (MAXAuthAttempt, bool, error) {
	if !validMAXUserID(maxUserID) || profile.MAXUserID != maxUserID || strings.TrimSpace(messageID) == "" || len(messageID) > 512 ||
		eventAt.IsZero() || now.IsZero() {
		return MAXAuthAttempt{}, false, errors.New("valid MAX contact identity, message and times are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MAXAuthAttempt{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	// A retried webhook is acknowledged without mutating the stored proof or
	// profile a second time.
	existing, replayErr := scanMAXAuthAttempt(tx.QueryRowContext(ctx, `SELECT `+maxAuthAttemptColumns+
		` FROM max_auth_attempts WHERE contact_message_id=$1 FOR UPDATE`, messageID))
	if replayErr == nil {
		if existing.MAXUserID != maxUserID {
			return MAXAuthAttempt{}, false, ErrNotFound
		}
		return existing, false, tx.Commit()
	}
	if !errors.Is(replayErr, ErrNotFound) {
		return MAXAuthAttempt{}, false, replayErr
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "max-auth-user:"+maxUserID); err != nil {
		return MAXAuthAttempt{}, false, err
	}
	attempt, err := scanMAXAuthAttempt(tx.QueryRowContext(ctx, `SELECT `+maxAuthAttemptColumns+
		` FROM max_auth_attempts WHERE max_user_id=$1 AND status='awaiting_contact' FOR UPDATE`, maxUserID))
	if err != nil {
		return MAXAuthAttempt{}, false, err
	}
	if !maxAuthEventWithinAttempt(attempt, eventAt, now) {
		if !attempt.ExpiresAt.After(now) {
			_, _ = tx.ExecContext(ctx, `DELETE FROM max_auth_attempts WHERE id=$1`, attempt.ID)
			_ = tx.Commit()
		}
		return MAXAuthAttempt{}, false, ErrNotFound
	}
	// The first valid contact proof is sufficient. A later contact cannot replace
	// it or change the profile while this browser attempt is in progress.
	if attempt.ContactMessageID != "" {
		return attempt, false, tx.Commit()
	}
	verifiedAt := eventAt.UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO max_auth_profiles(
max_user_id, first_name, last_name, username, avatar_url, contact_verified_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT(max_user_id) DO UPDATE SET first_name=excluded.first_name, last_name=excluded.last_name,
username=excluded.username, avatar_url=excluded.avatar_url,
contact_verified_at=excluded.contact_verified_at, updated_at=excluded.updated_at`, maxUserID,
		profile.FirstName, profile.LastName, profile.Username, profile.AvatarURL, verifiedAt, now.UTC()); err != nil {
		return MAXAuthAttempt{}, false, fmt.Errorf("store verified MAX auth profile: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE max_auth_attempts SET contact_message_id=$1,
	contact_event_at=$2, updated_at=$3 WHERE id=$4`, messageID, verifiedAt, now.UTC(), attempt.ID); err != nil {
		return MAXAuthAttempt{}, false, fmt.Errorf("record MAX auth contact: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return MAXAuthAttempt{}, false, err
	}
	attempt.ContactMessageID, attempt.UpdatedAt = messageID, now.UTC()
	attempt.ContactEventAt = &verifiedAt
	return attempt, true, nil
}

// ConfirmMAXAuthContact completes the second half of the two-proof flow. The
// callback token is the hash of the exact deep-link secret used for this
// attempt, so a callback from an older attempt cannot verify a newer one even
// when both belong to the same MAX account. Repeated callbacks are idempotent.
func (s *Store) ConfirmMAXAuthContact(ctx context.Context, deepTokenHash, maxUserID string, eventAt, now time.Time) (MAXAuthAttempt, bool, error) {
	if err := validateSHA256Hex("MAX auth deep-link token hash", deepTokenHash); err != nil {
		return MAXAuthAttempt{}, false, err
	}
	if !validMAXUserID(maxUserID) || eventAt.IsZero() || now.IsZero() {
		return MAXAuthAttempt{}, false, errors.New("MAX user id and event/current times are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MAXAuthAttempt{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "max-auth-user:"+maxUserID); err != nil {
		return MAXAuthAttempt{}, false, err
	}
	attempt, err := scanMAXAuthAttempt(tx.QueryRowContext(ctx, bindSQL(`SELECT `+maxAuthAttemptColumns+
		` FROM max_auth_attempts WHERE deep_token_hash=? FOR UPDATE`), deepTokenHash))
	if err != nil {
		return MAXAuthAttempt{}, false, err
	}
	if attempt.MAXUserID != maxUserID || !maxAuthEventWithinAttempt(attempt, eventAt, now) {
		if !attempt.ExpiresAt.After(now) &&
			(attempt.Status == MAXAuthAttemptAwaitingContact || attempt.Status == MAXAuthAttemptVerified) {
			if _, deleteErr := tx.ExecContext(ctx, `DELETE FROM max_auth_attempts WHERE id=$1`, attempt.ID); deleteErr != nil {
				return MAXAuthAttempt{}, false, deleteErr
			}
			if _, deleteErr := tx.ExecContext(ctx, bindSQL(deleteOrphanMAXAuthProfileSQL), attempt.MAXUserID); deleteErr != nil {
				return MAXAuthAttempt{}, false, deleteErr
			}
			if commitErr := tx.Commit(); commitErr != nil {
				return MAXAuthAttempt{}, false, commitErr
			}
		}
		return MAXAuthAttempt{}, false, ErrNotFound
	}
	if attempt.Status == MAXAuthAttemptVerified {
		return attempt, false, tx.Commit()
	}
	if attempt.Status != MAXAuthAttemptAwaitingContact {
		return MAXAuthAttempt{}, false, ErrNotFound
	}
	if attempt.ContactMessageID == "" || attempt.ContactEventAt == nil {
		// The exact attempt exists, but the first proof has not arrived yet. This
		// is not an error and leaves the callback safely retryable.
		return attempt, false, tx.Commit()
	}
	if eventAt.Before(*attempt.ContactEventAt) {
		return MAXAuthAttempt{}, false, ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE max_auth_attempts SET status='verified', updated_at=$1
	WHERE id=$2 AND status='awaiting_contact'`, now.UTC(), attempt.ID); err != nil {
		return MAXAuthAttempt{}, false, fmt.Errorf("confirm MAX auth contact: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return MAXAuthAttempt{}, false, err
	}
	attempt.Status, attempt.UpdatedAt = MAXAuthAttemptVerified, now.UTC()
	return attempt, true, nil
}

// CompleteMAXAuthAttempt resolves an existing MAX identity or creates a new
// tenant, records consent, and creates the provider-neutral session in one
// transaction. It never merges accounts by phone number.
func (s *Store) CompleteMAXAuthAttempt(ctx context.Context, requestID, browserTokenHash, newOwnerID string, session AuthSession, now time.Time) (AuthSession, error) {
	if strings.TrimSpace(requestID) == "" || strings.TrimSpace(newOwnerID) == "" || now.IsZero() {
		return AuthSession{}, errors.New("MAX auth request, prospective owner and current time are required")
	}
	if err := validateSHA256Hex("MAX auth browser token hash", browserTokenHash); err != nil {
		return AuthSession{}, err
	}
	if err := validateSHA256Hex("token hash", session.TokenHash); err != nil {
		return AuthSession{}, err
	}
	if err := validateLifetime(session.CreatedAt, session.ExpiresAt); err != nil {
		return AuthSession{}, fmt.Errorf("auth session: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AuthSession{}, err
	}
	defer func() { _ = tx.Rollback() }()
	attempt, err := scanMAXAuthAttempt(tx.QueryRowContext(ctx, `SELECT `+maxAuthAttemptColumns+
		` FROM max_auth_attempts WHERE id=$1 AND browser_token_hash=$2 FOR UPDATE`, requestID, browserTokenHash))
	if err != nil {
		return AuthSession{}, err
	}
	if attempt.Status != MAXAuthAttemptVerified || !attempt.ExpiresAt.After(now) || !validMAXUserID(attempt.MAXUserID) {
		if !attempt.ExpiresAt.After(now) && attempt.Status == MAXAuthAttemptVerified {
			_, _ = tx.ExecContext(ctx, `DELETE FROM max_auth_attempts WHERE id=$1`, attempt.ID)
			_ = tx.Commit()
		}
		return AuthSession{}, ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "max-auth-user:"+attempt.MAXUserID); err != nil {
		return AuthSession{}, err
	}
	var identityOwner, linkedOwner sql.NullString
	identityErr := tx.QueryRowContext(ctx, `SELECT owner_id FROM auth_identities
WHERE provider='max' AND subject=$1 FOR UPDATE`, attempt.MAXUserID).Scan(&identityOwner)
	if identityErr != nil && !errors.Is(identityErr, sql.ErrNoRows) {
		return AuthSession{}, fmt.Errorf("lookup MAX auth identity: %w", identityErr)
	}
	linkErr := tx.QueryRowContext(ctx, `SELECT owner_id FROM max_identity_links
WHERE max_user_id=$1 FOR UPDATE`, attempt.MAXUserID).Scan(&linkedOwner)
	if linkErr != nil && !errors.Is(linkErr, sql.ErrNoRows) {
		return AuthSession{}, fmt.Errorf("lookup MAX identity link: %w", linkErr)
	}
	if identityOwner.Valid && linkedOwner.Valid && identityOwner.String != linkedOwner.String {
		return AuthSession{}, fmt.Errorf("%w: MAX identity mappings disagree", ErrConflict)
	}
	ownerID := newOwnerID
	if identityOwner.Valid {
		ownerID = identityOwner.String
	} else if linkedOwner.Valid {
		ownerID = linkedOwner.String
	}
	var profile MAXAuthProfile
	if err := tx.QueryRowContext(ctx, `SELECT max_user_id, first_name, last_name, username, avatar_url,
contact_verified_at, updated_at FROM max_auth_profiles WHERE max_user_id=$1`, attempt.MAXUserID).Scan(
		&profile.MAXUserID, &profile.FirstName, &profile.LastName, &profile.Username, &profile.AvatarURL,
		&profile.ContactVerifiedAt, &profile.UpdatedAt); err != nil {
		return AuthSession{}, fmt.Errorf("load verified MAX auth profile: %w", err)
	}
	displayName := strings.TrimSpace(strings.TrimSpace(profile.FirstName) + " " + strings.TrimSpace(profile.LastName))
	if displayName == "" {
		displayName = firstStoreValue(profile.Username, "Пользователь MAX")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO users(id, login, email, display_name, avatar_url, created_at, updated_at)
VALUES ($1,$2,'',$3,$4,$5,$5)
ON CONFLICT(id) DO UPDATE SET
login=CASE WHEN users.login='' THEN excluded.login ELSE users.login END,
display_name=CASE WHEN users.display_name='' THEN excluded.display_name ELSE users.display_name END,
avatar_url=CASE WHEN users.avatar_url='' THEN excluded.avatar_url ELSE users.avatar_url END,
updated_at=excluded.updated_at`, ownerID, profile.Username, displayName, profile.AvatarURL, now.UTC()); err != nil {
		return AuthSession{}, fmt.Errorf("upsert MAX authenticated user: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO auth_identities(provider, subject, owner_id, created_at, updated_at)
VALUES ('max',$1,$2,$3,$3)
ON CONFLICT(provider, subject) DO UPDATE SET updated_at=excluded.updated_at
WHERE auth_identities.owner_id=excluded.owner_id`, attempt.MAXUserID, ownerID, now.UTC()); err != nil {
		return AuthSession{}, fmt.Errorf("persist MAX auth identity: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO max_identity_links(owner_id, max_user_id, linked_at, updated_at)
VALUES ($1,$2,$3,$3) ON CONFLICT DO NOTHING`, ownerID, attempt.MAXUserID, now.UTC()); err != nil {
		return AuthSession{}, fmt.Errorf("persist MAX identity link: %w", err)
	}
	var confirmedIdentityOwner, confirmedLinkedOwner string
	if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM auth_identities WHERE provider='max' AND subject=$1`, attempt.MAXUserID).Scan(&confirmedIdentityOwner); err != nil {
		return AuthSession{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM max_identity_links WHERE max_user_id=$1`, attempt.MAXUserID).Scan(&confirmedLinkedOwner); err != nil {
		return AuthSession{}, err
	}
	if confirmedIdentityOwner != ownerID || confirmedLinkedOwner != ownerID {
		return AuthSession{}, fmt.Errorf("%w: MAX identity belongs to another account", ErrConflict)
	}
	for _, consent := range []struct{ document, version string }{
		{"terms", attempt.TermsVersion}, {"personal_data", attempt.PersonalDataVersion},
	} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_consents(owner_id, document, version, accepted_at, source)
VALUES ($1,$2,$3,$4,'max_request_contact') ON CONFLICT(owner_id, document, version) DO NOTHING`,
			ownerID, consent.document, consent.version, attempt.ConsentAt.UTC()); err != nil {
			return AuthSession{}, fmt.Errorf("record MAX auth consent: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_sessions WHERE expires_at <= $1`, now.UTC()); err != nil {
		return AuthSession{}, err
	}
	session.OwnerID, session.YandexUserID, session.Provider, session.ProviderSubject = ownerID, ownerID, "max", attempt.MAXUserID
	session.Login, session.Email, session.DisplayName, session.AvatarURL, session.AllowlistIdentity =
		profile.Username, "", displayName, profile.AvatarURL, ""
	if _, err := tx.ExecContext(ctx, `INSERT INTO auth_sessions(token_hash, yandex_user_id, provider, login, email,
display_name, avatar_url, allowlist_identity, created_at, expires_at)
VALUES ($1,$2,'max',$3,'',$4,$5,'',$6,$7)`, session.TokenHash, ownerID, session.Login,
		session.DisplayName, session.AvatarURL, session.CreatedAt.UTC(), session.ExpiresAt.UTC()); err != nil {
		return AuthSession{}, fmt.Errorf("create MAX auth session: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM max_auth_attempts WHERE id=$1`, attempt.ID); err != nil {
		return AuthSession{}, err
	}
	if err := tx.Commit(); err != nil {
		return AuthSession{}, err
	}
	return session, nil
}

func (s *Store) CancelMAXAuthAttempt(ctx context.Context, requestID, browserTokenHash string, now time.Time) error {
	if strings.TrimSpace(requestID) == "" || now.IsZero() {
		return errors.New("MAX auth request id and current time are required")
	}
	if err := validateSHA256Hex("MAX auth browser token hash", browserTokenHash); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin cancel MAX auth attempt: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var maxUserID string
	err = tx.QueryRowContext(ctx, bindSQL(`DELETE FROM max_auth_attempts
WHERE id=? AND browser_token_hash=? AND status IN ('pending','awaiting_contact','verified','canceled','failed')
RETURNING max_user_id`), requestID, browserTokenHash).Scan(&maxUserID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("cancel MAX auth attempt: %w", err)
	}
	if maxUserID != "" {
		if _, err := tx.ExecContext(ctx, bindSQL(deleteOrphanMAXAuthProfileSQL), maxUserID); err != nil {
			return fmt.Errorf("delete canceled MAX auth profile: %w", err)
		}
	}
	return tx.Commit()
}

// PurgeExpiredMAXAuthAttempts enforces the short device-flow retention window
// even when the initiating browser never polls or cancels.
func (s *Store) PurgeExpiredMAXAuthAttempts(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		return errors.New("current time is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin MAX auth purge: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, bindSQL(`DELETE FROM max_auth_attempts
WHERE expires_at <= ? OR status IN ('authenticated','canceled','failed','expired')`), now.UTC()); err != nil {
		return fmt.Errorf("purge MAX auth attempts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM max_auth_profiles p
WHERE NOT EXISTS (SELECT 1 FROM auth_identities i WHERE i.provider='max' AND i.subject=p.max_user_id)
  AND NOT EXISTS (SELECT 1 FROM max_identity_links l WHERE l.max_user_id=p.max_user_id)
  AND NOT EXISTS (SELECT 1 FROM max_auth_attempts a WHERE a.max_user_id=p.max_user_id)`); err != nil {
		return fmt.Errorf("purge orphan MAX auth profiles: %w", err)
	}
	return tx.Commit()
}

const deleteOrphanMAXAuthProfileSQL = `DELETE FROM max_auth_profiles p
WHERE p.max_user_id=?
  AND NOT EXISTS (SELECT 1 FROM auth_identities i WHERE i.provider='max' AND i.subject=p.max_user_id)
  AND NOT EXISTS (SELECT 1 FROM max_identity_links l WHERE l.max_user_id=p.max_user_id)
  AND NOT EXISTS (SELECT 1 FROM max_auth_attempts a WHERE a.max_user_id=p.max_user_id)`

func scanMAXAuthAttempt(row scanner) (MAXAuthAttempt, error) {
	var attempt MAXAuthAttempt
	var contactAt, authenticatedAt sql.NullTime
	if err := row.Scan(&attempt.ID, &attempt.BrowserTokenHash, &attempt.DeepTokenHash, &attempt.ReturnTo,
		&attempt.ComparisonCode, &attempt.Status, &attempt.MAXUserID, &attempt.TermsVersion,
		&attempt.PersonalDataVersion, &attempt.ConsentAt, &attempt.ContactMessageID, &contactAt,
		&attempt.ErrorCode, &attempt.CreatedAt, &attempt.ExpiresAt, &authenticatedAt, &attempt.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MAXAuthAttempt{}, ErrNotFound
		}
		return MAXAuthAttempt{}, fmt.Errorf("scan MAX auth attempt: %w", err)
	}
	if contactAt.Valid {
		value := contactAt.Time.UTC()
		attempt.ContactEventAt = &value
	}
	if authenticatedAt.Valid {
		value := authenticatedAt.Time.UTC()
		attempt.AuthenticatedAt = &value
	}
	attempt.ConsentAt, attempt.CreatedAt, attempt.ExpiresAt, attempt.UpdatedAt = attempt.ConsentAt.UTC(), attempt.CreatedAt.UTC(), attempt.ExpiresAt.UTC(), attempt.UpdatedAt.UTC()
	return attempt, nil
}

func maxAuthEventWithinAttempt(attempt MAXAuthAttempt, eventAt, now time.Time) bool {
	eventAt, now = eventAt.UTC(), now.UTC()
	return !eventAt.Before(attempt.CreatedAt) && eventAt.Before(attempt.ExpiresAt) && attempt.ExpiresAt.After(now)
}

func validMAXUserID(value string) bool {
	if value == "" || len(value) > 20 {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func firstStoreValue(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
