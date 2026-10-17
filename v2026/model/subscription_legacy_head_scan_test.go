// A head revisit reads only the interval it revisits, however many due intents
// its payer has beyond the forward cursor.
package model

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// Due intents beyond the forward cursor, spread over every shard. Their index
// entries span hundreds of leaf pages.
const legacyHeadScanDense = 50_000

// Index pages one bounded seek reads in each shard: the descent and one leaf.
const legacyHeadScanPagesPerShard = 4

// Records every payer-scoped selection a page issues, with its arguments.
type legacyPayerSelectionTrace struct {
	stateLock sync.Mutex
	queries   []string
	args      [][]any
}

// Retains payer-scoped selections only.
func (self *legacyPayerSelectionTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.TrimSpace(data.SQL), "SELECT payer_page.") {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.queries = append(self.queries, data.SQL)
		self.args = append(self.args, slices.Clone(data.Args))
	}
	return ctx
}

// Nothing to record after a statement.
func (self *legacyPayerSelectionTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// The head revisit statements recorded so far, with their arguments.
func (self *legacyPayerSelectionTrace) heads() ([]string, [][]any) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	var queries []string
	var args [][]any
	for index, query := range self.queries {
		if strings.Contains(query, "(next_attempt_time,contract_id)<=(") {
			queries = append(queries, query)
			args = append(args, self.args[index])
		}
	}
	return queries, args
}

// Shared buffers one execution of the statement touches.
func legacyHeadScanPages(t testing.TB, ctx context.Context, query string, args []any) int {
	t.Helper()
	var plans []testingUrlCompletedPlan
	server.Db(ctx, func(conn server.PgConn) {
		var raw []byte
		server.Raise(conn.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+query, args...).Scan(&raw))
		server.Raise(json.Unmarshal(raw, &plans))
	}, server.OptNoRetry())
	if len(plans) != 1 {
		t.Fatal("missing head revisit plan")
	}
	return int(plans[0].Plan.SharedHits + plans[0].Plan.SharedReads)
}

// A dense payer's continued page revisits an interval with no due head while
// every later intent is still due. Holding the first two forward intents
// keeps the page from settling any of them, so the page issues exactly its
// head revisit and its wrapped retry between two busy forward visits. Each
// revisit must stop at the cursor it revisits; on a payer with millions of
// due intents a revisit that reads the remaining range spends the page's
// whole budget and the turn settles one contract.
func TestLegacyPayerHeadRevisitStopsAtTheForwardCursor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		now := server.NowUtc().Truncate(time.Microsecond)
		cursorTime := now.Add(-time.Hour)
		payerNetworkId, sourceId := server.NewId(), server.NewId()
		destinationNetworkId, destinationId := server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,payer_network_id)
				SELECT md5('synthetic-dense-payer-'||g)::uuid,$1,$2,$3,$4,1,$5,$1 FROM generate_series(1,$6) g`,
				payerNetworkId, sourceId, destinationNetworkId, destinationId, cursorTime, legacyHeadScanDense))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,next_attempt_time)
				SELECT id,get_byte(uuid_send(id),15)%16,'settled',$1::timestamp+g*interval '1 millisecond'
				FROM generate_series(1,$2) g CROSS JOIN LATERAL (SELECT md5('synthetic-dense-payer-'||g)::uuid AS id) AS synthetic`,
				cursorTime, legacyHeadScanDense))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE transfer_contract`))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE legacy_settlement_intent`))
		})
		var registered int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM legacy_settlement_intent WHERE payer_network_id=$1`, payerNetworkId).Scan(&registered))
		})
		if registered != legacyHeadScanDense {
			t.Fatal("synthetic intents did not register their payer", registered)
		}

		// The observation scope must exist before the holder borrows a connection.
		trace := &legacyPayerSelectionTrace{}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		server.Raise(err)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		held, err := conn.Begin(ctx)
		server.Raise(err)
		server.RaisePgResult(held.Exec(ctx, `SELECT contract_id FROM legacy_settlement_intent
			WHERE contract_id IN (md5('synthetic-dense-payer-1')::uuid,md5('synthetic-dense-payer-2')::uuid) FOR UPDATE`))
		cursor := &LegacySettlementCursor{NextAttemptTime: cursorTime, PassEndTime: now.Add(-time.Minute),
			HeadAfter: &LegacySettlementPosition{NextAttemptTime: cursorTime.Add(-time.Second)}}
		page, err := FlushLegacyPayerSettlements(ctx, payerNetworkId, cursor, 2)
		server.Raise(held.Rollback(ctx))
		conn.Release()
		server.Raise(scope.Close())
		if err != nil || page.Visited != 2 || page.BusyOrGone != 2 || page.Completed != 0 || page.Failed != 0 || page.HeadVisited != 0 || !page.More {
			t.Fatalf("page did not revisit an empty head between two held forward intents: %+v, %v", page, err)
		}
		queries, args := trace.heads()
		if len(queries) != 2 {
			t.Fatal("page did not issue its head revisit and wrapped retry", len(queries))
		}
		limit := legacyHeadScanPagesPerShard * LegacySettlementShardCount
		for index, query := range queries {
			if pages := legacyHeadScanPages(t, ctx, query, args[index]); limit < pages {
				t.Fatalf("head revisit %d read %d pages to find no head before a %d-intent due range; limit %d", index, pages, legacyHeadScanDense, limit)
			}
		}
	})
}
