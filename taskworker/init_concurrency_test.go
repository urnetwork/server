// Startup scheduling must converge on existing run-once rows while other
// initializers and the claim loop update those rows.
package taskworker

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/taskworker/work"
)

// The production initializer shares hot queue rows with live claim updates.
func TestInitTasksProductionConvergesDuringConcurrentClaim(t *testing.T) {
	testInitTasksConcurrentClaim(t, WorkloadProfileProduction)
}

// The narrower profile uses its own scheduling transaction and must retain
// the same conflict semantics without initializing excluded workloads.
func TestInitTasksSubnetOperatorConvergesDuringConcurrentClaim(t *testing.T) {
	testInitTasksConcurrentClaim(t, WorkloadProfileSubnetOperator)
}

// Hold an actual queue update open until both initializers are blocked behind
// it. The insert trigger's non-transactional sequence counts attempts, including
// aborted snapshots, without timing assumptions or a production test hook.
func testInitTasksConcurrentClaim(t *testing.T, profile WorkloadProfile) {
	t.Helper()
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			work.ScheduleExportStats(clientSession, tx)
			server.RaisePgResult(tx.Exec(ctx, `
			    CREATE SEQUENCE synthetic_init_attempts;
			    CREATE FUNCTION synthetic_count_init_attempt() RETURNS trigger LANGUAGE plpgsql AS $$
			    BEGIN
			        IF NEW.run_once_key = '["export_stats"]' THEN
			            PERFORM nextval('synthetic_init_attempts');
			        END IF;
			        RETURN NEW;
			    END $$;
			    CREATE TRIGGER synthetic_init_attempt BEFORE INSERT ON pending_task
			        FOR EACH ROW EXECUTE FUNCTION synthetic_count_init_attempt();
			`))
		})

		blocker, err := server.AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Release()
		server.RaisePgResult(blocker.Exec(ctx, `SET default_transaction_read_only = off`))
		claimTx, err := blocker.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer claimTx.Rollback(context.Background())
		var blockerPid int32
		server.Raise(claimTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPid))
		var taskId server.Id
		claimTime := server.NowUtc().Truncate(time.Millisecond)
		releaseTime := claimTime.Add(10 * time.Minute)
		server.Raise(claimTx.QueryRow(ctx, `
		    UPDATE pending_task SET claim_time=$1, release_time=$2,
		        args_json='{"synthetic_marker":"preserve"}'
		    WHERE run_once_key='["export_stats"]' RETURNING task_id;
		`, claimTime, releaseTime).Scan(&taskId))

		done := make(chan struct{})
		results := make(chan error, 2)
		for range 2 {
			go func() {
				var result error
				defer func() {
					if recovered := recover(); recovered != nil {
						result = fmt.Errorf("initializer panic: %v", recovered)
					}
					results <- result
				}()
				result = InitTasksForProfile(ctx, profile)
			}()
		}
		go func() {
			defer close(done)
			for range 2 {
				if err := <-results; err != nil {
					t.Errorf("concurrent initializer: %v", err)
				}
			}
		}()
		defer func() {
			cancel()
			_ = claimTx.Rollback(context.Background())
			<-done
		}()

		server.Db(ctx, func(observer server.PgConn) {
			for {
				var blocked int
				server.Raise(observer.QueryRow(ctx, `
				    WITH RECURSIVE waiting(pid) AS (
				        SELECT DISTINCT pid FROM pg_locks
				        WHERE NOT granted AND $1::int=ANY(pg_blocking_pids(pid))
				        UNION
				        SELECT l.pid FROM pg_locks l JOIN waiting w
				          ON w.pid=ANY(pg_blocking_pids(l.pid)) WHERE NOT l.granted
				    ) SELECT count(*) FROM waiting;
				`, blockerPid).Scan(&blocked))
				if blocked == 2 {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatalf("both initializers did not reach the claim barrier: %v", ctx.Err())
				case <-done:
					t.Fatal("initializers returned before the claim barrier opened")
				default:
					runtime.Gosched()
				}
			}
		})
		server.Raise(claimTx.Commit(ctx))
		<-done

		server.Db(ctx, func(conn server.PgConn) {
			var attempts int
			server.Raise(conn.QueryRow(ctx, `SELECT last_value FROM synthetic_init_attempts`).Scan(&attempts))
			if attempts != 2 {
				t.Errorf("two idempotent initializers made %d insert attempts after one claim update; want 2 without snapshot retries", attempts)
			}
			var count int
			var gotId server.Id
			var args string
			var gotClaimTime, gotReleaseTime time.Time
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE run_once_key='["export_stats"]'`).Scan(&count))
			server.Raise(conn.QueryRow(ctx, `
			    SELECT task_id,args_json,claim_time,release_time FROM pending_task
			    WHERE run_once_key='["export_stats"]'
			`).Scan(&gotId, &args, &gotClaimTime, &gotReleaseTime))
			if count != 1 || gotId != taskId || args != `{"synthetic_marker":"preserve"}` ||
				!gotClaimTime.Equal(claimTime) || !gotReleaseTime.Equal(releaseTime) {
				t.Fatal("initialization replaced the queue identity, arguments or active claim")
			}
		})
	})
}
