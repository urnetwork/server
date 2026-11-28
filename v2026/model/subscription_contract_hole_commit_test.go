package model

// Real commit and Redis command boundaries retain publication custody after
// caller cancellation and across out-of-order create/close callbacks.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

// Expiring a legacy pair is unknown, even though its source row still exists.
// A real new creation supplies fresh evidence without silently backfilling it.
func TestContractHoleMissingLegacyDropsUntilNewCreation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		legacy := insertContractHoleSourceRow(ctx, f.sourceId, f.destinationId)
		server.Raise(applyContractHoleEvent(ctx, legacy, f.sourceId, f.destinationId, "create"))
		keys := contractHoleKeys(f.sourceId, f.destinationId)
		server.Redis(ctx, func(client server.RedisClient) {
			for _, key := range keys[:2] {
				server.Raise(client.PExpireAt(ctx, key, time.Unix(1, 0)).Err())
			}
		})
		packetCtx := server.WithoutPostgres(ctx)
		status, err := ReadContractHole(packetCtx, f.sourceId, f.destinationId)
		if err != nil || status != ContractHoleUnknown || HasOpenContractHole(packetCtx, f.sourceId, f.destinationId) {
			t.Fatal("missing legacy evidence authorized", status, err)
		}
		_, err = CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 100)
		server.Raise(err)
		requireContractHoleCount(t, packetCtx, f.sourceId, f.destinationId, 1)
		server.Redis(ctx, func(client server.RedisClient) {
			_, err := client.ZScore(ctx, keys[1], legacy.String()).Result()
			if !errors.Is(err, server.RedisNil) {
				t.Fatal("new creation silently backfilled a legacy member", err)
			}
		})
		if server.PacketPostgresAttempts(packetCtx) != 0 {
			t.Fatal("missing/new-created reads queried PostgreSQL")
		}
	})
}

// The actual model create commits after its request goes away. The same caller
// cancellation at a committed close must still withdraw the published member.
func TestContractHoleCommittedLifecycleSurvivesCallerCancellation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		requestCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		var id server.Id
		server.Tx(requestCtx, func(tx server.PgTx) {
			var err error
			id, _, err = createContractNoEscrowInTx(requestCtx, tx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 100, true)
			server.Raise(err)
			cancel()
		}, server.OptNoRetry())
		requireContractHoleCount(t, ctx, f.sourceId, f.destinationId, 1)
		var expiration time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT expiration_time FROM transfer_contract WHERE contract_id=$1`, id).Scan(&expiration))
		})
		status, until, err := ReadContractHoleLease(ctx, f.sourceId, f.destinationId)
		if err != nil || status != ContractHolePositive || until.After(expiration) {
			t.Fatal("committed creation lost or extended its deadline", status, until, expiration, err)
		}
		closeCtx, cancelClose := context.WithCancel(ctx)
		defer cancelClose()
		server.Tx(closeCtx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(closeCtx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,checkpoint) VALUES($1,'source',0,false)`, id))
			contractHoleEventInTx(closeCtx, tx, id, f.sourceId, f.destinationId, "remove")
			cancelClose()
		}, server.OptNoRetry())
		requireContractHoleCount(t, ctx, f.sourceId, f.destinationId, 0)
	})
}

// Only this instance's first matching create waits; other lifecycle commands
// are free to reach Redis. Cancellation releases the admitted command.
type contractHoleCreateBarrier struct {
	contract string
	armed    atomic.Bool
	entered  chan struct{}
	release  chan struct{}
}

// Socket construction remains owned by the real Redis client.
func (self *contractHoleCreateBarrier) DialHook(next redis.DialHook) redis.DialHook { return next }

// Lifecycle publication uses individual commands, not pipelines.
func (self *contractHoleCreateBarrier) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// A command reaches Redis only after the test's competing lifecycle operation.
func (self *contractHoleCreateBarrier) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		args := command.Args()
		if command.Name() == "eval" && len(args) > 9 && args[1] == contractHoleEventScript && args[8] == "create" && args[9] == self.contract && self.armed.CompareAndSwap(true, false) {
			close(self.entered)
			select {
			case <-self.release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return next(ctx, command)
	}
}

// A delayed earlier create cannot shorten a later contract's key lease, and a
// final close wins even when its matching create is already waiting to publish.
func TestContractHoleCreateReorderingPreservesLaterLeaseAndFinalClose(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := server.WithoutPostgres(t.Context())
		for _, scenario := range []string{"later_create", "final_close"} {
			func() {
				source, destination, earlier := server.NewId(), server.NewId(), server.NewId()
				barrier := &contractHoleCreateBarrier{contract: earlier.String(), entered: make(chan struct{}), release: make(chan struct{})}
				barrier.armed.Store(true)
				install, cancelInstall := context.WithTimeout(ctx, time.Second)
				server.Raise(server.RedisWithDeadline(install, func(client server.RedisClient) error { client.AddHook(barrier); return nil }))
				cancelInstall()
				operationCtx, cancel := context.WithCancel(ctx)
				finished := make(chan struct{})
				result := make(chan error, 1)
				go func() {
					defer close(finished)
					result <- applyContractHoleEvent(operationCtx, earlier, source, destination, "create", server.NowUtc().Add(20*time.Minute))
				}()
				defer func() { cancel(); <-finished }()
				select {
				case <-barrier.entered:
				case err := <-result:
					t.Fatal("create escaped barrier", err)
				}
				keys := contractHoleKeys(source, destination)
				var originalExpiry int64
				if scenario == "later_create" {
					server.Raise(applyContractHoleEvent(ctx, server.NewId(), source, destination, "create", server.NowUtc().Add(DefaultContractExpiration)))
					server.Redis(ctx, func(client server.RedisClient) {
						var err error
						originalExpiry, err = client.Eval(ctx, `return redis.call('PEXPIRETIME',KEYS[1])`, keys[:1]).Int64()
						server.Raise(err)
					})
				} else {
					server.Raise(applyContractHoleEvent(ctx, earlier, source, destination, "remove"))
				}
				close(barrier.release)
				server.Raise(<-result)
				if scenario == "later_create" {
					requireContractHoleCount(t, ctx, source, destination, 2)
					server.Redis(ctx, func(client server.RedisClient) {
						for _, key := range keys[:2] {
							got, err := client.Eval(ctx, `return redis.call('PEXPIRETIME',KEYS[1])`, []string{key}).Int64()
							if err != nil || got < originalExpiry {
								t.Fatal("older callback shortened newer lease", got, originalExpiry, err)
							}
						}
					})
				} else {
					requireContractHoleCount(t, ctx, source, destination, 0)
				}
			}()
		}
		if server.PacketPostgresAttempts(ctx) != 0 {
			t.Fatal("reordered lifecycle called PostgreSQL")
		}
	})
}
