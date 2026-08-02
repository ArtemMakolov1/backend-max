-- Latest provider snapshot for Yandex Direct campaigns that have not been
-- imported into MaxPosty. The snapshot is isolated by workspace and
-- connection so reconnects and multi-tenant reads cannot leak provider data.

ALTER TABLE direct_connections
    ADD COLUMN external_campaigns_synced_at TIMESTAMPTZ;

ALTER TABLE direct_connections
    ADD COLUMN external_campaigns_sync_started_at TIMESTAMPTZ;

ALTER TABLE direct_connections
    ADD COLUMN external_campaigns_sync_claimed_at TIMESTAMPTZ;

ALTER TABLE direct_connections
    ADD COLUMN external_campaigns_sync_generation BIGINT DEFAULT 0;

ALTER TABLE direct_connections
    ADD CONSTRAINT direct_connections_external_campaign_sync_state CHECK (
        (
            COALESCE(external_campaigns_sync_generation, 0) = 0
            AND external_campaigns_synced_at IS NULL
            AND external_campaigns_sync_started_at IS NULL
            AND external_campaigns_sync_claimed_at IS NULL
        )
        OR
        (
            COALESCE(external_campaigns_sync_generation, 0) > 0
            AND external_campaigns_sync_started_at IS NOT NULL
            AND (
                external_campaigns_sync_claimed_at IS NULL
                OR external_campaigns_sync_claimed_at >= external_campaigns_sync_started_at
            )
        )
    );

CREATE TABLE direct_external_campaigns (
    workspace_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    provider_campaign_id BIGINT NOT NULL CHECK (provider_campaign_id > 0),
    name TEXT NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 255),
    campaign_type TEXT NOT NULL CHECK (
        campaign_type ~ '^[A-Z][A-Z0-9_]{0,63}$'
    ),
    provider_status TEXT NOT NULL CHECK (
        provider_status ~ '^[A-Z][A-Z0-9_]{0,63}$'
    ),
    provider_state TEXT NOT NULL CHECK (
        provider_state ~ '^[A-Z][A-Z0-9_]{0,63}$'
    ),
    provider_status_payment TEXT NOT NULL CHECK (
        provider_status_payment ~ '^[A-Z][A-Z0-9_]{0,63}$'
    ),
    starts_at DATE NOT NULL,
    ends_at DATE,
    timezone TEXT NOT NULL CHECK (
        btrim(timezone) <> '' AND char_length(timezone) <= 128
    ),
    synced_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT direct_external_campaigns_pk
        PRIMARY KEY (workspace_id, connection_id, provider_campaign_id),
    CONSTRAINT direct_external_campaigns_connection_fk
        FOREIGN KEY (workspace_id, connection_id)
        REFERENCES direct_connections(workspace_id, id) ON DELETE CASCADE,
    CONSTRAINT direct_external_campaigns_dates
        CHECK (ends_at IS NULL OR ends_at >= starts_at)
);

CREATE TRIGGER direct_external_campaigns_active_workspace_guard
BEFORE INSERT OR UPDATE ON direct_external_campaigns
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();

CREATE INDEX idx_direct_external_campaigns_workspace_connection_starts
    ON direct_external_campaigns(
        workspace_id, connection_id, starts_at DESC, provider_campaign_id DESC
    );
