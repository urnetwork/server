// Actual PostgreSQL barriers force final-cut expiry/cancellation and deferred
// rollback, rather than guessing that a callback reached a database statement.
package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/competitionagent"
	"github.com/urnetwork/server/v2026/model"
)

// Error publication precedes done closure; every early failure releases and
// joins its already launched holder without consuming a one-shot error twice.
type competitionDistributedRecoveryPgBarrier struct {
	pid     int32
	release chan struct{}
	done    chan struct{}
	err     error
	once    sync.Once
}

// Only this exact owned disposable transaction is released.
func (self *competitionDistributedRecoveryPgBarrier) unlock() {
	self.once.Do(func() { close(self.release) })
}

// The caller registers its enclosing cleanup before starting any owner. A
// positive native lock acknowledgement, not goroutine dispatch, admits the test.
func competitionDistributedRecoveryStartBarrier(t testing.TB, ctx context.Context, hold func(server.PgTx)) *competitionDistributedRecoveryPgBarrier {
	t.Helper()
	barrier := &competitionDistributedRecoveryPgBarrier{release: make(chan struct{}), done: make(chan struct{})}
	held := make(chan int32, 1)
	go func() {
		defer close(barrier.done)
		barrier.err = captureDatabaseError(func() {
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				hold(tx)
				var pid int32
				server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
				held <- pid
				select {
				case <-barrier.release:
				case <-ctx.Done():
					panic(ctx.Err())
				}
			}, server.OptNoRetry())
		})
	}()
	select {
	case barrier.pid = <-held:
		return barrier
	case <-barrier.done:
		t.Fatal("owned recovery barrier failed before lock acknowledgement", barrier.err)
	case <-ctx.Done():
		barrier.unlock()
		<-barrier.done
		t.Fatal("owned recovery barrier failed to acquire its exact lock")
	}
	return barrier
}

// Polling only rate-limits a positive PostgreSQL lock witness. No empty query,
// queue count or short negative timeout is accepted as proof of arrival.
func competitionDistributedRecoveryWaitForBlockedBackend(t testing.TB, ctx context.Context, holder int32, ownerDone <-chan struct{}) {
	t.Helper()
	for {
		var waiting int32
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COALESCE((SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND $1::integer=ANY(pg_blocking_pids(pid)) ORDER BY pid LIMIT 1),0)`, holder).Scan(&waiting))
		})
		if waiting != 0 {
			return
		}
		select {
		case <-ownerDone:
			t.Fatal("grant returned before its exact database barrier")
		case <-ctx.Done():
			t.Fatal("grant did not reach its positively owned database barrier")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Each mode reaches a distinct real persistence boundary. The database-only
// test trigger is installed only after confirming the generated test database.
func testCompetitionDistributedRecoveryPgBarrier(t *testing.T, phase string, cancelAtBarrier bool) {
	t.Helper()
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		watchdog, stop := context.WithTimeout(tb.Context(), 30*time.Second)
		defer stop()
		fixture := newCompetitionDistributedRecoveryPgFixture(t, tb)
		limits := defaultCompetitionDistributedReevaluationSettings()
		limits.NativeProofLifetime = time.Second
		fixture.settings.distributedReevaluationSettings = &limits
		pop := server.Config.PushSimpleResource(server.MaintenancePgConfigResourceName, []byte("min_connections: 0\nmax_connections: 3\n"))
		server.PgReset()
		defer func() { pop(); server.PgReset() }()
		var barrier *competitionDistributedRecoveryPgBarrier
		barrierReady := make(chan *competitionDistributedRecoveryPgBarrier, 1)
		operationCtx, cancel := context.WithCancel(watchdog)
		ownerDone := make(chan struct{})
		ownerStarted := false
		// Registered before holder or operation starts, including setup failures.
		defer func() {
			cancel()
			cleanup, closeCleanup := context.WithTimeout(context.WithoutCancel(watchdog), 10*time.Second)
			defer closeCleanup()
			if barrier == nil && ownerStarted && phase == "final-row-lock" {
				select {
				case barrier = <-barrierReady:
				case <-ownerDone:
					select {
					case barrier = <-barrierReady:
					default:
					}
				case <-cleanup.Done():
					tb.Error("native holder publication failed to join")
				}
			}
			if barrier != nil {
				barrier.unlock()
			}
			if ownerStarted {
				select {
				case <-ownerDone:
				case <-cleanup.Done():
					tb.Error("grant owner failed to physically join")
				}
			}
			if barrier != nil {
				select {
				case <-barrier.done:
				case <-cleanup.Done():
					tb.Error("owned recovery barrier failed to physically join")
				}
			}
		}()
		if phase != "final-row-lock" {
			server.Tx(watchdog, func(tx server.PgTx) {
				var database string
				server.Raise(tx.QueryRow(watchdog, `SELECT current_database()`).Scan(&database))
				if !strings.HasPrefix(database, "test_") {
					panic("recovery barriers require an isolated generated test database")
				}
				server.RaisePgResult(tx.Exec(watchdog, `CREATE FUNCTION synthetic_distributed_recovery_barrier() RETURNS trigger LANGUAGE plpgsql AS $body$
 BEGIN
  PERFORM set_config('lock_timeout','0',true);
  PERFORM pg_advisory_xact_lock(182731,92837);
  RETURN NEW;
 END $body$;`))
				if phase == "audit-insert" {
					server.RaisePgResult(tx.Exec(watchdog, `CREATE TRIGGER aaa_synthetic_distributed_recovery_barrier BEFORE INSERT ON competition_job_reevaluation FOR EACH ROW EXECUTE FUNCTION synthetic_distributed_recovery_barrier()`))
				} else if phase == "queue-update" {
					server.RaisePgResult(tx.Exec(watchdog, `CREATE TRIGGER aaa_synthetic_distributed_recovery_barrier BEFORE UPDATE ON competition_job FOR EACH ROW WHEN(OLD.evaluation_cycle=0 AND NEW.evaluation_cycle=1) EXECUTE FUNCTION synthetic_distributed_recovery_barrier()`))
				} else {
					panic("unknown synthetic recovery barrier phase")
				}
			})
			barrier = competitionDistributedRecoveryStartBarrier(tb, watchdog, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(watchdog, `SELECT pg_advisory_xact_lock(182731,92837)`))
			})
		} else {
			nativeBarrierStarted := false
			fixture.beforeNative = func(competitionagent.Request) {
				if !nativeBarrierStarted {
					nativeBarrierStarted = true
					holder := competitionDistributedRecoveryStartBarrier(tb, operationCtx, func(tx server.PgTx) {
						var id server.Id
						server.Raise(tx.QueryRow(watchdog, `SELECT job_id FROM competition_job WHERE job_id=$1 FOR UPDATE`, fixture.original.JobId).Scan(&id))
					})
					barrierReady <- holder
				}
			}
		}
		before := fixture.snapshot(watchdog)
		type outcome struct {
			receipt *model.CompetitionJobReevaluationReceipt
			err     error
		}
		returned := make(chan outcome, 1)
		ownerStarted = true
		go func() {
			defer close(ownerDone)
			receipt, err := fixture.authorize(tb, operationCtx)
			returned <- outcome{receipt: receipt, err: err}
		}()
		if phase == "final-row-lock" {
			// The exact native callback publishes its holder before returning.
			// Receive that publication through the holder's native acknowledgement.
			select {
			case barrier = <-barrierReady:
			case <-ownerDone:
				tb.Fatal("grant stopped before installing its final row-lock barrier")
			case <-watchdog.Done():
				tb.Fatal("final row-lock barrier was not reached")
			}
		}
		competitionDistributedRecoveryWaitForBlockedBackend(tb, watchdog, barrier.pid, ownerDone)
		if cancelAtBarrier {
			cancel()
		} else {
			// A positive declared lifetime expiry after a confirmed server wait
			// proves stale native authority; parent owner remains live throughout.
			select {
			case <-time.After(limits.NativeProofLifetime):
			case <-watchdog.Done():
				tb.Fatal("proof expiry witness exceeded watchdog")
			}
			if operationCtx.Err() != nil {
				tb.Fatal("parent cancellation confused proof expiry")
			}
		}
		barrier.unlock()
		var result outcome
		select {
		case result = <-returned:
		case <-watchdog.Done():
			tb.Fatal("grant failed to return after acknowledged barrier release")
		}
		<-ownerDone
		if result.receipt != nil || result.err == nil || !reflect.DeepEqual(before, fixture.snapshot(watchdog)) {
			tb.Fatal("expired/canceled grant partially committed its audit, cycle or idle fences", phase, result.err)
		}
		if phase == "queue-update" && !strings.Contains(fmt.Sprint(result.err), "expired before commit") {
			tb.Fatal("deferred native-proof guard was not the expiry owner", result.err)
		}
	})
}

// A final database row wait cannot reuse an old, otherwise unchanged native cut.
func TestCompetitionDistributedRecoveryPgFinalRowWaitExpiresNativeProof(t *testing.T) {
	testCompetitionDistributedRecoveryPgBarrier(t, "final-row-lock", false)
}

// Cancellation after the completed final gate but before audit insertion must
// roll back, even though all original native observations succeeded.
func TestCompetitionDistributedRecoveryPgCancelBetweenFinalGateAndInsert(t *testing.T) {
	testCompetitionDistributedRecoveryPgBarrier(t, "audit-insert", true)
}

// Expiry after the private audit and fence writes still rolls the entire
// transaction back through its deferred commit guard.
func TestCompetitionDistributedRecoveryPgDeferredExpiryRollsBackAllFences(t *testing.T) {
	testCompetitionDistributedRecoveryPgBarrier(t, "queue-update", false)
}
