-- Cache the complete read-only MAX profile used by channel setup and AI
-- recommendations. Nullable provider timestamps distinguish "not returned or
-- not synchronized yet" from a real event time. Defaults plus validated CHECK
-- constraints preserve required-field invariants during the additive phase.
ALTER TABLE channels
    ADD COLUMN description TEXT DEFAULT '',
    ADD COLUMN is_public BOOLEAN DEFAULT FALSE,
    ADD COLUMN messages_count INTEGER DEFAULT 0,
    ADD COLUMN has_pinned_message BOOLEAN DEFAULT FALSE,
    ADD COLUMN max_last_event_time TIMESTAMPTZ,
    ADD COLUMN max_info_synced_at TIMESTAMPTZ;

ALTER TABLE channels
    ADD CONSTRAINT channels_description_not_null
        CHECK (description IS NOT NULL) NOT VALID,
    ADD CONSTRAINT channels_is_public_not_null
        CHECK (is_public IS NOT NULL) NOT VALID,
    ADD CONSTRAINT channels_messages_count_valid
        CHECK (messages_count IS NOT NULL AND messages_count >= 0) NOT VALID,
    ADD CONSTRAINT channels_has_pinned_message_not_null
        CHECK (has_pinned_message IS NOT NULL) NOT VALID;

ALTER TABLE channels VALIDATE CONSTRAINT channels_description_not_null;
ALTER TABLE channels VALIDATE CONSTRAINT channels_is_public_not_null;
ALTER TABLE channels VALIDATE CONSTRAINT channels_messages_count_valid;
ALTER TABLE channels VALIDATE CONSTRAINT channels_has_pinned_message_not_null;

ALTER TABLE observed_bot_chats
    ADD COLUMN description TEXT DEFAULT '',
    ADD COLUMN is_public BOOLEAN DEFAULT FALSE,
    ADD COLUMN messages_count INTEGER DEFAULT 0,
    ADD COLUMN has_pinned_message BOOLEAN DEFAULT FALSE,
    ADD COLUMN max_last_event_time TIMESTAMPTZ,
    ADD COLUMN max_info_synced_at TIMESTAMPTZ;

ALTER TABLE observed_bot_chats
    ADD CONSTRAINT observed_bot_chats_description_not_null
        CHECK (description IS NOT NULL) NOT VALID,
    ADD CONSTRAINT observed_bot_chats_is_public_not_null
        CHECK (is_public IS NOT NULL) NOT VALID,
    ADD CONSTRAINT observed_bot_chats_messages_count_valid
        CHECK (messages_count IS NOT NULL AND messages_count >= 0) NOT VALID,
    ADD CONSTRAINT observed_bot_chats_has_pinned_message_not_null
        CHECK (has_pinned_message IS NOT NULL) NOT VALID;

ALTER TABLE observed_bot_chats VALIDATE CONSTRAINT observed_bot_chats_description_not_null;
ALTER TABLE observed_bot_chats VALIDATE CONSTRAINT observed_bot_chats_is_public_not_null;
ALTER TABLE observed_bot_chats VALIDATE CONSTRAINT observed_bot_chats_messages_count_valid;
ALTER TABLE observed_bot_chats VALIDATE CONSTRAINT observed_bot_chats_has_pinned_message_not_null;
