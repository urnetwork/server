// Payer scheduling artifacts are catalog-only checks for migrations 791–793.
// NULL keys are valid during bounded registration, not missing financial data.
package monitor

// Pin the published rolling-writer body, including the companion fallback and
// preservation of an explicitly supplied payer. No application rows are read.
const legacySettlementPayerGuardBody = `
 BEGIN
  IF NEW.payer_network_id IS NULL THEN
   SELECT COALESCE(payer_network_id,
     CASE WHEN companion_contract_id IS NULL THEN source_network_id ELSE destination_network_id END)
   INTO NEW.payer_network_id FROM transfer_contract WHERE contract_id=NEW.contract_id;
  END IF;
  RETURN NEW;
 END;
 `

// Ordinary stored UUIDs have no default; generated or required keys would
// change rolling-writer behavior. Each later index has its own published gate.
var legacySettlementPayerArtifactQueries = []string{
	`(EXISTS (
 SELECT 1 FROM pg_attribute a
 WHERE a.attrelid=to_regclass('public.legacy_settlement_intent')
 AND a.attname='payer_network_id' AND a.attnum>0 AND NOT a.attisdropped
 AND a.atttypid='uuid'::regtype AND a.atttypmod=-1 AND NOT a.attnotnull
 AND a.attgenerated='' AND a.attidentity=''
 AND NOT EXISTS(SELECT 1 FROM pg_attrdef d WHERE d.adrelid=a.attrelid AND d.adnum=a.attnum)
) AND ` + financialGuardArtifact("legacy_settlement_intent", "legacy_settlement_intent_assign_payer",
		"assign_legacy_settlement_intent_payer", legacySettlementPayerGuardBody, 7) + `)`,
	migrationFp2Index("legacy_settlement_intent", "legacy_settlement_intent_payer_due",
		"shard, payer_network_id, next_attempt_time, contract_id", "(payer_network_id IS NOT NULL)", false),
	migrationFp2Index("legacy_settlement_intent", "legacy_settlement_intent_payer_missing",
		"shard, contract_id", "(payer_network_id IS NULL)", false),
}
