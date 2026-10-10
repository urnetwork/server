// Clean URL history must not amplify serving-time hard-exclusion reads.
package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Candidate bounds alone do not bound a provider's accumulated clean URLs.
// Inspect the actual base-relation work under custom and generic prepared plans,
// while exact returned identities prove that unresolved TLS findings still win.
func TestProviderHardExclusionsQuerySkipsCleanUrlHistory(t *testing.T) {
	env := server.DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Tx(ctx, func(tx server.PgTx) {
			testingHardExclusionQueryTables(t, tx, true)
			for _, query := range []string{
				`INSERT INTO network_client_location_reliability SELECT lpad(to_hex(n),32,'0')::uuid,false FROM generate_series(1,4096) AS n`,
				`INSERT INTO client_connection_reliability_score SELECT lpad(to_hex(n),32,'0')::uuid,lookback,1.0 FROM generate_series(1,4096) AS n CROSS JOIN generate_series(1,3) AS lookback`,
				`INSERT INTO provider_egress_health SELECT lpad(to_hex(n),32,'0')::uuid,false,false FROM generate_series(1,4096) AS n`,
				`INSERT INTO provider_egress_url_security SELECT lpad(to_hex(n),32,'0')::uuid,'clean-'||url::text,false FROM generate_series(1,4096) AS n CROSS JOIN generate_series(1,93) AS url`,
				`INSERT INTO provider_egress_url_security SELECT lpad(to_hex(n),32,'0')::uuid,'unresolved',true FROM generate_series(1,4096) AS n WHERE n%17=0`,
				`ANALYZE network_client_location_reliability`, `ANALYZE client_connection_reliability_score`,
				`ANALYZE provider_egress_health`, `ANALYZE provider_egress_url_security`,
				`PREPARE hard_exclusion_url_fanout(uuid[]) AS ` + providerHardExclusionsSql(),
			} {
				server.RaisePgResult(tx.Exec(ctx, query))
			}
			var clean, unresolved int
			server.Raise(tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE NOT tls_failure),count(*) FILTER (WHERE tls_failure)
				FROM provider_egress_url_security`).Scan(&clean, &unresolved))
			if clean != 4096*93 || unresolved != 4096/17 {
				t.Fatal("URL fanout fixture lost its clean history or unresolved findings")
			}
			for _, count := range []int{1, 20, 256} {
				literals := make([]string, count)
				wanted := map[server.Id]bool{}
				for index := range literals {
					ordinal := index + 1
					literals[index] = fmt.Sprintf("'%032x'::uuid", ordinal)
					if ordinal%17 == 0 {
						wanted[server.RequireParseId(fmt.Sprintf("00000000-0000-0000-0000-%012x", ordinal))] = true
					}
				}
				query := "EXECUTE hard_exclusion_url_fanout(ARRAY[" + strings.Join(literals, ",") + "])"
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					server.RaisePgResult(tx.Exec(ctx, "SET LOCAL plan_cache_mode="+mode))
					got := map[server.Id]bool{}
					rows, err := tx.Query(ctx, query)
					server.WithPgResult(rows, err, func() {
						for rows.Next() {
							var id server.Id
							server.Raise(rows.Scan(&id))
							if !wanted[id] || got[id] {
								t.Fatal("hard-exclusion query changed exact TLS refusal authority")
							}
							got[id] = true
						}
					})
					if len(got) != len(wanted) {
						t.Fatal("hard-exclusion query lost an unresolved TLS finding")
					}
					var encoded []byte
					server.Raise(tx.QueryRow(ctx, "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+query).Scan(&encoded))
					var plans []struct {
						Plan hardExclusionPlanNode
					}
					if err := json.Unmarshal(encoded, &plans); err != nil || len(plans) != 1 {
						t.Fatal("invalid URL fanout query plan", err)
					}
					var urlRows, relationRows float64
					urlSequential := false
					var inspect func(hardExclusionPlanNode)
					inspect = func(node hardExclusionPlanNode) {
						if node.Relation != "" {
							work := (node.Rows + node.Removed) * node.Loops
							relationRows += work
							if node.Relation == "provider_egress_url_security" {
								urlRows += work
								urlSequential = urlSequential || node.NodeType == "Seq Scan"
							}
						}
						for _, child := range node.Plans {
							inspect(child)
						}
					}
					inspect(plans[0].Plan)
					t.Logf("candidates=%d mode=%s clean_urls_per_provider=93 unresolved=%d url_rows=%.0f all_relation_rows=%.0f", count, mode, len(wanted), urlRows, relationRows)
					if urlSequential || urlRows > float64(len(wanted)) {
						t.Fatalf("hard-exclusion request scanned clean URL history: candidates=%d mode=%s sequential=%t url_rows=%.0f unresolved=%d", count, mode, urlSequential, urlRows, len(wanted))
					}
				}
			}
			server.RaisePgResult(tx.Exec(ctx, `DEALLOCATE hard_exclusion_url_fanout`))
		}, server.OptNoRetry())
	})
}
