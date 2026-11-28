// Complete exception maps must not scan every historical default-false rollup.
package model

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Retain the original full-history query as the deterministic performance control.
func testingProviderCountFilterHistoricalSql() string {
	return `SELECT client_id, arin_risk, arin_non_quality,
		NOT (` + providerReliabilityEligibilitySql("provider_location.client_id") + `)
		FROM network_client_location_reliability AS provider_location`
}

// Nullability, unique keys and the sparse covering index match durable schema.
func testingProviderCountFilterTables(t testing.TB, tx server.PgTx) {
	t.Helper()
	for _, query := range []string{
		`SET LOCAL statement_timeout='30s'`, `SET LOCAL lock_timeout='2s'`,
		`CREATE TEMP TABLE network_client_location_reliability(
			client_id uuid PRIMARY KEY, arin_risk bool NOT NULL, arin_non_quality bool NOT NULL,
			connected bool NOT NULL, valid bool NOT NULL, payload text NOT NULL DEFAULT '') ON COMMIT DROP`,
		`CREATE TEMP TABLE client_connection_reliability_score(
			client_id uuid, lookback_index int, independent_reliability_weight double precision NOT NULL,
			PRIMARY KEY(client_id,lookback_index)) ON COMMIT DROP`,
		`CREATE INDEX network_client_location_reliability_arin_exceptions
			ON network_client_location_reliability(client_id) INCLUDE(arin_risk,arin_non_quality)
			WHERE arin_risk OR arin_non_quality`,
	} {
		server.RaisePgResult(tx.Exec(t.Context(), query))
	}
}

// The caller retains true bits only; union branches may overlap without clearing facts.
func testingProviderCountFilterMaps(t testing.TB, tx server.PgTx, query string) map[server.Id][3]bool {
	t.Helper()
	got := map[server.Id][3]bool{}
	rows, err := tx.Query(t.Context(), query)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var clientId server.Id
			var flags [3]bool
			server.Raise(rows.Scan(&clientId, &flags[0], &flags[1], &flags[2]))
			prior := got[clientId]
			for i, flag := range flags {
				prior[i] = prior[i] || flag
			}
			if prior[0] || prior[1] || prior[2] {
				got[clientId] = prior
			}
		}
	})
	return got
}

// Complete means historical, disconnected and invalid rows too, including
// providers referenced explicitly or by an older candidate cache. Missing
// history stays neutral and scores without a rollup do not invent exclusions.
func TestProviderCountFilterQueryCompleteMapParity(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.ApplyDbMigrations = false
	testEnv.Run(t, func(t testing.TB) {
		server.Tx(t.Context(), func(tx server.PgTx) {
			testingProviderCountFilterTables(t, tx)
			type provider struct {
				name       string
				rollup     bool
				risk       bool
				nonQuality bool
				connected  bool
				valid      bool
				weights    map[int]float64
				failed     bool
			}
			minimums := providerReliabilityMinimums()
			providers := []provider{
				{name: "current_missing_history", rollup: true, connected: true, valid: true},
				{name: "historical_missing_history", rollup: true},
				{name: "current_risk", rollup: true, connected: true, valid: true, risk: true},
				{name: "disconnected_invalid_risk", rollup: true, risk: true},
				{name: "disconnected_valid_non_quality", rollup: true, valid: true, nonQuality: true},
				{name: "connected_invalid_both", rollup: true, connected: true, risk: true, nonQuality: true},
				{name: "lookback_one", rollup: true, weights: map[int]float64{1: minimums[1] - 0.001}, failed: true},
				{name: "lookback_two", rollup: true, weights: map[int]float64{2: minimums[2] - 0.001}, failed: true},
				{name: "lookback_three", rollup: true, weights: map[int]float64{3: minimums[3] - 0.001}, failed: true},
				{name: "exact_thresholds", rollup: true, weights: minimums},
				{name: "unknown_lookback_negative", rollup: true, weights: map[int]float64{7: -0.001}, failed: true},
				{name: "unknown_lookback_zero", rollup: true, weights: map[int]float64{7: 0}},
				{name: "lookback_zero_negative", rollup: true, weights: map[int]float64{0: -0.001}, failed: true},
				{name: "overlapping_failures", rollup: true, risk: true, nonQuality: true, weights: map[int]float64{1: 0, 2: 0, 3: 0}, failed: true},
				{name: "missing_rollup_failed_history", weights: map[int]float64{1: 0, 2: 0}},
				{name: "missing_everything"},
			}
			want := map[server.Id][3]bool{}
			for _, fixture := range providers {
				clientId := server.NewId()
				if fixture.rollup {
					server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO network_client_location_reliability
						(client_id,arin_risk,arin_non_quality,connected,valid) VALUES($1,$2,$3,$4,$5)`,
						clientId, fixture.risk, fixture.nonQuality, fixture.connected, fixture.valid))
				}
				for lookback, weight := range fixture.weights {
					server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO client_connection_reliability_score VALUES($1,$2,$3)`, clientId, lookback, weight))
				}
				if fixture.rollup && (fixture.risk || fixture.nonQuality || fixture.failed) {
					want[clientId] = [3]bool{fixture.risk, fixture.nonQuality, fixture.failed}
				}
			}
			for _, query := range []string{testingProviderCountFilterHistoricalSql(), providerCountFilterCommonSql()} {
				if got := testingProviderCountFilterMaps(t, tx, query); !reflect.DeepEqual(got, want) {
					t.Fatal("complete risk/non-quality/reliability maps changed")
				}
			}
			server.RaisePgResult(tx.Exec(t.Context(), `TRUNCATE client_connection_reliability_score, network_client_location_reliability`))
			if len(testingProviderCountFilterMaps(t, tx, providerCountFilterCommonSql())) != 0 {
				t.Fatal("empty evidence invented exceptions")
			}
		}, server.OptNoRetry())
	})
}

// Count visited base rows, including filtered rows, independently of machine speed.
type providerCountFilterPlanNode struct {
	NodeType   string                        `json:"Node Type"`
	Relation   string                        `json:"Relation Name"`
	Index      string                        `json:"Index Name"`
	Rows       float64                       `json:"Actual Rows"`
	Loops      float64                       `json:"Actual Loops"`
	Removed    float64                       `json:"Rows Removed by Filter"`
	LocalHits  float64                       `json:"Local Hit Blocks"`
	LocalReads float64                       `json:"Local Read Blocks"`
	Plans      []providerCountFilterPlanNode `json:"Plans"`
}

// Historical default-false rows dwarf the current score population. The exact
// old query must fail the same work budget the new production query satisfies.
func TestProviderCountFilterQueryWorkIsExceptionBounded(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.ApplyDbMigrations = false
	testEnv.Run(t, func(t testing.TB) {
		server.Tx(t.Context(), func(tx server.PgTx) {
			testingProviderCountFilterTables(t, tx)
			for _, query := range []string{
				`SET LOCAL random_page_cost=1.25`, `SET LOCAL work_mem='256MB'`, `SET LOCAL effective_cache_size='512GB'`,
				`INSERT INTO network_client_location_reliability
					SELECT lpad(to_hex(n),32,'0')::uuid,n%997=0,n%1009=0,n<=1250,n%2=0,repeat('x',128)
					FROM generate_series(1,1250000) AS n`,
				`INSERT INTO client_connection_reliability_score
					SELECT lpad(to_hex(n),32,'0')::uuid,lookback,CASE WHEN n%101=0 THEN 0.5 ELSE 1 END
					FROM generate_series(1,1250) AS n CROSS JOIN generate_series(1,3) AS lookback WHERE n%17<>0`,
				`ANALYZE network_client_location_reliability`, `ANALYZE client_connection_reliability_score`,
			} {
				server.RaisePgResult(tx.Exec(t.Context(), query))
			}
			for i, query := range []string{testingProviderCountFilterHistoricalSql(), providerCountFilterCommonSql()} {
				var raw []byte
				server.Raise(tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+query).Scan(&raw))
				var plans []struct {
					Plan          providerCountFilterPlanNode
					ExecutionTime float64 `json:"Execution Time"`
				}
				if json.Unmarshal(raw, &plans) != nil || len(plans) != 1 {
					t.Fatal("invalid bulk exception plan")
				}
				var examined float64
				var historicalScan, sparseIndex bool
				var inspect func(providerCountFilterPlanNode)
				inspect = func(node providerCountFilterPlanNode) {
					if node.Relation != "" {
						examined += (node.Rows + node.Removed) * node.Loops
					}
					historicalScan = historicalScan || node.Relation == "network_client_location_reliability" && node.NodeType == "Seq Scan"
					sparseIndex = sparseIndex || node.Index == "network_client_location_reliability_arin_exceptions"
					for _, child := range node.Plans {
						inspect(child)
					}
				}
				inspect(plans[0].Plan)
				t.Logf("sparse=%t returned=%.0f examined=%.0f buffers=%.0f duration_ms=%.3f", i == 1,
					plans[0].Plan.Rows, examined, plans[0].Plan.LocalHits+plans[0].Plan.LocalReads, plans[0].ExecutionTime)
				bounded := examined <= 20000 && plans[0].Plan.Rows <= 10000 && !historicalScan && sparseIndex
				if i == 0 && (bounded || examined < 1250000) {
					t.Fatal("historical query did not reproduce unbounded work")
				}
				if i == 1 && !bounded {
					t.Fatal("complete exception query still scans neutral history")
				}
			}
		}, server.OptNoRetry())
	})
}
