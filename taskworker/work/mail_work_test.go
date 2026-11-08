// §15.15's retention chain on a private database (MAIL-M7-21): scheduled only
// in a binary that links MG7, and each run dropping the hours past 24 h,
// keeping a held one, and removing host keys 30 days after their retirement.
package work

import (
	"context"
	"crypto/sha256"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// How many runs of the chain wait in the queue.
func mailRetentionPendingCount(ctx context.Context) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT count(*) FROM pending_task WHERE run_once_key = $1`,
			task.RunOnce("mail_retention").String(),
		).Scan(&count))
	})
	return
}

// The chain is queued once, however often the schedule runs, and not at all in
// a binary that does not link MG7.
func TestMailRetentionScheduledOnlyWithMg7(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		for range 2 {
			server.Tx(ctx, func(tx server.PgTx) {
				ScheduleMailRetention(clientSession, tx)
			})
		}
		want := 0
		if server.SidecarMg7Linked() {
			want = 1
		}
		if count := mailRetentionPendingCount(ctx); count != want {
			t.Fatalf("%d runs queued, want %d", count, want)
		}
		if !server.SidecarMg7Linked() {
			result, err := MailRetention(&MailRetentionArgs{}, clientSession)
			if err != nil || len(result.Dropped) != 0 || result.More {
				t.Fatalf("a run without MG7: %+v, %v", result, err)
			}
		}
	})
}

// A run drops the hour past 24 h, keeps the hour a live hold covers and says
// so, and removes a host key retired more than 30 days ago.
func TestMailRetentionDropsHoursAndOldKeys(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg7Linked() {
			return
		}
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		now := server.NowUtc()
		oldHour := server.PartitionGranularityHourly.RangeStart(now.Add(-26 * time.Hour))
		heldHour := oldHour.Add(time.Hour)
		networkId := server.NewId()
		heldNetworkId := server.NewId()
		write := func(record *model.MailConnectionRecord) {
			result, err := model.Testing_WriteMailConnectionRecordsAt(ctx, []*model.MailConnectionRecord{record}, record.StartTime.Add(time.Minute))
			if err != nil || result.WrittenCount != 1 {
				t.Fatalf("write %+v, %v", result, err)
			}
		}
		write(&model.MailConnectionRecord{NetworkId: networkId, StartTime: oldHour.Add(5 * time.Minute), ByteCount: 10, Result: "spliced"})
		write(&model.MailConnectionRecord{NetworkId: heldNetworkId, StartTime: heldHour.Add(5 * time.Minute), ByteCount: 10, Result: "spliced"})
		spkiSha256 := sha256.Sum256([]byte("synthetic key"))
		server.Tx(ctx, func(tx server.PgTx) {
			_, err := model.UpsertMailHostKeyInTx(ctx, tx, &model.MailHostKey{
				NetworkId:         networkId,
				Host:              "mx",
				ReceiverSidecarId: server.NewId(),
				SpkiSha256:        spkiSha256[:],
			})
			server.Raise(err)
			server.RaisePgResult(tx.Exec(ctx, `UPDATE mail_host_key SET state = 'retired', retire_time = $2 WHERE network_id = $1`, networkId, now.AddDate(0, 0, -31)))
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
		})

		result, err := MailRetention(&MailRetentionArgs{}, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		oldLeaf := "mail_connection_record_h" + oldHour.Format("2006010215")
		heldLeaf := "mail_connection_record_h" + heldHour.Format("2006010215")
		if !slices.Contains(result.Dropped, oldLeaf) || slices.Contains(result.Dropped, heldLeaf) {
			t.Fatalf("dropped %v", result.Dropped)
		}
		if _, ok := result.Kept[heldLeaf]; !ok {
			t.Fatalf("the held hour is not reported kept: %v", result.Kept)
		}
		if result.RemovedHostKeyCount != 1 || len(model.ListMailHostKeys(ctx, networkId)) != 0 {
			t.Fatalf("removed %d host keys", result.RemovedHostKeyCount)
		}
	})
}
