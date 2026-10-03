// Explicit operator corrections retain earning attribution and cannot alter a
// processor attempt. Old writers cannot increment payout without this ledger.
package server

import (
	"context"
	"errors"
	"strings"
)

const providerPaymentBonusSchemaSql = `
ALTER TABLE account_payment ADD COLUMN bonus_payout_nano_cents bigint NOT NULL DEFAULT 0
    CHECK (bonus_payout_nano_cents >= 0);
ALTER TABLE account_payment ADD COLUMN attribution_review_required boolean NOT NULL DEFAULT false;

CREATE TABLE account_payment_bonus (
    operation_id uuid NOT NULL,
    origin_payment_id uuid NOT NULL REFERENCES account_payment(payment_id),
    payment_id uuid NOT NULL REFERENCES account_payment(payment_id),
    payment_plan_id uuid NOT NULL,
    network_id uuid NOT NULL,
    amount_nano_cents bigint NOT NULL CHECK (amount_nano_cents > 0),
    reason text NOT NULL CHECK (octet_length(reason) BETWEEN 1 AND 1024),
    latest_contract_close_time timestamp NOT NULL,
    subsidy_end_time timestamp NULL,
    policy_sha256 varchar(64) NOT NULL,
    create_time timestamp NOT NULL DEFAULT now(),
    PRIMARY KEY(operation_id, origin_payment_id)
);
CREATE INDEX account_payment_bonus_payment ON account_payment_bonus(payment_id);

CREATE FUNCTION guard_account_payment_bonus() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE source account_payment; target account_payment;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'provider bonus provenance cannot be deleted';
    END IF;
    IF TG_OP = 'UPDATE' THEN
        IF (to_jsonb(NEW) - 'payment_id') IS DISTINCT FROM (to_jsonb(OLD) - 'payment_id') THEN
            RAISE EXCEPTION 'provider bonus provenance is immutable';
        END IF;
        IF NEW.payment_id = OLD.payment_id THEN RETURN NEW; END IF;
        SELECT * INTO source FROM account_payment WHERE payment_id=OLD.payment_id FOR UPDATE;
        IF NOT source.canceled OR source.completed OR source.circle_idempotency_key IS NOT NULL
            OR source.payment_record IS NOT NULL OR source.tx_hash IS NOT NULL THEN
            RAISE EXCEPTION 'provider bonus source is not safely canceled';
        END IF;
    ELSIF NEW.payment_id <> NEW.origin_payment_id THEN
        RAISE EXCEPTION 'new provider bonus must bind its original payment';
    END IF;
    SELECT * INTO target FROM account_payment WHERE payment_id=NEW.payment_id FOR UPDATE;
    IF NOT FOUND OR target.network_id <> NEW.network_id OR target.completed OR target.canceled
        OR target.circle_idempotency_key IS NOT NULL OR target.payment_record IS NOT NULL OR target.tx_hash IS NOT NULL THEN
        RAISE EXCEPTION 'provider bonus target has a retained submission or invalid owner';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER account_payment_bonus_guard BEFORE INSERT OR UPDATE OR DELETE ON account_payment_bonus
    FOR EACH ROW EXECUTE FUNCTION guard_account_payment_bonus();

CREATE FUNCTION guard_account_payment_submission_basis() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE actual_bonus numeric;
BEGIN
    IF (NEW.network_id,NEW.wallet_id,NEW.payout_nano_cents,NEW.payout_byte_count,
        NEW.subsidy_payout_nano_cents,NEW.reliability_subsidy_nano_cents,NEW.bonus_payout_nano_cents)
        IS DISTINCT FROM
       (OLD.network_id,OLD.wallet_id,OLD.payout_nano_cents,OLD.payout_byte_count,
        OLD.subsidy_payout_nano_cents,OLD.reliability_subsidy_nano_cents,OLD.bonus_payout_nano_cents)
       AND (OLD.completed OR OLD.canceled OR OLD.circle_idempotency_key IS NOT NULL
            OR OLD.payment_record IS NOT NULL OR OLD.tx_hash IS NOT NULL) THEN
        RAISE EXCEPTION 'provider payment submission basis is immutable';
    END IF;
    IF NEW.payout_nano_cents IS DISTINCT FROM OLD.payout_nano_cents
        OR NEW.bonus_payout_nano_cents IS DISTINCT FROM OLD.bonus_payout_nano_cents THEN
        SELECT COALESCE(SUM(amount_nano_cents),0) INTO actual_bonus
            FROM account_payment_bonus WHERE payment_id=NEW.payment_id;
        IF NEW.bonus_payout_nano_cents <> actual_bonus
            OR NEW.payout_nano_cents::numeric - OLD.payout_nano_cents::numeric
                <> NEW.bonus_payout_nano_cents::numeric - OLD.bonus_payout_nano_cents::numeric THEN
            RAISE EXCEPTION 'provider payment amount change lacks exact bonus provenance';
        END IF;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER account_payment_submission_basis_guard BEFORE UPDATE ON account_payment
    FOR EACH ROW EXECUTE FUNCTION guard_account_payment_submission_basis();
`

// Require the consumed feature and exact installed guards, not equality with
// the global migration head. Later unrelated migrations remain compatible.
func RequireProviderPayoutSchema(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var migrationIndex = -1
	for index, migration := range migrations {
		if sql, ok := migration.(*SqlMigration); ok && sql.sql == providerPaymentBonusSchemaSql {
			migrationIndex = index
			break
		}
	}
	if migrationIndex < 0 {
		return errors.New("provider payout schema migration missing from binary")
	}
	identity, err := MigrationIdentity(migrationIndex)
	if err != nil {
		return err
	}
	// PostgreSQL stores the exact body between dollar delimiters. Bind each
	// installed trigger to that same function, enabled event mask and table.
	bodies := strings.Split(providerPaymentBonusSchemaSql, "$$")
	if len(bodies) != 5 {
		return errors.New("provider payout schema guard encoding invalid")
	}
	var ready bool
	var queryErr error
	Db(ctx, func(conn PgConn) {
		queryErr = conn.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM migration_catalog WHERE migration_index=$1 AND identity_sha256=$2)
		AND to_regclass('public.account_payment_bonus') IS NOT NULL
		AND EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid='account_payment'::regclass AND attname='bonus_payout_nano_cents' AND atttypid='bigint'::regtype AND attnotnull AND NOT attisdropped)
		AND EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid='account_payment'::regclass AND attname='attribution_review_required' AND atttypid='boolean'::regtype AND attnotnull AND NOT attisdropped)
		AND EXISTS (SELECT 1 FROM pg_trigger t JOIN pg_proc p ON p.oid=t.tgfoid WHERE t.tgrelid=to_regclass('public.account_payment_bonus')
			AND t.tgname='account_payment_bonus_guard' AND t.tgenabled IN ('O','A') AND t.tgtype=31 AND t.tgqual IS NULL AND p.prosrc=$3)
		AND EXISTS (SELECT 1 FROM pg_trigger t JOIN pg_proc p ON p.oid=t.tgfoid WHERE t.tgrelid='account_payment'::regclass
			AND t.tgname='account_payment_submission_basis_guard' AND t.tgenabled IN ('O','A') AND t.tgtype=19 AND t.tgqual IS NULL AND p.prosrc=$4)`, migrationIndex, identity, bodies[1], bodies[3]).Scan(&ready)
	})
	if queryErr != nil {
		return errors.Join(errors.New("provider payout schema capability unavailable"), queryErr)
	}
	if !ready {
		return errors.New("provider payout schema/guard capability is missing or changed; migrate before enabling payout workers")
	}
	return nil
}
