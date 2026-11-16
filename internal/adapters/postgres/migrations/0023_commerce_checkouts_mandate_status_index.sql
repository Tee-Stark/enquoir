-- lockMandate's usage query filters commerce_checkouts by mandate_id and
-- status (redeemed/paid, or authorized-and-unexpired). The existing
-- commerce_checkouts_authorized index only covers the authorized branch;
-- the redeemed/paid branch forced a sequential scan of the whole table,
-- held under the mandate's exclusive row lock. This covers both.
CREATE INDEX commerce_checkouts_mandate_status ON commerce_checkouts (mandate_id, status);
