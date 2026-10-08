// Known pre-publication refusals compensate only their own Redis tokens.
// PostgreSQL row-lock barriers make the public cancellation order explicit.
package model

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

func TestRedisAdmissionPublicRefusalPreservesOtherReservations(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, mode := range []string{"inactive-source", "inactive-destination", "foreign-destination"} {
			f := newNetEscrowOrderingTestFixture(t, ctx)
			neighbor := createRedisAdmissionTest(ctx, f, 17)
			want := ErrContractDestinationInactive
			if mode == "foreign-destination" {
				f.destinationNetworkId = server.NewId()
			} else {
				id := f.destinationId
				if mode == "inactive-source" {
					id, want = f.sourceId, ErrActiveClientNotFound
				}
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false, deactivate_time=$2 WHERE client_id=$1`, id, server.NowUtc()))
				})
			}
			escrow, err := CreateTransferEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 23)
			if escrow != nil || !errors.Is(err, want) {
				t.Fatalf("%s did not reach actual client refusal: escrow=%+v err=%v", mode, escrow, err)
			}
			if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 17 {
				t.Fatalf("%s retained refused bytes or released its neighbor: %d", mode, got)
			}
			requireRedisRefusalOnlyContract(t, ctx, f, neighbor.ContractId)
		}
	})
}

// A later reserve failure must compensate earlier accepted tokens. Corrupt
// state remains refused; neither its counter nor a live peer is reconstructed.
func TestRedisAdmissionPartialReserveErrorCompensatesKnownTokens(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, f, 17)
		second := &TransferBalance{NetworkId: f.sourceNetworkId, StartTime: server.NowUtc().Add(-time.Minute), EndTime: server.NowUtc().Add(2 * time.Hour), StartBalanceByteCount: 1000, BalanceByteCount: 1000, PurchaseToken: "synthetic-" + server.NewId().String()}
		AddTransferBalance(ctx, second)
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.Set(ctx, redisContractReservationKeys(second.BalanceId)[0], "malformed", 0).Err())
		})
		escrow, err := CreateTransferEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 1100)
		if escrow != nil || err == nil || !strings.Contains(err.Error(), "invalid reservation counter") {
			t.Fatalf("second reservation did not produce its intended error: escrow=%+v err=%v", escrow, err)
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 17 {
			t.Fatalf("partial reservation error retained first token: %d", got)
		}
		server.Redis(ctx, func(client server.RedisClient) {
			value, err := client.Get(ctx, redisContractReservationKeys(second.BalanceId)[0]).Result()
			server.Raise(err)
			if value != "malformed" {
				t.Fatal("compensation reconstructed corrupt neighbor state", value)
			}
		})
		requireRedisRefusalOnlyContract(t, ctx, f, neighbor.ContractId)
	})
}

func TestRedisAdmissionPublicCanceledFenceCompensatesWithoutCaller(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, f, 17)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT 1 FROM network_client WHERE client_id=$1 FOR UPDATE`, f.destinationId))
		pid := contractLifecycleTestBackendPid(t, ctx, held)
		requestCtx, requestCancel := context.WithCancel(ctx)
		defer requestCancel()
		done := make(chan error, 1)
		go func() {
			var err error
			value := server.HandleError(func() {
				_, err = CreateTransferEscrow(requestCtx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 23)
			})
			if value != nil {
				if cause, ok := value.(error); ok {
					err = cause
				} else {
					err = errors.New("unexpected non-error admission panic")
				}
			}
			done <- err
		}()
		requireContractLifecycleBlockedBy(t, ctx, held, pid)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 40 {
			t.Fatal("cancellation barrier did not follow actual Redis reservation", got)
		}
		requestCancel()
		select {
		case err = <-done:
		case <-ctx.Done():
			t.Fatal("canceled admission failed to join", ctx.Err())
		}
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatal("actual canceled SQL read lost its cause", err)
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 17 {
			t.Fatal("canceled caller prevented exact-token compensation", got)
		}
		server.Raise(held.Rollback(ctx))
		requireRedisRefusalOnlyContract(t, ctx, f, neighbor.ContractId)
	})
}

// This wrapper changes only the test-owned database clock observation after
// the real reservation and client lock. All queries and writes still use PG.
type redisRefusalTestTx struct {
	server.PgTx
	deadlineReads int
	beforeFence   func()
}

func (self *redisRefusalTestTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "contract_lifecycle_write_boundary") && self.beforeFence != nil {
		self.beforeFence()
	}
	return self.PgTx.Query(ctx, sql, args...)
}

func (self *redisRefusalTestTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if sql == `SELECT clock_timestamp() AT TIME ZONE 'UTC' < $1::timestamp` {
		self.deadlineReads++
		if self.deadlineReads == 2 {
			return self.PgTx.QueryRow(ctx, `SELECT false`)
		}
	}
	return self.PgTx.QueryRow(ctx, sql, args...)
}

func TestRedisAdmissionExpiredPostReserveClockCompensates(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		peer := newEscrowSelectionTestClients(t, ctx)
		neighbor, err := CreateTransferEscrow(ctx, owner.NetworkId, owner.ClientId, peer.providerNetworkId, peer.providerId, 17)
		if err != nil || neighbor == nil {
			t.Fatal("live shard reservation fixture failed", err)
		}
		requestCtx := withRedisContractAdmission(ctx)
		var failure error
		var reads int
		server.Tx(requestCtx, func(tx server.PgTx) {
			wrapped := &redisRefusalTestTx{PgTx: tx, beforeFence: func() {
				if got := Testing_NetEscrowByteCount(ctx, owner.BalanceId); got != 40 {
					t.Fatal("expiry seam preceded actual reservation", got)
				}
			}}
			escrow, _, err := createTransferEscrowInTx(requestCtx, wrapped, owner.NetworkId, owner.ClientId, peer.providerNetworkId, peer.providerId, owner.NetworkId, 23, nil)
			failure, reads = err, wrapped.deadlineReads
			if escrow != nil {
				t.Fatal("expired shard produced an escrow", escrow)
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		if err := redisAdmissionFromContext(requestCtx).compensate(ctx); err != nil {
			t.Fatal("expired request compensation failed after SQL joined", err)
		}
		if reads != 2 || !errors.Is(failure, ErrProberShardRetired) {
			t.Fatal("post-reserve deadline boundary was not exercised", reads, failure)
		}
		if got := Testing_NetEscrowByteCount(ctx, owner.BalanceId); got != 17 {
			t.Fatal("expired shard retained refused reservation", got)
		}
		requireRedisRefusalOnlyContract(t, ctx, netEscrowOrderingTestFixture{sourceNetworkId: owner.NetworkId, balanceId: owner.BalanceId}, neighbor.ContractId)
	})
}

func TestRedisAdmissionPrePublicationPanicCompensatesAndPropagates(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, f, 17)
		requestCtx := withRedisContractAdmission(ctx)
		const marker = "synthetic-pre-publication-panic"
		value := server.HandleError(func() {
			server.Tx(requestCtx, func(tx server.PgTx) {
				wrapped := &redisRefusalTestTx{PgTx: tx, beforeFence: func() {
					if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 40 {
						t.Fatal("panic seam preceded actual reservation", got)
					}
					panic(marker)
				}}
				_, _, _ = createTransferEscrowInTx(requestCtx, wrapped, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 23, nil)
			}, server.TxReadCommitted, server.OptNoRetry())
		})
		if value != marker {
			t.Fatal("compensation replaced the original panic", value)
		}
		if err := redisAdmissionFromContext(requestCtx).compensate(ctx); err != nil {
			t.Fatal("panicked request compensation failed after SQL joined", err)
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 17 {
			t.Fatal("pre-publication panic retained refused reservation", got)
		}
		requireRedisRefusalOnlyContract(t, ctx, f, neighbor.ContractId)
	})
}

// Both SQL custody and the Redis token of the earlier acknowledged contract
// must survive compensation; a numeric total alone could hide substitution.
func requireRedisRefusalOnlyContract(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, neighbor server.Id) {
	t.Helper()
	requireRedisRefusalOnlyContractWithByteCount(t, ctx, f, neighbor, 17)
}

// The same strict custody and token-identity check also covers other fixture
// sizes; retaining the wrong neighbor amount must still fail compensation.
func requireRedisRefusalOnlyContractWithByteCount(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, neighbor server.Id, byteCount ByteCount) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var contracts, escrows int
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM transfer_contract WHERE payer_network_id=$1 AND contract_id<>$2),
			(SELECT count(*) FROM transfer_escrow WHERE balance_id=$3 AND contract_id<>$2)`, f.sourceNetworkId, neighbor, f.balanceId).Scan(&contracts, &escrows))
		if contracts != 0 || escrows != 0 {
			t.Fatal("refused request published SQL custody", contracts, escrows)
		}
	})
	server.Redis(ctx, func(client server.RedisClient) {
		values, err := client.HGetAll(ctx, redisContractReservationKeys(f.balanceId)[1]).Result()
		server.Raise(err)
		if len(values) != 1 || values[neighbor.String()] != strconv.FormatInt(int64(byteCount), 10) {
			t.Fatal("compensation changed the live neighbor token", values)
		}
	})
}
