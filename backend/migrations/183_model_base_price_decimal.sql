ALTER TABLE account_model_prices
    ADD COLUMN procurement_discount NUMERIC(24,12)
        GENERATED ALWAYS AS ((price->>'discount')::NUMERIC) STORED,
    ADD COLUMN company_markup NUMERIC(24,12)
        GENERATED ALWAYS AS ((price->>'markup')::NUMERIC) STORED,
    ADD CONSTRAINT account_model_prices_discount_check CHECK (procurement_discount BETWEEN 0 AND 1),
    ADD CONSTRAINT account_model_prices_markup_check CHECK (company_markup >= 0);
