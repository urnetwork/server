// Connection owners take the connection table's lock before an endpoint
// fence, so a strong table lock never parks an owner that holds a fence.
package model

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// On Main (2026-10-12 18:33Z) autovacuum's end-of-vacuum truncation held
// AccessExclusiveLock on network_client_connection for 4 s. A connect that
// already owned its endpoint fence waited for the table there, and the
// endpoint's next connect waited for that fence on advisory lock 776. Here a
// session holds the same table lock while two connects of one endpoint start,
// as two transports of one client do. Once both are parked behind the
// session, directly or behind each other, neither may hold or await the
// endpoint fence: each waits for the table before taking it. Releasing the
// table then admits both connections in order.
func TestConnectionOwnersQueueForTableLockBeforeEndpointFence(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
		defer cancel()
		headSequence := func() uint64 {
			var sequence uint64
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT sequence FROM provider_work_session_head WHERE client_id=$1`, f.sourceId).Scan(&sequence))
			})
			return sequence
		}
		startSequence := headSequence()

		conn, err := server.AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		blocker, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		connectCtx, cancelConnects := context.WithCancel(ctx)
		var owners sync.WaitGroup
		defer func() {
			cancelConnects()
			cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancelCleanup()
			_ = blocker.Rollback(cleanupCtx)
			owners.Wait()
		}()
		// autovacuum truncation's mode, as Main's lock waits recorded it
		server.RaisePgResult(blocker.Exec(ctx, `LOCK TABLE network_client_connection IN ACCESS EXCLUSIVE MODE`))
		var blockerPid int32
		server.Raise(blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPid))

		type admission struct {
			connectionId server.Id
			err          error
		}
		admissions := make(chan admission, 2)
		for i := range 2 {
			owners.Add(1)
			go func() {
				defer owners.Done()
				var result admission
				if recovered := server.HandleError(func() {
					result.connectionId, _, _, _, result.err = ConnectNetworkClientWithIpFamily(
						connectCtx, f.sourceId, fmt.Sprintf("192.0.2.%d:10010", 20+i), f.handlerId, 4,
					)
				}); recovered != nil {
					result.err = fmt.Errorf("connect panicked: %v", recovered)
				}
				admissions <- result
			}()
		}

		for {
			var parkedCount int
			server.Raise(blocker.QueryRow(ctx, `
				WITH RECURSIVE parked(pid) AS (
					SELECT pid FROM pg_locks
					WHERE NOT granted AND $1::int = ANY(pg_blocking_pids(pid))
					UNION
					SELECT waiting.pid FROM pg_locks waiting
					JOIN parked ON parked.pid = ANY(pg_blocking_pids(waiting.pid))
					WHERE NOT waiting.granted
				)
				SELECT count(*) FROM parked
			`, blockerPid).Scan(&parkedCount))
			if parkedCount == 2 {
				t.Logf("both connects parked behind the connection table lock")
				break
			}
			select {
			case result := <-admissions:
				t.Fatalf("connect finished while the connection table was locked: %v", result.err)
			case <-ctx.Done():
				t.Fatalf("only %d of 2 connects parked behind the table lock: %v", parkedCount, ctx.Err())
			case <-time.After(time.Millisecond):
			}
		}

		// Granted or awaited, any lock on the endpoint key means an owner
		// reached the fence before the table it is about to write.
		var fenceLockCount int
		server.Raise(blocker.QueryRow(ctx, `
			SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND classid = 776 AND objsubid = 2
			AND objid::bigint = (('x'||substr(md5($1::uuid::text),1,8))::bit(32)::int::bigint & 4294967295)
		`, f.sourceId).Scan(&fenceLockCount))
		if fenceLockCount != 0 {
			t.Fatalf("%d endpoint fence locks held or awaited while the connection table was locked; owners must wait for the table first", fenceLockCount)
		}

		server.Raise(blocker.Rollback(ctx))
		releaseTime := time.Now()
		connectionIds := map[server.Id]bool{}
		for range 2 {
			result := <-admissions
			if result.err != nil {
				t.Fatalf("connect failed after the table lock was released: %v", result.err)
			}
			connectionIds[result.connectionId] = true
		}
		t.Logf("both connects committed %v after the table lock was released", time.Since(releaseTime))
		if len(connectionIds) != 2 {
			t.Fatalf("connects returned %d distinct connections, want 2", len(connectionIds))
		}
		if sequence := headSequence(); sequence != startSequence+2 {
			t.Fatalf("endpoint journal head = %d, want %d after two admissions", sequence, startSequence+2)
		}
	})
}
