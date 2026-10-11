// Local ownership barriers distinguish shared-grant contention from queue bugs.
package model

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A real legacy transaction owns the grant while all fifteen other contract
// shards attempt their pages. Once it proceeds, every contract drains exactly.
func TestLegacySettlementSharedGrantAcrossAllShards(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		const perShard = 32
		const count = LegacySettlementShardCount * perShard
		f := newNetEscrowOrderingTestFixture(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_balance_byte_count=1000000,balance_byte_count=1000000,net_revenue_nano_cents=2000000 WHERE balance_id=$1`, f.balanceId))
		})
		ids := make([]server.Id, count)
		for index := range ids {
			escrow, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
			server.RunPosts(ctx, posts...)
			id := escrow.ContractId
			id[15] = byte(index % LegacySettlementShardCount)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET contract_id=$2 WHERE contract_id=$1`, escrow.ContractId, id))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET contract_id=$2 WHERE contract_id=$1`, escrow.ContractId, id))
			})
			server.Raise(CloseContract(ctx, id, f.sourceId, 11, false))
			server.Raise(CloseContract(ctx, id, f.destinationId, 11, false))
			ids[index] = id
		}
		// Synthetic ID rewrites invalidate the cache. Keep this fixture warm,
		// so every settlement transaction must carry it forward exactly.
		refreshNetEscrow(ctx, []server.Id{f.balanceId})
		before := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId})[f.balanceId]
		acquired := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseOwner := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseOwner()
		stopHooks := startLegacyStatementHooks(t, ctx)
		defer stopHooks()
		trace := &legacyGrantOwnerDiagnostic{afterGrant: func() {
			close(acquired)
			select {
			case <-release:
			case <-ctx.Done():
				server.Raise(ctx.Err())
			}
		}}
		type ownerResult struct {
			err       any
			held      time.Duration
			postsTime time.Duration
		}
		ownerDone := make(chan ownerResult, 1)
		var workers sync.WaitGroup
		workers.Add(1)
		go func() {
			defer workers.Done()
			result := ownerResult{}
			result.err = server.HandleError(func() {
				var posts []func() any
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout='2s'; SET LOCAL lock_timeout='250ms'`))
					var completed, busy bool
					var gate legacySettlementBusyGate
					var err error
					hooked, unhook := withLegacyStatementHooks(ctx, tx, trace.hook())
					defer unhook()
					posts, completed, busy, gate, err = flushLegacySettlementInTx(hooked, tx, ids[0])
					server.Raise(err)
					if !completed || busy || gate != legacySettlementBusyNone {
						panic("diagnostic owner did not settle")
					}
				}, server.TxReadCommitted, server.OptNoRetry())
				result.held = time.Since(trace.acquiredAt) - trace.barrierTime
				started := time.Now()
				server.RunPosts(ctx, posts...)
				result.postsTime = time.Since(started)
			})
			ownerDone <- result
		}()
		defer func() {
			releaseOwner()
			cancel()
			workers.Wait()
		}()
		select {
		case <-acquired:
		case result := <-ownerDone:
			t.Fatal("owner ended before its grant barrier", result.err)
		case <-ctx.Done():
			t.Fatal("owner never acquired grant", ctx.Err())
		}
		type pageResult struct {
			shard int
			page  LegacySettlementFlushResult
			err   error
		}
		pages := make(chan pageResult, LegacySettlementShardCount)
		for shard := 1; shard < LegacySettlementShardCount; shard++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				page, err := FlushLegacySettlements(ctx, shard, nil, perShard)
				pages <- pageResult{shard: shard, page: page, err: err}
			}()
		}
		// The owner admitted the grant's financial ownership keys before its
		// grant lock, so every sibling cohort is refused at that admission.
		busy := 0
		for range LegacySettlementShardCount - 1 {
			result := <-pages
			if result.err != nil || result.page.Completed != 0 || result.page.Failed != 0 || result.page.Visited != perShard || result.page.BusyAdmissionDeferred != perShard || result.page.BusyOrGone != perShard {
				t.Fatalf("sibling shard did not identify actual legacy owner: %+v", result)
			}
			busy += result.page.BusyOrGone
		}
		releaseOwner()
		owner := <-ownerDone
		if owner.err != nil {
			t.Fatal("owner failed after release", owner.err)
		}
		t.Logf("owned grant forced %d skips across15 other shards; owner financial residence excluding synthetic barrier=%s; post_commit=%s; owned_sql_calls=%d", busy, owner.held, owner.postsTime, len(trace.statements))
		for index, statement := range trace.statements {
			t.Logf("owned_statement=%d duration=%s sql=%s", index, statement.elapsed, statement.sql)
		}
		for shard := range LegacySettlementShardCount {
			workers.Add(1)
			go func() {
				defer workers.Done()
				page, err := FlushLegacySettlements(ctx, shard, nil, perShard)
				pages <- pageResult{shard: shard, page: page, err: err}
			}()
		}
		completed, retries := 0, 0
		for range LegacySettlementShardCount {
			result := <-pages
			if result.err != nil || result.page.Failed != 0 || result.page.BusyOrGone != result.page.BusyAdmissionDeferred {
				t.Fatalf("concurrent same-grant page had a different failure: %+v", result)
			}
			completed += result.page.Completed
			retries += result.page.BusyOrGone
		}
		t.Logf("unforced concurrent16-shard wave completed=%d grant_busy=%d; this local timing is not a Main rate", completed, retries)
		for shard := range LegacySettlementShardCount {
			page, err := FlushLegacySettlements(ctx, shard, nil, 64)
			if err != nil || page.Failed != 0 || page.BusyOrGone != 0 || page.Cursor != nil {
				t.Fatalf("released cohort failed its finite drain: %+v %v", page, err)
			}
		}
		// Projection runs after the measured grant ownership and financial drain.
		projectLegacyProviderTotalsForTest(t, ctx)
		server.Db(ctx, func(conn server.PgConn) {
			var pending, terminal, metadata int
			var credit, swept, provided ByteCount
			var sweptRevenue, providedRevenue NanoCents
			server.Raise(conn.QueryRow(ctx, `SELECT
                (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1)),
                (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled'),
                (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2),
                (SELECT COALESCE(sum(payout_byte_count),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($1)),
                (SELECT provided_byte_count FROM account_balance WHERE network_id=$3),
                (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND settled AND settle_time IS NOT NULL AND payout_byte_count=11),
                (SELECT COALESCE(sum(payout_net_revenue_nano_cents),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($1)),
                (SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$3)`, ids, f.balanceId, f.destinationNetworkId).Scan(&pending, &terminal, &credit, &swept, &provided, &metadata, &sweptRevenue, &providedRevenue))
			if pending != 0 || terminal != count || metadata != count || credit != 1000000-11*count || swept != 11*count || provided != swept || sweptRevenue != 11*count || providedRevenue != sweptRevenue {
				t.Fatal("shared-grant conservation failed", pending, terminal, credit, swept, provided, metadata, sweptRevenue, providedRevenue)
			}
		})
		after := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId})[f.balanceId]
		cache := settlementCacheSnapshot(ctx, []server.Id{f.balanceId})[f.balanceId]
		// Each settlement transaction advances the grant's revision twice,
		// whatever its contract count, so the total depends on how the pages
		// grouped the contracts. The warm cache follows every one exactly.
		if after.reserved != 0 || after.revision <= before.revision || cache.reserved != 0 || cache.revision != after.revision {
			t.Fatal("shared-grant revision conservation failed", before, after, cache)
		}
		requireLegacyOwnedMetadataRedis(t, ctx, f.balanceId, 0)
	})
}

// One settlement owns this recorder, read only after its owner returns. It
// records the owner's statements after the grant lock and holds the owner at
// that lock. No production hook or global mutable state is used.
type legacyGrantOwnerDiagnostic struct {
	afterGrant  func()
	acquiredAt  time.Time
	barrierTime time.Duration
	statements  []legacyGrantOwnerStatement
}

// Each completed statement is measured separately from the deliberate barrier.
type legacyGrantOwnerStatement struct {
	sql     string
	elapsed time.Duration
}

// The completed grant lock result arms the barrier; every later statement,
// including a batch as one round trip, is recorded with its duration.
func (self *legacyGrantOwnerDiagnostic) hook() *legacyStatementHook {
	return &legacyStatementHook{after: func(sql string, elapsed time.Duration, _ error) {
		if !self.acquiredAt.IsZero() {
			self.statements = append(self.statements, legacyGrantOwnerStatement{sql: strings.Join(strings.Fields(sql), " "), elapsed: elapsed})
			return
		}
		if strings.Contains(sql, "FOR UPDATE OF balance") {
			self.acquiredAt = time.Now()
			self.afterGrant()
			self.barrierTime = time.Since(self.acquiredAt)
		}
	}}
}
