// Compare the exact old optional-bound update with the new owned page against
// synthetic hot-client and unrelated connected history. These plans run only
// on private temporary tables; actual journal concurrency has separate tests.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Preserved source body from the pre-fix owner, including its boolean fallback.
const handlerRetirementBaselineUpdateSql = `
				UPDATE network_client_connection
				SET
					connected = false,
					disconnect_time = $1
				WHERE
					network_client_connection.connected = true AND
					(NOT $2::boolean OR network_client_connection.client_id=ANY($3::uuid[])) AND
					NOT EXISTS (
						SELECT 1
						FROM network_client_handler
						WHERE
							network_client_handler.handler_id = network_client_connection.handler_id
					)
			`

// The whole-relation generic plan is the causal baseline. Neither its work nor
// the candidate's physical bounds are inferred from noisy wall-clock timing.
func TestHandlerRetirementPlansBoundWorkUnderFence(t *testing.T) {
	if testing.Short() {
		t.Skip("handler retirement query-work population")
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		server.RaisePgResult(conn.Exec(ctx, `SET search_path=pg_temp,public;
		 CREATE TEMP TABLE network_client_handler(handler_id uuid PRIMARY KEY);
		 CREATE TEMP TABLE network_client_connection(
		  connection_id uuid PRIMARY KEY,client_id uuid NOT NULL,handler_id uuid NOT NULL,
		  connected boolean NOT NULL,disconnect_time timestamp,connect_time timestamp NOT NULL,extender_id uuid);
		 CREATE INDEX handler_retirement_connected_client ON network_client_connection(connected,client_id);
		 CREATE INDEX handler_retirement_client_connected_extender ON network_client_connection(client_id,connected,extender_id);
		 CREATE INDEX handler_retirement_client_time ON network_client_connection(client_id,connect_time);
		 INSERT INTO network_client_handler SELECT lpad(to_hex(300000000+n),32,'0')::uuid FROM generate_series(1,8192)n`))
		defer func() {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			server.RaisePgResult(conn.Exec(cleanup, `DROP TABLE IF EXISTS pg_temp.network_client_connection; DROP TABLE IF EXISTS pg_temp.network_client_handler; RESET search_path`))
		}()
		clientId := server.NewId()
		missingHandlerId := server.NewId()
		for _, background := range []int{32768, 131072} {
			server.RaisePgResult(conn.Exec(ctx, `TRUNCATE network_client_connection`))
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO network_client_connection
			 (connection_id,client_id,handler_id,connected,connect_time)
			 SELECT lpad(to_hex(n),32,'0')::uuid,
			 CASE WHEN n<=64+$3 THEN $1::uuid ELSE lpad(to_hex(100000000+n),32,'0')::uuid END,
			 CASE WHEN n<=64 THEN $2::uuid ELSE lpad(to_hex(300000001+n%8192),32,'0')::uuid END,
			 true,timestamp '2026-01-01' FROM generate_series(1,64+2*$3::integer)n`, clientId, missingHandlerId, background))
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE network_client_connection; ANALYZE network_client_handler`))
			ids := make([]string, 64)
			for i := range ids {
				ids[i] = "'" + orphanSweepTestId(i+1).String() + "'"
			}
			keyArray := "ARRAY[" + strings.Join(ids, ",") + "]::uuid[]"
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				for _, candidate := range []bool{false, true} {
					func() {
						tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
						server.Raise(err)
						defer func() {
							cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
							defer stop()
							_ = tx.Rollback(cleanup)
							for _, name := range []string{"handler_baseline", "handler_lock", "handler_retire"} {
								_, _ = conn.Exec(cleanup, "DEALLOCATE "+name)
							}
						}()
						server.RaisePgResult(tx.Exec(ctx, `SET LOCAL jit=off; SET LOCAL statement_timeout='20s'; SET LOCAL lock_timeout='3s'; SET LOCAL plan_cache_mode=`+mode))
						var connectionWork, handlerWork float64
						pointLock, tidUpdate := false, false
						inspect := func(query string) {
							var raw []byte
							server.Raise(tx.QueryRow(ctx, `EXPLAIN(ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) `+query).Scan(&raw))
							var plans []struct{ Plan orphanSweepTestPlanNode }
							server.Raise(json.Unmarshal(raw, &plans))
							if len(plans) != 1 {
								t.Fatal("handler retirement plan missing")
							}
							var walk func(orphanSweepTestPlanNode)
							walk = func(node orphanSweepTestPlanNode) {
								if node.Relation != "" && node.NodeType != "ModifyTable" {
									work := (node.Rows + node.Removed + node.JoinRemoved + node.RecheckRemoved) * node.Loops
									if node.Relation == "network_client_connection" {
										connectionWork += work
										if candidate {
											switch node.NodeType {
											case "Tid Scan":
												tidUpdate = true
											case "Index Scan":
												if node.Index != "network_client_connection_pkey" || !strings.Contains(node.IndexCondition, "connection_id =") || node.Rows+node.Removed > 1 || node.Loops > 64 {
													t.Fatal("owned page lost its complete primary-key point lock", node.Index, node.IndexCondition)
												}
												pointLock = true
											default:
												t.Fatal("owned page used an unbounded connection access path", node.NodeType)
											}
										}
									}
									if node.Relation == "network_client_handler" {
										handlerWork += work
										if candidate && (node.Index != "network_client_handler_pkey" || !strings.Contains(node.IndexCondition, "handler_id =") || node.Rows+node.Removed > 1 || node.Loops > 64) {
											t.Fatal("handler recheck lost its indexed point bound")
										}
									}
								}
								for _, child := range node.Plans {
									walk(child)
								}
							}
							walk(plans[0].Plan)
						}
						if !candidate {
							server.RaisePgResult(tx.Exec(ctx, `PREPARE handler_baseline(timestamp,boolean,uuid[]) AS `+handlerRetirementBaselineUpdateSql))
							inspect(fmt.Sprintf("EXECUTE handler_baseline(timestamp '2026-01-02',true,ARRAY['%s']::uuid[])", clientId))
						} else {
							server.RaisePgResult(tx.Exec(ctx, `PREPARE handler_lock(uuid[],uuid) AS `+networkClientOrphanConnectionLockSql))
							lockCommand := fmt.Sprintf("EXECUTE handler_lock(%s,'%s')", keyArray, clientId)
							inspect(lockCommand)
							rows, err := tx.Query(ctx, lockCommand, pgx.QueryExecModeExec)
							server.Raise(err)
							var addresses []string
							for rows.Next() {
								var address string
								server.Raise(rows.Scan(&address))
								addresses = append(addresses, address)
							}
							rowErr := rows.Err()
							rows.Close()
							server.Raise(rowErr)
							if len(addresses) != 64 {
								t.Fatal("point-lock fixture lost candidates", len(addresses))
							}
							server.RaisePgResult(tx.Exec(ctx, `PREPARE handler_retire(timestamp,text[]) AS `+networkClientOrphanConnectionRetireSql))
							inspect(fmt.Sprintf("EXECUTE handler_retire(timestamp '2026-01-02',ARRAY['%s']::text[])", strings.Join(addresses, "','")))
						}
						var retired, wrong int
						server.Raise(tx.QueryRow(ctx, `SELECT count(*) FILTER(WHERE NOT connected),count(*) FILTER(WHERE NOT connected AND (client_id<>$1 OR handler_id<>$2)) FROM network_client_connection`, clientId, missingHandlerId).Scan(&retired, &wrong))
						if retired != 64 || wrong != 0 {
							t.Fatal("plan mutation changed the exact eligible set", retired, wrong)
						}
						if candidate && (connectionWork > 128 || handlerWork > 64 || !pointLock || !tidUpdate) {
							t.Fatal("owned page work grew beyond its selected keys", connectionWork, handlerWork, pointLock, tidUpdate)
						}
						if !candidate && mode == "force_generic_plan" && connectionWork < float64(background) {
							t.Fatal("baseline fixture did not reproduce the optional-bound broad scan", connectionWork)
						}
						t.Logf("background=%d mode=%s candidate=%t connection_work=%.0f handler_work=%.0f", background, mode, candidate, connectionWork, handlerWork)
					}()
				}
			}
		}
	})
}
