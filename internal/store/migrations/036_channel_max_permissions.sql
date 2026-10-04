-- Keep webhook access observations independent of the user's active toggle.
-- Event ordering also deduplicates deliveries before notifying the workspace.
CREATE TABLE channel_max_permissions (
    channel_id BIGINT PRIMARY KEY REFERENCES channels(id) ON DELETE CASCADE,
    can_publish BOOLEAN NOT NULL,
    event_at TIMESTAMPTZ NOT NULL,
    checked_at TIMESTAMPTZ NOT NULL
);
