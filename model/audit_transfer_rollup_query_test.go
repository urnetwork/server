package model

import (
	_ "embed"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Fixed sparse/dense work fixture, see TestTransferCloseByteChunkWorkIsBounded.
//
//go:embed testdata/audit_transfer_rollup_work.sql
var transferAuditWorkFixtureSQL string

// Exercise the actual daily replacement, including closed/NULL outcomes, late
// reports, canceled outcomes, both day edges and an independent report clock.
func TestRollupTransferAuditDailyReportBoundaries(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		dayEnd := startOfUtcDay(server.NowUtc())
		dayStart := dayEnd.Add(-24 * time.Hour)
		nullOutcomeID, lateReportID := server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			for _, row := range []struct {
				id      server.Id
				closed  any
				outcome any
				report  bool
				bytes   int64
			}{
				{server.NewId(), dayStart.Add(-time.Microsecond), "success", true, 19},
				{nullOutcomeID, dayStart, nil, true, 11},
				{server.NewId(), dayStart.Add(time.Hour), "canceled", true, 13},
				{server.NewId(), dayEnd.Add(-time.Microsecond), "success", true, 17},
				{server.NewId(), dayEnd, "success", true, 23},
				{server.NewId(), nil, nil, true, 29},
				{lateReportID, dayStart.Add(2 * time.Hour), "success", false, 7},
				{server.NewId(), dayStart.Add(3 * time.Hour), "success", true, 0},
			} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
					(contract_id,source_network_id,source_id,destination_network_id,destination_id,
					transfer_byte_count,create_time,close_time,outcome)
					VALUES($1,$2,$3,$4,$5,1000,$6,$7,$8)`, row.id,
					server.NewId(), server.NewId(), server.NewId(), server.NewId(),
					dayStart.Add(-24*time.Hour), row.closed, row.outcome))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
					(contract_id,close_time,party,used_transfer_byte_count)
					VALUES($1,$2,'source',999)`, row.id, dayStart.Add(-7*24*time.Hour)))
				if row.report {
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
						(contract_id,close_time,party,used_transfer_byte_count)
						VALUES($1,$2,'destination',$3)`, row.id, dayStart.Add(-7*24*time.Hour), row.bytes))
				}
			}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
				(contract_id,close_time,party,used_transfer_byte_count)
				VALUES($1,$2,'destination',77)`, server.NewId(), dayStart))
		})
		assertDays := func(wantYesterday int64) {
			if days := RollupTransferAuditEvents(ctx, dayStart.Add(-24*time.Hour), dayEnd); days != 2 {
				t.Fatalf("rollup days=%d, want 2", days)
			}
			server.Db(ctx, func(conn server.PgConn) {
				amounts := map[string]int64{}
				rows, err := conn.Query(ctx, `SELECT event_time,transfer_byte_count,transfer_packets
					FROM audit_contract_event WHERE event_details=$1`, AuditEventDetailsTransferRollup)
				server.WithPgResult(rows, err, func() {
					for rows.Next() {
						var at time.Time
						var amount, packets int64
						server.Raise(rows.Scan(&at, &amount, &packets))
						key := at.UTC().Format("2006-01-02")
						if _, exists := amounts[key]; exists || packets != 0 || !at.Equal(startOfUtcDay(at).Add(12*time.Hour)) {
							t.Fatal("daily replacement duplicated a day or changed its noon/packet contract")
						}
						amounts[key] = amount
					}
				})
				if len(amounts) != 2 || amounts[dayStart.Add(-24*time.Hour).Format("2006-01-02")] != 19 || amounts[dayStart.Format("2006-01-02")] != wantYesterday {
					t.Fatalf("daily report totals changed: %v", amounts)
				}
			})
		}
		assertDays(41)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET used_transfer_byte_count=31
				WHERE contract_id=$1 AND party='destination'`, nullOutcomeID))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
				(contract_id,close_time,party,used_transfer_byte_count)
				VALUES($1,$2,'destination',7)`, lateReportID, dayStart.Add(-7*24*time.Hour)))
		})
		assertDays(68)
		assertDays(68)
	})
}
