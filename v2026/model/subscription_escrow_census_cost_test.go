package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// A held positive reservation is an actual PostgreSQL lock barrier. NOWAIT
// makes accidental zero-byte grant locking fail deterministically.
func TestZeroByteEscrowDoesNotWaitForGrantOrCensus(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, internalProber := range []bool{false, true} {
			f := newNetEscrowOrderingTestFixture(t, ctx)
			if internalProber {
				setDynamicProberIdentityForTest(t, ctx, escrowSelectionTestClients{
					payerNetworkId: f.sourceNetworkId, payerId: f.sourceId,
				})
			}
			conn := acquireContractLifecycleTestConnection(t, ctx)
			defer conn.Release()
			held, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			server.Raise(err)
			defer held.Rollback(context.Background())
			_, _, err = createTransferEscrowInTx(ctx, held, f.sourceNetworkId, f.sourceId,
				f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 600, nil)
			server.Raise(err)
			server.Tx(ctx, func(tx server.PgTx) {
				counted := &grantHintTestTx{PgTx: noWaitGrantTestTx{PgTx: tx}}
				anchor, posts, err := createTransferEscrowInTx(ctx, counted,
					f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId,
					f.sourceNetworkId, 0, nil)
				if err != nil || anchor == nil || len(anchor.Balances) != 1 ||
					anchor.Balances[0].BalanceId != f.balanceId || anchor.Balances[0].BalanceByteCount != 0 || len(posts) != 0 {
					t.Fatalf("zero-byte anchor changed: %+v, error=%v posts=%d", anchor, err, len(posts))
				}
				if counted.reservationReads != 0 {
					t.Fatalf("zero-byte anchor performed %d exact reservation censuses", counted.reservationReads)
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			server.Raise(held.Commit(ctx))
			if got := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId})[f.balanceId].reserved; got != 600 {
				t.Fatalf("zero-byte overlap changed reserved credit to %d", got)
			}
		}
	})
}

// Zero anchors use one consistent committed grant snapshot: pending expiry,
// deletion or paid changes do not block a zero-credit contract. Once committed,
// the next anchor sees the new earliest eligible grant and its current priority.
func TestZeroByteEscrowConcurrentGrantTransitions(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, transition := range []struct {
			name, sql string
			retained  bool
		}{
			{"expiry", `UPDATE transfer_balance SET end_time=now()-interval '1 minute' WHERE balance_id=$1`, false},
			{"delete", `DELETE FROM transfer_balance WHERE balance_id=$1`, false},
			{"paid", `UPDATE transfer_balance SET net_revenue_nano_cents=0 WHERE balance_id=$1`, true},
		} {
			clients := newEscrowSelectionTestClients(t, ctx)
			now := server.NowUtc()
			first := &TransferBalance{NetworkId: clients.payerNetworkId, StartTime: now.Add(-time.Minute),
				EndTime: now.Add(time.Hour), StartBalanceByteCount: 1000, BalanceByteCount: 1000, NetRevenue: 1}
			second := &TransferBalance{NetworkId: clients.payerNetworkId, StartTime: now.Add(-time.Hour),
				EndTime: now.Add(2 * time.Hour), StartBalanceByteCount: 1000, BalanceByteCount: 1000}
			AddTransferBalance(ctx, first)
			AddTransferBalance(ctx, second)
			conn := acquireContractLifecycleTestConnection(t, ctx)
			defer conn.Release()
			held, err := conn.Begin(ctx)
			server.Raise(err)
			defer held.Rollback(context.Background())
			server.RaisePgResult(held.Exec(ctx, transition.sql, first.BalanceId))
			check := func(wantId server.Id, wantPriority Priority) {
				server.Tx(ctx, func(tx server.PgTx) {
					anchor, posts, err := createTransferEscrowInTx(ctx, noWaitGrantTestTx{PgTx: tx},
						clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId,
						clients.payerNetworkId, 0, nil)
					if err != nil || anchor == nil || len(anchor.Balances) != 1 || anchor.Balances[0].BalanceId != wantId ||
						anchor.Balances[0].BalanceByteCount != 0 || anchor.Priority != wantPriority || len(posts) != 0 {
						t.Fatalf("%s changed zero-byte snapshot/priority: %+v, %v", transition.name, anchor, err)
					}
				}, server.TxReadCommitted, server.OptNoRetry())
			}
			check(first.BalanceId, PaidPriority)
			server.Raise(held.Commit(ctx))
			wantId := second.BalanceId
			if transition.retained {
				wantId = first.BalanceId
			}
			check(wantId, UnpaidPriority)
		}
	})
}

// Removing zero-byte grant locks must not remove the separate endpoint fence.
// Observe a real wait on destination deactivation, then reject after its commit.
func TestZeroByteEscrowRetainsClientLifecycleFence(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `UPDATE network_client SET active=false,deactivate_time=now() WHERE client_id=$1`, f.destinationId))
		pid := contractLifecycleTestBackendPid(t, ctx, held)
		finished := make(chan contractLifecycleTestResult, 1)
		go func() {
			result := contractLifecycleTestResult{}
			defer func() {
				if value := recover(); value != nil {
					result.err = fmt.Errorf("zero-byte admission panic: %v", value)
				}
				finished <- result
			}()
			result.escrow, result.err = CreateTransferEscrow(ctx, f.sourceNetworkId, f.sourceId,
				f.destinationNetworkId, f.destinationId, 0)
		}()
		requireContractLifecycleBlockedBy(t, ctx, held, pid)
		server.Raise(held.Commit(ctx))
		select {
		case result := <-finished:
			if result.escrow != nil || !errors.Is(result.err, ErrContractDestinationInactive) {
				t.Fatalf("zero-byte request crossed endpoint fence: %+v", result)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	})
}

// An exclusive table lock makes any duplicate escrow census impossible. The
// unchanged-revision creation post must complete while that barrier is held.
func TestNetEscrowCreatePostReusesCensusWithoutEscrowTable(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		_, posts := createNetEscrowOrderingTestContract(ctx, f, 600)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `LOCK TABLE transfer_escrow IN ACCESS EXCLUSIVE MODE`))
		finished := make(chan any, 1)
		go func() {
			finished <- server.HandleError(func() { posts[0]() })
		}()
		select {
		case err := <-finished:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			server.Raise(held.Rollback(ctx))
			<-finished
			t.Fatal("creation post repeated the blocked transfer_escrow census")
		}
		server.Raise(held.Rollback(ctx))
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 600 {
			t.Fatalf("reused snapshot mirror=%d, want 600", got)
		}
	})
}

// PostgreSQL can reuse a rolled-back numeric revision. A matching revision
// alone must never authenticate a post from a transaction that did not commit.
func TestNetEscrowRolledBackCreatePostRejectsReusedRevision(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		rolledBack, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer rolledBack.Rollback(context.Background())
		_, stalePosts, err := createTransferEscrowInTx(ctx, rolledBack,
			f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId,
			f.sourceNetworkId, 17, nil)
		server.Raise(err)
		server.Raise(rolledBack.Rollback(ctx))
		_, currentPosts := createNetEscrowOrderingTestContract(ctx, f, 23)
		stalePosts[0]()
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 23 {
			t.Fatalf("rolled-back post published %d bytes at a reused revision", got)
		}
		currentPosts[0]()
	})
}

type beforeEscrowInsertTestTx struct {
	server.PgTx
	before func()
}

func (tx *beforeEscrowInsertTestTx) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	if tx.before != nil {
		before := tx.before
		tx.before = nil
		before()
	}
	return tx.PgTx.SendBatch(ctx, batch)
}

// A terminal transition can advance reservation revisions without taking the
// admission balance lock. Force that concurrent commit between census and
// insertion; the post must recensus, not publish the old amount plus its delta.
func TestNetEscrowCreatePostRechecksConcurrentOutcomeRevision(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		prior, _ := createNetEscrowOrderingTestContract(ctx, f, 200)
		var posts []func() any
		server.Tx(ctx, func(tx server.PgTx) {
			controlled := &beforeEscrowInsertTestTx{PgTx: tx, before: func() {
				server.Db(ctx, func(conn server.PgConn) {
					server.RaisePgResult(conn.Exec(ctx, `UPDATE transfer_contract
						SET outcome='canceled',close_time=now() WHERE contract_id=$1`, prior.ContractId))
				})
			}}
			var err error
			_, posts, err = createTransferEscrowInTx(ctx, controlled,
				f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId,
				f.sourceNetworkId, 400, nil)
			server.Raise(err)
		}, server.TxReadCommitted, server.OptNoRetry())
		posts[0]()
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 400 {
			t.Fatalf("concurrent terminal transition left mirror=%d, want 400", got)
		}
	})
}

// Most control anchors reserve zero bytes. They must be removed before the
// expensive per-contract outcome lookups; the SUM and revision remain exact.
func TestNetEscrowCensusExcludesZeroAnchorsBeforeContractLookup(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		ids := []server.Id{f.balanceId}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,
				 payer_network_id,transfer_byte_count,outcome,close_time,provider_usage)
				SELECT md5('census-cost-'||n)::uuid,$1,$2,$3,$4,$1,CASE WHEN n<=8192 THEN 0 ELSE 1 END,
				 CASE WHEN n>8256 THEN 'settled' ELSE NULL END,
				 CASE WHEN n>8256 THEN now() ELSE NULL END,
				 CASE WHEN n>8256 THEN '{"version":1,"byte_count":0,"providers":[]}'::jsonb ELSE NULL END
				FROM generate_series(1,8320) AS n`,
				f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow
				(contract_id,balance_id,balance_byte_count)
				SELECT md5('census-cost-'||n)::uuid,$1,CASE WHEN n<=8192 THEN 0 ELSE 1 END
				FROM generate_series(1,8320) AS n`, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE transfer_escrow`))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE transfer_contract`))
			beforeSQL := strings.Replace(netEscrowReservationPageSQL,
				" AND\n                transfer_escrow.balance_byte_count <> 0", "", 1)
			for _, query := range []struct{ name, sql string }{
				{"before_zero_filter", beforeSQL}, {"after_zero_filter", netEscrowReservationPageSQL},
			} {
				server.RaisePgResult(tx.Exec(ctx, `PREPARE `+query.name+` AS `+query.sql))
				defer tx.Exec(context.WithoutCancel(ctx), `DEALLOCATE `+query.name)
			}
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode=`+mode))
				var beforeRows, afterRows int
				for _, name := range []string{"before_zero_filter", "after_zero_filter"} {
					var raw []byte
					server.Raise(tx.QueryRow(ctx, fmt.Sprintf(
						`EXPLAIN (ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) EXECUTE %s('{%s}'::uuid[])`, name, f.balanceId)).Scan(&raw))
					var plans []map[string]any
					server.Raise(json.Unmarshal(raw, &plans))
					root := plans[0]["Plan"].(map[string]any)
					buffers := int(root["Shared Hit Blocks"].(float64) + root["Shared Read Blocks"].(float64))
					selectedRows := 0
					var inspect func(map[string]any)
					inspect = func(node map[string]any) {
						if node["Alias"] == "transfer_escrow" {
							selectedRows += int(node["Actual Rows"].(float64) * node["Actual Loops"].(float64))
						}
						if children, ok := node["Plans"].([]any); ok {
							for _, child := range children {
								inspect(child.(map[string]any))
							}
						}
					}
					inspect(root)
					if name == "before_zero_filter" {
						beforeRows = selectedRows
					} else {
						afterRows = selectedRows
					}
					t.Logf("%s %s contract_join_input_rows=%d buffers=%d execution_ms=%v", mode, name, selectedRows, buffers, plans[0]["Execution Time"])
				}
				if beforeRows != 8320 || afterRows != 128 {
					t.Fatalf("zero prefilter did not reduce contract join inputs: before=%d after=%d", beforeRows, afterRows)
				}
			}
			if got := readNetEscrowSnapshots(ctx, tx, ids)[f.balanceId].reserved; got != 64 {
				t.Fatalf("zero/closed history changed the exact reserved sum to %d", got)
			}
		}, server.TxReadCommitted, server.OptNoRetry())
	})
}
