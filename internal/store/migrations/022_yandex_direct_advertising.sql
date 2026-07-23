-- Workspace-scoped Yandex Direct integration. This is deliberately separate
-- from the existing campaigns/campaign_variants content-planning domain.
-- Provider credentials are application-encrypted before they reach PostgreSQL.

CREATE TABLE direct_oauth_states (
    state_hash TEXT PRIMARY KEY CHECK (state_hash ~ '^[0-9a-f]{64}$'),
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    actor_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    pkce_verifier TEXT NOT NULL CHECK (char_length(pkce_verifier) BETWEEN 32 AND 128),
    client_login TEXT NOT NULL DEFAULT '' CHECK (char_length(client_login) <= 255),
    return_to TEXT NOT NULL DEFAULT '/app/#/advertising'
        CHECK (return_to LIKE '/%' AND return_to NOT LIKE '//%'),
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    CHECK (expires_at > created_at),
    CHECK (consumed_at IS NULL OR consumed_at >= created_at)
);

CREATE INDEX idx_direct_oauth_states_expiry
    ON direct_oauth_states(expires_at) WHERE consumed_at IS NULL;

CREATE TABLE direct_connections (
    id TEXT PRIMARY KEY CHECK (id ~ '^dcon_[0-9a-f]{32}$'),
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    account_id TEXT NOT NULL CHECK (btrim(account_id) <> '' AND char_length(account_id) <= 255),
    client_login TEXT NOT NULL DEFAULT '' CHECK (char_length(client_login) <= 255),
    account_name TEXT NOT NULL DEFAULT '' CHECK (char_length(account_name) <= 255),
    currency_code TEXT NOT NULL CHECK (currency_code ~ '^[A-Z]{3}$'),
    timezone TEXT NOT NULL DEFAULT 'Europe/Moscow'
        CHECK (btrim(timezone) <> '' AND char_length(timezone) <= 128),
    read_only BOOLEAN NOT NULL DEFAULT FALSE,
    token_ciphertext TEXT NOT NULL,
    token_key_version INTEGER NOT NULL DEFAULT 1 CHECK (token_key_version > 0),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'error', 'revoked')),
    connected_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    last_verified_at TIMESTAMPTZ,
    error_code TEXT NOT NULL DEFAULT '' CHECK (char_length(error_code) <= 128),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    revoked_at TIMESTAMPTZ,
    CONSTRAINT direct_connections_workspace_id_unique UNIQUE (workspace_id, id),
    CONSTRAINT direct_connections_active_secret CHECK (
        (status = 'revoked' AND revoked_at IS NOT NULL AND token_ciphertext = '')
        OR
        (status IN ('active', 'error') AND revoked_at IS NULL AND token_ciphertext <> '')
    )
);

CREATE UNIQUE INDEX idx_direct_connections_one_active
    ON direct_connections(workspace_id)
    WHERE status IN ('active', 'error') AND revoked_at IS NULL;
CREATE INDEX idx_direct_connections_workspace_updated
    ON direct_connections(workspace_id, updated_at DESC, id);

CREATE TABLE direct_campaigns (
    id TEXT PRIMARY KEY CHECK (id ~ '^dcmp_[0-9a-f]{32}$'),
    workspace_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    provider_campaign_id BIGINT CHECK (provider_campaign_id IS NULL OR provider_campaign_id > 0),
    name TEXT NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 255),
    objective TEXT NOT NULL CHECK (objective ~ '^[a-z][a-z0-9_]{0,63}$'),
    landing_url TEXT NOT NULL CHECK (
        char_length(landing_url) BETWEEN 1 AND 2048
        AND landing_url ~ '^https://'
    ),
    brief TEXT NOT NULL CHECK (char_length(btrim(brief)) BETWEEN 1 AND 4000),
    regions JSONB NOT NULL CHECK (
        jsonb_typeof(regions) = 'array'
        AND jsonb_array_length(regions) BETWEEN 1 AND 100
    ),
    weekly_budget_minor BIGINT NOT NULL CHECK (weekly_budget_minor >= 30000),
    currency_code TEXT NOT NULL CHECK (currency_code = 'RUB'),
    starts_at DATE NOT NULL,
    ends_at DATE NOT NULL,
    status TEXT NOT NULL DEFAULT 'draft' CHECK (
        status IN (
            'draft', 'creating', 'provider_draft', 'moderation', 'accepted', 'rejected',
            'active', 'suspended', 'completed', 'error'
        )
    ),
    provider_status TEXT NOT NULL DEFAULT '' CHECK (
        provider_status IN ('', 'DRAFT', 'MODERATION', 'ACCEPTED', 'REJECTED')
    ),
    provider_state TEXT NOT NULL DEFAULT '' CHECK (char_length(provider_state) <= 64),
    provider_next_check_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    auto_launch_next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    created_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    submitted_at TIMESTAMPTZ,
    launch_claimed_at TIMESTAMPTZ,
    launch_state TEXT NOT NULL DEFAULT 'idle' CHECK (
        launch_state IN ('idle', 'launching', 'reconciling', 'confirmed', 'failed')
    ),
    launch_mode TEXT NOT NULL DEFAULT '' CHECK (
        launch_mode IN ('', 'manual', 'auto')
    ),
    launch_attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (
        launch_attempt_count BETWEEN 0 AND 2
    ),
    launch_reconcile_after TIMESTAMPTZ,
    launch_failed_at TIMESTAMPTZ,
    launched_at TIMESTAMPTZ,
    launch_failure_code TEXT NOT NULL DEFAULT '' CHECK (char_length(launch_failure_code) <= 128),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT direct_campaigns_workspace_id_unique UNIQUE (workspace_id, id),
    CONSTRAINT direct_campaigns_connection_fk
        FOREIGN KEY (workspace_id, connection_id)
        REFERENCES direct_connections(workspace_id, id) ON DELETE RESTRICT,
    CONSTRAINT direct_campaigns_dates CHECK (ends_at >= starts_at),
    CONSTRAINT direct_campaigns_provider_lifecycle CHECK (
        (
            provider_campaign_id IS NULL
            AND status IN ('draft', 'creating', 'error')
        )
        OR (
            provider_campaign_id IS NOT NULL
            AND status NOT IN ('draft', 'creating')
        )
    ),
    CONSTRAINT direct_campaigns_launch_claim CHECK (
        (
            launch_state = 'idle'
            AND launch_mode = ''
            AND launch_claimed_at IS NULL
            AND launched_at IS NULL
            AND launch_attempt_count = 0
            AND launch_reconcile_after IS NULL
            AND launch_failed_at IS NULL
        )
        OR (
            launch_state IN ('launching', 'reconciling')
            AND launch_mode IN ('manual', 'auto')
            AND launch_claimed_at IS NOT NULL
            AND launched_at IS NULL
            AND launch_reconcile_after IS NOT NULL
            AND launch_failed_at IS NULL
        )
        OR (
            launch_state = 'confirmed'
            AND launch_mode IN ('manual', 'auto')
            AND launch_claimed_at IS NOT NULL
            AND launched_at IS NOT NULL
            AND launch_reconcile_after IS NULL
            AND launch_failed_at IS NULL
            AND status IN ('active', 'suspended', 'completed', 'rejected')
        )
        OR (
            launch_state = 'failed'
            AND launch_mode IN ('manual', 'auto')
            AND launch_claimed_at IS NOT NULL
            AND launched_at IS NULL
            AND launch_attempt_count = 2
            AND launch_reconcile_after IS NULL
            AND launch_failed_at IS NOT NULL
            AND status = 'accepted'
        )
    )
);

CREATE UNIQUE INDEX idx_direct_campaigns_provider_id
    ON direct_campaigns(connection_id, provider_campaign_id)
    WHERE provider_campaign_id IS NOT NULL;
CREATE INDEX idx_direct_campaigns_workspace_updated
    ON direct_campaigns(workspace_id, updated_at DESC, id);
CREATE INDEX idx_direct_campaigns_auto_launch_candidates
    ON direct_campaigns(auto_launch_next_attempt_at, starts_at, id)
    WHERE status = 'accepted';
CREATE INDEX idx_direct_campaigns_provider_sync
    ON direct_campaigns(provider_next_check_at, status, id)
    WHERE status IN ('provider_draft', 'moderation', 'accepted', 'active', 'suspended');
CREATE INDEX idx_direct_campaigns_launch_recovery
    ON direct_campaigns(launch_reconcile_after, id)
    WHERE launch_state IN ('launching', 'reconciling');

CREATE INDEX idx_posts_direct_recent_context
    ON posts(workspace_id, channel_id, published_at DESC NULLS LAST, id DESC)
    WHERE status = 'published' AND content ~ '[^[:space:]]';

CREATE TABLE direct_auto_launch_consents (
    id TEXT PRIMARY KEY CHECK (id ~ '^dcons_[0-9a-f]{32}$'),
    workspace_id TEXT NOT NULL,
    campaign_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    actor_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    consent_version TEXT NOT NULL CHECK (consent_version = 'yandex-direct-auto-launch-v1'),
    confirmation TEXT NOT NULL CHECK (confirmation = 'АВТОЗАПУСК'),
    campaign_version BIGINT NOT NULL CHECK (campaign_version > 0),
    account_id TEXT NOT NULL CHECK (btrim(account_id) <> ''),
    provider_campaign_id BIGINT NOT NULL CHECK (provider_campaign_id > 0),
    campaign_name TEXT NOT NULL CHECK (char_length(btrim(campaign_name)) BETWEEN 1 AND 255),
    weekly_budget_minor BIGINT NOT NULL CHECK (weekly_budget_minor > 0),
    currency_code TEXT NOT NULL CHECK (currency_code ~ '^[A-Z]{3}$'),
    starts_at DATE NOT NULL,
    ends_at DATE NOT NULL,
    authorized_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    invalidated_at TIMESTAMPTZ,
    invalid_reason TEXT NOT NULL DEFAULT '' CHECK (char_length(invalid_reason) <= 128),
    consumed_at TIMESTAMPTZ,
    CONSTRAINT direct_auto_launch_consents_campaign_fk
        FOREIGN KEY (workspace_id, campaign_id)
        REFERENCES direct_campaigns(workspace_id, id) ON DELETE RESTRICT,
    CONSTRAINT direct_auto_launch_consents_connection_fk
        FOREIGN KEY (workspace_id, connection_id)
        REFERENCES direct_connections(workspace_id, id) ON DELETE RESTRICT,
    CONSTRAINT direct_auto_launch_consents_dates CHECK (ends_at >= starts_at),
    CONSTRAINT direct_auto_launch_consents_terminal_state CHECK (
        num_nonnulls(revoked_at, invalidated_at, consumed_at) <= 1
    ),
    CONSTRAINT direct_auto_launch_consents_invalid_reason CHECK (
        (invalidated_at IS NULL AND invalid_reason = '')
        OR (invalidated_at IS NOT NULL AND invalid_reason <> '')
    )
);

CREATE UNIQUE INDEX idx_direct_auto_launch_one_active
    ON direct_auto_launch_consents(workspace_id, campaign_id)
    WHERE revoked_at IS NULL AND invalidated_at IS NULL AND consumed_at IS NULL;
CREATE INDEX idx_direct_auto_launch_due
    ON direct_auto_launch_consents(authorized_at, campaign_id)
    WHERE revoked_at IS NULL AND invalidated_at IS NULL AND consumed_at IS NULL;

-- Consent snapshots are evidence. Only the lifecycle markers may change.
CREATE FUNCTION protect_direct_auto_launch_consent_snapshot() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.id IS DISTINCT FROM NEW.id
       OR OLD.workspace_id IS DISTINCT FROM NEW.workspace_id
       OR OLD.campaign_id IS DISTINCT FROM NEW.campaign_id
       OR OLD.connection_id IS DISTINCT FROM NEW.connection_id
       OR OLD.actor_user_id IS DISTINCT FROM NEW.actor_user_id
       OR OLD.consent_version IS DISTINCT FROM NEW.consent_version
       OR OLD.confirmation IS DISTINCT FROM NEW.confirmation
       OR OLD.campaign_version IS DISTINCT FROM NEW.campaign_version
       OR OLD.account_id IS DISTINCT FROM NEW.account_id
       OR OLD.provider_campaign_id IS DISTINCT FROM NEW.provider_campaign_id
       OR OLD.campaign_name IS DISTINCT FROM NEW.campaign_name
       OR OLD.weekly_budget_minor IS DISTINCT FROM NEW.weekly_budget_minor
       OR OLD.currency_code IS DISTINCT FROM NEW.currency_code
       OR OLD.starts_at IS DISTINCT FROM NEW.starts_at
       OR OLD.ends_at IS DISTINCT FROM NEW.ends_at
       OR OLD.authorized_at IS DISTINCT FROM NEW.authorized_at THEN
        RAISE EXCEPTION USING ERRCODE='55000',
            MESSAGE='direct auto-launch consent snapshots are immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER direct_auto_launch_consents_protect_snapshot
BEFORE UPDATE ON direct_auto_launch_consents
FOR EACH ROW EXECUTE FUNCTION protect_direct_auto_launch_consent_snapshot();

-- Any optimistic version or spend-critical change invalidates an outstanding
-- authorization. This protects paths added by future binaries as well as the
-- current store implementation.
CREATE FUNCTION invalidate_direct_auto_launch_consent() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.connection_id IS DISTINCT FROM NEW.connection_id
       OR OLD.name IS DISTINCT FROM NEW.name
       OR (
            OLD.provider_campaign_id IS NOT NULL
            AND OLD.provider_campaign_id IS DISTINCT FROM NEW.provider_campaign_id
       )
       OR OLD.weekly_budget_minor IS DISTINCT FROM NEW.weekly_budget_minor
       OR OLD.currency_code IS DISTINCT FROM NEW.currency_code
       OR OLD.starts_at IS DISTINCT FROM NEW.starts_at
       OR OLD.ends_at IS DISTINCT FROM NEW.ends_at
       OR OLD.version IS DISTINCT FROM NEW.version THEN
        UPDATE direct_auto_launch_consents
        SET invalidated_at = CURRENT_TIMESTAMP,
            invalid_reason = 'campaign_changed'
        WHERE workspace_id = OLD.workspace_id
          AND campaign_id = OLD.id
          AND revoked_at IS NULL
          AND invalidated_at IS NULL
          AND consumed_at IS NULL;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER direct_campaigns_invalidate_auto_launch
AFTER UPDATE ON direct_campaigns
FOR EACH ROW EXECUTE FUNCTION invalidate_direct_auto_launch_consent();

CREATE TRIGGER direct_oauth_states_active_workspace_guard
BEFORE INSERT OR UPDATE ON direct_oauth_states
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();

CREATE TRIGGER direct_connections_active_workspace_guard
BEFORE INSERT OR UPDATE ON direct_connections
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();

CREATE TRIGGER direct_campaigns_active_workspace_guard
BEFORE INSERT OR UPDATE ON direct_campaigns
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();

CREATE TRIGGER direct_auto_launch_consents_active_workspace_guard
BEFORE INSERT OR UPDATE ON direct_auto_launch_consents
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
