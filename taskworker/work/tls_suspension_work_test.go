// §13.9's sweep on a private database (TLS-M5-13): scheduled only in a binary
// that links MG5, and each run lifting the kills past their until.
package work

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

func TestTlsSuspensionSweepScheduledOnlyWithMg5(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		for range 2 {
			server.Tx(ctx, func(tx server.PgTx) {
				ScheduleTlsSuspensionSweep(clientSession, tx)
			})
		}
		want := 0
		if server.SidecarMg5Linked() {
			want = 1
		}
		var count int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT count(*) FROM pending_task WHERE run_once_key = $1`,
				task.RunOnce("tls_suspension_sweep").String(),
			).Scan(&count))
		})
		if count != want {
			t.Fatalf("%d runs queued, want %d", count, want)
		}
		if !server.SidecarMg5Linked() {
			result, err := TlsSuspensionSweep(&TlsSuspensionSweepArgs{}, clientSession)
			if err != nil || result.LiftedCount != 0 || result.More {
				t.Fatalf("a run without MG5: %+v, %v", result, err)
			}
		}
	})
}

func TestTlsSuspensionSweepLiftsWhatUntilPassed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		networkId := sidecar.NewId()
		now := server.NowUtc()
		for host, until := range map[string]time.Time{"past": now.Add(-time.Minute), "ahead": now.Add(time.Hour)} {
			if err := model.SetTlsSuspension(ctx, &sidecar.TlsSuspendArgs{NetworkId: networkId, Host: host, Reason: "abuse:case", Until: until}); err != nil {
				t.Fatal(err)
			}
		}
		result, err := TlsSuspensionSweep(&TlsSuspensionSweepArgs{}, clientSession)
		if err != nil || result.LiftedCount != 1 || result.More {
			t.Fatalf("the sweep: %+v, %v", result, err)
		}
		suspensions, err := model.GetTlsSuspensions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		hosts := []string{}
		for _, suspension := range suspensions {
			if suspension.NetworkId == networkId {
				hosts = append(hosts, suspension.Host)
			}
		}
		if len(hosts) != 1 || hosts[0] != "ahead" {
			t.Fatalf("after the sweep the network holds %v", hosts)
		}
	})
}
