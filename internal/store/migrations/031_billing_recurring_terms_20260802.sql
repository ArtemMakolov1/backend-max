-- Keep prior immutable consent evidence and its deployed constraint untouched
-- while recording newly accepted recurring-payment terms in an additive
-- version column. A later contract migration may consolidate the columns after
-- every old application release has drained.
ALTER TABLE billing_recurring_consents
    ADD COLUMN accepted_terms_version_v2 TEXT;

ALTER TABLE billing_recurring_consents
    ADD CONSTRAINT billing_recurring_consents_accepted_terms_version_v2_check
    CHECK (
        accepted_terms_version_v2 IS NULL
        OR accepted_terms_version_v2 = '2026-08-02'
    ) NOT VALID;

ALTER TABLE billing_recurring_consents
    VALIDATE CONSTRAINT billing_recurring_consents_accepted_terms_version_v2_check;
