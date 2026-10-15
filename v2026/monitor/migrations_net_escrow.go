// Cache ordering admission checks the installed transaction triggers and exact
// function bodies. A numeric migration head cannot prove reservation fencing.
package monitor

import (
	"strings"

	"github.com/urnetwork/server/v2026"
)

// Transition-table names are part of the function contract, so verify them as
// well as enabled state, timing, relation, and function identity.
var netEscrowRevisionArtifactQuery = `(
	(SELECT count(*) = 2 FROM (VALUES ('balance_id','uuid'), ('revision','bigint')) AS expected(column_name,data_type)
	WHERE EXISTS (SELECT 1 FROM information_schema.columns AS actual
		WHERE actual.table_schema='public' AND actual.table_name='transfer_balance_net_escrow_revision'
			AND actual.column_name=expected.column_name AND actual.data_type=expected.data_type AND actual.is_nullable='NO'))
	AND EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid=to_regclass('public.transfer_balance_net_escrow_revision')
		AND contype='p' AND convalidated AND pg_get_constraintdef(oid)='PRIMARY KEY (balance_id)')
	AND EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid=to_regclass('public.transfer_balance_net_escrow_revision')
		AND contype='c' AND convalidated AND pg_get_constraintdef(oid)='CHECK ((revision > 0))')
	AND (SELECT count(*) = 5 FROM (VALUES
		('advance_net_escrow_revision(uuid[])','void', '` + strings.ReplaceAll(server.NetEscrowAdvanceRevisionFunctionBodySql, "'", "''") + `'),
		('transfer_escrow_revision()','trigger', CASE WHEN (SELECT coalesce(max(end_version_number),0) FROM migration_audit WHERE status='success') >= 755 THEN '` + strings.ReplaceAll(server.NetEscrowRowsRedisRevisionFunctionBodySql, "'", "''") + `' ELSE '` + strings.ReplaceAll(server.NetEscrowRowsRevisionFunctionBodySql, "'", "''") + `' END),
		('transfer_contract_escrow_revision()','trigger', CASE WHEN (SELECT coalesce(max(end_version_number),0) FROM migration_audit WHERE status='success') >= 755 THEN '` + strings.ReplaceAll(server.NetEscrowContractsRedisRevisionFunctionBodySql, "'", "''") + `' WHEN
			(SELECT coalesce(max(end_version_number),0) FROM migration_audit WHERE status='success') >= 753
			THEN '` + strings.ReplaceAll(server.NetEscrowContractsRevisionFunctionBodySql, "'", "''") + `'
			ELSE '` + strings.ReplaceAll(server.NetEscrowContractsRevisionLegacyFunctionBodySql, "'", "''") + `' END),
		('transfer_balance_escrow_revision()','trigger', '` + strings.ReplaceAll(server.NetEscrowBalancesRevisionFunctionBodySql, "'", "''") + `'),
		('net_escrow_revision_guard()','trigger', '` + strings.ReplaceAll(server.NetEscrowRevisionGuardFunctionBodySql, "'", "''") + `')
	) AS expected(signature,return_type,body)
	WHERE EXISTS (SELECT 1 FROM pg_proc AS actual
		WHERE actual.oid=to_regprocedure('public.' || expected.signature)
			AND actual.prorettype=expected.return_type::regtype AND actual.prosrc=expected.body
			AND actual.prolang=(SELECT oid FROM pg_language WHERE lanname='plpgsql')))
	AND (SELECT count(*) = 13 FROM (VALUES
		('transfer_escrow','transfer_escrow_revision_insert','transfer_escrow_revision',4,NULL,'new_escrow_rows'),
		('transfer_escrow','transfer_escrow_revision_update','transfer_escrow_revision',16,'old_escrow_rows','new_escrow_rows'),
		('transfer_escrow','transfer_escrow_revision_delete','transfer_escrow_revision',8,'old_escrow_rows',NULL),
		('transfer_contract','transfer_contract_escrow_revision_insert','transfer_contract_escrow_revision',4,NULL,'new_escrow_contracts'),
		('transfer_contract','transfer_contract_escrow_revision_update','transfer_contract_escrow_revision',16,'old_escrow_contracts','new_escrow_contracts'),
		('transfer_contract','transfer_contract_escrow_revision_delete','transfer_contract_escrow_revision',8,'old_escrow_contracts',NULL),
		('transfer_balance','transfer_balance_escrow_revision_insert','transfer_balance_escrow_revision',4,NULL,'new_escrow_balances'),
		('transfer_balance','transfer_balance_escrow_revision_update','transfer_balance_escrow_revision',16,'old_escrow_balances','new_escrow_balances'),
		('transfer_balance','transfer_balance_escrow_revision_delete','transfer_balance_escrow_revision',8,'old_escrow_balances',NULL),
		('transfer_balance_net_escrow_revision','net_escrow_revision_guard','net_escrow_revision_guard',31,NULL,NULL),
		('transfer_balance_net_escrow_revision','net_escrow_revision_truncate_guard','net_escrow_revision_guard',34,NULL,NULL),
		('transfer_escrow','transfer_escrow_revision_truncate_guard','net_escrow_revision_guard',34,NULL,NULL),
		('transfer_balance','transfer_balance_revision_truncate_guard','net_escrow_revision_guard',34,NULL,NULL)
	) AS expected(table_name,trigger_name,function_name,kind,old_table,new_table)
	WHERE EXISTS (SELECT 1 FROM pg_trigger AS actual
		WHERE actual.tgrelid=to_regclass('public.' || expected.table_name) AND actual.tgname=expected.trigger_name
			AND actual.tgfoid=to_regprocedure('public.' || expected.function_name || '()')
			AND actual.tgtype=expected.kind AND actual.tgenabled='O'
			AND actual.tgoldtable IS NOT DISTINCT FROM expected.old_table
			AND actual.tgnewtable IS NOT DISTINCT FROM expected.new_table
			AND actual.tgnargs=0 AND actual.tgqual IS NULL AND actual.tgattr=''::int2vector AND NOT actual.tgisinternal))
)`
