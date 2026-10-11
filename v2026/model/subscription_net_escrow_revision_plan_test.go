package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// Inspect actual statement-trigger plans, including their transition relations.
// The historical function scans unrelated unsettled escrow with false-zero
// partial-index statistics. Both plan modes must retain contract-key bounds
// after the appended migration, including on an already-open connection.
func TestNetEscrowRevisionTriggerUsesContractKeyAfterUpgrade(t *testing.T) {
	env := server.DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		// Schema 753 changed this trigger. Keep its historical baseline fixed
		// when later, unrelated migrations are appended.
		server.ApplyDbMigrationsUpTo(ctx, 752)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contractID, unrelatedBalanceID := server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE transfer_escrow SET (autovacuum_enabled=false)`))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow (contract_id,balance_id,balance_byte_count,settled)
				SELECT md5('revision-history-'||n)::uuid,$1,1,true FROM generate_series(1,100000) n`, unrelatedBalanceID))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE transfer_escrow`))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow (contract_id,balance_id,balance_byte_count,settled)
				SELECT md5('revision-live-'||n)::uuid,$1,1,false FROM generate_series(1,2000) n`, unrelatedBalanceID))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow (contract_id,balance_id,balance_byte_count)
				VALUES ($1,$2,1)`, contractID, f.balanceId))
		})
		server.Db(ctx, func(pooled server.PgConn) {
			config := pooled.Conn().Config()
			var notices []string
			config.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) { notices = append(notices, n.Message) }
			conn, err := pgx.ConnectConfig(ctx, config)
			server.Raise(err)
			defer conn.Close(context.WithoutCancel(ctx))
			if _, err := conn.Exec(ctx, `LOAD 'auto_explain'`); err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && (pgErr.Code == "58P01" || pgErr.Code == "42501") {
					t.Skip("actual nested plan test requires auto_explain and permission to load it")
				}
				server.Raise(err)
			}
			server.RaisePgResult(conn.Exec(ctx, `SET auto_explain.log_min_duration=0;
				SET auto_explain.log_nested_statements=on;
				SET auto_explain.log_analyze=on;
				SET auto_explain.log_buffers=on;
				SET auto_explain.log_timing=off;
				SET auto_explain.log_format=json;
				SET auto_explain.log_level=notice;
				SET client_min_messages=notice;`))
			for _, upgraded := range []bool{false, true} {
				if upgraded {
					server.ApplyDbMigrations(ctx)
					var body string
					server.Raise(conn.QueryRow(ctx, `SELECT prosrc FROM pg_proc WHERE oid='transfer_contract_escrow_revision()'::regprocedure`).Scan(&body))
					if body != server.NetEscrowContractsRedisRevisionFunctionBodySql {
						t.Fatal("appended migration did not install current function on existing session")
					}
				}
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					server.RaisePgResult(conn.Exec(ctx, `SET plan_cache_mode=`+mode))
					for _, operation := range []string{"INSERT", "UPDATE", "DELETE"} {
						tx, err := conn.Begin(ctx)
						server.Raise(err)
						insert := func() {
							server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
								(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count)
								VALUES ($1,$2,$3,$4,$5,$2,1)`, contractID, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId))
						}
						if operation != "INSERT" {
							insert()
						}
						var before, neighborBefore int64
						server.Raise(tx.QueryRow(ctx, `SELECT revision FROM transfer_balance_net_escrow_revision WHERE balance_id=$1`, f.balanceId).Scan(&before))
						server.Raise(tx.QueryRow(ctx, `SELECT revision FROM transfer_balance_net_escrow_revision WHERE balance_id=$1`, unrelatedBalanceID).Scan(&neighborBefore))
						notices = nil
						switch operation {
						case "INSERT":
							insert()
						case "UPDATE":
							server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET outcome='canceled',close_time=now() WHERE contract_id=$1`, contractID))
						case "DELETE":
							server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, contractID))
						}
						captured := append([]string(nil), notices...)
						var after, neighborAfter int64
						server.Raise(tx.QueryRow(ctx, `SELECT revision FROM transfer_balance_net_escrow_revision WHERE balance_id=$1`, f.balanceId).Scan(&after))
						server.Raise(tx.QueryRow(ctx, `SELECT revision FROM transfer_balance_net_escrow_revision WHERE balance_id=$1`, unrelatedBalanceID).Scan(&neighborAfter))
						server.Raise(tx.Rollback(ctx))
						if after != before+1 || neighborAfter != neighborBefore {
							t.Fatalf("%s changed revision scope: target %d -> %d, neighbor %d -> %d", operation, before, after, neighborBefore, neighborAfter)
						}
						plans := 0
						for _, notice := range captured {
							at := strings.Index(notice, "{")
							if at < 0 {
								continue
							}
							var plan map[string]any
							server.Raise(json.Unmarshal([]byte(notice[at:]), &plan))
							query, _ := plan["Query Text"].(string)
							if !strings.Contains(query, "advance_net_escrow_revision(ARRAY(") || !strings.Contains(query, "FROM transfer_escrow") {
								continue
							}
							plans++
							rows, probes, buffers, pointScoped := 0, 0, 0, true
							var inspect func(map[string]any)
							inspect = func(node map[string]any) {
								if node["Relation Name"] == "transfer_escrow" {
									visited := node["Actual Rows"].(float64)
									if removed, ok := node["Rows Removed by Filter"].(float64); ok {
										visited += removed
									}
									loops := int(node["Actual Loops"].(float64))
									rows += int(visited * float64(loops))
									probes += loops
									buffers += int(node["Shared Hit Blocks"].(float64)) + int(node["Shared Read Blocks"].(float64))
									condition, _ := node["Index Cond"].(string)
									if !strings.Contains(condition, "contract_id =") {
										pointScoped = false
									}
								}
								if children, ok := node["Plans"].([]any); ok {
									for _, child := range children {
										inspect(child.(map[string]any))
									}
								}
							}
							inspect(plan["Plan"].(map[string]any))
							t.Logf("upgraded=%t mode=%s op=%s point_scoped=%t escrow_rows=%d probes=%d escrow_buffers=%d", upgraded, mode, operation, pointScoped, rows, probes, buffers)
							if upgraded && (!pointScoped || rows != 1 || probes != 1) {
								t.Fatalf("upgraded trigger escaped contract key: %s", notice)
							}
							if !upgraded && (pointScoped || rows < 2001) {
								t.Fatalf("historical plan did not reproduce global scan: %s", notice)
							}
						}
						if plans != 1 {
							t.Fatalf("captured %d contract trigger plans, want one", plans)
						}
					}
				}
			}
		})
	})
}
