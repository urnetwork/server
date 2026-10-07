package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A whole provider may span multiple FETCH pages. The cursor must include
// missing/old lookup rows and preserve its original snapshot through churn;
// the separate primary fact reader must see that churn, not the old snapshot.
func TestArinCurrentPublicCohortStreamsWholeSnapshotAndFreshFacts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		city := egressTestCity(ctx, "Cohort", "Cohort", "Cohort", "zz")
		p := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		q := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		inactive := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		child := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		stale := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		private := egressTestConnect(ctx, t, city, egressTestUnsampled, map[ProvideMode][]byte{ProvideModeNetwork: []byte("private")}, nil)
		_ = private
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, inactive.clientId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET source_client_id=$2 WHERE client_id=$1`, child.clientId, p.clientId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_handler SET heartbeat_time=$2 WHERE handler_id=(SELECT handler_id FROM network_client_connection WHERE connection_id=$1)`, stale.connectionId, server.NowUtc().Add(-3*NetworkClientHandlerHeartbeatTimeout)))
			var columns, values []string
			attrs, err := tx.Query(ctx, `SELECT quote_ident(attname) FROM pg_attribute WHERE attrelid='network_client_connection'::regclass AND attnum>0 AND NOT attisdropped AND attgenerated='' ORDER BY attnum`)
			server.WithPgResult(attrs, err, func() {
				for attrs.Next() {
					var column string
					server.Raise(attrs.Scan(&column))
					columns = append(columns, column)
					if column == "connection_id" {
						values = append(values, "md5('arin-cohort-'||n)::uuid")
					} else {
						values = append(values, "seed."+column)
					}
				}
			})
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_connection (`+strings.Join(columns, ",")+`) SELECT `+strings.Join(values, ",")+` FROM network_client_connection AS seed CROSS JOIN generate_series(1,512) n WHERE seed.connection_id=$1`, p.connectionId))
		})
		count, pages, maxPage := 0, 0, 0
		seenProviders := map[server.Id]bool{}
		var start ArinShadowPublicCohort
		cohort, err := StreamArinShadowPublicCohort(ctx, func(got ArinShadowPublicCohort) error {
			start = got
			if got.Complete || got.Providers != 2 || got.Connections != 514 {
				t.Fatal("source count or premature end marker")
			}
			// This write must finish even with the maintenance cursor's owner
			// held, and it must not silently shrink that cursor's denominator.
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_connection SET connected=false WHERE connection_id=$1`, q.connectionId))
			})
			return nil
		}, func(page []ArinShadowPublicConnection) error {
			pages++
			maxPage = max(maxPage, len(page))
			count += len(page)
			ids := make([]server.Id, len(page))
			for i, row := range page {
				ids[i] = row.ConnectionId
				seenProviders[row.ClientId] = true
				if row.ClientId == p.clientId && row.ProviderConnections != 513 {
					t.Fatal("provider split lost total")
				}
			}
			facts, err := ReadArinShadowCaptureFacts(ctx, ids)
			if err != nil {
				t.Fatal("fresh fact reader stalled behind snapshot owner", err)
			}
			for _, fact := range facts {
				if fact.ConnectionId == q.connectionId && fact.Connected {
					t.Fatal("fresh fact reader reused old cursor snapshot")
				}
			}
			return nil
		})
		if err != nil || !cohort.Complete || cohort.Providers != start.Providers || cohort.Connections != start.Connections || count != 514 || pages != 3 || maxPage != 256 || len(seenProviders) != 2 {
			t.Fatal("full source truncated or changed", err)
		}
		// A canceled consumer cannot certify a complete source or hold its
		// maintenance connection after the bounded cleanup has joined.
		stopCtx, stop := context.WithCancel(ctx)
		partial, err := StreamArinShadowPublicCohort(stopCtx, func(ArinShadowPublicCohort) error { return nil }, func([]ArinShadowPublicConnection) error { stop(); return nil })
		if err == nil || partial.Complete {
			t.Fatal("canceled stream certified complete")
		}
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			var one int
			server.Raise(conn.QueryRow(ctx, `SELECT 1`).Scan(&one))
			if one != 1 {
				t.Fatal("maintenance lease not returned")
			}
		})
	})
}

func TestArinCurrentPublicCohortUsesConnectedRangeAndPointJoins(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Plan", "Plan", "Plan", "zz")
		p := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		server.Db(ctx, func(conn server.PgConn) {
			var handler server.Id
			server.Raise(conn.QueryRow(ctx, `SELECT handler_id FROM network_client_connection WHERE connection_id=$1`, p.connectionId).Scan(&handler))
			for _, spec := range []struct {
				table, key string
				id         server.Id
				where      string
			}{
				{"network_client_connection", "connection_id", p.connectionId, ""},
				{"network_client", "client_id", p.clientId, ""},
				{"provide_key", "client_id", p.clientId, " AND provide_mode=3"},
				{"network_client_handler", "handler_id", handler, ""},
			} {
				table := spec.table
				server.RaisePgResult(conn.Exec(ctx, `CREATE TEMP TABLE `+table+` (LIKE public.`+table+` INCLUDING ALL)`))
				defer func() { server.RaisePgResult(conn.Exec(ctx, `DROP TABLE pg_temp.`+table)) }()
				server.RaisePgResult(conn.Exec(ctx, `ANALYZE pg_temp.`+table))
				var columns, values []string
				attrs, err := conn.Query(ctx, `SELECT quote_ident(attname) FROM pg_attribute WHERE attrelid=$1::regclass AND attnum>0 AND NOT attisdropped AND attgenerated='' ORDER BY attnum`, "pg_temp."+table)
				server.WithPgResult(attrs, err, func() {
					for attrs.Next() {
						var col string
						server.Raise(attrs.Scan(&col))
						columns = append(columns, col)
						switch {
						case col == spec.key:
							values = append(values, "md5('arin-history-'||n)::uuid")
						case table == "network_client_connection" && col == "connected":
							values = append(values, "false")
						default:
							values = append(values, "seed."+col)
						}
					}
				})
				list := strings.Join(columns, ",")
				server.RaisePgResult(conn.Exec(ctx, `INSERT INTO pg_temp.`+table+` (`+list+`) SELECT `+strings.Join(values, ",")+` FROM public.`+table+` seed CROSS JOIN generate_series(1,100000) n WHERE seed.`+spec.key+`=$1`+spec.where, spec.id))
				server.RaisePgResult(conn.Exec(ctx, `INSERT INTO pg_temp.`+table+` (`+list+`) SELECT `+list+` FROM public.`+table+` WHERE `+spec.key+`=$1`+spec.where, spec.id))
			}
			server.RaisePgResult(conn.Exec(ctx, `SET search_path=pg_temp,public`))
			// Match the dedicated read-only reader's explicit index guards and
			// transaction-local plan settings; ordinary query plans are unchanged.
			var indexesReady bool
			server.Raise(conn.QueryRow(ctx, arinShadowPublicIndexGuardSQL).Scan(&indexesReady))
			if !indexesReady {
				t.Fatal("faithful copied catalog is missing required indexes")
			}
			server.RaisePgResult(conn.Exec(ctx, `SET enable_seqscan=off; SET enable_bitmapscan=off`))
			defer func() {
				server.RaisePgResult(conn.Exec(ctx, `RESET search_path`))
				server.RaisePgResult(conn.Exec(ctx, `RESET plan_cache_mode`))
				server.RaisePgResult(conn.Exec(ctx, `RESET enable_seqscan; RESET enable_bitmapscan`))
			}()
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				server.RaisePgResult(conn.Exec(ctx, `SET plan_cache_mode=`+mode))
				for i, sql := range []string{arinShadowPublicConnectionsCountSQL, arinShadowPublicConnectionsCursorSQL} {
					name := fmt.Sprintf("arin_cohort_%s_%d", mode, i)
					server.RaisePgResult(conn.Exec(ctx, `PREPARE `+name+` (timestamp) AS `+sql))
					defer func() { server.RaisePgResult(conn.Exec(ctx, `DEALLOCATE `+name)) }()
					var raw []byte
					rows, err := conn.Query(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE `+name+` ('2000-01-01')`)
					server.WithPgResult(rows, err, func() {
						if !rows.Next() {
							t.Fatal("missing plan")
						}
						server.Raise(rows.Scan(&raw))
					})
					var plan []map[string]any
					if json.Unmarshal(raw, &plan) != nil || len(plan) != 1 {
						t.Fatal("bad plan")
					}
					seen := map[string]bool{}
					var walk func(map[string]any)
					walk = func(node map[string]any) {
						if relation, ok := node["Relation Name"].(string); ok {
							kind, _ := node["Node Type"].(string)
							cond, _ := node["Index Cond"].(string)
							if !strings.HasPrefix(kind, "Index") || cond == "" || node["Actual Loops"] != float64(1) || node["Actual Rows"] != float64(1) {
								t.Fatalf("cohort %s plan escaped active range/point joins: relation=%s kind=%s rows=%v loops=%v", mode, relation, kind, node["Actual Rows"], node["Actual Loops"])
							}
							if relation == "network_client_connection" && !strings.Contains(cond, "connected") {
								t.Fatal("connection history not range bounded")
							}
							seen[relation] = true
						}
						if children, ok := node["Plans"].([]any); ok {
							for _, child := range children {
								walk(child.(map[string]any))
							}
						}
					}
					walk(plan[0]["Plan"].(map[string]any))
					if len(seen) != 4 {
						t.Fatal("missing source relation")
					}
					t.Logf("%s query%d: 100000 historical/unrelated rows per table; one active range row + three PK probes; execution_ms=%v", mode, i, plan[0]["Execution Time"])
				}
			}
			// The runtime guard must refuse a catalog which has the same table
			// names but has lost the connected/client access path. Do not treat
			// enable_seqscan=off as proof that such an index actually exists.
			indexes, err := conn.Query(ctx, `SELECT quote_ident(index_class.relname)
                FROM pg_index i JOIN pg_class index_class ON index_class.oid=i.indexrelid
                JOIN pg_attribute first_key ON first_key.attrelid=i.indrelid AND first_key.attnum=i.indkey[0]
                JOIN pg_attribute second_key ON second_key.attrelid=i.indrelid AND second_key.attnum=i.indkey[1]
                WHERE i.indrelid='pg_temp.network_client_connection'::regclass
                  AND first_key.attname='connected' AND second_key.attname='client_id'`)
			var rangeIndexes []string
			server.WithPgResult(indexes, err, func() {
				for indexes.Next() {
					var name string
					server.Raise(indexes.Scan(&name))
					rangeIndexes = append(rangeIndexes, name)
				}
			})
			if len(rangeIndexes) == 0 {
				t.Fatal("negative catalog fixture did not find the range index")
			}
			for _, name := range rangeIndexes {
				server.RaisePgResult(conn.Exec(ctx, `DROP INDEX pg_temp.`+name))
			}
			server.Raise(conn.QueryRow(ctx, arinShadowPublicIndexGuardSQL).Scan(&indexesReady))
			if indexesReady {
				t.Fatal("missing range index accepted")
			}
		})
	})
}
