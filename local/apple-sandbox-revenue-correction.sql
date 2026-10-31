-- Explicit operator correction only; it needs the owner's OK before it runs.
-- No server path invokes it.
--
-- Before the controller fix that records no revenue for a sandbox App Store
-- purchase, every TestFlight and App Review credit recorded the purchase price
-- as revenue. This zeroes that recorded revenue on the already-credited rows,
-- and nothing else:
--
--   subscription_renewal.net_revenue_nano_cents          := 0
--       on each sandbox App Store supporter renewal (the rows the dry run
--       counts as renewals)
--   transfer_balance.subsidy_net_revenue_nano_cents      := 0
--       on the Pro balance each sandbox credit wrote: same network, pro, and
--       the renewal's exact window (credit_balances)
--   transfer_balance.subsidy_net_revenue_nano_cents      := 0
--       on a monthly Pro grant whose pro-rated subscription revenue came only
--       from sandbox renewals: it starts inside a sandbox renewal, and no
--       other supporter renewal of the network overlaps its first day
--       (grant_balances). A grant that shared a production renewal is left
--       alone.
--
-- Entitlements are untouched: no renewal window, Pro flag, balance window or
-- byte count changes, and no ledger row, notification, escrow, sweep or payment
-- is touched. transfer_balance.paid is generated from the revenue columns, so a
-- corrected balance becomes unpaid: contracts it funds from now on carry unpaid
-- priority, and later payout plans count its traffic as unpaid. Payout plans
-- already created keep their computed subsidy. The apple_subscription_transaction
-- ledger keeps each transaction's price and currency.
--
-- Idempotent: only rows with nonzero revenue change, so a second run changes
-- nothing (its dry run reports zero rows).
--
-- Run the dry run first, for the same scope, and pass its counts. The script
-- raises, so nothing commits, unless each changed-row count equals the count
-- passed, the transaction is serializable, and the scope is 'true' or 'false':
--
--   BEGIN ISOLATION LEVEL SERIALIZABLE;
--   SET LOCAL statement_timeout = '60s';
--   SELECT set_config('urnetwork.apple_sandbox_include_pre_ledger', 'false', true),
--          set_config('urnetwork.apple_sandbox_expected_renewals', '<renewals>', true),
--          set_config('urnetwork.apple_sandbox_expected_credit_balances', '<credit_balances>', true),
--          set_config('urnetwork.apple_sandbox_expected_grant_balances', '<grant_balances>', true);
--   \i local/apple-sandbox-revenue-correction.sql
--   SELECT current_setting('urnetwork.apple_sandbox_revenue_correction_receipt');
--   COMMIT;
--
-- The predicates are the dry run's, verbatim
-- (local/apple-sandbox-revenue-correction-dry-run.sql documents the two
-- signals). model/apple_sandbox_revenue_correction_test.go runs both scripts.
DO $apple_sandbox_revenue_correction$
DECLARE
    chosen_scope text := current_setting('urnetwork.apple_sandbox_include_pre_ledger', true);
    expected_renewals bigint;
    expected_credit_balances bigint;
    expected_grant_balances bigint;
    changed bigint;
BEGIN
    IF current_setting('transaction_isolation') <> 'serializable' THEN
        RAISE EXCEPTION 'apple sandbox revenue correction requires a serializable transaction';
    END IF;
    IF chosen_scope IS NULL OR chosen_scope NOT IN ('true', 'false') THEN
        RAISE EXCEPTION 'urnetwork.apple_sandbox_include_pre_ledger must be true or false';
    END IF;
    BEGIN
        expected_renewals := current_setting('urnetwork.apple_sandbox_expected_renewals')::bigint;
        expected_credit_balances := current_setting('urnetwork.apple_sandbox_expected_credit_balances')::bigint;
        expected_grant_balances := current_setting('urnetwork.apple_sandbox_expected_grant_balances')::bigint;
    EXCEPTION WHEN others THEN
        RAISE EXCEPTION 'apple sandbox revenue correction needs the dry run counts as integers';
    END;
    IF expected_renewals < 0 OR expected_credit_balances < 0 OR expected_grant_balances < 0 THEN
        RAISE EXCEPTION 'apple sandbox revenue correction counts cannot be negative';
    END IF;
    -- a Pro balance in use is locked briefly by contract creation; wait for no
    -- convoy, the whole correction retries instead
    PERFORM set_config('lock_timeout', '2s', true);

    CREATE TEMP TABLE apple_sandbox_renewal ON COMMIT DROP AS
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
    )
    SELECT * FROM sandbox_renewal;

    CREATE TEMP TABLE apple_sandbox_credit_balance ON COMMIT DROP AS
    WITH sandbox_renewal AS (
        SELECT * FROM apple_sandbox_renewal
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
    )
    SELECT balance_id FROM sandbox_credit_balance;

    CREATE TEMP TABLE apple_sandbox_grant_balance ON COMMIT DROP AS
    WITH sandbox_renewal AS (
        SELECT * FROM apple_sandbox_renewal
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
    SELECT balance_id FROM sandbox_grant_balance;

    -- each set was read before any write; the subscription_renewal key is
    -- (network_id, subscription_type, end_time, start_time, market)
    UPDATE subscription_renewal renewal
    SET net_revenue_nano_cents = 0
    FROM apple_sandbox_renewal sandbox
    WHERE renewal.network_id = sandbox.network_id
      AND renewal.subscription_type = sandbox.subscription_type
      AND renewal.market = sandbox.market
      AND renewal.start_time = sandbox.start_time
      AND renewal.end_time = sandbox.end_time
      AND renewal.net_revenue_nano_cents <> 0;
    GET DIAGNOSTICS changed = ROW_COUNT;
    IF changed <> expected_renewals THEN
        RAISE EXCEPTION 'apple sandbox revenue correction would change % renewals, the dry run reported %', changed, expected_renewals;
    END IF;

    UPDATE transfer_balance
    SET subsidy_net_revenue_nano_cents = 0
    WHERE balance_id IN (SELECT balance_id FROM apple_sandbox_credit_balance)
      AND subsidy_net_revenue_nano_cents <> 0;
    GET DIAGNOSTICS changed = ROW_COUNT;
    IF changed <> expected_credit_balances THEN
        RAISE EXCEPTION 'apple sandbox revenue correction would change % credit balances, the dry run reported %', changed, expected_credit_balances;
    END IF;

    UPDATE transfer_balance
    SET subsidy_net_revenue_nano_cents = 0
    WHERE balance_id IN (SELECT balance_id FROM apple_sandbox_grant_balance)
      AND subsidy_net_revenue_nano_cents <> 0;
    GET DIAGNOSTICS changed = ROW_COUNT;
    IF changed <> expected_grant_balances THEN
        RAISE EXCEPTION 'apple sandbox revenue correction would change % grant balances, the dry run reported %', changed, expected_grant_balances;
    END IF;

    PERFORM set_config(
        'urnetwork.apple_sandbox_revenue_correction_receipt',
        format(
            'include_pre_ledger=%s renewals=%s credit_balances=%s grant_balances=%s',
            chosen_scope, expected_renewals, expected_credit_balances, expected_grant_balances
        ),
        true
    );
END
$apple_sandbox_revenue_correction$;
