-- Existing files lived only on the previous server volume. Their post links
-- are intentionally discarded during the private S3 cutover; post text,
-- prompts, publication metadata and schedules remain intact. Ownership rows
-- stay only until migration 014 can quarantine them with additive columns.
-- Once this cutover starts, deployment is roll-forward only: the retired
-- local-media backend must not be restarted against the advanced schema.
UPDATE posts
SET image_url = '',
    image_path = ''
WHERE image_url <> '' OR image_path <> '';
