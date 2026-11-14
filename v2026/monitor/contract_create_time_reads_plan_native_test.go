// The monitor's reads of a create_time window of contracts must not need an
// index on transfer_contract.create_time: without transfer_contract_create_time
// a create_time range is a scan of the whole contract heap, or a skip scan of
// transfer_contract_destination_id_create_time once per destination. Each test
// captures the exact SQL a signal sends, then plans it natively without that
// index and with sequential scans disabled, over analyzed decoy contracts
// that make the skip scan the cheapest path for a read whose create_time
// window is an index condition, in a transaction that rolls back.
// transfer_contract may only be read as a range of its primary key bounded by
// contract_id.
package monitor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server/v2026"
)

// The SQL the signal sends that contains marker, answered with rows; every
// other query is answered with no rows.
func captureSignalQuery(t testing.TB, signal Signal, marker string, rows []Row) string {
	t.Helper()
	captured := ""
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, marker) {
			captured = query
			return rows, nil
		}
		return nil, nil
	}}
	if _, err := signal.Run(context.Background(), syntheticSettings(source)); err != nil {
		t.Fatal(err)
	}
	if captured == "" {
		t.Fatalf("%s sent no query containing %q", signal.Key(), marker)
	}
	return captured
}

type monitorContractPlanNode struct {
	NodeType       string                    `json:"Node Type"`
	Relation       string                    `json:"Relation Name"`
	Index          string                    `json:"Index Name"`
	IndexCondition string                    `json:"Index Cond"`
	Plans          []monitorContractPlanNode `json:"Plans"`
}

// Plans the query without transfer_contract_create_time and without
// sequential scans, in a transaction that rolls back, and requires every read
// of transfer_contract to be a range of its primary key bounded by
// contract_id. Decoys are contracts created in the 15 minutes before the
// window's start (a timestamptz SQL expression), inside its id range but
// outside it, all from one source to one destination, and analyzed: the id
// range must read past them, while a skip scan of
// transfer_contract_destination_id_create_time descends once and reads only
// the window.
func requireMonitorContractIdRangePlan(t testing.TB, ctx context.Context, query string, windowStartSql string) {
	t.Helper()
	statement := strings.TrimSuffix(strings.TrimSpace(query), ";")
	server.Db(ctx, func(conn server.PgConn) {
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
		server.Raise(err)
		defer func() {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}()
		server.RaisePgResult(tx.Exec(ctx, `DROP INDEX IF EXISTS transfer_contract_create_time`))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO transfer_contract (
				contract_id, source_network_id, source_id, destination_network_id, destination_id,
				transfer_byte_count, create_time
			)
			SELECT
				(lpad(to_hex(floor(extract(epoch FROM decoy.create_time) * 1000)::bigint), 12, '0') || substr(md5(n::text), 1, 20))::uuid,
				$1, $2, $3, $4, 0, decoy.create_time AT TIME ZONE 'UTC'
			FROM generate_series(1, 2000) AS n
			CROSS JOIN LATERAL (
				SELECT (`+windowStartSql+`) - n * interval '400 milliseconds' AS create_time
			) AS decoy
			`,
			server.NewId(), server.NewId(), server.NewId(), server.NewId(),
		))
		server.RaisePgResult(tx.Exec(ctx, `ANALYZE transfer_contract`))
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL enable_seqscan = off`))
		var raw []byte
		server.Raise(tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+statement).Scan(&raw))
		var plans []struct {
			Plan monitorContractPlanNode `json:"Plan"`
		}
		server.Raise(json.Unmarshal(raw, &plans))
		if len(plans) != 1 {
			t.Fatalf("%d plans for one query", len(plans))
		}
		ranged := false
		var inspect func(node monitorContractPlanNode)
		inspect = func(node monitorContractPlanNode) {
			bounded := node.Index == "transfer_contract_pkey" && strings.Contains(node.IndexCondition, "contract_id")
			switch {
			case node.Relation == "transfer_contract" && strings.Contains(node.NodeType, "Seq Scan"):
				t.Fatalf("without transfer_contract_create_time the query reads the whole contract heap:\n%s\n%s", statement, raw)
			case node.Relation == "transfer_contract" && strings.HasPrefix(node.NodeType, "Index") && !bounded:
				t.Fatalf("without transfer_contract_create_time the query reads %s unbounded by contract_id:\n%s\n%s", node.Index, statement, raw)
			case node.NodeType == "Bitmap Index Scan" && strings.HasPrefix(node.Index, "transfer_contract") && !bounded:
				t.Fatalf("without transfer_contract_create_time the query reads %s unbounded by contract_id:\n%s\n%s", node.Index, statement, raw)
			}
			ranged = ranged || bounded
			for _, child := range node.Plans {
				inspect(child)
			}
		}
		inspect(plans[0].Plan)
		if !ranged {
			t.Fatalf("without transfer_contract_create_time the query reads no contract id range:\n%s\n%s", statement, raw)
		}
	})
}

// SIGNALS.md 1.1's per-minute contract count reads only the minute's contract
// id range.
func TestContractRateCountNeedsNoCreateTimeIndex(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		query := captureSignalQuery(t, NewContractRateSignal(), "FROM transfer_contract", []Row{{"5000"}})
		requireMonitorContractIdRangePlan(t, t.Context(), query, `date_trunc('minute', now()) - interval '1 minute'`)
	})
}

// SIGNALS.md §2.20's successful contracts to inactive destinations read only
// the window's contract id range.
func TestStaleContractsReadNeedsNoCreateTimeIndex(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		query := captureSignalQuery(t, NewStaleContractsSignal(), "stale_success", nil)
		requireMonitorContractIdRangePlan(t, t.Context(), query, `now() - interval '5 minutes'`)
	})
}
