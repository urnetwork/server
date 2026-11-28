package monitor

import (
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The device day rollup's contract follows its migration's position.
func TestAuditDeviceDayMigrationContractFollowsItsMigration(t *testing.T) {
	version := server.AuditDeviceDayVersion()
	if version <= 0 || server.MigrationCount() < version {
		t.Fatalf("the rollup migration is at %d of %d", version, server.MigrationCount())
	}
	if auditDeviceDayMigrationContract.artifact.requiredVersion != version {
		t.Fatalf("the contract pins %d, the migration is at %d", auditDeviceDayMigrationContract.artifact.requiredVersion, version)
	}
}

// After the audit_device_event cutover every published artifact still reads
// present, the rollup's included, and the device index the partitioned parent
// renders ON ONLY reads as published.
func TestMigrationsAuditDeviceEventPartitionedCatalog(t *testing.T) {
	migrationContractTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.ApplyDbMigrations(ctx)
		server.Testing_PartitionAuditDeviceEvents(ctx, server.PartitionGranularityDaily.AddRanges(server.PartitionGranularityDaily.RangeStart(server.NowUtc()), -10))
		if !server.IsAuditDeviceEventPartitioned(ctx) {
			t.Fatal("the cutover left audit_device_event plain")
		}
		row, drift := migrationPingDatabaseCheck(t, ctx)
		if drift != "" {
			t.Fatalf("a partitioned audit_device_event reports drift at the head: %s", drift)
		}
		head := server.MigrationCount()
		for _, artifact := range migrationArtifacts {
			if artifact.removedVersion != 0 && artifact.removedVersion <= head {
				continue
			}
			if !migrationBool(row[artifact.rowColumn]) {
				t.Fatalf("%s@v%d reads absent on the partitioned layout", artifact.name, artifact.requiredVersion)
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var definition string
			server.Raise(conn.QueryRow(ctx, `SELECT `+migrationIndexDefinitionSql("index_relation", "table_relation.relname")+`
 FROM pg_index index_record
 JOIN pg_class index_relation ON index_relation.oid=index_record.indexrelid
 JOIN pg_class table_relation ON table_relation.oid=index_record.indrelid
 WHERE index_record.indexrelid=to_regclass('public.audit_device_event_device_id_event_time')`).Scan(&definition))
			if want := "CREATE INDEX audit_device_event_device_id_event_time ON public.audit_device_event USING btree (device_id, event_time, event_id) INCLUDE (event_type)"; definition != want {
				t.Fatalf("the device index reads %q", definition)
			}
		})
	})
}
