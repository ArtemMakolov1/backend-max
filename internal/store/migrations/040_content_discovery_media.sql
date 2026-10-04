-- A card is retrieval authority only after a grounded provider response has
-- been saved server-side. Client-supplied URLs are never transfer inputs.
CREATE TABLE content_discovery_candidates (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    actor_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel_id BIGINT,
    payload JSONB NOT NULL CHECK (jsonb_typeof(payload)='object'),
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL CHECK (expires_at>created_at),
    UNIQUE (workspace_id,actor_user_id,id),
    FOREIGN KEY (workspace_id,channel_id) REFERENCES channels(workspace_id,id)
      ON DELETE SET NULL (channel_id)
);
CREATE INDEX content_discovery_candidates_scope ON content_discovery_candidates(workspace_id,actor_user_id,expires_at);
CREATE TABLE content_discovery_draft_operations (
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    actor_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    client_request_id UUID NOT NULL,
    candidate_id TEXT NOT NULL,
    request_hash TEXT NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    post_id BIGINT,
    state TEXT NOT NULL CHECK (state IN ('working','complete')),
    generation BIGINT NOT NULL DEFAULT 1 CHECK (generation>0),
    lease_until TIMESTAMPTZ NOT NULL,
    expected_post_updated_at TIMESTAMPTZ NOT NULL,
    report JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(report)='object'),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(workspace_id,actor_user_id,client_request_id),
    FOREIGN KEY (workspace_id,actor_user_id,candidate_id)
      REFERENCES content_discovery_candidates(workspace_id,actor_user_id,id),
    FOREIGN KEY (workspace_id,post_id) REFERENCES posts(workspace_id,id)
      ON DELETE SET NULL (post_id)
);
CREATE TRIGGER content_discovery_candidates_active_workspace_guard
 BEFORE INSERT OR UPDATE ON content_discovery_candidates
 FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
CREATE TRIGGER content_discovery_draft_operations_active_workspace_guard
 BEFORE INSERT OR UPDATE ON content_discovery_draft_operations
 FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
