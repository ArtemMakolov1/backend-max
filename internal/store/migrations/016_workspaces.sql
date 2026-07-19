-- Expand-only workspace and collaboration schema. Legacy owner_id columns and
-- constraints remain in place so the current single-user API can run during
-- the rollout. New workspace-aware code uses workspace_id as the tenant key.

CREATE TABLE workspaces (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    compat_owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    is_personal BOOLEAN NOT NULL DEFAULT FALSE,
    approval_required BOOLEAN NOT NULL DEFAULT TRUE,
    require_distinct_approver BOOLEAN NOT NULL DEFAULT TRUE,
    created_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    archived_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX idx_workspaces_personal_owner
    ON workspaces(owner_user_id) WHERE is_personal;
CREATE INDEX idx_workspaces_owner_updated
    ON workspaces(owner_user_id, updated_at DESC, id);

CREATE TABLE workspace_members (
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK (role IN ('owner', 'editor', 'approver', 'viewer')),
    created_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    joined_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (workspace_id, user_id)
);

CREATE INDEX idx_workspace_members_user
    ON workspace_members(user_id, updated_at DESC, workspace_id);
CREATE INDEX idx_workspace_members_workspace_role
    ON workspace_members(workspace_id, role, user_id);

-- Deterministic IDs make this migration idempotent at the data level and let
-- operators correlate a legacy user with its personal workspace.
INSERT INTO workspaces(
    id, name, owner_user_id, compat_owner_user_id, is_personal,
    approval_required, require_distinct_approver,
    created_by, created_at, updated_at
)
SELECT
    'personal_' || md5('maxposty-personal:' || id),
    CASE WHEN btrim(display_name) <> '' THEN display_name ELSE 'Личное пространство' END,
    id, id, TRUE, FALSE, FALSE, id, created_at, updated_at
FROM users
ON CONFLICT DO NOTHING;

INSERT INTO workspace_members(workspace_id, user_id, role, created_by, joined_at, updated_at)
SELECT w.id, w.owner_user_id, 'owner', w.owner_user_id, w.created_at, w.updated_at
FROM workspaces w
WHERE w.is_personal
ON CONFLICT DO NOTHING;

-- Enforce the ownership invariant in the database as well as in application
-- transactions. This turns concurrent or future writer bugs into a rollback
-- instead of leaving a workspace with multiple owners.
CREATE UNIQUE INDEX idx_workspace_members_single_owner
    ON workspace_members(workspace_id) WHERE role = 'owner';

CREATE FUNCTION create_personal_workspace_for_user() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    personal_id TEXT := 'personal_' || md5('maxposty-personal:' || NEW.id);
BEGIN
    IF NEW.id LIKE 'workspace_compat_%' THEN
        RETURN NEW;
    END IF;
    INSERT INTO workspaces(
        id, name, owner_user_id, compat_owner_user_id, is_personal,
        approval_required, require_distinct_approver,
        created_by, created_at, updated_at
    ) VALUES (
        personal_id,
        CASE WHEN btrim(NEW.display_name) <> '' THEN NEW.display_name ELSE 'Личное пространство' END,
        NEW.id, NEW.id, TRUE, FALSE, FALSE, NEW.id, NEW.created_at, NEW.updated_at
    ) ON CONFLICT DO NOTHING;
    INSERT INTO workspace_members(workspace_id, user_id, role, created_by, joined_at, updated_at)
    VALUES (personal_id, NEW.id, 'owner', NEW.id, NEW.created_at, NEW.updated_at)
    ON CONFLICT DO NOTHING;
    RETURN NEW;
END;
$$;

CREATE TRIGGER users_create_personal_workspace
AFTER INSERT ON users FOR EACH ROW EXECUTE FUNCTION create_personal_workspace_for_user();

ALTER TABLE channels ADD COLUMN workspace_id TEXT;
ALTER TABLE posts ADD COLUMN workspace_id TEXT;
ALTER TABLE channel_claims ADD COLUMN workspace_id TEXT;
ALTER TABLE channel_claims ADD COLUMN requested_by_user_id TEXT;
ALTER TABLE media_assets ADD COLUMN workspace_id TEXT;
ALTER TABLE post_attachments ADD COLUMN workspace_id TEXT;
ALTER TABLE post_view_snapshots ADD COLUMN workspace_id TEXT;

UPDATE channels r SET workspace_id = w.id
FROM workspaces w WHERE w.is_personal AND w.owner_user_id = r.owner_id;
UPDATE posts r SET workspace_id = w.id
FROM workspaces w WHERE w.is_personal AND w.owner_user_id = r.owner_id;
UPDATE channel_claims r SET workspace_id = w.id
FROM workspaces w WHERE w.is_personal AND w.owner_user_id = r.owner_id;
UPDATE channel_claims SET requested_by_user_id=owner_id WHERE requested_by_user_id IS NULL;
UPDATE media_assets r SET workspace_id = w.id
FROM workspaces w WHERE w.is_personal AND w.owner_user_id = r.owner_id;
UPDATE post_attachments r SET workspace_id = w.id
FROM workspaces w WHERE w.is_personal AND w.owner_user_id = r.owner_id;
UPDATE post_view_snapshots r SET workspace_id = w.id
FROM workspaces w WHERE w.is_personal AND w.owner_user_id = r.owner_id;

ALTER TABLE channels
    ADD CONSTRAINT channels_workspace_fk FOREIGN KEY (workspace_id)
        REFERENCES workspaces(id) ON DELETE RESTRICT NOT VALID,
    ADD CONSTRAINT channels_workspace_present CHECK (workspace_id IS NOT NULL) NOT VALID,
    ADD CONSTRAINT channels_workspace_id_unique UNIQUE (workspace_id, id);
ALTER TABLE posts
    ADD CONSTRAINT posts_workspace_fk FOREIGN KEY (workspace_id)
        REFERENCES workspaces(id) ON DELETE RESTRICT NOT VALID,
    ADD CONSTRAINT posts_workspace_present CHECK (workspace_id IS NOT NULL) NOT VALID,
    ADD CONSTRAINT posts_workspace_id_unique UNIQUE (workspace_id, id);
ALTER TABLE channel_claims
    ADD CONSTRAINT channel_claims_workspace_fk FOREIGN KEY (workspace_id)
        REFERENCES workspaces(id) ON DELETE RESTRICT NOT VALID,
    ADD CONSTRAINT channel_claims_workspace_present CHECK (workspace_id IS NOT NULL) NOT VALID,
    ADD CONSTRAINT channel_claims_requested_by_fk FOREIGN KEY (requested_by_user_id)
        REFERENCES users(id) ON DELETE CASCADE NOT VALID,
    ADD CONSTRAINT channel_claims_requested_by_present CHECK (requested_by_user_id IS NOT NULL) NOT VALID;
ALTER TABLE media_assets
    ADD CONSTRAINT media_assets_workspace_fk FOREIGN KEY (workspace_id)
        REFERENCES workspaces(id) ON DELETE RESTRICT NOT VALID,
    ADD CONSTRAINT media_assets_workspace_present CHECK (workspace_id IS NOT NULL) NOT VALID;
ALTER TABLE post_attachments
    ADD CONSTRAINT post_attachments_workspace_fk FOREIGN KEY (workspace_id)
        REFERENCES workspaces(id) ON DELETE RESTRICT NOT VALID,
    ADD CONSTRAINT post_attachments_workspace_present CHECK (workspace_id IS NOT NULL) NOT VALID;
ALTER TABLE post_view_snapshots
    ADD CONSTRAINT post_view_snapshots_workspace_fk FOREIGN KEY (workspace_id)
        REFERENCES workspaces(id) ON DELETE RESTRICT NOT VALID,
    ADD CONSTRAINT post_view_snapshots_workspace_present CHECK (workspace_id IS NOT NULL) NOT VALID;

ALTER TABLE channels VALIDATE CONSTRAINT channels_workspace_fk;
ALTER TABLE channels VALIDATE CONSTRAINT channels_workspace_present;
ALTER TABLE posts VALIDATE CONSTRAINT posts_workspace_fk;
ALTER TABLE posts VALIDATE CONSTRAINT posts_workspace_present;
ALTER TABLE channel_claims VALIDATE CONSTRAINT channel_claims_workspace_fk;
ALTER TABLE channel_claims VALIDATE CONSTRAINT channel_claims_workspace_present;
ALTER TABLE channel_claims VALIDATE CONSTRAINT channel_claims_requested_by_fk;
ALTER TABLE channel_claims VALIDATE CONSTRAINT channel_claims_requested_by_present;
ALTER TABLE media_assets VALIDATE CONSTRAINT media_assets_workspace_fk;
ALTER TABLE media_assets VALIDATE CONSTRAINT media_assets_workspace_present;
ALTER TABLE post_attachments VALIDATE CONSTRAINT post_attachments_workspace_fk;
ALTER TABLE post_attachments VALIDATE CONSTRAINT post_attachments_workspace_present;
ALTER TABLE post_view_snapshots VALIDATE CONSTRAINT post_view_snapshots_workspace_fk;
ALTER TABLE post_view_snapshots VALIDATE CONSTRAINT post_view_snapshots_workspace_present;

CREATE INDEX idx_channels_workspace_active
    ON channels(workspace_id, active DESC, id);
CREATE INDEX idx_posts_workspace_created
    ON posts(workspace_id, created_at DESC, id DESC);
CREATE INDEX idx_posts_workspace_status_scheduled
    ON posts(workspace_id, status, scheduled_at, id);
CREATE INDEX idx_channel_claims_workspace_created
    ON channel_claims(workspace_id, created_at DESC, id);
CREATE INDEX idx_media_assets_workspace_updated
    ON media_assets(workspace_id, updated_at DESC, filename);
CREATE INDEX idx_post_attachments_workspace_post
    ON post_attachments(workspace_id, post_id, position, id);

-- Legacy inserts do not yet carry workspace_id. Resolve them to the owner's
-- personal workspace inside PostgreSQL so old and new binaries can overlap
-- during a rolling deployment.
CREATE FUNCTION assign_personal_workspace_id() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.workspace_id IS NULL THEN
        SELECT id INTO NEW.workspace_id
        FROM workspaces
        WHERE owner_user_id = NEW.owner_id AND is_personal;
        IF NEW.workspace_id IS NULL THEN
            RAISE EXCEPTION 'personal workspace missing for owner %', NEW.owner_id;
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER channels_assign_personal_workspace
BEFORE INSERT ON channels FOR EACH ROW EXECUTE FUNCTION assign_personal_workspace_id();
CREATE TRIGGER posts_assign_personal_workspace
BEFORE INSERT ON posts FOR EACH ROW EXECUTE FUNCTION assign_personal_workspace_id();
CREATE FUNCTION fill_legacy_channel_claim_workspace() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.workspace_id IS NULL THEN
        SELECT id INTO NEW.workspace_id FROM workspaces
        WHERE owner_user_id=NEW.owner_id AND is_personal;
    END IF;
    IF NEW.requested_by_user_id IS NULL THEN
        NEW.requested_by_user_id := NEW.owner_id;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER channel_claims_fill_legacy_workspace
BEFORE INSERT ON channel_claims FOR EACH ROW EXECUTE FUNCTION fill_legacy_channel_claim_workspace();
CREATE TRIGGER media_assets_assign_personal_workspace
BEFORE INSERT ON media_assets FOR EACH ROW EXECUTE FUNCTION assign_personal_workspace_id();
CREATE FUNCTION fill_post_child_workspace_id() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.workspace_id IS NULL THEN
        SELECT workspace_id INTO NEW.workspace_id FROM posts WHERE id=NEW.post_id;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER post_attachments_fill_workspace
BEFORE INSERT ON post_attachments FOR EACH ROW EXECUTE FUNCTION fill_post_child_workspace_id();
CREATE TRIGGER post_view_snapshots_fill_workspace
BEFORE INSERT ON post_view_snapshots FOR EACH ROW EXECUTE FUNCTION fill_post_child_workspace_id();

ALTER TABLE posts
    ADD COLUMN review_status TEXT DEFAULT 'draft',
    ADD COLUMN current_revision_id BIGINT;
UPDATE posts SET review_status = 'draft' WHERE review_status IS NULL;
ALTER TABLE posts
    ADD CONSTRAINT posts_review_status_valid CHECK (
        review_status IS NOT NULL AND
        review_status IN ('draft', 'in_review', 'changes_requested', 'approved')
    ) NOT VALID;
ALTER TABLE posts VALIDATE CONSTRAINT posts_review_status_valid;

CREATE TABLE post_revisions (
    id BIGSERIAL PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    post_id BIGINT NOT NULL,
    revision_number INTEGER NOT NULL CHECK (revision_number > 0),
    author_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    snapshot JSONB NOT NULL CHECK (jsonb_typeof(snapshot) = 'object'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT post_revisions_workspace_post_fk
        FOREIGN KEY (workspace_id, post_id)
        REFERENCES posts(workspace_id, id) ON DELETE CASCADE,
    UNIQUE (workspace_id, post_id, revision_number),
    UNIQUE (workspace_id, post_id, id)
);

CREATE INDEX idx_post_revisions_post_created
    ON post_revisions(workspace_id, post_id, revision_number DESC);

INSERT INTO post_revisions(
    workspace_id, post_id, revision_number, author_user_id, snapshot, created_at
)
SELECT p.workspace_id, p.id, 1, p.owner_id,
       jsonb_build_object(
           'title', p.title,
           'content', p.content,
           'format', p.format,
           'channel_id', p.channel_id,
           'image_url', p.image_url,
           'image_path', p.image_path,
           'image_prompt', p.image_prompt,
           'link_buttons', p.link_buttons,
           'notify', p.notify,
           'disable_link_preview', p.disable_link_preview,
           'attachments', COALESCE((
               SELECT jsonb_agg(jsonb_build_object(
                   'id', pa.id, 'type', pa.type, 'position', pa.position,
                   'storage_key', pa.storage_key, 'processing_status', pa.processing_status,
                   'size_bytes', pa.size_bytes, 'mime_type', pa.mime_type,
                   'width', pa.width, 'height', pa.height, 'duration_ms', pa.duration_ms
               ) ORDER BY pa.position, pa.id)
               FROM post_attachments pa WHERE pa.post_id=p.id
           ), '[]'::jsonb)
       ),
       p.updated_at
FROM posts p
ON CONFLICT DO NOTHING;

UPDATE posts p SET current_revision_id = r.id
FROM post_revisions r
WHERE r.workspace_id = p.workspace_id
  AND r.post_id = p.id
  AND r.revision_number = 1
  AND p.current_revision_id IS NULL;

-- current_revision_id is intentionally not a reverse FK. post_revisions
-- already cascade from posts; a reverse FK would create a delete cycle. Store
-- transactions validate that the selected revision belongs to the same post.
CREATE INDEX idx_posts_current_revision ON posts(current_revision_id)
    WHERE current_revision_id IS NOT NULL;

CREATE TABLE post_reviews (
    id BIGSERIAL PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    post_id BIGINT NOT NULL,
    revision_id BIGINT NOT NULL,
    reviewer_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    decision TEXT NOT NULL CHECK (decision IN ('approved', 'changes_requested')),
    comment TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT post_reviews_revision_fk
        FOREIGN KEY (workspace_id, post_id, revision_id)
        REFERENCES post_revisions(workspace_id, post_id, id) ON DELETE CASCADE
);

CREATE INDEX idx_post_reviews_revision_created
    ON post_reviews(workspace_id, post_id, revision_id, created_at DESC, id DESC);

CREATE TABLE workspace_invitations (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    email TEXT NOT NULL DEFAULT '',
    target_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
    token_hash TEXT NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-fA-F]{64}$'),
    role TEXT NOT NULL CHECK (role IN ('editor', 'approver', 'viewer')),
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'accepted', 'revoked', 'expired')),
    invited_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    accepted_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at TIMESTAMPTZ NOT NULL,
    accepted_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    CHECK (expires_at > created_at)
);

CREATE UNIQUE INDEX idx_workspace_invitations_pending_email
    ON workspace_invitations(workspace_id, lower(email))
    WHERE status = 'pending' AND email <> '';
CREATE UNIQUE INDEX idx_workspace_invitations_pending_user
    ON workspace_invitations(workspace_id, target_user_id)
    WHERE status = 'pending' AND target_user_id IS NOT NULL;
CREATE INDEX idx_workspace_invitations_workspace_created
    ON workspace_invitations(workspace_id, created_at DESC, id);
CREATE INDEX idx_workspace_invitations_pending_expiry
    ON workspace_invitations(expires_at) WHERE status = 'pending';

CREATE TABLE post_comments (
    id BIGSERIAL PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    post_id BIGINT NOT NULL,
    revision_id BIGINT,
    parent_id BIGINT,
    author_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    body TEXT NOT NULL CHECK (deleted_at IS NOT NULL OR btrim(body) <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at TIMESTAMPTZ,
    resolved_at TIMESTAMPTZ,
    resolved_by_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
    CONSTRAINT post_comments_post_fk
        FOREIGN KEY (workspace_id, post_id)
        REFERENCES posts(workspace_id, id) ON DELETE CASCADE,
    CONSTRAINT post_comments_revision_fk
        FOREIGN KEY (workspace_id, post_id, revision_id)
        REFERENCES post_revisions(workspace_id, post_id, id) ON DELETE CASCADE,
    UNIQUE (workspace_id, post_id, id),
    CONSTRAINT post_comments_parent_fk
        FOREIGN KEY (workspace_id, post_id, parent_id)
        REFERENCES post_comments(workspace_id, post_id, id) ON DELETE SET NULL (parent_id),
    CHECK ((resolved_at IS NULL) = (resolved_by_user_id IS NULL))
);

CREATE INDEX idx_post_comments_post_created
    ON post_comments(workspace_id, post_id, created_at, id);

CREATE TABLE audit_events (
    id BIGSERIAL PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    actor_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
    action TEXT NOT NULL CHECK (action ~ '^[a-z][a-z0-9_.-]{0,63}$'),
    entity_type TEXT NOT NULL CHECK (entity_type ~ '^[a-z][a-z0-9_.-]{0,31}$'),
    entity_id TEXT NOT NULL DEFAULT '',
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_audit_events_workspace_id_desc
    ON audit_events(workspace_id, id DESC);

CREATE TABLE notifications (
    id BIGSERIAL PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind ~ '^[a-z][a-z0-9_.-]{0,63}$'),
    title TEXT NOT NULL DEFAULT '',
    body TEXT NOT NULL DEFAULT '',
    entity_type TEXT NOT NULL DEFAULT '',
    entity_id TEXT NOT NULL DEFAULT '',
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object'),
    dedupe_key TEXT,
    read_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX idx_notifications_dedupe
    ON notifications(user_id, dedupe_key) WHERE dedupe_key IS NOT NULL;
CREATE INDEX idx_notifications_user_created
    ON notifications(user_id, id DESC);
CREATE INDEX idx_notifications_user_unread
    ON notifications(user_id, id DESC) WHERE read_at IS NULL;

-- Any payload edit invalidates the selected/approved revision. Scheduling is
-- deliberately excluded: an approved payload may be moved on the calendar.
CREATE FUNCTION invalidate_post_review_on_payload_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.title IS DISTINCT FROM NEW.title
       OR OLD.content IS DISTINCT FROM NEW.content
       OR OLD.format IS DISTINCT FROM NEW.format
       OR OLD.channel_id IS DISTINCT FROM NEW.channel_id
       OR OLD.image_url IS DISTINCT FROM NEW.image_url
       OR OLD.image_path IS DISTINCT FROM NEW.image_path
       OR OLD.image_prompt IS DISTINCT FROM NEW.image_prompt
       OR OLD.link_buttons IS DISTINCT FROM NEW.link_buttons
       OR OLD.notify IS DISTINCT FROM NEW.notify
       OR OLD.disable_link_preview IS DISTINCT FROM NEW.disable_link_preview THEN
        NEW.review_status := 'draft';
        NEW.current_revision_id := NULL;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER posts_invalidate_review_on_payload_change
BEFORE UPDATE ON posts FOR EACH ROW EXECUTE FUNCTION invalidate_post_review_on_payload_change();

CREATE FUNCTION invalidate_post_review_on_attachment_change() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    affected_post_id BIGINT;
    payload_changed BOOLEAN := TRUE;
BEGIN
    IF TG_OP = 'DELETE' THEN
        affected_post_id := OLD.post_id;
    ELSE
        affected_post_id := NEW.post_id;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        payload_changed := OLD.type IS DISTINCT FROM NEW.type
            OR OLD.position IS DISTINCT FROM NEW.position
            OR OLD.storage_key IS DISTINCT FROM NEW.storage_key
            OR OLD.processing_status IS DISTINCT FROM NEW.processing_status
            OR OLD.size_bytes IS DISTINCT FROM NEW.size_bytes
            OR OLD.mime_type IS DISTINCT FROM NEW.mime_type
            OR OLD.width IS DISTINCT FROM NEW.width
            OR OLD.height IS DISTINCT FROM NEW.height
            OR OLD.duration_ms IS DISTINCT FROM NEW.duration_ms;
    END IF;
    IF payload_changed THEN
        UPDATE posts SET review_status='draft',current_revision_id=NULL,updated_at=CURRENT_TIMESTAMP
        WHERE id=affected_post_id;
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER post_attachments_invalidate_review
AFTER INSERT OR UPDATE OR DELETE ON post_attachments
FOR EACH ROW EXECUTE FUNCTION invalidate_post_review_on_attachment_change();

-- Workspace-scoped quota ledgers coexist with the legacy per-user ledgers.
-- Existing personal usage is copied once; team usage never touches owner_id
-- balances and can move to these tables route by route.
CREATE TABLE workspace_ai_usage_buckets (
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    operation TEXT NOT NULL CHECK (operation ~ '^[a-z][a-z0-9_]{0,31}$'),
    bucket_kind TEXT NOT NULL CHECK (bucket_kind IN ('minute', 'day')),
    window_start TIMESTAMPTZ NOT NULL,
    used INTEGER NOT NULL CHECK (used > 0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (workspace_id, operation, bucket_kind)
);

CREATE TABLE workspace_ai_request_leases (
    id TEXT PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    operation TEXT NOT NULL CHECK (operation ~ '^[a-z][a-z0-9_]{0,31}$'),
    acquired_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > acquired_at)
);

CREATE INDEX idx_workspace_ai_leases_scope_expiry
    ON workspace_ai_request_leases(workspace_id, operation, expires_at);
CREATE INDEX idx_workspace_ai_leases_expiry
    ON workspace_ai_request_leases(expires_at);

CREATE TABLE workspace_media_usage (
    workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE RESTRICT,
    asset_count BIGINT NOT NULL DEFAULT 0 CHECK (asset_count >= 0),
    total_bytes BIGINT NOT NULL DEFAULT 0 CHECK (total_bytes >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO workspace_ai_usage_buckets(
    workspace_id, operation, bucket_kind, window_start, used, updated_at
)
SELECT w.id, b.operation, b.bucket_kind, b.window_start, b.used, b.updated_at
FROM ai_usage_buckets b
JOIN workspaces w ON w.owner_user_id=b.owner_id AND w.is_personal
ON CONFLICT DO NOTHING;

INSERT INTO workspace_ai_request_leases(id,workspace_id,operation,acquired_at,expires_at)
SELECT l.id,w.id,l.operation,l.acquired_at,l.expires_at
FROM ai_request_leases l
JOIN workspaces w ON w.owner_user_id=l.owner_id AND w.is_personal
ON CONFLICT DO NOTHING;

INSERT INTO workspace_media_usage(workspace_id,asset_count,total_bytes,updated_at)
SELECT w.id,u.asset_count,u.total_bytes,u.updated_at
FROM media_usage u
JOIN workspaces w ON w.owner_user_id=u.owner_id AND w.is_personal
ON CONFLICT DO NOTHING;

-- Every workspace child write takes a key-share lock on an active workspace.
-- Archival takes FOR UPDATE on the same row, so an in-flight create/update is
-- either observed before archival or rejected after it; it cannot commit an
-- invisible scheduled post, channel, invitation, comment, or media asset.
CREATE FUNCTION require_active_workspace_child_write() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM 1 FROM workspaces
    WHERE id=NEW.workspace_id AND archived_at IS NULL
    FOR KEY SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING
            ERRCODE = '23503',
            MESSAGE = 'workspace is archived or missing';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON workspace_members
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON channels
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON posts
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON channel_claims
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON media_assets
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON post_attachments
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON post_view_snapshots
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON post_revisions
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON post_reviews
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON workspace_invitations
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON post_comments
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON audit_events
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON notifications
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON workspace_ai_usage_buckets
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON workspace_ai_request_leases
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
