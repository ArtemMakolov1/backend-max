CREATE TABLE max_identity_links (
    owner_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    max_user_id TEXT NOT NULL UNIQUE CHECK (max_user_id ~ '^-?[0-9]+$'),
    linked_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE max_identity_link_attempts (
    id TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-fA-F]{64}$'),
    confirm_token_hash TEXT UNIQUE CHECK (confirm_token_hash IS NULL OR confirm_token_hash ~ '^[0-9a-fA-F]{64}$'),
    cancel_token_hash TEXT UNIQUE CHECK (cancel_token_hash IS NULL OR cancel_token_hash ~ '^[0-9a-fA-F]{64}$'),
    owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    requester_label TEXT NOT NULL,
    comparison_code TEXT NOT NULL CHECK (comparison_code ~ '^[0-9]{6}$'),
    status TEXT NOT NULL CHECK (status IN ('pending', 'awaiting_confirmation', 'linked', 'failed', 'expired')),
    max_user_id TEXT NOT NULL DEFAULT '',
    error_code TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > created_at)
);

CREATE UNIQUE INDEX idx_max_identity_attempts_one_active_owner
    ON max_identity_link_attempts(owner_id)
    WHERE status IN ('pending', 'awaiting_confirmation');
CREATE INDEX idx_max_identity_attempts_owner_created
    ON max_identity_link_attempts(owner_id, created_at DESC);
CREATE INDEX idx_max_identity_attempts_expires_at
    ON max_identity_link_attempts(expires_at)
    WHERE status IN ('pending', 'awaiting_confirmation');

ALTER TABLE observed_bot_chats ADD COLUMN icon_url TEXT DEFAULT '';
ALTER TABLE observed_bot_chats ADD COLUMN participants_count INTEGER DEFAULT 0 CHECK (participants_count IS NULL OR participants_count >= 0);
