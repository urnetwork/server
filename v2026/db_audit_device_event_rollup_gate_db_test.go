// The operator prepare's device day rollup gate against PostgreSQL with the
// migrated schema (db_audit_device_event_partition.go,
// auditDeviceEventRollupLacking).
package server

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The gate lists every day of the export's window without a complete row, an
// incomplete row and a missing row alike, while the rest of the window is
// complete, and the operator's prepare refuses on them without changing the
// table. Until 2026-11-17 the gate's NOT EXISTS named the window's day by the
// same name as audit_device_day.day, so it compared the rollup's column with
// itself: one complete row anywhere hid every lacking day (found by the 11-17
// rehearsal of the cutover on a copy of Main's catalog).
func TestAuditDeviceEventRollupGateListsIncompleteAndMissingDays(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testAuditDeviceEventPartitionSettings()
		lacking := func() (days []string) {
			MaintenanceDb(ctx, func(conn PgConn) {
				var err error
				days, err = auditDeviceEventRollupLacking(ctx, conn, settings)
				Raise(err)
			})
			return
		}

		testAuditDeviceEventCompleteRollup(t, ctx)
		if days := lacking(); len(days) != 0 {
			t.Fatalf("a complete rollup lacks %v", days)
		}

		// the days by the database's clock, as the gate reads them
		incomplete := testContractPartitionScalar[string](t, ctx, `SELECT to_char((now() AT TIME ZONE 'UTC')::date - 5, 'YYYY-MM-DD')`)
		missing := testContractPartitionScalar[string](t, ctx, `SELECT to_char((now() AT TIME ZONE 'UTC')::date - 7, 'YYYY-MM-DD')`)
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `UPDATE audit_device_day SET complete = false WHERE day = $1::date`, incomplete))
			RaisePgResult(tx.Exec(ctx, `DELETE FROM audit_device_day WHERE day = $1::date`, missing))
		})
		if days := lacking(); !slices.Equal(days, []string{missing, incomplete}) {
			t.Fatalf("the rollup lacks %v, want [%s %s]", days, missing, incomplete)
		}

		cutoverDay := PartitionGranularityDaily.AddRanges(PartitionGranularityDaily.RangeStart(NowUtc()), 2)
		lines := []string{}
		err := MigrateAuditDeviceEventsToPartitions(ctx, AuditDeviceEventPartitionMigrateOptions{
			CutoverDay: cutoverDay,
			Settings:   settings,
			Logf: func(format string, args ...any) {
				lines = append(lines, fmt.Sprintf(format, args...))
			},
		})
		if err == nil || !slices.ContainsFunc(lines, func(line string) bool {
			return strings.Contains(line, "the device day rollup lacks 2 complete day(s)")
		}) {
			t.Fatalf("the operator's prepare ran with a lacking rollup: %v %q", err, lines)
		}
		if testContractPartitionScalar[bool](t, ctx, `SELECT to_regclass('public.audit_device_event_staged') IS NOT NULL OR NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'audit_device_event_event_id_key')`) {
			t.Fatal("the refused prepare changed the table")
		}
	})
}
