-- Provider lifecycle reads run outside a database transaction. These columns
-- create a short recoverable lease and an exact generation fence so two
-- replicas cannot persist provider observations out of order.
ALTER TABLE direct_campaigns
    ADD COLUMN provider_sync_claimed_at TIMESTAMPTZ,
    ADD COLUMN provider_sync_lease_expires_at TIMESTAMPTZ;

ALTER TABLE direct_campaigns
    ADD CONSTRAINT direct_campaigns_provider_sync_claim_check
    CHECK (
        (provider_sync_claimed_at IS NULL AND provider_sync_lease_expires_at IS NULL)
        OR
        (provider_sync_claimed_at IS NOT NULL
         AND provider_sync_lease_expires_at IS NOT NULL
         AND provider_sync_lease_expires_at > provider_sync_claimed_at)
    ) NOT VALID;

ALTER TABLE direct_campaigns
    VALIDATE CONSTRAINT direct_campaigns_provider_sync_claim_check;

-- BidCeiling changes delivery and must invalidate the exact same graph and
-- consent evidence as budget, targeting, creative, or keyword changes. Keep
-- the already-deployed trigger functions immutable during the expand phase and
-- add narrowly scoped companion triggers for the new column.
CREATE FUNCTION invalidate_direct_bid_ceiling_graph_evidence() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.bid_ceiling_minor IS DISTINCT FROM NEW.bid_ceiling_minor THEN
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

CREATE TRIGGER direct_campaigns_invalidate_bid_ceiling_graph
BEFORE UPDATE OF bid_ceiling_minor ON direct_campaigns
FOR EACH ROW EXECUTE FUNCTION invalidate_direct_bid_ceiling_graph_evidence();

CREATE FUNCTION invalidate_direct_bid_ceiling_auto_launch_consent() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.bid_ceiling_minor IS DISTINCT FROM NEW.bid_ceiling_minor THEN
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

CREATE TRIGGER direct_campaigns_invalidate_bid_ceiling_auto_launch
AFTER UPDATE OF bid_ceiling_minor ON direct_campaigns
FOR EACH ROW EXECUTE FUNCTION invalidate_direct_bid_ceiling_auto_launch_consent();

-- Retain the previous index until a later contract migration. The new index
-- adds rejected campaigns to the adaptive lifecycle queue without replacing a
-- structure that an older release may still rely on.
CREATE INDEX idx_direct_campaigns_provider_sync_due_v2
    ON direct_campaigns(provider_next_check_at, id)
    WHERE status IN (
        'provider_draft', 'moderation', 'accepted', 'rejected', 'active', 'suspended'
    );

CREATE INDEX idx_direct_campaigns_provider_sync_lease
    ON direct_campaigns(provider_sync_lease_expires_at, id)
    WHERE provider_sync_claimed_at IS NOT NULL;
