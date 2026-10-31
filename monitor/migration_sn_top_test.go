package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// A known numeric head cannot certify these newly published tables when one
// is absent. This fails against the predecessor monitor's healthy-only result.
func TestSnTopMigrationMissingTablesReachSignal(t *testing.T) {
	chainVersion := server.SnChainSchemaMigrationVersion()
	topVersion := server.SnTopSchemaMigrationVersion()
	faults := []struct {
		table, owner string
		version      int
	}{
		{table: "sn_fleet_binding", owner: "SN chain-side top-board custody", version: chainVersion},
		{table: "sn_fleet_binding_sync", owner: "SN chain-side top-board custody", version: chainVersion},
		{table: "sn_incentive_snapshot", owner: "SN chain-side top-board custody", version: chainVersion},
		{table: "sn_incentive_uid", owner: "SN chain-side top-board custody", version: chainVersion},
		{table: "sn_head_summary", owner: "SN chain-side top-board custody", version: chainVersion},
		{table: "sn_head_summary_fleet", owner: "SN chain-side top-board custody", version: chainVersion},
		{table: "sn_trail_hop", owner: "SN top-board snapshots and history", version: topVersion},
		{table: "sn_top_snapshot", owner: "SN top-board snapshots and history", version: topVersion},
		{table: "sn_top_network", owner: "SN top-board snapshots and history", version: topVersion},
		{table: "sn_top_fleet", owner: "SN top-board snapshots and history", version: topVersion},
		{table: "sn_top_ema_state", owner: "SN top-board snapshots and history", version: topVersion},
		{table: "sn_top_network_daily", owner: "SN top-board snapshots and history", version: topVersion},
	}
	var observed []snMainnetMigrationAlertObservation
	snMainnetMigrationTestEnv().Run(t, func(tb testing.TB) {
		snTopMigrationRequireHealthy(tb, "published table baseline")
		for _, fault := range faults {
			snMainnetMigrationFaultTx(tb, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, "DROP TABLE "+fault.table); err != nil {
					tb.Fatalf("drop synthetic fixture table %s: %v", fault.table, err)
				}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					tb.Fatalf("inspect missing %s: %v", fault.table, err)
				}
				observed = append(observed, snMainnetMigrationAlertObservation{
					alerts: alerts, want: fmt.Sprintf("%s@v%d", fault.owner, fault.version),
				})
			})
		}
		snTopMigrationRequireHealthy(tb, "missing-table rollback")
	})
	if len(observed) != len(faults) {
		t.Fatalf("observed %d missing-table faults, want %d", len(observed), len(faults))
	}
	for index, observation := range observed {
		if alert := requireAlertClass(t, observation.alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), observation.want) {
			t.Fatalf("missing %s lost its owning migration: %s", faults[index].table, alert.Markdown())
		}
	}
}

// Present names and a matching audit head do not replace exact column, key,
// check and index shapes. Every mutation stays in a rolled-back local fixture.
func TestSnTopMigrationShapeFaultsReachSignal(t *testing.T) {
	chainVersion := server.SnChainSchemaMigrationVersion()
	topVersion := server.SnTopSchemaMigrationVersion()
	replicaVersion := server.SnTopReplicaSchemaMigrationVersion()
	const replicaOwner = "SN top-board replica provenance and genesis summary index"
	faults := []struct {
		name, owner, sql string
		version          int
	}{
		// Look-alike indexes under the published names: uniqueness and key
		// order are part of each pinned definition.
		{name: "binding log position index made non-unique", owner: "SN chain-side top-board custody", version: chainVersion, sql: `DROP INDEX sn_fleet_binding_bound; CREATE INDEX sn_fleet_binding_bound ON sn_fleet_binding (deployment_key, client_id, bound_block, bound_log_index)`},
		{name: "network position index reordered", owner: "SN top-board snapshots and history", version: topVersion, sql: `DROP INDEX sn_top_network_snapshot_position; CREATE INDEX sn_top_network_snapshot_position ON sn_top_network ("position", snapshot_id)`},
		{name: "replica validator hotkey absent", owner: replicaOwner, version: replicaVersion, sql: `ALTER TABLE sn_top_snapshot DROP COLUMN replica_validator_hotkey`},
		{name: "replica epoch narrowed", owner: replicaOwner, version: replicaVersion, sql: `ALTER TABLE sn_top_snapshot ALTER COLUMN replica_native_epoch TYPE integer`},
		{name: "replica completeness nullable", owner: replicaOwner, version: replicaVersion, sql: `ALTER TABLE sn_top_snapshot ALTER COLUMN replica_complete DROP NOT NULL`},
		{name: "replica completeness defaults complete", owner: replicaOwner, version: replicaVersion, sql: `ALTER TABLE sn_top_snapshot ALTER COLUMN replica_complete SET DEFAULT true`},
		{name: "fleet replica availability absent", owner: replicaOwner, version: replicaVersion, sql: `ALTER TABLE sn_top_fleet DROP COLUMN replica_available`},
		{name: "genesis marker default removed", owner: replicaOwner, version: replicaVersion, sql: `ALTER TABLE sn_head_summary ALTER COLUMN replica_genesis DROP DEFAULT`},
		{name: "genesis summary index removed", owner: replicaOwner, version: replicaVersion, sql: `DROP INDEX sn_head_summary_replica_genesis`},
		{name: "genesis summary index covers every summary", owner: replicaOwner, version: replicaVersion, sql: `DROP INDEX sn_head_summary_replica_genesis; CREATE INDEX sn_head_summary_replica_genesis ON sn_head_summary (netuid, validator_hotkey, native_epoch)`},
		{name: "genesis summary index reordered", owner: replicaOwner, version: replicaVersion, sql: `DROP INDEX sn_head_summary_replica_genesis; CREATE INDEX sn_head_summary_replica_genesis ON sn_head_summary (validator_hotkey, netuid, native_epoch) WHERE replica_genesis = true`},
		{name: "binding generation narrowed", owner: "SN chain-side top-board custody", version: chainVersion, sql: `ALTER TABLE sn_fleet_binding ALTER COLUMN generation TYPE integer`},
		{name: "binding hotkey nullable", owner: "SN chain-side top-board custody", version: chainVersion, sql: `ALTER TABLE sn_fleet_binding ALTER COLUMN hotkey DROP NOT NULL`},
		{name: "sync block hash narrowed", owner: "SN chain-side top-board custody", version: chainVersion, sql: `ALTER TABLE sn_fleet_binding_sync ALTER COLUMN block_hash TYPE varchar(79)`},
		{name: "incentive clock zoned", owner: "SN chain-side top-board custody", version: chainVersion, sql: `ALTER TABLE sn_incentive_snapshot ALTER COLUMN read_at TYPE timestamp with time zone`},
		{name: "optional incentive hotkey required", owner: "SN chain-side top-board custody", version: chainVersion, sql: `ALTER TABLE sn_incentive_uid ALTER COLUMN hotkey SET NOT NULL`},
		{name: "head score narrowed", owner: "SN chain-side top-board custody", version: chainVersion, sql: `ALTER TABLE sn_head_summary_fleet ALTER COLUMN score TYPE real`},
		{name: "top snapshot default invented", owner: "SN top-board snapshots and history", version: topVersion, sql: `ALTER TABLE sn_top_snapshot ALTER COLUMN total_ranked SET DEFAULT 0`},
		{name: "top label widened", owner: "SN top-board snapshots and history", version: topVersion, sql: `ALTER TABLE sn_top_network ALTER COLUMN label TYPE varchar(17)`},
		{name: "top fleet network array changed", owner: "SN top-board snapshots and history", version: topVersion, sql: `ALTER TABLE sn_top_fleet ALTER COLUMN network_ids TYPE text[] USING network_ids::text[]`},
		{name: "exact EMA required", owner: "SN top-board snapshots and history", version: topVersion, sql: `ALTER TABLE sn_top_ema_state ALTER COLUMN ema_exact SET NOT NULL`},
		{name: "daily date changed", owner: "SN top-board snapshots and history", version: topVersion, sql: `ALTER TABLE sn_top_network_daily ALTER COLUMN day TYPE timestamp without time zone USING day::timestamp`},
		{name: "trail prefix width unchecked", owner: "SN top-board snapshots and history", version: topVersion, sql: `ALTER TABLE sn_trail_hop DROP CONSTRAINT sn_trail_hop_prefix_hash_check`},
	}
	for _, table := range []string{"sn_fleet_binding", "sn_fleet_binding_sync", "sn_incentive_snapshot", "sn_incentive_uid", "sn_head_summary", "sn_head_summary_fleet"} {
		faults = append(faults, struct {
			name, owner, sql string
			version          int
		}{name: table + " primary key removed", owner: "SN chain-side top-board custody", version: chainVersion, sql: "ALTER TABLE " + table + " DROP CONSTRAINT " + table + "_pkey"})
	}
	for _, table := range []string{"sn_trail_hop", "sn_top_snapshot", "sn_top_network", "sn_top_fleet", "sn_top_ema_state", "sn_top_network_daily"} {
		faults = append(faults, struct {
			name, owner, sql string
			version          int
		}{name: table + " primary key removed", owner: "SN top-board snapshots and history", version: topVersion, sql: "ALTER TABLE " + table + " DROP CONSTRAINT " + table + "_pkey"})
	}
	for _, check := range []struct{ table, constraint string }{
		{table: "sn_fleet_binding", constraint: "sn_fleet_binding_fleet_id_check"},
		{table: "sn_fleet_binding", constraint: "sn_fleet_binding_hotkey_check"},
		{table: "sn_fleet_binding", constraint: "sn_fleet_binding_uid_check"},
		{table: "sn_fleet_binding", constraint: "sn_fleet_binding_check"},
		{table: "sn_incentive_uid", constraint: "sn_incentive_uid_uid_check"},
		{table: "sn_incentive_uid", constraint: "sn_incentive_uid_hotkey_check"},
		{table: "sn_incentive_uid", constraint: "sn_incentive_uid_check"},
		{table: "sn_head_summary", constraint: "sn_head_summary_netuid_check"},
		{table: "sn_head_summary", constraint: "sn_head_summary_validator_hotkey_check"},
		{table: "sn_head_summary", constraint: "sn_head_summary_native_snapshot_hash_check"},
		{table: "sn_head_summary", constraint: "sn_head_summary_evm_snapshot_hash_check"},
		{table: "sn_head_summary", constraint: "sn_head_summary_policy_hash_check"},
		{table: "sn_head_summary", constraint: "sn_head_summary_validator_uid_check"},
		{table: "sn_head_summary_fleet", constraint: "sn_head_summary_fleet_uid_check"},
		{table: "sn_head_summary_fleet", constraint: "sn_head_summary_fleet_fleet_id_check"},
		{table: "sn_head_summary_fleet", constraint: "sn_head_summary_fleet_hotkey_check"},
	} {
		faults = append(faults, struct {
			name, owner, sql string
			version          int
		}{name: check.constraint + " removed", owner: "SN chain-side top-board custody", version: chainVersion, sql: "ALTER TABLE " + check.table + " DROP CONSTRAINT " + check.constraint})
	}
	for _, index := range []struct {
		name, owner string
		version     int
	}{
		{name: "sn_fleet_binding_bound", owner: "SN chain-side top-board custody", version: chainVersion},
		{name: "sn_incentive_snapshot_read_at", owner: "SN chain-side top-board custody", version: chainVersion},
		{name: "sn_head_summary_received_at", owner: "SN chain-side top-board custody", version: chainVersion},
		{name: "sn_trail_hop_confirm_time", owner: "SN top-board snapshots and history", version: topVersion},
		{name: "sn_top_snapshot_computed_at", owner: "SN top-board snapshots and history", version: topVersion},
		{name: "sn_top_network_snapshot_position", owner: "SN top-board snapshots and history", version: topVersion},
		{name: "sn_top_ema_state_update_time", owner: "SN top-board snapshots and history", version: topVersion},
		{name: "sn_top_network_daily_day", owner: "SN top-board snapshots and history", version: topVersion},
	} {
		faults = append(faults, struct {
			name, owner, sql string
			version          int
		}{name: index.name + " removed", owner: index.owner, version: index.version, sql: "DROP INDEX " + index.name})
	}
	var observed []snMainnetMigrationAlertObservation
	snMainnetMigrationTestEnv().Run(t, func(tb testing.TB) {
		snTopMigrationRequireHealthy(tb, "published shape baseline")
		for _, fault := range faults {
			snMainnetMigrationFaultTx(tb, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					tb.Fatalf("apply %s: %v", fault.name, err)
				}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					tb.Fatalf("inspect %s: %v", fault.name, err)
				}
				observed = append(observed, snMainnetMigrationAlertObservation{
					alerts: alerts, want: fmt.Sprintf("%s@v%d", fault.owner, fault.version),
				})
			})
		}
		snTopMigrationRequireHealthy(tb, "shape rollback")
	})
	if len(observed) != len(faults) {
		t.Fatalf("observed %d shape faults, want %d", len(observed), len(faults))
	}
	for index, observation := range observed {
		if alert := requireAlertClass(t, observation.alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), observation.want) {
			t.Fatalf("%s lost its owning migration: %s", faults[index].name, alert.Markdown())
		}
	}
}

// Capture source-owned catalog facts only after the signal releases its query
// connection; healthy controls do not allocate a second database connection.
func snTopMigrationRequireHealthy(t testing.TB, phase string) {
	t.Helper()
	if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
		server.Db(t.Context(), func(conn server.PgConn) {
			reference, err := snTopMigrationWriteCatalogDiagnostic(t.Context(), conn)
			if err != nil {
				t.Log("synthetic native catalog diagnostic unavailable")
			} else {
				t.Log("synthetic native catalog diagnostic " + reference)
			}
		})
		t.Fatalf("%s reports SN top-board schema drift: %s", phase, drift)
	}
}

// Read only the twelve published fixture tables' schema, never application
// rows. Constraint bodies cover only required primary keys and checks; column
// nullability is already captured above. Counts retain every constraint kind.
// The same queryer may be a connection or its already-owned transaction.
func snTopMigrationWriteCatalogDiagnostic(ctx context.Context, source server.PgCanQuery) (string, error) {
	const query = `WITH expected(name) AS (VALUES
		('sn_fleet_binding'), ('sn_fleet_binding_sync'), ('sn_incentive_snapshot'),
		('sn_incentive_uid'), ('sn_head_summary'), ('sn_head_summary_fleet'),
		('sn_trail_hop'), ('sn_top_snapshot'), ('sn_top_network'), ('sn_top_fleet'),
		('sn_top_ema_state'), ('sn_top_network_daily')
	), relations AS (
		SELECT c.oid, c.relname, c.relkind, c.relpersistence
		FROM expected e JOIN pg_class c ON c.oid=to_regclass('public.' || e.name)
	)
	SELECT 'table'::text, r.relname::text, r.relname::text,
		jsonb_build_object('kind',r.relkind,'persistence',r.relpersistence)
	FROM relations r
	UNION ALL
	SELECT 'column',r.relname::text,a.attname::text,
		jsonb_build_object('type',format_type(a.atttypid,a.atttypmod),
		'not_null',a.attnotnull,'generated',a.attgenerated,'identity',a.attidentity,
		'default',pg_get_expr(d.adbin,d.adrelid))
	FROM relations r JOIN pg_attribute a ON a.attrelid=r.oid
	LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
	WHERE a.attnum>0 AND NOT a.attisdropped
	UNION ALL
	SELECT 'constraint',r.relname::text,k.conname::text,
		jsonb_build_object('definition',pg_get_constraintdef(k.oid),
		'validated',k.convalidated,'deferrable',k.condeferrable,'deferred',k.condeferred)
	FROM relations r JOIN pg_constraint k ON k.conrelid=r.oid
	WHERE k.contype IN ('p','c')
	UNION ALL
	SELECT 'census','sn_schema','constraint_kind_counts',
		jsonb_object_agg(contype, constraint_count)
	FROM (
		SELECT k.contype::text, count(*) AS constraint_count
		FROM relations r JOIN pg_constraint k ON k.conrelid=r.oid
		GROUP BY k.contype
	) constraint_counts
	UNION ALL
	SELECT 'index',r.relname::text,i.relname::text,
		jsonb_build_object('definition',pg_get_indexdef(i.oid),
		'valid',x.indisvalid,'ready',x.indisready,
		'predicate',pg_get_expr(x.indpred,x.indrelid))
	FROM relations r JOIN pg_index x ON x.indrelid=r.oid
	JOIN pg_class i ON i.oid=x.indexrelid
	ORDER BY 1,2,3 LIMIT 257`
	rows, err := source.Query(ctx, query)
	if err != nil {
		return "", errors.New("synthetic SN catalog read unavailable")
	}
	defer rows.Close()
	type record struct {
		Kind  string          `json:"kind"`
		Table string          `json:"table"`
		Name  string          `json:"name"`
		Shape json.RawMessage `json:"shape"`
	}
	var records []record
	for rows.Next() {
		if len(records) == 256 {
			return "", errors.New("synthetic SN catalog row bound exceeded")
		}
		var value record
		var shape []byte
		if err := rows.Scan(&value.Kind, &value.Table, &value.Name, &shape); err != nil {
			return "", errors.New("synthetic SN catalog row unavailable")
		}
		value.Shape = json.RawMessage(shape)
		records = append(records, value)
	}
	if rows.Err() != nil {
		return "", errors.New("synthetic SN catalog iteration incomplete")
	}
	raw, err := json.Marshal(records)
	if err != nil || len(raw) > 64*1024 {
		return "", errors.New("synthetic SN catalog body bound exceeded")
	}
	// A private directory under the qualified profile TMPDIR survives test and
	// container cleanup; t.TempDir would erase the decisive failed-run artifact.
	directory, err := os.MkdirTemp("", "monitor-sn-top-catalog-")
	if err != nil {
		return "", errors.New("synthetic SN catalog private directory unavailable")
	}
	path := filepath.Join(directory, "catalog.private.json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", errors.New("synthetic SN catalog private output unavailable")
	}
	defer file.Close()
	if _, err := file.Write(raw); err != nil {
		return "", errors.New("synthetic SN catalog private write incomplete")
	}
	if err := file.Sync(); err != nil {
		return "", errors.New("synthetic SN catalog private sync incomplete")
	}
	if err := file.Close(); err != nil {
		return "", errors.New("synthetic SN catalog private close incomplete")
	}
	return fmt.Sprintf("path=%s sha256=%x rows=%d bytes=%d", path, sha256.Sum256(raw), len(records), len(raw)), nil
}
