// Identity churn retention (PARTITION-MAINTENANCE-PLAN.md Part D): retired
// hosted identities, the children a hosted proxy device mints, are hard
// deleted a day after their deactivation, and the signed key history of a
// client that is gone is deleted once the contract horizon has passed since it
// went, never sooner than ClientKeyHistoryRetentionFloor. Heads go first, then
// the history they named, then the record of when the client went.
package server

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The retired hosted client reap (model.RemoveRetiredHostedNetworkClients)
// pages the inactive children in deactivation order. The index holds only
// retired children: a hosted window for about a day, another child until the
// 30-day reap.
const retiredChildNetworkClientIndexRecoverySql = `
	DROP INDEX CONCURRENTLY IF EXISTS network_client_retired_child_deactivate_time
`

const retiredChildNetworkClientIndexSql = `
	CREATE INDEX CONCURRENTLY network_client_retired_child_deactivate_time
	ON network_client (deactivate_time, client_id)
	WHERE NOT active AND source_client_id IS NOT NULL
`

const retiredChildNetworkClientIndexAuditSql = `
	DROP INDEX IF EXISTS network_client_retired_child_deactivate_time;
	CREATE INDEX network_client_retired_child_deactivate_time
	ON network_client (deactivate_time, client_id)
	WHERE NOT active AND source_client_id IS NOT NULL
`

// The index as PostgreSQL renders it, which completion recovery compares.
const retiredChildNetworkClientIndexDefinition = "CREATE INDEX network_client_retired_child_deactivate_time ON public.network_client USING btree (deactivate_time, client_id) WHERE ((NOT active) AND (source_client_id IS NOT NULL))"

const retiredChildNetworkClientIndexPredicate = "((NOT active) AND (source_client_id IS NOT NULL))"

// Enrolls the index build in completion recovery: a completed build, including
// one made out of band with the same statement, is kept when the migration
// reruns, and a builder still running is refused rather than raced. The
// recovery is keyed by the migration's version, which is its position, known
// only once the list is built.
func init() {
	version := RetiredChildNetworkClientIndexMigrationVersion()
	migrations[version-1].(*OnlineSqlMigration).withIndexCompletion(
		int32(version),
		"network_client",
		"network_client_retired_child_deactivate_time",
		retiredChildNetworkClientIndexDefinition,
		retiredChildNetworkClientIndexPredicate,
	)
}

// st_client_key_gone records when each client with key history went: written
// by the network_client delete trigger, which loses the client's row, and by
// the retention task for clients deleted before this migration. A child goes
// at its deactivation (or its delete, when it was deleted while active); a
// top-level client goes at its own reap. The history guard admits a delete
// only from a transaction that sets urnetwork.client_key_history_retention,
// and only of a client gone more than 91 days, the contract retention
// (ContractRetention), with no client row and no remaining head. An update
// and a truncate stay refused.
const clientKeyHistoryRetentionSchemaSql = `
	SET LOCAL lock_timeout = '5s';
	CREATE TABLE st_client_key_gone (
		client_id uuid NOT NULL,
		gone_time timestamp NOT NULL,
		PRIMARY KEY (client_id)
	);
	CREATE INDEX st_client_key_gone_gone_time ON st_client_key_gone (gone_time);
	CREATE OR REPLACE FUNCTION st_client_key_retire_deleted_client()
	RETURNS trigger LANGUAGE plpgsql AS $st_client_key_retire_deleted_client$
	BEGIN
		UPDATE st_client_key_head SET retired = true WHERE client_id = OLD.client_id;
		IF FOUND THEN
			INSERT INTO st_client_key_gone (client_id, gone_time)
			VALUES (
				OLD.client_id,
				CASE
					WHEN OLD.source_client_id IS NULL THEN now() AT TIME ZONE 'UTC'
					ELSE COALESCE(OLD.deactivate_time, now() AT TIME ZONE 'UTC')
				END
			)
			ON CONFLICT (client_id) DO NOTHING;
		END IF;
		RETURN OLD;
	END
	$st_client_key_retire_deleted_client$;
	CREATE OR REPLACE FUNCTION st_client_key_history_immutable_guard()
	RETURNS trigger LANGUAGE plpgsql AS $st_client_key_history_immutable_guard$
	BEGIN
		IF TG_OP = 'DELETE' AND current_setting('urnetwork.client_key_history_retention', true) = 'on' THEN
			IF EXISTS (
				SELECT 1 FROM st_client_key_gone
				WHERE
					st_client_key_gone.client_id = OLD.client_id AND
					st_client_key_gone.gone_time < (now() AT TIME ZONE 'UTC') - interval '91 days'
			) AND NOT EXISTS (
				SELECT 1 FROM st_client_key_head
				WHERE st_client_key_head.client_id = OLD.client_id
			) AND NOT EXISTS (
				SELECT 1 FROM network_client
				WHERE network_client.client_id = OLD.client_id
			) THEN
				RETURN OLD;
			END IF;
		END IF;
		RAISE EXCEPTION 'signed client-key history is immutable';
	END
	$st_client_key_history_immutable_guard$;
	CREATE TRIGGER st_client_key_history_truncate_guard
	BEFORE TRUNCATE ON st_client_key_history
	FOR EACH STATEMENT EXECUTE FUNCTION st_client_key_history_immutable_guard();
`

// The shortest time a gone client's signed key history is kept, whatever the
// retention's horizon (owner, 2026-11-01). The retention follows the contract
// horizon reaper's effective horizon, which never goes below the reaper's own
// floor, unless its brake's `horizonDays` sets its own, which may reach this
// floor; this floor is the guard's, below every horizon the reaper applies,
// so a task, a setting or an operator statement can never delete history
// younger than it. Every reader of a gone client's history ends well inside
// it:
//   - close report admission reads the reporting client's own history
//     (readOriginalCloseKey). A report is admitted only for a contract the
//     client is party to, and a contract opens only between active clients
//     and ends at its deadline, model.DefaultContractExpiration (60 minutes)
//     after it opened, when the expiry sweep force-closes it; so a gone
//     client's last report is admitted within hours of its going, not days.
//     An admitted report keeps its own copy of the registration in its
//     evidence row;
//   - the current key readers (GetClientPublicKey, GetClientContractMetadata,
//     LoadStClientKeyHistory, the provider work owner) refuse a client whose
//     row is gone or inactive either way, and the retention deletes the
//     legacy Redis key with the heads;
//   - the epoch close's key overlay (overlayStClientKeyCurrent) reads the
//     heads of the providers of an epoch, which closes within days of its end.
//
// The floor is the SQL function st_client_key_history_retention_floor(), which
// both the guard and the retention's batch read. So the retention never
// selects a client the guard refuses, and one statement moves the floor for
// both: ClientKeyHistoryRetentionFloorRollbackSql restores 91 days, and
// ClientKeyHistoryRetentionFloorRestoreSql the published floor
// (`bringyourctl db key-history-floor`).
const ClientKeyHistoryRetentionFloor = 30 * 24 * time.Hour

// Replaces the guard's fixed 91 days with the floor function. Nothing else in
// the guard changes: the opt-in, the record of when the client went, and the
// absence of the client's row and heads are still required, and an update or a
// truncate is still refused. Both statements change only function definitions:
// no table is locked, scanned or rewritten, and both replace what they find,
// so a rerun after an interrupted apply completes it.
const clientKeyHistoryRetentionFloorSql = `
	SET LOCAL lock_timeout = '5s';
	CREATE OR REPLACE FUNCTION st_client_key_history_retention_floor()
	RETURNS interval LANGUAGE sql STABLE AS $st_client_key_history_retention_floor$
		SELECT interval '30 days'
	$st_client_key_history_retention_floor$;
	CREATE OR REPLACE FUNCTION st_client_key_history_immutable_guard()
	RETURNS trigger LANGUAGE plpgsql AS $st_client_key_history_immutable_guard$
	BEGIN
		IF TG_OP = 'DELETE' AND current_setting('urnetwork.client_key_history_retention', true) = 'on' THEN
			IF EXISTS (
				SELECT 1 FROM st_client_key_gone
				WHERE
					st_client_key_gone.client_id = OLD.client_id AND
					st_client_key_gone.gone_time < (now() AT TIME ZONE 'UTC') - st_client_key_history_retention_floor()
			) AND NOT EXISTS (
				SELECT 1 FROM st_client_key_head
				WHERE st_client_key_head.client_id = OLD.client_id
			) AND NOT EXISTS (
				SELECT 1 FROM network_client
				WHERE network_client.client_id = OLD.client_id
			) THEN
				RETURN OLD;
			END IF;
		END IF;
		RAISE EXCEPTION 'signed client-key history is immutable';
	END
	$st_client_key_history_immutable_guard$;
`

// The floor's rollback, run by an operator: the guard and the retention's
// batch then keep 91 days again, as before the floor, while the code that
// reads the floor keeps working. The monitor reports the changed floor body as
// drift until the floor is restored.
const ClientKeyHistoryRetentionFloorRollbackSql = `
	CREATE OR REPLACE FUNCTION st_client_key_history_retention_floor()
	RETURNS interval LANGUAGE sql STABLE AS $st_client_key_history_retention_floor$
		SELECT interval '91 days'
	$st_client_key_history_retention_floor$
`

// The monitor pins the exact published function bodies.
func ClientKeyHistoryGuardBodySql() string {
	return strings.Split(clientKeyHistoryRetentionSchemaSql, "$st_client_key_history_immutable_guard$")[1]
}

// The guard body that reads the floor, published by the floor migration.
func ClientKeyHistoryFloorGuardBodySql() string {
	return strings.Split(clientKeyHistoryRetentionFloorSql, "$st_client_key_history_immutable_guard$")[1]
}

func ClientKeyHistoryRetentionFloorBodySql() string {
	return strings.Split(clientKeyHistoryRetentionFloorSql, "$st_client_key_history_retention_floor$")[1]
}

// The floor function exactly as the floor migration publishes it, the restore
// after a rollback: the migration's own statement, cut from its text, so its
// bytes are the body the monitor pins.
func ClientKeyHistoryRetentionFloorRestoreSql() string {
	const start = "CREATE OR REPLACE FUNCTION st_client_key_history_retention_floor()"
	const end = "$st_client_key_history_retention_floor$;"
	from := strings.Index(clientKeyHistoryRetentionFloorSql, start)
	to := strings.Index(clientKeyHistoryRetentionFloorSql, end)
	if from < 0 || to < from {
		panic(fmt.Errorf("the floor migration has no floor function statement"))
	}
	return clientKeyHistoryRetentionFloorSql[from : to+len(end)-1]
}

// Moves the floor for both the guard and the retention's batch: back to 91
// days (rollback), or to the published floor (restore). One statement, with
// the migration's 5-second lock wait.
func SetClientKeyHistoryRetentionFloor(ctx context.Context, rollback bool) {
	sql := ClientKeyHistoryRetentionFloorRestoreSql()
	if rollback {
		sql = ClientKeyHistoryRetentionFloorRollbackSql
	}
	MaintenanceTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`))
		RaisePgResult(tx.Exec(ctx, sql))
	}, OptNoRetry())
}

// The floor the database applies, and whether its function holds the
// published body.
func GetClientKeyHistoryRetentionFloor(ctx context.Context) (floor string, published bool) {
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(
			ctx,
			`
			SELECT st_client_key_history_retention_floor()::text, p.prosrc = $1
			FROM pg_proc p
			WHERE p.oid = 'public.st_client_key_history_retention_floor()'::regprocedure
			`,
			ClientKeyHistoryRetentionFloorBodySql(),
		).Scan(&floor, &published))
	}, OptReadOnly())
	return
}

func ClientKeyRetireDeletedClientBodySql() string {
	return strings.Split(clientKeyHistoryRetentionSchemaSql, "$st_client_key_retire_deleted_client$")[1]
}

// Versions are positions in the migration list, and branches merge in any
// order, so the monitor looks its contracts up by migration content.
func RetiredChildNetworkClientIndexMigrationVersion() int {
	return migrationVersionOf(retiredChildNetworkClientIndexSql)
}

func ClientKeyHistoryRetentionMigrationVersion() int {
	return migrationVersionOf(clientKeyHistoryRetentionSchemaSql)
}

func ClientKeyHistoryRetentionFloorMigrationVersion() int {
	return migrationVersionOf(clientKeyHistoryRetentionFloorSql)
}

// The 1-based version of the one migration whose SQL is exactly sql.
func migrationVersionOf(sql string) int {
	version := 0
	for index, migration := range migrations {
		var migrationSql string
		switch typed := migration.(type) {
		case *SqlMigration:
			migrationSql = typed.sql
		case *OnlineSqlMigration:
			migrationSql = typed.sql
		}
		if migrationSql == sql {
			if version != 0 {
				panic(fmt.Errorf("migration sql appears at versions %d and %d", version, index+1))
			}
			version = index + 1
		}
	}
	if version == 0 {
		panic(fmt.Errorf("migration sql is not in the migration list"))
	}
	return version
}
