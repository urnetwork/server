// Local ownership barriers distinguish shared-grant contention from queue bugs.
package model

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
		before := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId})[f.balanceId]
		acquired := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseOwner := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseOwner()
		trace := &legacyGrantOwnerDiagnosticTx{afterGrant: func() {
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
					trace.PgTx = tx
					var completed, busy bool
					var gate legacySettlementBusyGate
					var err error
					posts, completed, busy, gate, err = flushLegacySettlementInTx(ctx, trace, ids[0])
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
		busy := 0
		for range LegacySettlementShardCount - 1 {
			result := <-pages
			if result.err != nil || result.page.Completed != 0 || result.page.Failed != 0 || result.page.Visited != perShard || result.page.BusyGrantSetMismatch != perShard || result.page.BusyOrGone != perShard {
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
			if result.err != nil || result.page.Failed != 0 || result.page.BusyOrGone != result.page.BusyGrantSetMismatch {
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
		if after.reserved != 0 || after.revision != before.revision+2*count || cache.reserved != 0 || cache.revision != after.revision {
			t.Fatal("shared-grant revision conservation failed", before, after, cache)
		}
		requireLegacyOwnedMetadataRedis(t, ctx, f.balanceId, 0)
	})
}

// A single transaction owns this recorder; its result channel publishes it only
// after all calls finish. No production hook or global mutable state is used.
type legacyGrantOwnerDiagnosticTx struct {
	server.PgTx
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

// Record only calls made after this transaction acquired its financial grant.
func (self *legacyGrantOwnerDiagnosticTx) record(sql string, started time.Time) {
	if !self.acquiredAt.IsZero() {
		self.statements = append(self.statements, legacyGrantOwnerStatement{sql: strings.Join(strings.Fields(sql), " "), elapsed: time.Since(started)})
	}
}

// Ordinary statements still execute on the original real transaction.
func (self *legacyGrantOwnerDiagnosticTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	started := time.Now()
	tag, err := self.PgTx.Exec(ctx, sql, args...)
	self.record(sql, started)
	return tag, err
}

// Result consumption ends the timed query; the first grant result arms the barrier.
func (self *legacyGrantOwnerDiagnosticTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	started := time.Now()
	rows, err := self.PgTx.Query(ctx, sql, args...)
	if err != nil {
		return rows, err
	}
	return &legacyGrantOwnerDiagnosticRows{Rows: rows, close: func() {
		if self.acquiredAt.IsZero() && strings.Contains(sql, "FOR UPDATE OF balance SKIP LOCKED") {
			self.acquiredAt = time.Now()
			self.afterGrant()
			self.barrierTime = time.Since(self.acquiredAt)
		} else {
			self.record(sql, started)
		}
	}}, nil
}

// The scan consumes the actual row before publishing its duration.
func (self *legacyGrantOwnerDiagnosticTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	started := time.Now()
	return &legacyGrantOwnerDiagnosticRow{Row: self.PgTx.QueryRow(ctx, sql, args...), finish: func() { self.record(sql, started) }}
}

// A batch retains its real results and records one round trip when closed.
func (self *legacyGrantOwnerDiagnosticTx) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	started := time.Now()
	sql := fmt.Sprintf("BATCH %d", len(batch.QueuedQueries))
	for _, item := range batch.QueuedQueries {
		sql += " | " + item.SQL
	}
	return &legacyGrantOwnerDiagnosticBatch{BatchResults: self.PgTx.SendBatch(ctx, batch), finish: func() { self.record(sql, started) }}
}

// Closing is idempotent even if a helper releases a result more than once.
type legacyGrantOwnerDiagnosticRows struct {
	pgx.Rows
	close func()
}

// Close the real result before any barrier or callback runs.
func (self *legacyGrantOwnerDiagnosticRows) Close() {
	self.Rows.Close()
	if self.close != nil {
		finish := self.close
		self.close = nil
		finish()
	}
}

// QueryRow owns one scan and its completion callback.
type legacyGrantOwnerDiagnosticRow struct {
	pgx.Row
	finish func()
}

// Return the original scan error unchanged.
func (self *legacyGrantOwnerDiagnosticRow) Scan(dest ...any) error {
	err := self.Row.Scan(dest...)
	self.finish()
	return err
}

// Batch timing includes every original result's consumption.
type legacyGrantOwnerDiagnosticBatch struct {
	pgx.BatchResults
	finish func()
}

// Return the original batch error unchanged.
func (self *legacyGrantOwnerDiagnosticBatch) Close() error {
	err := self.BatchResults.Close()
	self.finish()
	return err
}
