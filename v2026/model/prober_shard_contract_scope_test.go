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

// A private grant can be exhausted before a zero-byte contract is admitted.
// Such a contract has no escrow anchor; even a disputed one must prevent the
// account from being reaped. Checking only its balance's escrows is insufficient.
func TestProberShardUnanchoredDisputedZeroBlocksCleanup(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		peer := newEscrowSelectionTestClients(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET balance_byte_count=0 WHERE balance_id=$1`, owner.BalanceId))
		})
		zero, err := CreateTransferEscrow(ctx, owner.NetworkId, owner.ClientId,
			peer.providerNetworkId, peer.providerId, 0)
		if err != nil || zero == nil || len(zero.Balances) != 0 {
			t.Fatalf("fixture did not create an unanchored zero-byte contract: %+v, %v", zero, err)
		}
		server.Raise(DrainProberShard(ctx, owner.Key))
		if deleted, err := ReapProberShard(ctx, owner.Key); err != nil || deleted {
			t.Fatalf("unanchored zero-byte contract was discarded: deleted=%t err=%v", deleted, err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET dispute=true WHERE contract_id=$1`, zero.ContractId))
		})
		if deleted, err := ReapProberShard(ctx, owner.Key); err != nil || deleted {
			t.Fatalf("disputed zero-byte contract was discarded: deleted=%t err=%v", deleted, err)
		}
		// Two actual matching reports let the normal settlement path finish.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
				(contract_id,party,used_transfer_byte_count,checkpoint)
				VALUES($1,'source',0,false),($1,'destination',0,false)`, zero.ContractId))
		})
		if deleted, err := ReapProberShard(ctx, owner.Key); err != nil || !deleted {
			t.Fatalf("reported disputed zero-byte contract did not finish through settlement: deleted=%t err=%v", deleted, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var retained bool
			server.Raise(conn.QueryRow(ctx, `SELECT outcome='settled' AND provider_usage IS NOT NULL
				FROM transfer_contract WHERE contract_id=$1`, zero.ContractId).Scan(&retained))
			if !retained {
				t.Fatal("cleanup lost the immutable settlement receipt")
			}
		})
	})
}

// Use the complete migrated schema, including the dropped full payer index.
// A retained writer snapshot makes global outcome/open partial indexes report
// zero rows although 2,000 unrelated open contracts become visible afterwards.
// Both plan modes must still perform two indexed ranges for this payer and
// primary-key report lookups, preserving disputed and zero-byte obligations.
func TestProberShardContractPlansStayInsidePayerWithFalseZeroStats(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		peer := newEscrowSelectionTestClients(t, ctx)
		seedFalseZeroOpenContractStats(t, ctx, server.NewId(), server.NewId(), server.NewId(), server.NewId())
		server.Raise(DrainProberShard(ctx, owner.Key))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,
				 payer_network_id,transfer_byte_count,dispute,outcome,close_time)
				SELECT md5('shard-scope-owned-'||n)::uuid,$1,$2,$3,$4,$1,0,n IN (5,6),
				 CASE WHEN n>6 THEN 'canceled' ELSE NULL END,
				 CASE WHEN n>6 THEN now() ELSE NULL END
				FROM generate_series(1,12) AS n`, owner.NetworkId, owner.ClientId, peer.providerNetworkId, peer.providerId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
				(contract_id,party,used_transfer_byte_count,checkpoint)
				SELECT md5('shard-scope-owned-'||n)::uuid,party,0,n=6 AND party='destination'
				FROM generate_series(1,12) AS n CROSS JOIN (VALUES('source'),('destination')) AS parties(party)`))
		})
		server.Db(ctx, func(conn server.PgConn) {
			var removedIndex *string
			server.Raise(conn.QueryRow(ctx, `SELECT to_regclass('transfer_contract_payer_network_id')::text`).Scan(&removedIndex))
			if removedIndex != nil {
				t.Fatal("fixture restored the removed full payer index")
			}
			assertOpenPlanStatsAreFalseZero(t, ctx, conn)
			const beforeExists = `SELECT EXISTS(SELECT 1 FROM transfer_contract WHERE payer_network_id=$1 AND outcome IS NULL)`
			for _, query := range []struct{ name, sql string }{
				{"shard_scope_before", beforeExists},
				{"shard_scope_exists", proberShardHasUnresolvedContractsSql},
				{"shard_scope_reported", proberShardReportedContractsSql},
			} {
				server.RaisePgResult(conn.Exec(ctx, `PREPARE `+query.name+` AS `+query.sql))
				defer conn.Exec(context.WithoutCancel(ctx), `DEALLOCATE `+query.name)
			}
			missingPayer := server.NewId()
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				server.RaisePgResult(conn.Exec(ctx, `SET plan_cache_mode=`+mode))
				for _, query := range []struct{ name, arguments string }{
					{"shard_scope_before", fmt.Sprintf("'%s'::uuid", missingPayer)},
					{"shard_scope_exists", fmt.Sprintf("'%s'::uuid", missingPayer)},
					{"shard_scope_reported", fmt.Sprintf("'%s'::uuid,'%s'::uuid,'source','destination'", owner.Key.TaskId, owner.Key.Epoch)},
				} {
					var raw []byte
					server.Raise(conn.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) EXECUTE `+query.name+`(`+query.arguments+`)`).Scan(&raw))
					var plans []map[string]any
					server.Raise(json.Unmarshal(raw, &plans))
					root := plans[0]["Plan"].(map[string]any)
					contractRows, payerRanges := 0, 0
					var inspect func(map[string]any)
					inspect = func(node map[string]any) {
						if node["Relation Name"] == "transfer_contract" {
							rows := node["Actual Rows"].(float64)
							if removed, ok := node["Rows Removed by Filter"].(float64); ok {
								rows += removed
							}
							contractRows += int(rows * node["Actual Loops"].(float64))
							if query.name != "shard_scope_before" && node["Index Name"] != "transfer_contract_open_payer_network_id_transfer_byte_count" {
								t.Fatalf("%s %s escaped payer index: %s", mode, query.name, raw)
							}
						}
						if node["Index Name"] == "transfer_contract_open_payer_network_id_transfer_byte_count" {
							condition, _ := node["Index Cond"].(string)
							if !strings.Contains(condition, "open =") || !strings.Contains(condition, "payer_network_id =") {
								t.Fatalf("payer range lost a leading equality: %s", condition)
							}
							payerRanges += int(node["Actual Loops"].(float64))
						}
						if query.name == "shard_scope_reported" && node["Relation Name"] == "contract_close" {
							condition, _ := node["Index Cond"].(string)
							partyEquality := strings.Contains(condition, "party =") || strings.Contains(condition, "(party)::text =")
							if node["Index Name"] != "contract_close_pkey" || !strings.Contains(condition, "contract_id =") || !partyEquality {
								t.Fatalf("report read lost exact contract/party lookup: %s", raw)
							}
						}
						if children, ok := node["Plans"].([]any); ok {
							for _, child := range children {
								inspect(child.(map[string]any))
							}
						}
					}
					inspect(root)
					if query.name == "shard_scope_before" {
						if contractRows < 2000 {
							t.Fatalf("old absent-payer query failed to reproduce global work: %s", raw)
						}
					} else if payerRanges != 2 ||
						(query.name == "shard_scope_exists" && contractRows != 0) ||
						(query.name == "shard_scope_reported" && (contractRows != 12 || root["Actual Rows"].(float64) != 5)) {
						t.Fatalf("%s %s changed payer/report bounds: ranges=%d contract_rows=%d plan=%s", mode, query.name, payerRanges, contractRows, raw)
					}
					t.Logf("%s %s contract_rows=%d payer_ranges=%d buffers=%v execution_ms=%v", mode, query.name,
						contractRows, payerRanges, root["Shared Hit Blocks"], plans[0]["Execution Time"])
				}
			}
		})
	})
}
