// Conditional onboarding inserts must remain idempotent across concurrent
// transactions, independently of the per-client connect.day cache.
package model

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Two different clients can both pass the cache and observe an empty network
// day before either inserts. Both writes must complete with exactly one event.
func TestRecordConnectDayConcurrentClientsPersistOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		networkId := server.NewId()
		clientIds := []server.Id{server.NewId(), server.NewId()}
		connectAt := time.Date(2026, 9, 10, 12, 30, 0, 0, time.UTC)
		day := ConnectDayStart(connectAt)
		server.Tx(ctx, func(tx server.PgTx) {
			for _, clientId := range clientIds {
				server.RaisePgResult(tx.Exec(ctx,
					`INSERT INTO network_client (client_id, network_id, active) VALUES ($1, $2, true)`,
					clientId, networkId,
				))
			}
		})

		writers := []func(context.Context){}
		for _, clientId := range clientIds {
			writers = append(writers, func(writeCtx context.Context) {
				waitConnectDayWrites(recordConnectDayForTest(writeCtx, clientId, connectAt))
			})
		}
		runConcurrentOnboardingInserts(t, ctx, writers)
		for _, clientId := range clientIds {
			if connectDaySeen.remember(clientId, day) {
				t.Fatal("concurrent connect.day write failed and forgot the client")
			}
		}
		if count := connectDayEventCount(t, ctx, networkId); count != 1 {
			t.Fatalf("concurrent connect.day writes stored %d events, want 1", count)
		}
	})
}

// App-open attribution uses the same conditional-insert boundary: concurrent
// authentications must not both attribute the same landing click.
func TestAttributeAppOpenConcurrentCallsAttributeOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		networkId := server.NewId()
		clickAt := time.Date(2026, 9, 10, 12, 30, 0, 0, time.UTC)
		if err := AddOnboardingEvent(ctx, &OnboardingEvent{
			NetworkId: networkId, Name: EventLandingClicked, At: clickAt,
		}); err != nil {
			t.Fatal(err)
		}

		attributed := make([]bool, 2)
		writers := []func(context.Context){}
		for index := range attributed {
			writers = append(writers, func(writeCtx context.Context) {
				attributed[index] = AttributeAppOpen(writeCtx, networkId, clickAt.Add(time.Minute))
			})
		}
		runConcurrentOnboardingInserts(t, ctx, writers)
		attributionCount := 0
		for _, wrote := range attributed {
			if wrote {
				attributionCount++
			}
		}
		if attributionCount != 1 {
			t.Errorf("concurrent app opens returned %d attributions, want 1", attributionCount)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(ctx,
				`SELECT count(*) FROM network_onboarding_event WHERE network_id = $1 AND name = $2`,
				networkId, EventAppOpened,
			).Scan(&count))
			if count != 1 {
				t.Errorf("concurrent app opens stored %d events, want 1", count)
			}
		})
	})
}

// A private-database row trigger parks every writer after its NOT EXISTS read.
// Observing all advisory-lock waiters before release forces the write skew;
// joining the writers prevents an intermediate count of one from hiding it.
func runConcurrentOnboardingInserts(t testing.TB, ctx context.Context, writers []func(context.Context)) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			CREATE FUNCTION synthetic_pause_onboarding_insert() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				PERFORM pg_advisory_xact_lock(1, 1);
				RETURN NEW;
			END;
			$$;
			CREATE TRIGGER synthetic_pause_onboarding_insert
			BEFORE INSERT ON network_onboarding_event
			FOR EACH ROW EXECUTE FUNCTION synthetic_pause_onboarding_insert();
		`))
	})

	conn, err := server.AcquireMaintenanceDbConn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	lockTx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writeCtx, cancelWrites := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() {
		cancelWrites()
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelCleanup()
		_ = lockTx.Rollback(cleanupCtx)
		workers.Wait()
	}()
	server.RaisePgResult(lockTx.Exec(ctx, `SELECT pg_advisory_xact_lock(1, 1)`))
	var blockerPid int32
	server.Raise(lockTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPid))

	finished := make(chan any, len(writers))
	for _, writer := range writers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			finished <- server.HandleError(func() { writer(writeCtx) })
		}()
	}
	for {
		var blockedCount int
		server.Raise(lockTx.QueryRow(ctx, `
			SELECT count(DISTINCT pid)
			FROM pg_locks
			WHERE locktype = 'advisory' AND NOT granted
				AND $1::int = ANY(pg_blocking_pids(pid))
		`, blockerPid).Scan(&blockedCount))
		if blockedCount == len(writers) {
			t.Logf("all %d writers reached the insert barrier after reading the empty predicate", blockedCount)
			break
		}
		select {
		case recovered := <-finished:
			t.Fatalf("writer completed before the insert barrier: %v", recovered)
		case <-ctx.Done():
			t.Fatalf("only %d of %d writers reached the insert barrier: %v", blockedCount, len(writers), ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	server.Raise(lockTx.Rollback(ctx))
	workers.Wait()
	for range writers {
		if recovered := <-finished; recovered != nil {
			t.Fatalf("concurrent onboarding writer failed: %v", recovered)
		}
	}
}
