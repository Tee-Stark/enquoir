-- A revocation the merchant asks for waits for the customer to confirm
-- it from an emailed link. Meanwhile the mandate grants nothing; if the
-- hour passes unconfirmed, the request lapses.
ALTER TABLE commerce_mandates
    ADD COLUMN revocation_requested_at TIMESTAMPTZ,
    ADD COLUMN revocation_expires_at   TIMESTAMPTZ,
    ADD CONSTRAINT commerce_mandates_revocation_window
        CHECK ((revocation_requested_at IS NULL) = (revocation_expires_at IS NULL));
