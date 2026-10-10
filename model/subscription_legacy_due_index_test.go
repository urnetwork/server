// Legacy settlement due selection takes a fresh, indexable cutoff in every
// statement a page issues.
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
	"github.com/urnetwork/server"
)

// Records each distinct due selection a page issues, with its arguments.
type legacyDueSelectionTrace struct {
	stateLock sync.Mutex
	queries   []string
	args      [][]any
}

// Retains the first execution of each due selection statement.
func (self *legacyDueSelectionTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	query := strings.TrimSpace(data.SQL)
	if strings.HasPrefix(query, "SELECT next_attempt_time,contract_id,") && strings.Contains(query, "FROM legacy_settlement_intent") {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !slices.Contains(self.queries, query) {
			self.queries = append(self.queries, query)
			self.args = append(self.args, slices.Clone(data.Args))
		}
	}
	return ctx
}

// Nothing to record after a statement.
func (self *legacyDueSelectionTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// Real pages issue every due selection shape: the initial page, a continued
// forward read, a head revisit, one bounded below by a persisted head, and its
// wrapped retry. Two held due intents keep each page's forward visits busy, so
// the pages reach their revisits without settling anything. Each captured
// statement then runs on a native index over a shard whose 400,000 rows are all
// in the future. A LIMIT cannot bound a volatile Filter over that shard; every
// statement must instead bound the index range by a fresh cutoff.
func TestLegacySettlementDueSelectionBoundsFutureShard(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const shard = 2
		now := server.NowUtc().Truncate(time.Microsecond)
		cursorTime := now.Add(-time.Hour)
		prefix := server.NewId()
		ids := []server.Id{legacyPayerTestContractId(prefix, 1, shard), legacyPayerTestContractId(prefix, 2, shard)}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,usage_origin_is_source)
 SELECT id,$2,$3,$4,$5,2,true FROM unnest($1::uuid[]) AS requested(id)`,
				ids, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,next_attempt_time)
 SELECT id,$2,'settled',$3::timestamp+ordinality*interval '1 millisecond' FROM unnest($1::uuid[]) WITH ORDINALITY AS requested(id,ordinality)`,
				ids, shard, cursorTime))
		})

		trace := &legacyDueSelectionTrace{}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		server.Raise(err)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		held, err := conn.Begin(ctx)
		server.Raise(err)
		server.RaisePgResult(held.Exec(ctx, `SELECT contract_id FROM legacy_settlement_intent WHERE contract_id=ANY($1) FOR UPDATE`, ids))
		passEnd := now.Add(-time.Minute)
		for index, page := range []struct {
			cursor *LegacySettlementCursor
			limit  int
		}{
			{nil, 1},
			{&LegacySettlementCursor{NextAttemptTime: cursorTime, PassEndTime: passEnd}, 2},
			{&LegacySettlementCursor{NextAttemptTime: cursorTime, PassEndTime: passEnd,
				HeadAfter: &LegacySettlementPosition{NextAttemptTime: cursorTime.Add(-time.Second)}}, 2},
		} {
			result, err := FlushLegacySettlements(ctx, shard, page.cursor, page.limit)
			if err != nil || result.Completed != 0 || result.Failed != 0 || result.BusyOrGone != result.Visited || result.Visited == 0 {
				t.Fatalf("page %d did not visit only held intents: %+v, %v", index, result, err)
			}
		}
		server.Raise(held.Rollback(ctx))
		conn.Release()
		server.Raise(scope.Close())
		shapes := map[string]bool{}
		for _, query := range trace.queries {
			switch {
			case strings.Contains(query, "<=($5,$6)"):
				shapes["wrapped_head"] = true
			case strings.Contains(query, ">($5,$6)"):
				shapes["head_after"] = true
			case strings.Contains(query, "<=($2,$3)"):
				shapes["head"] = true
			case strings.Contains(query, ">($2,$3)"):
				shapes["forward"] = true
			case !strings.Contains(query, "$2"):
				shapes["initial"] = true
			}
		}
		if len(shapes) != 5 {
			t.Fatal("pages did not issue every due selection shape", shapes, len(trace.queries))
		}

		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `CREATE TEMP TABLE legacy_settlement_intent (
 contract_id uuid PRIMARY KEY, shard smallint NOT NULL, next_attempt_time timestamp NOT NULL);
 CREATE INDEX legacy_settlement_intent_due ON legacy_settlement_intent(shard,next_attempt_time,contract_id);
 INSERT INTO legacy_settlement_intent SELECT md5(g::text)::uuid,0,
 (statement_timestamp() AT TIME ZONE 'UTC')+interval '1 hour'+g*interval '1 microsecond'
 FROM generate_series(1,400000)g;
 ANALYZE legacy_settlement_intent;`))
			defer conn.Exec(ctx, "DROP TABLE legacy_settlement_intent")
			failures := 0
			for index, query := range trace.queries {
				// The native shard holds every future row.
				args := slices.Clone(trace.args[index])
				args[0] = 0
				var raw []byte
				server.Raise(conn.QueryRow(ctx, "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+query, args...).Scan(&raw))
				var plan []struct {
					Plan struct {
						Plans []struct {
							Type    string `json:"Node Type"`
							Cond    string `json:"Index Cond"`
							Filter  string `json:"Filter"`
							Removed int    `json:"Rows Removed by Filter"`
							Hit     int    `json:"Local Hit Blocks"`
							Read    int    `json:"Local Read Blocks"`
						} `json:"Plans"`
					} `json:"Plan"`
					MS float64 `json:"Execution Time"`
				}
				server.Raise(json.Unmarshal(raw, &plan))
				if len(plan) != 1 || len(plan[0].Plan.Plans) != 1 {
					t.Fatal("unexpected bounded selection plan", string(raw))
				}
				n := plan[0].Plan.Plans[0]
				bounded := strings.Contains(n.Cond, "next_attempt_time <=") && !strings.Contains(n.Filter, "next_attempt_time") && n.Removed == 0 && n.Hit+n.Read < 32
				t.Logf("selection=%d bounded_index_cutoff=%t future_rows_removed=%d local_buffers=%d execution_ms=%.3f", index, bounded, n.Removed, n.Hit+n.Read, plan[0].MS)
				if !bounded {
					failures++
				}
			}
			if failures != 0 {
				t.Fatalf("%d selections filter future rows instead of bounding the index range", failures)
			}
		})
	})
}

func TestLegacySettlementDueCutoffRefreshesWithinTransaction(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Tx(ctx, func(tx server.PgTx) {
			var due time.Time
			server.Raise(tx.QueryRow(ctx, `SELECT (statement_timestamp() AT TIME ZONE 'UTC')+interval '40 milliseconds'`).Scan(&due))
			server.RaisePgResult(tx.Exec(ctx, `SELECT pg_sleep(0.06)`))
			var byStatement, byTransaction bool
			server.Raise(tx.QueryRow(ctx, `SELECT $1::timestamp <= statement_timestamp() AT TIME ZONE 'UTC', $1::timestamp <= current_timestamp AT TIME ZONE 'UTC'`, due).Scan(&byStatement, &byTransaction))
			if !byStatement || byTransaction {
				t.Fatal("new due work did not become visible on a fresh statement cutoff")
			}
		})
	})
}
