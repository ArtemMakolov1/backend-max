-- A draft can change after publication. Keep the delivered payload fingerprint
-- separate from the editable row so reopening an editor does not mistake saved
-- local changes for a successful MAX update. Existing rows remain unknown until
-- the next confirmed delivery rather than guessing which version reached MAX.
ALTER TABLE posts ADD COLUMN max_published_fingerprint TEXT DEFAULT '';

ALTER TABLE posts ADD CONSTRAINT posts_max_published_fingerprint_valid CHECK (
    max_published_fingerprint IS NOT NULL AND
    (max_published_fingerprint = '' OR max_published_fingerprint ~ '^[0-9a-f]{64}$')
) NOT VALID;

ALTER TABLE posts VALIDATE CONSTRAINT posts_max_published_fingerprint_valid;
