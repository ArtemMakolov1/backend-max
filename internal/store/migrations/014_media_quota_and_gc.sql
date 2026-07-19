-- Legacy files were stored on the retired server volume and cannot be copied
-- into the private S3 bucket reliably. Migration 013 already removed every
-- legacy image reference from posts. Quarantine the pre-existing ownership
-- rows as expired reservations so they can never be mistaken for quota-paid
-- S3 objects. The bounded orphan worker removes them immediately after the
-- new backend starts; a same-name upload is allowed only after that cleanup.

ALTER TABLE media_assets
    ADD COLUMN size_bytes BIGINT DEFAULT 0 CHECK (size_bytes IS DISTINCT FROM NULL AND size_bytes >= 0),
    ADD COLUMN state TEXT DEFAULT 'ready' CHECK (state IS DISTINCT FROM NULL AND state IN ('pending', 'ready')),
    ADD COLUMN reservation_token TEXT DEFAULT '' CHECK (reservation_token IS DISTINCT FROM NULL),
    ADD COLUMN updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP CHECK (updated_at IS DISTINCT FROM NULL);

UPDATE media_assets
SET state = 'pending',
    reservation_token = 'legacy-local-cutover',
    updated_at = TIMESTAMPTZ '1970-01-01 00:00:00+00'
WHERE state = 'ready'
  AND size_bytes = 0
  AND reservation_token = '';

CREATE INDEX idx_media_assets_state_updated
    ON media_assets(state, updated_at, owner_id, filename);

CREATE TABLE media_usage (
    owner_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    asset_count BIGINT NOT NULL DEFAULT 0 CHECK (asset_count >= 0),
    total_bytes BIGINT NOT NULL DEFAULT 0 CHECK (total_bytes >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- A queue is required because ON DELETE CASCADE can remove the last ownership
-- row when a user is deleted. The storage object is content-addressed and may
-- be shared by several tenants, so the worker always rechecks that no owner
-- remains before deleting the physical object.
CREATE TABLE media_gc_queue (
    filename TEXT PRIMARY KEY CHECK (filename <> '' AND filename !~ '[/\\]'),
    orphaned_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_media_gc_queue_orphaned_at
    ON media_gc_queue(orphaned_at, filename);

CREATE FUNCTION enqueue_media_object_gc() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO media_gc_queue(filename, orphaned_at)
    VALUES (OLD.filename, CURRENT_TIMESTAMP)
    ON CONFLICT(filename) DO UPDATE
        SET orphaned_at = LEAST(media_gc_queue.orphaned_at, EXCLUDED.orphaned_at);
    RETURN OLD;
END;
$$;

CREATE TRIGGER media_assets_enqueue_gc_after_delete
AFTER DELETE ON media_assets
FOR EACH ROW EXECUTE FUNCTION enqueue_media_object_gc();
