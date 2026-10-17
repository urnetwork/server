// Real disposable fixtures pin missing-cache reads and the singleton maintenance boundary.
package work

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Native admission deliberately leaves both legacy metadata tables empty.
func TestPrivateLoadAccountingNativeWithoutLegacyRows(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := newPrivateLoadFixtureCount(t, ctx, 0, 0, 1)
		defer f.Close()
		report := privateLoadCreateWave(ctx, t, f)
		if report.Completed != 1 || report.Failed != 0 {
			t.Fatal("native accounting preparation failed")
		}
		var snapshots, revisions int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT
    (SELECT count(*) FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1),
    (SELECT count(*) FROM transfer_balance_net_escrow_revision WHERE balance_id=$1)`, f.owner.BalanceId).Scan(&snapshots, &revisions))
		})
		if snapshots != 0 || revisions != 0 {
			t.Fatal("native control unexpectedly created legacy metadata")
		}
		// Both original callers must use the same nullable legacy metadata reader.
		privateLoadObserveAccounting(t, ctx, f)
		privateLoadAssertAccounting(t, ctx, f, model.ByteCount(report.Contracts[0].TransferByteCount))
		privateLoadAssertNativeAdmission(t, ctx, f, report.Counters, 1)
	})
}

// Sample acknowledgement is requested only after the actual singleton lease holds its row.
func TestPrivateLoadObserverLeavesSingletonMaintenanceSlot(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := newPrivateLoadFixtureCount(t, ctx, 0, 0, 1)
		defer f.Close()
		pop := server.Config.PushSimpleResource("db.yml", []byte("min_connections: 0\nmax_connections: 1\n"))
		popMaintenance := server.Config.PushSimpleResource("db_maintenance.yml", []byte("min_connections: 0\nmax_connections: 1\n"))
		server.PgReset()
		defer func() { popMaintenance(); pop(); server.PgReset() }()
		requests := make(chan chan struct{}, 1)
		observe := privateLoadStartObserver(ctx, t, requests)
		defer observe()
		maximum := privateLoadMetric(t, "urnetwork_pg_pool_connections", map[string]string{"pool": "maintenance", "state": "maximum"})
		acquired := privateLoadMetric(t, "urnetwork_pg_pool_connections", map[string]string{"pool": "maintenance", "state": "acquired"})
		if maximum != 1 || acquired != 0 {
			t.Fatalf("observer retained singleton maintenance slot: maximum=%g acquired=%g", maximum, acquired)
		}
		err := privateLoadWithBarrier(ctx, privateLoadAcquireBarrier, `SELECT 1 FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, f.owner.BalanceId, func() error {
			sampled := make(chan struct{})
			requests <- sampled
			select {
			case <-sampled:
			case <-ctx.Done():
				return ctx.Err()
			}
			if got := privateLoadMetric(t, "urnetwork_pg_pool_connections", map[string]string{"pool": "maintenance", "state": "acquired"}); got != 1 {
				t.Errorf("row barrier lost its sole lease: acquired=%g", got)
			}
			if got := privateLoadMetric(t, "urnetwork_pg_pool_connections", map[string]string{"pool": "maintenance", "state": "maximum"}); got != 1 {
				t.Errorf("observer enlarged maintenance pool: maximum=%g", got)
			}
			values := observe()
			if values["samples"] < 1 || values["sampling_error"] != 0 || values["cleanup_error"] != 0 {
				t.Error("observer did not complete healthy sampling under row barrier")
			}
			return nil
		})
		if err != nil {
			t.Fatal("singleton observer/barrier failed", err)
		}
	})
}

// Captured handles exist only for red-test rescue; the real scope still owns normal cleanup.
type privateLoadCapturedBarrier struct {
	server.PgConn
	held server.PgTx
}

// Capture the successfully created transaction without changing Begin's ownership semantics.
func (self *privateLoadCapturedBarrier) Begin(ctx context.Context) (server.PgTx, error) {
	tx, err := self.PgConn.Begin(ctx)
	if err == nil {
		self.held = tx
	}
	return tx, err
}

// Fatal/Goexit cannot reach a real PgReset while a callback-owned lease remains acquired.
func TestPrivateLoadBarrierGoexitBeforeRealPoolReset(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		f := newPrivateLoadFixtureCount(t, ctx, 0, 0, 1)
		defer f.Close()
		pop := server.Config.PushSimpleResource("db.yml", []byte("min_connections: 0\nmax_connections: 1\n"))
		popMaintenance := server.Config.PushSimpleResource("db_maintenance.yml", []byte("min_connections: 0\nmax_connections: 1\n"))
		server.PgReset()
		defer func() { popMaintenance(); pop(); server.PgReset() }()
		for _, stage := range []string{"acquired", "transaction", "locked", "callback"} {
			func() {
				finished := make(chan struct{})
				go func() {
					defer close(finished)
					var captured *privateLoadCapturedBarrier
					observerJoined := false
					defer func() {
						if got := privateLoadMetric(t, "urnetwork_pg_pool_connections", map[string]string{"pool": "maintenance", "state": "acquired"}); got != 0 {
							t.Errorf("stage=%s reached real pool reset with acquired=%g", stage, got)
						}
						if !observerJoined {
							t.Errorf("stage=%s reached real pool reset before observer joined", stage)
						}
						// Rescue only the exact handles of this intentionally reversible control.
						if captured != nil {
							if captured.held != nil {
								cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), server.PgRollbackTimeout)
								_ = captured.held.Rollback(cleanupCtx)
								cleanupCancel()
							}
							captured.Release()
						}
						server.PgReset()
					}()
					observe := privateLoadStartObserver(ctx, t)
					defer func() { observe(); observerJoined = true }()
					if acquired := privateLoadMetric(t, "urnetwork_pg_pool_connections", map[string]string{"pool": "maintenance", "state": "acquired"}); acquired != 0 {
						t.Error("observer consumed the slot before unwind control")
						return
					}
					_ = privateLoadWithBarrier(ctx, func(ctx context.Context) (privateLoadBarrierConn, error) {
						conn, err := server.AcquireMaintenanceDbConn(ctx)
						if err != nil {
							return nil, err
						}
						captured = &privateLoadCapturedBarrier{PgConn: conn}
						return captured, nil
					}, `SELECT 1 FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, f.owner.BalanceId, func() error { runtime.Goexit(); return nil }, func(at string) {
						if at == stage {
							runtime.Goexit()
						}
					})
				}()
				select {
				case <-finished:
				case <-ctx.Done():
					t.Fatal("real pool unwind owner did not join")
				}
			}()
		}
	})
}
