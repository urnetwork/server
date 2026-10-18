// Conditional onboarding inserts must remain idempotent across concurrent
// transactions, independently of the connect.day process memory and claim.
// They run read committed under a per-key advisory lock, never serializable,
// so they add no SSI work to the primary.
package model

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// Two clients of one network, admitted by two processes, both reach the
// database when the fleet's claim cannot admit one writer, here because Redis
// cannot answer for the day, and can observe an empty network day before
// either inserts. Both writes must complete with exactly one event.
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
		// a value of another type makes every claim of the day fail
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.HSet(ctx, connectDayKey(networkId, day), "synthetic", "1").Err())
		})
		defer useConnectDayProcess()()
		// as admission leaves it; a failed write forgets the day
		connectDaySeen.remember(networkId, day)

		writers := []func(context.Context){}
		for _, clientId := range clientIds {
			writers = append(writers, func(writeCtx context.Context) {
				recordConnectDayWrite(connectDayWriteJob{
					ctx:         writeCtx,
					networkId:   networkId,
					clientId:    clientId,
					connectTime: connectAt,
					day:         day,
				})
			})
		}
		runConcurrentOnboardingInserts(t, ctx, writers)
		if connectDaySeen.remember(networkId, day) {
			t.Fatal("concurrent connect.day write failed and forgot the network")
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

// Without the network's insert lock, the connect.day conditional insert alone
// stores a duplicate: both clients' transactions read the empty day before
// either commits. This control shows the barrier forces the race that
// TestRecordConnectDayConcurrentClientsPersistOnce proves the lock prevents.
func TestUnlockedConnectDayInsertsStoreDuplicate(t *testing.T) {
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
				server.Tx(writeCtx, func(tx server.PgTx) {
					insertConnectDayInTx(tx, writeCtx, clientId, networkId, connectAt, day)
				}, server.TxReadCommitted)
			})
		}
		runConcurrentOnboardingInserts(t, ctx, writers)
		if count := connectDayEventCount(t, ctx, networkId); count != 2 {
			t.Fatalf("unlocked concurrent connect.day inserts stored %d events, want the duplicate pair", count)
		}
	})
}

// Without the network's insert lock, the app-open conditional insert alone
// attributes one landing click twice. This control shows the barrier forces
// the race that TestAttributeAppOpenConcurrentCallsAttributeOnce proves the
// lock prevents.
func TestUnlockedAppOpenAttributionsStoreDuplicate(t *testing.T) {
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
				server.Tx(writeCtx, func(tx server.PgTx) {
					attributed[index] = insertAppOpenAttributionInTx(tx, writeCtx, networkId, clickAt.Add(time.Minute))
				}, server.TxReadCommitted)
			})
		}
		runConcurrentOnboardingInserts(t, ctx, writers)
		if !attributed[0] || !attributed[1] {
			t.Errorf("unlocked concurrent app opens returned %v, want both to attribute", attributed)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(ctx,
				`SELECT count(*) FROM network_onboarding_event WHERE network_id = $1 AND name = $2`,
				networkId, EventAppOpened,
			).Scan(&count))
			if count != 2 {
				t.Errorf("unlocked concurrent app opens stored %d events, want the duplicate pair", count)
			}
		})
	})
}

// The insert lock orders writers only for read committed statements. A
// repeatable read snapshot is fixed before the lock wait and could miss the
// earlier holder's insert, so the lock refuses it and nothing is written.
func TestOnboardingEventInsertLockRefusesRepeatableRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		clickAt := time.Date(2026, 9, 10, 12, 30, 0, 0, time.UTC)
		if err := AddOnboardingEvent(ctx, &OnboardingEvent{
			NetworkId: networkId, Name: EventLandingClicked, At: clickAt,
		}); err != nil {
			t.Fatal(err)
		}

		attributeIn := func(isolation pgx.TxIsoLevel) (attributed bool, recovered any) {
			defer func() {
				recovered = recover()
			}()
			server.Tx(ctx, func(tx server.PgTx) {
				attributed = AttributeAppOpenInTx(tx, ctx, networkId, clickAt.Add(time.Minute))
			}, isolation)
			return
		}
		attributed, recovered := attributeIn(server.TxRepeatableRead)
		if recovered == nil {
			t.Fatalf("repeatable read attribution returned %v without a refusal", attributed)
		}
		if !strings.Contains(fmt.Sprint(recovered), "requires read committed isolation") {
			t.Fatalf("repeatable read attribution failed for another reason: %v", recovered)
		}
		// the refused transaction wrote nothing, so the click is still open
		attributed, recovered = attributeIn(server.TxReadCommitted)
		if recovered != nil || !attributed {
			t.Fatalf("read committed attribution returned %v (%v), want one attribution", attributed, recovered)
		}
	})
}

// A private-database row trigger parks every writer that reaches its insert,
// after its NOT EXISTS read. A writer the design makes wait for another writer
// parks behind that writer instead. Once every writer is parked, directly or
// behind another, none has observed another's insert, so any design that lets
// two writers read the missing event before either commits produces the write
// skew. Parked writers must hold no SSI predicate locks: these inserts run read
// committed under an advisory lock, not serializable. Joining the writers
// prevents an intermediate count of one from hiding a duplicate.
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
		var parkedCount int
		server.Raise(lockTx.QueryRow(ctx, `
			WITH RECURSIVE parked(pid) AS (
				SELECT pid
				FROM pg_locks
				WHERE NOT granted AND $1::int = ANY(pg_blocking_pids(pid))
				UNION
				SELECT waiting.pid
				FROM pg_locks waiting
				JOIN parked ON parked.pid = ANY(pg_blocking_pids(waiting.pid))
				WHERE NOT waiting.granted
			)
			SELECT count(*) FROM parked
		`, blockerPid).Scan(&parkedCount))
		if parkedCount == len(writers) {
			t.Logf("all %d writers parked at the insert barrier or behind a parked writer", parkedCount)
			break
		}
		select {
		case recovered := <-finished:
			t.Fatalf("writer completed before the insert barrier: %v", recovered)
		case <-ctx.Done():
			t.Fatalf("only %d of %d writers parked at the insert barrier: %v", parkedCount, len(writers), ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	var predicateLockCount int
	server.Raise(lockTx.QueryRow(ctx, `
		SELECT count(*)
		FROM pg_locks
		WHERE mode = 'SIReadLock' AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
	`).Scan(&predicateLockCount))
	if predicateLockCount != 0 {
		t.Fatalf("parked onboarding writers hold %d SSI predicate locks; these inserts must not run serializable", predicateLockCount)
	}
	server.Raise(lockTx.Rollback(ctx))
	workers.Wait()
	for range writers {
		if recovered := <-finished; recovered != nil {
			t.Fatalf("concurrent onboarding writer failed: %v", recovered)
		}
	}
}
