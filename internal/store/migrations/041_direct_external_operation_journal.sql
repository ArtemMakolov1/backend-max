-- Provider creation IDs and ambiguous writes survive HTTP cancellation.
-- This journal is independent of MaxPosty's managed campaign graph.
CREATE TABLE direct_external_operation_journal (
    workspace_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    provider_campaign_id BIGINT NOT NULL,
    client_request_id UUID NOT NULL,
    operation_id TEXT NOT NULL CHECK (operation_id ~ '^dxe_[0-9a-f]{32}$'),
    operation_kind TEXT NOT NULL CHECK (operation_kind IN ('budget','group','keyword','bid','state','create_group','create_keyword')),
    request_hash TEXT NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    request_payload JSONB NOT NULL CHECK (jsonb_typeof(request_payload)='object'),
    desired_hash TEXT NOT NULL CHECK (desired_hash ~ '^[0-9a-f]{64}$'),
    state TEXT NOT NULL DEFAULT 'updating' CHECK (state IN ('updating','uncertain','succeeded','rejected')),
    write_started BOOLEAN NOT NULL DEFAULT FALSE,
    provider_result_id BIGINT CHECK (provider_result_id > 0),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK (state <> 'uncertain' OR write_started),
    CHECK (provider_result_id IS NULL OR write_started),
    PRIMARY KEY (workspace_id,connection_id,provider_campaign_id,client_request_id),
    UNIQUE (operation_id),
    FOREIGN KEY (workspace_id,connection_id,provider_campaign_id)
      REFERENCES direct_external_edit_controls(workspace_id,connection_id,provider_campaign_id) ON DELETE CASCADE
);
CREATE INDEX direct_external_operation_journal_pending
 ON direct_external_operation_journal(workspace_id,connection_id,provider_campaign_id,created_at DESC)
 WHERE state IN ('updating','uncertain');
CREATE TRIGGER direct_external_operation_journal_active_workspace_guard
 BEFORE INSERT OR UPDATE ON direct_external_operation_journal
 FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
