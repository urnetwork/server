// The derived-location migrations need typed schema proof, not only a numeric
// head.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The two appended versions (connect/GEOMAP.md §5.4, §6), each with the one
// contract that owns its positional column.
var migrationDerivedLocationArtifacts = []migrationArtifact{
	{name: "derived_location table and node primary key", requiredVersion: 702, rowColumn: 113},
	{name: "network_client_location.genesis_location_id", requiredVersion: 703, rowColumn: 114},
}

// Every appended version keeps exactly one contract in the positional row.
func TestMigrationDerivedLocationContractsAreComplete(t *testing.T) {
	if server.MigrationCount() < 703 {
		t.Fatal("derived-location migrations have not been appended")
	}
	for _, want := range migrationDerivedLocationArtifacts {
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
			t.Errorf("version %d has %d derived-location artifact contracts, want one", want.requiredVersion, count)
		}
	}
}

// Absent future artifacts are staging, not drift: each prefix of the rollout
// is coherent and reports only the existing head lag.
func TestMigrationDerivedLocationStagedArtifactsDoNotPage(t *testing.T) {
	for version := 701; version <= 703; version++ {
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
func TestMigrationDerivedLocationPublishedArtifactsAreRequired(t *testing.T) {
	for _, artifact := range migrationDerivedLocationArtifacts {
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

// The query interrogates the actual relation, every column's type,
// nullability and default, and the primary key; a catalog label alone is not
// proof.
func TestMigrationDerivedLocationQueryPinsExactShapes(t *testing.T) {
	head := server.MigrationCount()
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return syntheticMigrationCatalogRows(head), nil
		}
		normalized := strings.Join(strings.Fields(query), " ")
		for _, want := range []string{
			"to_regclass('public.derived_location') IS NOT NULL",
			"SELECT count(*) = 20 FROM (VALUES ('node_kind', 'smallint'), ('node_id', 'uuid'), ('genesis_latitude', 'double precision'), ('genesis_longitude', 'double precision'), ('genesis_accuracy_km', 'real')",
			"('delta_latitude', 'double precision'), ('delta_longitude', 'double precision'), ('latitude', 'double precision'), ('longitude', 'double precision')",
			"('ping_count', 'integer'), ('peer_count', 'integer'), ('residual_km', 'real'), ('reputation', 'real'), ('crossed_region', 'boolean'), ('crossed_country', 'boolean')",
			"('location_id', 'uuid'), ('city_location_id', 'uuid'), ('region_location_id', 'uuid'), ('country_location_id', 'uuid'), ('update_time', 'timestamp without time zone')",
			"AND actual.table_name = 'derived_location'",
			"AND actual.is_nullable = 'NO' AND actual.column_default IS NULL",
			"WHERE table_name = 'derived_location' AND constraint_type = 'p' AND definition = 'PRIMARY KEY (node_kind, node_id)' AND validated",
			"table_name = 'network_client_location' AND column_name = 'genesis_location_id' AND data_type = 'uuid' AND is_nullable = 'YES' AND column_default IS NULL",
		} {
			if !strings.Contains(normalized, want) {
				t.Fatalf("derived-location migration query lost %q", want)
			}
		}
		return []Row{syntheticMigrationArtifactRow(head)}, nil
	}}
	if alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source)); err != nil || len(alerts) != 0 {
		t.Fatalf("complete derived-location migration artifact catalog is not coherent: %+v, %v", alerts, err)
	}
}
