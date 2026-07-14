-- One row per account and UTC calendar day is sufficient for DAU/WAU/MAU.
-- We deliberately do not persist routes, user agents, IP addresses, or event
-- payloads here: product metrics only need a pseudonymous per-day marker that
-- is exported solely as aggregate counts.
-- The independent product-analytics refresh removes markers older than 35
-- days, covering the 30-day MAU window with a small processing margin.
CREATE TABLE user_activity_daily (
    owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    activity_date DATE NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (owner_id, activity_date),
    CONSTRAINT user_activity_daily_utc_date_consistency CHECK (
        activity_date = (last_seen_at AT TIME ZONE 'UTC')::date
    )
);

CREATE INDEX idx_user_activity_daily_date_owner
    ON user_activity_daily(activity_date, owner_id);
