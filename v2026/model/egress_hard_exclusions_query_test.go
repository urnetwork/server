// Request-only policy parity and plan bounds use disposable synthetic tables.
package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Keep the pre-boundary SQL as a control, with the same shared policy factory.
func hardExclusionUnscopedSqlForTest() string {
	return `SELECT client_id FROM network_client_location_reliability AS provider_location
		WHERE client_id = ANY($1) AND NOT (` + providerEgressEligibilitySql("provider_location") + `)
		UNION SELECT client_id FROM provider_egress_health
		WHERE client_id = ANY($1) AND tls_authentication_failure = true
		UNION SELECT client_id FROM provider_egress_url_security
		WHERE client_id = ANY($1) AND tls_failure`
}

// The indexed keys and nullability match the four durable input relations.
func testingHardExclusionQueryTables(t testing.TB, tx server.PgTx, temporary bool) {
	t.Helper()
	for _, query := range []string{
		`SET LOCAL statement_timeout='30s'`, `SET LOCAL lock_timeout='2s'`,
		`CREATE TEMP TABLE network_client_location_reliability(client_id uuid PRIMARY KEY, arin_risk bool NOT NULL) ON COMMIT DROP`,
		`CREATE TEMP TABLE client_connection_reliability_score(client_id uuid, lookback_index int, independent_reliability_weight double precision NOT NULL, PRIMARY KEY(client_id,lookback_index)) ON COMMIT DROP`,
		`CREATE TEMP TABLE provider_egress_health(client_id uuid PRIMARY KEY, tls_authentication_failure bool NOT NULL, legacy_tls_authentication_failure bool NOT NULL) ON COMMIT DROP`,
		`CREATE TEMP TABLE provider_egress_url_security(client_id uuid, url_key text, tls_failure bool NOT NULL, PRIMARY KEY(client_id,url_key)) ON COMMIT DROP`,
		`CREATE INDEX ON provider_egress_health(client_id) WHERE tls_authentication_failure`,
		`CREATE INDEX ON provider_egress_url_security(client_id,url_key) WHERE tls_failure`,
	} {
		if !temporary {
			query = strings.ReplaceAll(query, "CREATE TEMP TABLE", "CREATE TABLE")
			query = strings.ReplaceAll(query, " ON COMMIT DROP", "")
		}
		server.RaisePgResult(tx.Exec(t.Context(), query))
	}
}

// Missing history is neutral. Missing rollup retains the original standalone
// TLS/URL behavior: a legacy-only health bit without a rollup is not broadened.
func TestProviderHardExclusionsQueryPolicyParity(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.ApplyDbMigrations = false
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Tx(ctx, func(tx server.PgTx) {
			testingHardExclusionQueryTables(t, tx, true)
			type provider struct {
				name      string
				rollup    bool
				risk      bool
				health    bool
				tls       bool
				legacyTls bool
				weights   map[int]float64
				urls      []bool
				excluded  bool
			}
			minimums := providerReliabilityMinimums()
			providers := []provider{
				{name: "healthy_missing_history", rollup: true},
				{name: "risk", rollup: true, risk: true, excluded: true},
				{name: "lookback_one", rollup: true, weights: map[int]float64{1: minimums[1] - 0.001}, excluded: true},
				{name: "lookback_two", rollup: true, weights: map[int]float64{2: minimums[2] - 0.001}, excluded: true},
				{name: "lookback_three", rollup: true, weights: map[int]float64{3: minimums[3] - 0.001}, excluded: true},
				{name: "all_thresholds", rollup: true, weights: minimums},
				{name: "unknown_lookback_negative", rollup: true, weights: map[int]float64{7: -0.001}, excluded: true},
				{name: "unknown_lookback_zero", rollup: true, weights: map[int]float64{7: 0}},
				{name: "health_tls", rollup: true, health: true, tls: true, excluded: true},
				{name: "legacy_tls", rollup: true, health: true, legacyTls: true, excluded: true},
				{name: "mixed_url_findings", rollup: true, urls: []bool{false, true, false}, excluded: true},
				{name: "all_clean", rollup: true, health: true, weights: map[int]float64{0: 1, 1: 1, 2: 1, 3: 1}, urls: []bool{false, false}},
				{name: "no_rollup_reliability", weights: map[int]float64{1: 0}},
				{name: "no_rollup_legacy", health: true, legacyTls: true},
				{name: "no_rollup_tls", health: true, tls: true, excluded: true},
				{name: "no_rollup_url", urls: []bool{true}, excluded: true},
				{name: "unknown"},
				{name: "overlapping_failures", rollup: true, risk: true, health: true, tls: true, legacyTls: true, weights: map[int]float64{1: 0, 2: 0, 3: 0}, urls: []bool{true, true}, excluded: true},
			}
			ids := make([]server.Id, len(providers))
			for i, fixture := range providers {
				ids[i] = server.NewId()
				if fixture.rollup {
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location_reliability VALUES($1,$2)`, ids[i], fixture.risk))
				}
				if fixture.health {
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health VALUES($1,$2,$3)`, ids[i], fixture.tls, fixture.legacyTls))
				}
				for lookback, weight := range fixture.weights {
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO client_connection_reliability_score VALUES($1,$2,$3)`, ids[i], lookback, weight))
				}
				for url, failure := range fixture.urls {
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_url_security VALUES($1,$2,$3)`, ids[i], fmt.Sprint(url), failure))
				}
			}
			outside := server.NewId()
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location_reliability VALUES($1,true)`, outside))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health VALUES($1,true,true)`, outside))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_url_security VALUES($1,'outside',true)`, outside))
			requested := append(append([]server.Id{}, ids...), ids[0], ids[len(ids)-1])
			for _, query := range []string{hardExclusionUnscopedSqlForTest(), providerHardExclusionsSql()} {
				for _, candidates := range [][]server.Id{requested, ids[:1], nil} {
					got := map[server.Id]bool{}
					rows, err := tx.Query(ctx, query, candidates)
					server.WithPgResult(rows, err, func() {
						for rows.Next() {
							var id server.Id
							server.Raise(rows.Scan(&id))
							if got[id] {
								t.Fatal("duplicate exclusion")
							}
							got[id] = true
						}
					})
					if got[outside] {
						t.Fatal("query widened the requested population")
					}
					wanted := 0
					for i, fixture := range providers {
						want := len(candidates) > 1 && fixture.excluded
						if want {
							wanted++
						}
						if got[ids[i]] != want {
							t.Fatalf("%s: got exclusion=%t want=%t", fixture.name, got[ids[i]], want)
						}
					}
					if len(got) != wanted {
						t.Fatal("unknown exclusion population")
					}
				}
			}
		}, server.OptNoRetry())
	})
}

// Plan rows and base-relation scans, not wall-clock timing, guard the fix.
type hardExclusionPlanNode struct {
	NodeType   string                  `json:"Node Type"`
	Relation   string                  `json:"Relation Name"`
	Rows       float64                 `json:"Actual Rows"`
	Loops      float64                 `json:"Actual Loops"`
	Removed    float64                 `json:"Rows Removed by Filter"`
	LocalHits  float64                 `json:"Local Hit Blocks"`
	LocalReads float64                 `json:"Local Read Blocks"`
	Plans      []hardExclusionPlanNode `json:"Plans"`
}

// The exact production query must visit only requested input rows at realistic
// cardinality, including prepared custom and generic plans. The old query is
// retained as an observable control, not selected by any production option.
func TestProviderHardExclusionsQueryPlanIsCandidateBounded(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.ApplyDbMigrations = false
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Tx(ctx, func(tx server.PgTx) {
			testingHardExclusionQueryTables(t, tx, true)
			for _, query := range []string{
				`INSERT INTO network_client_location_reliability SELECT lpad(to_hex(n),32,'0')::uuid,n%97=0 FROM generate_series(1,125000) AS n`,
				`INSERT INTO client_connection_reliability_score SELECT lpad(to_hex(n),32,'0')::uuid,lookback,CASE WHEN n%101=0 THEN 0.5 ELSE 1.0 END FROM generate_series(1,125000) AS n CROSS JOIN generate_series(1,3) AS lookback`,
				`INSERT INTO provider_egress_health SELECT lpad(to_hex(n),32,'0')::uuid,n%103=0,n%107=0 FROM generate_series(1,125000) AS n`,
				`INSERT INTO provider_egress_url_security SELECT lpad(to_hex(n),32,'0')::uuid,url::text,n%109=0 FROM generate_series(1,125000) AS n CROSS JOIN generate_series(1,3) AS url`,
				`ANALYZE network_client_location_reliability`, `ANALYZE client_connection_reliability_score`,
				`ANALYZE provider_egress_health`, `ANALYZE provider_egress_url_security`,
				`PREPARE hard_exclusion_original(uuid[]) AS ` + hardExclusionUnscopedSqlForTest(),
				`PREPARE hard_exclusion_bounded(uuid[]) AS ` + providerHardExclusionsSql(),
			} {
				server.RaisePgResult(tx.Exec(ctx, query))
			}
			for _, count := range []int{1, 20, 256} {
				literals := make([]string, count)
				for i := range literals {
					literals[i] = fmt.Sprintf("'%032x'::uuid", i+1)
				}
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					server.RaisePgResult(tx.Exec(ctx, "SET LOCAL plan_cache_mode="+mode))
					var baselineRows float64
					for _, name := range []string{"hard_exclusion_original", "hard_exclusion_bounded"} {
						var plans []struct {
							Plan          hardExclusionPlanNode
							ExecutionTime float64 `json:"Execution Time"`
						}
						rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE "+name+"(ARRAY["+strings.Join(literals, ",")+"])")
						server.WithPgResult(rows, err, func() {
							if !rows.Next() {
								t.Fatal("missing query plan")
							}
							var raw []byte
							server.Raise(rows.Scan(&raw))
							if json.Unmarshal(raw, &plans) != nil || len(plans) != 1 {
								t.Fatal("invalid query plan")
							}
						})
						var relationRows float64
						sequential := false
						var inspect func(hardExclusionPlanNode)
						inspect = func(node hardExclusionPlanNode) {
							if node.Relation != "" {
								relationRows += (node.Rows + node.Removed) * node.Loops
								sequential = sequential || node.NodeType == "Seq Scan"
							}
							for _, child := range node.Plans {
								inspect(child)
							}
						}
						inspect(plans[0].Plan)
						t.Logf("candidates=%d mode=%s query=%s returned=%.0f examined=%.0f buffers=%.0f duration_ms=%.3f", count, mode, name, plans[0].Plan.Rows, relationRows, plans[0].Plan.LocalHits+plans[0].Plan.LocalReads, plans[0].ExecutionTime)
						if name == "hard_exclusion_original" {
							baselineRows = plans[0].Plan.Rows
							continue
						}
						if plans[0].Plan.Rows != baselineRows {
							t.Fatal("bounded query changed exclusion count")
						}
						if sequential || relationRows > float64(8*count) {
							t.Fatalf("request read fleet inputs: sequential=%t examined=%.0f candidate_bound=%d", sequential, relationRows, 8*count)
						}
					}
				}
			}
			server.RaisePgResult(tx.Exec(ctx, `DEALLOCATE hard_exclusion_original`))
			server.RaisePgResult(tx.Exec(ctx, `DEALLOCATE hard_exclusion_bounded`))
		}, server.OptNoRetry())
	})
}

// Missing, old ready:v1 and expired ready:v2 snapshots all run the actual
// request path across multiple chunks. Expiry is forced into the past, not
// timed with a sleep; candidate reads never publish a partial global cache.
func TestProviderHardExclusionsCacheFallbackChunks(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.ApplyDbMigrations = false
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		ids := make([]server.Id, 530)
		for i := range ids {
			ids[i] = server.NewId()
		}
		outside := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			testingHardExclusionQueryTables(t, tx, false)
			for _, id := range []server.Id{ids[0], ids[len(ids)-1], outside} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location_reliability VALUES($1,true)`, id))
			}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health VALUES($1,true,true)`, outside))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_url_security VALUES($1,'outside',true)`, outside))
		}, server.OptNoRetry())
		requested := append(append([]server.Id{}, ids...), ids[0], ids[len(ids)-1])
		for _, cacheState := range []string{"missing", "legacy", "expired"} {
			hardExclusionTestClear(ctx, t)
			server.Redis(ctx, func(r server.RedisClient) {
				if cacheState == "legacy" {
					server.Raise(r.SAdd(ctx, providerHardExclusionsKey, "ready:v1").Err())
				}
				if cacheState == "expired" {
					server.Raise(r.SAdd(ctx, providerHardExclusionsKey, "ready:v2").Err())
					server.Raise(r.ExpireAt(ctx, providerHardExclusionsKey, time.Unix(1, 0)).Err())
				}
			})
			excluded, err := getProviderHardExclusions(ctx, requested)
			if err != nil || len(excluded) != 2 || !excluded[ids[0]] || !excluded[ids[len(ids)-1]] || excluded[outside] {
				t.Fatalf("%s cache: bounded fallback lost policy, chunks or scope: count=%d err=%v", cacheState, len(excluded), err)
			}
			server.Redis(ctx, func(r server.RedisClient) {
				members, err := r.SMembers(ctx, providerHardExclusionsKey).Result()
				if err != nil || cacheState == "legacy" && (len(members) != 1 || members[0] != "ready:v1") || cacheState != "legacy" && len(members) != 0 {
					t.Fatalf("%s cache: candidate-only read changed publication", cacheState)
				}
			})
		}
	})
}
