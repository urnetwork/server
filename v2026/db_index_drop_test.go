package server

import (
	"context"
	"testing"
)

// The index-drop migrations remove hot-table indexes that no current reader
// uses. After every migration they must be absent, and the maintenance rotation
// must not keep rebuilding a dropped name: every priority index has to exist at
// head.
func TestDroppedHotTableIndexesStayOutOfSchemaAndMaintenance(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		dropped := []string{
			"transfer_escrow_sweep_destination_id_sweep_time",
			"transfer_contract_pair_open_create_time",
			"transfer_contract_open_destination_partial",
			"pending_task_has_reschedule_error",
			"provider_work_session_event_transaction",
			"contract_close_report_evidence_contract",
			"provider_probe_cycle_completed_waiting",
			"provider_probe_cycle_slot_completed_waiting",
			"audit_device_event_stats_device_id",
		}
		exists := func(name string) bool {
			var present bool
			MaintenanceDb(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(ctx, `SELECT to_regclass('public.'||$1) IS NOT NULL`, name).Scan(&present))
			}, OptReadOnly())
			return present
		}
		for _, name := range dropped {
			if exists(name) {
				t.Errorf("dropped index %s exists after all migrations", name)
			}
		}
		for _, name := range priorityReindexIndexes {
			if !exists(name) {
				t.Errorf("priority reindex index %s does not exist after all migrations", name)
			}
		}
	})
}
