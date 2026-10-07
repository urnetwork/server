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
