// Report receipts stay under their contract's existing owner and retention.
package monitor

var contractCloseReportArtifactQuery = `(` +
	migrationFp2Table("contract_close_report") + ` AND ` +
	financialColumnsArtifact("contract_close_report", `('contract_id','uuid','NO',NULL),('party','text','NO',NULL),('report_id','uuid','NO',NULL),('used_transfer_byte_count','bigint','NO',NULL),('checkpoint','boolean','NO',NULL),('create_time','timestamp with time zone','NO','now()')`) + ` AND ` +
	financialConstraintsArtifact("contract_close_report", `('PRIMARY KEY (contract_id, party, report_id)'),('FOREIGN KEY (contract_id) REFERENCES transfer_contract(contract_id) ON DELETE CASCADE'),('CHECK ((party = ANY (ARRAY[''source''::text, ''destination''::text])))'),('CHECK ((report_id <> ''00000000-0000-0000-0000-000000000000''::uuid))'),('CHECK ((used_transfer_byte_count >= 0))')`) + ` AND
 NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid=to_regclass('public.contract_close_report') AND contype='f'
 AND pg_get_constraintdef(oid)<>'FOREIGN KEY (contract_id) REFERENCES transfer_contract(contract_id) ON DELETE CASCADE') AND
 NOT EXISTS(SELECT 1 FROM pg_index WHERE indrelid=to_regclass('public.contract_close_report') AND indisunique
 AND pg_get_indexdef(indexrelid)<>'CREATE UNIQUE INDEX contract_close_report_pkey ON public.contract_close_report USING btree (contract_id, party, report_id)') AND
 NOT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid=to_regclass('public.contract_close_report') AND NOT tgisinternal) AND
 NOT EXISTS(SELECT 1 FROM (VALUES
  ('contract_close_report','RI_FKey_check_ins',5),
  ('contract_close_report','RI_FKey_check_upd',17),
  ('transfer_contract','RI_FKey_cascade_del',9),
  ('transfer_contract','RI_FKey_noaction_upd',17)
 ) AS expected(table_name,function_name,trigger_type)
 WHERE NOT EXISTS(SELECT 1 FROM pg_constraint k JOIN pg_trigger t ON t.tgconstraint=k.oid
 JOIN pg_proc p ON p.oid=t.tgfoid JOIN pg_namespace n ON n.oid=p.pronamespace
 WHERE k.conrelid=to_regclass('public.contract_close_report') AND k.contype='f'
 AND pg_get_constraintdef(k.oid)='FOREIGN KEY (contract_id) REFERENCES transfer_contract(contract_id) ON DELETE CASCADE'
 AND t.tgrelid=to_regclass('public.'||expected.table_name) AND t.tgisinternal
 AND t.tgenabled IN ('O','A') AND t.tgtype=expected.trigger_type AND t.tgqual IS NULL
 AND NOT t.tgdeferrable AND NOT t.tginitdeferred AND t.tgnargs=0
 AND n.nspname='pg_catalog' AND p.proname=expected.function_name)))`
