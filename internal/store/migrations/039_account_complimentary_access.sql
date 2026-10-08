-- Operator-managed, account-bound access. This does not create a paid plan,
-- payment, membership, or administrative role. Positive ownership is resolved
-- for every workspace, including workspaces created after the grant.
CREATE TABLE account_complimentary_access (
    owner_user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    granted_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    revoked_at TIMESTAMPTZ,
    granted_by TEXT NOT NULL CHECK (char_length(granted_by) BETWEEN 1 AND 128),
    updated_by TEXT NOT NULL CHECK (char_length(updated_by) BETWEEN 1 AND 128),
    CHECK (active = (revoked_at IS NULL))
);
CREATE TABLE account_complimentary_access_events (
    event_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id TEXT NOT NULL,
    operation TEXT NOT NULL CHECK (operation IN ('grant','revoke')),
    operator_name TEXT NOT NULL CHECK (char_length(operator_name) BETWEEN 1 AND 128),
    operation_ref TEXT NOT NULL CHECK (char_length(operation_ref) BETWEEN 1 AND 512),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX account_complimentary_access_events_owner
    ON account_complimentary_access_events(owner_user_id,occurred_at);
CREATE FUNCTION protect_account_complimentary_access_event() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION USING ERRCODE='55000', MESSAGE='complimentary access audit is immutable';
END;
$$;
CREATE TRIGGER account_complimentary_access_events_immutable
BEFORE UPDATE OR DELETE ON account_complimentary_access_events
FOR EACH ROW EXECUTE FUNCTION protect_account_complimentary_access_event();

CREATE FUNCTION workspace_has_complimentary_access(target_workspace TEXT) RETURNS BOOLEAN
LANGUAGE SQL STABLE AS $$
    SELECT EXISTS (
        SELECT 1 FROM workspaces w JOIN account_complimentary_access g
          ON g.owner_user_id=w.owner_user_id AND g.active
        WHERE w.id=target_workspace AND w.archived_at IS NULL
    );
$$;

-- SECURITY INVOKER plus table and EXECUTE privileges restrict activation to
-- the migration/operator connection. Inspect performs no writes or row locks
-- and is valid inside a READ ONLY transaction.
CREATE FUNCTION manage_account_complimentary_access(
    target_email TEXT, operation TEXT, operator_name TEXT, operation_ref TEXT
) RETURNS TABLE(user_id TEXT, active BOOLEAN, owned_workspaces BIGINT, changed BOOLEAN)
LANGUAGE plpgsql SECURITY INVOKER AS $$
DECLARE
    matches TEXT[];
    selected_owner TEXT;
    target_workspace TEXT;
    enabled BOOLEAN;
    previous_active BOOLEAN;
BEGIN
    IF operation NOT IN ('inspect','grant','revoke') OR operation IS NULL
      OR target_email IS NULL OR char_length(target_email) NOT BETWEEN 3 AND 320
      OR btrim(target_email) !~ '^[^[:space:]@]+@[^[:space:]@]+$'
      OR operator_name IS NULL OR char_length(btrim(operator_name)) NOT BETWEEN 1 AND 128
      OR operation_ref IS NULL OR char_length(btrim(operation_ref)) NOT BETWEEN 1 AND 512 THEN
        RAISE EXCEPTION USING ERRCODE='22023', MESSAGE='invalid complimentary access operation';
    END IF;
    SELECT array_agg(u.id ORDER BY u.id) INTO matches
    FROM users u JOIN auth_identities i ON i.owner_id=u.id AND i.provider='yandex'
    WHERE lower(btrim(u.email))=lower(btrim(target_email));
    IF COALESCE(cardinality(matches),0)<>1 THEN
        RAISE EXCEPTION USING ERRCODE='22023', MESSAGE='target account must have one verified Yandex identity';
    END IF;
    selected_owner := matches[1];
    changed := FALSE;
    IF operation<>'inspect' THEN
        -- Billing operations and ownership transfer acquire the workspace
        -- billing lock first. Keep that order before the owner-capacity lock.
        FOR target_workspace IN
            SELECT w.id FROM workspaces w WHERE w.owner_user_id=selected_owner ORDER BY w.id
        LOOP
            PERFORM pg_advisory_xact_lock(hashtextextended('maxstudio:billing:' || target_workspace,0));
        END LOOP;
        PERFORM pg_advisory_xact_lock(hashtextextended('maxposty:owned-team-workspaces:' || selected_owner,0));
        PERFORM u.id FROM users u WHERE u.id=selected_owner
          AND lower(btrim(u.email))=lower(btrim(target_email)) FOR SHARE;
        IF NOT FOUND THEN
            RAISE EXCEPTION USING ERRCODE='40001', MESSAGE='target account changed during operation';
        END IF;
        enabled := operation='grant';
        SELECT g.active INTO previous_active FROM account_complimentary_access g
          WHERE g.owner_user_id=selected_owner FOR UPDATE;
        IF enabled AND previous_active IS DISTINCT FROM TRUE THEN
            INSERT INTO account_complimentary_access(owner_user_id,active,granted_by,updated_by)
            VALUES(selected_owner,TRUE,operator_name,operator_name)
            ON CONFLICT(owner_user_id) DO UPDATE SET active=TRUE,revoked_at=NULL,
              updated_at=CURRENT_TIMESTAMP,updated_by=EXCLUDED.updated_by;
            changed := TRUE;
        ELSIF NOT enabled AND previous_active IS TRUE THEN
            UPDATE account_complimentary_access g SET active=FALSE,revoked_at=CURRENT_TIMESTAMP,
              updated_at=CURRENT_TIMESTAMP,updated_by=operator_name WHERE g.owner_user_id=selected_owner;
            changed := TRUE;
        END IF;
        IF changed THEN
            INSERT INTO account_complimentary_access_events(owner_user_id,operation,operator_name,operation_ref)
            VALUES(selected_owner,operation,operator_name,operation_ref);
        END IF;
    END IF;
    user_id := selected_owner;
    SELECT COALESCE((SELECT g.active FROM account_complimentary_access g WHERE g.owner_user_id=selected_owner),FALSE) INTO active;
    SELECT count(*) INTO owned_workspaces FROM workspaces w WHERE w.owner_user_id=selected_owner;
    RETURN NEXT;
END;
$$;
REVOKE ALL ON FUNCTION manage_account_complimentary_access(TEXT,TEXT,TEXT,TEXT) FROM PUBLIC;

-- init-app-role grants DML on future tables by default. Revoke those explicit
-- ACL grants too; revoking PUBLIC alone would leave the runtime role writable.
REVOKE ALL ON account_complimentary_access,account_complimentary_access_events FROM PUBLIC;
DO $$
DECLARE
    relation RECORD;
    grantee RECORD;
BEGIN
    FOR relation IN SELECT c.oid,c.relowner,n.nspname,c.relname
      FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
      WHERE n.nspname=current_schema() AND c.relname IN ('account_complimentary_access','account_complimentary_access_events')
    LOOP
        FOR grantee IN SELECT DISTINCT r.rolname FROM pg_class c,
          LATERAL aclexplode(COALESCE(c.relacl,acldefault('r',c.relowner))) a JOIN pg_roles r ON r.oid=a.grantee
          WHERE c.oid=relation.oid AND a.grantee<>relation.relowner
        LOOP
            EXECUTE format('REVOKE ALL ON TABLE %I.%I FROM %I',relation.nspname,relation.relname,grantee.rolname);
            EXECUTE format('GRANT SELECT ON TABLE %I.%I TO %I',relation.nspname,relation.relname,grantee.rolname);
        END LOOP;
    END LOOP;
END;
$$;

CREATE OR REPLACE FUNCTION workspace_entitlement_limit(target_workspace TEXT, target_metric TEXT) RETURNS BIGINT
LANGUAGE plpgsql STABLE AS $$
DECLARE
    result BIGINT;
BEGIN
    IF workspace_has_complimentary_access(target_workspace) THEN
        RETURN NULL; -- unbounded, never an invented high allowance
    END IF;
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

CREATE OR REPLACE FUNCTION workspace_static_entitlement_overage(target_workspace TEXT) RETURNS BOOLEAN
LANGUAGE plpgsql STABLE AS $$
DECLARE
    channel_limit BIGINT;
    seat_limit BIGINT;
    storage_limit BIGINT;
    channel_count BIGINT;
    seat_count BIGINT;
    storage_bytes BIGINT;
BEGIN
    IF workspace_has_complimentary_access(target_workspace) THEN
        RETURN FALSE;
    END IF;
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
