-- Refresh tokens rotate on every successful Yandex OAuth refresh. The lease
-- serializes that external mutation across backend instances; the encrypted
-- access/refresh token bundle itself remains in token_ciphertext.
ALTER TABLE direct_connections
    ADD COLUMN token_refresh_claimed_at TIMESTAMPTZ;

-- Product safety ceiling: 10,000 RUB per campaign per week. NOT VALID keeps
-- the migration deployable if an operator imported an older, larger campaign,
-- while PostgreSQL still enforces the cap for every new or changed row.
ALTER TABLE direct_campaigns
    ADD CONSTRAINT direct_campaigns_weekly_budget_safety_cap
    CHECK (weekly_budget_minor <= 1000000) NOT VALID;
