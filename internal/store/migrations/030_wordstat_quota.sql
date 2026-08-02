-- Cross-replica rolling quotas for the shared Yandex Wordstat provider key.
-- Only a one-way API-key fingerprint is persisted; provider credentials and
-- request phrases never enter this ledger.

CREATE TABLE wordstat_quota_events (
    id BIGSERIAL PRIMARY KEY,
    provider_key_hash TEXT NOT NULL CHECK (provider_key_hash ~ '^[0-9a-f]{64}$'),
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    actor_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    occurred_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_wordstat_quota_provider_time
    ON wordstat_quota_events(provider_key_hash, occurred_at, id);
CREATE INDEX idx_wordstat_quota_workspace_time
    ON wordstat_quota_events(workspace_id, occurred_at, id);
CREATE INDEX idx_wordstat_quota_actor_workspace_time
    ON wordstat_quota_events(actor_user_id, workspace_id, occurred_at, id);
CREATE INDEX idx_wordstat_quota_time
    ON wordstat_quota_events(occurred_at, id);
