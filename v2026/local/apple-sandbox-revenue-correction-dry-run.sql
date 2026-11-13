-- Read-only dry run for local/apple-sandbox-revenue-correction.sql. It counts
-- exactly the rows that script would change, for the scope chosen with the
-- transaction-local setting urnetwork.apple_sandbox_include_pre_ledger
-- ('true' or 'false'; the script refuses anything else). It writes nothing:
--
--   SET statement_timeout = '20s';
--   BEGIN READ ONLY;
--   SELECT set_config('urnetwork.apple_sandbox_include_pre_ledger', 'false', true);
--   \i local/apple-sandbox-revenue-correction-dry-run.sql
--   ROLLBACK;
--
-- Every predicate here is repeated verbatim in the correction script, which
-- refuses to commit unless its changed-row counts equal the counts this query
-- reports (model/apple_sandbox_revenue_correction_test.go runs both).
--
-- A sandbox App Store renewal is recognized by two independent signals, never
-- by one alone:
--   ledger:     apple_subscription_transaction row whose transaction id is in
--               the App Store sandbox id family (16 digits, prefix 200000), and
--               a paid period of exactly 24 hours (TestFlight renews daily;
--               no production monthly or yearly period is a day)
--   pre-ledger: renewal written by the handler before the transaction ledger
--               (keyed on appTransactionId, no ledger row) whose period, from
--               processing to expiry, is a TestFlight day (24 h +- 6 min) or a
--               sandbox 5-minute month (3 to 7 min)
WITH correction_scope AS (
    SELECT current_setting('urnetwork.apple_sandbox_include_pre_ledger', true) AS include_pre_ledger
), sandbox_renewal AS MATERIALIZED (
    SELECT renewal.network_id, renewal.subscription_type, renewal.market,
           renewal.start_time, renewal.end_time, renewal.transaction_id,
           renewal.net_revenue_nano_cents
    FROM subscription_renewal renewal
    INNER JOIN apple_subscription_transaction ledger
        ON ledger.transaction_id = renewal.transaction_id
       AND ledger.network_id = renewal.network_id
    WHERE renewal.market = 'apple'
      AND renewal.subscription_type = 'supporter'
      AND renewal.transaction_id ~ '^200000[0-9]{10}$'
      AND renewal.end_time - renewal.start_time = interval '48 hours'
    UNION ALL
    SELECT renewal.network_id, renewal.subscription_type, renewal.market,
           renewal.start_time, renewal.end_time, renewal.transaction_id,
           renewal.net_revenue_nano_cents
    FROM subscription_renewal renewal
    WHERE (SELECT include_pre_ledger FROM correction_scope) = 'true'
      AND renewal.market = 'apple'
      AND renewal.subscription_type = 'supporter'
      AND NOT EXISTS (
          SELECT 1 FROM apple_subscription_transaction ledger
          WHERE ledger.transaction_id = renewal.transaction_id
      )
      AND (
          renewal.end_time - renewal.start_time - interval '24 hours'
              BETWEEN interval '23 hours 54 minutes' AND interval '24 hours 6 minutes'
          OR renewal.end_time - renewal.start_time - interval '24 hours'
              BETWEEN interval '3 minutes' AND interval '7 minutes'
      )
), sandbox_credit_balance AS MATERIALIZED (
    SELECT balance.balance_id, balance.subsidy_net_revenue_nano_cents, balance.end_time
    FROM sandbox_renewal renewal
    INNER JOIN transfer_balance balance
        ON balance.network_id = renewal.network_id
       AND balance.pro
       AND balance.start_time = renewal.start_time
       AND balance.end_time = renewal.end_time
    WHERE balance.net_revenue_nano_cents = 0
      AND balance.subsidy_net_revenue_nano_cents <> 0
), sandbox_grant_balance AS MATERIALIZED (
    SELECT balance.balance_id, balance.subsidy_net_revenue_nano_cents, balance.end_time
    FROM transfer_balance balance
    WHERE balance.pro
      AND COALESCE(balance.grant_kind, '') IN ('', 'pro')
      AND balance.net_revenue_nano_cents = 0
      AND balance.subsidy_net_revenue_nano_cents <> 0
      AND balance.network_id IN (SELECT network_id FROM sandbox_renewal)
      AND NOT EXISTS (
          SELECT 1 FROM subscription_renewal any_renewal
          WHERE any_renewal.network_id = balance.network_id
            AND any_renewal.start_time = balance.start_time
            AND any_renewal.end_time = balance.end_time
      )
      AND EXISTS (
          SELECT 1 FROM sandbox_renewal renewal
          WHERE renewal.network_id = balance.network_id
            AND renewal.start_time <= balance.start_time
            AND balance.start_time < renewal.end_time
      )
      AND NOT EXISTS (
          SELECT 1 FROM subscription_renewal other
          WHERE other.network_id = balance.network_id
            AND other.subscription_type = 'supporter'
            AND other.start_time < balance.start_time + interval '1 day'
            AND balance.start_time < other.end_time
            AND NOT EXISTS (
                SELECT 1 FROM sandbox_renewal renewal
                WHERE renewal.network_id = other.network_id
                  AND renewal.market = other.market
                  AND renewal.start_time = other.start_time
                  AND renewal.end_time = other.end_time
            )
      )
)
SELECT (SELECT include_pre_ledger FROM correction_scope) AS include_pre_ledger,
       (SELECT count(*) FROM sandbox_renewal WHERE net_revenue_nano_cents <> 0) AS renewals,
       (SELECT round(COALESCE(sum(net_revenue_nano_cents), 0) / 1e9, 2)
        FROM sandbox_renewal WHERE net_revenue_nano_cents <> 0) AS renewal_net_revenue_usd,
       (SELECT count(DISTINCT network_id) FROM sandbox_renewal) AS networks,
       (SELECT count(*) FROM sandbox_credit_balance) AS credit_balances,
       (SELECT round(COALESCE(sum(subsidy_net_revenue_nano_cents), 0) / 1e9, 2)
        FROM sandbox_credit_balance) AS credit_subsidy_usd,
       (SELECT count(*) FROM sandbox_grant_balance) AS grant_balances,
       (SELECT round(COALESCE(sum(subsidy_net_revenue_nano_cents), 0) / 1e9, 2)
        FROM sandbox_grant_balance) AS grant_subsidy_usd,
       (SELECT count(*) FROM sandbox_credit_balance WHERE now() < end_time)
           + (SELECT count(*) FROM sandbox_grant_balance WHERE now() < end_time) AS open_balances
