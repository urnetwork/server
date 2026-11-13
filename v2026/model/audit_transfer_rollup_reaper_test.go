package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A transfer rollup never replaces the row of a day whose contracts the
// contract horizon reaper can have removed: that row was written while the
// day was whole, and a sum of what the reaper left would undercount it. A day
// the reaper cannot reach is rolled again from its contracts.
func TestRollupTransferAuditKeepsDaysTheReaperCanReach(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const day = 24 * time.Hour
		now := server.NowUtc()
		reachable := startOfUtcDay(now.Add(-40 * day))
		whole := startOfUtcDay(now.Add(-20 * day))
		// the rows earlier rolls wrote; neither day has a contract left
		server.Tx(ctx, func(tx server.PgTx) {
			for _, dayStart := range []time.Time{reachable, whole} {
				zeroId := server.Id{}
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_contract_event
					(event_id,event_time,contract_id,client_network_id,client_device_id,provider_network_id,provider_device_id,
					 extender_network_id,extender_id,event_type,event_details,transfer_byte_count,transfer_packets)
					VALUES($1,$2,$3,$3,$3,$3,$3,NULL,NULL,$4,$5,4242,0)`,
					server.NewId(), dayStart.Add(12*time.Hour), zeroId, AuditEventTypeContractClosedSuccess, AuditEventDetailsTransferRollup))
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		rollupRows := func(dayStart time.Time) (count int, byteCount int64) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*), coalesce(sum(transfer_byte_count), 0)::bigint FROM audit_contract_event
					WHERE event_details = $1 AND $2 <= event_time AND event_time < $3`,
					AuditEventDetailsTransferRollup, dayStart, dayStart.Add(day)).Scan(&count, &byteCount))
			})
			return
		}

		RollupTransferAuditEvents(ctx, reachable, whole.Add(day))
		if count, byteCount := rollupRows(reachable); count != 1 || byteCount != 4242 {
			t.Fatalf("a rollup replaced the row of a day 40 days old: %d rows, %d bytes", count, byteCount)
		}
		if count, byteCount := rollupRows(whole); count != 1 || byteCount != 0 {
			t.Fatalf("a rollup did not roll a day 20 days old again: %d rows, %d bytes", count, byteCount)
		}
	})
}
