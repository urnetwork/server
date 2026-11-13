// The reservation script's admission, compensation, recovery and settlement
// release calls send no preflight PING, and those made outside a PostgreSQL
// transaction run on the late-reply deadline pool. The deadline pools replay
// nothing at either retry layer, so the PING decided nothing about the
// script's own call: it reached an arbitrary node and cost each call a round
// trip and a pool turn there, and on a slow node it failed the call or closed
// the connection. A late reply on the late-reply pool costs its caller the
// deadline but keeps the connection, where the default deadline pool closes it
// and the next call dials and handshakes again on the slow node.
package model

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

// Counts a deadline pool's PINGs and scripts, pipelined or not, while armed.
type redisPreflightCountHook struct {
	armed   atomic.Bool
	pings   atomic.Int64
	scripts atomic.Int64
}

func (self *redisPreflightCountHook) count(command redis.Cmder) {
	if !self.armed.Load() {
		return
	}
	switch command.Name() {
	case "ping":
		self.pings.Add(1)
	case "eval", "evalsha":
		self.scripts.Add(1)
	}
}

func (self *redisPreflightCountHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (self *redisPreflightCountHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		self.count(command)
		return next(ctx, command)
	}
}
func (self *redisPreflightCountHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		for _, command := range commands {
			self.count(command)
		}
		return next(ctx, commands)
	}
}

// Adds a hook to both deadline pools: a reservation call runs on the
// late-reply pool outside a PostgreSQL transaction and on the deadline pool
// inside one.
func addRedisDeadlinePoolsHook(ctx context.Context, hook redis.Hook) {
	for _, options := range [][]any{nil, {server.OptRedisKeepLateReplies()}} {
		func() {
			bounded, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			server.Raise(server.RedisWithDeadline(bounded, func(client server.RedisClient) error {
				client.AddHook(hook)
				return nil
			}, options...))
		}()
	}
}

func installRedisPreflightCountHook(ctx context.Context, options ...any) *redisPreflightCountHook {
	hook := &redisPreflightCountHook{}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	server.Raise(server.RedisWithDeadline(bounded, func(client server.RedisClient) error {
		client.AddHook(hook)
		return nil
	}, options...))
	return hook
}

func TestRedisAdmissionReservationCallsSkipPreflightAndKeepLateReplies(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		deadline := installRedisPreflightCountHook(ctx)
		late := installRedisPreflightCountHook(ctx, server.OptRedisKeepLateReplies())

		late.armed.Store(true)
		// Admission: the pending reservation, then the publication's recovery
		// page and acknowledgement.
		settled := createRedisAdmissionTest(ctx, f, 17)
		// Settlement: the debit worker's release.
		server.RunPosts(ctx, settleNetEscrowOrderingTestContract(ctx, settled.ContractId)...)
		flushRedisAdmissionTestDebits(t, ctx, f.balanceId, 1, 1)
		// Compensation: a request reserves, its client fence refuses it before
		// publication, and it releases its pending token, then pages recovery.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false, deactivate_time=$2 WHERE client_id=$1`, f.destinationId, server.NowUtc()))
		})
		escrow, err := CreateTransferEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 23)
		if escrow != nil || !errors.Is(err, ErrContractDestinationInactive) {
			t.Fatal("refused request", escrow, err)
		}
		late.armed.Store(false)

		// Recovery of one request reads its token inside its transaction, on
		// the deadline pool.
		deadline.armed.Store(true)
		if err := recoverRedisReservationRequest(ctx, f.balanceId, server.NewId()); err != nil {
			t.Fatal("request recovery", err)
		}
		deadline.armed.Store(false)

		latePings, lateScripts := late.pings.Load(), late.scripts.Load()
		deadlinePings, deadlineScripts := deadline.pings.Load(), deadline.scripts.Load()
		if latePings != 0 || lateScripts != 7 || deadlinePings != 0 || deadlineScripts != 1 {
			t.Fatalf("late-reply pool: %d preflight PINGs with %d scripts, want none with 7; deadline pool in a transaction: %d preflight PINGs with %d scripts, want none with 1",
				latePings, lateScripts, deadlinePings, deadlineScripts)
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 0 {
			t.Fatal("reservations left after their releases", got)
		}
	})
}
