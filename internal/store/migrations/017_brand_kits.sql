-- Workspace editorial identity and reusable channel-specific overrides.
-- This migration is expand-only: existing workspace/content columns and
-- compatibility paths remain untouched.

CREATE TABLE workspace_brand_kits (
    workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    audience TEXT NOT NULL DEFAULT '' CHECK (char_length(audience) <= 500),
    tone TEXT NOT NULL DEFAULT '' CHECK (char_length(tone) <= 100),
    cta TEXT NOT NULL DEFAULT '' CHECK (char_length(cta) <= 500),
    forbidden_words TEXT[] NOT NULL DEFAULT '{}'::TEXT[]
        CHECK (cardinality(forbidden_words) <= 50 AND array_position(forbidden_words, NULL) IS NULL),
    example_posts TEXT[] NOT NULL DEFAULT '{}'::TEXT[]
        CHECK (cardinality(example_posts) <= 10 AND array_position(example_posts, NULL) IS NULL),
    visual_style TEXT NOT NULL DEFAULT '' CHECK (char_length(visual_style) <= 1000),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO workspace_brand_kits(workspace_id, created_at, updated_at)
SELECT id, created_at, created_at FROM workspaces
ON CONFLICT DO NOTHING;

CREATE FUNCTION create_workspace_brand_kit() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO workspace_brand_kits(workspace_id, created_at, updated_at)
    VALUES (NEW.id, NEW.created_at, NEW.created_at)
    ON CONFLICT DO NOTHING;
    RETURN NEW;
END;
$$;

CREATE TRIGGER workspaces_create_brand_kit
AFTER INSERT ON workspaces
FOR EACH ROW EXECUTE FUNCTION create_workspace_brand_kit();

CREATE TABLE workspace_channel_templates (
    id BIGSERIAL PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    channel_id BIGINT,
    name TEXT NOT NULL CHECK (btrim(name) <> '' AND char_length(name) <= 120),
    audience TEXT NOT NULL DEFAULT '' CHECK (char_length(audience) <= 500),
    tone TEXT NOT NULL DEFAULT '' CHECK (char_length(tone) <= 100),
    cta TEXT NOT NULL DEFAULT '' CHECK (char_length(cta) <= 500),
    forbidden_words TEXT[] NOT NULL DEFAULT '{}'::TEXT[]
        CHECK (cardinality(forbidden_words) <= 50 AND array_position(forbidden_words, NULL) IS NULL),
    example_posts TEXT[] NOT NULL DEFAULT '{}'::TEXT[]
        CHECK (cardinality(example_posts) <= 10 AND array_position(example_posts, NULL) IS NULL),
    visual_style TEXT NOT NULL DEFAULT '' CHECK (char_length(visual_style) <= 1000),
    is_default BOOLEAN NOT NULL DEFAULT FALSE,
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT workspace_channel_templates_channel_fk
        FOREIGN KEY (workspace_id, channel_id)
        REFERENCES channels(workspace_id, id) ON DELETE CASCADE,
    CONSTRAINT workspace_channel_templates_default_scope CHECK (
        NOT is_default OR channel_id IS NULL
    )
);

CREATE UNIQUE INDEX idx_workspace_channel_templates_name
    ON workspace_channel_templates(workspace_id, lower(name));
CREATE UNIQUE INDEX idx_workspace_channel_templates_default
    ON workspace_channel_templates(workspace_id) WHERE is_default;
CREATE INDEX idx_workspace_channel_templates_channel
    ON workspace_channel_templates(workspace_id, channel_id, lower(name), id);

-- Archive serialization must cover every new workspace-scoped mutable table.
-- The shared function was introduced by migration 016.
CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON workspace_brand_kits
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();

CREATE TRIGGER zz_workspace_active_write_guard
BEFORE INSERT OR UPDATE ON workspace_channel_templates
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
