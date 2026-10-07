// Published 759–763 artifacts are inspected through catalogs only. Missing
// future tables/functions remain pending migrations, not query errors.
package monitor

var testBalanceDrainArtifactQuery = `(` +
	migrationFp2Table("test_balance_drain") + ` AND ` +
	financialColumnsArtifact("test_balance_drain", `('drain_id','uuid','NO',NULL),('network_id','uuid','NO',NULL),('start_time','timestamp without time zone','NO',NULL),('end_time','timestamp without time zone','NO',NULL),('restore_time','timestamp without time zone','YES',NULL),('drained_balance_byte_count','bigint','NO',NULL)`) + ` AND ` +
	financialConstraintsArtifact("test_balance_drain", `('PRIMARY KEY (drain_id)')`) + `)`

var testBalanceDrainIndexArtifactQuery = migrationFp2Index("test_balance_drain", "test_balance_drain_network_id_end_time", "network_id, end_time", "(restore_time IS NULL)", false)

var solanaPaymentAmountArtifactQuery = `(` +
	financialColumnsArtifact("solana_payment_intent", `('expected_amount_micro','bigint','YES',NULL)`) + ` AND ` +
	migrationFp2Index("solana_payment_intent", "solana_payment_intent_expected_amount_micro", "expected_amount_micro, expires_at", "(expected_amount_micro IS NOT NULL)", false) + ` AND ` +
	migrationFp2Table("solana_payment_amount_reservation") + ` AND ` +
	financialColumnsArtifact("solana_payment_amount_reservation", `('amount_micro','bigint','NO',NULL),('payment_reference','text','NO',NULL),('reserved_until','timestamp without time zone','NO',NULL)`) + ` AND ` +
	financialConstraintsArtifact("solana_payment_amount_reservation", `('PRIMARY KEY (amount_micro)')`) + ` AND ` +
	financialColumnsArtifact("solana_unfulfilled_payment", `('sender_account','character varying','YES',NULL),('match_note','text','YES',NULL)`) + ` AND
 EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='solana_unfulfilled_payment'
 AND column_name='sender_account' AND character_maximum_length=64))`

// Pin the exact published body, including the narrow NULL-to-terminal event.
// The native prefix/fault fixture detects any divergence from migration 763.
const legacySettlementOutcomeGuardBody = `
 BEGIN
  IF OLD.outcome IS NULL AND NEW.outcome IS NOT NULL AND
     EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=OLD.contract_id) THEN
   RAISE EXCEPTION 'contract has a pending legacy settlement owner' USING ERRCODE='23514';
  END IF;
  RETURN NEW;
 END;
 `

var legacySettlementIntentArtifactQuery = `(` +
	migrationFp2Table("legacy_settlement_intent") + ` AND ` +
	financialColumnsArtifact("legacy_settlement_intent", `('contract_id','uuid','NO',NULL),('shard','smallint','NO',NULL),('outcome','text','NO',NULL),('clear_dispute','boolean','NO','false'),('create_time','timestamp without time zone','NO','(clock_timestamp() AT TIME ZONE ''UTC''::text)'),('next_attempt_time','timestamp without time zone','NO','(clock_timestamp() AT TIME ZONE ''UTC''::text)'),('failure_code','text','NO','''none''::text')`) + ` AND ` +
	financialConstraintsArtifact("legacy_settlement_intent", `('PRIMARY KEY (contract_id)'),('FOREIGN KEY (contract_id) REFERENCES transfer_contract(contract_id)'),('CHECK ((shard = (get_byte(uuid_send(contract_id), 15) % 16)))'),('CHECK ((outcome = ANY (ARRAY[''settled''::text, ''dispute_resolved_to_source''::text, ''dispute_resolved_to_destination''::text])))'),('CHECK ((failure_code = ANY (ARRAY[''none''::text, ''accounting''::text, ''operational''::text])))')`) + ` AND ` +
	financialIndexArtifact("legacy_settlement_intent", "legacy_settlement_intent_age", "shard, failure_code, create_time") + ` AND ` +
	financialIndexArtifact("legacy_settlement_intent", "legacy_settlement_intent_due", "shard, next_attempt_time, contract_id") + ` AND ` +
	financialGuardArtifact("transfer_contract", "transfer_contract_legacy_settlement_owner", "guard_legacy_settlement_intent_outcome", legacySettlementOutcomeGuardBody, 19, "outcome") + ` AND
 NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid=to_regclass('public.legacy_settlement_intent') AND contype='f'
 AND pg_get_constraintdef(oid)<>'FOREIGN KEY (contract_id) REFERENCES transfer_contract(contract_id)') AND
 NOT EXISTS(SELECT 1 FROM (VALUES
  ('legacy_settlement_intent','RI_FKey_check_ins',5),
  ('legacy_settlement_intent','RI_FKey_check_upd',17),
  ('transfer_contract','RI_FKey_noaction_del',9),
  ('transfer_contract','RI_FKey_noaction_upd',17)
 ) AS expected(table_name,function_name,trigger_type)
 WHERE NOT EXISTS(SELECT 1 FROM pg_constraint k JOIN pg_trigger t ON t.tgconstraint=k.oid
 JOIN pg_proc p ON p.oid=t.tgfoid JOIN pg_namespace n ON n.oid=p.pronamespace
 WHERE k.conrelid=to_regclass('public.legacy_settlement_intent') AND k.contype='f'
 AND pg_get_constraintdef(k.oid)='FOREIGN KEY (contract_id) REFERENCES transfer_contract(contract_id)'
 AND t.tgrelid=to_regclass('public.'||expected.table_name) AND t.tgisinternal
 AND t.tgenabled IN ('O','A') AND t.tgtype=expected.trigger_type AND t.tgqual IS NULL
 AND NOT t.tgdeferrable AND NOT t.tginitdeferred AND t.tgnargs=0
 AND n.nspname='pg_catalog' AND p.proname=expected.function_name)))`
