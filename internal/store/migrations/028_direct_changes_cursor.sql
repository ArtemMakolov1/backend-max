-- Provider cursor used by Changes.check. It is advanced only after the
-- corresponding full graph refresh (when required) has completed.
ALTER TABLE direct_campaigns
    ADD COLUMN provider_changes_timestamp TIMESTAMPTZ;
