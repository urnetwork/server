package model

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Opt-in performance evidence uses the fully migrated disposable schema, a
// large historical connection table, and the exact serving query. Timings are
// observations, not flaky CI limits or a production capacity claim.
func TestSubscriberGuardQueryPlan(t *testing.T) {
	if os.Getenv("ARIN_SUBSCRIBER_BENCHMARK") != "1" {
		t.Skip("set ARIN_SUBSCRIBER_BENCHMARK=1 for the disposable workload")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		ctx := t.Context()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_handler (handler_id,heartbeat_time)
				SELECT md5('subscriber-benchmark-handler-'||n)::uuid, $1::timestamp FROM generate_series(0,127) AS n`, server.NowUtc()))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_connection
				(client_id,connection_id,connected,connect_time,disconnect_time,connection_host,connection_service,connection_block,handler_id)
				SELECT md5('subscriber-benchmark-client-'||n)::uuid, md5('subscriber-benchmark-connection-'||n||':'||generation)::uuid,
					generation<=2, now(), CASE WHEN generation>2 THEN now() END, 'synthetic','synthetic','synthetic',
					md5('subscriber-benchmark-handler-'||(n%128))::uuid
				FROM generate_series(1,20000) AS n CROSS JOIN generate_series(1,12) AS generation`))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location
				(client_id,connection_id,city_location_id,region_location_id,country_location_id,arin_quality_verified)
				SELECT md5('subscriber-benchmark-client-'||n)::uuid, md5('subscriber-benchmark-connection-'||n||':'||generation)::uuid,
					$1::uuid,$1::uuid,$1::uuid, NOT (generation=2 AND n%10=0)
				FROM generate_series(1,20000) AS n CROSS JOIN generate_series(1,12) AS generation
				WHERE NOT (generation=2 AND n%20=0)`, server.NewId()))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE network_client_connection`))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE network_client_location`))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE network_client_handler`))
		})
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.SAdd(ctx, providerHardExclusionsKey, providerHardExclusionsReadyMember).Err())
		})
		t.Logf("subscriber_guard_fixture providers=20000 current_connections=40000 historical_connections=200000 locations=239000 migration_version=%d", server.MigrationCount())
		for _, count := range []int{20, 256, 1000, 4000} {
			ids := []server.Id{}
			server.Db(ctx, func(conn server.PgConn) {
				rows, err := conn.Query(ctx, `SELECT md5('subscriber-benchmark-client-'||n)::uuid FROM generate_series(1,$1::int) AS n`, count)
				server.WithPgResult(rows, err, func() {
					for rows.Next() {
						var id server.Id
						server.Raise(rows.Scan(&id))
						ids = append(ids, id)
					}
				})
				var encoded []byte
				server.Raise(conn.QueryRow(ctx, "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+providerSubscriberExclusionsSql,
					ids[:min(256, len(ids))], server.NowUtc().Add(-2*NetworkClientHandlerHeartbeatTimeout)).Scan(&encoded))
				var explain []map[string]any
				server.Raise(json.Unmarshal(encoded, &explain))
				var nodes []map[string]any
				var walk func(map[string]any)
				walk = func(plan map[string]any) {
					node := map[string]any{}
					for _, key := range []string{"Node Type", "Relation Name", "Index Name", "Actual Rows", "Actual Loops", "Shared Hit Blocks", "Shared Read Blocks"} {
						if value, ok := plan[key]; ok {
							node[key] = value
						}
					}
					if plan["Node Type"] == "Seq Scan" && (plan["Relation Name"] == "network_client_connection" || plan["Relation Name"] == "network_client_location") {
						t.Fatal("subscriber guard scans the whole connection or location table")
					}
					nodes = append(nodes, node)
					if children, ok := plan["Plans"].([]any); ok {
						for _, child := range children {
							walk(child.(map[string]any))
						}
					}
				}
				walk(explain[0]["Plan"].(map[string]any))
				diagnostic, err := json.Marshal(nodes)
				server.Raise(err)
				t.Logf("subscriber_guard_plan batch=%d execution_ms=%v nodes=%s", min(256, count), explain[0]["Execution Time"], diagnostic)
			}, server.OptReadOnly(), server.OptNoRetry())
			times := []time.Duration{}
			for iteration := 0; iteration < 31; iteration++ {
				start := time.Now()
				excluded, err := getProviderRequestExclusions(ctx, ids, RankModeQuality)
				if err != nil || len(excluded) != count/10 {
					t.Fatalf("subscriber guard benchmark changed classification: error=%v exclusions=%d want=%d", err, len(excluded), count/10)
				}
				if iteration > 0 {
					times = append(times, time.Since(start))
				}
			}
			slices.Sort(times)
			t.Logf("subscriber_guard_runtime candidates=%d queries=%d p50_ms=%.3f p95_ms=%.3f", count, (count+255)/256, float64(times[len(times)/2])/float64(time.Millisecond), float64(times[28])/float64(time.Millisecond))
		}
	})
}
