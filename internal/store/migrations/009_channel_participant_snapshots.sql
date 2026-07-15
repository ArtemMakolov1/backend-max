ALTER TABLE channels
    ADD COLUMN participants_stats_attempted_at TIMESTAMPTZ,
    ADD COLUMN participants_stats_synced_at TIMESTAMPTZ;

CREATE INDEX idx_channels_participant_stats_due
    ON channels (
        (COALESCE(participants_stats_attempted_at, '-infinity'::timestamptz)),
        id
    )
    WHERE active;

-- Participant history deliberately contains no owner identity, MAX chat ID,
-- channel title or public link. Tenant authorization is always enforced by
-- joining back to channels before reading or writing a snapshot.
CREATE TABLE channel_participant_snapshots (
    channel_id BIGINT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    observed_on DATE NOT NULL,
    captured_at TIMESTAMPTZ NOT NULL,
    participants_count INTEGER NOT NULL CHECK (participants_count >= 0),
    PRIMARY KEY (channel_id, observed_on)
);

CREATE INDEX idx_channel_participant_snapshots_channel_captured
    ON channel_participant_snapshots(channel_id, captured_at DESC);
