-- A merchant may identify a saved payment method by its PSP's own ID
-- (Stripe's pm_..., Adyen's storedPaymentMethodId) instead of, or as
-- well as, a reference of its own. Each is unique per merchant (the
-- PSP's per provider); a binding has at least one.
ALTER TABLE commerce_funding_methods
    ALTER COLUMN merchant_funding_method_id DROP NOT NULL,
    ADD COLUMN provider_method_reference TEXT,
    ADD CONSTRAINT commerce_funding_methods_has_reference
        CHECK (merchant_funding_method_id IS NOT NULL OR provider_method_reference IS NOT NULL);
CREATE UNIQUE INDEX commerce_funding_methods_provider_reference
    ON commerce_funding_methods (merchant_id, provider, provider_method_reference)
    WHERE provider_method_reference IS NOT NULL;
