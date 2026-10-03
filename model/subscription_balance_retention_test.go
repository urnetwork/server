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

func TestCompletedTransferBalanceRetentionPreservesDebtAndLegacy(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		retained := []server.Id{}
		for _, state := range []string{"open", "disputed", "zero", "terminal_unsettled"} {
			f := newNetEscrowOrderingTestFixture(t, ctx)
			amount := ByteCount(600)
			if state == "zero" {
				amount = 0
			}
			escrow, err := CreateTransferEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, amount)
			server.Raise(err)
			if state == "terminal_unsettled" {
				for _, client := range []server.Id{f.sourceId, f.destinationId} {
					server.Raise(CloseContract(ctx, escrow.ContractId, client, amount, false))
				}
			}
			server.Tx(ctx, func(tx server.PgTx) {
				if state == "disputed" {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET dispute=true WHERE contract_id=$1`, escrow.ContractId))
				}
				if state == "terminal_unsettled" {
					// Durable consumption and its terminal receipt committed, but
					// the metadata post is missing. Retain this repair obligation.
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET settled=false WHERE contract_id=$1`, escrow.ContractId))
				}
			})
			retained = append(retained, f.balanceId)
		}
		legacy := newNetEscrowOrderingTestFixture(t, ctx)
		setDynamicProberIdentityForTest(t, ctx, escrowSelectionTestClients{payerNetworkId: legacy.sourceNetworkId, payerId: legacy.sourceId})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET balance_byte_count=0 WHERE balance_id=$1`, legacy.balanceId))
		})
		unanchored, err := CreateTransferEscrow(ctx, legacy.sourceNetworkId, legacy.sourceId, legacy.destinationNetworkId, legacy.destinationId, 0)
		server.Raise(err)
		if len(unanchored.Balances) != 0 {
			t.Fatal("legacy fixture did not create unanchored zero-byte debt")
		}
		retained = append(retained, legacy.balanceId)
		private := shardTestOwner(t, ctx, shardTestKey(0))
		retained = append(retained, private.BalanceId)
		finished := newNetEscrowOrderingTestFixture(t, ctx)
		escrow, err := CreateTransferEscrow(ctx, finished.sourceNetworkId, finished.sourceId, finished.destinationNetworkId, finished.destinationId, 600)
		server.Raise(err)
		for _, client := range []server.Id{finished.sourceId, finished.destinationId} {
			server.Raise(CloseContract(ctx, escrow.ContractId, client, 300, false))
		}
		all := append(append([]server.Id{}, retained...), finished.balanceId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET end_time=now()-interval '8 days' WHERE balance_id=ANY($1)`, all))
		})
		preserved := map[server.Id]payoutDebitTestState{}
		for _, balanceId := range retained {
			preserved[balanceId] = readPayoutDebitTestState(t, ctx, balanceId)
		}
		removeCompletedTransferBalanceBatches(ctx, server.NowUtc().Add(-7*24*time.Hour))
		check := func(wantGone bool) {
			server.Db(ctx, func(conn server.PgConn) {
				var kept int
				var gone, receipt, unanchoredKept, tombstone bool
				server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM transfer_balance WHERE balance_id=ANY($1)),
				NOT EXISTS(SELECT 1 FROM transfer_balance WHERE balance_id=$2),
				EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$3 AND outcome='settled' AND provider_usage IS NOT NULL),
				EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$4 AND outcome IS NULL),
				EXISTS(SELECT 1 FROM transfer_balance_net_escrow_revision WHERE balance_id=$2 AND revision>0)`,
					retained, finished.balanceId, escrow.ContractId, unanchored.ContractId).Scan(&kept, &gone, &receipt, &unanchoredKept, &tombstone))
				if kept != len(retained) || gone != wantGone || !receipt || !unanchoredKept || tombstone != wantGone {
					t.Fatalf("retention lost debt/receipt or failed safe cleanup: kept=%d gone=%t receipt=%t zero=%t tombstone=%t", kept, gone, receipt, unanchoredKept, tombstone)
				}
			})
		}
		// A terminal contract still owes its unapplied debit. The first expiry
		// pass must keep its original balance and must not create a tombstone.
		check(false)
		assertPayoutDebitTestConsumptionAndDrain(t, ctx, finished.balanceId, 1000, 300)
		for balanceId, before := range preserved {
			if after := readPayoutDebitTestState(t, ctx, balanceId); after != before {
				t.Fatalf("targeted public debit changed unrelated retained obligation %s: before=%+v after=%+v", balanceId, before, after)
			}
		}
		removeCompletedTransferBalanceBatches(ctx, server.NowUtc().Add(-7*24*time.Hour))
		check(true)
	})
}

func TestCompletedTransferBalanceRetentionAdvancesPastRetainedPage(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		network := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance
				(balance_id,network_id,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents,pro)
				SELECT md5('retained-grant-'||n)::uuid,$1,now()-interval '9 days',1000,1000,0,false FROM generate_series(1,300) n`, network))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
				SELECT md5('retained-contract-'||n)::uuid,md5('retained-grant-'||n)::uuid,0 FROM generate_series(1,300) n`))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance
				(balance_id,network_id,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents,pro)
				SELECT md5('finished-grant-'||n)::uuid,$1,now()-interval '8 days',1000,1000,0,false FROM generate_series(1,20) n`, network))
		})
		removeCompletedTransferBalanceBatches(ctx, server.NowUtc().Add(-7*24*time.Hour))
		server.Db(ctx, func(conn server.PgConn) {
			var balances, debt int
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM transfer_balance WHERE network_id=$1),
				(SELECT count(*) FROM transfer_escrow WHERE NOT settled)`, network).Scan(&balances, &debt))
			if balances != 300 || debt != 300 {
				t.Fatalf("blocked page starved cleanup or lost orphan debt: balances=%d debt=%d", balances, debt)
			}
		})
	})
}

func TestCompletedTransferBalanceRetentionConcurrentReservations(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer held.Rollback(context.Background())
		_, _, err = createTransferEscrowInTx(ctx, held, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 600, nil)
		server.Raise(err)
		// A caller-supplied future cutoff deliberately overlaps live admission.
		// The row is locked by real admission, so retention must skip it promptly.
		cutoff := server.NowUtc().Add(48 * time.Hour)
		fast, stop := context.WithTimeout(ctx, 3*time.Second)
		removeCompletedTransferBalanceBatches(fast, cutoff)
		stop()
		server.Raise(held.Commit(ctx))
		removeCompletedTransferBalanceBatches(ctx, cutoff)
		server.Db(ctx, func(c server.PgConn) {
			var exists bool
			server.Raise(c.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_balance WHERE balance_id=$1)`, f.balanceId).Scan(&exists))
			if !exists {
				t.Fatal("retention erased a concurrently committed positive reservation")
			}
		})

		zero := newNetEscrowOrderingTestFixture(t, ctx)
		locked, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer locked.Rollback(context.Background())
		server.RaisePgResult(locked.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, zero.balanceId))
		// Zero-byte admission does not take a financial lock. Commit an actual
		// anchor after the locking snapshot; the separate delete must see it.
		anchor, err := CreateTransferEscrow(ctx, zero.sourceNetworkId, zero.sourceId, zero.destinationNetworkId, zero.destinationId, 0)
		server.Raise(err)
		if len(anchor.Balances) != 1 {
			t.Fatal("concurrent zero-byte contract has no test anchor")
		}
		rows, err := locked.Query(ctx, completedTransferBalanceDeleteSql, []server.Id{zero.balanceId})
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				t.Fatal("post-lock deletion snapshot missed a committed zero-byte anchor")
			}
		})
		server.Raise(locked.Commit(ctx))

		// Discovery is unlocked. A refreshed end time must be checked again
		// under the batch lock instead of trusting the old expiry observation.
		extended := newNetEscrowOrderingTestFixture(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET end_time=$2 WHERE balance_id=$1`, extended.balanceId, cutoff.Add(time.Hour)))
		})
		removeCompletedTransferBalanceBatch(ctx, []server.Id{extended.balanceId}, cutoff)
		server.Db(ctx, func(c server.PgConn) {
			var exists bool
			server.Raise(c.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_balance WHERE balance_id=$1)`, extended.balanceId).Scan(&exists))
			if !exists {
				t.Fatal("retention trusted discovery after the balance was extended")
			}
		})
	})
}

func TestCompletedTransferBalanceRetentionPlansStayIndexed(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		network, target := server.NewId(), server.NewId()
		cutoff := server.NowUtc().Add(-7 * 24 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE transfer_escrow SET(autovacuum_enabled=false)`))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance
				(balance_id,network_id,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents,pro)
				SELECT md5('future-grant-'||n)::uuid,$1,now()+interval '30 days',1000,1000,0,false FROM generate_series(1,100000) n`, network))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance
				(balance_id,network_id,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents,pro)
				VALUES($1,$2,$3::timestamp-interval '1 day',1000,1000,0,false)`, target, network, cutoff))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count,settled)
				SELECT md5('historic-contract-'||n)::uuid,md5('historic-grant-'||n)::uuid,1,true FROM generate_series(1,100000) n`))
		})
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_balance`))
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_escrow`))
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
				SELECT md5('live-contract-'||n)::uuid,md5('live-grant-'||n)::uuid,1 FROM generate_series(1,2000) n`))
			var estimated float64
			server.Raise(conn.QueryRow(ctx, `SELECT reltuples FROM pg_class WHERE oid='transfer_escrow_unsettled_balance_contract'::regclass`).Scan(&estimated))
			if estimated != 0 {
				t.Fatalf("fixture lacks false-zero unsettled index estimate: %v", estimated)
			}
			for _, q := range []struct{ name, sql string }{
				{"retention_candidates", completedTransferBalanceCandidatesSql},
				{"retention_lock", completedTransferBalanceLockSql},
				{"retention_delete", completedTransferBalanceDeleteSql},
			} {
				server.RaisePgResult(conn.Exec(ctx, `PREPARE `+q.name+` AS `+q.sql))
				defer conn.Exec(context.WithoutCancel(ctx), `DEALLOCATE `+q.name)
			}
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				server.RaisePgResult(conn.Exec(ctx, `SET plan_cache_mode=`+mode))
				for _, q := range []struct{ name, args string }{
					{"retention_candidates", fmt.Sprintf("'%s'::timestamp", cutoff.Format(time.RFC3339Nano))},
					{"retention_lock", fmt.Sprintf("ARRAY['%s'::uuid],'%s'::timestamp", target, cutoff.Format(time.RFC3339Nano))},
					{"retention_delete", fmt.Sprintf("ARRAY['%s'::uuid]", target)},
				} {
					server.RaisePgResult(conn.Exec(ctx, `BEGIN`))
					var raw []byte
					server.Raise(conn.QueryRow(ctx, `EXPLAIN(ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) EXECUTE `+q.name+`(`+q.args+`)`).Scan(&raw))
					server.RaisePgResult(conn.Exec(ctx, `ROLLBACK`))
					var plan []map[string]any
					server.Raise(json.Unmarshal(raw, &plan))
					root := plan[0]["Plan"].(map[string]any)
					balanceRows, escrowRows, escrowProbes := 0, 0, 0
					var inspect func(map[string]any)
					inspect = func(node map[string]any) {
						if relation, _ := node["Relation Name"].(string); relation == "transfer_balance" || relation == "transfer_escrow" {
							rows := node["Actual Rows"].(float64)
							if removed, ok := node["Rows Removed by Filter"].(float64); ok {
								rows += removed
							}
							count := int(rows * node["Actual Loops"].(float64))
							if relation == "transfer_balance" && node["Node Type"] != "ModifyTable" {
								balanceRows += count
								want := "transfer_balance_end_time"
								if q.name != "retention_candidates" {
									want = "transfer_balance_pkey"
								}
								if node["Index Name"] != want {
									t.Fatalf("%s %s balance scan escaped index: %s", mode, q.name, raw)
								}
							}
							if relation == "transfer_escrow" {
								escrowRows += count
								escrowProbes += int(node["Actual Loops"].(float64))
								condition, _ := node["Index Cond"].(string)
								if node["Index Name"] != "transfer_escrow_unsettled_balance_contract" || !strings.Contains(condition, "balance_id =") {
									t.Fatalf("%s escrow scan escaped exact balance: %s", mode, raw)
								}
							}
						}
						if children, ok := node["Plans"].([]any); ok {
							for _, child := range children {
								inspect(child.(map[string]any))
							}
						}
					}
					inspect(root)
					if balanceRows != 1 || escrowRows != 0 || (q.name == "retention_delete" && escrowProbes != 1) {
						t.Fatalf("unexpected retention work: balances=%d escrow=%d probes=%d plan=%s", balanceRows, escrowRows, escrowProbes, raw)
					}
					t.Logf("%s %s balance_rows=%d escrow_rows=%d escrow_probes=%d buffers=%v execution_ms=%v", mode, q.name, balanceRows, escrowRows, escrowProbes, root["Shared Hit Blocks"], plan[0]["Execution Time"])
				}
			}
		})
	})
}
