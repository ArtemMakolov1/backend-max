ALTER TABLE users
    ADD COLUMN avatar_url TEXT DEFAULT '';

UPDATE users
SET avatar_url = ''
WHERE avatar_url IS NULL;

ALTER TABLE users
    ADD CONSTRAINT users_avatar_url_not_null
    CHECK (avatar_url IS NOT NULL) NOT VALID;

ALTER TABLE users
    VALIDATE CONSTRAINT users_avatar_url_not_null;

ALTER TABLE auth_sessions
    ADD COLUMN avatar_url TEXT DEFAULT '';

UPDATE auth_sessions
SET avatar_url = ''
WHERE avatar_url IS NULL;

ALTER TABLE auth_sessions
    ADD CONSTRAINT auth_sessions_avatar_url_not_null
    CHECK (avatar_url IS NOT NULL) NOT VALID;

ALTER TABLE auth_sessions
    VALIDATE CONSTRAINT auth_sessions_avatar_url_not_null;
