package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// An unchanged registry row still expires while admission waits. Use the real
// connection owner and exact database lock edges, then advance only the clock.
func exerciseProberShardConnectionDeadlineWait(t testing.TB, boundary string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	owner := shardTestOwner(t, ctx, shardTestKey(0))
	handler := CreateNetworkClientHandler(ctx)
	var deadline time.Time
	server.Tx(ctx, func(tx server.PgTx) {
		server.Raise(tx.QueryRow(ctx, `UPDATE prober_shard_run
			SET deadline=(clock_timestamp() AT TIME ZONE 'UTC')+interval '5 seconds'
			WHERE network_id=$1 RETURNING deadline`, owner.NetworkId).Scan(&deadline))
	})
	holderConn := acquireContractLifecycleTestConnection(t, ctx)
	defer holderConn.Release()
	held, err := holderConn.Begin(ctx)
	server.Raise(err)
	defer rollbackCloseReportTestTransaction(ctx, held)
	heldPid := contractLifecycleTestBackendPid(t, ctx, held)
	switch boundary {
	case "registry":
		server.RaisePgResult(held.Exec(ctx, `SELECT 1 FROM prober_shard_run WHERE network_id=$1 FOR UPDATE`, owner.NetworkId))
	case "endpoint":
		providerWorkLockSessionMutationInTx(ctx, held, owner.ClientId)
	default:
		t.Fatal("unknown connection admission barrier")
	}
	admissionCtx, stopAdmission := context.WithCancel(ctx)
	result := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		var admissionErr error
		server.HandleError(func() {
			_, _, _, _, admissionErr = ConnectNetworkClientWithIpFamily(admissionCtx,
				owner.ClientId, "192.0.2.21:10011", handler, 4, owner.NetworkId)
		}, func(err error) { admissionErr = err })
		result <- admissionErr
	}()
	defer func() {
		stopAdmission()
		rollbackCloseReportTestTransaction(ctx, held)
		select {
		case <-joined:
		case <-time.After(35 * time.Second):
			t.Error("connection admission did not join cleanup")
		}
	}()
	if boundary == "endpoint" {
		requireProviderWorkConnectionWaiters(t, ctx, held, owner.ClientId, 1)
	} else {
		requireContractLifecycleBlockedBy(t, ctx, held, heldPid)
	}
	server.RaisePgResult(held.Exec(ctx, `SELECT pg_sleep(GREATEST(0,
		EXTRACT(EPOCH FROM ($1::timestamp-(clock_timestamp() AT TIME ZONE 'UTC')))))`, deadline))
	server.Raise(held.Commit(ctx))
	select {
	case err := <-result:
		if !errors.Is(err, ErrProberShardRetired) {
			t.Fatalf("%s wait admitted after the database deadline: %v", boundary, err)
		}
	case <-ctx.Done():
		t.Fatal("connection admission did not finish after the deadline barrier", ctx.Err())
	}
	server.Db(ctx, func(conn server.PgConn) {
		var connections, heads, events, originals int
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM network_client_connection WHERE client_id=$1),
			(SELECT count(*) FROM provider_work_session_head WHERE client_id=$1),
			(SELECT count(*) FROM provider_work_session_event WHERE client_id=$1),
			(SELECT count(*) FROM provider_work_session_receipt WHERE client_id=$1)`, owner.ClientId).
			Scan(&connections, &heads, &events, &originals))
		if connections != 0 || heads != 0 || events != 0 || originals != 0 {
			t.Fatalf("refused connection wrote connection/head/event/original=%d/%d/%d/%d",
				connections, heads, events, originals)
		}
	})
}

func TestProberShardConnectionRechecksDeadlineAfterRegistryWait(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseProberShardConnectionDeadlineWait(t, "registry") })
}

func TestProberShardConnectionRechecksDeadlineAfterEndpointWait(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseProberShardConnectionDeadlineWait(t, "endpoint") })
}
