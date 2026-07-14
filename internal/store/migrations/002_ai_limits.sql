CREATE TABLE ai_usage_buckets (
    owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    operation TEXT NOT NULL CHECK (operation ~ '^[a-z][a-z0-9_]{0,31}$'),
    bucket_kind TEXT NOT NULL CHECK (bucket_kind IN ('minute', 'day')),
    window_start TIMESTAMPTZ NOT NULL,
    used INTEGER NOT NULL CHECK (used > 0),
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (owner_id, operation, bucket_kind)
);

CREATE TABLE ai_request_leases (
    id TEXT PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    operation TEXT NOT NULL CHECK (operation ~ '^[a-z][a-z0-9_]{0,31}$'),
    acquired_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > acquired_at)
);

CREATE INDEX idx_ai_request_leases_owner_operation_expires
    ON ai_request_leases(owner_id, operation, expires_at);
CREATE INDEX idx_ai_request_leases_expires_at
    ON ai_request_leases(expires_at);
