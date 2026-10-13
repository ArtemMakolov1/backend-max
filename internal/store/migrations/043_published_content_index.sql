-- Local publication history for duplicate discovery filtering. No provider
-- requests or raw media metadata are stored in this editorial projection.
CREATE TABLE published_content_index (
    workspace_id TEXT NOT NULL,
    channel_id BIGINT NOT NULL,
    post_id BIGINT NOT NULL,
    content_hash TEXT NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    publication_fingerprint TEXT NOT NULL DEFAULT '',
    proof TEXT NOT NULL CHECK (proof IN ('confirmed', 'imported')),
    title TEXT NOT NULL CHECK (char_length(title) <= 200),
    content TEXT NOT NULL CHECK (char_length(content) <= 4000),
    source_urls JSONB NOT NULL CHECK (jsonb_typeof(source_urls) = 'array'),
    source_hashes TEXT[] NOT NULL DEFAULT '{}',
    search_vector TSVECTOR GENERATED ALWAYS AS
        (to_tsvector('russian'::regconfig, title || ' ' || content)) STORED,
    indexed_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (workspace_id, channel_id, post_id, content_hash),
    FOREIGN KEY (workspace_id, channel_id) REFERENCES channels(workspace_id, id) ON DELETE CASCADE
);
CREATE INDEX published_content_index_search ON published_content_index USING GIN(search_vector);
CREATE INDEX published_content_index_sources ON published_content_index USING GIN(source_hashes);
CREATE TABLE published_content_index_state (
    workspace_id TEXT NOT NULL,
    channel_id BIGINT NOT NULL,
    version INTEGER NOT NULL,
    last_post_id BIGINT NOT NULL DEFAULT 0,
    complete BOOLEAN NOT NULL DEFAULT FALSE,
    unverified_posts INTEGER NOT NULL DEFAULT 0 CHECK (unverified_posts >= 0),
    PRIMARY KEY (workspace_id, channel_id),
    FOREIGN KEY (workspace_id, channel_id) REFERENCES channels(workspace_id, id) ON DELETE CASCADE
);
CREATE TRIGGER published_content_index_active_workspace_guard
BEFORE INSERT OR UPDATE ON published_content_index
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
CREATE TRIGGER published_content_index_state_active_workspace_guard
BEFORE INSERT OR UPDATE ON published_content_index_state
FOR EACH ROW EXECUTE FUNCTION require_active_workspace_child_write();
