// Actual callback arrivals prove which work delays a committed legacy page.
package model

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

type legacySettlementTimingGate struct {
	family    string
	streamKey string
	enabled   atomic.Bool
	entered   chan struct{}
	release   chan struct{}
}

func (self *legacySettlementTimingGate) DialHook(next redis.DialHook) redis.DialHook { return next }

func (self *legacySettlementTimingGate) matches(command redis.Cmder) bool {
	args := command.Args()
	if len(args) < 2 {
		return false
	}
	switch self.family {
	case "mirror":
		return command.Name() == "eval" && args[1] == netEscrowSnapshotScript
	case "clock":
		return command.Name() == "incrby" && args[1] == clockTransferByteCountRedisKey
	case "stream":
		return command.Name() == "get" && args[1] == self.streamKey
	}
	return false
}

func (self *legacySettlementTimingGate) wait(ctx context.Context) error {
	select {
	case self.entered <- struct{}{}:
	default:
	}
	select {
	case <-self.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (self *legacySettlementTimingGate) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		if self.enabled.Load() && self.matches(command) {
			if err := self.wait(ctx); err != nil {
				return err
			}
		}
		return next(ctx, command)
	}
}

func (self *legacySettlementTimingGate) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		for _, command := range commands {
			if self.enabled.Load() && self.matches(command) {
				if err := self.wait(ctx); err != nil {
					return err
				}
				break
			}
		}
		return next(ctx, commands)
	}
}

// The native hook stops exactly one real dependency family after the outcome
// commits. Another connection acquires the payer grant while the page cannot
// yet visit its next independently funded contract. Releasing the hook keeps
// every original callback and financial owner, then verifies exact recovery.
func TestLegacySettlementTimingAttributesJoinedCallbackWait(t *testing.T) {
	for _, family := range []string{"mirror", "clock", "stream"} {
		t.Run(family, func(t *testing.T) {
			env := server.DefaultTestEnv()
			env.RerunCount = 0
			env.Run(t, func(t testing.TB) {
				ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
				defer cancel()
				first, firstId := legacySettlementTestIntent(t, ctx)
				second := newNetEscrowOrderingTestFixture(t, ctx)
				escrow, createPosts := createNetEscrowOrderingTestContract(ctx, second, 100)
				server.RunPosts(ctx, createPosts...)
				// Establish the shared hash shard before any close or intent
				// exists. The real intent CHECK remains enabled throughout.
				secondId := escrow.ContractId
				secondId[15] = firstId[15]
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET contract_id=$2 WHERE contract_id=$1`, escrow.ContractId, secondId))
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET contract_id=$2 WHERE contract_id=$1`, escrow.ContractId, secondId))
				})
				server.Raise(CloseContract(ctx, secondId, second.sourceId, 11, false))
				server.Raise(CloseContract(ctx, secondId, second.destinationId, 11, false))
				refreshNetEscrow(ctx, []server.Id{second.balanceId})
				shard := int(firstId[15]) % LegacySettlementShardCount
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET next_attempt_time=$2 WHERE contract_id=$1`, firstId, server.NowUtc().Add(-time.Hour)))
					server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET next_attempt_time=$2 WHERE contract_id=$1`, secondId, server.NowUtc().Add(-30*time.Minute)))
					// One cold legacy mirror is the exact-census positive control;
					// the second grant retains its current cache and needs no scan.
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1`, first.balanceId))
				})
				gate := &legacySettlementTimingGate{family: family, streamKey: contractStreamKey(firstId),
					entered: make(chan struct{}, 1), release: make(chan struct{})}
				release := sync.OnceFunc(func() { close(gate.release) })
				defer release()
				defer gate.enabled.Store(false)
				server.Redis(ctx, func(client server.RedisClient) { client.AddHook(gate) })
				gate.enabled.Store(true)
				type completion struct {
					result LegacySettlementFlushResult
					err    error
				}
				done := make(chan completion, 1)
				retired := make(chan struct{})
				go func() {
					defer close(retired)
					result, err := FlushLegacySettlements(ctx, shard, nil, 2)
					done <- completion{result, err}
				}()
				defer func() {
					gate.enabled.Store(false)
					release()
					cancel()
					select {
					case <-retired:
					case <-time.After(10 * time.Second):
						t.Error("native phase fixture did not join its settlement owner")
					}
				}()
				select {
				case <-gate.entered:
				case <-done:
					t.Fatal("fixture did not reach its actual callback family")
				case <-ctx.Done():
					t.Fatal("legacy callback fixture did not reach a dependency boundary")
				}
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE NOWAIT`, first.balanceId))
					var committed, untouched bool
					server.Raise(tx.QueryRow(ctx, `SELECT
						(SELECT outcome IS NOT NULL FROM transfer_contract WHERE contract_id=$1),
						(SELECT outcome IS NULL FROM transfer_contract WHERE contract_id=$2)`, firstId, secondId).Scan(&committed, &untouched))
					if !committed || !untouched {
						t.Fatal("callback barrier did not isolate committed financial work from the next contract")
					}
				})
				select {
				case <-done:
					t.Fatal("legacy page stopped joining an admitted callback")
				default:
				}
				gate.enabled.Store(false)
				release()
				var got completion
				select {
				case got = <-done:
				case <-ctx.Done():
					t.Fatal("legacy page did not finish after its callback was released")
				}
				if got.err != nil || got.result.Visited != 2 || got.result.Completed != 2 || got.result.Failed != 0 || got.result.Timings == nil {
					t.Fatal("phase instrumentation changed the completed financial page", got.err)
				}
				timings := got.result.Timings
				if timings.Selection.Count != 2 || timings.Financial.Count != 2 || timings.JoinedPosts.Count != 2 ||
					timings.Mirror.Count != 2 || timings.ColdCensus.Count != 1 || timings.Clock.Count != 2 || timings.Stream.Count != 2 {
					t.Fatalf("actual phase ownership differed: %+v", timings)
				}
				requireLegacySettlementTestState(t, ctx, first, firstId, false, true, 989, 0)
				requireLegacySettlementTestState(t, ctx, second, secondId, false, true, 989, 0)
				requireLegacyProviderDurability(t, ctx, first, firstId, 11)
				requireLegacyProviderDurability(t, ctx, second, secondId, 11)
				requireRedisExpiryClock(t, ctx, "22")
				again, err := FlushLegacySettlements(ctx, shard, nil, 2)
				if err != nil || again.Visited != 0 || again.Completed != 0 || again.Timings.Selection.Count != 1 || again.Timings.Financial.Count != 0 {
					t.Fatal("observed page replay duplicated work or lost the empty selector observation")
				}
				requireRedisExpiryClock(t, ctx, "22")
				t.Logf("family=%s visited=2 committed=2 cold_census=1 grant_available_at_callback=true", family)
			})
		})
	}
}
