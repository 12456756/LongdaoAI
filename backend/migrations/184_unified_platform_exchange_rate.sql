ALTER TABLE payment_orders ALTER COLUMN amount TYPE decimal(20,8);
ALTER TABLE payment_orders ALTER COLUMN refund_amount TYPE decimal(20,8);

-- Subscription prices and internal balances use USD.
UPDATE subscription_plans SET currency = 'USD' WHERE currency IS DISTINCT FROM 'USD';

INSERT INTO settings (key, value) VALUES ('platform_usd_to_cny_rate', '7.15')
ON CONFLICT (key) DO NOTHING;

DELETE FROM settings WHERE key IN (
    'BALANCE_RECHARGE_MULTIPLIER',
    'SUBSCRIPTION_USD_TO_CNY_RATE',
    'balance_display_cny_rate'
);
