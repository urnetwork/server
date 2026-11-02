package server

import (
	"context"
	"testing"
)

// The run columns (extender_activation_run_schema.go) are additions the running
// release does not notice. Migrations apply while it still serves, and it names
// the columns it writes, so its insert keeps storing after the migration. A row
// written either side of it reads null in all three, and each column has the
// nullable shape the monitor's contract pins.
func TestExtenderActivationRunMigrationIsAdditive(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		version := ExtenderActivationRunSchemaMigrationVersion()
		if version == 0 {
			t.Fatal("the run columns' migration is not in the migration list")
		}

		// the insert of the release that predates the columns
		insertRunningRelease := func() {
			MaintenanceTx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO network_extender_activation (
						activation_id,
						extender_id,
						activate_time,
						ip_version,
						client_address_hash,
						country_code,
						location_id,
						city_location_id,
						region_location_id,
						country_location_id,
						accuracy_km
					)
					VALUES ($1, $2, $3, 4, NULL, '', NULL, NULL, NULL, NULL, NULL)
					`,
					NewId(),
					NewId(),
					NowUtc(),
				))
			})
		}
		columnCount := func() int {
			var count int
			MaintenanceDb(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(
					ctx,
					`
					SELECT count(*)
					FROM information_schema.columns
					WHERE table_schema = 'public'
						AND table_name = 'network_extender_activation'
						AND column_name IN ('run_id', 'run_start_time', 'previous_exit')
						AND is_nullable = 'YES'
						AND column_default IS NULL
					`,
				).Scan(&count))
			}, OptReadOnly(), OptNoRetry())
			return count
		}

		ApplyDbMigrationsUpTo(ctx, version-1)
		if count := columnCount(); count != 0 {
			t.Fatalf("%d run columns exist before their migration", count)
		}
		insertRunningRelease()

		ApplyDbMigrationsUpTo(ctx, version)
		if count := columnCount(); count != 3 {
			t.Fatalf("%d nullable run columns exist after their migration, want 3", count)
		}
		insertRunningRelease()

		var rowCount int
		var runCount int
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(
				ctx,
				`
				SELECT
					count(*),
					count(*) FILTER (
						WHERE run_id IS NOT NULL
							OR run_start_time IS NOT NULL
							OR previous_exit IS NOT NULL
					)
				FROM network_extender_activation
				`,
			).Scan(&rowCount, &runCount))
		}, OptReadOnly(), OptNoRetry())
		if rowCount != 2 || runCount != 0 {
			t.Fatalf("rows = %d, with a run = %d; want two rows and none with a run", rowCount, runCount)
		}

		// and the head still applies from here
		ApplyDbMigrations(ctx)
	})
}
