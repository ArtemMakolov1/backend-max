ALTER TABLE posts
    ADD COLUMN max_message_url TEXT DEFAULT '',
    ADD COLUMN max_views BIGINT,
    ADD COLUMN max_stats_synced_at TIMESTAMPTZ,
    ADD COLUMN max_stats_attempted_at TIMESTAMPTZ,
    ADD COLUMN max_is_pinned BOOLEAN DEFAULT FALSE;

UPDATE posts
SET max_message_url = COALESCE(max_message_url, ''),
    max_is_pinned = COALESCE(max_is_pinned, FALSE)
WHERE max_message_url IS NULL OR max_is_pinned IS NULL;

ALTER TABLE posts
    ADD CONSTRAINT posts_max_message_url_not_null
        CHECK (max_message_url IS NOT NULL) NOT VALID,
    ADD CONSTRAINT posts_max_is_pinned_not_null
        CHECK (max_is_pinned IS NOT NULL) NOT VALID,
    ADD CONSTRAINT posts_max_views_nonnegative
        CHECK (max_views IS NULL OR max_views >= 0) NOT VALID;

ALTER TABLE posts
    VALIDATE CONSTRAINT posts_max_message_url_not_null;
ALTER TABLE posts
    VALIDATE CONSTRAINT posts_max_is_pinned_not_null;
ALTER TABLE posts
    VALIDATE CONSTRAINT posts_max_views_nonnegative;

CREATE INDEX idx_posts_owner_channel_pinned
    ON posts(owner_id, channel_id)
    WHERE max_is_pinned;

CREATE INDEX idx_posts_stats_due
    ON posts((COALESCE(max_stats_attempted_at, max_stats_synced_at, published_at, created_at)), id)
    WHERE status = 'published' AND max_message_id <> '' AND channel_id IS NOT NULL;

ALTER TABLE posts
    ADD CONSTRAINT posts_owner_id_unique UNIQUE (owner_id, id);

CREATE TABLE post_view_snapshots (
    id BIGSERIAL PRIMARY KEY,
    owner_id TEXT NOT NULL,
    post_id BIGINT NOT NULL,
    max_message_id TEXT NOT NULL,
    views BIGINT NOT NULL CHECK (views >= 0),
    captured_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT post_view_snapshots_owner_post_fk
        FOREIGN KEY (owner_id, post_id) REFERENCES posts(owner_id, id) ON DELETE CASCADE
);

CREATE INDEX idx_post_view_snapshots_owner_captured
    ON post_view_snapshots(owner_id, captured_at);
CREATE INDEX idx_post_view_snapshots_owner_post_captured
    ON post_view_snapshots(owner_id, post_id, captured_at DESC, id DESC);
CREATE INDEX idx_post_view_snapshots_post_publication_captured
    ON post_view_snapshots(post_id, max_message_id, captured_at DESC);
