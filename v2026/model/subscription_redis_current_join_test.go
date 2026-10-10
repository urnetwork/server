// Current bounded admission and recovery must compose with asynchronous debit
// writeback. Real transactions retain each lost post; no timing assumption
// supplies the financial or recovery ordering.
package model

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

// Commit a real owned admission without its optional recovery page, modeling
// a successful SQL commit whose caller loses the reply before marker cleanup.
func redisCurrentJoinRetainedContract(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, amount ByteCount) *TransferEscrow {
	t.Helper()
	owned := withRedisContractAdmission(ctx)
	var result *TransferEscrow
	server.Tx(owned, func(tx server.PgTx) {
		var err error
		result, _, err = createTransferEscrowInTx(owned, tx, f.sourceNetworkId, f.sourceId,
			f.destinationNetworkId, f.destinationId, f.sourceNetworkId, amount, nil)
		server.Raise(err)
	}, server.TxReadCommitted, server.OptNoRetry())
	if result == nil {
		t.Fatal("retained request did not publish SQL custody")
	}
	redisRecoveryRequireMarker(t, ctx, f.balanceId, result.ContractId, true)
	return result
}

func TestRedisCurrentJoinBoundedShrinkKeepsExactCustody(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		ids := redisSelectionTestGrants(t, ctx, clients, 3, Mib, false)
		escrow, observed, err := redisSelectionCreateCounted(t, ctx, clients, 128*Mib)
		if err != nil || escrow == nil || escrow.TransferByteCount != 3*Mib || len(escrow.Balances) != 3 {
			t.Fatal("complete bounded census did not preserve shrink-to-fit", escrow, err)
		}
		for _, rows := range observed.pageCounts {
			if rows > redisGrantPageRows {
				t.Fatal("shrink materialized an unbounded grant page", rows)
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var contract, allocated ByteCount
			server.Raise(conn.QueryRow(ctx, `SELECT transfer_byte_count FROM transfer_contract WHERE contract_id=$1`, escrow.ContractId).Scan(&contract))
			server.Raise(conn.QueryRow(ctx, `SELECT sum(balance_byte_count) FROM transfer_escrow WHERE contract_id=$1 AND redis_reserved`, escrow.ContractId).Scan(&allocated))
			if contract != 3*Mib || allocated != contract {
				t.Fatal("returned, signed-contract and escrow allocation diverged", contract, allocated)
			}
		})
		for _, id := range ids {
			if got := Testing_NetEscrowByteCount(ctx, id); got != Mib {
				t.Fatal("shrink lost its exact reservation", got)
			}
			redisRecoveryRequireMarker(t, ctx, id, escrow.ContractId, false)
		}
	})
}

func TestRedisCurrentJoinIncompleteCensusNeverShrinks(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, unknownCounter := range []bool{false, true} {
			ctx := context.WithValue(t.Context(), redisGrantSelectionContextKey{}, redisGrantSelectionLimits{MaxRows: 64, MaxSelected: 1})
			clients := newEscrowSelectionTestClients(t, ctx)
			ids := redisSelectionTestGrants(t, ctx, clients, 2, 2*Mib, false)
			if unknownCounter {
				ctx = context.WithValue(ctx, redisGrantSelectionContextKey{}, redisGrantSelectionLimits{MaxRows: 64, MaxSelected: 2})
				server.Redis(ctx, func(client server.RedisClient) {
					server.Raise(client.Set(ctx, redisContractReservationKeys(ids[1])[0], "unknown-counter", 0).Err())
				})
			}
			escrow, err := clients.create(ctx, 128*Mib, false)
			if err == nil || escrow != nil || !unknownCounter && !errors.Is(err, errRedisGrantSelectionCapacity) {
				t.Fatal("incomplete census invented a smaller funding authority", unknownCounter, escrow, err)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var count int
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE payer_network_id=$1`, clients.payerNetworkId).Scan(&count))
				if count != 0 {
					t.Fatal("refused census published a contract", count)
				}
			})
			if got := Testing_NetEscrowByteCount(ctx, ids[0]); got != 0 {
				t.Fatal("refused partial shrink lost compensation", got)
			}
			if unknownCounter {
				server.Redis(ctx, func(client server.RedisClient) {
					value, err := client.Get(ctx, redisContractReservationKeys(ids[1])[0]).Result()
					if err != nil || value != "unknown-counter" {
						t.Fatal("unknown counter was silently reset", value, err)
					}
				})
			}
		}
	})
}

func TestRedisCurrentJoinRecoveryRetainsDebitAndAdvancesPeer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := redisCurrentJoinRetainedContract(t, ctx, f, 100)
		posts := asyncDebitTestSettle(ctx, contract.ContractId, 11)
		abandoned := redisRecoveryAbandonedTestRequest(t, ctx, f, 23)
		neighbor := createRedisAdmissionTest(ctx, f, 29)
		credit, pending, applied := asyncDebitTestState(t, ctx, f.balanceId)
		if credit != 1000 || pending != 1 || applied != 0 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 129 {
			t.Fatal("fresh public recovery released consumed debt before SQL writeback", credit, pending, applied)
		}
		redisRecoveryRequireMarker(t, ctx, f.balanceId, contract.ContractId, true)
		redisRecoveryRequireMarker(t, ctx, f.balanceId, abandoned, false)
		redisRecoveryRequireMarker(t, ctx, f.balanceId, neighbor.ContractId, false)
		n, released, busy, err := flushTransferDebitBalance(ctx, f.balanceId)
		if err != nil || n != 1 || released != 1 || busy {
			t.Fatal("original debit owner could not complete", n, released, busy, err)
		}
		server.RunPosts(ctx, posts...)
		credit, pending, applied = asyncDebitTestState(t, ctx, f.balanceId)
		if credit != 989 || pending+applied != 0 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 29 {
			t.Fatal("debit completion or lost post replay changed its healthy neighbor", credit, pending, applied)
		}
	})
}

func TestRedisCurrentJoinRecoveryUnderstandsReducedAppliedDebit(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := redisCurrentJoinRetainedContract(t, ctx, f, 100)
		type expectedState struct {
			credit, token, allocation, payout, debt ByteCount
			pending, applied                        int
			marker                                  bool
		}
		providerFixture := &forceCloseDisputeFixture{contractId: contract.ContractId, providerNetworkId: f.destinationNetworkId}
		providerWant := forceCloseProviderProjection{}
		requireState := func(want expectedState) {
			t.Helper()
			credit, pending, applied := asyncDebitTestState(t, ctx, f.balanceId)
			if credit != want.credit || pending != want.pending || applied != want.applied || Testing_NetEscrowByteCount(ctx, f.balanceId) != want.token {
				t.Fatalf("recovery conservation credit=%d pending=%d applied=%d; want=%+v", credit, pending, applied, want)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var allocation, payout, debt ByteCount
				var marked bool
				server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count,COALESCE(payout_byte_count,0),redis_reserved FROM transfer_escrow WHERE contract_id=$1 AND balance_id=$2`, contract.ContractId, f.balanceId).Scan(&allocation, &payout, &marked))
				server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(sum(debit_byte_count),0) FROM transfer_debit_journal WHERE contract_id=$1 AND balance_id=$2`, contract.ContractId, f.balanceId).Scan(&debt))
				if !marked || allocation != want.allocation || payout != want.payout || debt != want.debt {
					t.Fatal("recovery changed immutable allocation, payout metadata or durable consumption")
				}
			})
			server.Redis(ctx, func(client server.RedisClient) {
				values, err := client.HGetAll(ctx, redisContractReservationKeys(f.balanceId)[1]).Result()
				server.Raise(err)
				if want.token == 0 {
					if len(values) != 0 {
						t.Fatal("completed recovery retained or recreated a token")
					}
				} else if len(values) != 1 || values[contract.ContractId.String()] != strconv.FormatInt(int64(want.token), 10) {
					t.Fatal("recovery substituted a reservation token")
				}
			})
			redisRecoveryRequireMarker(t, ctx, f.balanceId, contract.ContractId, want.marker)
			if provider := readForceCloseProviderProjection(t, ctx, providerFixture); provider != providerWant {
				t.Fatalf("debit or recovery changed provider earnings or their durable projection owner: got=%+v want=%+v", provider, providerWant)
			}
		}
		want := expectedState{credit: 1000, token: 100, allocation: 100, marker: true}
		requireState(want)
		posts := asyncDebitTestSettle(ctx, contract.ContractId, 11)
		server.RunPosts(ctx, posts...)
		want.pending, want.debt = 1, 11
		providerWant = forceCloseProviderProjection{sweptBytes: 11, unappliedBytes: 11, owners: 1}
		requireState(want)
		// Current settlement posts retain the original token until the worker.
		// Explicit repair is the supported producer of a reduced pending token.
		ReconcileRedisContractReservation(ctx, contract.ContractId)
		want.token = 11
		requireState(want)
		if err := recoverRedisReservationRequest(ctx, f.balanceId, contract.ContractId); err == nil || errors.Is(err, errRedisReservationRecoveryIdentity) {
			t.Fatal("pending debit became absent or contradictory SQL custody", err)
		}
		if Testing_NetEscrowByteCount(ctx, f.balanceId) != 11 {
			t.Fatal("recovery changed the exact already-settled token")
		}
		requireState(want)
		hook := &asyncDebitReleaseHook{key: redisContractReservationKeys(f.balanceId)[0]}
		hook.enabled.Store(true)
		defer hook.enabled.Store(false)
		asyncDebitInstallHook(ctx, hook)
		if _, _, _, err := flushTransferDebitBalance(ctx, f.balanceId); err == nil || hook.hits.Load() == 0 {
			t.Fatal("fixture did not retain the real applied-debit release failure", err)
		}
		want.credit, want.payout = 989, 11
		want.pending, want.applied = 0, 1
		requireState(want)
		hook.enabled.Store(false)
		if err := recoverRedisReservationRequest(ctx, f.balanceId, contract.ContractId); err != nil {
			t.Fatal("exact reduced applied debit was mistaken for changed authority", err)
		}
		if Testing_NetEscrowByteCount(ctx, f.balanceId) != 0 {
			t.Fatal("committed debit recovery did not release its original token")
		}
		want.token, want.marker = 0, false
		requireState(want)
		n, released, busy, err := flushTransferDebitBalance(ctx, f.balanceId)
		credit, pending, applied := asyncDebitTestState(t, ctx, f.balanceId)
		if err != nil || n != 0 || released != 1 || busy || credit != 989 || pending+applied != 0 {
			t.Fatal("writeback replay re-debited recovered credit", credit, pending, applied, err)
		}
		want.applied, want.debt = 0, 0
		requireState(want)
		server.RunPosts(ctx, posts...)
		ReconcileRedisContractReservation(ctx, contract.ContractId)
		if err := recoverRedisReservationRequest(ctx, f.balanceId, contract.ContractId); err != nil {
			t.Fatal("completed missing marker lost idempotent recovery", err)
		}
		n, released, busy, err = flushTransferDebitBalance(ctx, f.balanceId)
		if err != nil || n != 0 || released != 0 || busy {
			t.Fatal("completed debit replay recreated work", err)
		}
		requireState(want)
	})
}

func TestRedisCurrentJoinArchivedContractRetainsPendingDebit(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := redisCurrentJoinRetainedContract(t, ctx, f, 100)
		_ = asyncDebitTestSettle(ctx, contract.ContractId, 11)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, contract.ContractId))
		})
		server.Db(ctx, func(conn server.PgConn) {
			var live, archived int
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM transfer_contract WHERE contract_id=$1),
				(SELECT count(*) FROM st_provider_usage_archive WHERE contract_id=$1)`, contract.ContractId).Scan(&live, &archived))
			if live != 0 || archived != 1 {
				t.Fatal("fixture did not reach the actual archived-contract boundary", live, archived)
			}
		})
		if err := recoverRedisReservationRequest(ctx, f.balanceId, contract.ContractId); err == nil || errors.Is(err, errRedisReservationRecoveryIdentity) {
			t.Fatal("archived history concealed pending consumption or invented changed custody", err)
		}
		credit, pending, applied := asyncDebitTestState(t, ctx, f.balanceId)
		if credit != 1000 || pending != 1 || applied != 0 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 100 {
			t.Fatal("archival released consumed credit before writeback", credit, pending, applied)
		}
		n, released, busy, err := flushTransferDebitBalance(ctx, f.balanceId)
		if err != nil || n != 1 || released != 1 || busy {
			t.Fatal("archived debit could not finish its original writeback", n, released, busy, err)
		}
	})
}

func TestAsyncDebitReleaseHookFollowsDeclaredKeyCount(t *testing.T) {
	for _, count := range []int{4, 5} {
		hook := &asyncDebitReleaseHook{key: "synthetic-reservation", after: true}
		hook.enabled.Store(true)
		args := []any{"eval", "synthetic-script", count}
		for index := range count {
			key := "synthetic-peer-" + strconv.Itoa(index)
			if index == 0 {
				key = hook.key
			}
			args = append(args, key)
		}
		args = append(args, "release")
		forwarded := 0
		err := hook.ProcessPipelineHook(func(context.Context, []redis.Cmder) error { forwarded++; return nil })(t.Context(), []redis.Cmder{redis.NewCmd(t.Context(), args...)})
		if err == nil || forwarded != 1 || hook.hits.Load() != 1 {
			t.Fatal("lost-ack barrier did not follow actual Redis key count", count, forwarded, err)
		}
	}
}
