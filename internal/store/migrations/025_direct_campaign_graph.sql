-- Full Yandex Direct graph persistence. This migration is expand-only: legacy
-- campaign-shell rows receive safe local placeholder creative data, but no
-- graph hash, revision, verification timestamp, or launch authority.

ALTER TABLE direct_campaigns
    ADD COLUMN titles JSONB DEFAULT '["Черновик объявления"]'::jsonb,
    ADD COLUMN texts JSONB DEFAULT '["Проверьте текст объявления"]'::jsonb,
    ADD COLUMN keywords JSONB DEFAULT '["черновик"]'::jsonb,
    ADD COLUMN negative_keywords JSONB DEFAULT '[]'::jsonb,
    ADD COLUMN provider_ad_group_id BIGINT,
    ADD COLUMN provider_ad_id BIGINT,
    ADD COLUMN provider_keyword_ids JSONB DEFAULT '[]'::jsonb,
    ADD COLUMN provider_keyword_mappings JSONB DEFAULT '[]'::jsonb,
    ADD COLUMN provider_warnings JSONB DEFAULT '[]'::jsonb,
    ADD COLUMN submission_operation_id TEXT,
    ADD COLUMN submission_stage TEXT DEFAULT 'idle',
    ADD COLUMN submission_operation_marker TEXT DEFAULT '',
    ADD COLUMN submission_claimed_at TIMESTAMPTZ,
    ADD COLUMN submission_lease_expires_at TIMESTAMPTZ,
    ADD COLUMN submission_failure_code TEXT DEFAULT '',
    ADD COLUMN submission_failure_clarification TEXT DEFAULT '',
    ADD COLUMN provider_graph_hash TEXT DEFAULT '',
    ADD COLUMN provider_revision_id TEXT,
    ADD COLUMN graph_verified_at TIMESTAMPTZ,
    ADD COLUMN moderation_status TEXT DEFAULT '',
    ADD COLUMN moderation_clarification TEXT DEFAULT '',
    ADD COLUMN campaign_moderation JSONB DEFAULT '{}'::jsonb,
    ADD COLUMN ad_group_moderation JSONB DEFAULT '{}'::jsonb,
    ADD COLUMN ad_moderation JSONB DEFAULT '{}'::jsonb,
    ADD COLUMN keyword_moderation JSONB DEFAULT '[]'::jsonb;

-- PostgreSQL applies the constant DEFAULT values above to every legacy row
-- without issuing row UPDATEs. Avoiding a data UPDATE is important here:
-- archived workspaces intentionally reject child-row writes via their existing
-- active-workspace trigger.

ALTER TABLE direct_campaigns
    ADD CONSTRAINT direct_campaigns_titles_shape CHECK (
        titles IS NOT NULL
        AND jsonb_typeof(titles) = 'array'
        AND jsonb_array_length(titles) BETWEEN 1 AND 7
    ) NOT VALID,
    ADD CONSTRAINT direct_campaigns_texts_shape CHECK (
        texts IS NOT NULL
        AND jsonb_typeof(texts) = 'array'
        AND jsonb_array_length(texts) BETWEEN 1 AND 3
    ) NOT VALID,
    ADD CONSTRAINT direct_campaigns_keywords_shape CHECK (
        keywords IS NOT NULL
        AND jsonb_typeof(keywords) = 'array'
        AND jsonb_array_length(keywords) BETWEEN 1 AND 50
    ) NOT VALID,
    ADD CONSTRAINT direct_campaigns_negative_keywords_shape CHECK (
        negative_keywords IS NOT NULL
        AND jsonb_typeof(negative_keywords) = 'array'
        AND jsonb_array_length(negative_keywords) BETWEEN 0 AND 50
    ) NOT VALID,
    ADD CONSTRAINT direct_campaigns_provider_children CHECK (
        (provider_ad_group_id IS NULL OR provider_ad_group_id > 0)
        AND (provider_ad_id IS NULL OR provider_ad_id > 0)
        AND provider_keyword_ids IS NOT NULL
        AND provider_keyword_mappings IS NOT NULL
        AND jsonb_typeof(provider_keyword_ids) = 'array'
        AND jsonb_array_length(provider_keyword_ids)
            BETWEEN 0 AND jsonb_array_length(keywords)
        AND jsonb_typeof(provider_keyword_mappings) = 'array'
        AND jsonb_array_length(provider_keyword_mappings)
            BETWEEN 0 AND jsonb_array_length(keywords)
    ) NOT VALID,
    ADD CONSTRAINT direct_campaigns_provider_warnings_shape CHECK (
        provider_warnings IS NOT NULL
        AND jsonb_typeof(provider_warnings) = 'array'
        AND jsonb_array_length(provider_warnings) <= 1000
    ) NOT VALID,
    ADD CONSTRAINT direct_campaigns_submission_stage CHECK (
        submission_stage IS NOT NULL
        AND submission_stage IN (
            'idle', 'claimed', 'campaign_created', 'ad_group_created',
            'ad_created', 'keywords_created', 'graph_observed', 'verified',
            'moderation_requested', 'completed', 'reconciling', 'failed',
            'campaign_updated', 'ad_group_updated', 'ad_updated',
            'keywords_updated'
        )
    ) NOT VALID,
    ADD CONSTRAINT direct_campaigns_submission_marker CHECK (
        submission_operation_marker IS NOT NULL
        AND submission_failure_code IS NOT NULL
        AND submission_failure_clarification IS NOT NULL
        AND char_length(submission_operation_marker) <= 128
        AND char_length(submission_failure_code) <= 128
        AND char_length(submission_failure_clarification) <= 2000
    ) NOT VALID,
    ADD CONSTRAINT direct_campaigns_graph_hash CHECK (
        provider_graph_hash IS NOT NULL
        AND (provider_graph_hash = '' OR provider_graph_hash ~ '^[0-9a-f]{64}$')
    ) NOT VALID,
    ADD CONSTRAINT direct_campaigns_graph_evidence_complete CHECK (
        (
            provider_graph_hash IS NOT NULL
            AND graph_verified_at IS NULL
            AND provider_graph_hash = ''
            AND provider_revision_id IS NULL
        )
        OR (
            graph_verified_at IS NOT NULL
            AND provider_graph_hash ~ '^[0-9a-f]{64}$'
            AND provider_revision_id IS NOT NULL
            AND provider_campaign_id IS NOT NULL
            AND provider_ad_group_id IS NOT NULL
            AND provider_ad_id IS NOT NULL
            AND jsonb_array_length(provider_keyword_ids) = jsonb_array_length(keywords)
            AND jsonb_array_length(provider_keyword_mappings) = jsonb_array_length(keywords)
        )
    ) NOT VALID,
    ADD CONSTRAINT direct_campaigns_moderation_status CHECK (
        moderation_status IS NOT NULL
        AND moderation_clarification IS NOT NULL
        AND campaign_moderation IS NOT NULL
        AND ad_group_moderation IS NOT NULL
        AND ad_moderation IS NOT NULL
        AND keyword_moderation IS NOT NULL
        AND moderation_status IN ('', 'UNKNOWN', 'DRAFT', 'MODERATION', 'ACCEPTED', 'REJECTED')
        AND char_length(moderation_clarification) <= 2000
        AND jsonb_typeof(campaign_moderation) = 'object'
        AND jsonb_typeof(ad_group_moderation) = 'object'
        AND jsonb_typeof(ad_moderation) = 'object'
        AND jsonb_typeof(keyword_moderation) = 'array'
        AND jsonb_array_length(keyword_moderation) <= 50
    ) NOT VALID;

CREATE UNIQUE INDEX idx_direct_campaigns_submission_marker
    ON direct_campaigns(connection_id, submission_operation_marker)
    WHERE submission_operation_marker <> '';
CREATE INDEX idx_direct_campaigns_submission_recovery
    ON direct_campaigns(submission_lease_expires_at, id)
    WHERE submission_operation_id IS NOT NULL
      AND submission_stage NOT IN ('completed', 'failed', 'idle');
CREATE INDEX idx_direct_campaigns_graph_moderation
    ON direct_campaigns(provider_next_check_at, moderation_status, id)
    WHERE graph_verified_at IS NOT NULL
      AND moderation_status IN ('', 'UNKNOWN', 'DRAFT', 'MODERATION');

CREATE TABLE direct_campaign_revisions (
    id TEXT PRIMARY KEY CHECK (id ~ '^drev_[0-9a-f]{32}$'),
    workspace_id TEXT NOT NULL,
    campaign_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    revision_number BIGINT NOT NULL CHECK (revision_number > 0),
    campaign_version BIGINT NOT NULL CHECK (campaign_version > 0),
    graph_version TEXT NOT NULL CHECK (
        char_length(btrim(graph_version)) BETWEEN 1 AND 128
    ),
    desired_graph JSONB NOT NULL CHECK (jsonb_typeof(desired_graph) = 'object'),
    observed_graph JSONB NOT NULL CHECK (jsonb_typeof(observed_graph) = 'object'),
    graph_hash TEXT NOT NULL CHECK (graph_hash ~ '^[0-9a-f]{64}$'),
    provider_campaign_id BIGINT NOT NULL CHECK (provider_campaign_id > 0),
    provider_ad_group_id BIGINT NOT NULL CHECK (provider_ad_group_id > 0),
    provider_ad_id BIGINT NOT NULL CHECK (provider_ad_id > 0),
    provider_keyword_mappings JSONB NOT NULL CHECK (
        jsonb_typeof(provider_keyword_mappings) = 'array'
        AND jsonb_array_length(provider_keyword_mappings) BETWEEN 1 AND 50
    ),
    provider_warnings JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (
        jsonb_typeof(provider_warnings) = 'array'
    ),
    moderation_status TEXT NOT NULL DEFAULT '' CHECK (
        moderation_status IN ('', 'UNKNOWN', 'DRAFT', 'MODERATION', 'ACCEPTED', 'REJECTED')
    ),
    moderation_clarification TEXT NOT NULL DEFAULT ''
        CHECK (char_length(moderation_clarification) <= 2000),
    actor_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    observed_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT direct_campaign_revisions_campaign_fk
        FOREIGN KEY (workspace_id, campaign_id)
        REFERENCES direct_campaigns(workspace_id, id) ON DELETE RESTRICT,
    CONSTRAINT direct_campaign_revisions_connection_fk
        FOREIGN KEY (workspace_id, connection_id)
        REFERENCES direct_connections(workspace_id, id) ON DELETE RESTRICT,
    CONSTRAINT direct_campaign_revisions_scope_unique
        UNIQUE (workspace_id, campaign_id, id),
    CONSTRAINT direct_campaign_revisions_number_unique
        UNIQUE (workspace_id, campaign_id, revision_number)
);

CREATE INDEX idx_direct_campaign_revisions_campaign
    ON direct_campaign_revisions(workspace_id, campaign_id, revision_number DESC);

CREATE TABLE direct_provider_operations (
    id TEXT PRIMARY KEY CHECK (id ~ '^dpop_[0-9a-f]{32}$'),
    workspace_id TEXT NOT NULL,
    campaign_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    actor_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    operation_kind TEXT NOT NULL CHECK (operation_kind IN ('submission', 'update')),
    operation_marker TEXT NOT NULL CHECK (
        operation_marker ~ '^[A-Za-z0-9_-]{16,128}$'
    ),
    expected_campaign_version BIGINT NOT NULL CHECK (expected_campaign_version > 0),
    expected_graph_hash TEXT NOT NULL DEFAULT '' CHECK (
        expected_graph_hash = '' OR expected_graph_hash ~ '^[0-9a-f]{64}$'
    ),
    expected_revision_id TEXT,
    stage TEXT NOT NULL CHECK (
        stage IN (
            'claimed', 'campaign_created', 'ad_group_created', 'ad_created',
            'keywords_created', 'graph_observed', 'verified',
            'moderation_requested', 'completed', 'reconciling', 'failed',
            'campaign_updated', 'ad_group_updated', 'ad_updated',
            'keywords_updated'
        )
    ),
    desired_graph JSONB NOT NULL CHECK (jsonb_typeof(desired_graph) = 'object'),
    observed_graph JSONB CHECK (
        observed_graph IS NULL OR jsonb_typeof(observed_graph) = 'object'
    ),
    provider_campaign_id BIGINT CHECK (provider_campaign_id IS NULL OR provider_campaign_id > 0),
    provider_ad_group_id BIGINT CHECK (provider_ad_group_id IS NULL OR provider_ad_group_id > 0),
    provider_ad_id BIGINT CHECK (provider_ad_id IS NULL OR provider_ad_id > 0),
    provider_keyword_mappings JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (
        jsonb_typeof(provider_keyword_mappings) = 'array'
        AND jsonb_array_length(provider_keyword_mappings) <= 50
    ),
    provider_warnings JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (
        jsonb_typeof(provider_warnings) = 'array'
    ),
    graph_hash TEXT NOT NULL DEFAULT '' CHECK (
        graph_hash = '' OR graph_hash ~ '^[0-9a-f]{64}$'
    ),
    revision_id TEXT,
    last_provider_error_code TEXT NOT NULL DEFAULT ''
        CHECK (char_length(last_provider_error_code) <= 128),
    last_provider_clarification TEXT NOT NULL DEFAULT ''
        CHECK (char_length(last_provider_clarification) <= 2000),
    claimed_at TIMESTAMPTZ NOT NULL,
    lease_expires_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT direct_provider_operations_campaign_fk
        FOREIGN KEY (workspace_id, campaign_id)
        REFERENCES direct_campaigns(workspace_id, id) ON DELETE RESTRICT,
    CONSTRAINT direct_provider_operations_connection_fk
        FOREIGN KEY (workspace_id, connection_id)
        REFERENCES direct_connections(workspace_id, id) ON DELETE RESTRICT,
    CONSTRAINT direct_provider_operations_expected_revision_fk
        FOREIGN KEY (workspace_id, campaign_id, expected_revision_id)
        REFERENCES direct_campaign_revisions(workspace_id, campaign_id, id)
        ON DELETE RESTRICT,
    CONSTRAINT direct_provider_operations_revision_fk
        FOREIGN KEY (workspace_id, campaign_id, revision_id)
        REFERENCES direct_campaign_revisions(workspace_id, campaign_id, id)
        ON DELETE RESTRICT,
    CONSTRAINT direct_provider_operations_marker_unique
        UNIQUE (connection_id, operation_marker),
    CONSTRAINT direct_provider_operations_scope_unique
        UNIQUE (workspace_id, campaign_id, id),
    CONSTRAINT direct_provider_operations_lease CHECK (
        lease_expires_at >= claimed_at
        AND (completed_at IS NULL OR completed_at >= claimed_at)
    )
);

CREATE UNIQUE INDEX idx_direct_provider_operations_one_live
    ON direct_provider_operations(workspace_id, campaign_id)
    WHERE completed_at IS NULL AND stage NOT IN ('completed', 'failed');
CREATE INDEX idx_direct_provider_operations_recovery
    ON direct_provider_operations(lease_expires_at, id)
    WHERE completed_at IS NULL AND stage NOT IN ('completed', 'failed');

CREATE TABLE direct_provider_operation_journal (
    id BIGSERIAL PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    campaign_id TEXT NOT NULL,
    operation_id TEXT NOT NULL,
    stage TEXT NOT NULL CHECK (char_length(stage) BETWEEN 1 AND 64),
    snapshot JSONB NOT NULL CHECK (jsonb_typeof(snapshot) = 'object'),
    recorded_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT direct_provider_operation_journal_campaign_fk
        FOREIGN KEY (workspace_id, campaign_id)
        REFERENCES direct_campaigns(workspace_id, id) ON DELETE RESTRICT,
    CONSTRAINT direct_provider_operation_journal_operation_fk
        FOREIGN KEY (workspace_id, campaign_id, operation_id)
        REFERENCES direct_provider_operations(workspace_id, campaign_id, id)
        ON DELETE RESTRICT
);

CREATE INDEX idx_direct_provider_operation_journal_campaign
    ON direct_provider_operation_journal(workspace_id, campaign_id, id);

ALTER TABLE direct_campaigns
    ADD CONSTRAINT direct_campaigns_current_revision_fk
        FOREIGN KEY (workspace_id, id, provider_revision_id)
        REFERENCES direct_campaign_revisions(workspace_id, campaign_id, id)
        DEFERRABLE INITIALLY IMMEDIATE,
    ADD CONSTRAINT direct_campaigns_submission_operation_fk
        FOREIGN KEY (workspace_id, id, submission_operation_id)
        REFERENCES direct_provider_operations(workspace_id, campaign_id, id)
        DEFERRABLE INITIALLY IMMEDIATE;

CREATE TABLE direct_auto_launch_consents_v2 (
    id TEXT PRIMARY KEY CHECK (id ~ '^dcons_[0-9a-f]{32}$'),
    workspace_id TEXT NOT NULL,
    campaign_id TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    actor_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    consent_version TEXT NOT NULL CHECK (
        consent_version = 'yandex-direct-auto-launch-v2'
    ),
    confirmation TEXT NOT NULL CHECK (confirmation = 'АВТОЗАПУСК'),
    campaign_version BIGINT NOT NULL CHECK (campaign_version > 0),
    account_id TEXT NOT NULL CHECK (btrim(account_id) <> ''),
    provider_campaign_id BIGINT NOT NULL CHECK (provider_campaign_id > 0),
    campaign_name TEXT NOT NULL CHECK (char_length(btrim(campaign_name)) BETWEEN 1 AND 255),
    weekly_budget_minor BIGINT NOT NULL CHECK (weekly_budget_minor > 0),
    currency_code TEXT NOT NULL CHECK (currency_code ~ '^[A-Z]{3}$'),
    starts_at DATE NOT NULL,
    ends_at DATE NOT NULL,
    expected_graph_hash TEXT NOT NULL CHECK (
        expected_graph_hash ~ '^[0-9a-f]{64}$'
    ),
    expected_revision_id TEXT NOT NULL,
    authorized_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    invalidated_at TIMESTAMPTZ,
    invalid_reason TEXT NOT NULL DEFAULT '' CHECK (char_length(invalid_reason) <= 128),
    consumed_at TIMESTAMPTZ,
    CONSTRAINT direct_auto_launch_consents_v2_campaign_fk
        FOREIGN KEY (workspace_id, campaign_id)
        REFERENCES direct_campaigns(workspace_id, id) ON DELETE RESTRICT,
    CONSTRAINT direct_auto_launch_consents_v2_connection_fk
        FOREIGN KEY (workspace_id, connection_id)
        REFERENCES direct_connections(workspace_id, id) ON DELETE RESTRICT,
    CONSTRAINT direct_auto_launch_consents_v2_revision_fk
        FOREIGN KEY (workspace_id, campaign_id, expected_revision_id)
        REFERENCES direct_campaign_revisions(workspace_id, campaign_id, id)
        ON DELETE RESTRICT,
    CONSTRAINT direct_auto_launch_consents_v2_dates CHECK (ends_at >= starts_at),
    CONSTRAINT direct_auto_launch_consents_v2_terminal_state CHECK (
        num_nonnulls(revoked_at, invalidated_at, consumed_at) <= 1
    ),
    CONSTRAINT direct_auto_launch_consents_v2_invalid_reason CHECK (
        (invalidated_at IS NULL AND invalid_reason = '')
        OR (invalidated_at IS NOT NULL AND invalid_reason <> '')
    )
);

CREATE UNIQUE INDEX idx_direct_auto_launch_v2_one_active
    ON direct_auto_launch_consents_v2(workspace_id, campaign_id)
    WHERE revoked_at IS NULL AND invalidated_at IS NULL AND consumed_at IS NULL;
CREATE INDEX idx_direct_auto_launch_v2_due
    ON direct_auto_launch_consents_v2(authorized_at, campaign_id)
    WHERE revoked_at IS NULL AND invalidated_at IS NULL AND consumed_at IS NULL;

CREATE VIEW direct_auto_launch_consent_evidence AS
SELECT id, workspace_id, campaign_id, connection_id, actor_user_id,
       consent_version, confirmation, campaign_version, account_id,
       provider_campaign_id, campaign_name, weekly_budget_minor, currency_code,
       starts_at, ends_at, ''::TEXT AS expected_graph_hash,
       NULL::TEXT AS expected_revision_id, authorized_at, revoked_at,
       CASE
           WHEN revoked_at IS NULL AND invalidated_at IS NULL AND consumed_at IS NULL
               THEN authorized_at
           ELSE invalidated_at
       END AS invalidated_at,
       CASE
           WHEN revoked_at IS NULL AND invalidated_at IS NULL AND consumed_at IS NULL
               THEN 'legacy_consent_version'
           ELSE invalid_reason
       END AS invalid_reason,
       consumed_at
FROM direct_auto_launch_consents
UNION ALL
SELECT id, workspace_id, campaign_id, connection_id, actor_user_id,
       consent_version, confirmation, campaign_version, account_id,
       provider_campaign_id, campaign_name, weekly_budget_minor, currency_code,
       starts_at, ends_at, expected_graph_hash, expected_revision_id,
       authorized_at, revoked_at, invalidated_at, invalid_reason, consumed_at
FROM direct_auto_launch_consents_v2;

UPDATE direct_auto_launch_consents c
SET invalidated_at = CURRENT_TIMESTAMP,
    invalid_reason = 'legacy_consent_version'
FROM workspaces w
WHERE c.workspace_id = w.id
  AND w.archived_at IS NULL
  AND c.revoked_at IS NULL
  AND c.invalidated_at IS NULL
  AND c.consumed_at IS NULL;

CREATE FUNCTION invalidate_legacy_direct_auto_launch_consent() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.revoked_at IS NULL
       AND NEW.invalidated_at IS NULL
       AND NEW.consumed_at IS NULL THEN
        UPDATE direct_auto_launch_consents
        SET invalidated_at = CURRENT_TIMESTAMP,
            invalid_reason = 'legacy_consent_version'
        WHERE id = NEW.id
          AND revoked_at IS NULL
          AND invalidated_at IS NULL
          AND consumed_at IS NULL;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER direct_auto_launch_consents_reject_legacy_active
AFTER INSERT ON direct_auto_launch_consents
FOR EACH ROW EXECUTE FUNCTION invalidate_legacy_direct_auto_launch_consent();

CREATE FUNCTION protect_direct_auto_launch_consent_v2_snapshot() RETURNS trigger
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
       OR OLD.expected_graph_hash IS DISTINCT FROM NEW.expected_graph_hash
       OR OLD.expected_revision_id IS DISTINCT FROM NEW.expected_revision_id
       OR OLD.authorized_at IS DISTINCT FROM NEW.authorized_at THEN
        RAISE EXCEPTION USING ERRCODE='55000',
            MESSAGE='direct auto-launch consent v2 snapshots are immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER direct_auto_launch_consents_v2_protect_snapshot
BEFORE UPDATE ON direct_auto_launch_consents_v2
FOR EACH ROW EXECUTE FUNCTION protect_direct_auto_launch_consent_v2_snapshot();

CREATE FUNCTION invalidate_direct_campaign_graph_evidence() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.connection_id IS DISTINCT FROM NEW.connection_id
       OR OLD.name IS DISTINCT FROM NEW.name
       OR OLD.landing_url IS DISTINCT FROM NEW.landing_url
       OR OLD.regions IS DISTINCT FROM NEW.regions
       OR OLD.weekly_budget_minor IS DISTINCT FROM NEW.weekly_budget_minor
       OR OLD.currency_code IS DISTINCT FROM NEW.currency_code
       OR OLD.starts_at IS DISTINCT FROM NEW.starts_at
       OR OLD.ends_at IS DISTINCT FROM NEW.ends_at
       OR OLD.titles IS DISTINCT FROM NEW.titles
       OR OLD.texts IS DISTINCT FROM NEW.texts
       OR OLD.keywords IS DISTINCT FROM NEW.keywords
       OR OLD.negative_keywords IS DISTINCT FROM NEW.negative_keywords
       OR OLD.provider_campaign_id IS DISTINCT FROM NEW.provider_campaign_id
       OR OLD.provider_ad_group_id IS DISTINCT FROM NEW.provider_ad_group_id
       OR OLD.provider_ad_id IS DISTINCT FROM NEW.provider_ad_id
       OR OLD.provider_keyword_ids IS DISTINCT FROM NEW.provider_keyword_ids
       OR OLD.provider_keyword_mappings IS DISTINCT FROM NEW.provider_keyword_mappings THEN
        NEW.provider_graph_hash := '';
        NEW.provider_revision_id := NULL;
        NEW.graph_verified_at := NULL;
        NEW.moderation_status := '';
        NEW.moderation_clarification := '';
        NEW.campaign_moderation := '{}'::jsonb;
        NEW.ad_group_moderation := '{}'::jsonb;
        NEW.ad_moderation := '{}'::jsonb;
        NEW.keyword_moderation := '[]'::jsonb;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER direct_campaigns_invalidate_graph_evidence
BEFORE UPDATE ON direct_campaigns
FOR EACH ROW EXECUTE FUNCTION invalidate_direct_campaign_graph_evidence();

CREATE FUNCTION invalidate_direct_auto_launch_consent_v2() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.connection_id IS DISTINCT FROM NEW.connection_id
       OR OLD.name IS DISTINCT FROM NEW.name
       OR OLD.landing_url IS DISTINCT FROM NEW.landing_url
       OR OLD.regions IS DISTINCT FROM NEW.regions
       OR OLD.provider_campaign_id IS DISTINCT FROM NEW.provider_campaign_id
       OR OLD.provider_ad_group_id IS DISTINCT FROM NEW.provider_ad_group_id
       OR OLD.provider_ad_id IS DISTINCT FROM NEW.provider_ad_id
       OR OLD.provider_keyword_ids IS DISTINCT FROM NEW.provider_keyword_ids
       OR OLD.provider_keyword_mappings IS DISTINCT FROM NEW.provider_keyword_mappings
       OR OLD.titles IS DISTINCT FROM NEW.titles
       OR OLD.texts IS DISTINCT FROM NEW.texts
       OR OLD.keywords IS DISTINCT FROM NEW.keywords
       OR OLD.negative_keywords IS DISTINCT FROM NEW.negative_keywords
       OR OLD.provider_graph_hash IS DISTINCT FROM NEW.provider_graph_hash
       OR OLD.provider_revision_id IS DISTINCT FROM NEW.provider_revision_id
       OR OLD.graph_verified_at IS DISTINCT FROM NEW.graph_verified_at
       OR OLD.moderation_status IS DISTINCT FROM NEW.moderation_status
       OR OLD.weekly_budget_minor IS DISTINCT FROM NEW.weekly_budget_minor
       OR OLD.currency_code IS DISTINCT FROM NEW.currency_code
       OR OLD.starts_at IS DISTINCT FROM NEW.starts_at
       OR OLD.ends_at IS DISTINCT FROM NEW.ends_at
       OR OLD.version IS DISTINCT FROM NEW.version THEN
        UPDATE direct_auto_launch_consents_v2
        SET invalidated_at = CURRENT_TIMESTAMP,
            invalid_reason = 'campaign_graph_changed'
        WHERE workspace_id = OLD.workspace_id
          AND campaign_id = OLD.id
          AND revoked_at IS NULL
          AND invalidated_at IS NULL
          AND consumed_at IS NULL;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER direct_campaigns_invalidate_auto_launch_v2
AFTER UPDATE ON direct_campaigns
FOR EACH ROW EXECUTE FUNCTION invalidate_direct_auto_launch_consent_v2();

CREATE FUNCTION protect_direct_campaign_revision() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION USING ERRCODE='55000',
        MESSAGE='direct campaign revisions are immutable';
END;
$$;

CREATE TRIGGER direct_campaign_revisions_immutable
BEFORE UPDATE OR DELETE ON direct_campaign_revisions
FOR EACH ROW EXECUTE FUNCTION protect_direct_campaign_revision();

CREATE FUNCTION protect_direct_provider_operation_journal() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION USING ERRCODE='55000',
        MESSAGE='direct provider operation journal is immutable';
END;
$$;

CREATE TRIGGER direct_provider_operation_journal_immutable
BEFORE UPDATE OR DELETE ON direct_provider_operation_journal
FOR EACH ROW EXECUTE FUNCTION protect_direct_provider_operation_journal();

CREATE TRIGGER direct_campaign_revisions_active_workspace_guard
BEFORE INSERT OR UPDATE ON direct_campaign_revisions
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();

CREATE TRIGGER direct_provider_operations_active_workspace_guard
BEFORE INSERT OR UPDATE ON direct_provider_operations
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();

CREATE TRIGGER direct_provider_operation_journal_active_workspace_guard
BEFORE INSERT OR UPDATE ON direct_provider_operation_journal
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();

CREATE TRIGGER direct_auto_launch_consents_v2_active_workspace_guard
BEFORE INSERT OR UPDATE ON direct_auto_launch_consents_v2
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
