-- Durable, tenant-scoped import of posts that already exist in a connected
-- MAX channel. Imported provider media stays remote until it is copied into
-- MaxPosty storage; opaque provider tokens remain server-side only.

ALTER TABLE posts
    ADD COLUMN origin TEXT DEFAULT 'maxposty',
    ADD COLUMN max_history_attachments_complete BOOLEAN DEFAULT TRUE,
    ADD COLUMN max_sender_is_bot BOOLEAN;

UPDATE posts
SET origin = COALESCE(origin, 'maxposty'),
    max_history_attachments_complete = COALESCE(max_history_attachments_complete, TRUE)
WHERE origin IS NULL OR max_history_attachments_complete IS NULL;

ALTER TABLE posts
    ADD CONSTRAINT posts_origin_valid CHECK (
        origin IS NOT NULL AND origin IN ('maxposty', 'max_history')
    ) NOT VALID,
    ADD CONSTRAINT posts_max_history_attachments_complete_present CHECK (
        max_history_attachments_complete IS NOT NULL
    ) NOT VALID,
    ADD CONSTRAINT posts_workspace_id_channel_unique
        UNIQUE (workspace_id, id, channel_id);

ALTER TABLE posts VALIDATE CONSTRAINT posts_origin_valid;
ALTER TABLE posts VALIDATE CONSTRAINT posts_max_history_attachments_complete_present;

-- MAX message IDs are idempotency keys inside one channel. This also closes
-- the race between a normal publication write and history ingestion.
CREATE UNIQUE INDEX idx_posts_channel_max_message_id_unique
    ON posts(channel_id, max_message_id)
    WHERE channel_id IS NOT NULL AND max_message_id <> '';

CREATE SEQUENCE max_history_import_generation_seq AS BIGINT START WITH 1;
CREATE SEQUENCE max_history_import_run_seq AS BIGINT START WITH 1;

CREATE TABLE channel_history_imports (
    workspace_id TEXT NOT NULL,
    channel_id BIGINT NOT NULL,
    generation BIGINT NOT NULL DEFAULT nextval('max_history_import_generation_seq')
        CHECK (generation > 0),
    run_id BIGINT NOT NULL DEFAULT nextval('max_history_import_run_seq')
        CHECK (run_id > 0),
    status TEXT NOT NULL CHECK (status IN ('in_progress', 'complete', 'failed')),
    cursor_from BIGINT CHECK (cursor_from IS NULL OR cursor_from >= 0),
    expected_count BIGINT NOT NULL DEFAULT 0 CHECK (expected_count >= 0),
    processed_count BIGINT NOT NULL DEFAULT 0 CHECK (processed_count >= 0),
    imported_count BIGINT NOT NULL DEFAULT 0 CHECK (imported_count >= 0),
    existing_count BIGINT NOT NULL DEFAULT 0 CHECK (existing_count >= 0),
    skipped_count BIGINT NOT NULL DEFAULT 0 CHECK (skipped_count >= 0),
    claimed_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    previous_completed_at TIMESTAMPTZ,
    error_code TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (workspace_id, channel_id),
    UNIQUE (generation),
    UNIQUE (run_id),
    CONSTRAINT channel_history_imports_channel_fk
        FOREIGN KEY (workspace_id, channel_id)
        REFERENCES channels(workspace_id, id) ON DELETE CASCADE,
    CONSTRAINT channel_history_imports_counters_consistent CHECK (
        processed_count = imported_count + existing_count + skipped_count
    ),
    CONSTRAINT channel_history_imports_state_consistent CHECK (
        (
            status = 'in_progress'
            AND completed_at IS NULL
            AND error_code = ''
        )
        OR
        (
            status = 'complete'
            AND claimed_at IS NULL
            AND completed_at IS NOT NULL
            AND error_code = ''
        )
        OR
        (
            status = 'failed'
            AND claimed_at IS NULL
            AND completed_at IS NOT NULL
            AND btrim(error_code) <> ''
        )
    ),
    CONSTRAINT channel_history_imports_times_consistent CHECK (
        updated_at >= created_at
        AND (claimed_at IS NULL OR claimed_at >= created_at)
        AND (completed_at IS NULL OR completed_at >= created_at)
        AND (previous_completed_at IS NULL OR previous_completed_at >= created_at)
    )
);

CREATE INDEX idx_channel_history_imports_workspace_status
    ON channel_history_imports(workspace_id, status, updated_at DESC, channel_id);
CREATE INDEX idx_channel_history_imports_claimed
    ON channel_history_imports(claimed_at)
    WHERE claimed_at IS NOT NULL;

-- Provider-backed media is additive and deliberately separate from local S3
-- attachments. It shares the legacy attachment sequence so IDs exposed by
-- the combined application projection remain globally unique, while the
-- existing storage_key/media FK and quota semantics stay untouched.
CREATE TABLE max_history_post_attachments (
    id BIGINT PRIMARY KEY DEFAULT nextval('post_attachments_id_seq'),
    owner_id TEXT NOT NULL,
    workspace_id TEXT NOT NULL,
    post_id BIGINT NOT NULL,
    type TEXT NOT NULL CHECK (type IN ('image', 'video')),
    position INTEGER NOT NULL CHECK (position >= 0),
    processing_status TEXT NOT NULL DEFAULT 'ready'
        CHECK (processing_status IN ('uploading', 'processing', 'ready', 'failed')),
    size_bytes BIGINT NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    mime_type TEXT NOT NULL CHECK (btrim(mime_type) <> ''),
    width INTEGER CHECK (width IS NULL OR width > 0),
    height INTEGER CHECK (height IS NULL OR height > 0),
    duration_ms BIGINT CHECK (duration_ms IS NULL OR duration_ms >= 0),
    provider_token TEXT NOT NULL CHECK (btrim(provider_token) <> ''),
    remote_url TEXT NOT NULL CHECK (btrim(remote_url) <> ''),
    provider_meta JSONB NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(provider_meta) = 'object'),
    error_code TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT max_history_post_attachments_owner_post_fk
        FOREIGN KEY (owner_id, post_id)
        REFERENCES posts(owner_id, id) ON DELETE CASCADE,
    CONSTRAINT max_history_post_attachments_workspace_post_fk
        FOREIGN KEY (workspace_id, post_id)
        REFERENCES posts(workspace_id, id) ON DELETE CASCADE,
    CONSTRAINT max_history_post_attachments_post_position_unique
        UNIQUE (post_id, position)
);

CREATE INDEX idx_max_history_post_attachments_workspace_post
    ON max_history_post_attachments(workspace_id, post_id, position, id);

CREATE TABLE max_history_messages (
    workspace_id TEXT NOT NULL,
    channel_id BIGINT NOT NULL,
    post_id BIGINT NOT NULL,
    max_message_id TEXT NOT NULL CHECK (btrim(max_message_id) <> ''),
    last_import_run_id BIGINT NOT NULL CHECK (last_import_run_id > 0),
    raw JSONB NOT NULL CHECK (jsonb_typeof(raw) = 'object'),
    imported_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT max_history_messages_channel_message_unique
        UNIQUE (channel_id, max_message_id),
    CONSTRAINT max_history_messages_post_unique UNIQUE (post_id),
    CONSTRAINT max_history_messages_channel_fk
        FOREIGN KEY (workspace_id, channel_id)
        REFERENCES channels(workspace_id, id) ON DELETE CASCADE,
    CONSTRAINT max_history_messages_workspace_post_channel_fk
        FOREIGN KEY (workspace_id, post_id, channel_id)
        REFERENCES posts(workspace_id, id, channel_id) ON DELETE CASCADE
);

CREATE INDEX idx_max_history_messages_workspace_channel_imported
    ON max_history_messages(workspace_id, channel_id, imported_at DESC, post_id DESC);

CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON channel_history_imports
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();

CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON max_history_messages
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();

CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON max_history_post_attachments
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
