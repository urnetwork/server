package model

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// A real PostgreSQL blocking edge identifies the failed statement before its
// finite timeout. The same SQLSTATE must retain two distinct static phases.
func TestLegacyProviderTotalsFailurePhaseSeparatesHeldRows(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, phase := range []string{"pending_read", "account_write"} {
			name := "single/" + phase
			if batch {
				name = "batch/" + phase
			}
			t.Run(name, func(t *testing.T) {
				providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
					networkId := server.NewId()
					ids := []server.Id{providerTotalsTestTask(ctx, server.NewId(), networkId)}
					if batch {
						ids = append(ids, providerTotalsTestTask(ctx, server.NewId(), networkId))
					}
					server.Tx(ctx, func(tx server.PgTx) {
						server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_balance(network_id) VALUES($1)`, networkId))
					})
					holder := acquireContractLifecycleTestConnection(t, ctx)
					defer holder.Release()
					held, err := holder.Begin(ctx)
					server.Raise(err)
					var releaseOnce sync.Once
					release := func() { releaseOnce.Do(func() { _ = held.Rollback(context.Background()) }) }
					defer release()
					var holderPid int
					server.Raise(held.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPid))
					queryFragment := "%FROM pending_task%"
					if phase == "pending_read" {
						server.RaisePgResult(held.Exec(ctx, `SELECT task_id FROM pending_task WHERE task_id=$1 FOR UPDATE`, ids[0]))
					} else {
						queryFragment = "%INSERT INTO account_balance%"
						server.RaisePgResult(held.Exec(ctx, `SELECT network_id FROM account_balance WHERE network_id=$1 FOR UPDATE`, networkId))
					}
					apply := func(tx server.PgTx) error {
						if batch {
							return applyLegacyProviderTotalsBatchInTx(ctx, tx, ids, networkId)
						}
						return applyLegacyProviderTotalsInTx(ctx, tx, ids[0])
					}
					pidReady := make(chan int, 1)
					done := make(chan error, 1)
					go func() {
						var failure error
						server.HandleError(func() {
							server.Tx(ctx, func(tx server.PgTx) {
								// Only this control lengthens the wait for observing
								// the edge. Production retains its 250ms limit.
								server.RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout='2s'; SET LOCAL statement_timeout='5s'`))
								var pid int
								server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
								pidReady <- pid
								server.Raise(apply(tx))
							}, server.TxReadCommitted, server.OptNoRetry())
						}, func(err error) { failure = err })
						done <- failure
					}()
					joined := false
					defer func() {
						release()
						if !joined {
							<-done
						}
					}()
					var waiterPid int
					select {
					case waiterPid = <-pidReady:
					case <-ctx.Done():
						t.Fatal("provider phase owner did not start", ctx.Err())
					}
					tick := time.NewTicker(5 * time.Millisecond)
					defer tick.Stop()
					for {
						var blocked bool
						server.Db(ctx, func(conn server.PgConn) {
							server.Raise(conn.QueryRow(ctx, `SELECT $2=ANY(pg_blocking_pids($1)) AND EXISTS(
                        SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock' AND query LIKE $3)`,
								waiterPid, holderPid, queryFragment).Scan(&blocked))
						})
						if blocked {
							break
						}
						select {
						case err := <-done:
							joined = true
							t.Fatal("provider phase owner ended before its actual blocking edge", err)
						case <-ctx.Done():
							t.Fatal("provider phase blocking edge was not observed", ctx.Err())
						case <-tick.C:
						}
					}
					// A held owner cannot consume an unrelated provider's credit.
					otherNetwork := server.NewId()
					otherId := providerTotalsTestTask(ctx, server.NewId(), otherNetwork)
					server.Tx(ctx, func(tx server.PgTx) {
						server.Raise(applyLegacyProviderTotalsInTx(ctx, tx, otherId))
					}, server.TxReadCommitted, server.OptNoRetry())
					requireProviderTotalsTestState(t, ctx, otherId, otherNetwork, true, 17, 29)
					var failure error
					select {
					case failure = <-done:
						joined = true
					case <-ctx.Done():
						t.Fatal("provider phase owner did not return its finite lock timeout", ctx.Err())
					}
					var pgErr *pgconn.PgError
					if !errors.As(failure, &pgErr) || pgErr.Code != "55P03" || !errors.Is(failure, pgErr) {
						t.Fatal("held provider phase lost its exact typed PostgreSQL error")
					}
					for _, id := range ids {
						requireProviderTotalsTestState(t, ctx, id, networkId, false, 0, 0)
					}
					release()
					for range 2 {
						server.Tx(ctx, func(tx server.PgTx) { server.Raise(apply(tx)) }, server.TxReadCommitted, server.OptNoRetry())
					}
					for _, id := range ids {
						requireProviderTotalsTestState(t, ctx, id, networkId, true, int64(17*len(ids)), int64(29*len(ids)))
					}
					// The uninstrumented owner passes all custody/replay checks
					// but cannot name the actual failed statement's phase.
					if !strings.Contains(failure.Error(), "phase="+phase) {
						t.Fatalf("held %s statement has no distinct static provider phase", phase)
					}
				})
			})
		}
	}
}
