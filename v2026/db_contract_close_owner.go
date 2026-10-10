// The additive source-owner proposal retains source identity for every intent.
// NULL means not yet classified, including old non-NULL inferred payer hints.
package server

// Shared by application registration and the proposed rolling-writer trigger.
// Actual escrow lookup is needed only when retained payer metadata is absent.
const contractCloseOwnerSelectSql = `SELECT contract.source_id,contract.payer_network_id,
 ARRAY(SELECT DISTINCT balance.network_id FROM transfer_escrow AS escrow
 INNER JOIN transfer_balance AS balance ON balance.balance_id=escrow.balance_id
 WHERE escrow.contract_id=contract.contract_id AND contract.payer_network_id IS NULL
 ORDER BY balance.network_id),
 EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=contract.contract_id)
 FROM transfer_contract AS contract WHERE contract.contract_id=`

// Proposed migration 803 retains identity without a table rewrite or a client
// foreign key. The correcting trigger sorts after schema 791's old assignment;
// rolling old writers therefore receive the same actual close owner.
const ContractCloseOwnerSchemaSql = `
 SET LOCAL lock_timeout='5s';
 ALTER TABLE legacy_settlement_intent ADD COLUMN source_client_id uuid NULL;
 CREATE FUNCTION assign_legacy_settlement_close_owner() RETURNS trigger LANGUAGE plpgsql AS $body$` + ContractCloseOwnerGuardBody + `$body$;
 CREATE TRIGGER legacy_settlement_intent_resolve_close_owner BEFORE INSERT OR UPDATE OF payer_network_id,source_client_id ON legacy_settlement_intent
 FOR EACH ROW EXECUTE FUNCTION assign_legacy_settlement_close_owner();

`

// Keep the exact selector body shared; only the bound identity expression
// differs between a SQL parameter and the trigger's new row.
const contractCloseOwnerTriggerReadSql = contractCloseOwnerSelectSql + "NEW.contract_id"

const ContractCloseOwnerReadSql = contractCloseOwnerSelectSql + "$1"

const ContractCloseSourceDueIndexSql = `CREATE INDEX legacy_settlement_intent_source_due
 ON legacy_settlement_intent(shard,source_client_id,next_attempt_time,contract_id)
 WHERE payer_network_id IS NULL AND source_client_id IS NOT NULL`

const ContractCloseOwnerMissingIndexSql = `CREATE INDEX legacy_settlement_intent_owner_missing
 ON legacy_settlement_intent(shard,contract_id) WHERE source_client_id IS NULL`

// Include every byte inside the dollar quotes so catalog equality verifies
// the published function while the migration keeps its original identity.
const ContractCloseOwnerGuardBody = `
 DECLARE
  source_id uuid;
  payer_id uuid;
  escrow_payers uuid[];
  has_escrow boolean;
 BEGIN
  IF TG_OP='UPDATE' AND (OLD.source_client_id IS NULL OR NEW.source_client_id IS NULL) THEN RETURN NEW; END IF;
  SELECT owner.source_id,owner.payer_network_id,owner.escrow_payers,owner.has_escrow
  INTO source_id,payer_id,escrow_payers,has_escrow FROM (` + contractCloseOwnerTriggerReadSql + `) AS owner(source_id,payer_network_id,escrow_payers,has_escrow);
  IF source_id IS NULL OR source_id='00000000-0000-0000-0000-000000000000'::uuid THEN RAISE EXCEPTION 'contract close source client is missing'; END IF;
  IF payer_id IS NULL AND has_escrow THEN
   IF cardinality(escrow_payers)<>1 THEN RAISE EXCEPTION 'legacy close financial payer is unresolved'; END IF;
   payer_id := escrow_payers[1];
  END IF;
  IF payer_id='00000000-0000-0000-0000-000000000000'::uuid THEN RAISE EXCEPTION 'contract close payer is empty'; END IF;
  NEW.payer_network_id := payer_id;
  NEW.source_client_id := source_id;
  RETURN NEW;
 END;

 `
