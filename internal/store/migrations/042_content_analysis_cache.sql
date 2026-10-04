-- Short semantic summaries only; no raw images, sound, source URLs or tokens.
-- One current entry per channel keeps the cache bounded as history changes.
CREATE TABLE content_analysis_cache (
    workspace_id TEXT NOT NULL,
    channel_id BIGINT NOT NULL,
    snapshot_key TEXT NOT NULL CHECK (snapshot_key ~ '^[0-9a-f]{64}$'),
    claim_id TEXT NOT NULL CHECK (claim_id ~ '^[0-9a-f]{32}$'),
    claimed_until TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    result_json TEXT NOT NULL DEFAULT '' CHECK (octet_length(result_json) <= 16384),
    PRIMARY KEY(workspace_id,channel_id),
    FOREIGN KEY(workspace_id,channel_id) REFERENCES channels(workspace_id,id) ON DELETE CASCADE
);
CREATE TRIGGER content_analysis_cache_active_workspace_guard
BEFORE INSERT OR UPDATE ON content_analysis_cache
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
