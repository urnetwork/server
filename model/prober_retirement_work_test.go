package model

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Count committed physical row updates through a test-only trigger, including
// updates that write the same active value. Rollback must roll back this audit.
func installProberRetirementTestAudit(t testing.TB, ctx context.Context) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, `
CREATE TABLE test_prober_retirement_updates(client_id uuid NOT NULL);
CREATE FUNCTION test_prober_retirement_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN INSERT INTO test_prober_retirement_updates VALUES(NEW.client_id); RETURN NEW; END $$;
CREATE TRIGGER test_prober_retirement_update AFTER UPDATE OF active,deactivate_time ON network_client
FOR EACH ROW EXECUTE FUNCTION test_prober_retirement_update();`))
	})
}

func proberRetirementTestCount(t testing.TB, ctx context.Context) int {
	t.Helper()
	var count int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM test_prober_retirement_updates`).Scan(&count))
	})
	return count
}

func proberRetirementTestCall(client server.Id, s *session.ClientSession) error {
	_, err := server.HandleError2(func() (struct{}, error) {
		result, err := RetireProberNetworkClient(&RemoveNetworkClientArgs{ClientId: client}, s)
		if err != nil {
			return struct{}{}, err
		}
		if result == nil || result.Error != nil {
			return struct{}{}, fmt.Errorf("internal retirement refused")
		}
		return struct{}{}, nil
	}, func(err error) (struct{}, error) { return struct{}{}, err })
	return err
}

func TestRetireProberNetworkClientDoesNotRewriteCompletedCleanup(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		network, foreignNetwork := server.NewId(), server.NewId()
		s := providerPayoutTestSession(ctx, network)
		clients := make([]server.Id, 64)
		owned := map[server.Id]server.Id{}
		for i := range clients {
			clients[i] = server.NewId()
			owned[clients[i]] = network
		}
		foreign := server.NewId()
		owned[foreign] = foreignNetwork
		insertContractLifecycleTestClients(t, ctx, owned)
		installProberRetirementTestAudit(t, ctx)
		type state struct {
			xmin string
			at   time.Time
		}
		first := map[server.Id]state{}
		for attempt := 0; attempt < 3; attempt++ {
			for _, client := range clients {
				if err := proberRetirementTestCall(client, s); err != nil {
					t.Fatal(err)
				}
			}
			if count := proberRetirementTestCount(t, ctx); count != len(clients) {
				t.Fatalf("attempt%d committed updates=%d want64", attempt, count)
			}
			server.Db(ctx, func(conn server.PgConn) {
				rows, err := conn.Query(ctx, `SELECT client_id,xmin::text,deactivate_time,active FROM network_client WHERE client_id=ANY($1)`, clients)
				server.WithPgResult(rows, err, func() {
					seen := 0
					for rows.Next() {
						var client server.Id
						var got state
						var active bool
						server.Raise(rows.Scan(&client, &got.xmin, &got.at, &active))
						if active || got.at.IsZero() {
							t.Fatal("first retirement did not commit")
						}
						if attempt == 0 {
							first[client] = got
						} else if prior := first[client]; prior.xmin != got.xmin || !prior.at.Equal(got.at) {
							t.Fatal("completed internal cleanup rewrote or extended its row")
						}
						seen++
					}
					if seen != len(clients) {
						t.Fatal("retirement lost an owned client")
					}
				})
			})
		}
		for _, id := range []server.Id{foreign, server.NewId()} {
			result, err := RetireProberNetworkClient(&RemoveNetworkClientArgs{ClientId: id}, s)
			if err != nil || result == nil || result.Error == nil {
				t.Fatal("foreign/missing client became a successful cleanup")
			}
		}
		// Public removal still refreshes the existing row and timestamp.
		result, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: clients[0]}, s)
		if err != nil || result == nil || result.Error != nil || proberRetirementTestCount(t, ctx) != 65 {
			t.Fatal("public repeated-removal semantics changed")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var active bool
			server.Raise(conn.QueryRow(ctx, `SELECT active FROM network_client WHERE client_id=$1`, foreign).Scan(&active))
			if !active {
				t.Fatal("foreign client changed")
			}
		})
	})
}

func TestRetireProberNetworkClientWaitedDuplicatesUseOnePostLockClock(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		network, client := server.NewId(), server.NewId()
		s := providerPayoutTestSession(ctx, network)
		insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{client: network})
		installProberRetirementTestAudit(t, ctx)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		holder, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer holder.Rollback(context.Background())
		var id server.Id
		server.Raise(holder.QueryRow(ctx, `SELECT client_id FROM network_client WHERE client_id=$1 FOR SHARE`, client).Scan(&id))
		pid := contractLifecycleTestBackendPid(t, ctx, holder)
		done := make(chan error, 2)
		launched, joined := 0, 0
		defer func() {
			cancel()
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = holder.Rollback(cleanup)
			for joined < launched {
				select {
				case <-done:
					joined++
				case <-cleanup.Done():
					t.Error("retirement workers did not join")
					return
				}
			}
		}()
		launched++
		go func() { done <- proberRetirementTestCall(client, s) }()
		first := requireContractLifecycleBlockedBy(t, ctx, holder, pid)
		launched++
		go func() { done <- proberRetirementTestCall(client, s) }()
		for {
			var second int32
			server.Raise(holder.QueryRow(ctx, `SELECT coalesce(min(pid),0) FROM pg_locks
			 WHERE NOT granted AND pid<>$1 AND ($2=ANY(pg_blocking_pids(pid)) OR $1=ANY(pg_blocking_pids(pid)))`, first, pid).Scan(&second))
			if second != 0 {
				break
			}
			if ctx.Err() != nil {
				t.Fatal("second actual retirement wait not observed", ctx.Err())
			}
			runtime.Gosched()
		}
		var release time.Time
		server.Raise(holder.QueryRow(ctx, `SELECT clock_timestamp() AT TIME ZONE 'UTC'`).Scan(&release))
		server.Raise(holder.Commit(ctx))
		for joined < 2 {
			select {
			case err := <-done:
				joined++
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		if proberRetirementTestCount(t, ctx) != 1 {
			t.Fatal("concurrent completed cleanups rewrote the row")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var at time.Time
			var active bool
			server.Raise(conn.QueryRow(ctx, `SELECT active,deactivate_time FROM network_client WHERE client_id=$1`, client).Scan(&active, &at))
			if active || at.Before(release) {
				t.Fatal("retirement clock preceded the actual held admission lock")
			}
		})
	})
}

func TestRetireProberNetworkClientRollbackLeavesCleanupRetryable(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		network, client := server.NewId(), server.NewId()
		s := providerPayoutTestSession(ctx, network)
		insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{client: network})
		installProberRetirementTestAudit(t, ctx)
		server.Db(ctx, func(conn server.PgConn) {
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			server.Raise(err)
			defer tx.Rollback(context.Background())
			matched, err := retireProberNetworkClientInTx(ctx, tx, client, network)
			if err != nil || !matched {
				t.Fatal("initial cleanup failed", err)
			}
			server.Raise(tx.Rollback(ctx))
			var active bool
			var at *time.Time
			server.Raise(conn.QueryRow(ctx, `SELECT active,deactivate_time FROM network_client WHERE client_id=$1`, client).Scan(&active, &at))
			if !active || at != nil {
				t.Fatal("rolled back cleanup published state")
			}
		})
		if proberRetirementTestCount(t, ctx) != 0 {
			t.Fatal("rollback retained physical update audit")
		}
		if err := proberRetirementTestCall(client, s); err != nil || proberRetirementTestCount(t, ctx) != 1 {
			t.Fatal("retry lost real retirement", err)
		}
	})
}

func TestProberShardDrainDoesNotWaitOnOrRewriteRetiredChildren(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		retired, live := server.NewId(), server.NewId()
		insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{retired: owner.NetworkId, live: owner.NetworkId})
		oldTime := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false,deactivate_time=$2 WHERE client_id=$1`, retired, oldTime))
		})
		installProberRetirementTestAudit(t, ctx)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		holder, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer holder.Rollback(context.Background())
		var id server.Id
		server.Raise(holder.QueryRow(ctx, `SELECT client_id FROM network_client WHERE client_id=$1 FOR UPDATE`, retired).Scan(&id))
		pid := contractLifecycleTestBackendPid(t, ctx, holder)
		done := make(chan error, 1)
		joined := false
		go func() { done <- DrainProberShard(ctx, owner.Key) }()
		defer func() {
			cancel()
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = holder.Rollback(cleanup)
			if !joined {
				select {
				case <-done:
				case <-cleanup.Done():
					t.Error("drain worker did not join")
				}
			}
		}()
	wait:
		for {
			select {
			case err := <-done:
				joined = true
				if err != nil {
					t.Fatal(err)
				}
				break wait
			default:
			}
			var waiters int
			server.Raise(holder.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE NOT granted AND $1=ANY(pg_blocking_pids(pid))`, pid).Scan(&waiters))
			if waiters != 0 {
				t.Fatal("drain waited on an already retired client")
			}
			if ctx.Err() != nil {
				t.Fatal(ctx.Err())
			}
			runtime.Gosched()
		}
		// The old row remains locked while both live endpoints have committed.
		if proberRetirementTestCount(t, ctx) != 2 {
			t.Fatal("drain rewrote retired children or missed a live endpoint")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var at time.Time
			var inactive, clocks int
			server.Raise(conn.QueryRow(ctx, `SELECT deactivate_time FROM network_client WHERE client_id=$1`, retired).Scan(&at))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FILTER(WHERE NOT active),count(DISTINCT deactivate_time) FROM network_client WHERE client_id=ANY($1)`, []server.Id{owner.ClientId, live}).Scan(&inactive, &clocks))
			if !at.Equal(oldTime) || inactive != 2 || clocks != 1 {
				t.Fatal("drain changed retired timestamp or split its live clock")
			}
		})
		server.Raise(holder.Rollback(ctx))
		if err := DrainProberShard(ctx, owner.Key); err != nil || proberRetirementTestCount(t, ctx) != 2 {
			t.Fatal("drain replay rewrote its completed clients", err)
		}
	})
}
