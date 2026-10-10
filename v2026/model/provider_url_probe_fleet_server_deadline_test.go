package model

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urnetwork/server/v2026"
)

type testingCensusBackendState struct {
	present     bool
	active      bool
	hasSnapshot bool
	inSleep     bool
}

// Each observation is its own transaction. PID plus backend_start selects only
// this fixture's backend generation, never a recycled PID or another worker.
func testingCensusBackend(ctx context.Context, observer server.PgConn, pid uint32, started time.Time) testingCensusBackendState {
	var state testingCensusBackendState
	err := observer.QueryRow(ctx, `SELECT state='active',backend_xmin IS NOT NULL,
		COALESCE(wait_event='PgSleep',false) FROM pg_stat_activity
		WHERE pid=$1 AND backend_start=$2`, pid, started).Scan(&state.active, &state.hasSnapshot, &state.inSleep)
	if errors.Is(err, pgx.ErrNoRows) {
		return state
	}
	server.Raise(err)
	state.present = true
	return state
}

func testingWaitCensusBackend(ctx context.Context, observer server.PgConn, pid uint32, started time.Time, until time.Time, ready func(testingCensusBackendState) bool) (testingCensusBackendState, bool) {
	for {
		state := testingCensusBackend(ctx, observer, pid, started)
		if ready(state) {
			return state, true
		}
		if !time.Now().Before(until) {
			return state, false
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			server.Raise(ctx.Err())
		case <-timer.C:
		}
	}
}

// The exact production census wrapper must retire the server statement and
// snapshot within its configured owner budget even when the client's separate
// cancellation connection cannot proceed. A client-side return is not the
// oracle. The early-cancel case forces pgx's async cleanup before the deadline;
// the deadline case also admits a server 57014 response that wins that race.
func TestProviderUrlProbeFleetServerDeadlineReleasesSnapshot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		for _, earlyCancel := range []bool{true, false} {
			name := "deadline"
			if earlyCancel {
				name = "early_cancel"
			}
			func() {
				t.Logf("server lifetime control: %s", name)
				ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
				defer cancel()
				server.Db(ctx, func(observer server.PgConn) {
					config, err := pgxpool.ParseConfig("")
					server.Raise(err)
					config.ConnConfig = observer.Conn().Config().Copy()
					config.MaxConns = 1
					config.MinConns = 0
					config.ConnConfig.RuntimeParams["statement_timeout"] = "0"
					config.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "0"
					config.ConnConfig.RuntimeParams["transaction_timeout"] = "0"
					var holdCancel atomic.Bool
					cancelEntered := make(chan struct{})
					cancelRelease := make(chan struct{})
					var enteredOnce, releaseOnce sync.Once
					release := func() { releaseOnce.Do(func() { close(cancelRelease) }) }
					dial := config.ConnConfig.DialFunc
					config.ConnConfig.DialFunc = func(dialCtx context.Context, network, address string) (net.Conn, error) {
						if !holdCancel.Load() {
							return dial(dialCtx, network, address)
						}
						enteredOnce.Do(func() { close(cancelEntered) })
						select {
						case <-cancelRelease:
							return nil, errors.New("synthetic held census cancellation transport")
						case <-dialCtx.Done():
							return nil, dialCtx.Err()
						}
					}
					pool, err := pgxpool.NewWithConfig(ctx, config)
					server.Raise(err)
					defer pool.Close()
					conn, err := pool.Acquire(ctx)
					server.Raise(err)
					pid := conn.Conn().PgConn().PID()
					var backendStart time.Time
					server.Raise(observer.QueryRow(ctx, "SELECT backend_start FROM pg_stat_activity WHERE pid=$1", pid).Scan(&backendStart))
					readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
					deadline, _ := readCtx.Deadline()
					joined := make(chan struct{})
					result := make(chan any, 1)
					defer func() {
						readCancel()
						release()
						cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
						defer cleanupCancel()
						// Fixture-only cleanup comes after the oracle. It cannot make
						// an observed lingering server statement pass the assertion.
						_, _ = observer.Exec(cleanupCtx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
							WHERE pid=$1 AND backend_start=$2`, pid, backendStart)
						select {
						case <-joined:
						case <-cleanupCtx.Done():
							t.Error("census fixture worker did not join during bounded cleanup")
						}
						_ = conn.Conn().Close(cleanupCtx)
						conn.Release()
					}()
					holdCancel.Store(true)
					go func() {
						defer close(joined)
						result <- server.HandleError(func() {
							providerUrlProbeFleetRead(readCtx, conn, func(tx server.PgTx) {
								rows, err := tx.Query(readCtx, "SELECT pg_sleep(30)")
								server.WithPgResult(rows, err, func() {
									for rows.Next() {
									}
								})
							})
						})
					}()
					_, entered := testingWaitCensusBackend(ctx, observer, pid, backendStart, time.Now().Add(time.Second), func(s testingCensusBackendState) bool {
						return s.present && s.active && s.hasSnapshot && s.inSleep
					})
					if !entered {
						t.Fatal("fixture never observed the owned active query and snapshot")
					}
					if earlyCancel {
						readCancel()
					}
					var failure any
					select {
					case failure = <-result:
					case <-time.After(time.Until(deadline.Add(time.Second))):
						t.Fatal("client census owner did not return within its deadline and join allowance")
					}
					if failure == nil {
						t.Fatal("slow census unexpectedly completed without an error")
					}
					if earlyCancel {
						if !errors.Is(readCtx.Err(), context.Canceled) {
							t.Fatal("early cancellation lost the caller's cause")
						}
						select {
						case <-cancelEntered:
						case <-time.After(time.Second):
							t.Fatal("client did not enter the deliberately held cancellation transport")
						}
					} else if readCtx.Err() == nil {
						err, ok := failure.(error)
						var pgErr *pgconn.PgError
						if !ok || !errors.As(err, &pgErr) || pgErr.Code != "57014" {
							t.Fatal("non-deadline failure did not preserve the server timeout error")
						}
					}
					last, retired := testingWaitCensusBackend(ctx, observer, pid, backendStart, deadline.Add(time.Second), func(s testingCensusBackendState) bool {
						return !s.present || (!s.active && !s.hasSnapshot)
					})
					select {
					case <-cancelRelease:
						t.Fatal("cancellation transport was released before the server lifetime assertion")
					default:
					}
					if !retired {
						t.Errorf("client returned but census server statement/snapshot outlived original deadline plus 1s: active=%t snapshot=%t", last.active, last.hasSnapshot)
					}
				}, server.OptNoRetry())
			}()
		}
	})
}

// A transaction-local cap must not widen an administrator's tighter timeout,
// and neither the cap nor JIT may leak when this same backend is reused.
func TestProviderUrlProbeFleetServerDeadlineScope(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testingUrlCensusJitConnection(t, func(ctx context.Context, conn server.PgConn) {
			var original string
			server.Raise(conn.QueryRow(ctx, "SHOW statement_timeout").Scan(&original))
			defer func() {
				server.RaisePgResult(conn.Exec(ctx, "SELECT set_config('statement_timeout',$1,false)", original))
			}()
			for _, initial := range []string{"0", "200ms", "30s"} {
				server.RaisePgResult(conn.Exec(ctx, "SELECT set_config('statement_timeout',$1,false)", initial))
				readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				providerUrlProbeFleetRead(readCtx, conn, func(tx server.PgTx) {
					var millis int64
					server.Raise(tx.QueryRow(readCtx, "SELECT (EXTRACT(EPOCH FROM current_setting('statement_timeout')::interval)*1000)::bigint").Scan(&millis))
					if millis <= 0 || millis > 2000 || (initial == "200ms" && millis != 200) {
						t.Errorf("census server cap widened or disabled: initial=%s actual_ms=%d", initial, millis)
					}
				})
				cancel()
				var restored string
				server.Raise(conn.QueryRow(ctx, "SHOW statement_timeout").Scan(&restored))
				if restored != initial {
					t.Errorf("census statement timeout leaked on reused backend: got %s want %s", restored, initial)
				}
				testingCheckUrlCensusJitRestored(ctx, conn, "on")
			}
		})
	})
}
