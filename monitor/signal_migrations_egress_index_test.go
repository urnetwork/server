// The egress index migrations need typed schema proof, not only a numeric
// head.
package monitor

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The three appended versions (connect/GEOMAP.md §10.4), each with the one
// contract that owns its positional column.
var migrationEgressIndexArtifacts = []migrationArtifact{
	{name: "network_client_location_reliability.egress_index", requiredVersion: 703, rowColumn: 114},
	{name: "network_client_location_reliability.egress_quality", requiredVersion: 704, rowColumn: 115},
	{name: "network_client_location_reliability.egress_evidence_time", requiredVersion: 705, rowColumn: 116},
}

// Every appended version keeps exactly one contract in the positional row.
func TestMigrationEgressIndexContractsAreComplete(t *testing.T) {
	if server.MigrationCount() < 705 {
		t.Fatal("egress index migrations have not been appended")
	}
	for _, want := range migrationEgressIndexArtifacts {
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
			t.Errorf("version %d has %d egress index artifact contracts, want one", want.requiredVersion, count)
		}
	}
}

// Absent future artifacts are staging, not drift: each prefix of the rollout
// is coherent and reports only the existing head lag.
func TestMigrationEgressIndexStagedArtifactsDoNotPage(t *testing.T) {
	for version := 702; version <= 705; version++ {
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
func TestMigrationEgressIndexPublishedArtifactsAreRequired(t *testing.T) {
	for _, artifact := range migrationEgressIndexArtifacts {
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

// The query interrogates each column's relation, type, nullability and
// default; a catalog label alone is not proof.
func TestMigrationEgressIndexQueryPinsExactShapes(t *testing.T) {
	head := server.MigrationCount()
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return syntheticMigrationCatalogRows(head), nil
		}
		normalized := strings.Join(strings.Fields(query), " ")
		for _, want := range []string{
			"table_name = 'network_client_location_reliability' AND column_name = 'egress_index' AND data_type = 'smallint' AND is_nullable = 'YES' AND column_default IS NULL",
			"table_name = 'network_client_location_reliability' AND column_name = 'egress_quality' AND data_type = 'boolean' AND is_nullable = 'YES' AND column_default IS NULL",
			"table_name = 'network_client_location_reliability' AND column_name = 'egress_evidence_time' AND data_type = 'timestamp without time zone' AND is_nullable = 'YES' AND column_default IS NULL",
		} {
			if !strings.Contains(normalized, want) {
				t.Fatalf("egress index migration query lost %q", want)
			}
		}
		return []Row{syntheticMigrationArtifactRow(head)}, nil
	}}
	if alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source)); err != nil || len(alerts) != 0 {
		t.Fatalf("complete egress index migration artifact catalog is not coherent: %+v, %v", alerts, err)
	}
}

// Execute the emitted column guards over synthetic catalog values on local
// PostgreSQL: the canonical nullable, no-default column is admitted and every
// look-alike is not. No schema, migration history or production data changes.
func TestMigrationEgressIndexColumnsExecuteExactGuards(t *testing.T) {
	if os.Getenv("WARP_ENV") != "local" {
		t.Skip("the egress index column guards execute against the attested local test environment")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	query := ""
	source := &syntheticSource{postgresFn: func(emitted string) ([]Row, error) {
		if !strings.Contains(emitted, "FROM migration_catalog") {
			query = emitted
		}
		if strings.Contains(emitted, "FROM migration_catalog") {
			return syntheticMigrationCatalogRows(server.MigrationCount()), nil
		}
		return []Row{syntheticMigrationArtifactRow(server.MigrationCount())}, nil
	}}
	if _, err := NewMigrationsSignal().Run(ctx, syntheticSettings(source)); err != nil {
		t.Fatal(err)
	}
	normalized := strings.Join(strings.Fields(query), " ")

	server.Db(ctx, func(conn server.PgConn) {
		for _, column := range []struct {
			name string
			kind string
		}{
			{name: "egress_index", kind: "smallint"},
			{name: "egress_quality", kind: "boolean"},
			{name: "egress_evidence_time", kind: "timestamp without time zone"},
		} {
			marker := "EXISTS ( SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'network_client_location_reliability' AND column_name = '" + column.name + "'"
			if strings.Count(normalized, marker) != 1 {
				t.Fatalf("%s has no unique relation-scoped column guard", column.name)
			}
			start := strings.Index(normalized, marker)
			// the next artifact, or the end of the select list
			guard, _, ok := strings.Cut(normalized[start:], " ),")
			if last, _, lastOk := strings.Cut(normalized[start:], " ) FROM version;"); lastOk && (!ok || len(last) < len(guard)) {
				guard, ok = last, true
			}
			if !ok {
				t.Fatalf("%s column guard has no following artifact boundary", column.name)
			}
			guard = strings.ReplaceAll(guard+" )", "information_schema.columns", "observed_column")

			for _, fault := range []string{"healthy", "missing", "wrong table", "wrong type", "not nullable", "default"} {
				table, kind, nullable, present := "network_client_location_reliability", column.kind, "YES", true
				var defaults *string
				switch fault {
				case "missing":
					present = false
				case "wrong table":
					table = "network_client_location"
				case "wrong type":
					kind = "integer"
				case "not nullable":
					nullable = "NO"
				case "default":
					value := "NULL::" + column.kind
					defaults = &value
				}
				var admitted bool
				err := conn.QueryRow(ctx, `
					WITH observed_column AS (
						SELECT 'public'::text AS table_schema, $1::text AS table_name,
						       $2::text AS column_name, $3::text AS data_type,
						       $4::text AS is_nullable, $5::text AS column_default
						WHERE $6::boolean
					)
					SELECT `+guard,
					table, column.name, kind, nullable, defaults, present,
				).Scan(&admitted)
				if err != nil {
					t.Fatalf("%s %s: %v", column.name, fault, err)
				}
				if admitted != (fault == "healthy") {
					t.Errorf("%s %s: admitted=%t", column.name, fault, admitted)
				}
			}
		}
	}, server.OptNoRetry())
}
