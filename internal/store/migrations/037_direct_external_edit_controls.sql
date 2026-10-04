-- Independent controls survive replacement of the read-only provider list.
CREATE TABLE direct_external_edit_controls (
    workspace_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    provider_campaign_id BIGINT NOT NULL CHECK (provider_campaign_id > 0),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    observed_hash TEXT NOT NULL CHECK (observed_hash ~ '^[0-9a-f]{64}$'),
    revision_id TEXT NOT NULL,
    bookmarked BOOLEAN NOT NULL DEFAULT FALSE,
    edit_state TEXT NOT NULL DEFAULT 'idle' CHECK (edit_state IN ('idle','updating','uncertain')),
    operation_id TEXT NOT NULL DEFAULT '',
    write_started BOOLEAN NOT NULL DEFAULT FALSE,
    claimed_at TIMESTAMPTZ,
    desired_hash TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (workspace_id, connection_id, provider_campaign_id),
    FOREIGN KEY (workspace_id, connection_id)
        REFERENCES direct_connections(workspace_id, id) ON DELETE CASCADE,
    CHECK ((edit_state = 'idle' AND operation_id = '' AND claimed_at IS NULL AND desired_hash = '' AND NOT write_started)
        OR (edit_state <> 'idle' AND operation_id <> '' AND claimed_at IS NOT NULL AND desired_hash ~ '^[0-9a-f]{64}$'))
);

CREATE TRIGGER direct_external_edit_controls_active_workspace_guard
BEFORE INSERT OR UPDATE ON direct_external_edit_controls
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
