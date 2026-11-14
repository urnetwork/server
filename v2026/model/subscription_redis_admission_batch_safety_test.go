// Held PostgreSQL owners prove batch snapshots and post-wait clock checks.
package model

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

type redisBatchAdmissionResult struct {
	escrow *TransferEscrow
	err    error
}

// Run the actual reserved creation in an independent, joined transaction owner.
func startRedisBatchAdmission(ctx context.Context, f netEscrowOrderingTestFixture, options ...any) (<-chan redisBatchAdmissionResult, func()) {
	ctx, cancel := context.WithCancel(ctx)
	result := make(chan redisBatchAdmissionResult, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		got := redisBatchAdmissionResult{}
		panicErr := redisPublicationHandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				got.escrow, _, got.err = createTransferEscrowInTx(ctx, tx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 23, nil)
			}, append([]any{server.TxReadCommitted, server.OptNoRetry()}, options...)...)
		})
		got.err = errors.Join(got.err, panicErr)
		result <- got
	}()
	return result, func() { cancel(); <-done }
}

// The existence statement must see a publisher that commits while this
// request waits for its fence, even when both statements share one batch.
func TestRedisAdmissionBatchCustodySnapshotFollowsRequestFence(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, f, 17)
		owned := withRedisContractAdmission(ctx)
		owner := redisAdmissionFromContext(owned)
		reserved, finishPending := reserveRedisAdmissionTestBeforeTx(t, owned, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 23)
		defer finishPending()
		held, finish := beginRareGrantTestTx(t, ctx)
		defer finish(false)
		server.RaisePgResult(held.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, redisContractAdmissionLock(owner.contractId)))
		heldPid := contractLifecycleTestBackendPid(t, ctx, held)
		result, stop := startRedisBatchAdmission(reserved, f)
		defer stop()
		requireContractLifecycleBlockedBy(t, ctx, held, heldPid)
		server.RaisePgResult(held.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count,redis_reserved) VALUES($1,$2,23,true)`, owner.contractId, f.balanceId))
		server.RaisePgResult(held.Exec(ctx, `INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,expiration_time) VALUES($1,$2,$3,$4,$5,$2,23,$6)`, owner.contractId, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, server.NowUtc().Add(time.Hour)))
		server.Raise(finish(true))
		got := <-result
		if got.escrow != nil || got.err == nil || !strings.Contains(got.err.Error(), "already has SQL custody") {
			t.Fatal("custody read did not follow the joined publisher", got.escrow, got.err)
		}
		finishPending()
		if err := recoverRedisReservationRequest(ctx, f.balanceId, owner.contractId); err != nil {
			t.Fatal(err)
		}
		requireRedisPublicationCustody(t, ctx, f, neighbor.ContractId, owner.contractId, 40, 2)
	})
}

// Grant freshness can wait after endpoint validation. Its database clock
// must still reject an expired shard before any contract or escrow is written.
func TestRedisAdmissionBatchRechecksShardDeadlineAfterGrantWait(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		shard := shardTestOwner(t, ctx, shardTestKey(0))
		peer := newEscrowSelectionTestClients(t, ctx)
		f := netEscrowOrderingTestFixture{sourceNetworkId: shard.NetworkId, sourceId: shard.ClientId, destinationNetworkId: peer.providerNetworkId, destinationId: peer.providerId, balanceId: shard.BalanceId}
		owned := withRedisContractAdmission(ctx)
		owner := redisAdmissionFromContext(owned)
		reserved, finishPending := reserveRedisAdmissionTestBeforeTx(t, owned, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 23)
		defer finishPending()
		// Warm both historical/current grant statements on the exact borrowed
		// connection before BEGIN. Otherwise batch preparation could wait on
		// the relation before its first clock, hiding the post-clock boundary.
		warmed, begin := make(chan struct{}, 1), make(chan struct{})
		beforeBegin := server.TxReadBeforeBegin(func(query server.PgCanQuery) {
			var current bool
			var observed time.Time
			rows, err := query.Query(ctx, `SELECT count(*)=$2 FROM transfer_balance
		WHERE balance_id=ANY($1) AND active AND start_time<=clock_timestamp() AT TIME ZONE 'UTC'
		AND clock_timestamp() AT TIME ZONE 'UTC'<end_time`, []server.Id{f.balanceId}, 1)
			server.WithPgResult(rows, err, func() {
				if rows.Next() {
					server.Raise(rows.Scan(&current))
				}
			})
			rows, err = query.Query(ctx, `SELECT count(*)=$2,clock_timestamp() AT TIME ZONE 'UTC' FROM transfer_balance WHERE balance_id=ANY($1) AND active AND start_time<=clock_timestamp() AT TIME ZONE 'UTC' AND clock_timestamp() AT TIME ZONE 'UTC'<end_time`, []server.Id{f.balanceId}, 1)
			server.WithPgResult(rows, err, func() {
				if rows.Next() {
					server.Raise(rows.Scan(&current, &observed))
				}
			})
			warmed <- struct{}{}
			select {
			case <-begin:
			case <-ctx.Done():
				server.Raise(ctx.Err())
			}
		})
		result, stop := startRedisBatchAdmission(reserved, f, beforeBegin)
		defer stop()
		defer close(begin)
		select {
		case <-warmed:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		// This held transaction belongs to beginRareGrantTestTx's independent
		// goroutine. The test's setup transaction joins before admission starts.
		held, finish := beginRareGrantTestTx(t, ctx)
		defer finish(false)
		var revision int64
		server.Raise(held.QueryRow(ctx, `SELECT COALESCE((SELECT revision FROM transfer_balance_net_escrow_revision WHERE balance_id=$1),0)`, shard.BalanceId).Scan(&revision))
		server.RaisePgResult(held.Exec(ctx, `LOCK TABLE transfer_balance IN ACCESS EXCLUSIVE MODE`))
		var deadline time.Time
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(tx.QueryRow(ctx, `UPDATE prober_shard_run SET deadline=(clock_timestamp() AT TIME ZONE 'UTC')+make_interval(secs=>$2) WHERE network_id=$1 RETURNING deadline`, shard.NetworkId, proberShardDeadlineBarrierWindow.Seconds()).Scan(&deadline))
		}, server.TxReadCommitted, server.OptNoRetry())
		heldPid := contractLifecycleTestBackendPid(t, ctx, held)
		begin <- struct{}{}
		requireContractLifecycleBlockedBy(t, ctx, held, heldPid)
		server.RaisePgResult(held.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamp-(clock_timestamp() AT TIME ZONE 'UTC')))))`, deadline))
		server.Raise(finish(true))
		got := <-result
		if got.escrow != nil || !errors.Is(got.err, ErrProberShardRetired) {
			t.Fatal("grant wait crossed shard deadline without refusal", got.escrow, got.err)
		}
		finishPending()
		if err := owner.compensate(ctx); err != nil {
			t.Fatal(err)
		}
		requireShardAdmissionNoWrites(t, ctx, shard, revision)
		if Testing_NetEscrowByteCount(ctx, shard.BalanceId) != 0 {
			t.Fatal("deadline refusal retained its pending reservation")
		}
	})
}
