// tls-3's roll-up chain on a private database (TLS-M5-12): scheduled only in a
// binary that links MG5, and each run rolling up and dropping the hours past
// 24 h, keeping a held one, and sweeping the days past 13 months.
package work

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// How many runs of the chain wait in the queue.
func tlsConnectionRollupPendingCount(ctx context.Context) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT count(*) FROM pending_task WHERE run_once_key = $1`,
			task.RunOnce("tls_connection_rollup").String(),
		).Scan(&count))
	})
	return
}

// The chain is queued once, however often the schedule runs, and not at all in
// a binary that does not link MG5.
func TestTlsConnectionRollupScheduledOnlyWithMg5(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		for range 2 {
			server.Tx(ctx, func(tx server.PgTx) {
				ScheduleTlsConnectionRollup(clientSession, tx)
			})
		}
		want := 0
		if server.SidecarMg5Linked() {
			want = 1
		}
		if count := tlsConnectionRollupPendingCount(ctx); count != want {
			t.Fatalf("%d runs queued, want %d", count, want)
		}
		// a run without the group touches nothing and asks for no rerun
		if !server.SidecarMg5Linked() {
			result, err := TlsConnectionRollup(&TlsConnectionRollupArgs{}, clientSession)
			if err != nil || len(result.RolledUp) != 0 || result.More {
				t.Fatalf("a run without MG5: %+v, %v", result, err)
			}
		}
	})
}

// A record of network ending at endTime, from a synthetic visitor hash.
func tlsConnectionRollupTestRecord(networkId server.Id, host string, endTime time.Time) *model.TlsConnectionRecord {
	return &model.TlsConnectionRecord{
		ConnectionId: server.NewId(),
		NetworkId:    networkId,
		Host:         host,
		StartTime:    endTime.Add(-time.Minute),
		EndTime:      endTime,
		ByteCount:    1000,
		VisitorHash:  []byte("fedcba9876543210"),
	}
}

// A run drops the hour that ended more than 23 h ago, which a network holds no
// hold on, with its counts landing in the day, keeps the hour a live hold
// covers, and warns of it; the newer hour waits its turn. The day past 13
// months goes.
func TestTlsConnectionRollupRetiresHoursAndSweepsDays(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		now := server.NowUtc()
		oldHour := server.PartitionGranularityHourly.RangeStart(now.Add(-26 * time.Hour))
		heldHour := oldHour.Add(time.Hour)
		newHour := server.PartitionGranularityHourly.RangeStart(now.Add(-2 * time.Hour))
		networkId := server.NewId()
		heldNetworkId := server.NewId()
		write := func(records ...*model.TlsConnectionRecord) {
			result, err := model.Testing_WriteTlsConnectionRecordsAt(ctx, records, records[0].EndTime.Add(time.Minute))
			if err != nil || result.WrittenCount != len(records) {
				t.Fatalf("write %+v, %v", result, err)
			}
		}
		write(tlsConnectionRollupTestRecord(networkId, "@", oldHour.Add(5*time.Minute)), tlsConnectionRollupTestRecord(networkId, "@", oldHour.Add(6*time.Minute)))
		write(tlsConnectionRollupTestRecord(heldNetworkId, "www", heldHour.Add(5*time.Minute)))
		write(tlsConnectionRollupTestRecord(networkId, "@", newHour.Add(5*time.Minute)))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO abuse_legal_hold (network_id, case_id, part, kind, scope, place_time, expire_time)
				VALUES ($1, $2, 0, 'preserve_2703f', '"network"', $3, $4)
				`,
				heldNetworkId,
				server.NewId(),
				heldHour.Add(30*time.Minute),
				now.AddDate(0, 3, 0),
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO tls_connection_day (network_id, host, day, connection_count, byte_count) VALUES ($1, '@', $2, 9, 9)`,
				networkId,
				server.PartitionGranularityDaily.RangeStart(now).AddDate(0, -14, 0),
			))
		})

		result, err := TlsConnectionRollup(&TlsConnectionRollupArgs{}, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		oldLeaf := "tls_connection_record_h" + oldHour.Format("2006010215")
		heldLeaf := "tls_connection_record_h" + heldHour.Format("2006010215")
		if !slices.Contains(result.RolledUp, oldLeaf) || slices.Contains(result.RolledUp, heldLeaf) {
			t.Fatalf("rolled up %v", result.RolledUp)
		}
		if _, ok := result.Kept[heldLeaf]; !ok {
			t.Fatalf("the held hour is not reported kept: %v", result.Kept)
		}
		if result.RemovedDayCount != 1 {
			t.Fatalf("removed %d days past 13 months", result.RemovedDayCount)
		}
		days := model.GetTlsConnectionDays(ctx, networkId, server.PartitionGranularityDaily.RangeStart(oldHour))
		if len(days) != 1 || days[0].Host != "@" || days[0].ConnectionCount != 2 || days[0].ByteCount != 2000 {
			t.Fatalf("days %+v", days)
		}
		if days := model.GetTlsConnectionDays(ctx, heldNetworkId, server.PartitionGranularityDaily.RangeStart(oldHour)); len(days) != 0 {
			t.Fatalf("the held hour's counts landed before its drop: %+v", days)
		}
	})
}
