-- Existing files lived only on the previous server volume. Their post links
-- are intentionally discarded during the private S3 cutover; post text,
-- prompts, publication metadata and schedules remain intact. Ownership rows
-- stay for expand/rollback compatibility and can be compacted after old
-- releases and rollback snapshots are retired.
UPDATE posts
SET image_url = '',
    image_path = ''
WHERE image_url <> '' OR image_path <> '';
