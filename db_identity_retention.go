// Identity churn retention (PARTITION-MAINTENANCE-PLAN.md Part D): retired
// hosted identities, the children a hosted proxy device mints, are hard
// deleted a day after their deactivation, and the signed key history of a
// client that is gone is deleted 91 days after it went. Heads go first, then
// the history they named, then the record of when the client went.
package server

import (
	"fmt"
	"strings"
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

// The monitor pins the exact published function bodies.
func ClientKeyHistoryGuardBodySql() string {
	return strings.Split(clientKeyHistoryRetentionSchemaSql, "$st_client_key_history_immutable_guard$")[1]
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
