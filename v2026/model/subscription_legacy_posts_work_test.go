// Real finite pages measure dependency dispatches separately from financial work.
package model

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

type legacyPostsWorkKey struct{}

// Counts only commands carrying this measured page's context. A dispatch is a
// client hook invocation, not a claimed cluster-wide network round trip.
type legacyPostsWorkHook struct {
	stateLock  sync.Mutex
	commands   map[string]int
	dispatches map[string]int
}

func (self *legacyPostsWorkHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (self *legacyPostsWorkHook) record(ctx context.Context, commands []redis.Cmder) {
	if ctx.Value(legacyPostsWorkKey{}) != self {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	families := map[string]bool{}
	for _, command := range commands {
		args := command.Args()
		family := ""
		if len(args) > 1 {
			if command.Name() == "incrby" && args[1] == clockTransferByteCountRedisKey {
				family = "clock"
			} else if command.Name() == "eval" && args[1] == netEscrowSnapshotScript {
				family = "mirror"
			} else {
				key, _ := args[1].(string)
				if command.Name() == "eval" && len(args) > 3 {
					key, _ = args[3].(string)
				}
				if strings.HasSuffix(key, "s2_ct_sk") && (command.Name() == "get" || command.Name() == "eval") {
					family = "stream_lookup"
				}
			}
		}
		if family != "" {
			self.commands[family]++
			families[family] = true
		}
	}
	for family := range families {
		self.dispatches[family]++
	}
}

func (self *legacyPostsWorkHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		self.record(ctx, []redis.Cmder{command})
		return next(ctx, command)
	}
}

func (self *legacyPostsWorkHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		self.record(ctx, commands)
		return next(ctx, commands)
	}
}

// A 512-contract cohort uses two funded payers, actual payouts, warm mirrors,
// 32 multi-member streams and two full pages. The same fixture runs against the
// baseline: conservation passes, then its 512 family dispatches violate the
// two-page bound. Wall time is supporting evidence; operation counts are exact.
func TestLegacySettlementPostBatchLoadedWork(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		const count = 512
		owners := []netEscrowOrderingTestFixture{newNetEscrowOrderingTestFixture(t, ctx), newNetEscrowOrderingTestFixture(t, ctx)}
		ids := make([]server.Id, count)
		prefix := server.NewId()
		for index := range ids {
			ids[index] = prefix
			binary.BigEndian.PutUint32(ids[index][11:15], uint32(index+1))
			ids[index][15] = 1
		}
		balances := []server.Id{owners[0].balanceId, owners[1].balanceId}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_balance_byte_count=1000000,
				balance_byte_count=1000000,net_revenue_nano_cents=2000000 WHERE balance_id=ANY($1)`, balances))
			for ownerIndex, owner := range owners {
				var selected []server.Id
				for index, id := range ids {
					if index%2 == ownerIndex {
						selected = append(selected, id)
					}
				}
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
					(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,usage_origin_is_source)
					SELECT id,$2,$3,$4,$5,$2,2,true FROM unnest($1::uuid[]) AS row(id)`, selected,
					owner.sourceNetworkId, owner.sourceId, owner.destinationNetworkId, owner.destinationId))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
					SELECT id,$2,2 FROM unnest($1::uuid[]) AS row(id)`, selected, owner.balanceId))
			}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
				SELECT id,party,1,statement_timestamp() AT TIME ZONE 'UTC',false FROM unnest($1::uuid[]) AS row(id)
				CROSS JOIN (VALUES ('source'),('destination')) AS parties(party)`, ids))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,clear_dispute,next_attempt_time)
				SELECT id,1,'settled',false,'2010-01-01'::timestamp FROM unnest($1::uuid[]) AS row(id)`, ids))
		})
		refreshNetEscrow(ctx, balances)
		intermediaries := make([]server.Id, 32)
		for index := range intermediaries {
			intermediaries[index] = server.NewId()
		}
		for index, id := range ids {
			owner := owners[index%2]
			AddToStream(ctx, id, owner.sourceId, owner.destinationId, []server.Id{intermediaries[index%32]})
		}
		testingResetClock(ctx)
		defer testingResetClock(ctx)
		hook := &legacyPostsWorkHook{commands: map[string]int{}, dispatches: map[string]int{}}
		server.Redis(ctx, func(client server.RedisClient) { client.AddHook(hook) })
		server.RedisDoOnce(ctx, func(client server.RedisClient) { client.AddHook(hook) })
		measured := context.WithValue(ctx, legacyPostsWorkKey{}, hook)
		var cursor *LegacySettlementCursor
		var pages []LegacySettlementFlushResult
		started := time.Now()
		for range 2 {
			page, err := FlushLegacySettlements(measured, 1, cursor, LegacySettlementPageLimit)
			if err != nil || page.Completed != 256 || page.Visited != 256 || page.BusyOrGone != 0 || page.Failed != 0 || page.Cursor == nil {
				t.Fatalf("loaded page lost ordinary financial progress: %+v %v", page, err)
			}
			pages = append(pages, page)
			cursor = page.Cursor
		}
		elapsed := time.Since(started)
		for _, owner := range owners {
			server.Db(ctx, func(conn server.PgConn) {
				var exact bool
				server.Raise(conn.QueryRow(ctx, `SELECT
					(SELECT balance_byte_count=1000000-256 FROM transfer_balance WHERE balance_id=$1)
					AND (SELECT count(*) FROM transfer_contract WHERE source_network_id=$2 AND outcome='settled' AND provider_usage IS NOT NULL)=256
					AND (SELECT count(*) FROM transfer_escrow WHERE balance_id=$1 AND settled)=256
					AND (SELECT sum(payout_byte_count)=256 AND sum(payout_net_revenue_nano_cents)=256 FROM transfer_escrow_sweep WHERE balance_id=$1)
					AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=ANY($3))
					AND (SELECT count(*) FROM pending_task WHERE function_name=$4)=512`, owner.balanceId, owner.sourceNetworkId, ids,
					task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()).Scan(&exact))
				if !exact {
					t.Fatal("loaded pages lost debit, outcome, metadata, usage or payout authority")
				}
			})
			if Testing_NetEscrowByteCount(ctx, owner.balanceId) != 0 {
				t.Fatal("coalesced mirror lost committed zero reservation")
			}
			_, sourceHops := GetStreamHops(ctx, owner.sourceId)
			_, destinationHops := GetStreamHops(ctx, owner.destinationId)
			if len(sourceHops) != 0 || len(destinationHops) != 0 {
				t.Fatal("loaded page left closed stream hops")
			}
		}
		for _, intermediary := range intermediaries {
			_, hops := GetStreamHops(ctx, intermediary)
			if len(hops) != 0 {
				t.Fatal("loaded page left an intermediary hop")
			}
		}
		requireRedisExpiryClock(t, ctx, "512")
		if replay, err := FlushLegacySettlements(measured, 1, nil, LegacySettlementPageLimit); err != nil || replay.Visited != 0 || replay.Completed != 0 {
			t.Fatal("loaded replay repeated financial work", replay, err)
		}
		requireRedisExpiryClock(t, ctx, "512")
		timings, err := json.Marshal(pages)
		server.Raise(err)
		t.Logf("loaded_post_work contracts=512 pages=2 elapsed=%s commands=%v dispatches=%v timings=%s", elapsed, hook.commands, hook.dispatches, timings)
		for _, family := range []string{"clock", "mirror", "stream_lookup"} {
			if hook.dispatches[family] != 2 {
				t.Errorf("%s dispatched %d times for two pages; per-contract round trips remain", family, hook.dispatches[family])
			}
		}
		if hook.commands["clock"] != count || hook.commands["mirror"] != 4 || hook.commands["stream_lookup"] != count {
			t.Fatalf("projection command conservation differs: %v", hook.commands)
		}
	})
}
