// Exercises optional function lookup against actual published PostgreSQL
// schemas, including functions absent before publication or after schema drift.
package server_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Future function references must not abort historical-head inspection, and
// missing published functions must remain observable as exact schema drift.
func TestPublishedMigrationMonitorCompetitionFunctionArtifacts(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		check := func(tx server.PgTx, version int, missing []int) {
			artifacts := checkPublishedMigrationMonitor(t, ctx, tx, version, missing)
			if len(artifacts) <= 678-589 {
				t.Fatalf("migration artifact row has %d columns, want columns through version 678", len(artifacts))
			}
			for _, artifact := range []struct {
				version int
				present bool
			}{
				{version: 676, present: version >= 676},
				{version: 677, present: version == 677},
				{version: 678, present: version >= 678},
			} {
				for _, absent := range missing {
					if artifact.version == absent {
						artifact.present = false
					}
				}
				if got := artifacts[artifact.version-589]; got != fmt.Sprint(artifact.present) {
					t.Fatalf("version %d artifact v%d presence = %q, want %t", version, artifact.version, got, artifact.present)
				}
			}
		}

		for _, version := range []int{588, 589, 592, 593, 661, 675, 676, 677, 678, server.MigrationCount()} {
			server.ApplyDbMigrationsUpTo(ctx, version)
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				check(tx, version, nil)
			}, server.OptReadOnly(), server.OptNoRetry())
			if version < 676 {
				continue
			}

			baselineMissing := []int{676}
			var reviewMissing []int
			if version == 677 {
				reviewMissing = []int{677}
			} else if version >= 678 {
				baselineMissing = append(baselineMissing, 678)
				reviewMissing = []int{678}
			}
			for _, fault := range []struct {
				name    string
				sql     string
				missing []int
			}{
				{
					name:    "baseline function missing",
					sql:     `DROP FUNCTION public.competition_round_baseline_insert_guard() CASCADE`,
					missing: baselineMissing,
				},
				{
					name:    "review function missing",
					sql:     `DROP FUNCTION public.competition_candidate_review_insert_guard() CASCADE`,
					missing: reviewMissing,
				},
				{
					// Renaming preserves the trigger's function oid, forcing the
					// expected-name lookup to handle absence independently.
					name:    "append-only function renamed",
					sql:     `ALTER FUNCTION public.competition_append_only_guard() RENAME TO synthetic_missing_append_only_guard`,
					missing: []int{676},
				},
			} {
				t.Logf("version %d schema fault: %s", version, fault.name)
				server.MaintenanceDb(ctx, func(conn server.PgConn) {
					tx, err := conn.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback(ctx)
					if _, err := tx.Exec(ctx, fault.sql); err != nil {
						t.Fatalf("apply %s: %v", fault.name, err)
					}
					check(tx, version, fault.missing)
				}, server.OptReadWrite(), server.OptNoRetry())
			}
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				check(tx, version, nil)
			}, server.OptReadOnly(), server.OptNoRetry())
		}
	})
}
