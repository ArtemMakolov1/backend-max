ALTER TABLE direct_campaigns
    ADD COLUMN bid_ceiling_minor BIGINT DEFAULT 0;

UPDATE direct_campaigns
SET bid_ceiling_minor = 0
WHERE bid_ceiling_minor IS NULL;

ALTER TABLE direct_campaigns
    ADD CONSTRAINT direct_campaigns_bid_ceiling_minor_check
    CHECK (bid_ceiling_minor IS NOT NULL AND bid_ceiling_minor >= 0) NOT VALID;

ALTER TABLE direct_campaigns
    VALIDATE CONSTRAINT direct_campaigns_bid_ceiling_minor_check;
