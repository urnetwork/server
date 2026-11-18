package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

type grantWaitAdmissionResult struct {
	escrow *TransferEscrow
	posts  int
	err    error
}

// A real held grant establishes the queue. The caller retains the barrier
// until its unrelated client operation has completed or failed explicitly.
func startGrantWaitAdmission(t testing.TB, ctx context.Context, clients escrowSelectionTestClients, balanceId server.Id) (
	server.PgTx, int32, <-chan grantWaitAdmissionResult, func(),
) {
	t.Helper()
	conn := acquireContractLifecycleTestConnection(t, ctx)
	held, err := conn.Begin(ctx)
	server.Raise(err)
	server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, balanceId))
	heldPid := contractLifecycleTestBackendPid(t, ctx, held)
	createCtx, cancel := context.WithCancel(ctx)
	result := make(chan grantWaitAdmissionResult, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		value := grantWaitAdmissionResult{}
		panicErr := server.HandleError(func() {
			server.Tx(createCtx, func(tx server.PgTx) {
				var posts []func() any
				value.escrow, posts, value.err = createTransferEscrowInTx(createCtx, tx,
					clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId,
					clients.payerNetworkId, 1024, nil)
				value.posts = len(posts)
			}, server.TxReadCommitted, server.OptNoRetry())
		})
		if panicErr != nil {
			value.err = fmt.Errorf("admission panic: %v", panicErr)
		}
		result <- value
	}()
	stop := func() {
		cancel()
		_ = held.Rollback(context.Background())
		<-done
		conn.Release()
	}
	return held, heldPid, result, stop
}

func TestEscrowGrantWaitDoesNotBlockClientAuthRefresh(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		clients := newEscrowSelectionTestClients(t, ctx)
		now := server.NowUtc()
		grant := addDynamicProberGrantForTest(ctx, clients.payerNetworkId, now.Add(-time.Hour), now.Add(time.Hour), 4096)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET auth_time=now()-interval '2 hours' WHERE client_id=ANY($1)`, []server.Id{clients.payerId, clients.providerId}))
		})
		held, heldPid, result, stop := startGrantWaitAdmission(t, ctx, clients, grant.BalanceId)
		defer stop()
		requireContractLifecycleBlockedBy(t, ctx, held, heldPid)
		// This is the exact throttled write in ConnectNetworkClient. A local
		// lock timeout makes the original fanout fail without a timing guess.
		refreshErr := server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout='100ms'`))
				for _, clientId := range []server.Id{clients.payerId, clients.providerId} {
					tag := server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET auth_time=$2 WHERE client_id=$1 AND auth_time<$3`,
						clientId, now, now.Add(-clientAuthTimeRefreshMinInterval)))
					if tag.RowsAffected() != 1 {
						t.Fatal("stale connection activity marker was not refreshed")
					}
				}
			}, server.TxReadCommitted, server.OptNoRetry())
		})
		if refreshErr != nil {
			t.Fatalf("grant waiter blocked connection activity refresh: %v", refreshErr)
		}
		select {
		case got := <-result:
			t.Fatalf("admission crossed held financial lock: %+v", got)
		default:
		}
		server.Raise(held.Commit(ctx))
		got := <-result
		if got.err != nil || got.escrow == nil || got.posts != 1 {
			t.Fatalf("admission after refresh=%+v", got)
		}
		if reserved := openEscrowReservedForBalances(ctx, []server.Id{grant.BalanceId})[grant.BalanceId].reserved; reserved != 1024 {
			t.Fatalf("reserved=%d, want 1024", reserved)
		}
	})
}

func TestEscrowGrantWaitRechecksBothClientLifecyclesBeforeWrites(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, source := range []bool{false, true} {
			func() {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				clients := newEscrowSelectionTestClients(t, ctx)
				now := server.NowUtc()
				grant := addDynamicProberGrantForTest(ctx, clients.payerNetworkId, now.Add(-time.Hour), now.Add(time.Hour), 4096)
				held, heldPid, result, stop := startGrantWaitAdmission(t, ctx, clients, grant.BalanceId)
				defer stop()
				requireContractLifecycleBlockedBy(t, ctx, held, heldPid)
				clientId, networkId, wantErr := clients.providerId, clients.providerNetworkId, ErrContractDestinationInactive
				if source {
					clientId, networkId, wantErr = clients.payerId, clients.payerNetworkId, ErrActiveClientNotFound
				}
				deactivateErr := server.HandleError(func() {
					server.Tx(ctx, func(tx server.PgTx) {
						server.RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout='100ms'`))
						_, err := deactivateNetworkClientsInTx(ctx, tx, []server.Id{clientId}, networkId)
						server.Raise(err)
					}, server.TxReadCommitted, server.OptNoRetry())
				})
				if deactivateErr != nil {
					t.Fatalf("queued grant admission blocked client deactivation: %v", deactivateErr)
				}
				server.Raise(held.Commit(ctx))
				got := <-result
				if !errors.Is(got.err, wantErr) || got.escrow != nil || got.posts != 0 {
					t.Fatalf("late inactive endpoint escaped write fence: %+v", got)
				}
				if count := contractLifecycleTestCount(t, ctx, clients.payerId, clients.providerId); count != 0 {
					t.Fatalf("inactive endpoint left %d contracts", count)
				}
				server.Db(ctx, func(conn server.PgConn) {
					var count int
					server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_escrow WHERE balance_id=$1`, grant.BalanceId).Scan(&count))
					if count != 0 {
						t.Fatalf("inactive endpoint left %d escrow rows", count)
					}
				})
			}()
		}
	})
}

// A client lock can outlive a grant's eligibility even though no transaction
// updates that grant. Admission must choose the next unexpired locked grant.
func TestEscrowClientWaitRechecksGrantExpiry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		clients := newEscrowSelectionTestClients(t, ctx)
		now := server.NowUtc()
		first := addDynamicProberGrantForTest(ctx, clients.payerNetworkId, now.Add(-time.Hour), now.Add(time.Hour), 4096)
		second := addDynamicProberGrantForTest(ctx, clients.payerNetworkId, now.Add(-time.Hour), now.Add(2*time.Hour), 4096)
		var deadline time.Time
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(tx.QueryRow(ctx, `UPDATE transfer_balance
				SET end_time=(clock_timestamp() AT TIME ZONE 'UTC')+interval '2 seconds'
				WHERE balance_id=$1 RETURNING end_time`, first.BalanceId).Scan(&deadline))
		})
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT 1 FROM network_client WHERE client_id=$1 FOR UPDATE`, clients.providerId))
		pid := contractLifecycleTestBackendPid(t, ctx, held)
		result := make(chan grantWaitAdmissionResult, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			value := grantWaitAdmissionResult{}
			panicErr := server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					var posts []func() any
					value.escrow, posts, value.err = createTransferEscrowInTx(ctx, tx,
						clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId,
						clients.payerNetworkId, 1024, nil)
					value.posts = len(posts)
				}, server.TxReadCommitted, server.OptNoRetry())
			})
			if panicErr != nil {
				value.err = fmt.Errorf("admission panic: %v", panicErr)
			}
			result <- value
		}()
		defer func() { cancel(); _ = held.Rollback(context.Background()); <-done }()
		requireContractLifecycleBlockedBy(t, ctx, held, pid)
		server.RaisePgResult(held.Exec(ctx, `SELECT pg_sleep(GREATEST(0,
			EXTRACT(EPOCH FROM ($1::timestamp-(clock_timestamp() AT TIME ZONE 'UTC')))))`, deadline))
		server.Raise(held.Commit(ctx))
		got := <-result
		if got.err != nil || got.escrow == nil || len(got.escrow.Balances) != 1 ||
			got.escrow.Balances[0].BalanceId != second.BalanceId || got.escrow.Balances[0].BalanceByteCount != 1024 {
			t.Fatalf("client wait spent expired grant or lost later credit: %+v escrow=%+v", got, got.escrow)
		}
		reserved := openEscrowReservedForBalances(ctx, []server.Id{first.BalanceId, second.BalanceId})
		if reserved[first.BalanceId].reserved != 0 || reserved[second.BalanceId].reserved != 1024 {
			t.Fatal("client wait changed earliest eligible grant reservation")
		}
	})
}
