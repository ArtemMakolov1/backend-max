CREATE TABLE auth_identities (
    provider TEXT NOT NULL CHECK (provider IN ('yandex', 'max')),
    subject TEXT NOT NULL,
    owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (provider, subject),
    UNIQUE (owner_id, provider)
);

INSERT INTO auth_identities(provider, subject, owner_id, created_at, updated_at)
SELECT 'yandex', id, id, created_at, updated_at
FROM users
ON CONFLICT DO NOTHING;

INSERT INTO auth_identities(provider, subject, owner_id, created_at, updated_at)
SELECT 'max', max_user_id, owner_id, linked_at, updated_at
FROM max_identity_links
ON CONFLICT DO NOTHING;

ALTER TABLE auth_sessions
    ADD COLUMN provider TEXT DEFAULT 'yandex'
        CHECK (provider IN ('yandex', 'max'));

CREATE INDEX idx_auth_sessions_owner_provider
    ON auth_sessions(yandex_user_id, provider);

CREATE TABLE max_auth_profiles (
    max_user_id TEXT PRIMARY KEY CHECK (max_user_id ~ '^[0-9]+$'),
    first_name TEXT NOT NULL DEFAULT '',
    last_name TEXT NOT NULL DEFAULT '',
    username TEXT NOT NULL DEFAULT '',
    avatar_url TEXT NOT NULL DEFAULT '',
    contact_verified_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE max_auth_attempts (
    id TEXT PRIMARY KEY,
    browser_token_hash TEXT NOT NULL UNIQUE
        CHECK (browser_token_hash ~ '^[0-9a-fA-F]{64}$'),
    deep_token_hash TEXT NOT NULL UNIQUE
        CHECK (deep_token_hash ~ '^[0-9a-fA-F]{64}$'),
    return_to TEXT NOT NULL DEFAULT '/app/',
    comparison_code TEXT NOT NULL CHECK (comparison_code ~ '^[0-9]{6}$'),
    status TEXT NOT NULL CHECK (status IN (
        'pending', 'awaiting_contact', 'verified', 'authenticated',
        'canceled', 'failed', 'expired'
    )),
    max_user_id TEXT NOT NULL DEFAULT '',
    terms_version TEXT NOT NULL,
    personal_data_version TEXT NOT NULL,
    consent_at TIMESTAMPTZ NOT NULL,
    contact_message_id TEXT UNIQUE,
    contact_event_at TIMESTAMPTZ,
    error_code TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    authenticated_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > created_at),
    CHECK (max_user_id = '' OR max_user_id ~ '^[0-9]+$'),
    CHECK ((contact_message_id IS NULL) = (contact_event_at IS NULL))
);

CREATE UNIQUE INDEX idx_max_auth_one_awaiting_per_user
    ON max_auth_attempts(max_user_id)
    WHERE status = 'awaiting_contact';
CREATE INDEX idx_max_auth_attempts_expires_at
    ON max_auth_attempts(expires_at)
    WHERE status IN ('pending', 'awaiting_contact', 'verified');
