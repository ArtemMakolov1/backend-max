-- Ordered post media. S3 remains the canonical object store while rows in
-- media_assets account for tenant ownership/quota. Provider upload tokens are
-- intentionally internal cache data and are never exposed through the API.

CREATE TABLE post_attachments (
    id BIGSERIAL PRIMARY KEY,
    owner_id TEXT NOT NULL,
    post_id BIGINT NOT NULL,
    type TEXT NOT NULL CHECK (type IN ('image', 'video')),
    position INTEGER NOT NULL CHECK (position >= 0),
    storage_key TEXT NOT NULL CHECK (storage_key <> '' AND storage_key !~ '[/\\]'),
    processing_status TEXT NOT NULL DEFAULT 'ready'
        CHECK (processing_status IN ('uploading', 'processing', 'ready', 'failed')),
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    mime_type TEXT NOT NULL CHECK (mime_type <> ''),
    width INTEGER CHECK (width IS NULL OR width > 0),
    height INTEGER CHECK (height IS NULL OR height > 0),
    duration_ms BIGINT CHECK (duration_ms IS NULL OR duration_ms >= 0),
    provider_token TEXT NOT NULL DEFAULT '',
    provider_token_expires_at TIMESTAMPTZ,
    provider_meta JSONB NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(provider_meta) = 'object'),
    error_code TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT post_attachments_post_fk
        FOREIGN KEY (owner_id, post_id) REFERENCES posts(owner_id, id) ON DELETE CASCADE,
    CONSTRAINT post_attachments_media_fk
        FOREIGN KEY (owner_id, storage_key) REFERENCES media_assets(owner_id, filename) ON DELETE RESTRICT,
    UNIQUE (owner_id, post_id, position)
);

CREATE INDEX idx_post_attachments_post_position
    ON post_attachments(owner_id, post_id, position, id);
CREATE INDEX idx_post_attachments_storage_key
    ON post_attachments(owner_id, storage_key);
CREATE INDEX idx_post_attachments_processing
    ON post_attachments(processing_status, updated_at)
    WHERE processing_status <> 'ready';

-- Migration 013 intentionally removed references to unavailable local files.
-- Only current quota-accounted S3 objects are safe to backfill.
INSERT INTO post_attachments (
    owner_id, post_id, type, position, storage_key, processing_status,
    size_bytes, mime_type, created_at, updated_at
)
SELECT
    p.owner_id,
    p.id,
    'image',
    0,
    p.image_path,
    'ready',
    ma.size_bytes,
    CASE lower(substring(p.image_path from '[.][^.]+$'))
        WHEN '.jpg' THEN 'image/jpeg'
        WHEN '.jpeg' THEN 'image/jpeg'
        WHEN '.png' THEN 'image/png'
        WHEN '.gif' THEN 'image/gif'
        ELSE 'application/octet-stream'
    END,
    p.created_at,
    p.updated_at
FROM posts p
JOIN media_assets ma
  ON ma.owner_id = p.owner_id
 AND ma.filename = p.image_path
 AND ma.state = 'ready'
WHERE p.image_path <> ''
  AND lower(substring(p.image_path from '[.][^.]+$')) IN ('.jpg', '.jpeg', '.png', '.gif')
ON CONFLICT (owner_id, post_id, position) DO NOTHING;
