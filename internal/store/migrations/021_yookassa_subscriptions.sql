-- Public billing catalog v2 and durable YooKassa subscription lifecycle.
-- Existing plan snapshots stay immutable; only their rollout visibility changes.

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM workspace_subscriptions
        WHERE plan_version=1 AND plan_code<>'free'
    ) THEN
        RAISE EXCEPTION USING ERRCODE='55000',
            MESSAGE='billing migration requires operator review of non-Free legacy subscriptions';
    END IF;
END;
$$;

UPDATE billing_plan_versions
SET public=FALSE, available=FALSE
WHERE available OR public;

INSERT INTO billing_plan_versions(
    plan_code, version, catalog_version, name, description, currency_code,
    monthly_price_minor, billing_interval, public, available
) VALUES
    ('free', 2, DATE '2026-07-22', 'Free', 'Бесплатно навсегда для одного канала', 'RUB', 0, 'month', TRUE, TRUE),
    ('solo', 2, DATE '2026-07-22', 'Автор', 'Для одного автора и его каналов', 'RUB', 99000, 'month', TRUE, TRUE),
    ('pro',  2, DATE '2026-07-22', 'Команда', 'Для небольшой контент-команды', 'RUB', 249000, 'month', TRUE, TRUE)
ON CONFLICT (plan_code, version) DO NOTHING;

DROP TRIGGER IF EXISTS billing_plan_entitlements_protect_snapshot ON billing_plan_entitlements;

INSERT INTO billing_plan_entitlements(
    plan_code, plan_version, entitlement_key, usage_metric,
    limit_value, unit, period, unit_scale, hard_limit
) VALUES
    ('free', 2, 'channels',             'channels',             1,          'channel',                 'current', 1, TRUE),
    ('free', 2, 'seats',                'seats',                1,          'seat',                    'current', 1, TRUE),
    ('free', 2, 'storage_bytes',        'storage_bytes',        1073741824, 'byte',                    'current', 1, TRUE),
    ('free', 2, 'ai_images_monthly',    'ai_image_credits',     0,          'medium_equivalent_image', 'month',   9, TRUE),
    ('free', 2, 'ai_research_monthly',  'ai_research_requests', 0,          'request',                 'month',   1, TRUE),
    ('free', 2, 'ai_format_monthly',    'ai_format_requests',   0,          'request',                 'month',   1, TRUE),

    ('solo', 2, 'channels',             'channels',             3,          'channel',                 'current', 1, TRUE),
    ('solo', 2, 'seats',                'seats',                1,          'seat',                    'current', 1, TRUE),
    ('solo', 2, 'storage_bytes',        'storage_bytes',        3221225472, 'byte',                    'current', 1, TRUE),
    ('solo', 2, 'ai_images_monthly',    'ai_image_credits',     12,         'medium_equivalent_image', 'month',   9, TRUE),
    ('solo', 2, 'ai_research_monthly',  'ai_research_requests', 8,          'request',                 'month',   1, TRUE),
    ('solo', 2, 'ai_format_monthly',    'ai_format_requests',   40,         'request',                 'month',   1, TRUE),

    ('pro', 2, 'channels',              'channels',             10,          'channel',                 'current', 1, TRUE),
    ('pro', 2, 'seats',                 'seats',                5,           'seat',                    'current', 1, TRUE),
    ('pro', 2, 'storage_bytes',         'storage_bytes',        10737418240, 'byte',                    'current', 1, TRUE),
    ('pro', 2, 'ai_images_monthly',     'ai_image_credits',     30,          'medium_equivalent_image', 'month',   9, TRUE),
    ('pro', 2, 'ai_research_monthly',   'ai_research_requests', 16,          'request',                 'month',   1, TRUE),
    ('pro', 2, 'ai_format_monthly',     'ai_format_requests',   100,         'request',                 'month',   1, TRUE)
ON CONFLICT (plan_code, plan_version, entitlement_key) DO NOTHING;

CREATE TRIGGER billing_plan_entitlements_protect_snapshot
BEFORE INSERT OR UPDATE OR DELETE ON billing_plan_entitlements
FOR EACH ROW EXECUTE FUNCTION protect_billing_plan_entitlement_snapshot();

-- Move only already-compliant workspaces to the new hard-limited Free plan.
-- Over-limit workspaces remain on the hidden legacy snapshot until their
-- footprint is reduced; this avoids a surprise lockout during the rollout.
UPDATE workspace_subscriptions
SET plan_version=2, status='active', updated_at=CURRENT_TIMESTAMP
WHERE plan_code='free' AND plan_version=1
  AND workspace_id IN (SELECT id FROM workspaces WHERE archived_at IS NULL)
  AND (SELECT count(*) FROM channels c
       WHERE c.workspace_id=workspace_subscriptions.workspace_id AND c.active=TRUE) <= 1
  AND (SELECT count(*) FROM workspace_members m
       WHERE m.workspace_id=workspace_subscriptions.workspace_id) <= 1
  AND COALESCE((SELECT u.total_bytes FROM workspace_media_usage u
                WHERE u.workspace_id=workspace_subscriptions.workspace_id),0) <= 1073741824;

CREATE FUNCTION create_free_v2_subscription_for_workspace() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO workspace_subscriptions(
        workspace_id, plan_code, plan_version, status, started_at, updated_at
    ) VALUES (
        NEW.id, 'free', 2, 'active', NEW.created_at, NEW.created_at
    ) ON CONFLICT (workspace_id) DO NOTHING;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS workspaces_create_free_subscription ON workspaces;
CREATE TRIGGER workspaces_create_free_subscription
AFTER INSERT ON workspaces
FOR EACH ROW EXECUTE FUNCTION create_free_v2_subscription_for_workspace();

CREATE TABLE billing_subscription_periods (
    id BIGSERIAL PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    plan_code TEXT NOT NULL,
    plan_version INTEGER NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'active', 'completed', 'failed')),
    period_start TIMESTAMPTZ NOT NULL,
    period_end TIMESTAMPTZ NOT NULL,
    list_price_minor BIGINT NOT NULL CHECK (list_price_minor >= 0),
    charged_price_minor BIGINT NOT NULL CHECK (charged_price_minor >= 0),
    currency_code TEXT NOT NULL CHECK (currency_code ~ '^[A-Z]{3}$'),
    discount_basis_points INTEGER NOT NULL DEFAULT 0 CHECK (discount_basis_points BETWEEN 0 AND 10000),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (period_end > period_start),
    CONSTRAINT billing_subscription_periods_plan_fk
        FOREIGN KEY (plan_code, plan_version)
        REFERENCES billing_plan_versions(plan_code, version) ON DELETE RESTRICT,
    UNIQUE (workspace_id, period_start, period_end),
    UNIQUE (id, workspace_id)
);

CREATE UNIQUE INDEX idx_billing_periods_one_active
    ON billing_subscription_periods(workspace_id) WHERE status='active';
CREATE INDEX idx_billing_periods_workspace_history
    ON billing_subscription_periods(workspace_id, period_start DESC, id DESC);

CREATE FUNCTION protect_billing_period_snapshot() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION USING ERRCODE='55000', MESSAGE='billing periods are immutable';
    END IF;
    IF OLD.workspace_id IS DISTINCT FROM NEW.workspace_id
       OR OLD.plan_code IS DISTINCT FROM NEW.plan_code
       OR OLD.plan_version IS DISTINCT FROM NEW.plan_version
       OR OLD.period_start IS DISTINCT FROM NEW.period_start
       OR OLD.period_end IS DISTINCT FROM NEW.period_end
       OR OLD.list_price_minor IS DISTINCT FROM NEW.list_price_minor
       OR OLD.charged_price_minor IS DISTINCT FROM NEW.charged_price_minor
       OR OLD.currency_code IS DISTINCT FROM NEW.currency_code
       OR OLD.discount_basis_points IS DISTINCT FROM NEW.discount_basis_points
       OR OLD.created_at IS DISTINCT FROM NEW.created_at THEN
        RAISE EXCEPTION USING ERRCODE='55000', MESSAGE='billing period snapshot fields are immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER billing_subscription_periods_protect_snapshot
BEFORE UPDATE OR DELETE ON billing_subscription_periods
FOR EACH ROW EXECUTE FUNCTION protect_billing_period_snapshot();

CREATE TABLE billing_subscription_contracts (
    workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE RESTRICT,
    payer_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    provider TEXT NOT NULL DEFAULT 'yookassa' CHECK (provider='yookassa'),
    status TEXT NOT NULL CHECK (status IN ('active', 'past_due', 'ended')),
    payment_method_id TEXT NOT NULL DEFAULT '',
    current_period_id BIGINT,
    cancel_at_period_end BOOLEAN NOT NULL DEFAULT FALSE,
    next_charge_at TIMESTAMPTZ,
    grace_until TIMESTAMPTZ,
    retention_offer_used BOOLEAN NOT NULL DEFAULT FALSE,
    next_period_discount_basis_points INTEGER NOT NULL DEFAULT 0
        CHECK (next_period_discount_basis_points BETWEEN 0 AND 5000),
    renewal_attempts INTEGER NOT NULL DEFAULT 0 CHECK (renewal_attempts >= 0),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (status='ended' OR payment_method_id <> '' OR cancel_at_period_end=TRUE),
    FOREIGN KEY (current_period_id, workspace_id)
        REFERENCES billing_subscription_periods(id, workspace_id) ON DELETE RESTRICT
);

CREATE INDEX idx_billing_contracts_due
    ON billing_subscription_contracts(next_charge_at, workspace_id)
    WHERE status IN ('active','past_due');

CREATE TABLE billing_payment_attempts (
    id TEXT PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    requested_by_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    period_id BIGINT,
    purpose TEXT NOT NULL CHECK (purpose IN ('checkout','renewal')),
    attempt_number INTEGER NOT NULL DEFAULT 1 CHECK (attempt_number > 0),
    idempotency_key TEXT NOT NULL UNIQUE CHECK (idempotency_key ~ '^[0-9a-f-]{36}$'),
    provider_payment_id TEXT UNIQUE,
    plan_code TEXT NOT NULL,
    plan_version INTEGER NOT NULL,
    amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
    currency_code TEXT NOT NULL CHECK (currency_code ~ '^[A-Z]{3}$'),
    status TEXT NOT NULL CHECK (status IN ('prepared','pending','succeeded','canceled','failed','manual_review')),
    confirmation_url TEXT NOT NULL DEFAULT '',
    requested_period_start TIMESTAMPTZ,
    requested_period_end TIMESTAMPTZ,
    error_code TEXT NOT NULL DEFAULT '',
    provider_description TEXT NOT NULL CHECK (btrim(provider_description) <> ''),
    provider_return_url TEXT NOT NULL DEFAULT '',
    payment_method_snapshot TEXT NOT NULL DEFAULT '',
    discount_basis_points INTEGER NOT NULL DEFAULT 0 CHECK (discount_basis_points BETWEEN 0 AND 5000),
    provider_create_started_at TIMESTAMPTZ,
    create_deadline TIMESTAMPTZ NOT NULL,
    next_attempt_at TIMESTAMPTZ NOT NULL,
    create_attempts INTEGER NOT NULL DEFAULT 0 CHECK (create_attempts >= 0),
    status_check_attempts INTEGER NOT NULL DEFAULT 0 CHECK (status_check_attempts >= 0),
    worker_lease_token TEXT NOT NULL DEFAULT '',
    worker_lease_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT billing_payment_attempts_plan_fk
        FOREIGN KEY (plan_code, plan_version)
        REFERENCES billing_plan_versions(plan_code, version) ON DELETE RESTRICT,
    CHECK ((requested_period_start IS NULL) = (requested_period_end IS NULL)),
    CHECK (requested_period_end IS NULL OR requested_period_end > requested_period_start),
    CHECK ((purpose='checkout' AND provider_return_url<>'' AND payment_method_snapshot='')
        OR (purpose='renewal' AND provider_return_url=''
            AND (status<>'prepared' OR payment_method_snapshot<>''))),
    CHECK (create_deadline > created_at),
    FOREIGN KEY (period_id, workspace_id)
        REFERENCES billing_subscription_periods(id, workspace_id) ON DELETE RESTRICT,
    UNIQUE (id, workspace_id, requested_by_user_id, plan_code, plan_version, amount_minor, currency_code)
);

CREATE UNIQUE INDEX idx_billing_attempts_one_open_checkout
    ON billing_payment_attempts(workspace_id)
    WHERE purpose='checkout' AND status IN ('prepared','pending','manual_review');
CREATE UNIQUE INDEX idx_billing_attempts_renewal_cycle
    ON billing_payment_attempts(workspace_id, requested_period_start, attempt_number)
    WHERE purpose='renewal';
-- An unresolved provider outcome from any earlier cycle must stop all future
-- automatic debits for the workspace until an operator resolves it. A new
-- explicit checkout is intentionally still allowed.
CREATE UNIQUE INDEX idx_billing_attempts_one_open_renewal
    ON billing_payment_attempts(workspace_id)
    WHERE purpose='renewal' AND status IN ('prepared','pending','manual_review');
CREATE INDEX idx_billing_attempts_pending_provider
    ON billing_payment_attempts(updated_at, id)
    WHERE status='pending' AND provider_payment_id IS NOT NULL;

-- An initial checkout may save a payment method only after a separate,
-- explicit recurring-payment consent. The exact text and commercial snapshot
-- are retained independently from mutable UI copy and from the contract.
CREATE TABLE billing_recurring_consents (
    payment_attempt_id TEXT PRIMARY KEY
        REFERENCES billing_payment_attempts(id) ON DELETE RESTRICT,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    actor_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    consent_version TEXT NOT NULL CHECK (consent_version='yookassa-recurring-v2'),
    consent_text TEXT NOT NULL CHECK (consent_text <> ''),
    terms_version TEXT NOT NULL CHECK (terms_version='2026-07-22'),
    terms_url TEXT NOT NULL CHECK (terms_url='https://maxposty.ru/terms/'),
    plan_code TEXT NOT NULL,
    plan_version INTEGER NOT NULL,
    monthly_price_minor BIGINT NOT NULL CHECK (monthly_price_minor > 0),
    currency_code TEXT NOT NULL CHECK (currency_code ~ '^[A-Z]{3}$'),
    accepted_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT billing_recurring_consents_plan_fk
        FOREIGN KEY (plan_code, plan_version)
        REFERENCES billing_plan_versions(plan_code, version) ON DELETE RESTRICT,
    FOREIGN KEY (payment_attempt_id, workspace_id, actor_user_id, plan_code, plan_version,
                 monthly_price_minor, currency_code)
        REFERENCES billing_payment_attempts(id, workspace_id, requested_by_user_id, plan_code,
                                            plan_version, amount_minor, currency_code)
        ON DELETE RESTRICT
);

CREATE FUNCTION protect_billing_recurring_consent() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION USING ERRCODE='55000', MESSAGE='recurring payment consents are immutable';
    RETURN NULL;
END;
$$;

CREATE TRIGGER billing_recurring_consents_protect
BEFORE UPDATE OR DELETE ON billing_recurring_consents
FOR EACH ROW EXECUTE FUNCTION protect_billing_recurring_consent();

CREATE TABLE billing_webhook_receipts (
    dedupe_key TEXT PRIMARY KEY CHECK (dedupe_key ~ '^[0-9a-f]{64}$'),
    event_type TEXT NOT NULL CHECK (event_type ~ '^[a-z.]{1,64}$'),
    object_id TEXT NOT NULL CHECK (object_id <> ''),
    result TEXT NOT NULL CHECK (result IN ('processed','ignored','failed')),
    received_at TIMESTAMPTZ NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_billing_webhook_receipts_received
    ON billing_webhook_receipts(received_at DESC);

CREATE TABLE billing_retention_offers (
    token_hash TEXT PRIMARY KEY CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    requested_by_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    subscription_period_id BIGINT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending','accepted','cancel_confirmed','expired')),
    discount_basis_points INTEGER NOT NULL CHECK (discount_basis_points=5000),
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    CHECK (expires_at > created_at),
    FOREIGN KEY (subscription_period_id, workspace_id)
        REFERENCES billing_subscription_periods(id, workspace_id) ON DELETE RESTRICT
);

CREATE UNIQUE INDEX idx_billing_retention_one_pending
    ON billing_retention_offers(workspace_id) WHERE status='pending';

CREATE TABLE workspace_usage_periods (
    subscription_period_id BIGINT NOT NULL,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    metric TEXT NOT NULL CHECK (metric ~ '^[a-z][a-z0-9_]{0,63}$'),
    quantity BIGINT NOT NULL DEFAULT 0 CHECK (quantity >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (subscription_period_id, metric),
    FOREIGN KEY (subscription_period_id, workspace_id)
        REFERENCES billing_subscription_periods(id, workspace_id) ON DELETE RESTRICT
);

CREATE INDEX idx_workspace_usage_periods_workspace
    ON workspace_usage_periods(workspace_id, subscription_period_id, metric);

-- Enforce current-resource entitlements in PostgreSQL so legacy, workspace and
-- webhook connection paths cannot bypass limits by selecting another route.
CREATE FUNCTION workspace_entitlement_limit(target_workspace TEXT, target_metric TEXT) RETURNS BIGINT
LANGUAGE plpgsql STABLE AS $$
DECLARE
    result BIGINT;
BEGIN
    -- Free v1 intentionally had no hard static-resource enforcement. During
    -- rollout it keeps post editing/publishing below, but resource growth is
    -- capped at the same nominal 1/1/1GB limits as Free v2.
    IF EXISTS (
        SELECT 1 FROM workspace_subscriptions
        WHERE workspace_id=target_workspace AND plan_code='free' AND plan_version=1
          AND status IN ('active','trialing')
    ) THEN
        SELECT e.limit_value*e.unit_scale INTO result
        FROM billing_plan_entitlements e
        WHERE e.plan_code='free' AND e.plan_version=1 AND e.usage_metric=target_metric;
        IF result IS NULL THEN
            RAISE EXCEPTION USING ERRCODE='23514', MESSAGE='legacy workspace entitlement is unavailable',
                CONSTRAINT='workspace_entitlement_unavailable';
        END IF;
        RETURN result;
    END IF;
    SELECT e.limit_value*e.unit_scale INTO result
    FROM workspace_subscriptions s
    JOIN billing_plan_entitlements e
      ON e.plan_code=s.plan_code AND e.plan_version=s.plan_version
    WHERE s.workspace_id=target_workspace
      AND s.status IN ('active','trialing')
      AND (
        s.plan_code='free'
        OR EXISTS (
          SELECT 1
          FROM billing_subscription_contracts c
          JOIN billing_subscription_periods bp ON bp.id=c.current_period_id
          WHERE c.workspace_id=s.workspace_id
            AND c.status IN ('active','past_due')
            AND bp.status='active'
            AND (
              bp.period_end>CURRENT_TIMESTAMP
              OR (c.status='past_due' AND c.grace_until>CURRENT_TIMESTAMP)
            )
        )
      )
      AND e.usage_metric=target_metric
      AND e.hard_limit=TRUE;
    IF result IS NULL THEN
        RAISE EXCEPTION USING ERRCODE='23514', MESSAGE='workspace entitlement is unavailable',
            CONSTRAINT='workspace_entitlement_unavailable';
    END IF;
    RETURN result;
END;
$$;

CREATE FUNCTION enforce_workspace_channel_entitlement() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    used BIGINT;
    allowed BIGINT;
BEGIN
    IF NOT NEW.active OR (TG_OP='UPDATE' AND OLD.workspace_id IS NOT DISTINCT FROM NEW.workspace_id AND OLD.active) THEN
        RETURN NEW;
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('maxstudio:billing:' || NEW.workspace_id, 0));
    PERFORM pg_advisory_xact_lock(hashtextextended('maxstudio:workspace-resource:' || NEW.workspace_id || ':channels', 0));
    allowed := workspace_entitlement_limit(NEW.workspace_id, 'channels');
    SELECT count(*) INTO used FROM channels WHERE workspace_id=NEW.workspace_id AND active=TRUE;
    IF used >= allowed THEN
        RAISE EXCEPTION USING ERRCODE='23514', MESSAGE='workspace channel limit exceeded',
            CONSTRAINT='workspace_channels_entitlement';
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION workspace_static_entitlement_overage(target_workspace TEXT) RETURNS BOOLEAN
LANGUAGE plpgsql STABLE AS $$
DECLARE
    channel_limit BIGINT;
    seat_limit BIGINT;
    storage_limit BIGINT;
    channel_count BIGINT;
    seat_count BIGINT;
    storage_bytes BIGINT;
BEGIN
    IF EXISTS (
        SELECT 1 FROM workspace_subscriptions
        WHERE workspace_id=target_workspace AND plan_code='free' AND plan_version=1
          AND status IN ('active','trialing')
    ) THEN
        RETURN FALSE;
    END IF;
    channel_limit := workspace_entitlement_limit(target_workspace, 'channels');
    seat_limit := workspace_entitlement_limit(target_workspace, 'seats');
    storage_limit := workspace_entitlement_limit(target_workspace, 'storage_bytes');
    SELECT
      (SELECT count(*) FROM channels WHERE workspace_id=target_workspace AND active=TRUE),
      (SELECT count(*) FROM workspace_members WHERE workspace_id=target_workspace),
      COALESCE((SELECT total_bytes FROM workspace_media_usage WHERE workspace_id=target_workspace),0)
    INTO channel_count,seat_count,storage_bytes;
    RETURN channel_count>channel_limit OR seat_count>seat_limit OR storage_bytes>storage_limit;
END;
$$;

CREATE FUNCTION enforce_workspace_static_entitlement_compliance() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('maxstudio:billing:' || NEW.workspace_id, 0));
    IF TG_OP='UPDATE' AND (
      OLD.status='publishing'
      OR (OLD.status='scheduled' AND NEW.status IN ('draft','failed') AND NEW.scheduled_at IS NULL)
      OR (OLD.max_message_id<>'' AND NEW.max_message_id='')
      OR (OLD.image_path<>'' AND NEW.image_path='')
    ) THEN
        RETURN NEW;
    END IF;
    IF workspace_static_entitlement_overage(NEW.workspace_id) THEN
        RAISE EXCEPTION USING ERRCODE='23514', MESSAGE='workspace resources exceed the active plan',
            CONSTRAINT='workspace_static_entitlement_overage';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER posts_enforce_workspace_static_entitlement
BEFORE INSERT OR UPDATE ON posts
FOR EACH ROW EXECUTE FUNCTION enforce_workspace_static_entitlement_compliance();

CREATE TRIGGER workspace_channels_enforce_entitlement
BEFORE INSERT OR UPDATE OF workspace_id, active ON channels
FOR EACH ROW EXECUTE FUNCTION enforce_workspace_channel_entitlement();

CREATE FUNCTION enforce_workspace_seat_entitlement() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    used BIGINT;
    allowed BIGINT;
BEGIN
    IF TG_OP='UPDATE' AND OLD.workspace_id IS NOT DISTINCT FROM NEW.workspace_id THEN
        RETURN NEW;
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('maxstudio:billing:' || NEW.workspace_id, 0));
    PERFORM pg_advisory_xact_lock(hashtextextended('maxstudio:workspace-resource:' || NEW.workspace_id || ':seats', 0));
    allowed := workspace_entitlement_limit(NEW.workspace_id, 'seats');
    SELECT count(*) INTO used FROM workspace_members WHERE workspace_id=NEW.workspace_id;
    IF used >= allowed THEN
        RAISE EXCEPTION USING ERRCODE='23514', MESSAGE='workspace seat limit exceeded',
            CONSTRAINT='workspace_seats_entitlement';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER workspace_members_enforce_entitlement
BEFORE INSERT OR UPDATE OF workspace_id ON workspace_members
FOR EACH ROW EXECUTE FUNCTION enforce_workspace_seat_entitlement();

CREATE FUNCTION enforce_workspace_storage_entitlement() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    allowed BIGINT;
BEGIN
    IF TG_OP='UPDATE' AND NEW.total_bytes <= OLD.total_bytes THEN
        RETURN NEW;
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('maxstudio:billing:' || NEW.workspace_id, 0));
    PERFORM pg_advisory_xact_lock(hashtextextended('maxstudio:workspace-resource:' || NEW.workspace_id || ':storage_bytes', 0));
    allowed := workspace_entitlement_limit(NEW.workspace_id, 'storage_bytes');
    IF NEW.total_bytes > allowed THEN
        RAISE EXCEPTION USING ERRCODE='23514', MESSAGE='workspace storage limit exceeded',
            CONSTRAINT='workspace_storage_entitlement';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER workspace_media_usage_enforce_entitlement
BEFORE INSERT OR UPDATE OF total_bytes ON workspace_media_usage
FOR EACH ROW EXECUTE FUNCTION enforce_workspace_storage_entitlement();

DROP TRIGGER IF EXISTS zz_workspace_active_write_guard ON billing_subscription_periods;
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON billing_subscription_periods
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();

DROP TRIGGER IF EXISTS zz_workspace_active_write_guard ON billing_subscription_contracts;
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON billing_subscription_contracts
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();

DROP TRIGGER IF EXISTS zz_workspace_active_write_guard ON billing_payment_attempts;
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON billing_payment_attempts
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();

DROP TRIGGER IF EXISTS zz_workspace_active_write_guard ON billing_retention_offers;
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON billing_retention_offers
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();

DROP TRIGGER IF EXISTS zz_workspace_active_write_guard ON billing_recurring_consents;
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT ON billing_recurring_consents
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();

DROP TRIGGER IF EXISTS zz_workspace_active_write_guard ON workspace_usage_periods;
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON workspace_usage_periods
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
