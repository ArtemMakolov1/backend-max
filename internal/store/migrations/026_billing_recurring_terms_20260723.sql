-- Preserve immutable evidence recorded under the original terms_version
-- constraint while recording the terms version actually accepted by new users.
ALTER TABLE billing_recurring_consents
    ADD COLUMN accepted_terms_version TEXT
    CHECK (
        accepted_terms_version IS NULL
        OR accepted_terms_version IN ('2026-07-22', '2026-07-23')
    );
