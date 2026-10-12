package model

// Clients retire a derived window client with duplicate removal requests that
// reach the database together. A duplicate that waits on the first removal's
// row lock must finish against the committed row: it must not abort its
// transaction for a rerun (the serialization failure a repeatable read
// snapshot raises behind a concurrent update), and a client token's duplicate
// must not write the row a second time. A trigger barrier parks a removal
// after its deactivation write and before its commit, which is the window a
// duplicate meets in production.

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A network with a top-level client and a child that client created.
type removalConcurrencyTestNetwork struct {
	networkId      server.Id
	userId         server.Id
	parentClientId server.Id
	parentDeviceId server.Id
	childClientId  server.Id
}

func newRemovalConcurrencyTestNetwork(t testing.TB, ctx context.Context) *removalConcurrencyTestNetwork {
	t.Helper()
	network := &removalConcurrencyTestNetwork{
		networkId:      server.NewId(),
		userId:         server.NewId(),
		parentClientId: server.NewId(),
		parentDeviceId: server.NewId(),
		childClientId:  server.NewId(),
	}
	Testing_CreateNetwork(ctx, network.networkId, "removal-"+network.networkId.String(), network.userId)
	createTime := server.NowUtc()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_client (client_id, network_id, active, create_time, auth_time, source_client_id)
				VALUES ($1, $3, true, $4, $4, NULL), ($2, $3, true, $4, $4, $1)
			`,
			network.parentClientId,
			network.childClientId,
			network.networkId,
			createTime,
		))
	})
	return network
}

// The parent's client-token session, or the network credential's session when
// clientId is nil. Every rerun of one of its transactions counts in reruns.
func (self *removalConcurrencyTestNetwork) session(ctx context.Context, clientId *server.Id, reruns *atomic.Int32) *session.ClientSession {
	ctx = server.Testing_WithTxRerunHook(ctx, func() {
		reruns.Add(1)
	})
	byJwt := &session.ByJwt{
		NetworkId: self.networkId,
		UserId:    self.userId,
	}
	if clientId != nil {
		byJwt.ClientId = clientId
		byJwt.DeviceId = &self.parentDeviceId
	}
	return session.Testing_CreateClientSession(ctx, byJwt)
}

// A removal that reports a refusal or a failed transaction as an error.
func removalConcurrencyTestCall(clientSession *session.ClientSession, clientId server.Id) func() error {
	return func() error {
		_, err := server.HandleError2(func() (struct{}, error) {
			result, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: clientId}, clientSession)
			if err != nil {
				return struct{}{}, err
			}
			if result == nil || result.Error != nil {
				return struct{}{}, fmt.Errorf("removal refused: %+v", result)
			}
			return struct{}{}, nil
		}, func(err error) (struct{}, error) {
			return struct{}{}, err
		})
		return err
	}
}

// Records each committed write of a client's lifecycle columns, in write
// order, then parks the writer on the barrier lock while another transaction
// holds it. Each test has its own database, so the table, trigger and lock are
// private to the test.
func installRemovalConcurrencyTestBarrier(t testing.TB, ctx context.Context) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			CREATE TABLE test_removal_write (
				write_order bigserial PRIMARY KEY,
				client_id uuid NOT NULL,
				active boolean NOT NULL,
				deactivate_time timestamp
			);
			CREATE FUNCTION test_removal_write() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				INSERT INTO test_removal_write (client_id, active, deactivate_time)
				VALUES (NEW.client_id, NEW.active, NEW.deactivate_time);
				PERFORM pg_advisory_xact_lock(2, 1);
				RETURN NEW;
			END;
			$$;
			CREATE TRIGGER test_removal_write
			AFTER UPDATE OF active, deactivate_time ON network_client
			FOR EACH ROW EXECUTE FUNCTION test_removal_write();
		`))
	})
}

// The deactivate_time of each committed lifecycle write of the client.
func removalConcurrencyTestWrites(t testing.TB, ctx context.Context, clientId server.Id) []time.Time {
	t.Helper()
	writeTimes := []time.Time{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			ctx,
			`SELECT active, deactivate_time FROM test_removal_write WHERE client_id = $1 ORDER BY write_order`,
			clientId,
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var active bool
				var deactivateTime *time.Time
				server.Raise(rows.Scan(&active, &deactivateTime))
				if active || deactivateTime == nil {
					t.Fatalf("a lifecycle write left active=%t deactivate_time=%v", active, deactivateTime)
				}
				writeTimes = append(writeTimes, *deactivateTime)
			}
		})
	})
	return writeTimes
}

func removalConcurrencyTestState(t testing.TB, ctx context.Context, clientId server.Id) (active bool, deactivateTime *time.Time) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT active, deactivate_time FROM network_client WHERE client_id = $1`,
			clientId,
		).Scan(&active, &deactivateTime))
	})
	return
}

// A holder transaction takes the barrier lock and runs `holder`. Each step then
// starts in order, and the next starts only once the step's backend is blocked
// by the previous one (the first step by the holder). Then the holder reads the
// database clock and commits, or rolls back when `commit` is false, and every
// step's outcome is returned with that release time. A step that completes
// before it is blocked fails the test.
func runRemovalConcurrencyTestSteps(
	t testing.TB,
	ctx context.Context,
	holder func(holderTx server.PgTx),
	commit bool,
	steps []func() error,
) (stepErrs []error, releaseTime time.Time) {
	t.Helper()
	conn := acquireContractLifecycleTestConnection(t, ctx)
	defer conn.Release()
	holderTx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	stepDones := []chan error{}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_ = holderTx.Rollback(cleanupCtx)
		for _, stepDone := range stepDones {
			select {
			case <-stepDone:
			case <-cleanupCtx.Done():
				t.Error("removal step did not join")
				return
			}
		}
	}()
	server.RaisePgResult(holderTx.Exec(ctx, `SELECT pg_advisory_xact_lock(2, 1)`))
	if holder != nil {
		holder(holderTx)
	}

	blockerPid := contractLifecycleTestBackendPid(t, ctx, holderTx)
	for i, step := range steps {
		stepDone := make(chan error, 1)
		stepDones = append(stepDones, stepDone)
		go func() {
			stepDone <- step()
		}()
		for {
			var blockedPid int32
			server.Raise(holderTx.QueryRow(
				ctx,
				`SELECT coalesce(min(pid), 0) FROM pg_locks WHERE NOT granted AND $1::int = ANY(pg_blocking_pids(pid))`,
				blockerPid,
			).Scan(&blockedPid))
			if blockedPid != 0 {
				blockerPid = blockedPid
				break
			}
			select {
			case err := <-stepDone:
				// keep the outcome for the deferred join
				stepDone <- err
				t.Fatalf("removal step %d completed before it was blocked: %v", i, err)
			case <-ctx.Done():
				t.Fatalf("removal step %d was not blocked: %v", i, ctx.Err())
			default:
				runtime.Gosched()
			}
		}
	}

	server.Raise(holderTx.QueryRow(ctx, `SELECT clock_timestamp() AT TIME ZONE 'UTC'`).Scan(&releaseTime))
	if commit {
		server.Raise(holderTx.Commit(ctx))
	} else {
		server.Raise(holderTx.Rollback(ctx))
	}
	stepErrs = make([]error, len(steps))
	for i, stepDone := range stepDones {
		select {
		case stepErrs[i] = <-stepDone:
			// keep the outcome for the deferred join
			stepDone <- stepErrs[i]
		case <-ctx.Done():
			t.Fatalf("removal step %d did not complete: %v", i, ctx.Err())
		}
	}
	return
}

// The duplicate of a client token's removal waits on the first removal's row
// lock. It must neither abort for a rerun nor deactivate the child again, and
// both callers see the removal succeed.
func TestRemoveNetworkClientWaitedDuplicateDeactivatesOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		network := newRemovalConcurrencyTestNetwork(t, ctx)
		installRemovalConcurrencyTestBarrier(t, ctx)

		var firstReruns, duplicateReruns atomic.Int32
		stepErrs, _ := runRemovalConcurrencyTestSteps(t, ctx, nil, true, []func() error{
			removalConcurrencyTestCall(network.session(ctx, &network.parentClientId, &firstReruns), network.childClientId),
			removalConcurrencyTestCall(network.session(ctx, &network.parentClientId, &duplicateReruns), network.childClientId),
		})
		if stepErrs[0] != nil || stepErrs[1] != nil {
			t.Fatalf("removals failed: first=%v duplicate=%v", stepErrs[0], stepErrs[1])
		}
		if reruns := firstReruns.Load(); reruns != 0 {
			t.Errorf("the first removal reran %d times", reruns)
		}
		if reruns := duplicateReruns.Load(); reruns != 0 {
			t.Errorf("the duplicate removal's transaction aborted and reran %d times behind the committed removal", reruns)
		}
		writeTimes := removalConcurrencyTestWrites(t, ctx, network.childClientId)
		if len(writeTimes) != 1 {
			t.Fatalf("the child was deactivated %d times, want once", len(writeTimes))
		}
		active, deactivateTime := removalConcurrencyTestState(t, ctx, network.childClientId)
		if active || deactivateTime == nil || !deactivateTime.Equal(writeTimes[0]) {
			t.Fatalf("child state active=%t deactivate_time=%v, want the first removal's %v", active, deactivateTime, writeTimes[0])
		}
		if active, _ := removalConcurrencyTestState(t, ctx, network.parentClientId); !active {
			t.Fatal("removing the child deactivated its parent")
		}
	})
}

// The network credential's removal keeps its repeated-removal semantics (a
// repeat rewrites the inactive row with a new clock, as
// TestDeactivationRepeatedRequestRewritesInactiveRows characterizes), but its
// duplicate must wait for the row lock instead of aborting for a rerun.
func TestRemoveNetworkClientNetworkTokenWaitedDuplicateDoesNotRerun(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		network := newRemovalConcurrencyTestNetwork(t, ctx)
		installRemovalConcurrencyTestBarrier(t, ctx)

		var firstReruns, duplicateReruns atomic.Int32
		stepErrs, _ := runRemovalConcurrencyTestSteps(t, ctx, nil, true, []func() error{
			removalConcurrencyTestCall(network.session(ctx, nil, &firstReruns), network.childClientId),
			removalConcurrencyTestCall(network.session(ctx, nil, &duplicateReruns), network.childClientId),
		})
		if stepErrs[0] != nil || stepErrs[1] != nil {
			t.Fatalf("removals failed: first=%v duplicate=%v", stepErrs[0], stepErrs[1])
		}
		if reruns := firstReruns.Load(); reruns != 0 {
			t.Errorf("the first removal reran %d times", reruns)
		}
		if reruns := duplicateReruns.Load(); reruns != 0 {
			t.Errorf("the duplicate removal's transaction aborted and reran %d times behind the committed removal", reruns)
		}
		writeTimes := removalConcurrencyTestWrites(t, ctx, network.childClientId)
		if len(writeTimes) != 2 || writeTimes[1].Before(writeTimes[0]) {
			t.Fatalf("lifecycle writes %v, want the removal and its later rewrite", writeTimes)
		}
		active, deactivateTime := removalConcurrencyTestState(t, ctx, network.childClientId)
		if active || deactivateTime == nil || !deactivateTime.Equal(writeTimes[1]) {
			t.Fatalf("child state active=%t deactivate_time=%v, want the rewrite's %v", active, deactivateTime, writeTimes[1])
		}
	})
}

// A client token's removal of a child that a previous removal already
// deactivated succeeds and writes nothing, keeping the original clock.
func TestRemoveNetworkClientRepeatedOwnedRemovalWritesNothing(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		network := newRemovalConcurrencyTestNetwork(t, ctx)
		installRemovalConcurrencyTestBarrier(t, ctx)

		var reruns atomic.Int32
		clientSession := network.session(ctx, &network.parentClientId, &reruns)
		for attempt := 0; attempt < 3; attempt += 1 {
			if err := removalConcurrencyTestCall(clientSession, network.childClientId)(); err != nil {
				t.Fatalf("attempt %d: %v", attempt, err)
			}
		}
		writeTimes := removalConcurrencyTestWrites(t, ctx, network.childClientId)
		if len(writeTimes) != 1 {
			t.Fatalf("three removals deactivated the child %d times, want once", len(writeTimes))
		}
		active, deactivateTime := removalConcurrencyTestState(t, ctx, network.childClientId)
		if active || deactivateTime == nil || !deactivateTime.Equal(writeTimes[0]) {
			t.Fatalf("child state active=%t deactivate_time=%v, want the first removal's %v", active, deactivateTime, writeTimes[0])
		}
		if n := reruns.Load(); n != 0 {
			t.Fatalf("sequential removals reran %d times", n)
		}
	})
}

// A removal that waits on a removal that then rolls back must deactivate the
// child itself: the inactive row it waited on never committed.
func TestRemoveNetworkClientWaitedOnRolledBackRemovalDeactivates(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		network := newRemovalConcurrencyTestNetwork(t, ctx)
		installRemovalConcurrencyTestBarrier(t, ctx)

		var reruns atomic.Int32
		stepErrs, releaseTime := runRemovalConcurrencyTestSteps(t, ctx, func(holderTx server.PgTx) {
			// the holder owns the barrier lock, so its own write passes the trigger
			server.RaisePgResult(holderTx.Exec(
				ctx,
				`UPDATE network_client SET active = false, deactivate_time = clock_timestamp() AT TIME ZONE 'UTC' WHERE client_id = $1`,
				network.childClientId,
			))
		}, false, []func() error{
			removalConcurrencyTestCall(network.session(ctx, &network.parentClientId, &reruns), network.childClientId),
		})
		if stepErrs[0] != nil {
			t.Fatal(stepErrs[0])
		}
		if n := reruns.Load(); n != 0 {
			t.Errorf("the removal reran %d times", n)
		}
		writeTimes := removalConcurrencyTestWrites(t, ctx, network.childClientId)
		if len(writeTimes) != 1 || writeTimes[0].Before(releaseTime) {
			t.Fatalf("lifecycle writes %v, want only the waiting removal's, after the rollback at %v", writeTimes, releaseTime)
		}
		active, deactivateTime := removalConcurrencyTestState(t, ctx, network.childClientId)
		if active || deactivateTime == nil || !deactivateTime.Equal(writeTimes[0]) {
			t.Fatalf("child state active=%t deactivate_time=%v, want the waiting removal's %v", active, deactivateTime, writeTimes[0])
		}
	})
}

// A contract creation holds its client fence (FOR SHARE) on the child. The
// removal waits for it and reads the database clock only after the lock, so the
// deactivation stays ordered after the contract in state and in time.
func TestRemoveNetworkClientWaitedBehindContractFenceUsesPostLockClock(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		network := newRemovalConcurrencyTestNetwork(t, ctx)
		installRemovalConcurrencyTestBarrier(t, ctx)

		var reruns atomic.Int32
		stepErrs, releaseTime := runRemovalConcurrencyTestSteps(t, ctx, func(holderTx server.PgTx) {
			// the contract creation's client fence (lockActiveContractClientsInTx)
			server.RaisePgResult(holderTx.Exec(
				ctx,
				`SELECT client_id, network_id, active FROM network_client WHERE client_id = ANY($1) ORDER BY client_id FOR SHARE`,
				[]server.Id{network.childClientId},
			))
		}, true, []func() error{
			removalConcurrencyTestCall(network.session(ctx, &network.parentClientId, &reruns), network.childClientId),
		})
		if stepErrs[0] != nil {
			t.Fatal(stepErrs[0])
		}
		if n := reruns.Load(); n != 0 {
			t.Errorf("the removal reran %d times behind the contract fence", n)
		}
		writeTimes := removalConcurrencyTestWrites(t, ctx, network.childClientId)
		if len(writeTimes) != 1 || writeTimes[0].Before(releaseTime) {
			t.Fatalf("lifecycle writes %v, want one after the fence release at %v", writeTimes, releaseTime)
		}
	})
}

// Mechanism control. Behind a committed concurrent deactivation, the owned
// removal's row lock fails under a repeatable read snapshot with the
// serialization failure ("could not serialize access due to concurrent
// update") the production log shows. Under read committed it locks the
// committed row and reads it inactive.
func TestOwnedRemovalLockBehindCommittedDeactivationByIsolation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		for _, c := range []struct {
			isolation pgx.TxIsoLevel
			wantCode  string
		}{
			{isolation: pgx.RepeatableRead, wantCode: "40001"},
			{isolation: pgx.ReadCommitted, wantCode: ""},
		} {
			network := newRemovalConcurrencyTestNetwork(t, ctx)
			lockedActive := true
			lockErr := func() error {
				conn := acquireContractLifecycleTestConnection(t, ctx)
				defer conn.Release()
				stepErrs, _ := runRemovalConcurrencyTestSteps(t, ctx, func(holderTx server.PgTx) {
					server.RaisePgResult(holderTx.Exec(
						ctx,
						`UPDATE network_client SET active = false, deactivate_time = clock_timestamp() AT TIME ZONE 'UTC' WHERE client_id = $1`,
						network.childClientId,
					))
				}, true, []func() error{
					func() error {
						tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: c.isolation})
						if err != nil {
							return err
						}
						defer tx.Rollback(context.Background())
						return tx.QueryRow(
							ctx,
							`
								SELECT active
								FROM network_client
								WHERE client_id = $1 AND network_id = $2 AND (client_id = $3 OR source_client_id = $3)
								FOR UPDATE
							`,
							network.childClientId,
							network.networkId,
							network.parentClientId,
						).Scan(&lockedActive)
					},
				})
				return stepErrs[0]
			}()
			var pgErr *pgconn.PgError
			if c.wantCode != "" {
				if !errors.As(lockErr, &pgErr) || pgErr.Code != c.wantCode {
					t.Errorf("%s: lock returned %v, want sqlstate %s", c.isolation, lockErr, c.wantCode)
				}
			} else if lockErr != nil || lockedActive {
				t.Errorf("%s: lock returned %v active=%t, want the committed inactive row", c.isolation, lockErr, lockedActive)
			}
		}
	})
}
