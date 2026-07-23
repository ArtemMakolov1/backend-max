-- Accept the current 2026-07-23 recurring terms snapshot without rewriting
-- immutable consent evidence recorded under the preceding 2026-07-22 terms.
ALTER TABLE billing_recurring_consents
    DROP CONSTRAINT billing_recurring_consents_terms_version_check;

ALTER TABLE billing_recurring_consents
    ADD CONSTRAINT billing_recurring_consents_terms_version_check
    CHECK (terms_version IN ('2026-07-22', '2026-07-23')) NOT VALID;

ALTER TABLE billing_recurring_consents
    VALIDATE CONSTRAINT billing_recurring_consents_terms_version_check;
