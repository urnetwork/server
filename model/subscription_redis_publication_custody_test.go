// Exact successful commits and terminal debit custody survive a canceled
// caller whose commit acknowledgement is withheld at the real public boundary.
package model

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// The inherited tracer supplies query identity and joined pool ownership. The
// callback runs only after the backend acknowledges this request's real commit.
type redisWithheldPublicationTrace struct {
	*redisPublicationObserver
	cancel      context.CancelFunc
	afterCommit func()
	injected    atomic.Bool
}

func (self *redisWithheldPublicationTrace) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	self.redisPublicationObserver.TraceQueryEnd(ctx, conn, data)
	if ctx.Value(self.redisPublicationObserver) != true || ctx.Value(redisPublicationCommitQueryKey{}) != true || data.Err != nil || !self.injected.CompareAndSwap(false, true) {
		return
	}
	if self.afterCommit != nil {
		self.afterCommit()
	}
	self.cancel()
	panic(errRedisPublicationLostCommitReply)
}

func testRedisCanceledPublicationKeepsCustody(t *testing.T, terminal bool, consumed ByteCount) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, fixture, 17)
		observer := &redisPublicationObserver{}
		trace := &redisWithheldPublicationTrace{redisPublicationObserver: observer}
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
		trace.cancel = stop
		owned := withRedisContractAdmission(caller)
		owner := redisAdmissionFromContext(owned)
		var posts []func() any
		if terminal {
			// A distinct settlement owner starts before the creator acquires PG.
			// The tracer only signals the acknowledged commit and joins this owner.
			settleCtx, settleCancel := context.WithCancel(ctx)
			commitObserved := make(chan struct{})
			settlementDone := make(chan struct{})
			var settlementErr error
			go func() {
				defer close(settlementDone)
				select {
				case <-settleCtx.Done():
					settlementErr = settleCtx.Err()
					return
				case <-commitObserved:
				}
				settlementErr = redisPublicationHandleError(func() {
					posts = asyncDebitTestSettle(settleCtx, owner.contractId, consumed)
				})
			}()
			defer func() {
				settleCancel()
				<-settlementDone
			}()
			trace.afterCommit = func() {
				close(commitObserved)
				<-settlementDone
				server.Raise(settlementErr)
			}
		}
		var created *TransferEscrow
		var createErr error
		panicErr := redisPublicationHandleError(func() {
			created, createErr = CreateTransferEscrow(owned, fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId, 23)
		})
		if !trace.injected.Load() || !errors.Is(panicErr, errRedisPublicationLostCommitReply) || createErr != nil || created != nil || caller.Err() != context.Canceled {
			t.Fatal("public result did not preserve its canceled, withheld real commit acknowledgement", createErr, panicErr)
		}
		observer.stateLock.Lock()
		joined := observer.acquisitions == observer.releases && len(observer.activeConnKVs) == 0
		observer.stateLock.Unlock()
		if !joined {
			t.Fatal("failed public result left its SQL owner running")
		}
		var actualTerminal, exactEscrow bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT c.outcome IS NOT NULL,
				EXISTS(SELECT 1 FROM transfer_escrow e WHERE e.contract_id=c.contract_id AND e.balance_id=$2 AND e.redis_reserved AND e.balance_byte_count=23)
				FROM transfer_contract c WHERE c.contract_id=$1 AND c.payer_network_id=$3`, owner.contractId, fixture.balanceId, fixture.sourceNetworkId).Scan(&actualTerminal, &exactEscrow))
		})
		if actualTerminal != terminal || !exactEscrow {
			t.Fatal("failed public reply lost exact durable custody")
		}
		var tokens map[string]string
		server.Redis(ctx, func(client server.RedisClient) {
			var readErr error
			tokens, readErr = client.HGetAll(ctx, redisContractReservationKeys(fixture.balanceId)[1]).Result()
			server.Raise(readErr)
		})
		if len(tokens) != 2 || tokens[owner.contractId.String()] != "23" || tokens[neighbor.ContractId.String()] != "17" || Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 40 {
			t.Fatal("canceled uncertain publication released durable or neighboring custody", tokens)
		}
		credit, pending, applied := asyncDebitTestState(t, ctx, fixture.balanceId)
		wantPending := 0
		if terminal {
			wantPending = 1
		}
		if credit != 1000 || pending != wantPending || applied != 0 {
			t.Fatal("failed public return applied or discarded pending consumption", credit, pending, applied)
		}
		t.Logf("joined withheld commit keeps terminal=%t consumed=%d own_token=23 neighbor=17 credit=%d pending=%d", terminal, consumed, credit, pending)
		if !terminal {
			return
		}
		redisRecoveryRequireMarker(t, ctx, fixture.balanceId, owner.contractId, true)
		server.Db(ctx, func(conn server.PgConn) {
			var amount ByteCount
			server.Raise(conn.QueryRow(ctx, `SELECT debit_byte_count FROM transfer_debit_journal WHERE contract_id=$1 AND balance_id=$2 AND NOT applied`, owner.contractId, fixture.balanceId).Scan(&amount))
			if amount != consumed {
				t.Fatal("cleanup changed durable consumption", amount)
			}
		})
		count, released, busy, flushErr := flushTransferDebitBalance(ctx, fixture.balanceId)
		if flushErr != nil || count != 1 || released != 1 || busy {
			t.Fatal("original durable debit owner could not release exact custody", count, released, busy, flushErr)
		}
		server.RunPosts(ctx, posts...)
		credit, pending, applied = asyncDebitTestState(t, ctx, fixture.balanceId)
		if credit != 1000-consumed || pending+applied != 0 || Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 17 {
			t.Fatal("debit completion or retained posts changed neighboring custody", credit, pending, applied)
		}
		redisRecoveryRequireMarker(t, ctx, fixture.balanceId, owner.contractId, false)
	})
}

func TestRedisCanceledLostCommitReplyKeepsLiveCustody(t *testing.T) {
	testRedisCanceledPublicationKeepsCustody(t, false, 0)
}

func TestRedisCanceledLostCommitReplyKeepsPendingDebit(t *testing.T) {
	testRedisCanceledPublicationKeepsCustody(t, true, 11)
}

func TestRedisCanceledLostCommitReplyKeepsZeroDebit(t *testing.T) {
	testRedisCanceledPublicationKeepsCustody(t, true, 0)
}
