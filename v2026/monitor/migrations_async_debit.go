// Financial migration artifacts use exact table, guard and bounded-index shapes.
package monitor

import (
	"fmt"
	"strings"

	"github.com/urnetwork/server/v2026"
)

func financialColumnsArtifact(table, values string) string {
	return `(NOT EXISTS(SELECT 1 FROM (VALUES ` + values + `) AS expected(name,kind,nullable,default_value)
 WHERE NOT EXISTS(SELECT 1 FROM information_schema.columns actual
 WHERE actual.table_schema='public' AND actual.table_name='` + table + `' AND actual.column_name=expected.name
 AND actual.data_type=expected.kind AND actual.is_nullable=expected.nullable
 AND actual.column_default IS NOT DISTINCT FROM expected.default_value)))`
}
func financialConstraintsArtifact(table, values string) string {
	return `(NOT EXISTS(SELECT 1 FROM (VALUES ` + values + `) AS expected(definition)
 WHERE NOT EXISTS(SELECT 1 FROM pg_constraint actual WHERE actual.conrelid=to_regclass('public.` + table + `')
 AND actual.convalidated AND NOT actual.condeferrable AND pg_get_constraintdef(actual.oid)=expected.definition
 AND (actual.contype NOT IN ('p','u') OR EXISTS(SELECT 1 FROM pg_index i WHERE i.indexrelid=actual.conindid AND i.indisvalid AND i.indisready)))))`
}
func financialIndexArtifact(table, name, columns string) string {
	return `EXISTS(SELECT 1 FROM pg_index i WHERE i.indrelid=to_regclass('public.` + table + `')
 AND i.indexrelid=to_regclass('public.` + name + `') AND i.indisvalid AND i.indisready
 AND pg_get_indexdef(i.indexrelid)='CREATE INDEX ` + name + ` ON public.` + table + ` USING btree (` + columns + `)')`
}
func financialGuardArtifact(table, trigger, function, body string, mask int, updateColumns ...string) string {
	attributes := "t.tgattr=''::int2vector"
	if len(updateColumns) != 0 {
		columns := make([]string, len(updateColumns))
		for i, column := range updateColumns {
			columns[i] = "'" + strings.ReplaceAll(column, "'", "''") + "'"
		}
		attributes = `ARRAY(SELECT a.attname::text FROM pg_attribute a
 WHERE a.attrelid=t.tgrelid AND a.attnum=ANY(t.tgattr) ORDER BY a.attname)=ARRAY[` + strings.Join(columns, ",") + `]::text[]`
	}
	return fmt.Sprintf(`EXISTS(SELECT 1 FROM pg_trigger t JOIN pg_proc p ON p.oid=t.tgfoid
 WHERE t.tgrelid=to_regclass('public.%s') AND t.tgname='%s' AND t.tgfoid=to_regprocedure('public.%s()')
 AND NOT t.tgisinternal AND t.tgenabled IN ('O','A') AND t.tgtype=%d AND t.tgqual IS NULL AND t.tgnargs=0 AND %s
 AND p.prorettype='trigger'::regtype AND p.pronargs=0 AND p.prolang=(SELECT oid FROM pg_language WHERE lanname='plpgsql')
 AND p.prokind='f' AND p.provolatile='v' AND p.proparallel='u' AND NOT p.prosecdef AND NOT p.proleakproof AND NOT p.proisstrict
 AND p.proconfig IS NULL AND p.prosrc='%s')`, table, trigger, function, mask, attributes, strings.ReplaceAll(body, "'", "''"))
}

var providerPaymentBonusArtifactQuery = func() string {
	bonus, submission := server.ProviderPaymentBonusGuardBodiesSql()
	return `(` + financialColumnsArtifact("account_payment", `('bonus_payout_nano_cents','bigint','NO','0'),('attribution_review_required','boolean','NO','false')`) + ` AND ` +
		financialColumnsArtifact("account_payment_bonus", `('operation_id','uuid','NO',NULL),('origin_payment_id','uuid','NO',NULL),('payment_id','uuid','NO',NULL),('payment_plan_id','uuid','NO',NULL),('network_id','uuid','NO',NULL),('amount_nano_cents','bigint','NO',NULL),('reason','text','NO',NULL),('latest_contract_close_time','timestamp without time zone','NO',NULL),('subsidy_end_time','timestamp without time zone','YES',NULL),('policy_sha256','character varying','NO',NULL),('create_time','timestamp without time zone','NO','now()')`) + ` AND ` +
		`(SELECT count(*)=1 FROM information_schema.columns WHERE table_schema='public' AND table_name='account_payment_bonus' AND column_name='policy_sha256' AND character_maximum_length=64) AND ` + financialConstraintsArtifact("account_payment", `('CHECK ((bonus_payout_nano_cents >= 0))')`) + ` AND ` +
		financialConstraintsArtifact("account_payment_bonus", `('PRIMARY KEY (operation_id, origin_payment_id)'),('FOREIGN KEY (origin_payment_id) REFERENCES account_payment(payment_id)'),('FOREIGN KEY (payment_id) REFERENCES account_payment(payment_id)'),('CHECK ((amount_nano_cents > 0))'),('CHECK (((octet_length(reason) >= 1) AND (octet_length(reason) <= 1024)))')`) + ` AND ` +
		financialIndexArtifact("account_payment_bonus", "account_payment_bonus_payment", "payment_id") + ` AND ` +
		financialGuardArtifact("account_payment_bonus", "account_payment_bonus_guard", "guard_account_payment_bonus", bonus, 31) + ` AND ` +
		financialGuardArtifact("account_payment", "account_payment_submission_basis_guard", "guard_account_payment_submission_basis", submission, 19) + `)`
}()

var providerPayoutBoundaryArtifactQuery = `(` +
	financialColumnsArtifact("provider_payout_boundary", `('singleton','boolean','NO','true'),('earning_identity','text','NO',NULL),('identity_sha256','character varying','NO',NULL),('initial_config_sha256','character varying','NO',NULL),('prepared_at','timestamp without time zone','NO','timezone(''utc''::text, now())')`) + ` AND ` +
	`(SELECT count(*)=2 FROM information_schema.columns WHERE table_schema='public' AND table_name='provider_payout_boundary' AND column_name IN ('identity_sha256','initial_config_sha256') AND character_maximum_length=64) AND ` + financialConstraintsArtifact("provider_payout_boundary", `('PRIMARY KEY (singleton)'),('CHECK (singleton)'),('CHECK (((octet_length(earning_identity) >= 1) AND (octet_length(earning_identity) <= 4096)))'),('CHECK (((identity_sha256)::text ~ ''^[0-9a-f]{64}$''::text))'),('CHECK (((initial_config_sha256)::text ~ ''^[0-9a-f]{64}$''::text))')`) + ` AND ` +
	financialGuardArtifact("provider_payout_boundary", "provider_payout_boundary_guard", "guard_provider_payout_boundary", server.ProviderPayoutBoundaryGuardBodySql(), 27) + ` AND ` +
	financialGuardArtifact("provider_payout_boundary", "provider_payout_boundary_truncate_guard", "guard_provider_payout_boundary", server.ProviderPayoutBoundaryGuardBodySql(), 34) + `)`

var transferDebitArtifactQuery = `(` +
	financialColumnsArtifact("transfer_debit_journal", `('contract_id','uuid','NO',NULL),('balance_id','uuid','NO',NULL),('shard','smallint','NO',NULL),('debit_byte_count','bigint','NO',NULL),('applied','boolean','NO','false'),('create_time','timestamp without time zone','NO','(clock_timestamp() AT TIME ZONE ''UTC''::text)')`) + ` AND ` +
	financialConstraintsArtifact("transfer_debit_journal", `('PRIMARY KEY (balance_id, contract_id)'),('CHECK ((shard = (get_byte(uuid_send(balance_id), 15) % 16)))'),('CHECK ((debit_byte_count >= 0))')`) + ` AND ` +
	financialIndexArtifact("transfer_debit_journal", "transfer_debit_journal_contract", "contract_id, balance_id") + ` AND ` +
	financialIndexArtifact("transfer_debit_journal", "transfer_debit_journal_shard", "shard, balance_id, contract_id") + ` AND ` +
	financialIndexArtifact("transfer_debit_journal", "transfer_debit_journal_age", "shard, applied, create_time") + ` AND ` +
	financialGuardArtifact("transfer_balance", "transfer_balance_pending_debit_guard", "protect_pending_transfer_debit", server.TransferDebitGuardBodySql(), 11) + `
 AND NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid=to_regclass('public.transfer_debit_journal') AND contype='f')
 AND NOT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid=to_regclass('public.transfer_debit_journal') AND NOT tgisinternal))`
