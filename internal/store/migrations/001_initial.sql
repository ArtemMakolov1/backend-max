CREATE TABLE users (
    id TEXT PRIMARY KEY,
    login TEXT NOT NULL DEFAULT '',
    email TEXT NOT NULL DEFAULT '',
    display_name TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE channels (
    id BIGSERIAL PRIMARY KEY,
    owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    verified_max_owner_id TEXT NOT NULL,
    max_chat_id TEXT NOT NULL UNIQUE,
    title TEXT NOT NULL,
    public_link TEXT NOT NULL DEFAULT '',
    icon_url TEXT NOT NULL DEFAULT '',
    participants_count INTEGER NOT NULL DEFAULT 0 CHECK (participants_count >= 0),
    is_channel BOOLEAN NOT NULL DEFAULT TRUE,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (owner_id, id)
);

CREATE INDEX idx_channels_owner_active ON channels(owner_id, active DESC, id);

CREATE TABLE observed_bot_chats (
    max_chat_id TEXT PRIMARY KEY,
    public_link TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL DEFAULT '',
    max_owner_id TEXT NOT NULL DEFAULT '',
    active BOOLEAN NOT NULL DEFAULT TRUE,
    last_seen_at TIMESTAMPTZ NOT NULL,
    removed_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX idx_observed_bot_chats_public_link_active
    ON observed_bot_chats(lower(public_link)) WHERE active AND public_link <> '';

CREATE TABLE posts (
    id BIGSERIAL PRIMARY KEY,
    owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title TEXT NOT NULL DEFAULT '',
    content TEXT NOT NULL DEFAULT '',
    format TEXT NOT NULL DEFAULT 'markdown' CHECK (format IN ('markdown', 'html')),
    status TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'scheduled', 'publishing', 'published', 'failed')),
    channel_id BIGINT,
    image_url TEXT NOT NULL DEFAULT '',
    image_path TEXT NOT NULL DEFAULT '',
    image_prompt TEXT NOT NULL DEFAULT '',
    notify BOOLEAN NOT NULL DEFAULT TRUE,
    disable_link_preview BOOLEAN NOT NULL DEFAULT FALSE,
    scheduled_at TIMESTAMPTZ,
    max_message_id TEXT NOT NULL DEFAULT '',
    last_error TEXT NOT NULL DEFAULT '',
    published_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT posts_owner_channel_fk FOREIGN KEY (owner_id, channel_id)
        REFERENCES channels(owner_id, id) ON DELETE SET NULL (channel_id),
    CONSTRAINT posts_schedule_consistency CHECK (
        (status = 'scheduled' AND scheduled_at IS NOT NULL) OR
        (status <> 'scheduled' AND scheduled_at IS NULL)
    )
);

CREATE INDEX idx_posts_owner_created ON posts(owner_id, created_at DESC, id DESC);
CREATE INDEX idx_posts_owner_status_scheduled_at ON posts(owner_id, status, scheduled_at);
CREATE INDEX idx_posts_status_scheduled_at ON posts(status, scheduled_at)
    WHERE owner_id <> '' AND status = 'scheduled';
CREATE INDEX idx_posts_owner_channel_id ON posts(owner_id, channel_id);

CREATE TABLE user_consents (
    owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    document TEXT NOT NULL CHECK (document IN ('terms', 'personal_data')),
    version TEXT NOT NULL,
    accepted_at TIMESTAMPTZ NOT NULL,
    source TEXT NOT NULL,
    PRIMARY KEY (owner_id, document, version)
);

CREATE TABLE auth_sessions (
    token_hash TEXT PRIMARY KEY CHECK (token_hash ~ '^[0-9a-fA-F]{64}$'),
    yandex_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    login TEXT NOT NULL DEFAULT '',
    email TEXT NOT NULL DEFAULT '',
    display_name TEXT NOT NULL DEFAULT '',
    allowlist_identity TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > created_at)
);

CREATE INDEX idx_auth_sessions_expires_at ON auth_sessions(expires_at);

CREATE TABLE oauth_states (
    state_hash TEXT PRIMARY KEY CHECK (state_hash ~ '^[0-9a-fA-F]{64}$'),
    pkce_verifier TEXT NOT NULL,
    return_to TEXT NOT NULL DEFAULT '',
    terms_version TEXT NOT NULL,
    personal_data_version TEXT NOT NULL,
    consent_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > created_at)
);

CREATE INDEX idx_oauth_states_expires_at ON oauth_states(expires_at);

CREATE TABLE channel_claims (
    id TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-fA-F]{64}$'),
    confirm_token_hash TEXT UNIQUE CHECK (confirm_token_hash IS NULL OR confirm_token_hash ~ '^[0-9a-fA-F]{64}$'),
    cancel_token_hash TEXT UNIQUE CHECK (cancel_token_hash IS NULL OR cancel_token_hash ~ '^[0-9a-fA-F]{64}$'),
    owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    max_chat_id TEXT NOT NULL,
    public_link TEXT NOT NULL DEFAULT '',
    requested_title TEXT NOT NULL DEFAULT '',
    requester_label TEXT NOT NULL,
    comparison_code TEXT NOT NULL CHECK (comparison_code ~ '^[0-9]{6}$'),
    status TEXT NOT NULL CHECK (status IN ('pending', 'awaiting_confirmation', 'identity_verified', 'connected', 'failed', 'expired')),
    max_user_id TEXT NOT NULL DEFAULT '',
    channel_id BIGINT,
    error_code TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > created_at),
    FOREIGN KEY (owner_id, channel_id) REFERENCES channels(owner_id, id) ON DELETE SET NULL (channel_id)
);

CREATE UNIQUE INDEX idx_channel_claims_one_active_target
    ON channel_claims(owner_id, max_chat_id)
    WHERE status IN ('pending', 'awaiting_confirmation', 'identity_verified');
CREATE INDEX idx_channel_claims_owner_created ON channel_claims(owner_id, created_at DESC);
CREATE INDEX idx_channel_claims_expires_at ON channel_claims(expires_at)
    WHERE status IN ('pending', 'awaiting_confirmation', 'identity_verified');

CREATE TABLE media_assets (
    owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    filename TEXT NOT NULL CHECK (filename <> '' AND filename !~ '[/\\]'),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (owner_id, filename)
);

CREATE INDEX idx_media_assets_filename ON media_assets(filename);
