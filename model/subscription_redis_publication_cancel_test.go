// Exercises cancellation at the actual public creator's successful SQL batch,
// before its transaction owner can commit.
package model

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// The existing fixture-local observer joins real connection ownership. Only the
// exact request's publication batch can trigger cancellation; no SQL is mocked.
type redisCanceledPublicationTrace struct {
	*redisPublicationObserver
	contractId         server.Id
	cancel             context.CancelFunc
	publicationBatches atomic.Int32
	publicationWrites  atomic.Int32
	batchFailed        atomic.Bool
}

type redisPublicationBatchKey struct{}

func (self *redisCanceledPublicationTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	if ctx.Value(self.redisPublicationObserver) != true {
		return ctx
	}
	var escrow, contract bool
	for _, query := range data.Batch.QueuedQueries {
		if len(query.Arguments) == 0 {
			continue
		}
		id, ok := query.Arguments[0].(server.Id)
		if !ok || id != self.contractId {
			continue
		}
		escrow = escrow || strings.Contains(query.SQL, "INSERT INTO transfer_escrow(")
		contract = contract || strings.Contains(query.SQL, "INSERT INTO transfer_contract(")
	}
	return context.WithValue(ctx, redisPublicationBatchKey{}, escrow && contract)
}

func (self *redisCanceledPublicationTrace) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if ctx.Value(redisPublicationBatchKey{}) != true {
		return
	}
	if data.Err != nil {
		self.batchFailed.Store(true)
		return
	}
	if strings.Contains(data.SQL, "INSERT INTO transfer_escrow(") || strings.Contains(data.SQL, "INSERT INTO transfer_contract(") {
		if data.CommandTag.RowsAffected() != 1 {
			self.batchFailed.Store(true)
		}
		self.publicationWrites.Add(1)
	}
}

func (self *redisCanceledPublicationTrace) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchEndData) {
	if ctx.Value(redisPublicationBatchKey{}) != true {
		return
	}
	if data.Err != nil {
		self.batchFailed.Store(true)
		return
	}
	self.publicationBatches.Add(1)
	if self.cancel != nil {
		self.cancel()
	}
}

func testRedisPublicPublicationBoundary(t *testing.T, cancelAfterBatch bool) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, fixture, 17)
		observer := &redisPublicationObserver{}
		trace := &redisCanceledPublicationTrace{redisPublicationObserver: observer}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		caller, stop := context.WithCancel(context.WithValue(ctx, observer, true))
		defer stop()
		owned := withRedisContractAdmission(caller)
		owner := redisAdmissionFromContext(owned)
		trace.contractId = owner.contractId
		if cancelAfterBatch {
			trace.cancel = stop
		}
		var created *TransferEscrow
		var createErr error
		panicErr := redisPublicationHandleError(func() {
			created, createErr = CreateTransferEscrow(owned, fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId, 23)
		})
		if trace.publicationBatches.Load() != 1 || trace.publicationWrites.Load() != 2 || trace.batchFailed.Load() {
			t.Fatalf("public creator did not complete its real escrow/contract publication batch: batches=%d writes=%d failed=%t", trace.publicationBatches.Load(), trace.publicationWrites.Load(), trace.batchFailed.Load())
		}
		observer.stateLock.Lock()
		joined := observer.acquisitions == observer.releases && len(observer.activeConnKVs) == 0
		observer.stateLock.Unlock()
		if !joined {
			t.Fatal("public method returned before joining its actual PostgreSQL owner")
		}
		owner.stateLock.Lock()
		publicationStarted := owner.publicationStarted
		owner.stateLock.Unlock()
		if !publicationStarted {
			t.Fatal("cancellation did not cross the sticky publication boundary")
		}

		var contract, escrow, journal bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT
				EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$1),
				EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$1),
				EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1)`, owner.contractId).Scan(&contract, &escrow, &journal))
		})
		var tokens map[string]string
		var marked bool
		server.Redis(ctx, func(client server.RedisClient) {
			var readErr error
			tokens, readErr = client.HGetAll(ctx, redisContractReservationKeys(fixture.balanceId)[1]).Result()
			server.Raise(readErr)
			_, readErr = client.ZScore(ctx, redisContractReservationKeys(fixture.balanceId)[4], owner.contractId.String()).Result()
			if readErr != nil && !errors.Is(readErr, redis.Nil) {
				server.Raise(readErr)
			}
			marked = readErr == nil
		})
		t.Logf("joined public result canceled=%t publication_started=%t SQL contract/escrow/journal=%t/%t/%t own_token=%q marked=%t neighbor_token=%q", caller.Err() != nil, publicationStarted, contract, escrow, journal, tokens[owner.contractId.String()], marked, tokens[neighbor.ContractId.String()])
		if tokens[neighbor.ContractId.String()] != "17" {
			t.Fatal("public request changed the independent live neighbor")
		}
		if !cancelAfterBatch {
			if createErr != nil || panicErr != nil || created == nil || created.ContractId != owner.contractId || caller.Err() != nil || !contract || !escrow || journal || tokens[owner.contractId.String()] != "23" || marked || len(tokens) != 2 {
				t.Fatal("healthy publication did not retain exact committed custody", createErr, panicErr)
			}
			return
		}
		if !errors.Is(errors.Join(createErr, panicErr), context.Canceled) || created != nil || caller.Err() != context.Canceled || contract || escrow || journal {
			t.Fatal("canceled publication did not join a real rollback with its original cause", createErr, panicErr)
		}
		if tokens[owner.contractId.String()] != "" || marked || len(tokens) != 1 {
			t.Error("joined public rollback left its exact reservation after caller cancellation")
		}
		// This independent bounded owner must prove absence under the existing
		// exact SQL publication fence, never infer it from caller cancellation.
		if err := recoverRedisReservationRequest(ctx, fixture.balanceId, owner.contractId); err != nil {
			t.Fatal("existing exact fenced recovery could not resolve the rolled-back request", err)
		}
		redisRecoveryRequireMarker(t, ctx, fixture.balanceId, owner.contractId, false)
		if got := Testing_NetEscrowByteCount(ctx, fixture.balanceId); got != 17 {
			t.Fatal("exact fenced recovery changed neighboring custody", got)
		}
		requireRedisRefusalOnlyContract(t, ctx, fixture, neighbor.ContractId)
	})
}

func TestRedisCanceledPublicationBatchJoinsExactRecovery(t *testing.T) {
	testRedisPublicPublicationBoundary(t, true)
}

func TestRedisHealthyPublicationBatchKeepsCustody(t *testing.T) {
	testRedisPublicPublicationBoundary(t, false)
}
