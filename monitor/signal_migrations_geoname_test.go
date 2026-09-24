// The GeoNames migrations need typed schema proof, not only a numeric head.
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

// Keep the new columns explicit even against a pre-fix catalog. The old
// reducer must fail for ignoring published evidence, not an out-of-bounds row.
func syntheticMigrationGeonameRow(version int, column, index bool) Row {
	row := syntheticMigrationArtifactRow(version)
	for len(row) < 104 {
		row = append(row, "t")
	}
	row[102] = fmt.Sprint(column)
	row[103] = fmt.Sprint(index)
	return row
}

// Both appended versions keep one contract in the positional SQL row.
func TestMigrationGeonameContractsAreComplete(t *testing.T) {
	if server.MigrationCount() < 692 {
		t.Fatal("GeoNames migrations have not been appended")
	}
	for _, want := range []migrationArtifact{
		{name: "location.geoname_id", requiredVersion: 691, rowColumn: 102},
		{name: "location_geoname_id", requiredVersion: 692, rowColumn: 103},
	} {
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
			t.Errorf("version %d has %d GeoNames artifact contracts, want one", want.requiredVersion, count)
		}
	}
}

// Absent future artifacts are staging, not drift. Each healthy published
// prefix remains coherent while the existing head-lag warning stays intact.
func TestMigrationGeonameStagedArtifactsDoNotPage(t *testing.T) {
	for _, version := range []int{690, 691, 692} {
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(version), nil
			}
			return []Row{syntheticMigrationGeonameRow(version, 691 <= version, 692 <= version)}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(t.Context(), syntheticSettings(source))
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

// Published-but-missing column and index evidence must reach their own drift
// finding; this control also fails deterministically with the old catalog.
func TestMigrationGeonamePublishedArtifactsAreRequired(t *testing.T) {
	for _, test := range []struct {
		version int
		column  bool
		want    string
	}{
		{version: 691, want: "location.geoname_id@v691"},
		{version: 692, column: true, want: "location_geoname_id@v692"},
	} {
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(test.version), nil
			}
			return []Row{syntheticMigrationGeonameRow(test.version, test.column, false)}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(t.Context(), syntheticSettings(source))
		if err != nil {
			t.Fatalf("version %d: %v", test.version, err)
		}
		found := false
		for _, alert := range alerts {
			if alert.Class == "migration-schema-drift" && alert.Severity == SeverityPage && strings.Contains(alert.Markdown(), test.want) {
				found = true
			}
		}
		if !found {
			t.Errorf("version %d ignored published missing artifact %s: %+v", test.version, test.want, alerts)
		}
	}
}

// Execute the emitted column guard over synthetic catalog values on local
// PostgreSQL. No schema, migration history, or production data is changed.
func TestMigrationGeonameColumnExecutesExactGuard(t *testing.T) {
	if os.Getenv("WARP_ENV") != "local" {
		t.Fatal("GeoNames schema fixtures require the attested local test environment")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	head := server.MigrationCount()
	server.Db(ctx, func(conn server.PgConn) {
		for _, fault := range []string{"healthy", "missing", "wrong schema", "wrong table", "wrong column", "wrong type", "not nullable", "default"} {
			schema, table, column := "public", "location", "geoname_id"
			kind, nullable, present := "bigint", "YES", true
			var columnDefault *string
			switch fault {
			case "missing":
				present = false
			case "wrong schema":
				schema = "synthetic_other_schema"
			case "wrong table":
				table = "synthetic_other_table"
			case "wrong column":
				column = "synthetic_other_column"
			case "wrong type":
				kind = "integer"
			case "not nullable":
				nullable = "NO"
			case "default":
				value := "0"
				columnDefault = &value
			}
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				if strings.Contains(query, "FROM migration_catalog") {
					return syntheticMigrationCatalogRows(head), nil
				}
				normalized := strings.Join(strings.Fields(query), " ")
				marker := "EXISTS ( SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'location' AND column_name = 'geoname_id'"
				if strings.Count(normalized, marker) != 1 {
					t.Fatal("migration 691 has no unique relation-scoped column guard")
				}
				start := strings.Index(normalized, marker)
				guard, _, ok := strings.Cut(normalized[start:], " ),")
				if !ok {
					t.Fatal("migration 691 column guard has no following artifact boundary")
				}
				guard = strings.ReplaceAll(guard+" )", "information_schema.columns", "observed_column")
				var admitted bool
				err := conn.QueryRow(ctx, `
					WITH observed_column AS (
						SELECT $1::text AS table_schema, $2::text AS table_name,
						       $3::text AS column_name, $4::text AS data_type,
						       $5::text AS is_nullable, $6::text AS column_default
						WHERE $7::boolean
					)
					SELECT `+guard,
					schema, table, column, kind, nullable, columnDefault, present,
				).Scan(&admitted)
				if err != nil {
					return nil, err
				}
				return []Row{syntheticMigrationGeonameRow(head, admitted, true)}, nil
			}}
			alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(source))
			if err != nil {
				t.Fatalf("%s: actual column guard failed: %v", fault, err)
			}
			if fault == "healthy" {
				if len(alerts) != 0 {
					t.Fatalf("canonical nullable no-default bigint was rejected: %+v", alerts)
				}
			} else {
				alert := requireAlertClass(t, alerts, "migration-schema-drift")
				if !strings.Contains(alert.Markdown(), "location.geoname_id@v691") {
					t.Fatalf("%s lost the column's owning artifact", fault)
				}
			}
		}
	}, server.OptNoRetry())
}
