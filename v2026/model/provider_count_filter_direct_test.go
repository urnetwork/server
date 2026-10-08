package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Preserve the exact admitted 0c901 query as the dense-population control.
func testingProviderCountFilterSelfProbeSql() string {
	return `WITH failed_reliability AS MATERIALIZED (
		SELECT DISTINCT observed_reliability.client_id
		FROM client_connection_reliability_score AS observed_reliability
		WHERE NOT (` + providerReliabilityEligibilitySql("observed_reliability.client_id") + `)
	)
	SELECT client_id, arin_risk, arin_non_quality, false
	FROM network_client_location_reliability
	WHERE arin_risk OR arin_non_quality
	UNION ALL
	SELECT failed_reliability.client_id, false, false, true
	FROM failed_reliability
	WHERE EXISTS (
		SELECT 1 FROM network_client_location_reliability AS provider_location
		WHERE provider_location.client_id = failed_reliability.client_id
	)`
}

// These are the exact normalized bodies admitted to the native non-ANALYZE
// plan read. A cost comparison remains a plan estimate, not a runtime claim.
func TestProviderCountFilterDirectMatchesObservedPlanBodies(t *testing.T) {
	for _, test := range []struct{ query, want string }{
		{testingProviderCountFilterSelfProbeSql(), "61eca863229f39ce0962461f03afc8335d1a2496d044de379179ee2e7d8c0339"},
		{providerCountFilterCommonSql(), "7e9dbf0dcdc0021732edecfd03489c97e6ed24b680a8a50012fdc9b3adc893c5"},
	} {
		hash := sha256.Sum256([]byte(strings.Join(strings.Fields(test.query), " ")))
		if hex.EncodeToString(hash[:]) != test.want {
			t.Fatal("query no longer matches the observed plan body")
		}
	}
}

// One hundred thousand clients with three scores each exposes the redundant
// self-probe even when most clients pass. Complete maps must stay identical
// when failed scores become dense; this does not remove the ARIN exception scan.
func TestProviderCountFilterDirectDenseScoreWorkAndParity(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.ApplyDbMigrations = false
	testEnv.Run(t, func(t testing.TB) {
		server.Tx(t.Context(), func(tx server.PgTx) {
			testingProviderCountFilterTables(t, tx)
			for _, query := range []string{
				`SET LOCAL work_mem='64MB'`, `SET LOCAL random_page_cost=1.25`, `SET LOCAL effective_cache_size='512GB'`,
				`INSERT INTO network_client_location_reliability
					SELECT lpad(to_hex(n),32,'0')::uuid,n%997=0,n%1009=0,n<=100000,n%2=0,repeat('x',128)
					FROM generate_series(1,1250000) AS n`,
				`INSERT INTO client_connection_reliability_score
					SELECT lpad(to_hex(n),32,'0')::uuid,lookback,CASE WHEN n%101=0 THEN 0.5 ELSE 1 END
					FROM generate_series(1,100000) AS n CROSS JOIN generate_series(1,3) AS lookback`,
				`ANALYZE network_client_location_reliability`, `ANALYZE client_connection_reliability_score`,
			} {
				server.RaisePgResult(tx.Exec(t.Context(), query))
			}
			for _, dense := range []bool{false, true} {
				if dense {
					server.RaisePgResult(tx.Exec(t.Context(), `UPDATE client_connection_reliability_score
						SET independent_reliability_weight=CASE WHEN get_byte(uuid_send(client_id),15)%4<>0 THEN 0.5 ELSE 1 END`))
					server.RaisePgResult(tx.Exec(t.Context(), `ANALYZE client_connection_reliability_score`))
				}
				baseline := testingProviderCountFilterMaps(t, tx, testingProviderCountFilterSelfProbeSql())
				if !reflect.DeepEqual(baseline, testingProviderCountFilterMaps(t, tx, providerCountFilterCommonSql())) {
					t.Fatal("dense complete-map equivalence failed")
				}
				for _, test := range []struct {
					current bool
					query   string
				}{{false, testingProviderCountFilterSelfProbeSql()}, {true, providerCountFilterCommonSql()}} {
					var raw []byte
					server.Raise(tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+test.query).Scan(&raw))
					var plans []struct{ Plan providerCountFilterPlanNode }
					if json.Unmarshal(raw, &plans) != nil || len(plans) != 1 {
						t.Fatal("invalid local native plan")
					}
					var visited float64
					var scans int
					var walk func(providerCountFilterPlanNode)
					walk = func(node providerCountFilterPlanNode) {
						if node.Relation == "client_connection_reliability_score" {
							visited += (node.Rows + node.Removed) * node.Loops
							scans++
						}
						for _, child := range node.Plans {
							walk(child)
						}
					}
					walk(plans[0].Plan)
					t.Logf("dense=%t direct=%t score_rows_examined=%.0f score_scan_nodes=%d", dense, test.current, visited, scans)
					if test.current {
						if scans != 1 || visited > 300001 {
							t.Fatal("direct failing-score query repeats score work")
						}
					} else if scans < 2 || visited < 599999 {
						t.Fatal("admitted query did not reproduce duplicate score work")
					}
				}
			}
		}, server.OptNoRetry())
	})
}
