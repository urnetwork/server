// The payer reads' plans once the open set has drained (CPUOPT4 Part D #1).
//
// On Main, 2026-11-04 07:23Z, a transfer_contract vacuum counted 4.41M entries
// in transfer_contract_unresolved_payer_transfer_byte_count, which still held the
// pages of its 79M-entry peak. Priced by pages per entry, with the opaque
// unresolved CASE assumed true for half the table, the partial index lost to a
// PostgreSQL 18 skip scan of (open, payer_network_id, transfer_byte_count) that
// reads every contract the payer ever had: the open-sum went from about 20 ms to
// about 590 ms. This fixture is that shape at test size: few open contracts, and
// unresolved partial indexes that keep the pages of a peak since closed and
// vacuumed away.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

const (
	drainedOpenSetPayers       = 40
	drainedOpenSetRowsPerPayer = 2000
	// one contract of every this many of a payer's stays open: 25 a payer,
	// 1,000 in all
	drainedOpenSetOpenEvery = 80
	// the open contracts the unresolved partial indexes held at their peak, all
	// gone and vacuumed away since
	drainedOpenSetPeakOpen = 300000

	unresolvedPayerIndex    = "transfer_contract_unresolved_payer_transfer_byte_count"
	openPayerCompositeIndex = "transfer_contract_open_payer_network_id_transfer_byte_count"
)

// GetOpenTransferByteCount's read, as the code sends it (openTransferByteCountSql).
const unresolvedPayerOpenSumSql = `
			SELECT
			   COALESCE(SUM(transfer_byte_count), 0)
			FROM transfer_contract
			WHERE
			    payer_network_id = $1 AND
			    -- Isolate this payer aggregate from every false-zero global,
			    -- pair, or create-time partial index.
			    (CASE WHEN outcome IS NULL THEN dispute = false ELSE false END)
			`

// The fixture's ids: deterministic, one family per prefix.
func drainedOpenSetIdSql(prefix string, expression string) string {
	return fmt.Sprintf(`('%s-0000-4000-8000-' || lpad(to_hex(%s), 12, '0'))::uuid`, prefix, expression)
}

// The id of the fixture's payer p.
func drainedOpenSetPayerId(p int) server.Id {
	return server.RequireParseId(fmt.Sprintf("f0000000-0000-4000-8000-%012x", p))
}

// Builds the drained open set in transfer_contract: a peak of open contracts
// for every payer, gone, then each payer's 2,000 contracts with one in 80
// open, and a vacuum that removes the dead entries, keeps the pages and counts
// what is left.
func buildDrainedOpenSet(t testing.TB, ctx context.Context) {
	t.Helper()
	columns := `contract_id, source_network_id, source_id, destination_network_id, destination_id,
		payer_network_id, transfer_byte_count, create_time, expiration_time, close_time, outcome`
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, fmt.Sprintf(`
			INSERT INTO transfer_contract (%s)
			SELECT %s, %s, %s, %s, %s, %s, 1024,
				timestamp '2026-11-01 00:00:00' + i * interval '1 millisecond',
				timestamp '2026-11-01 01:00:00' + i * interval '1 millisecond',
				NULL, NULL
			FROM generate_series(0, %d) AS i`,
			columns,
			drainedOpenSetIdSql("a0000000", "i"),
			drainedOpenSetIdSql("b0000000", "i % 997"), drainedOpenSetIdSql("c0000000", "i % 997"),
			drainedOpenSetIdSql("d0000000", "i % 991"), drainedOpenSetIdSql("e0000000", "i % 991"),
			drainedOpenSetIdSql("f0000000", fmt.Sprintf("i %% %d", drainedOpenSetPayers)),
			drainedOpenSetPeakOpen-1,
		)))
		server.RaisePgResult(conn.Exec(ctx,
			`DELETE FROM transfer_contract WHERE contract_id < 'a1000000-0000-4000-8000-000000000000'::uuid`))
		server.RaisePgResult(conn.Exec(ctx, fmt.Sprintf(`
			INSERT INTO transfer_contract (%s)
			SELECT %s, %s, %s, %s, %s, %s, 1024,
				timestamp '2026-11-02 00:00:00' + i * interval '1 millisecond',
				timestamp '2026-11-02 01:00:00' + i * interval '1 millisecond',
				CASE WHEN (i / %d) %% %d = 0 THEN NULL
					ELSE timestamp '2026-11-02 00:01:00' + i * interval '1 millisecond' END,
				CASE WHEN (i / %d) %% %d = 0 THEN NULL ELSE 'canceled' END
			FROM generate_series(0, %d) AS i`,
			columns,
			drainedOpenSetIdSql("a1000000", "i"),
			drainedOpenSetIdSql("b0000000", "i % 997"), drainedOpenSetIdSql("c0000000", "i % 997"),
			drainedOpenSetIdSql("d0000000", "i % 991"), drainedOpenSetIdSql("e0000000", "i % 991"),
			drainedOpenSetIdSql("f0000000", fmt.Sprintf("i %% %d", drainedOpenSetPayers)),
			drainedOpenSetPayers, drainedOpenSetOpenEvery,
			drainedOpenSetPayers, drainedOpenSetOpenEvery,
			drainedOpenSetPayers*drainedOpenSetRowsPerPayer-1,
		)))
		server.RaisePgResult(conn.Exec(ctx, `VACUUM transfer_contract`))
	}, server.OptReadWrite(), server.OptNoRetry())

	// the shape: a plain table, its open set exact, and a partial index with
	// more pages than entries
	var relkind string
	var openCount int64
	var indexTuples float64
	var indexPages int64
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `
			SELECT
				(SELECT relkind::text FROM pg_class WHERE oid = 'transfer_contract'::regclass),
				(SELECT count(*) FROM transfer_contract WHERE (CASE WHEN outcome IS NULL THEN dispute = false ELSE false END)),
				reltuples, relpages
			FROM pg_class WHERE oid = to_regclass($1)`, unresolvedPayerIndex).Scan(&relkind, &openCount, &indexTuples, &indexPages))
	}, server.OptReadOnly())
	wantOpen := int64(drainedOpenSetPayers * drainedOpenSetRowsPerPayer / drainedOpenSetOpenEvery)
	if relkind != "r" || openCount != wantOpen || int64(indexTuples) != wantOpen || indexPages <= wantOpen {
		t.Fatalf("not the drained shape: relkind=%s open=%d index tuples=%.0f pages=%d (want r, %d, %d, more than %d)",
			relkind, openCount, indexTuples, indexPages, wantOpen, wantOpen, wantOpen)
	}
	t.Logf("drained shape: %d open contracts, %s has %d pages", openCount, unresolvedPayerIndex, indexPages)
}

type unresolvedPayerPlanNode struct {
	NodeType string                    `json:"Node Type"`
	Relation string                    `json:"Relation Name"`
	Index    string                    `json:"Index Name"`
	Cost     float64                   `json:"Total Cost"`
	Plans    []unresolvedPayerPlanNode `json:"Plans"`
}

// Main's planner costs (by-us-fmt-5-edge-2 postgresql.conf), so the crossover
// between the paths is Main's.
const unresolvedPayerPlanSettingsSql = `SET LOCAL random_page_cost = 1.1; SET LOCAL effective_cache_size = '768GB'`

// How each scan of the plan reads transfer_contract (an index name, or the
// node type of a scan without one), and the plan's total cost, under Main's
// planner costs. A read with arguments is explained with them bound, as a
// custom plan. A read without is explained as its generic plan, sent in the
// simple protocol so its placeholders reach the server unbound.
func unresolvedPayerPlan(t testing.TB, ctx context.Context, sql string, args ...any) (scans []string, cost float64) {
	t.Helper()
	var raw []byte
	if 0 < len(args) {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, unresolvedPayerPlanSettingsSql))
			server.Raise(tx.QueryRow(ctx, sql, args...).Scan(&raw))
		}, server.OptReadOnly())
	} else {
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			results, err := conn.Conn().PgConn().Exec(ctx,
				`BEGIN READ ONLY; `+unresolvedPayerPlanSettingsSql+`; `+sql+`; COMMIT`).ReadAll()
			server.Raise(err)
			for _, result := range results {
				if 0 < len(result.Rows) {
					raw = result.Rows[0][0]
				}
			}
		}, server.OptReadOnly())
	}
	var plans []struct {
		Plan unresolvedPayerPlanNode
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("invalid plan: %v %s", err, raw)
	}
	var walk func(unresolvedPayerPlanNode)
	walk = func(node unresolvedPayerPlanNode) {
		if node.Relation == "transfer_contract" {
			if node.Index != "" {
				scans = append(scans, node.Index)
			} else {
				scans = append(scans, node.NodeType)
			}
		}
		for _, child := range node.Plans {
			walk(child)
		}
	}
	walk(plans[0].Plan)
	return scans, plans[0].Plan.Cost
}

// The payer reads' four plans: each read with a payer's id bound, as a custom
// plan, and as the generic plan a cached statement can switch to.
type unresolvedPayerRead struct {
	name string
	sql  string
	args []any
}

func unresolvedPayerReads() []unresolvedPayerRead {
	payerId := drainedOpenSetPayerId(7)
	dueBefore := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	return []unresolvedPayerRead{
		{"open-sum", `EXPLAIN (FORMAT JSON) ` + unresolvedPayerOpenSumSql, []any{payerId}},
		{"open-sum generic", `EXPLAIN (GENERIC_PLAN, FORMAT JSON) ` + unresolvedPayerOpenSumSql, nil},
		{"payer-due", `EXPLAIN (FORMAT JSON) ` + payerDueContractsSql,
			[]any{payerId, dueBefore, dueBefore.Add(-DefaultContractExpiration), 4096}},
		{"payer-due generic", `EXPLAIN (GENERIC_PLAN, FORMAT JSON) ` + payerDueContractsSql, nil},
	}
}

// With the open set drained and the table analyzed, as autovacuum leaves it,
// both payer reads use the unresolved payer partial index, custom and generic,
// and never scan a payer's whole history through the composite index.
func TestPayerReadsUseTheUnresolvedPayerIndexWhenTheOpenSetHasDrained(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		buildDrainedOpenSet(t, ctx)
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_contract`))
		}, server.OptReadWrite(), server.OptNoRetry())

		for _, read := range unresolvedPayerReads() {
			scans, cost := unresolvedPayerPlan(t, ctx, read.sql, read.args...)
			t.Logf("%s: %v cost %.1f", read.name, scans, cost)
			if len(scans) == 0 || slices.ContainsFunc(scans, func(scan string) bool { return scan != unresolvedPayerIndex }) {
				t.Errorf("%s reads transfer_contract through %v, want only %s", read.name, scans, unresolvedPayerIndex)
			}
		}
	})
}
