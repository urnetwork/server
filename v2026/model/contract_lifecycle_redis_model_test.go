package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Actual public Redis admission is held at its endpoint row. The database
// clock must be read after release, and cancellation/deactivation/expiry must
// leave no durable contract or escrow. An abandoned Redis token may retain
// temporary debt under the explicitly accepted approximate admission policy.
func TestRedisContractLifecycleClockAndRejectionAfterEndpointWait(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, scenario := range []string{"healthy", "expired", "inactive", "canceled"} {
			func() {
				ctx, stop := context.WithTimeout(t.Context(), 15*time.Second)
				defer stop()
				owner := shardTestOwner(t, ctx, shardTestKey(0))
				peer := newEscrowSelectionTestClients(t, ctx)
				var deadline time.Time
				if scenario == "expired" {
					server.Tx(ctx, func(tx server.PgTx) {
						server.Raise(tx.QueryRow(ctx, `UPDATE prober_shard_run SET deadline=(clock_timestamp() AT TIME ZONE 'UTC')+interval '1 second' WHERE network_id=$1 RETURNING deadline`, owner.NetworkId).Scan(&deadline))
					})
				}
				conn := acquireContractLifecycleTestConnection(t, ctx)
				defer conn.Release()
				held, err := conn.Begin(ctx)
				server.Raise(err)
				defer held.Rollback(context.Background())
				server.RaisePgResult(held.Exec(ctx, `SELECT 1 FROM network_client WHERE client_id=$1 FOR UPDATE`, peer.providerId))
				holder := contractLifecycleTestBackendPid(t, ctx, held)
				request, cancel := context.WithCancel(ctx)
				defer cancel()
				type result struct {
					escrow     *TransferEscrow
					err        error
					panicValue any
				}
				done := make(chan result, 1)
				joined := make(chan struct{})
				go func() {
					defer close(joined)
					got := result{}
					got.panicValue = captureShardQueryPanic(func() {
						got.escrow, got.err = CreateTransferEscrow(request, owner.NetworkId, owner.ClientId, peer.providerNetworkId, peer.providerId, 17)
					})
					done <- got
				}()
				defer func() { cancel(); _ = held.Rollback(context.Background()); <-joined }()
				requireContractLifecycleBlockedBy(t, ctx, held, holder)
				switch scenario {
				case "expired":
					server.RaisePgResult(held.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(epoch FROM ($1::timestamp-(clock_timestamp() AT TIME ZONE 'UTC')))))`, deadline))
				case "inactive":
					server.RaisePgResult(held.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, peer.providerId))
				case "canceled":
					cancel()
				}
				var beforeRelease time.Time
				server.Raise(held.QueryRow(ctx, `SELECT clock_timestamp() AT TIME ZONE 'UTC'`).Scan(&beforeRelease))
				server.Raise(held.Commit(ctx))
				got := <-done
				if got.panicValue != nil {
					if e, ok := got.panicValue.(error); ok {
						got.err = e
					} else {
						t.Fatalf("%s unexpected panic %T", scenario, got.panicValue)
					}
				}
				var contracts, escrows int
				server.Db(ctx, func(c server.PgConn) {
					server.Raise(c.QueryRow(ctx, `SELECT (SELECT count(*) FROM transfer_contract WHERE source_id=$1),(SELECT count(*) FROM transfer_escrow WHERE balance_id=$2)`, owner.ClientId, owner.BalanceId).Scan(&contracts, &escrows))
				})
				if scenario == "healthy" {
					if got.err != nil || got.escrow == nil || contracts != 1 || escrows != 1 {
						t.Fatalf("healthy writer contracts=%d escrows=%d error=%v", contracts, escrows, got.err)
					}
					var created time.Time
					var marked bool
					server.Db(ctx, func(c server.PgConn) {
						server.Raise(c.QueryRow(ctx, `SELECT c.create_time,e.redis_reserved FROM transfer_contract c JOIN transfer_escrow e USING(contract_id) WHERE c.contract_id=$1`, got.escrow.ContractId).Scan(&created, &marked))
					})
					if created.Before(beforeRelease) || !marked {
						t.Fatal("Redis clock/marker preceded endpoint lock")
					}
				} else {
					if got.err == nil || got.escrow != nil || contracts != 0 || escrows != 0 {
						t.Fatalf("%s admitted stale writer: contracts=%d escrows=%d error=%v", scenario, contracts, escrows, got.err)
					}
					if scenario == "expired" && !errors.Is(got.err, ErrProberShardRetired) {
						t.Fatal(got.err)
					}
					if scenario == "canceled" && !errors.Is(got.err, context.Canceled) && !server.IsDoneError(got.err) {
						t.Fatal(got.err)
					}
				}
			}()
		}
	})
}
