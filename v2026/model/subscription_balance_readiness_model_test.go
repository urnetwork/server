package model

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

func TestHasActiveTransferBalancePreservesFundingObservations(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, name := range []string{"empty", "current", "reserved", "over-reserved", "negative mirror", "expired", "future", "inactive", "fragmented", "fallback", "no double count", "overflow"} {
			networkId := server.NewId()
			now := server.NowUtc()
			minimum := ByteCount(1024)
			want := name == "current" || name == "negative mirror" || name == "fragmented" || name == "fallback" || name == "overflow"
			count := 1
			switch name {
			case "empty":
				count = 0
			case "fragmented":
				count = 2
			case "fallback", "no double count":
				count = activeTransferBalanceReadinessCount + 1
			case "overflow":
				count = 2
				minimum = math.MaxInt64
			}
			for index := range count {
				balance := &TransferBalance{NetworkId: networkId,
					StartTime: now.Add(time.Duration(index-count) * time.Minute), EndTime: now.Add(time.Hour),
					StartBalanceByteCount: 1024, BalanceByteCount: 1024}
				reserved := ByteCount(0)
				switch name {
				case "reserved":
					reserved = 1024
				case "over-reserved":
					reserved = 2048
				case "negative mirror":
					reserved = -1024
				case "expired":
					balance.EndTime = now
				case "future":
					balance.StartTime = now.Add(time.Minute)
				case "inactive":
					balance.BalanceByteCount = 0
				case "fragmented":
					balance.BalanceByteCount = 512
				case "fallback":
					if index > 0 {
						reserved = 1024
					}
				case "no double count":
					balance.BalanceByteCount = 32
				case "overflow":
					balance.StartBalanceByteCount, balance.BalanceByteCount = math.MaxInt64, math.MaxInt64
				}
				AddTransferBalance(ctx, balance)
				if reserved != 0 {
					server.Redis(ctx, func(r server.RedisClient) {
						server.Raise(r.Set(ctx, netEscrowKey(balance.BalanceId), reserved, time.Hour).Err())
					})
				}
			}
			if got := HasActiveTransferBalance(ctx, networkId, minimum); got != want {
				t.Fatalf("%s: readiness=%t, want %t", name, got, want)
			}
		}
	})
}

// The optimization must observe each depletion and replenishment, and a corrupt
// required mirror must still fail closed in both the first window and fallback.
func TestHasActiveTransferBalanceFreshReadsAndMirrorErrors(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		now := server.NowUtc()
		balance := &TransferBalance{NetworkId: networkId, StartTime: now.Add(-time.Hour),
			EndTime: now.Add(time.Hour), StartBalanceByteCount: 1024, BalanceByteCount: 1024}
		AddTransferBalance(ctx, balance)
		for _, reserved := range []int64{0, 1024, 512, 0} {
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Set(ctx, netEscrowKey(balance.BalanceId), reserved, time.Hour).Err())
			})
			if got := HasActiveTransferBalance(ctx, networkId, 1024); got != (reserved == 0) {
				t.Fatalf("readiness reused credit across reservation=%d", reserved)
			}
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, netEscrowKey(balance.BalanceId), "corrupt", time.Hour).Err())
		})
		for _, fallback := range []bool{false, true} {
			if fallback {
				for index := range activeTransferBalanceReadinessCount {
					newer := &TransferBalance{NetworkId: networkId, StartTime: now.Add(time.Duration(index-32) * time.Minute),
						EndTime: now.Add(time.Hour), StartBalanceByteCount: 1, BalanceByteCount: 1}
					AddTransferBalance(ctx, newer)
				}
			}
			if err := server.HandleError(func() { HasActiveTransferBalance(ctx, networkId, 1024) }); err == nil {
				t.Fatalf("fallback=%t: corrupt required mirror manufactured readiness", fallback)
			}
		}
	})
}

// A retained snapshot models grant version churn independently of any Main CPU
// attribution. Both prepared custom and generic plans must use bounded ordered
// access with the existing index, and ordinary discovery must never scan grants.
func TestBalanceReadinessAndOrdinaryDiscoveryBoundRetainedGrantWork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		payer, ordinary := server.NewId(), server.NewId()
		now := server.NowUtc()
		server.Db(ctx, func(writer server.PgConn) {
			server.RaisePgResult(writer.Exec(ctx, `INSERT INTO transfer_balance
				(balance_id,network_id,start_time,end_time,start_balance_byte_count,balance_byte_count,
				 net_revenue_nano_cents,subsidy_net_revenue_nano_cents,pro)
				SELECT md5('readiness-grant-'||i)::uuid,
				 CASE WHEN i<=512 THEN $1::uuid WHEN i<=1024 THEN $2::uuid ELSE md5('ordinary-'||(i/32))::uuid END,
				 $3::timestamp-interval '1 hour'-i*interval '1 second', $3::timestamp+interval '1 hour',
				 1048576,1048576,0,0,false FROM generate_series(1,33792) AS i`, payer, ordinary, now))
			server.RaisePgResult(writer.Exec(ctx, `INSERT INTO prober_identity(singleton,network_id) VALUES(true,$1)`, payer))
			server.Db(ctx, func(reader server.PgConn) {
				old, err := reader.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
				server.Raise(err)
				defer old.Rollback(context.WithoutCancel(ctx))
				var count int
				server.Raise(old.QueryRow(ctx, `SELECT count(*) FROM transfer_balance WHERE network_id=$1`, payer).Scan(&count))
				if count != 512 {
					t.Fatal("retained snapshot lacks the intended payer population")
				}
				for range 16 {
					server.RaisePgResult(writer.Exec(ctx, `UPDATE transfer_balance SET
					 end_time=end_time+interval '1 microsecond',balance_byte_count=balance_byte_count-1
					 WHERE network_id=$1 OR network_id=$2`, payer, ordinary))
				}
				server.RaisePgResult(writer.Exec(ctx, `ANALYZE transfer_balance`))
				queries := []struct{ name, sql string }{
					{"readiness_full", activeTransferBalanceSql},
					{"readiness_window", activeTransferBalanceReadinessSql},
					{"discovery_before", strings.Replace(proberGrantSelectionSql, "ELSE 0 END", "ELSE NULL END", 1)},
					{"discovery_after", proberGrantSelectionSql},
				}
				for _, query := range queries {
					server.RaisePgResult(writer.Exec(ctx, `PREPARE `+query.name+` AS `+query.sql))
					defer writer.Exec(context.WithoutCancel(ctx), `DEALLOCATE `+query.name)
				}
				type observation struct{ rows, buffers, grantRows, grantLoops int }
				observe := func(name string, networkId server.Id) observation {
					args := fmt.Sprintf("'%s','%s'", networkId, now.Format("2006-01-02 15:04:05.999999"))
					if name == "readiness_window" {
						args += ",16"
					} else if strings.HasPrefix(name, "discovery_") {
						args += ",16,1048576"
					}
					var raw []byte
					server.Raise(writer.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) EXECUTE `+name+`(`+args+`)`).Scan(&raw))
					var plans []map[string]any
					server.Raise(json.Unmarshal(raw, &plans))
					root := plans[0]["Plan"].(map[string]any)
					seen := observation{rows: int(root["Actual Rows"].(float64)), buffers: int(root["Shared Hit Blocks"].(float64) + root["Shared Read Blocks"].(float64))}
					var visit func(map[string]any)
					visit = func(node map[string]any) {
						if node["Relation Name"] == "transfer_balance" {
							seen.grantRows += int(node["Actual Rows"].(float64))
							seen.grantLoops += int(node["Actual Loops"].(float64))
							if name == "readiness_window" && (node["Index Name"] != "transfer_balance_active_network_id_start_end_time" || node["Scan Direction"] != "Backward") {
								t.Fatalf("readiness window lost existing ordered index: %+v", node)
							}
						}
						if children, ok := node["Plans"].([]any); ok {
							for _, child := range children {
								visit(child.(map[string]any))
							}
						}
					}
					visit(root)
					return seen
				}
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					server.RaisePgResult(writer.Exec(ctx, `SET plan_cache_mode=`+mode))
					defer writer.Exec(context.WithoutCancel(ctx), `RESET plan_cache_mode`)
					full, window := observe("readiness_full", payer), observe("readiness_window", payer)
					before, after := observe("discovery_before", ordinary), observe("discovery_after", ordinary)
					if full.rows != 512 || window.rows != 16 || window.grantRows != 16 || window.buffers >= full.buffers {
						t.Fatalf("%s readiness did not bound grant work: full=%+v window=%+v", mode, full, window)
					}
					if before.rows != 512 || after.rows != 0 || after.grantLoops != 0 || after.buffers >= before.buffers {
						t.Fatalf("%s ordinary discovery still scans grants: before=%+v after=%+v", mode, before, after)
					}
					t.Logf("%s readiness full=%+v window=%+v; ordinary discovery before=%+v after=%+v", mode, full, window, before, after)
				}
			}, server.OptReadOnly(), server.OptNoRetry())
		}, server.OptReadWrite(), server.OptNoRetry())
	})
}
