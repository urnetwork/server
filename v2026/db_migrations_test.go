package server

import (
	"context"
	"strings"
	"testing"
	// "github.com/urnetwork/connect/v2026"
)

func locationNameBackfillMigrationIndex(t testing.TB) int {
	t.Helper()
	for i, migration := range migrations {
		if sqlMigration, ok := migration.(*SqlMigration); ok &&
			strings.Contains(sqlMigration.sql, "iso_country_name_backfill") {
			return i
		}
	}
	t.Fatal("location-name backfill migration not found")
	return -1
}

// A pending migration can race ahead of the runtime fix that stopped blank
// location names from being written. In that state a later blank region/city
// can coexist with an older canonical row, and normalizing both full names to
// the same value must not abort the migration.
func TestLocationNameBackfillHandlesCanonicalFullNameCollisions(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		migrationIndex := locationNameBackfillMigrationIndex(t)
		ApplyDbMigrationsUpTo(ctx, migrationIndex)

		countryId := NewId()
		canonicalRegionId := NewId()
		canonicalCityId := NewId()
		legacyRegionId := NewId()
		legacyDuplicateCityId := NewId()
		legacyUniqueCityId := NewId()

		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `
				INSERT INTO location (
					location_id,
					location_type,
					location_name,
					city_location_id,
					region_location_id,
					country_location_id,
					country_code,
					location_full_name
				)
				VALUES
					($1, 'country', '', NULL, NULL, $1, 'sg', 'sg'),
					($2, 'region', 'Singapore', NULL, $2, $1, 'sg', 'Singapore, sg'),
					($3, 'city', 'Bedok', $3, $2, $1, 'sg', 'Bedok, Singapore, sg'),
					($4, 'region', '', NULL, $4, $1, 'sg', ', sg'),
					($5, 'city', 'Bedok', $5, $4, $1, 'sg', 'Bedok, , sg'),
					($6, 'city', 'Jurong', $6, $4, $1, 'sg', 'Jurong, , sg')
			`,
				countryId,
				canonicalRegionId,
				canonicalCityId,
				legacyRegionId,
				legacyDuplicateCityId,
				legacyUniqueCityId,
			))
		})

		ApplyDbMigrationsUpTo(ctx, migrationIndex+1)

		type locationState struct {
			name     string
			fullName string
		}
		states := map[Id]locationState{}
		blankNameCount := 0
		constraintExists := false
		MaintenanceDb(ctx, func(conn PgConn) {
			result, err := conn.Query(ctx, `
				SELECT location_id, location_name, location_full_name
				FROM location
				WHERE location_id IN ($1, $2, $3, $4, $5, $6)
			`,
				countryId,
				canonicalRegionId,
				canonicalCityId,
				legacyRegionId,
				legacyDuplicateCityId,
				legacyUniqueCityId,
			)
			WithPgResult(result, err, func() {
				for result.Next() {
					var locationId Id
					var state locationState
					Raise(result.Scan(&locationId, &state.name, &state.fullName))
					states[locationId] = state
				}
			})

			result, err = conn.Query(ctx, `SELECT COUNT(*) FROM location WHERE location_name = ''`)
			WithPgResult(result, err, func() {
				if result.Next() {
					Raise(result.Scan(&blankNameCount))
				}
			})

			result, err = conn.Query(ctx, `
				SELECT EXISTS (
					SELECT 1
					FROM pg_constraint
					WHERE
						conrelid = 'location'::regclass AND
						conname = 'location_name_not_blank'
				)
			`)
			WithPgResult(result, err, func() {
				if result.Next() {
					Raise(result.Scan(&constraintExists))
				}
			})
		}, OptReadOnly())

		assertState := func(locationId Id, wantName string, wantFullName string) {
			t.Helper()
			got, ok := states[locationId]
			if !ok {
				t.Fatalf("location %s was removed", locationId)
			}
			if got.name != wantName || got.fullName != wantFullName {
				t.Fatalf(
					"location %s = (%q, %q), want (%q, %q)",
					locationId,
					got.name,
					got.fullName,
					wantName,
					wantFullName,
				)
			}
		}

		assertState(countryId, "Singapore", "sg")
		assertState(canonicalRegionId, "Singapore", "Singapore, sg")
		assertState(canonicalCityId, "Bedok", "Bedok, Singapore, sg")
		assertState(legacyRegionId, "Singapore", ", sg")
		assertState(legacyDuplicateCityId, "Bedok", "Bedok, , sg")
		assertState(legacyUniqueCityId, "Jurong", "Jurong, Singapore, sg")

		if blankNameCount != 0 {
			t.Fatalf("blank location names after migration = %d, want 0", blankNameCount)
		}
		if !constraintExists {
			t.Fatal("location_name_not_blank constraint was not added")
		}
		if version := DbVersion(ctx); version != migrationIndex+1 {
			t.Fatalf("DB version = %d, want %d", version, migrationIndex+1)
		}
	})
}

func TestApplyDbMigrations(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()

		ApplyDbMigrations(ctx)

		var reloptions []string
		var retiredIndexExists bool
		var idleTimeoutConfigured bool
		MaintenanceDb(ctx, func(conn PgConn) {
			result, err := conn.Query(ctx, `
				SELECT coalesce(reloptions, ARRAY[]::text[])
				FROM pg_class
				WHERE oid = 'pending_task'::regclass
			`)
			WithPgResult(result, err, func() {
				if result.Next() {
					Raise(result.Scan(&reloptions))
				}
			})

			result, err = conn.Query(ctx, `
				SELECT to_regclass('transfer_contract_open_source_id_companion_contract_id') IS NOT NULL
			`)
			WithPgResult(result, err, func() {
				if result.Next() {
					Raise(result.Scan(&retiredIndexExists))
				}
			})

			result, err = conn.Query(ctx, `
				SELECT EXISTS (
					SELECT 1
					FROM pg_db_role_setting settings
					CROSS JOIN LATERAL unnest(settings.setconfig) config(value)
					WHERE settings.setdatabase = (SELECT oid FROM pg_database WHERE datname = current_database())
					  AND settings.setrole = 0
					  AND config.value = 'idle_in_transaction_session_timeout=5min'
				)
			`)
			WithPgResult(result, err, func() {
				if result.Next() {
					Raise(result.Scan(&idleTimeoutConfigured))
				}
			})
		}, OptReadWrite())

		options := strings.Join(reloptions, ",")
		for _, want := range []string{
			"autovacuum_vacuum_scale_factor=0",
			"autovacuum_vacuum_threshold=50",
			"autovacuum_vacuum_cost_delay=0",
			"autovacuum_analyze_scale_factor=0",
			"autovacuum_analyze_threshold=50",
		} {
			if !strings.Contains(options, want) {
				t.Fatalf("pending_task reloptions %q missing %q", options, want)
			}
		}
		if retiredIndexExists {
			t.Fatal("retired 99GB companion index still exists after head migrations")
		}
		if !idleTimeoutConfigured {
			t.Fatal("database idle-in-transaction timeout was not configured")
		}
	})
}
