-- Cache the complete read-only MAX profile used by channel setup and AI
-- recommendations. Nullable provider timestamps distinguish "not returned or
-- not synchronized yet" from a real event time.
ALTER TABLE channels
    ADD COLUMN description TEXT NOT NULL DEFAULT '',
    ADD COLUMN is_public BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN messages_count INTEGER NOT NULL DEFAULT 0 CHECK (messages_count >= 0),
    ADD COLUMN has_pinned_message BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN max_last_event_time TIMESTAMPTZ,
    ADD COLUMN max_info_synced_at TIMESTAMPTZ;

ALTER TABLE observed_bot_chats
    ADD COLUMN description TEXT NOT NULL DEFAULT '',
    ADD COLUMN is_public BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN messages_count INTEGER NOT NULL DEFAULT 0 CHECK (messages_count >= 0),
    ADD COLUMN has_pinned_message BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN max_last_event_time TIMESTAMPTZ,
    ADD COLUMN max_info_synced_at TIMESTAMPTZ;
