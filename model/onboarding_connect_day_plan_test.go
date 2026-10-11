// The connect.day probe runs for every uncached client connection, so its read
// must stay within the connecting network's day however stale the statistics.
package model

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Captures the connect.day conditional insert exactly as the real writer sends it.
type connectDayProbeCapture struct {
	mutex sync.Mutex
	sql   string
	args  []any
}

func (self *connectDayProbeCapture) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "INSERT INTO network_onboarding_event") && strings.Contains(data.SQL, "FROM network_client nc") {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		self.sql = data.SQL
		self.args = append([]any(nil), data.Args...)
	}
	return ctx
}

func (self *connectDayProbeCapture) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// A new UTC day lies past the last ANALYZE of `at` until a tenth of the table
// changes. PostgreSQL then estimated the whole day at one row under the
// (name, at) index and preferred it to the network's own index, so every
// probe read every network's connect.day row of the day (3.6k buffers per call
// on Main late in the day). The probe must read only its own network's day.
func TestRecordConnectDayProbeReadsOnlyItsNetworkDay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
		// fewer same-day rows than autoanalyze's 50 + 10% threshold, and
		// autovacuum off for the table, so the histogram stays at its ANALYZE
		const historyNetworkCount = 400
		const historyDayCount = 10
		const sameDayNetworkCount = 300

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE network_onboarding_event SET (autovacuum_enabled = false)`))
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO network_onboarding_event (event_id, network_id, name, at, received_at)
				SELECT
					md5('connect-day-plan-event-' || network_index || '-' || day_index)::uuid,
					md5('connect-day-plan-network-' || network_index)::uuid,
					$1,
					$2::timestamp - (day_index + 1) * interval '1 day' + network_index * interval '1 minute',
					$2::timestamp
				FROM generate_series(1, $3::int) AS network_index, generate_series(0, $4::int - 1) AS day_index
			`, EventConnectDay, day, historyNetworkCount, historyDayCount))
		})
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE network_onboarding_event`))
		})
		// the probe day's other networks connect before the probe client
		probeNetworkId := server.NewId()
		probeClientId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO network_onboarding_event (event_id, network_id, name, at, received_at)
				SELECT
					md5('connect-day-plan-same-day-' || network_index)::uuid,
					md5('connect-day-plan-network-' || network_index)::uuid,
					$1,
					$2::timestamp + network_index * interval '1 minute',
					$2::timestamp
				FROM generate_series(1, $3::int) AS network_index
			`, EventConnectDay, day, sameDayNetworkCount))
			server.RaisePgResult(tx.Exec(ctx,
				`INSERT INTO network_client (client_id, network_id, active) VALUES ($1, $2, true)`,
				probeClientId, probeNetworkId,
			))
		})

		capture := &connectDayProbeCapture{}
		func() {
			scope, err := server.NewTestPgQueryScope(ctx, capture)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := scope.Close(); err != nil {
					t.Error(err)
				}
			}()
			waitConnectDayWrites(recordConnectDayForTest(ctx, probeClientId, day.Add(23*time.Hour)))
		}()
		if capture.sql == "" {
			t.Fatal("the connect.day writer did not send its conditional insert")
		}
		if count := connectDayEventCount(t, ctx, probeNetworkId); count != 1 {
			t.Fatalf("probe network stored %d connect.day events, want 1", count)
		}

		// Replaying the real statement now matches the probe's own row, which
		// is the last connect.day row of the day in (name, at) order.
		var planJson string
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+capture.sql, capture.args...).Scan(&planJson))
		})
		var plans []struct {
			Plan map[string]any `json:"Plan"`
		}
		if err := json.Unmarshal([]byte(planJson), &plans); err != nil || len(plans) != 1 {
			t.Fatalf("unreadable plan %q: %v", planJson, err)
		}
		examined := 0.0
		var walk func(node map[string]any)
		walk = func(node map[string]any) {
			if node["Relation Name"] == "network_onboarding_event" && node["Node Type"] != "ModifyTable" {
				rows, _ := node["Actual Rows"].(float64)
				loops, _ := node["Actual Loops"].(float64)
				removed, _ := node["Rows Removed by Filter"].(float64)
				examined += rows*loops + removed
			}
			children, _ := node["Plans"].([]any)
			for _, child := range children {
				if childNode, ok := child.(map[string]any); ok {
					walk(childNode)
				}
			}
		}
		walk(plans[0].Plan)
		if 2 < examined {
			t.Fatalf("connect.day probe examined %.0f event rows, want only its network's day:\n%s", examined, planJson)
		}
		if count := connectDayEventCount(t, ctx, probeNetworkId); count != 1 {
			t.Fatalf("probe replay stored %d connect.day events, want 1", count)
		}
	})
}
