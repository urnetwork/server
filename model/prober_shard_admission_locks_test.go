package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

type shardAdmissionTestResult struct {
	grantWaitAdmissionResult
	reachedGrantBeforeReturn bool
}

// Keep the actual server transaction identity: financial ownership deliberately
// refuses wrappers that could borrow an outer transaction's admitted keys.
func startShardAdmissionForTest(ctx context.Context, owner *ProberShardOwner, peer escrowSelectionTestClients) (<-chan shardAdmissionTestResult, func()) {
	ctx, cancel := context.WithCancel(ctx)
	result := make(chan shardAdmissionTestResult, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		value := shardAdmissionTestResult{}
		panicErr := server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				var posts []func() any
				value.escrow, posts, value.err = createTransferEscrowInTx(ctx, tx,
					owner.NetworkId, owner.ClientId, peer.providerNetworkId, peer.providerId,
					owner.NetworkId, 1024, nil)
				value.posts = len(posts)
				// Observe before COMMIT releases this backend's relation locks.
				// The registry-refusal path must never reach grant discovery,
				// including after the external registry holder is released.
				server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
					WHERE pid=pg_backend_pid() AND relation='transfer_balance'::regclass AND granted)`).
					Scan(&value.reachedGrantBeforeReturn))
			}, server.TxReadCommitted, server.OptNoRetry())
		})
		if panicErr != nil {
			value.err = fmt.Errorf("admission panic: %v", panicErr)
		}
		result <- value
	}()
	return result, func() { cancel(); <-done }
}

func requireShardAdmissionNoWrites(t testing.TB, ctx context.Context, owner *ProberShardOwner, priorRevision int64) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var contracts, escrows int
		var revision int64
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM transfer_contract WHERE payer_network_id=$1),
			(SELECT count(*) FROM transfer_escrow WHERE balance_id=$2),
			COALESCE((SELECT revision FROM transfer_balance_net_escrow_revision WHERE balance_id=$2),0)`,
			owner.NetworkId, owner.BalanceId).Scan(&contracts, &escrows, &revision))
		if contracts != 0 || escrows != 0 || revision != priorRevision {
			t.Fatalf("rejected admission changed financial state: contracts=%d escrows=%d revision=%d prior=%d",
				contracts, escrows, revision, priorRevision)
		}
	})
}

// While admission owns the registry but waits for its grant, drain must wait
// at that registry. It cannot take the client locks that admission needs next.
func TestProberShardGrantWaitFencesDrainBeforeClientLocks(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		peer := newEscrowSelectionTestClients(t, ctx)
		clients := escrowSelectionTestClients{payerNetworkId: owner.NetworkId, payerId: owner.ClientId,
			providerNetworkId: peer.providerNetworkId, providerId: peer.providerId}
		held, heldPid, result, stop := startGrantWaitAdmission(t, ctx, clients, owner.BalanceId)
		defer stop()
		admissionPid := requireContractLifecycleBlockedBy(t, ctx, held, heldPid)
		drainCtx, cancelDrain := context.WithCancel(ctx)
		drainResult := make(chan error, 1)
		drainDone := make(chan struct{})
		go func() { defer close(drainDone); drainResult <- DrainProberShard(drainCtx, owner.Key) }()
		defer func() { cancelDrain(); _ = held.Rollback(context.Background()); <-drainDone }()
		drainPid := requireContractLifecycleBlockedBy(t, ctx, held, admissionPid)
		var prematureClients bool
		server.Raise(held.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
			WHERE pid=$1 AND relation='network_client'::regclass)`, drainPid).Scan(&prematureClients))
		if prematureClients {
			t.Fatal("drain reached client locks while admission was queued on its grant")
		}
		server.Raise(held.Commit(ctx))
		got := <-result
		if got.err != nil || got.escrow == nil || got.posts != 1 {
			t.Fatalf("admission-first failed: %+v", got)
		}
		if err := <-drainResult; err != nil {
			t.Fatal(err)
		}
		if reserved := openEscrowReservedForBalances(ctx, []server.Id{owner.BalanceId})[owner.BalanceId].reserved; reserved != 1024 {
			t.Fatalf("drain changed admitted debt to %d", reserved)
		}
	})
}

// A drain-first transaction changes ownership and clients while retaining its
// locks. Admission must wait at the registry and reject without reading grants.
func TestProberShardDrainFirstRejectsBeforeGrantRead(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		peer := newEscrowSelectionTestClients(t, ctx)
		held, finish := beginRareGrantTestTx(t, ctx)
		defer finish(false)
		server.RaisePgResult(held.Exec(ctx, `SELECT 1 FROM prober_shard_run WHERE network_id=$1 FOR UPDATE`, owner.NetworkId))
		server.Raise(drainProberShardInTx(ctx, held, owner))
		var priorRevision int64
		server.Raise(held.QueryRow(ctx, `SELECT COALESCE((SELECT revision FROM transfer_balance_net_escrow_revision WHERE balance_id=$1),0)`, owner.BalanceId).Scan(&priorRevision))
		pid := contractLifecycleTestBackendPid(t, ctx, held)
		result, stop := startShardAdmissionForTest(ctx, owner, peer)
		defer stop()
		admissionPid := requireContractLifecycleBlockedBy(t, ctx, held, pid)
		var prematureGrant bool
		server.Raise(held.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
			WHERE pid=$1 AND relation='transfer_balance'::regclass)`, admissionPid).Scan(&prematureGrant))
		if prematureGrant {
			t.Fatal("drain-first admission reached the financial table before registry ownership")
		}
		server.Raise(finish(true))
		got := <-result
		if got.err == nil || got.escrow != nil || got.posts != 0 || got.reachedGrantBeforeReturn {
			t.Fatalf("drain-first admission crossed ownership: escrow=%v posts=%d grant=%t err=%v", got.escrow, got.posts, got.reachedGrantBeforeReturn, got.err)
		}
		requireShardAdmissionNoWrites(t, ctx, owner, priorRevision)
	})
}

// PostgreSQL's clock advances while a row is unchanged and exclusively locked,
// while a census runs, or while admission waits for an endpoint. These explicit
// barriers exercise each deadline recheck without changing the registry row.
func TestProberShardAdmissionRechecksDeadlineAfterWaits(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, boundary := range []string{"registry", "census", "client"} {
			func() {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				owner := shardTestOwner(t, ctx, shardTestKey(0))
				peer := newEscrowSelectionTestClients(t, ctx)
				var deadline time.Time
				var priorRevision int64
				server.Tx(ctx, func(tx server.PgTx) {
					server.Raise(tx.QueryRow(ctx, `UPDATE prober_shard_run
						SET deadline=(clock_timestamp() AT TIME ZONE 'UTC')+interval '2 seconds'
						WHERE network_id=$1 RETURNING deadline`, owner.NetworkId).Scan(&deadline))
					server.Raise(tx.QueryRow(ctx, `SELECT COALESCE((SELECT revision FROM transfer_balance_net_escrow_revision WHERE balance_id=$1),0)`, owner.BalanceId).Scan(&priorRevision))
				})
				waitForDeadline := func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `SELECT pg_sleep(GREATEST(0,
						EXTRACT(EPOCH FROM ($1::timestamp-(clock_timestamp() AT TIME ZONE 'UTC')))))`, deadline))
				}
				conn := acquireContractLifecycleTestConnection(t, ctx)
				defer conn.Release()
				held, err := conn.Begin(ctx)
				server.Raise(err)
				defer held.Rollback(context.Background())
				pid := contractLifecycleTestBackendPid(t, ctx, held)
				switch boundary {
				case "registry":
					server.RaisePgResult(held.Exec(ctx, `SELECT 1 FROM prober_shard_run WHERE network_id=$1 FOR UPDATE`, owner.NetworkId))
				case "client":
					server.RaisePgResult(held.Exec(ctx, `SELECT 1 FROM network_client WHERE client_id=$1 FOR UPDATE`, peer.providerId))
				case "census":
					// A new shard has no cached reservation snapshot. Block its
					// actual census after grant ownership, without wrapping tx.
					server.RaisePgResult(held.Exec(ctx, `LOCK TABLE transfer_escrow IN ACCESS EXCLUSIVE MODE`))
				}
				result, stop := startShardAdmissionForTest(ctx, owner, peer)
				defer stop()
				admissionPid := requireContractLifecycleBlockedBy(t, ctx, held, pid)
				var reachedGrant, waitingCensus, beforeDeadline bool
				server.Raise(held.QueryRow(ctx, `SELECT
					EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND relation='transfer_balance'::regclass AND granted),
					EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND relation='transfer_escrow'::regclass
						AND mode='AccessShareLock' AND NOT granted),
					(clock_timestamp() AT TIME ZONE 'UTC') < $2::timestamp`, admissionPid, deadline).
					Scan(&reachedGrant, &waitingCensus, &beforeDeadline))
				if reachedGrant != (boundary != "registry") || waitingCensus != (boundary == "census") {
					t.Fatalf("%s did not reach its actual database boundary: grant=%t census=%t", boundary, reachedGrant, waitingCensus)
				}
				if !beforeDeadline {
					t.Fatalf("%s barrier was not established before the database deadline", boundary)
				}
				waitForDeadline(held)
				server.Raise(held.Commit(ctx))
				got := <-result
				if !errors.Is(got.err, ErrProberShardRetired) || got.escrow != nil || got.posts != 0 {
					t.Fatalf("%s wait crossed the database deadline: escrow=%v posts=%d err=%v", boundary, got.escrow, got.posts, got.err)
				}
				if got.reachedGrantBeforeReturn != (boundary != "registry") {
					t.Fatalf("%s did not retain its callback financial boundary: grant=%t", boundary, got.reachedGrantBeforeReturn)
				}
				requireShardAdmissionNoWrites(t, ctx, owner, priorRevision)
				t.Logf("%s wait rejected after expiry with no financial writes", boundary)
			}()
		}
	})
}
