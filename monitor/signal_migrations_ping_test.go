// The pinger-reported ping migrations need typed schema proof, not only a
// numeric head: the table, its keys and indexes as first published, and the
// day partitioning that superseded them (connect/GEOMAP.md §5.7, D26).
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The appended ping versions (connect/GEOMAP.md §2.6, §2.7, §2.9, §5.1, §5.7),
// each with the one contract that owns its positional column. The first
// table's key and indexes are required until the day partitioning replaced
// them at 713, and the partitioned table from then on; the tallies the
// dashboard reads follow at 715-718.
var migrationPingArtifacts = []migrationArtifact{
	{name: "network_ping table and primary key", requiredVersion: 693, removedVersion: 713, rowColumn: 104},
	{name: "network_ping_target_pinger_nonce replay index", requiredVersion: 694, removedVersion: 713, rowColumn: 105},
	{name: "network_ping_create_time retention index", requiredVersion: 695, removedVersion: 713, rowColumn: 106},
	{name: "network_ping_target_extender_id_create_time target lookup index", requiredVersion: 696, removedVersion: 713, rowColumn: 107},
	{name: "network_ping_pinger_kind_pinger_id_create_time pinger lookup index", requiredVersion: 697, removedVersion: 713, rowColumn: 108},
	{name: "network_client_location.accuracy_km", requiredVersion: 698, rowColumn: 109},
	{name: "network_extender_activation.accuracy_km", requiredVersion: 699, rowColumn: 110},
	{name: "network_ping.hop_count", requiredVersion: 700, rowColumn: 111},
	{name: "network_ping day partitions, replay key and read indexes", requiredVersion: 713, rowColumn: 124},
	{name: "network_ping_legacy removed", requiredVersion: 714, rowColumn: 125},
	{name: "network_ping_hour_tally table and hour tally key", requiredVersion: 715, rowColumn: 126},
	{name: "network_ping_pinger_day day partitions and pinger key", requiredVersion: 716, rowColumn: 127},
	{name: "network_ping_target_day day partitions and target key", requiredVersion: 717, rowColumn: 128},
	{name: "network_ping_target_hour_tally day partitions and target hour key", requiredVersion: 718, rowColumn: 129},
}

// A healthy row at `version`: every artifact published by then and not yet
// superseded is present, and every other is absent, which is what a database
// mid-rollout looks like.
func syntheticMigrationPingRow(version int) Row {
	row := syntheticMigrationArtifactRow(version)
	for _, artifact := range migrationArtifacts {
		if version < artifact.requiredVersion ||
			(artifact.removedVersion != 0 && artifact.removedVersion <= version) {
			row[artifact.rowColumn] = "f"
		}
	}
	return row
}

// Every appended version keeps exactly one contract in the positional row.
func TestMigrationPingContractsAreComplete(t *testing.T) {
	if server.MigrationCount() < 718 {
		t.Fatal("the ping migrations have not been appended")
	}
	for _, want := range migrationPingArtifacts {
		count := 0
		for _, actual := range migrationArtifacts {
			if actual.requiredVersion == want.requiredVersion {
				count++
				if actual != want {
					t.Errorf("version %d artifact=%+v, want %+v", want.requiredVersion, actual, want)
				}
			}
		}
		if count != 1 {
			t.Errorf("version %d has %d ping artifact contracts, want one", want.requiredVersion, count)
		}
	}
}

// Absent future artifacts are staging, not drift, and so are superseded ones
// past their removal: each prefix of the rollout, the conversion to day
// partitions and the drop of the old table included, is coherent and reports
// only the existing head lag.
func TestMigrationPingStagedArtifactsDoNotPage(t *testing.T) {
	for version := 692; version <= 718; version++ {
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(version), nil
			}
			return []Row{syntheticMigrationPingRow(version)}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatalf("version %d: %v", version, err)
		}
		wantCount := 0
		if version < server.MigrationCount() {
			wantCount = 1
		}
		if len(alerts) != wantCount {
			t.Fatalf("coherent version %d: alerts=%+v, want only existing head-lag evidence", version, alerts)
		}
		for _, alert := range alerts {
			if alert.Class != "migration-behind" {
				t.Fatalf("coherent version %d produced %s", version, alert.Class)
			}
		}
	}
}

// A published artifact that is missing reaches its own paging drift finding,
// named by its version.
func TestMigrationPingPublishedArtifactsAreRequired(t *testing.T) {
	for _, artifact := range migrationPingArtifacts {
		version := artifact.requiredVersion
		row := syntheticMigrationPingRow(version)
		row[artifact.rowColumn] = "f"
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(version), nil
			}
			return []Row{row}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatalf("version %d: %v", version, err)
		}
		want := fmt.Sprintf("%s@v%d", artifact.name, version)
		found := false
		for _, alert := range alerts {
			if alert.Class == "migration-schema-drift" && alert.Severity == SeverityPage && strings.Contains(alert.Markdown(), want) {
				found = true
			}
		}
		if !found {
			t.Errorf("version %d ignored published missing artifact %s: %+v", version, want, alerts)
		}
	}
}

// The query interrogates the actual relation, types, nullability, defaults,
// complete index definitions, the partition key and every partition's name
// against its bounds; a catalog label alone is not proof.
func TestMigrationPingQueryPinsExactShapes(t *testing.T) {
	head := server.MigrationCount()
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return syntheticMigrationCatalogRows(head), nil
		}
		normalized := strings.Join(strings.Fields(query), " ")
		for _, want := range []string{
			"to_regclass('public.network_ping') IS NOT NULL",
			"SELECT count(*) = 12 FROM (VALUES ('ping_id', 'uuid', 'NO', NULL::text[]), ('pinger_kind', 'smallint', 'NO', NULL), ('pinger_id', 'uuid', 'NO', NULL), ('target_extender_id', 'uuid', 'NO', NULL)",
			"('probe_nonce', 'bytea', 'NO', NULL), ('rtt_ms', 'integer', 'NO', NULL), ('probe_time', 'timestamp without time zone', 'NO', NULL), ('cosign', 'smallint', 'NO', NULL)",
			"('cosign_reason', 'smallint', 'NO', ARRAY['0', '0::smallint', '''0''::smallint'])",
			"('pinger_signature', 'bytea', 'NO', NULL), ('cosignature', 'bytea', 'YES', NULL), ('create_time', 'timestamp without time zone', 'NO', ARRAY['now()'])",
			"AND actual.table_name = 'network_ping'",
			"(expected.column_defaults IS NULL AND actual.column_default IS NULL) OR actual.column_default = ANY(expected.column_defaults)",
			"WHERE table_name = 'network_ping' AND constraint_type = 'p' AND definition = 'PRIMARY KEY (ping_id)' AND validated",
			"definition = 'CREATE UNIQUE INDEX network_ping_target_pinger_nonce ON public.network_ping USING btree (target_extender_id, pinger_kind, pinger_id, probe_nonce)'",
			"definition = 'CREATE INDEX network_ping_create_time ON public.network_ping USING btree (create_time)'",
			"definition = 'CREATE INDEX network_ping_target_extender_id_create_time ON public.network_ping USING btree (target_extender_id, create_time)'",
			"definition = 'CREATE INDEX network_ping_pinger_kind_pinger_id_create_time ON public.network_ping USING btree (pinger_kind, pinger_id, create_time)'",
			"table_name = 'network_client_location' AND column_name = 'accuracy_km' AND data_type = 'real' AND is_nullable = 'YES' AND column_default IS NULL",
			"table_name = 'network_extender_activation' AND column_name = 'accuracy_km' AND data_type = 'real' AND is_nullable = 'YES' AND column_default IS NULL",
			"table_name = 'network_ping' AND column_name = 'hop_count' AND data_type = 'smallint' AND is_nullable = 'NO' AND column_default IN ('0', '0::smallint', '''0''::smallint')",
			"AND relation.relname = 'network_ping' AND relation.relkind = 'p' AND pg_get_partkeydef(relation.oid) = 'RANGE (create_time)'",
			"SELECT count(*) = 13 FROM (VALUES ('ping_id', 'uuid', 'NO', NULL::text[])",
			"('hop_count', 'smallint', 'NO', ARRAY['0', '0::smallint', '''0''::smallint']) ) AS expected(column_name, data_type, is_nullable, column_defaults)",
			"('network_ping_target_pinger_nonce', 'CREATE UNIQUE INDEX network_ping_target_pinger_nonce ON ONLY public.network_ping USING btree (target_extender_id, pinger_kind, pinger_id, probe_nonce, create_time)')",
			"('network_ping_target_extender_id_create_time', 'CREATE INDEX network_ping_target_extender_id_create_time ON ONLY public.network_ping USING btree (target_extender_id, create_time)')",
			"('network_ping_pinger_kind_pinger_id_create_time', 'CREATE INDEX network_ping_pinger_kind_pinger_id_create_time ON ONLY public.network_ping USING btree (pinger_kind, pinger_id, create_time)')",
			"AND actual.definition = expected.definition AND actual.predicate_definition IS NULL AND actual.indisvalid AND actual.indisready",
			"WHERE inheritance.inhparent = to_regclass('public.network_ping') AND NOT coalesce( partition_relation.relkind = 'r' AND bound.lower_bound = date_trunc('day', bound.lower_bound) AND bound.upper_bound = bound.lower_bound + interval '1 day' AND partition_relation.relname = 'network_ping_p' || to_char(bound.lower_bound, 'YYYYMMDD'), false )",
			"to_regclass('public.network_ping_legacy') IS NULL",
			"SELECT relkind = 'r' FROM pg_class WHERE oid = to_regclass('public.network_ping_hour_tally')",
			"('beyond_half_planet_count', 'bigint') ) AS expected(column_name, data_type)",
			"AND actual.table_name = 'network_ping_hour_tally'",
			"definition = 'PRIMARY KEY (hour, shard, pinger_kind, relayed, cosign, cosign_reason)'",
			"SELECT relkind = 'p' AND pg_get_partkeydef(oid) = 'RANGE (day)' FROM pg_class WHERE oid = to_regclass('public.network_ping_pinger_day')",
			"definition = 'PRIMARY KEY (day, pinger_kind, pinger_id)'",
			"WHERE inheritance.inhparent = to_regclass('public.network_ping_pinger_day')",
			"partition_relation.relname = 'network_ping_pinger_day_p' || to_char(bound.lower_bound, 'YYYYMMDD')",
			"definition = 'PRIMARY KEY (day, target_extender_id)'",
			"partition_relation.relname = 'network_ping_target_day_p' || to_char(bound.lower_bound, 'YYYYMMDD')",
			"SELECT relkind = 'p' AND pg_get_partkeydef(oid) = 'RANGE (hour)' FROM pg_class WHERE oid = to_regclass('public.network_ping_target_hour_tally')",
			"definition = 'PRIMARY KEY (hour, target_extender_id, pinger_kind)'",
			"partition_relation.relname = 'network_ping_target_hour_tally_p' || to_char(bound.lower_bound, 'YYYYMMDD')",
		} {
			if !strings.Contains(normalized, want) {
				t.Fatalf("ping migration query lost %q", want)
			}
		}
		return []Row{syntheticMigrationArtifactRow(head)}, nil
	}}
	if alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source)); err != nil || len(alerts) != 0 {
		t.Fatalf("complete ping migration artifact catalog is not coherent: %+v, %v", alerts, err)
	}
}

// A source that runs the migration signal's queries on the test database and
// keeps the artifact row it returned, so a test can read one column of it.
type migrationPingDatabaseSource struct {
	syntheticSource
	artifactRow Row
}

// Implements SignalSource on the test database, rendering values as psql does.
func (self *migrationPingDatabaseSource) PostgreSQL(ctx context.Context, query string) ([]Row, error) {
	rows := []Row{}
	var queryErr error
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, query)
		if err != nil {
			queryErr = err
			return
		}
		defer result.Close()
		for result.Next() {
			values, err := result.Values()
			if err != nil {
				queryErr = err
				return
			}
			row := make(Row, len(values))
			for i, value := range values {
				switch v := value.(type) {
				case nil:
					row[i] = ""
				case bool:
					row[i] = "f"
					if v {
						row[i] = "t"
					}
				default:
					row[i] = fmt.Sprint(v)
				}
			}
			rows = append(rows, row)
		}
		queryErr = result.Err()
	})
	if queryErr == nil && !strings.Contains(query, "FROM migration_catalog") && len(rows) == 1 {
		self.artifactRow = rows[0]
	}
	return rows, queryErr
}

// The migration signal's own query over a real database: the artifact row it
// read, and the names of the artifacts its drift finding lists.
func migrationPingDatabaseCheck(t testing.TB, ctx context.Context) (Row, string) {
	t.Helper()
	source := &migrationPingDatabaseSource{}
	alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if source.artifactRow == nil {
		t.Fatal("the signal read no artifact row")
	}
	drift := ""
	for _, alert := range alerts {
		if alert.Class == "migration-schema-drift" {
			drift = alert.Markdown()
		}
	}
	return source.artifactRow, drift
}

// On a database migrated to the head, the partitioned table meets its
// contract and the first table's key and indexes read absent, which past
// their removal is no drift; a partition named against its bounds, or an old
// table back, breaks the contract that owns it.
func TestMigrationPingPartitionArtifactsOnAMigratedDatabase(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		row, drift := migrationPingDatabaseCheck(t, ctx)
		for _, artifact := range migrationPingArtifacts {
			want := "t"
			if artifact.removedVersion != 0 {
				want = "f"
			}
			if row[artifact.rowColumn] != want {
				t.Errorf("%s@v%d reads %q on the migrated database, want %q", artifact.name, artifact.requiredVersion, row[artifact.rowColumn], want)
			}
			if strings.Contains(drift, artifact.name+"@") {
				t.Errorf("the migrated database drifts on %s: %s", artifact.name, drift)
			}
		}

		for _, fault := range []struct {
			sql      string
			artifact string
		}{
			{
				sql:      `CREATE TABLE network_ping_synthetic_other PARTITION OF network_ping FOR VALUES FROM ('2099-01-01') TO ('2099-01-02')`,
				artifact: "network_ping day partitions, replay key and read indexes@v713",
			},
			{
				sql:      `CREATE TABLE network_ping_p20990201 PARTITION OF network_ping FOR VALUES FROM ('2099-02-01') TO ('2099-02-03')`,
				artifact: "network_ping day partitions, replay key and read indexes@v713",
			},
			{
				sql:      `CREATE TABLE network_ping_legacy (ping_id uuid NOT NULL)`,
				artifact: "network_ping_legacy removed@v714",
			},
			{
				sql:      `CREATE TABLE network_ping_pinger_day_synthetic_other PARTITION OF network_ping_pinger_day FOR VALUES FROM ('2099-01-01') TO ('2099-01-02')`,
				artifact: "network_ping_pinger_day day partitions and pinger key@v716",
			},
			{
				sql:      `CREATE TABLE network_ping_target_hour_tally_p20990201 PARTITION OF network_ping_target_hour_tally FOR VALUES FROM ('2099-02-01') TO ('2099-02-03')`,
				artifact: "network_ping_target_hour_tally day partitions and target hour key@v718",
			},
		} {
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, fault.sql))
			})
			_, drift := migrationPingDatabaseCheck(t, ctx)
			if !strings.Contains(drift, fault.artifact) {
				t.Errorf("%s left %s unreported: %q", fault.sql, fault.artifact, drift)
			}
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, "DROP TABLE "+strings.Fields(fault.sql)[2]))
			})
		}
		_, drift = migrationPingDatabaseCheck(t, ctx)
		for _, artifact := range migrationPingArtifacts {
			if strings.Contains(drift, artifact.name+"@") {
				t.Errorf("the repaired database still drifts on %s: %s", artifact.name, drift)
			}
		}
	})
}

// On a database stopped just before the conversion, the first table's key and
// indexes meet their contracts and the partitioned table's is not yet due, so
// the head lag is the only finding.
func TestMigrationPingArtifactsBeforeThePartitionConversion(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		conversionVersion := 0
		for _, artifact := range migrationPingArtifacts {
			if artifact.name == "network_ping day partitions, replay key and read indexes" {
				conversionVersion = artifact.requiredVersion
			}
		}
		server.ApplyDbMigrationsUpTo(ctx, conversionVersion-1)
		row, drift := migrationPingDatabaseCheck(t, ctx)
		for _, artifact := range migrationPingArtifacts {
			if strings.Contains(drift, artifact.name+"@") {
				t.Errorf("the database before the conversion drifts on %s: %s", artifact.name, drift)
			}
			want := "t"
			if conversionVersion <= artifact.requiredVersion {
				want = "f"
			}
			if artifact.requiredVersion == conversionVersion+1 {
				// no old table exists yet, so it is absent before its drop too
				want = "t"
			}
			if row[artifact.rowColumn] != want {
				t.Errorf("%s@v%d reads %q before the conversion, want %q", artifact.name, artifact.requiredVersion, row[artifact.rowColumn], want)
			}
		}
	})
}
