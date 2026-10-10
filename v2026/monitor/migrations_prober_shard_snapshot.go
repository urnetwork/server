// Current financial/runtime artifacts remain checked after newer migrations.
package monitor

import (
	"strings"

	"github.com/urnetwork/server/v2026"
)

const proberShardOwnershipArtifactQuery = `(
	(SELECT count(*)=15 FROM (VALUES
		('task_id','uuid','NO',NULL),('epoch','uuid','NO',NULL),
		('shard_index','integer','NO',NULL),('shard_count','integer','NO',NULL),
		('network_id','uuid','NO',NULL),('user_id','uuid','NO',NULL),
		('client_id','uuid','NO',NULL),('device_id','uuid','NO',NULL),('balance_id','uuid','NO',NULL),
		('state','text','NO',NULL),('retired_client_ids','ARRAY','NO','''{}''::uuid[]'),
		('create_time','timestamp without time zone','NO',NULL),
		('deadline','timestamp without time zone','NO',NULL),
		('next_cleanup_time','timestamp without time zone','NO',NULL),
		('close_time','timestamp without time zone','YES',NULL)
	) AS expected(column_name,data_type,is_nullable,column_default)
	WHERE EXISTS (SELECT 1 FROM information_schema.columns AS actual
		WHERE actual.table_schema='public' AND actual.table_name='prober_shard_run'
			AND actual.column_name=expected.column_name AND actual.data_type=expected.data_type
			AND actual.is_nullable=expected.is_nullable
			AND actual.column_default IS NOT DISTINCT FROM expected.column_default
			AND (expected.column_name<>'retired_client_ids' OR actual.udt_name='_uuid')))
	AND (SELECT count(*)=10 FROM (VALUES
		('p','PRIMARY KEY (task_id, epoch)'),
		('u','UNIQUE (network_id)'),('u','UNIQUE (user_id)'),('u','UNIQUE (client_id)'),
		('u','UNIQUE (device_id)'),('u','UNIQUE (balance_id)'),
		('c','CHECK (((shard_index >= 0) AND (shard_index < 256)))'),
		('c','CHECK (((shard_count > shard_index) AND (shard_count <= 256)))'),
		('c','CHECK ((state = ANY (ARRAY[''active''::text, ''draining''::text, ''deleted''::text, ''closed''::text])))'),
		('c','CHECK (((state = ANY (ARRAY[''deleted''::text, ''closed''::text])) = (close_time IS NOT NULL)))')
	) AS expected(kind,definition)
	WHERE EXISTS (SELECT 1 FROM pg_constraint AS actual
		WHERE actual.conrelid=to_regclass('public.prober_shard_run')
			AND actual.contype::text=expected.kind AND actual.convalidated AND NOT actual.condeferrable
			AND pg_get_constraintdef(actual.oid)=expected.definition
			AND (expected.kind='c' OR EXISTS (SELECT 1 FROM pg_index AS i
				WHERE i.indexrelid=actual.conindid AND i.indisvalid AND i.indisready AND i.indisunique))))
	AND (SELECT count(*)=2 FROM (VALUES
		('prober_shard_run_active_slot','CREATE UNIQUE INDEX prober_shard_run_active_slot ON public.prober_shard_run USING btree (shard_index) WHERE (state = ''active''::text)'),
		('prober_shard_run_cleanup','CREATE INDEX prober_shard_run_cleanup ON public.prober_shard_run USING btree (next_cleanup_time) WHERE (state <> ''closed''::text)')
	) AS expected(index_name,definition)
	WHERE EXISTS (SELECT 1 FROM pg_index AS actual
		WHERE actual.indrelid=to_regclass('public.prober_shard_run')
			AND actual.indexrelid=to_regclass('public.' || expected.index_name)
			AND actual.indisvalid AND actual.indisready AND pg_get_indexdef(actual.indexrelid)=expected.definition))
)`

var netEscrowContractPointRevisionArtifactQuery = `EXISTS (
	SELECT 1 FROM pg_proc AS actual
	WHERE actual.oid=to_regprocedure('public.transfer_contract_escrow_revision()')
		AND actual.prorettype='trigger'::regtype AND actual.pronargs=0
		AND actual.prolang=(SELECT oid FROM pg_language WHERE lanname='plpgsql')
		AND actual.prokind='f' AND actual.provolatile='v' AND actual.proparallel='u'
		AND NOT actual.prosecdef AND NOT actual.proleakproof AND NOT actual.proisstrict
		AND actual.proconfig IS NULL
		AND actual.prosrc=CASE WHEN (SELECT coalesce(max(end_version_number),0) FROM migration_audit WHERE status='success')>=755 THEN '` + strings.ReplaceAll(server.NetEscrowContractsRedisRevisionFunctionBodySql, "'", "''") + `' ELSE '` + strings.ReplaceAll(server.NetEscrowContractsRevisionFunctionBodySql, "'", "''") + `' END
)`

const netEscrowSnapshotArtifactQuery = `(
	(SELECT count(*)=3 FROM (VALUES
		('balance_id','uuid'),('revision','bigint'),('reserved_byte_count','bigint')
	) AS expected(column_name,data_type)
	WHERE EXISTS (SELECT 1 FROM information_schema.columns AS actual
		WHERE actual.table_schema='public' AND actual.table_name='transfer_balance_net_escrow_snapshot'
			AND actual.column_name=expected.column_name AND actual.data_type=expected.data_type
			AND actual.is_nullable='NO' AND actual.column_default IS NULL))
	AND (SELECT count(*)=3 FROM (VALUES
		('p','PRIMARY KEY (balance_id)'),('c','CHECK ((revision >= 0))'),('c','CHECK ((reserved_byte_count >= 0))')
	) AS expected(kind,definition)
	WHERE EXISTS (SELECT 1 FROM pg_constraint AS actual
		WHERE actual.conrelid=to_regclass('public.transfer_balance_net_escrow_snapshot')
			AND actual.contype::text=expected.kind AND actual.convalidated AND NOT actual.condeferrable
			AND pg_get_constraintdef(actual.oid)=expected.definition
			AND (expected.kind='c' OR EXISTS (SELECT 1 FROM pg_index AS i
				WHERE i.indexrelid=actual.conindid AND i.indisvalid AND i.indisready AND i.indisunique))))
)`

const redisAdmissionArtifactQuery = `(
    EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public'
        AND table_name='transfer_escrow' AND column_name='redis_reserved'
        AND data_type='boolean' AND is_nullable='NO' AND column_default='false')
    AND (SELECT count(*)=2 FROM information_schema.columns WHERE table_schema='public'
        AND table_name='redis_contract_admission_policy' AND column_name IN ('singleton','enabled')
        AND data_type='boolean' AND is_nullable='NO')
    AND EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid=to_regclass('public.redis_contract_admission_policy')
        AND contype='p' AND convalidated AND pg_get_constraintdef(oid)='PRIMARY KEY (singleton)')
    AND EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid=to_regclass('public.redis_contract_admission_policy')
        AND contype='c' AND convalidated AND pg_get_constraintdef(oid)='CHECK (singleton)')
)`
