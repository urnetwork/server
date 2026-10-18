// Native prepared plans must seek past completed contracts while preserving
// exact payment membership, earning weeks and resumable cursor accounting.
package model

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Counts base-relation visits within one producer, never inclusive ancestors
// or repeated consumers of its materialized result.
type pointsRollupWorkPlan struct {
	Name      string                 `json:"Subplan Name"`
	Relation  string                 `json:"Relation Name"`
	Rows      float64                `json:"Actual Rows"`
	Loops     float64                `json:"Actual Loops"`
	Filtered  float64                `json:"Rows Removed by Filter"`
	Rechecked float64                `json:"Rows Removed by Index Recheck"`
	Hits      int64                  `json:"Shared Hit Blocks"`
	Reads     int64                  `json:"Shared Read Blocks"`
	Plans     []pointsRollupWorkPlan `json:"Plans"`
}

// Returns one producer and its base sweep work without double-counting buffers.
func (self pointsRollupWorkPlan) producer(name string) (pointsRollupWorkPlan, float64, bool) {
	if self.Name == "CTE "+name {
		var visit func(pointsRollupWorkPlan) float64
		visit = func(node pointsRollupWorkPlan) float64 {
			var rows float64
			if node.Relation == "transfer_escrow_sweep" {
				rows = (node.Rows + node.Filtered + node.Rechecked) * node.Loops
			}
			for _, child := range node.Plans {
				rows += visit(child)
			}
			return rows
		}
		return self, visit(self), true
	}
	for _, child := range self.Plans {
		if node, rows, found := child.producer(name); found {
			return node, rows, true
		}
	}
	return pointsRollupWorkPlan{}, 0, false
}

// The exact production SQL runs under both prepared plan modes. Growing the
// completed prefix must not grow the batch or paid producer's sweep-row visits.
func TestPointsBlockRollupQueryWork(t *testing.T) {
	env := server.DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Db(ctx, func(conn server.PgConn) {
			for _, sql := range []string{
				`CREATE TABLE account_payment(payment_id uuid PRIMARY KEY,create_time timestamp NOT NULL,payment_plan_id uuid,block_rollup_cursor uuid,block_rollup_complete boolean NOT NULL DEFAULT false)`,
				`CREATE INDEX points_pending_fixture ON account_payment(create_time,payment_id) WHERE NOT block_rollup_complete`,
				`CREATE TABLE account_point(account_payment_id uuid,point_value bigint,create_time timestamp)`,
				`CREATE INDEX points_positive_fixture ON account_point(account_payment_id) WHERE point_value>0`,
				`CREATE TABLE payment_plan_assignment(payment_plan_id uuid PRIMARY KEY,complete_time timestamp)`,
				`CREATE TABLE transfer_escrow_sweep(payment_id uuid,contract_id uuid,payout_byte_count bigint,sweep_time timestamp)`,
				`CREATE INDEX sweep_payment_contract_fixture ON transfer_escrow_sweep(payment_id,contract_id)`,
				`CREATE TABLE account_payment_block(payment_id uuid,block_number bigint,PRIMARY KEY(payment_id,block_number))`,
				`SET statement_timeout='15s'`,
			} {
				server.RaisePgResult(conn.Exec(ctx, sql))
			}
			cases := []struct {
				name                              string
				prefix, remaining, fanout, stride int
				pending, foreign, interleaved     bool
			}{
				{name: "null_zero", remaining: 1000, fanout: 3, stride: 1},
				{name: "zero_cursor", prefix: 1, remaining: 1000, fanout: 3, stride: 1},
				{name: "prefix512", prefix: 512, remaining: 1000, fanout: 3, stride: 1},
				{name: "prefix4096", prefix: 4096, remaining: 1000, fanout: 3, stride: 1},
				{name: "fanout_null", remaining: 1000, fanout: 8, stride: 1},
				{name: "fanout_prefix", prefix: 4096, remaining: 1000, fanout: 8, stride: 1},
				{name: "empty_null", fanout: 3, stride: 1},
				{name: "empty_cursor", prefix: 4096, fanout: 3, stride: 1},
				{name: "last_one", prefix: 4096, remaining: 1, fanout: 3, stride: 1},
				{name: "past_limit", prefix: 4096, remaining: 1001, fanout: 3, stride: 1},
				{name: "pending_assignment", prefix: 4096, remaining: 1000, fanout: 3, stride: 1, pending: true},
				{name: "sparse_foreign", prefix: 4096, remaining: 1000, fanout: 3, stride: 7, foreign: true},
				{name: "sparse_last", prefix: 4096, remaining: 1, fanout: 3, stride: 7, foreign: true},
				{name: "foreign_null", remaining: 1000, fanout: 3, stride: 1, foreign: true},
				{name: "one_null", remaining: 1, fanout: 3, stride: 1},
				{name: "single_sweep", prefix: 4096, remaining: 1000, fanout: 1, stride: 1},
				{name: "interleaved_prefix", prefix: 4096, remaining: 1000, fanout: 3, stride: 1, interleaved: true},
				{name: "interleaved_null", remaining: 1000, fanout: 3, stride: 1, interleaved: true},
				{name: "large_suffix", prefix: 4096, remaining: 20000, fanout: 3, stride: 1},
			}
			for _, c := range cases {
				server.RaisePgResult(conn.Exec(ctx, `TRUNCATE account_payment,account_point,payment_plan_assignment,transfer_escrow_sweep,account_payment_block`))
				var cursor any
				if c.prefix > 0 {
					cursor = fmt.Sprintf("%032x", (c.prefix-1)*c.stride)
				}
				server.RaisePgResult(conn.Exec(ctx, `INSERT INTO account_payment VALUES('00000000-0000-0000-0000-000000000001',$1,'00000000-0000-0000-0000-000000000003',$2,false);`, SubnetBlockGenesis, cursor))
				server.RaisePgResult(conn.Exec(ctx, `INSERT INTO account_point VALUES('00000000-0000-0000-0000-000000000001',1,$1)`, SubnetBlockGenesis))
				server.RaisePgResult(conn.Exec(ctx, `INSERT INTO transfer_escrow_sweep SELECT '00000000-0000-0000-0000-000000000001',lpad(to_hex(n*$2::int),32,'0')::uuid,1,$3::timestamp+interval '99 weeks' FROM generate_series(0,$1::int-1) AS n`, c.prefix, c.stride, SubnetBlockGenesis))
				// Explicit heap order keeps setup plans from silently changing the
				// buffer comparison when prepared statements become generic.
				order := "n,j"
				if c.interleaved {
					order = "j,n"
				}
				server.RaisePgResult(conn.Exec(ctx, `INSERT INTO transfer_escrow_sweep SELECT '00000000-0000-0000-0000-000000000001',lpad(to_hex(n*$4::int),32,'0')::uuid,CASE WHEN j=1 THEN 0 ELSE 1 END,$5::timestamp+CASE WHEN j=2 THEN interval '-1 day' ELSE (j%4)*interval '1 week' END FROM generate_series($1::int,$1::int+$2::int-1) AS n CROSS JOIN generate_series(0,$3::int-1) AS j ORDER BY `+order, c.prefix, c.remaining, c.fanout, c.stride, SubnetBlockGenesis))
				if c.pending {
					server.RaisePgResult(conn.Exec(ctx, `INSERT INTO payment_plan_assignment VALUES('00000000-0000-0000-0000-000000000003',NULL)`))
				}
				if c.foreign {
					server.RaisePgResult(conn.Exec(ctx, `INSERT INTO transfer_escrow_sweep SELECT '00000000-0000-0000-0000-000000000002',lpad(to_hex(n),32,'0')::uuid,1,$2::timestamp+interval '500 weeks' FROM generate_series(0,$1::int) AS n`, (c.prefix+c.remaining)*c.stride, SubnetBlockGenesis))
				}
				for _, table := range []string{"account_payment", "account_point", "payment_plan_assignment", "transfer_escrow_sweep"} {
					server.RaisePgResult(conn.Exec(ctx, "ANALYZE "+table))
				}
				// Infer parameter types as the production driver does. Declaring
				// them here would hide a scalar limit inferred as text.
				server.RaisePgResult(conn.Exec(ctx, `PREPARE points_rollup_work AS `+pointsBlockRollupSql))
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					server.RaisePgResult(conn.Exec(ctx, "SET plan_cache_mode="+mode))
					server.RaisePgResult(conn.Exec(ctx, `UPDATE account_payment SET block_rollup_cursor=$1,block_rollup_complete=false`, cursor))
					server.RaisePgResult(conn.Exec(ctx, `TRUNCATE account_payment_block`))
					var raw []byte
					query := fmt.Sprintf("EXPLAIN(ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) EXECUTE points_rollup_work(%d,'%s',%d)", pointsBlockRollupBatchContracts, SubnetBlockGenesis.Format(time.DateTime), int64(SubnetBlockDuration/time.Second))
					server.Raise(conn.QueryRow(ctx, query).Scan(&raw))
					var plans []struct{ Plan pointsRollupWorkPlan }
					if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
						t.Fatal("invalid rollup plan", err)
					}
					batch, batchWork, batchFound := plans[0].Plan.producer("batch")
					paid, paidWork, paidFound := plans[0].Plan.producer("paid")
					if !batchFound || !paidFound {
						t.Fatal("missing rollup producer plans")
					}
					selected := min(c.remaining, pointsBlockRollupBatchContracts)
					if c.pending {
						selected = 0
					}
					wantRows := float64(1)
					if c.pending {
						wantRows = 0
					}
					if plans[0].Plan.Rows != wantRows {
						t.Fatalf("payment selection changed: case=%s mode=%s rows=%.0f want=%.0f", c.name, mode, plans[0].Plan.Rows, wantRows)
					}
					limit := float64(selected*c.fanout + 1)
					t.Logf("case=%s mode=%s prefix=%d selected=%d batch_rows=%.0f paid_rows=%.0f batch_hits=%d paid_hits=%d root_hits=%d batch_reads=%d paid_reads=%d", c.name, mode, c.prefix, selected, batchWork, paidWork, batch.Hits, paid.Hits, plans[0].Plan.Hits, batch.Reads, paid.Reads)
					if batchWork > limit || paidWork > float64(selected*c.fanout) {
						t.Errorf("cursor prefix was rescanned: case=%s mode=%s batch=%.0f paid=%.0f limits=%.0f/%d", c.name, mode, batchWork, paidWork, limit, selected*c.fanout)
					}
					var gotCursor *string
					var complete bool
					server.Raise(conn.QueryRow(ctx, `SELECT block_rollup_cursor::text,block_rollup_complete FROM account_payment`).Scan(&gotCursor, &complete))
					wantCursor := cursor
					if selected > 0 {
						wantCursor = fmt.Sprintf("%032x", (c.prefix+selected-1)*c.stride)
					}
					if wantCursor == nil && gotCursor != nil || wantCursor != nil && (gotCursor == nil || server.RequireParseId(*gotCursor) != server.RequireParseId(wantCursor.(string))) || complete != (!c.pending && selected < pointsBlockRollupBatchContracts) {
						t.Fatalf("cursor/completion changed: case=%s mode=%s cursor=%v complete=%t", c.name, mode, gotCursor, complete)
					}
					wantBlocks := []int64{}
					if selected > 0 {
						for j := 0; j < c.fanout; j++ {
							if j != 1 && j != 2 {
								wantBlocks = append(wantBlocks, int64(1+j%4))
							}
						}
					}
					slices.Sort(wantBlocks)
					wantBlocks = slices.Compact(wantBlocks)
					gotBlocks := []int64{}
					rows, err := conn.Query(ctx, `SELECT block_number FROM account_payment_block ORDER BY block_number`)
					server.WithPgResult(rows, err, func() {
						for rows.Next() {
							var block int64
							server.Raise(rows.Scan(&block))
							gotBlocks = append(gotBlocks, block)
						}
					})
					if !slices.Equal(gotBlocks, wantBlocks) {
						t.Fatalf("earning blocks changed: case=%s mode=%s got=%v want=%v", c.name, mode, gotBlocks, wantBlocks)
					}
				}
				server.RaisePgResult(conn.Exec(ctx, `DEALLOCATE points_rollup_work`))
			}
		})
	})
}
