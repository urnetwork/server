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

func TestArinCurrentDurableFactsBindExactConnectionAndOriginalLookup(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Shadow", "Shadow", "Shadow", "zz")
		old := server.NowUtc().Add(-48 * time.Hour).Truncate(time.Microsecond)
		p := egressTestConnect(ctx, t, city, egressTestUnsampled, nil,
			&ConnectionLocationScores{ArinLookupAt: &old, ArinDatabaseBuildEpoch: old.Unix() - 1, ArinQualityVerified: true})
		missing := server.NewId()
		read := func() []server.ArinShadowCaptureFacts {
			t.Helper()
			rows, err := ReadArinShadowCaptureFacts(ctx, []server.Id{p.connectionId, missing})
			if err != nil || len(rows) != 2 {
				t.Fatal("exact fact read failed", err)
			}
			return rows
		}
		rows := read()
		if !rows[0].Present || !rows[0].Connected || rows[0].ClientId != p.clientId || !rows[0].Actual.At.Equal(old) ||
			!rows[0].Actual.Verified || rows[0].ObservedAt.Before(server.NowUtc().Add(-time.Second)) || rows[1].Present || rows[1].Connected {
			t.Fatal("fresh read was confused with immutable lookup or absent row")
		}
		for _, which := range []string{"misbound_location", "null_lookup", "disconnect", "missing_location"} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location SET client_id=$2, arin_lookup_at=$3 WHERE connection_id=$1`, p.connectionId, p.clientId, old))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_connection SET connected=true WHERE connection_id=$1`, p.connectionId))
				switch which {
				case "misbound_location":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location SET client_id=$2 WHERE connection_id=$1`, p.connectionId, missing))
				case "null_lookup":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location SET arin_lookup_at=NULL WHERE connection_id=$1`, p.connectionId))
				case "disconnect":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_connection SET connected=false WHERE connection_id=$1`, p.connectionId))
				case "missing_location":
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client_location WHERE connection_id=$1`, p.connectionId))
				}
			})
			row := read()[0]
			if which == "disconnect" {
				if row.Connected {
					t.Fatal("disconnected row qualified")
				}
			} else if row.Present {
				t.Fatalf("%s inherited lookup authority", which)
			}
		}
		if _, err := ReadArinShadowCaptureFacts(ctx, []server.Id{p.connectionId, p.connectionId}); err == nil {
			t.Fatal("duplicate admitted")
		}
		if _, err := ReadArinShadowCaptureFacts(ctx, make([]server.Id, 257)); err == nil {
			t.Fatal("oversized admitted")
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := ReadArinShadowCaptureFacts(canceled, []server.Id{p.connectionId}); err == nil {
			t.Fatal("canceled admitted")
		}
	})
}

func TestArinCurrentDurableFactsPointPlansWithFalseZeroStats(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "ShadowPlan", "ShadowPlan", "ShadowPlan", "zz")
		now := server.NowUtc()
		p := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, &ConnectionLocationScores{ArinLookupAt: &now, ArinDatabaseBuildEpoch: now.Unix() - 1})
		server.Db(ctx, func(conn server.PgConn) {
			for _, table := range []string{"network_client_connection", "network_client_location"} {
				server.RaisePgResult(conn.Exec(ctx, `CREATE TEMP TABLE `+table+` (LIKE public.`+table+` INCLUDING ALL)`))
				defer func() { server.RaisePgResult(conn.Exec(ctx, `DROP TABLE pg_temp.`+table)) }()
				server.RaisePgResult(conn.Exec(ctx, `ANALYZE pg_temp.`+table))
				var columns, values []string
				attrs, attrErr := conn.Query(ctx, `SELECT quote_ident(attname) FROM pg_attribute WHERE attrelid=$1::regclass AND attnum>0 AND NOT attisdropped AND attgenerated='' ORDER BY attnum`, "pg_temp."+table)
				server.WithPgResult(attrs, attrErr, func() {
					for attrs.Next() {
						var column string
						server.Raise(attrs.Scan(&column))
						columns = append(columns, column)
						if column == "connection_id" {
							values = append(values, "md5('arin-point-'||n)::uuid")
						} else {
							values = append(values, "seed."+column)
						}
					}
				})
				list := strings.Join(columns, ",")
				server.RaisePgResult(conn.Exec(ctx, `INSERT INTO pg_temp.`+table+` (`+list+`) SELECT `+strings.Join(values, ",")+`
					FROM public.`+table+` AS seed CROSS JOIN generate_series(1,100000) AS n WHERE seed.connection_id=$1`, p.connectionId))
				server.RaisePgResult(conn.Exec(ctx, `INSERT INTO pg_temp.`+table+` (`+list+`) SELECT `+list+` FROM public.`+table+` WHERE connection_id=$1`, p.connectionId))
			}
			server.RaisePgResult(conn.Exec(ctx, `SET search_path=pg_temp,public`))
			defer func() {
				server.RaisePgResult(conn.Exec(ctx, `RESET search_path`))
				server.RaisePgResult(conn.Exec(ctx, `RESET plan_cache_mode`))
			}()
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				server.RaisePgResult(conn.Exec(ctx, `SET plan_cache_mode=`+mode))
				name := "arin_facts_" + mode
				server.RaisePgResult(conn.Exec(ctx, `PREPARE `+name+` (uuid[]) AS `+arinShadowCaptureFactsSQL))
				defer func() { server.RaisePgResult(conn.Exec(ctx, `DEALLOCATE `+name)) }()
				var raw []byte
				rows, err := conn.Query(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE `+name+` ('{`+p.connectionId.String()+`}')`)
				server.WithPgResult(rows, err, func() {
					if !rows.Next() {
						t.Fatal("missing plan")
					}
					server.Raise(rows.Scan(&raw))
				})
				var plan []map[string]any
				if json.Unmarshal(raw, &plan) != nil || len(plan) != 1 {
					t.Fatal("malformed plan")
				}
				points := 0
				var walk func(map[string]any)
				walk = func(node map[string]any) {
					if relation, _ := node["Relation Name"].(string); relation == "network_client_connection" || relation == "network_client_location" {
						kind, _ := node["Node Type"].(string)
						condition, _ := node["Index Cond"].(string)
						if !strings.HasPrefix(kind, "Index") || !strings.Contains(condition, "connection_id") || node["Actual Loops"] != float64(1) || node["Actual Rows"] != float64(1) {
							t.Fatal("fact lookup escaped exact primary-key scope")
						}
						points++
					}
					if children, ok := node["Plans"].([]any); ok {
						for _, child := range children {
							walk(child.(map[string]any))
						}
					}
				}
				walk(plan[0]["Plan"].(map[string]any))
				if points != 2 {
					t.Fatal("expected two exact point probes")
				}
				t.Log(fmt.Sprintf("%s: 100001 rows/table; two one-row primary-key probes; execution_ms=%v", mode, plan[0]["Execution Time"]))
			}
		})
	})
}
