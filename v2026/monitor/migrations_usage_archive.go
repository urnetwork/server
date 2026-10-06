// The usage archive is only ready when its exact copy and custody functions,
// relation shapes and enabled row/statement triggers are all installed.
package monitor

import (
	"strings"

	"github.com/urnetwork/server/v2026"
)

// Numeric migration progress alone cannot establish durable cleanup custody.
var providerUsageArchiveArtifactQuery = `(
	(SELECT count(*) = 4 FROM (VALUES
		('contract_id','uuid','NO'), ('outcome','character varying','NO'),
		('close_time','timestamp without time zone','YES'), ('provider_usage','jsonb','YES')
	) AS expected(column_name,data_type,is_nullable)
	WHERE EXISTS (SELECT 1 FROM information_schema.columns AS actual
		WHERE actual.table_schema='public' AND actual.table_name='st_provider_usage_archive'
			AND actual.column_name=expected.column_name AND actual.data_type=expected.data_type
			AND actual.is_nullable=expected.is_nullable))
	AND EXISTS (SELECT 1 FROM pg_constraint
		WHERE conrelid=to_regclass('public.st_provider_usage_archive') AND contype='p' AND convalidated
			AND pg_get_constraintdef(oid)='PRIMARY KEY (contract_id)')
	AND EXISTS (SELECT 1 FROM pg_index
		WHERE indrelid=to_regclass('public.st_provider_usage_archive')
			AND indexrelid=to_regclass('public.st_provider_usage_archive_close_time') AND indisvalid AND indisready
			AND pg_get_indexdef(indexrelid)='CREATE INDEX st_provider_usage_archive_close_time ON public.st_provider_usage_archive USING btree (close_time, contract_id)')
	AND (SELECT count(*) = 2 FROM (VALUES
		('st_provider_usage_archive_guard', '` + strings.ReplaceAll(server.ProviderUsageArchiveGuardFunctionBodySql, "'", "''") + `'),
		('transfer_contract_usage_archive_capture', '` + strings.ReplaceAll(server.ProviderUsageArchiveCaptureFunctionBodySql, "'", "''") + `')
	) AS expected(function_name,body)
	WHERE EXISTS (SELECT 1 FROM pg_proc AS actual
		WHERE actual.oid=to_regprocedure('public.' || expected.function_name || '()')
			AND actual.prorettype='trigger'::regtype AND actual.prosrc=expected.body
			AND actual.prolang=(SELECT oid FROM pg_language WHERE lanname='plpgsql')))
	AND (SELECT count(*) = 4 FROM (VALUES
		('st_provider_usage_archive','st_provider_usage_archive_guard','st_provider_usage_archive_guard',31),
		('st_provider_usage_archive','st_provider_usage_archive_truncate_guard','st_provider_usage_archive_guard',34),
		('transfer_contract','transfer_contract_usage_archive_capture','transfer_contract_usage_archive_capture',11),
		('transfer_contract','transfer_contract_usage_archive_truncate_guard','st_provider_usage_archive_guard',34)
	) AS expected(table_name,trigger_name,function_name,kind)
	WHERE EXISTS (SELECT 1 FROM pg_trigger AS actual
		WHERE actual.tgrelid=to_regclass('public.' || expected.table_name) AND actual.tgname=expected.trigger_name
			AND actual.tgfoid=to_regprocedure('public.' || expected.function_name || '()')
			AND actual.tgtype=expected.kind AND actual.tgenabled='O'
			AND actual.tgnargs=0 AND actual.tgqual IS NULL AND actual.tgattr=''::int2vector
			AND NOT actual.tgisinternal))
)`
