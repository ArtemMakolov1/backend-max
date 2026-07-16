-- Versioned commercial catalog and workspace-scoped usage foundation.
--
-- Plan rows are immutable snapshots: a price or entitlement change creates a
-- new version and existing subscriptions keep referencing the old one.  Paid
-- plans are seeded for product/forecast work but remain internal and
-- unavailable until checkout and lifecycle handling are implemented.

CREATE TABLE IF NOT EXISTS billing_plan_versions (
    plan_code TEXT NOT NULL CHECK (plan_code ~ '^[a-z][a-z0-9_]{0,31}$'),
    version INTEGER NOT NULL CHECK (version > 0),
    catalog_version DATE NOT NULL,
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    description TEXT NOT NULL DEFAULT '',
    currency_code TEXT NOT NULL CHECK (currency_code ~ '^[A-Z]{3}$'),
    monthly_price_minor BIGINT NOT NULL CHECK (monthly_price_minor >= 0),
    billing_interval TEXT NOT NULL DEFAULT 'month' CHECK (billing_interval = 'month'),
    public BOOLEAN NOT NULL DEFAULT FALSE,
    available BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (plan_code, version),
    CHECK (NOT public OR available)
);

CREATE INDEX IF NOT EXISTS idx_billing_plan_versions_public_available
    ON billing_plan_versions(public, available, monthly_price_minor, plan_code, version);
CREATE UNIQUE INDEX IF NOT EXISTS idx_billing_plan_versions_one_available
    ON billing_plan_versions(plan_code) WHERE available;

-- unit_scale tells consumers how many ledger base units make one displayed
-- entitlement unit.  Image quotas are presented as medium-equivalent images,
-- while enforcement and accounting use cost-weighted image credits.
CREATE TABLE IF NOT EXISTS billing_plan_entitlements (
    plan_code TEXT NOT NULL,
    plan_version INTEGER NOT NULL,
    entitlement_key TEXT NOT NULL CHECK (entitlement_key ~ '^[a-z][a-z0-9_]{0,63}$'),
    usage_metric TEXT NOT NULL CHECK (usage_metric ~ '^[a-z][a-z0-9_]{0,63}$'),
    limit_value BIGINT NOT NULL CHECK (limit_value >= 0),
    unit TEXT NOT NULL CHECK (unit ~ '^[a-z][a-z0-9_]{0,31}$'),
    period TEXT NOT NULL CHECK (period IN ('current', 'month')),
    unit_scale BIGINT NOT NULL DEFAULT 1 CHECK (unit_scale > 0),
    hard_limit BOOLEAN NOT NULL DEFAULT TRUE,
    PRIMARY KEY (plan_code, plan_version, entitlement_key),
    CONSTRAINT billing_plan_entitlements_plan_fk
        FOREIGN KEY (plan_code, plan_version)
        REFERENCES billing_plan_versions(plan_code, version) ON DELETE RESTRICT,
    CONSTRAINT billing_plan_entitlements_usage_metric_unique
        UNIQUE (plan_code, plan_version, usage_metric)
);

-- A version is a durable commercial snapshot. Visibility/availability are
-- rollout state and may change, but price, identity and descriptive fields
-- require a new version. Entitlements are wholly immutable for the same reason.
CREATE OR REPLACE FUNCTION protect_billing_plan_version_snapshot() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION USING
            ERRCODE = '55000',
            MESSAGE = 'billing plan versions are immutable';
    END IF;
    IF OLD.plan_code IS DISTINCT FROM NEW.plan_code
       OR OLD.version IS DISTINCT FROM NEW.version
       OR OLD.catalog_version IS DISTINCT FROM NEW.catalog_version
       OR OLD.name IS DISTINCT FROM NEW.name
       OR OLD.description IS DISTINCT FROM NEW.description
       OR OLD.currency_code IS DISTINCT FROM NEW.currency_code
       OR OLD.monthly_price_minor IS DISTINCT FROM NEW.monthly_price_minor
       OR OLD.billing_interval IS DISTINCT FROM NEW.billing_interval
       OR OLD.created_at IS DISTINCT FROM NEW.created_at THEN
        RAISE EXCEPTION USING
            ERRCODE = '55000',
            MESSAGE = 'billing plan snapshot fields are immutable';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS billing_plan_versions_protect_snapshot ON billing_plan_versions;
CREATE TRIGGER billing_plan_versions_protect_snapshot
BEFORE UPDATE OR DELETE ON billing_plan_versions
FOR EACH ROW EXECUTE FUNCTION protect_billing_plan_version_snapshot();

CREATE OR REPLACE FUNCTION protect_billing_plan_entitlement_snapshot() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
	IF TG_OP = 'INSERT' THEN
		IF EXISTS (
			SELECT 1 FROM workspace_subscriptions
			WHERE plan_code=NEW.plan_code AND plan_version=NEW.plan_version
		) THEN
			RAISE EXCEPTION USING
				ERRCODE = '55000',
				MESSAGE = 'subscribed billing plan entitlements are immutable';
		END IF;
		RETURN NEW;
	END IF;
    RAISE EXCEPTION USING
        ERRCODE = '55000',
        MESSAGE = 'billing plan entitlements are immutable';
    RETURN NULL;
END;
$$;

CREATE TABLE IF NOT EXISTS workspace_subscriptions (
    workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    plan_code TEXT NOT NULL,
    plan_version INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'trialing', 'paused', 'canceled')),
    started_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT workspace_subscriptions_plan_fk
        FOREIGN KEY (plan_code, plan_version)
        REFERENCES billing_plan_versions(plan_code, version) ON DELETE RESTRICT
);

CREATE INDEX IF NOT EXISTS idx_workspace_subscriptions_plan
    ON workspace_subscriptions(plan_code, plan_version, status, workspace_id);

-- Monthly counters are append-by-period and quantity based.  The application
-- uses an atomic UPSERT under a transaction advisory lock, so weighted charges
-- cannot be lost across replicas.  Historical months are retained for future
-- invoices and unit-economics analysis.
CREATE TABLE IF NOT EXISTS workspace_usage_monthly (
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    period_start DATE NOT NULL CHECK (EXTRACT(DAY FROM period_start) = 1),
    metric TEXT NOT NULL CHECK (metric ~ '^[a-z][a-z0-9_]{0,63}$'),
    quantity BIGINT NOT NULL DEFAULT 0 CHECK (quantity >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (workspace_id, period_start, metric)
);

INSERT INTO billing_plan_versions(
    plan_code, version, catalog_version, name, description, currency_code,
    monthly_price_minor, billing_interval, public, available
) VALUES
    ('free',   1, DATE '2026-07-16', 'Free',   'Базовый тариф для знакомства с MaxPosty', 'RUB',      0, 'month', TRUE,  TRUE),
    ('solo',   1, DATE '2026-07-16', 'Solo',   'Для самостоятельного автора',             'RUB',  149000, 'month', FALSE, FALSE),
    ('pro',    1, DATE '2026-07-16', 'Pro',    'Для небольшой контент-команды',           'RUB',  549000, 'month', FALSE, FALSE),
    ('agency', 1, DATE '2026-07-16', 'Agency', 'Для агентств и нескольких брендов',       'RUB', 1599000, 'month', FALSE, FALSE)
ON CONFLICT (plan_code, version) DO NOTHING;

-- Temporarily remove the insert guard for an idempotent seed replay. The whole
-- migration is transactional and holds the global migration advisory lock.
DROP TRIGGER IF EXISTS billing_plan_entitlements_protect_snapshot ON billing_plan_entitlements;

-- Current-resource entitlements are catalog/display foundations only until the
-- corresponding create/upload paths consume them, so hard_limit is false for
-- channels, seats and storage. Monthly AI limits are enforceable behind the
-- explicit deployment switch.
-- One medium image equals 9 credits; low=1, medium/default=9, high/auto=36.
INSERT INTO billing_plan_entitlements(
    plan_code, plan_version, entitlement_key, usage_metric,
    limit_value, unit, period, unit_scale, hard_limit
) VALUES
    ('free',   1, 'channels',          'channels',             1,            'channel',                 'current', 1, FALSE),
    ('free',   1, 'seats',             'seats',                1,            'seat',                    'current', 1, FALSE),
    ('free',   1, 'storage_bytes',     'storage_bytes',        1073741824,   'byte',                    'current', 1, FALSE),
    ('free',   1, 'ai_images_monthly', 'ai_image_credits',     3,            'medium_equivalent_image', 'month',   9, TRUE),
    ('free',   1, 'ai_research_monthly','ai_research_requests',3,            'request',                 'month',   1, TRUE),
    ('free',   1, 'ai_format_monthly',  'ai_format_requests',  10,           'request',                 'month',   1, TRUE),

    ('solo',   1, 'channels',          'channels',             3,            'channel',                 'current', 1, FALSE),
    ('solo',   1, 'seats',             'seats',                1,            'seat',                    'current', 1, FALSE),
    ('solo',   1, 'storage_bytes',     'storage_bytes',        3221225472,   'byte',                    'current', 1, FALSE),
    ('solo',   1, 'ai_images_monthly', 'ai_image_credits',     20,           'medium_equivalent_image', 'month',   9, TRUE),
    ('solo',   1, 'ai_research_monthly','ai_research_requests',12,           'request',                 'month',   1, TRUE),
    ('solo',   1, 'ai_format_monthly',  'ai_format_requests',  60,           'request',                 'month',   1, TRUE),

    ('pro',    1, 'channels',          'channels',             10,           'channel',                 'current', 1, FALSE),
    ('pro',    1, 'seats',             'seats',                5,            'seat',                    'current', 1, FALSE),
    ('pro',    1, 'storage_bytes',     'storage_bytes',        10737418240,  'byte',                    'current', 1, FALSE),
    ('pro',    1, 'ai_images_monthly', 'ai_image_credits',     80,           'medium_equivalent_image', 'month',   9, TRUE),
    ('pro',    1, 'ai_research_monthly','ai_research_requests',45,           'request',                 'month',   1, TRUE),
    ('pro',    1, 'ai_format_monthly',  'ai_format_requests',  240,          'request',                 'month',   1, TRUE),

    ('agency', 1, 'channels',          'channels',             30,           'channel',                 'current', 1, FALSE),
    ('agency', 1, 'seats',             'seats',                15,           'seat',                    'current', 1, FALSE),
    ('agency', 1, 'storage_bytes',     'storage_bytes',        32212254720,  'byte',                    'current', 1, FALSE),
    ('agency', 1, 'ai_images_monthly', 'ai_image_credits',     250,          'medium_equivalent_image', 'month',   9, TRUE),
    ('agency', 1, 'ai_research_monthly','ai_research_requests',140,          'request',                 'month',   1, TRUE),
    ('agency', 1, 'ai_format_monthly',  'ai_format_requests',  800,          'request',                 'month',   1, TRUE)
ON CONFLICT (plan_code, plan_version, entitlement_key) DO NOTHING;

INSERT INTO workspace_subscriptions(
    workspace_id, plan_code, plan_version, status, started_at, updated_at
)
SELECT id, 'free', 1, 'active', created_at, CURRENT_TIMESTAMP
FROM workspaces
ON CONFLICT (workspace_id) DO NOTHING;

CREATE TRIGGER billing_plan_entitlements_protect_snapshot
BEFORE INSERT OR UPDATE OR DELETE ON billing_plan_entitlements
FOR EACH ROW EXECUTE FUNCTION protect_billing_plan_entitlement_snapshot();

CREATE OR REPLACE FUNCTION create_free_subscription_for_workspace() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO workspace_subscriptions(
        workspace_id, plan_code, plan_version, status, started_at, updated_at
    ) VALUES (
        NEW.id, 'free', 1, 'active', NEW.created_at, NEW.created_at
    ) ON CONFLICT (workspace_id) DO NOTHING;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS workspaces_create_free_subscription ON workspaces;
CREATE TRIGGER workspaces_create_free_subscription
AFTER INSERT ON workspaces
FOR EACH ROW EXECUTE FUNCTION create_free_subscription_for_workspace();

-- Reuse the active-workspace guard introduced by 016.  Subscriptions and
-- usage may be read after archival, but no new charges or plan changes should
-- be written to an archived tenant.
DROP TRIGGER IF EXISTS zz_workspace_active_write_guard ON workspace_subscriptions;
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON workspace_subscriptions
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();

DROP TRIGGER IF EXISTS zz_workspace_active_write_guard ON workspace_usage_monthly;
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON workspace_usage_monthly
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
