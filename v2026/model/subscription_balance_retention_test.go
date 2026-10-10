package model

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
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
		beforeDebit := readPayoutDebitTestState(t, ctx, finished.balanceId)
		// Current Redis-funded settlement commits its outcome and journal first.
		// The debit worker owns both escrow metadata and the original reservation
		// release. A 300-byte debit must still retain the entire 600-byte token.
		if beforeDebit.initial != 1000 || beforeDebit.credit != 1000 || beforeDebit.pending != 1 || beforeDebit.pendingBytes != 300 || beforeDebit.applied != 0 ||
			beforeDebit.escrows != 1 || beforeDebit.settledEscrows != 0 || beforeDebit.settled != 0 || beforeDebit.anchors != 0 || beforeDebit.invalid != 1 ||
			beforeDebit.legacy != 0 || beforeDebit.reserved != 600 || beforeDebit.inWindow {
			t.Fatal("retention fixture lacks the exact pre-worker debit and full original reservation")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var pendingMetadata bool
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_escrow
				WHERE contract_id=$1 AND balance_id=$2 AND redis_reserved AND NOT settled
				AND balance_byte_count=600 AND payout_byte_count IS NULL AND settle_time IS NULL
				AND EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1
					AND balance_id=$2 AND debit_byte_count=300 AND NOT applied))`, escrow.ContractId, finished.balanceId).Scan(&pendingMetadata))
			if !pendingMetadata {
				t.Fatal("pre-worker escrow lacks its exact pending metadata shape")
			}
		})
		sweeps := readPayoutDebitTestSweeps(t, ctx, finished.balanceId)
		if len(sweeps) != 1 || sweeps[0].contractId != escrow.ContractId || sweeps[0].bytes != 300 {
			t.Fatal("terminal contract lacks its exact committed provider allocation")
		}
		accounts := map[server.Id]contractPayoutTestAmount{
			finished.sourceNetworkId: contractPayoutTestAccountAmount(t, ctx, finished.sourceNetworkId),
		}
		for _, sweep := range sweeps {
			accounts[sweep.networkId] = contractPayoutTestAccountAmount(t, ctx, sweep.networkId)
		}
		checkUntouched := func() {
			for balanceId, before := range preserved {
				if after := readPayoutDebitTestState(t, ctx, balanceId); after != before {
					t.Fatal("targeted retention/debit changed an unrelated retained obligation")
				}
			}
			if after := readPayoutDebitTestSweeps(t, ctx, finished.balanceId); !slices.Equal(sweeps, after) {
				t.Fatal("retention/debit changed the committed provider allocation")
			}
			for networkId, amount := range accounts {
				if after := contractPayoutTestAccountAmount(t, ctx, networkId); after != amount {
					t.Fatal("retention/debit changed a payer or provider account")
				}
			}
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
		if after := readPayoutDebitTestState(t, ctx, finished.balanceId); after != beforeDebit {
			t.Fatal("retention changed the pending debit before its worker ran")
		}
		checkUntouched()
		if available := GetActiveTransferBalanceByteCount(ctx, finished.sourceNetworkId); available != 0 {
			t.Fatalf("expired pending credit became spendable: got=%d", available)
		}
		// The public one-balance page seeks immediately before this exact UUID,
		// so another retained grant cannot accidentally be drained by the test.
		previous := finished.balanceId
		for index := len(previous) - 1; ; index-- {
			if index < 0 {
				t.Fatal("synthetic balance has no preceding UUID")
			}
			if previous[index] != 0 {
				previous[index]--
				break
			}
			previous[index] = 255
		}
		result, err := FlushTransferDebits(ctx, transferDebitShard(finished.balanceId), &previous, 1)
		if err != nil || result.Failed != 0 || result.Busy != 0 || result.Balances != 1 || result.Applied != 1 || result.Released != 1 || !result.More ||
			result.LastBalanceId == nil || *result.LastBalanceId != finished.balanceId {
			t.Fatalf("public debit page did not finish the exact retained grant: error_type=%T balances=%d applied=%d released=%d busy=%d failed=%d", err, result.Balances, result.Applied, result.Released, result.Busy, result.Failed)
		}
		afterDebit := readPayoutDebitTestState(t, ctx, finished.balanceId)
		if afterDebit.initial != 1000 || afterDebit.credit != 700 || afterDebit.pending != 0 || afterDebit.pendingBytes != 0 || afterDebit.applied != 0 ||
			afterDebit.escrows != 1 || afterDebit.settledEscrows != 1 || afterDebit.settled != 300 || afterDebit.invalid != 0 || afterDebit.anchors != 0 ||
			afterDebit.legacy != 0 || afterDebit.reserved != 0 || afterDebit.inWindow {
			t.Fatal("public debit did not conserve 300 consumed bytes, 700 remaining bytes, settled metadata and full token release")
		}
		if available := GetActiveTransferBalanceByteCount(ctx, finished.sourceNetworkId); available != 0 {
			t.Fatalf("public debit made expired remaining credit spendable: got=%d", available)
		}
		checkUntouched()
		applied, released, busy, err := flushTransferDebitBalance(ctx, finished.balanceId)
		if err != nil || applied != 0 || released != 0 || busy {
			t.Fatalf("empty exact debit replay changed the journal: error_type=%T applied=%d released=%d busy=%t", err, applied, released, busy)
		}
		if replayed := readPayoutDebitTestState(t, ctx, finished.balanceId); replayed != afterDebit {
			t.Fatal("empty debit replay changed settled credit or escrow metadata")
		}
		checkUntouched()
		removeCompletedTransferBalanceBatches(ctx, server.NowUtc().Add(-7*24*time.Hour))
		check(true)
		checkUntouched()
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
		// Each actor owns a current server transaction on its own goroutine.
		// The main actor holds no PostgreSQL connection while running retention
		// or a separate creator. Both normal and failing paths join the owner.
		startHolder := func(run func(context.Context, server.PgTx, func())) (finish func(), abort func()) {
			heldCtx, stop := context.WithCancel(ctx)
			ready, proceed, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var recovered any
			go func() {
				defer close(done)
				recovered = server.HandleError(func() {
					server.Tx(heldCtx, func(tx server.PgTx) {
						run(heldCtx, tx, func() {
							close(ready)
							select {
							case <-proceed:
							case <-heldCtx.Done():
								server.Raise(heldCtx.Err())
							}
						})
					}, server.TxReadCommitted, server.OptNoRetry())
				})
			}()
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(proceed) }) }
			abort = func() {
				stop()
				release()
				<-done
			}
			select {
			case <-ready:
			case <-done:
				abort()
				t.Fatalf("independent holder failed before its barrier: panic_type=%T", recovered)
			case <-ctx.Done():
				abort()
				t.Fatal("independent holder did not reach its barrier before the fixture deadline")
			}
			finish = func() {
				release()
				<-done
				if recovered != nil {
					t.Fatalf("independent holder failed after its barrier: panic_type=%T", recovered)
				}
			}
			return
		}
		f := newNetEscrowOrderingTestFixture(t, ctx)
		var admitted *TransferEscrow
		var posts []func() any
		finishPositive, abortPositive := startHolder(func(heldCtx context.Context, held server.PgTx, wait func()) {
			var err error
			admitted, posts, err = createTransferEscrowInTx(heldCtx, held, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 600, nil)
			server.Raise(err)
			wait()
		})
		defer abortPositive()
		// A caller-supplied future cutoff deliberately overlaps live admission.
		// The row is locked by real admission, so retention must skip it promptly.
		cutoff := server.NowUtc().Add(48 * time.Hour)
		fast, stop := context.WithTimeout(ctx, 3*time.Second)
		defer stop()
		removeCompletedTransferBalanceBatches(fast, cutoff)
		stop()
		finishPositive()
		server.RunPosts(ctx, posts...)
		removeCompletedTransferBalanceBatches(ctx, cutoff)
		server.Db(ctx, func(c server.PgConn) {
			var exists, reserved bool
			server.Raise(c.QueryRow(ctx, `SELECT
				EXISTS(SELECT 1 FROM transfer_balance WHERE balance_id=$1 AND balance_byte_count=1000),
				EXISTS(SELECT 1 FROM transfer_escrow WHERE balance_id=$1 AND contract_id=$2 AND balance_byte_count=600 AND NOT settled)`, f.balanceId, admitted.ContractId).Scan(&exists, &reserved))
			if !exists || !reserved {
				t.Fatal("retention erased a concurrently committed balance or its exact positive reservation")
			}
		})
		if reserved := Testing_NetEscrowByteCount(ctx, f.balanceId); reserved != 600 {
			t.Fatalf("positive admission post lost its committed reservation: got=%d", reserved)
		}

		zero := newNetEscrowOrderingTestFixture(t, ctx)
		var deletedZero bool
		finishZero, abortZero := startHolder(func(heldCtx context.Context, locked server.PgTx, wait func()) {
			owned, err := tryTransferBalanceOwnershipInTx(heldCtx, locked, []server.Id{zero.balanceId})
			server.Raise(err)
			if !owned {
				panic("independent zero-anchor holder did not acquire balance ownership")
			}
			server.RaisePgResult(locked.Exec(heldCtx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, zero.balanceId))
			wait()
			rows, err := locked.Query(heldCtx, completedTransferBalanceDeleteSql, []server.Id{zero.balanceId})
			server.WithPgResult(rows, err, func() { deletedZero = rows.Next() })
		})
		defer abortZero()
		// Zero-byte admission does not take a financial lock. Commit an actual
		// anchor after the locking snapshot; the separate delete must see it.
		anchor, err := CreateTransferEscrow(ctx, zero.sourceNetworkId, zero.sourceId, zero.destinationNetworkId, zero.destinationId, 0)
		server.Raise(err)
		if len(anchor.Balances) != 1 {
			t.Fatal("concurrent zero-byte contract has no test anchor")
		}
		finishZero()
		if deletedZero {
			t.Fatal("post-lock deletion snapshot missed a committed zero-byte anchor")
		}

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
