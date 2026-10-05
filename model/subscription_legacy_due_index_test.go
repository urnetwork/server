package model

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// This measures the exact production selection statements on a native index.
// A LIMIT cannot bound a volatile Filter over an all-future shard. Each new
// statement must instead take a fresh, indexable cutoff, including cursor reads.
func TestLegacySettlementDueSelectionBoundsFutureShard(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		source, readErr := os.ReadFile("subscription_legacy_settlement.go")
		server.Raise(readErr)
		statements := regexp.MustCompile("(?s)`(SELECT next_attempt_time,contract_id,.*? FROM legacy_settlement_intent.*?)`").FindAllSubmatch(source, -1)
		if len(statements) != 3 {
			t.Fatal("expected initial, continued and head selection statements")
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
			for index, statement := range statements {
				query := string(statement[1])
				args := []any{0}
				if index > 0 {
					args = append(args, server.NowUtc().Add(-time.Second), server.Id{}, server.NowUtc())
				}
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
					t.Fatal("unexpected bounded selection plan")
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
