-- Durable multi-channel planning. Variants remain independent planning rows
-- until materialized as posts; scheduling is still gated by the post revision
-- approval workflow.
CREATE TABLE campaigns (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    name TEXT NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 160),
    description TEXT NOT NULL DEFAULT '' CHECK (char_length(description) <= 4000),
    status TEXT NOT NULL DEFAULT 'planned' CHECK (
        status IN ('planned', 'active', 'completed', 'archived')
    ),
    created_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    archived_at TIMESTAMPTZ,
    CONSTRAINT campaigns_workspace_id_unique UNIQUE (workspace_id, id),
    CONSTRAINT campaigns_archive_state CHECK (
        (status = 'archived') = (archived_at IS NOT NULL)
    )
);

CREATE TABLE campaign_variants (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    campaign_id TEXT NOT NULL,
    channel_id BIGINT NOT NULL,
    post_id BIGINT,
    title TEXT NOT NULL CHECK (char_length(btrim(title)) BETWEEN 1 AND 200),
    content TEXT NOT NULL CHECK (char_length(content) BETWEEN 1 AND 4000),
    format TEXT NOT NULL DEFAULT 'markdown' CHECK (format IN ('markdown', 'html')),
    planned_at TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL DEFAULT 'planned' CHECK (
        status IN ('planned', 'materialized', 'scheduled', 'published')
    ),
    created_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT campaign_variants_campaign_fk
        FOREIGN KEY (workspace_id, campaign_id)
        REFERENCES campaigns(workspace_id, id) ON DELETE CASCADE,
    CONSTRAINT campaign_variants_channel_fk
        FOREIGN KEY (workspace_id, channel_id)
        REFERENCES channels(workspace_id, id) ON DELETE RESTRICT,
    CONSTRAINT campaign_variants_post_fk
        FOREIGN KEY (workspace_id, post_id)
        REFERENCES posts(workspace_id, id) ON DELETE SET NULL (post_id),
    CONSTRAINT campaign_variants_workspace_id_unique UNIQUE (workspace_id, id),
    CONSTRAINT campaign_variants_campaign_channel_time_unique
        UNIQUE (campaign_id, channel_id, planned_at),
    CONSTRAINT campaign_variants_materialization_state CHECK (
        (post_id IS NULL AND status = 'planned') OR
        (post_id IS NOT NULL AND status IN ('materialized', 'scheduled', 'published'))
    )
);

CREATE INDEX idx_campaigns_workspace_updated
    ON campaigns(workspace_id, archived_at, updated_at DESC, id);
CREATE INDEX idx_campaign_variants_campaign_planned
    ON campaign_variants(workspace_id, campaign_id, planned_at, id);
CREATE INDEX idx_campaign_variants_calendar
    ON campaign_variants(workspace_id, planned_at, channel_id, id);
CREATE UNIQUE INDEX idx_campaign_variants_post
    ON campaign_variants(workspace_id, post_id) WHERE post_id IS NOT NULL;
CREATE INDEX idx_posts_workspace_scheduled_calendar
    ON posts(workspace_id, scheduled_at, channel_id, id)
    WHERE scheduled_at IS NOT NULL;
CREATE INDEX idx_posts_workspace_published_calendar
    ON posts(workspace_id, published_at, channel_id, id)
    WHERE published_at IS NOT NULL;

-- A materialized post may still be deleted through the normal post workflow.
-- The composite FK detaches the variant; reset its derived state in the same
-- statement so the materialization check remains true and the plan is reusable.
CREATE FUNCTION reset_campaign_variant_after_post_detach() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.post_id IS NOT NULL AND NEW.post_id IS NULL THEN
        NEW.status := 'planned';
        NEW.updated_at := CURRENT_TIMESTAMP;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER campaign_variants_reset_post_detach
BEFORE UPDATE OF post_id ON campaign_variants
FOR EACH ROW EXECUTE FUNCTION reset_campaign_variant_after_post_detach();

-- Campaign status is derived in one place from all of its variants. Besides
-- publication updates, this covers adding/removing variants and FK-driven post
-- detaches, so a completed campaign cannot retain a stale lifecycle state.
CREATE FUNCTION recompute_campaign_status() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    target_workspace_id TEXT;
    target_campaign_id TEXT;
BEGIN
    target_workspace_id := COALESCE(NEW.workspace_id, OLD.workspace_id);
    target_campaign_id := COALESCE(NEW.campaign_id, OLD.campaign_id);

    UPDATE campaigns AS cp
    SET status = CASE
            WHEN EXISTS (
                SELECT 1 FROM campaign_variants AS cv
                WHERE cv.workspace_id = cp.workspace_id
                  AND cv.campaign_id = cp.id
            ) AND NOT EXISTS (
                SELECT 1 FROM campaign_variants AS cv
                WHERE cv.workspace_id = cp.workspace_id
                  AND cv.campaign_id = cp.id
                  AND cv.status <> 'published'
            ) THEN 'completed'
            WHEN EXISTS (
                SELECT 1 FROM campaign_variants AS cv
                WHERE cv.workspace_id = cp.workspace_id
                  AND cv.campaign_id = cp.id
                  AND cv.status IN ('materialized', 'scheduled', 'published')
            ) THEN 'active'
            ELSE 'planned'
        END,
        updated_at = CURRENT_TIMESTAMP
    WHERE cp.workspace_id = target_workspace_id
      AND cp.id = target_campaign_id
      AND cp.archived_at IS NULL;
    RETURN NULL;
END;
$$;

CREATE TRIGGER campaign_variants_recompute_campaign_status
AFTER INSERT OR UPDATE OR DELETE ON campaign_variants
FOR EACH ROW EXECUTE FUNCTION recompute_campaign_status();

-- Post lifecycle changes only derive the linked variant state. The variant
-- trigger above performs the campaign aggregation for every path, including a
-- normal schedule cancellation (scheduled -> draft).
CREATE FUNCTION sync_campaign_variant_from_post() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    UPDATE campaign_variants AS cv
    SET status = CASE
            WHEN NEW.status = 'published' THEN 'published'
            WHEN NEW.status = 'scheduled' THEN 'scheduled'
            ELSE 'materialized'
        END,
        updated_at = CURRENT_TIMESTAMP
    FROM campaigns AS cp
    WHERE cv.workspace_id = NEW.workspace_id
      AND cv.post_id = NEW.id
      AND cp.workspace_id = cv.workspace_id
      AND cp.id = cv.campaign_id
      AND cp.archived_at IS NULL;
    RETURN NEW;
END;
$$;

CREATE TRIGGER posts_sync_campaign_variant
AFTER UPDATE OF status ON posts
FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status)
EXECUTE FUNCTION sync_campaign_variant_from_post();

-- Archival takes an exclusive lock on the workspace row. These key-share
-- guards make campaign writes participate in the same lifecycle protocol as
-- posts, revisions and other workspace children.
CREATE TRIGGER campaigns_active_workspace_guard
BEFORE INSERT OR UPDATE ON campaigns
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();

CREATE TRIGGER campaign_variants_active_workspace_guard
BEFORE INSERT OR UPDATE ON campaign_variants
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
