package server

// Legacy reservations remain open until one worker commits their original
// debit/outcome transaction. This queue has no shared financial-row foreign key.
// Its contract foreign key fences old retention statements atomically, including
// statements that delete dependent accounting rows before deleting the contract.
const legacySettlementIntentSchemaSql = `
 CREATE TABLE legacy_settlement_intent (
  contract_id uuid PRIMARY KEY REFERENCES transfer_contract(contract_id),
  shard smallint NOT NULL CHECK (shard = get_byte(uuid_send(contract_id),15) % 16),
  outcome text NOT NULL CHECK (outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')),
  clear_dispute boolean NOT NULL DEFAULT false,
  create_time timestamp NOT NULL DEFAULT (clock_timestamp() AT TIME ZONE 'UTC'),
  next_attempt_time timestamp NOT NULL DEFAULT (clock_timestamp() AT TIME ZONE 'UTC'),
  failure_code text NOT NULL DEFAULT 'none' CHECK (failure_code IN ('none','accounting','operational'))
 );
 CREATE INDEX legacy_settlement_intent_age ON legacy_settlement_intent(shard,failure_code,create_time);
 CREATE INDEX legacy_settlement_intent_due ON legacy_settlement_intent(shard,next_attempt_time,contract_id);
 CREATE FUNCTION guard_legacy_settlement_intent_outcome() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN
  IF OLD.outcome IS NULL AND NEW.outcome IS NOT NULL AND
     EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=OLD.contract_id) THEN
   RAISE EXCEPTION 'contract has a pending legacy settlement owner' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
 END;
 $$;
 CREATE TRIGGER transfer_contract_legacy_settlement_owner
 BEFORE UPDATE OF outcome ON transfer_contract FOR EACH ROW
 EXECUTE FUNCTION guard_legacy_settlement_intent_outcome();
`

// Scheduling metadata belongs to each intent. It never owns a balance, changes
// financial eligibility, or takes a shared payer row lock. Old writers omit the
// nullable key; their insert trigger uses the same endpoint fallback as the
// accounting participant resolver. Existing rows are filled in bounded pages.
const legacySettlementPayerSchemaSql = `
 SET LOCAL lock_timeout = '5s';
 ALTER TABLE legacy_settlement_intent ADD COLUMN payer_network_id uuid NULL;
 CREATE FUNCTION assign_legacy_settlement_intent_payer() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN
  IF NEW.payer_network_id IS NULL THEN
   SELECT COALESCE(payer_network_id,
     CASE WHEN companion_contract_id IS NULL THEN source_network_id ELSE destination_network_id END)
   INTO NEW.payer_network_id FROM transfer_contract WHERE contract_id=NEW.contract_id;
  END IF;
  RETURN NEW;
 END;
 $$;
 CREATE TRIGGER legacy_settlement_intent_assign_payer
 BEFORE INSERT ON legacy_settlement_intent FOR EACH ROW
 EXECUTE FUNCTION assign_legacy_settlement_intent_payer();
`

// Retires the legacy settlement queue's guards (PARTITION-MAINTENANCE-PLAN.md,
// Part B). Every escrow admitted since Redis admission became unconditional is
// redis_reserved, and no current binary reads or writes this table. The
// migration refuses while any intent remains, then drops the outcome guard on
// transfer_contract, its function, and the table's contract key.
//
// The table itself stays, empty, for one release: migrations apply before the
// binaries that need them serve, so the previous release still reads it on
// its expiry, deadline-close and reaper paths. A later release drops it with
// its two insert-time trigger functions, once no deployed binary reads it.
//
// The guard runs inside an outcome writer that already holds transfer_contract,
// so transfer_contract is locked first. Locking in the guard's order cannot
// deadlock with those writers. Both locks are bounded by the lock timeout; a
// timed out or refused run leaves the migration unapplied and can be retried.
// The intent check follows both locks, so it sees every committed intent and
// no new one can commit before this transaction does.
const legacySettlementIntentRetirementSql = `
	SET LOCAL lock_timeout = '5s';
	LOCK TABLE transfer_contract IN ACCESS EXCLUSIVE MODE;
	LOCK TABLE legacy_settlement_intent IN ACCESS EXCLUSIVE MODE;
	DO $legacy_settlement_intent_retirement$
	BEGIN
		IF EXISTS (SELECT 1 FROM legacy_settlement_intent) THEN
			RAISE EXCEPTION 'legacy_settlement_intent still holds pending legacy settlements; settle them before retiring the queue'
				USING HINT = 'PARTITION-MAINTENANCE-PLAN.md, Part B: the table must be empty before this migration applies.';
		END IF;
	END
	$legacy_settlement_intent_retirement$;
	DROP TRIGGER IF EXISTS transfer_contract_legacy_settlement_owner ON transfer_contract;
	DROP FUNCTION IF EXISTS guard_legacy_settlement_intent_outcome();
	ALTER TABLE legacy_settlement_intent DROP CONSTRAINT IF EXISTS legacy_settlement_intent_contract_id_fkey;
`

// The version (slice index + 1) at which the retirement applies, found by the
// migration's identity, or 0 when this binary has none. The monitor's catalog
// contract follows the migration wherever a merge places it.
func LegacySettlementIntentRetirementMigrationVersion() int {
	for index, migration := range migrations {
		if sql, ok := migration.(*SqlMigration); ok && sql.sql == legacySettlementIntentRetirementSql {
			return index + 1
		}
	}
	return 0
}
