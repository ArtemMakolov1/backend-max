-- Keep a durable, workspace-scoped generation marker after an OAuth state is
-- consumed. Provider token/account calls happen outside a database
-- transaction, so one-shot state consumption alone cannot prevent an older
-- in-flight completion from overwriting a connection after a newer start.

CREATE TABLE direct_oauth_latest_attempts (
    workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    state_hash TEXT NOT NULL UNIQUE
        REFERENCES direct_oauth_states(state_hash) ON DELETE CASCADE,
    actor_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_direct_oauth_latest_attempts_actor
    ON direct_oauth_latest_attempts(actor_user_id, workspace_id);
