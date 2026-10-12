// Production discovery runs unchanged against session-private copies of both
// tables that replay every migrated connection index under its published name.
// The preserved scan of every open row is the result oracle and the causal
// baseline; work is read from actual plan row counts, not from elapsed time.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Preserved source body of the pre-820 discovery.
const handlerRetirementBaselineDiscoverySql = `SELECT DISTINCT client_id FROM network_client_connection c
		 WHERE c.connected AND NOT EXISTS(SELECT 1 FROM network_client_handler h WHERE h.handler_id=c.handler_id)
		 ORDER BY client_id LIMIT 4096`

const handlerRetirementDiscoveryTestIndex = "network_client_connection_connected_handler_id"

// Shadow both tables through search_path. Returns the migrated open-row handler
// index definition retargeted to the copy, so a test can remove and restore it.
func handlerRetirementDiscoveryTestTables(t testing.TB, ctx context.Context, conn server.PgConn) string {
	t.Helper()
	server.RaisePgResult(conn.Exec(ctx, `SET search_path=pg_temp,public;
	 CREATE TEMP TABLE network_client_handler (LIKE public.network_client_handler INCLUDING ALL);
	 CREATE TEMP TABLE network_client_connection (LIKE public.network_client_connection INCLUDING DEFAULTS);
	 ALTER TABLE network_client_connection ADD PRIMARY KEY (connection_id)`))
	type migratedIndex struct{ name, definition string }
	var migratedIndexes []migratedIndex
	rows, err := conn.Query(ctx, `SELECT index_relation.relname::text, pg_get_indexdef(index_record.indexrelid)
	 FROM pg_index index_record JOIN pg_class index_relation ON index_relation.oid=index_record.indexrelid
	 WHERE index_record.indrelid='public.network_client_connection'::regclass AND NOT index_record.indisprimary
	 ORDER BY 1`)
	server.Raise(err)
	for rows.Next() {
		var index migratedIndex
		server.Raise(rows.Scan(&index.name, &index.definition))
		migratedIndexes = append(migratedIndexes, index)
	}
	rowErr := rows.Err()
	rows.Close()
	server.Raise(rowErr)
	handlerIndexDefinition := ""
	for _, index := range migratedIndexes {
		if strings.Count(index.definition, " ON public.network_client_connection ") != 1 {
			t.Fatal("migrated connection index does not belong to the expected table", index.name)
		}
		definition := strings.Replace(index.definition, " ON public.network_client_connection ", " ON pg_temp.network_client_connection ", 1)
		server.RaisePgResult(conn.Exec(ctx, definition))
		if index.name == handlerRetirementDiscoveryTestIndex {
			handlerIndexDefinition = definition
		}
	}
	if handlerIndexDefinition == "" {
		t.Fatal("migrated schema lacks the open connection handler index")
	}
	return handlerIndexDefinition
}

// The pooled connection returns to other owners without the shadow tables.
func handlerRetirementDiscoveryTestCleanup(t testing.TB, conn server.PgConn) {
	cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if _, err := conn.Exec(cleanup, `DROP TABLE IF EXISTS pg_temp.network_client_connection, pg_temp.network_client_handler; RESET search_path`); err != nil {
		t.Errorf("discovery fixture cleanup: %v", err)
	}
}

// Client ids in the order the statement returns them.
func handlerRetirementDiscoveryTestClientIds(t testing.TB, ctx context.Context, conn server.PgConn, query string) []server.Id {
	t.Helper()
	rows, err := conn.Query(ctx, query)
	server.Raise(err)
	var clientIds []server.Id
	for rows.Next() {
		var clientId server.Id
		server.Raise(rows.Scan(&clientId))
		clientIds = append(clientIds, clientId)
	}
	rowErr := rows.Err()
	rows.Close()
	server.Raise(rowErr)
	return clientIds
}

// Every population compares the complete ordered result, including the 4096
// cut, closed rows and dead versions, NULL handler ids, a deleted handler and
// clients whose rows span live, absent and NULL handlers.
func TestHandlerRetirementDiscoveryMatchesOpenRowScan(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		defer handlerRetirementDiscoveryTestCleanup(t, conn)
		handlerRetirementDiscoveryTestTables(t, ctx, conn)
		// Ids derive from md5 labels, so client order is unrelated to insert order.
		const insertRows = `INSERT INTO network_client_connection
		 (connection_id,client_id,handler_id,connected,connect_time,disconnect_time,connection_host,connection_service,connection_block)
		 SELECT md5('row-'||n)::uuid, md5('client-'||(n*7919%%%[1]d))::uuid, %[2]s, %[3]s,
		 timestamp '2026-01-01', NULL, 'connect.example', 'connect', 'test' FROM generate_series(1,%[4]d) n`
		insertLiveHandlers := `INSERT INTO network_client_handler(handler_id,heartbeat_time,handler_host)
		 SELECT md5('live-'||n)::uuid, timestamp '2026-01-01', 'handler.example' FROM generate_series(1,8) n`
		// n%16: 0-7 live, 8-13 and 15 absent, 14 NULL; one in five rows closed.
		mixedHandler := `CASE WHEN n%16<8 THEN md5('live-'||(1+n%8))::uuid WHEN n%16=14 THEN NULL ELSE md5('absent-'||(1+n%6))::uuid END`
		populations := []struct {
			name        string
			sqls        []string
			clientCount int
		}{
			{name: "empty", clientCount: 0},
			{name: "live handlers only", clientCount: 0, sqls: []string{
				insertLiveHandlers,
				fmt.Sprintf(insertRows, 3000, `md5('live-'||(1+n%8))::uuid`, `true`, 6000),
			}},
			{name: "absent handlers under the limit", clientCount: 300, sqls: []string{
				insertLiveHandlers,
				fmt.Sprintf(insertRows, 300, `md5('absent-'||(1+n%3))::uuid`, `true`, 900),
				fmt.Sprintf(strings.Replace(insertRows, "'row-'", "'live-row-'", 1), 300, `md5('live-'||(1+n%8))::uuid`, `true`, 900),
			}},
			{name: "only NULL handler ids", clientCount: 40, sqls: []string{
				fmt.Sprintf(insertRows, 40, `NULL`, `n%3<>0`, 160),
			}},
			{name: "mixed beyond the limit", clientCount: 4096, sqls: []string{
				insertLiveHandlers,
				fmt.Sprintf(insertRows, 6007, mixedHandler, `n%5<>0`, 24000),
				// Dead open versions: closing rows leaves their open index entries.
				`UPDATE network_client_connection SET connected=false, disconnect_time=timestamp '2026-01-02'
				 WHERE connected AND ('x'||substr(connection_id::text,1,8))::bit(32)::int%7=0`,
				// A handler deleted after its rows opened makes them orphans.
				`DELETE FROM network_client_handler WHERE handler_id=md5('live-3')::uuid`,
			}},
		}
		for _, population := range populations {
			server.RaisePgResult(conn.Exec(ctx, `TRUNCATE network_client_connection, network_client_handler`))
			for _, sql := range population.sqls {
				server.RaisePgResult(conn.Exec(ctx, sql))
			}
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE network_client_connection; ANALYZE network_client_handler`))
			expectedClientIds := handlerRetirementDiscoveryTestClientIds(t, ctx, conn, handlerRetirementBaselineDiscoverySql)
			clientIds := handlerRetirementDiscoveryTestClientIds(t, ctx, conn, networkClientOrphanClientSql)
			if len(expectedClientIds) != population.clientCount {
				t.Fatalf("%s: oracle returned %d clients, want %d", population.name, len(expectedClientIds), population.clientCount)
			}
			if !slices.Equal(clientIds, expectedClientIds) {
				t.Fatalf("%s: discovery returned %d clients that differ from the open-row scan's %d", population.name, len(clientIds), len(expectedClientIds))
			}
		}
	})
}

// Growing the open rows of live handlers, and their closed history, must not
// grow discovery. The old statement reads every open row with or without the
// index; the new one reads one row per handler plus the rows of absent handlers.
func TestHandlerRetirementDiscoveryReadsOnlyAbsentHandlers(t *testing.T) {
	if testing.Short() {
		t.Skip("handler retirement discovery work population")
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
		defer cancel()
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		defer handlerRetirementDiscoveryTestCleanup(t, conn)
		handlerIndexDefinition := handlerRetirementDiscoveryTestTables(t, ctx, conn)

		// Rows produced or filtered on each relation, and every index read.
		type discoveryWork struct {
			connectionRows float64
			handlerRows    float64
			connectionSeq  bool
			indexNames     map[string]bool
		}
		explain := func(query string) discoveryWork {
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			server.Raise(err)
			defer func() {
				cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				_ = tx.Rollback(cleanup)
			}()
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL jit=off; SET LOCAL statement_timeout='60s'`))
			var raw []byte
			server.Raise(tx.QueryRow(ctx, `EXPLAIN(ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) `+query).Scan(&raw))
			var plans []struct{ Plan orphanSweepTestPlanNode }
			server.Raise(json.Unmarshal(raw, &plans))
			if len(plans) != 1 {
				t.Fatal("discovery plan missing")
			}
			work := discoveryWork{indexNames: map[string]bool{}}
			var walk func(orphanSweepTestPlanNode)
			walk = func(node orphanSweepTestPlanNode) {
				rows := (node.Rows + node.Removed + node.JoinRemoved + node.RecheckRemoved) * node.Loops
				switch node.Relation {
				case "network_client_connection":
					work.connectionRows += rows
					if node.NodeType == "Seq Scan" {
						work.connectionSeq = true
					}
				case "network_client_handler":
					work.handlerRows += rows
				}
				if node.Index != "" {
					work.indexNames[node.Index] = true
				}
				for _, child := range node.Plans {
					walk(child)
				}
			}
			walk(plans[0].Plan)
			return work
		}

		const liveHandlerCount, absentHandlerCount, absentRowCount, nullRowCount = 24, 3, 192, 4
		var boundedRows []float64
		for _, background := range []int{32768, 131072} {
			server.RaisePgResult(conn.Exec(ctx, `TRUNCATE network_client_connection, network_client_handler`))
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO network_client_handler(handler_id,heartbeat_time,handler_host)
			 SELECT md5('live-'||n)::uuid, timestamp '2026-01-01', 'handler.example' FROM generate_series(1,$1::integer) n`, liveHandlerCount))
			// Open rows of live handlers, then twice as many closed rows on every
			// kind of handler, then the orphan set that discovery must return.
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO network_client_connection
			 (connection_id,client_id,handler_id,connected,connect_time,disconnect_time,connection_host,connection_service,connection_block)
			 SELECT md5('open-'||n)::uuid, md5('client-'||(n%20000))::uuid, md5('live-'||(1+n%$2::integer))::uuid, true,
			  timestamp '2026-01-01', NULL, 'connect.example', 'connect', 'test' FROM generate_series(1,$1::integer) n
			 UNION ALL
			 SELECT md5('closed-'||n)::uuid, md5('client-'||(n%30000))::uuid,
			  CASE WHEN n%3=0 THEN md5('live-'||(1+n%$2::integer))::uuid WHEN n%3=1 THEN md5('absent-'||(1+n%$3::integer))::uuid END,
			  false, timestamp '2026-01-01', timestamp '2026-01-02', 'connect.example', 'connect', 'test' FROM generate_series(1,2*$1::integer) n
			 UNION ALL
			 SELECT md5('orphan-'||n)::uuid, md5('orphan-client-'||(n%96))::uuid, md5('absent-'||(1+n%$3::integer))::uuid, true,
			  timestamp '2026-01-01', NULL, 'connect.example', 'connect', 'test' FROM generate_series(1,$4::integer) n
			 UNION ALL
			 SELECT md5('null-'||n)::uuid, md5('null-client-'||n)::uuid, NULL, true,
			  timestamp '2026-01-01', NULL, 'connect.example', 'connect', 'test' FROM generate_series(1,$5::integer) n`,
				background, liveHandlerCount, absentHandlerCount, absentRowCount, nullRowCount))
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE network_client_connection; ANALYZE network_client_handler`))

			// Pre-migration schema: the old statement reads every open row.
			server.RaisePgResult(conn.Exec(ctx, `DROP INDEX pg_temp.`+handlerRetirementDiscoveryTestIndex))
			expectedClientIds := handlerRetirementDiscoveryTestClientIds(t, ctx, conn, handlerRetirementBaselineDiscoverySql)
			baseline := explain(handlerRetirementBaselineDiscoverySql)
			server.RaisePgResult(conn.Exec(ctx, handlerIndexDefinition))
			// The index alone does not bound the old statement.
			indexedBaseline := explain(handlerRetirementBaselineDiscoverySql)
			clientIds := handlerRetirementDiscoveryTestClientIds(t, ctx, conn, networkClientOrphanClientSql)
			work := explain(networkClientOrphanClientSql)

			if len(expectedClientIds) != 96+nullRowCount || !slices.Equal(clientIds, expectedClientIds) {
				t.Fatalf("discovery changed the orphan clients: %d, want %d", len(clientIds), len(expectedClientIds))
			}
			if baseline.connectionRows < float64(background) || indexedBaseline.connectionRows < float64(background) {
				t.Fatalf("fixture did not reproduce the open-row scan: %.0f, %.0f", baseline.connectionRows, indexedBaseline.connectionRows)
			}
			for indexName := range work.indexNames {
				if strings.HasPrefix(indexName, "network_client_connection") && indexName != handlerRetirementDiscoveryTestIndex {
					t.Fatal("discovery read connections through another index", indexName)
				}
			}
			// One row per handler step, plus the rows it must return.
			handlerCount := float64(liveHandlerCount + absentHandlerCount)
			if work.connectionSeq || !work.indexNames[handlerRetirementDiscoveryTestIndex] ||
				work.connectionRows > handlerCount+absentRowCount+nullRowCount || work.handlerRows > handlerCount+liveHandlerCount {
				t.Fatalf("discovery read beyond its handler steps and orphan rows: %+v", work)
			}
			boundedRows = append(boundedRows, work.connectionRows)
			t.Logf("background=%d baseline_connection_rows=%.0f indexed_baseline_connection_rows=%.0f discovery_connection_rows=%.0f discovery_handler_rows=%.0f",
				background, baseline.connectionRows, indexedBaseline.connectionRows, work.connectionRows, work.handlerRows)
		}
		if boundedRows[0] != boundedRows[1] {
			t.Fatalf("discovery work grew with live open rows: %v", boundedRows)
		}
	})
}
