package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Migration 857: the evidence guard admits a delete only from a transaction
// that opts in, and only of a row older than the 13-week horizon. Updates and
// truncates stay refused with or without the opt-in.
func TestContractCloseEvidenceRetentionGuard(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()

		insert := func(acceptedAt time.Time) (clientId Id, reportId Id) {
			clientId = NewId()
			reportId = NewId()
			Tx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO contract_close_report_evidence (
						client_id, report_id, contract_id, party,
						acked_byte_count, unacked_byte_count, checkpoint, accepted_at
					) VALUES ($1, $2, $3, 'source', 1, 0, false, $4)
					`,
					clientId,
					reportId,
					NewId(),
					acceptedAt,
				))
			})
			return
		}
		exists := func(clientId Id, reportId Id) (found bool) {
			Db(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(
					ctx,
					`SELECT EXISTS (SELECT 1 FROM contract_close_report_evidence WHERE client_id = $1 AND report_id = $2)`,
					clientId,
					reportId,
				).Scan(&found))
			})
			return
		}
		// runs sql in its own transaction, optionally opted in to retention
		exec := func(optIn bool, sql string, args ...any) (err error) {
			conn, acquireErr := AcquireMaintenanceDbConn(ctx)
			Raise(acquireErr)
			defer conn.Release()
			tx, beginErr := conn.Begin(ctx)
			Raise(beginErr)
			defer tx.Rollback(ctx)
			if optIn {
				_, err = tx.Exec(ctx, `SET LOCAL urnetwork.close_evidence_retention = 'on'`)
				Raise(err)
			}
			if _, err = tx.Exec(ctx, sql, args...); err != nil {
				return
			}
			return tx.Commit(ctx)
		}
		immutable := func(err error) bool {
			return err != nil && strings.Contains(err.Error(), "original contract close reports are immutable")
		}

		expiredClientId, expiredReportId := insert(now.Add(-92 * 24 * time.Hour))
		retainedClientId, retainedReportId := insert(now.Add(-90 * 24 * time.Hour))
		deleteSql := `DELETE FROM contract_close_report_evidence WHERE client_id = $1 AND report_id = $2`

		// without the opt-in nothing can be deleted, however old
		connect.AssertEqual(t, immutable(exec(false, deleteSql, expiredClientId, expiredReportId)), true)
		connect.AssertEqual(t, exists(expiredClientId, expiredReportId), true)

		// with it, a row inside the horizon is still refused
		connect.AssertEqual(t, immutable(exec(true, deleteSql, retainedClientId, retainedReportId)), true)
		connect.AssertEqual(t, exists(retainedClientId, retainedReportId), true)

		// updates and truncates stay refused with the opt-in
		connect.AssertEqual(t, immutable(exec(true, `UPDATE contract_close_report_evidence SET checkpoint = true WHERE client_id = $1 AND report_id = $2`, expiredClientId, expiredReportId)), true)
		connect.AssertEqual(t, immutable(exec(true, `TRUNCATE contract_close_report_evidence`)), true)

		// a row older than the horizon is deleted
		connect.AssertEqual(t, exec(true, deleteSql, expiredClientId, expiredReportId), nil)
		connect.AssertEqual(t, exists(expiredClientId, expiredReportId), false)
		connect.AssertEqual(t, exists(retainedClientId, retainedReportId), true)
	})
}
