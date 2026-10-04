-- Provider comments are independent from editorial post_comments. The MAX
-- root MID is retained so republishing a local post cannot mix two threads.
CREATE SEQUENCE max_comments_sync_generation_seq;

CREATE TABLE max_post_comments (
    workspace_id TEXT NOT NULL,
    post_id BIGINT NOT NULL,
    channel_id BIGINT NOT NULL,
    root_message_id TEXT NOT NULL CHECK (root_message_id <> ''),
    message_id TEXT NOT NULL CHECK (message_id <> ''),
    text TEXT NOT NULL DEFAULT '',
    sender_user_id TEXT NOT NULL DEFAULT '',
    sender_name TEXT NOT NULL DEFAULT '',
    sender_is_bot BOOLEAN NOT NULL DEFAULT FALSE,
    text_editable BOOLEAN NOT NULL DEFAULT FALSE,
    reply_to_message_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    deleted_at TIMESTAMPTZ,
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    PRIMARY KEY (workspace_id, post_id, root_message_id, message_id),
    FOREIGN KEY (workspace_id, post_id) REFERENCES posts(workspace_id,id) ON DELETE CASCADE,
    FOREIGN KEY (workspace_id, channel_id) REFERENCES channels(workspace_id,id) ON DELETE CASCADE
);
CREATE INDEX max_post_comments_inbox ON max_post_comments(workspace_id,post_id,root_message_id,created_at DESC,message_id);
CREATE INDEX max_post_comments_provider_mapping ON max_post_comments(channel_id,message_id);

CREATE TABLE max_post_comment_sync (
    workspace_id TEXT NOT NULL,
    post_id BIGINT NOT NULL,
    root_message_id TEXT NOT NULL CHECK (root_message_id <> ''),
    generation BIGINT NOT NULL DEFAULT 0,
    claimed_at TIMESTAMPTZ,
    last_synced_at TIMESTAMPTZ,
    truncated BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (workspace_id,post_id,root_message_id),
    FOREIGN KEY (workspace_id,post_id) REFERENCES posts(workspace_id,id) ON DELETE CASCADE
);

-- The caller's request key is permanent. Even an expired/ambiguous send must
-- never be repeated automatically, including with a newly generated key.
CREATE TABLE max_post_comment_operations (
    operation_id TEXT PRIMARY KEY CHECK (operation_id ~ '^mco_[0-9a-f]{32}$'),
    workspace_id TEXT NOT NULL,
    post_id BIGINT NOT NULL,
    channel_id BIGINT NOT NULL,
    root_message_id TEXT NOT NULL CHECK (root_message_id <> ''),
    client_request_id TEXT NOT NULL CHECK (client_request_id ~ '^[A-Za-z0-9_-]{16,80}$'),
    request_hash TEXT NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    actor_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
    bot_user_id TEXT NOT NULL CHECK (bot_user_id ~ '^[1-9][0-9]{0,18}$'),
    kind TEXT NOT NULL CHECK (kind IN ('send','edit','delete')),
    state TEXT NOT NULL CHECK (state IN ('sending','succeeded','rejected','uncertain')),
    write_started BOOLEAN NOT NULL DEFAULT FALSE,
    message_id TEXT NOT NULL DEFAULT '',
    expected_version BIGINT NOT NULL DEFAULT 0,
    desired_text TEXT NOT NULL DEFAULT '',
    desired_format TEXT NOT NULL DEFAULT '',
    desired_reply_to TEXT NOT NULL DEFAULT '',
    claimed_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ,
    UNIQUE (workspace_id,post_id,client_request_id),
    FOREIGN KEY (workspace_id,post_id) REFERENCES posts(workspace_id,id) ON DELETE CASCADE,
    FOREIGN KEY (workspace_id,channel_id) REFERENCES channels(workspace_id,id) ON DELETE CASCADE
);
CREATE INDEX max_post_comment_operations_active ON max_post_comment_operations(workspace_id,post_id,root_message_id,state,claimed_at DESC);
CREATE INDEX max_post_comment_operations_known_sender ON max_post_comment_operations(workspace_id,post_id,root_message_id,message_id)
WHERE kind='send' AND state='succeeded';

CREATE TRIGGER max_post_comment_sync_active_workspace_guard
BEFORE INSERT OR UPDATE ON max_post_comment_sync
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
-- Comments and operations deliberately have no active-workspace trigger: detached
-- finalization must record a known provider result after member removal or
-- workspace archival. Claims and begin-write enforce active scope explicitly.
