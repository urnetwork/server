package model

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

// The actual EVAL runs on the private Redis fixture. Only delivery of its
// completed reply is changed, after Redis has established the token state.
type redisReservationReplyTestHook struct {
	key    string
	cancel context.CancelFunc
	hit    atomic.Bool
}

func (h *redisReservationReplyTestHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *redisReservationReplyTestHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *redisReservationReplyTestHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		err := next(ctx, command)
		args := command.Args()
		if err != nil || command.Name() != "eval" || len(args) < 4 || args[3] != h.key || h.hit.Swap(true) {
			return err
		}
		if h.cancel != nil {
			h.cancel() // The deadline wrapper must turn a successful zero into uncertainty.
			return nil
		}
		return io.ErrUnexpectedEOF
	}
}

func TestRedisAmbiguousCompensationKeepsActualReplyOwnership(t *testing.T) {
	for _, mode := range []string{"lost-positive-reply", "lost-zero-reply", "canceled-zero-reply"} {
		t.Run(mode, func(t *testing.T) {
			env := server.DefaultTestEnv()
			env.RerunCount = 0
			env.Run(t, func(t testing.TB) {
				ctx := t.Context()
				clients := newEscrowSelectionTestClients(t, ctx)
				ids := redisSelectionTestGrants(t, ctx, clients, 1, 100, false)
				neighbor := ByteCount(17)
				if mode != "lost-positive-reply" {
					neighbor = 100
				}
				redisSelectionReserveLegacy(t, ctx, clients, ids, neighbor)
				caller, cancel := context.WithCancel(ctx)
				defer cancel()
				owned := withRedisContractAdmission(caller)
				owner := redisAdmissionFromContext(owned)
				hook := &redisReservationReplyTestHook{key: redisContractReservationKeys(ids[0])[0]}
				if mode == "canceled-zero-reply" {
					hook.cancel = cancel
				}
				bounded, stop := context.WithTimeout(ctx, time.Second)
				server.Raise(server.RedisWithDeadline(bounded, func(client server.RedisClient) error { client.AddHook(hook); return nil }))
				stop()
				var reservationErr error
				_ = server.HandleError(func() {
					server.Tx(owned, func(tx server.PgTx) {
						_, _, reservationErr = selectRedisTransferBalances(owned, tx, owner, clients.payerNetworkId, clients.payerId, 23)
					}, server.TxReadCommitted, server.OptNoRetry())
				})
				if !hook.hit.Load() || reservationErr == nil || errors.Is(reservationErr, errRedisReservationInsufficient) {
					t.Fatal("actual reply-loss boundary was not observed", reservationErr)
				}
				if mode == "canceled-zero-reply" && !errors.Is(reservationErr, context.Canceled) {
					t.Fatal("deadline wrapper did not override the successful zero reply", reservationErr)
				}
				if !slices.Equal(owner.compensationIds(), ids) {
					t.Fatal("an unknown Redis outcome discarded its compensation obligation")
				}
				if mode == "lost-positive-reply" {
					redisRecoveryRequireMarker(t, ctx, ids[0], owner.contractId, true)
					if got := Testing_NetEscrowByteCount(ctx, ids[0]); got != neighbor+23 {
						t.Fatal("lost reply did not leave actual allocated custody", got)
					}
				}
				if err := owner.compensate(ctx); err != nil {
					t.Fatal("uncertain request failed to join compensation", err)
				}
				if got := Testing_NetEscrowByteCount(ctx, ids[0]); got != neighbor {
					t.Fatal("compensation lost allocated custody or changed its live neighbor", got)
				}
				redisRecoveryRequireMarker(t, ctx, ids[0], owner.contractId, false)
			})
		})
	}
}
