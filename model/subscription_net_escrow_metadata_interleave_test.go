package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Holds the metadata owner just after its cached revision read, signalling
// read, until release. The owner admits transaction ownership only on the
// server's own transaction (ddad361c), so the hold is a statement hook bound
// to that transaction's connection rather than a wrapper around it.
func metadataInterleaveHook(ctx context.Context, read chan struct{}, release <-chan struct{}) *legacyStatementHook {
	var once sync.Once
	return &legacyStatementHook{
		after: func(sql string, _ time.Duration, _ error) {
			if sql != netEscrowAdmissionCacheSQL {
				return
			}
			once.Do(func() {
				close(read)
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		},
	}
}

func metadataBalanceFixture(t testing.TB, ctx context.Context) (netEscrowOrderingTestFixture, server.Id, []server.Id) {
	t.Helper()
	f := seedAdmissionCacheHistory(t, ctx, 3, 100)
	contract, _ := createNetEscrowOrderingTestContract(ctx, f, 2)
	ids := []server.Id{f.balanceId, server.NewId(), server.NewId()}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance
			(balance_id,network_id,start_time,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents,pro)
			SELECT balance_id,$2,now()-interval '1 minute',now()+interval '1 hour',100,100,0,false
			FROM unnest($1::uuid[]) AS selected(balance_id)`, ids[1:], f.sourceNetworkId))
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=1 WHERE contract_id=$1`, contract.ContractId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
			VALUES($1,$2,1),($1,$3,0)`, contract.ContractId, ids[1], ids[2]))
	})
	settlementCacheCloseReports(ctx, contract.ContractId)
	server.Tx(ctx, func(tx server.PgTx) {
		_, closed, err := settleEscrowInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
		server.Raise(err)
		if !closed {
			t.Fatal("financial outcome did not commit")
		}
	}, server.TxReadCommitted, server.OptNoRetry())
	return f, contract.ContractId, ids
}

func TestNetEscrowMetadataLocksAllBalancesBeforeEscrow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f, contract, ids := metadataBalanceFixture(t, ctx)
		stopHooks := startLegacyStatementHooks(t, ctx)
		defer stopHooks()
		read, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		defer unblock()
		done := make(chan error, 1)
		partial := map[server.Id]sweepPayout{f.balanceId: {escrowBalanceByteCount: 1, returnByteCount: 1}}
		go func() {
			done <- server.HandleError1(func() error {
				server.Tx(ctx, func(tx server.PgTx) {
					hooked, releaseHooks := withLegacyStatementHooks(ctx, tx, metadataInterleaveHook(ctx, read, release))
					defer releaseHooks()
					settleEscrowMetadataInTx(hooked, tx, contract, server.NowUtc(), partial)
				}, server.TxReadCommitted, server.OptNoRetry())
				return nil
			}, func(err error) error { return err })
		}()
		select {
		case <-read:
		case err := <-done:
			t.Fatal("metadata did not read its snapshot", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		for _, id := range ids {
			server.Db(ctx, func(conn server.PgConn) {
				tx, err := conn.Begin(ctx)
				server.Raise(err)
				defer tx.Rollback(context.WithoutCancel(ctx))
				_, err = tx.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE NOWAIT`, id)
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
					t.Fatal("metadata did not lock every existing contract balance before its escrow/cache read")
				}
			})
		}
		unblock()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		var settled int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FILTER(WHERE settled) FROM transfer_escrow WHERE contract_id=$1`, contract).Scan(&settled))
		})
		if settled != 1 {
			t.Fatal("balance lock scope expanded metadata writes beyond caller payout map")
		}
		before := settlementCacheSnapshot(ctx, ids)[f.balanceId]
		server.Tx(ctx, func(tx server.PgTx) {
			settleEscrowMetadataInTx(ctx, tx, contract, server.NowUtc(), partial)
		}, server.TxReadCommitted, server.OptNoRetry())
		if after := settlementCacheSnapshot(ctx, ids)[f.balanceId]; before.revision != after.revision || before.reserved != after.reserved {
			t.Fatal("metadata replay changed a settled reservation snapshot")
		}
		// Legacy missing balances and zero rows retain the old metadata behavior.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance WHERE balance_id=$1`, ids[1]))
			settleEscrowMetadataInTx(ctx, tx, contract, server.NowUtc(), map[server.Id]sweepPayout{
				ids[1]: {escrowBalanceByteCount: 1, returnByteCount: 1}, ids[2]: {},
			})
		}, server.TxReadCommitted, server.OptNoRetry())
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FILTER(WHERE settled) FROM transfer_escrow WHERE contract_id=$1`, contract).Scan(&settled))
		})
		if settled != 3 {
			t.Fatal("missing balance or zero metadata was lost")
		}
	})
}

func TestNetEscrowMetadataBalanceLocksCustomGenericPointPlans(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE transfer_balance SET(autovacuum_enabled=false);
				ALTER TABLE transfer_escrow SET(autovacuum_enabled=false); ANALYZE transfer_balance; ANALYZE transfer_escrow`))
		})
		f, contract, ids := metadataBalanceFixture(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance
				(balance_id,network_id,start_time,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents,pro)
				SELECT md5('unrelated-metadata-balance-'||n)::uuid,$1,now()-interval '1 minute',now()+interval '1 hour',1,1,0,false
				FROM generate_series(1,100000)n`, f.sourceNetworkId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
				SELECT md5('unrelated-metadata-contract-'||n)::uuid,$1,1 FROM generate_series(1,100000)n`, f.balanceId))
		})
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `PREPARE metadata_balance_lock AS `+settlementMetadataBalanceLocksSQL))
			defer conn.Exec(context.WithoutCancel(ctx), `DEALLOCATE metadata_balance_lock`)
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				server.RaisePgResult(conn.Exec(ctx, `SET plan_cache_mode=`+mode))
				var encoded []byte
				server.Raise(conn.QueryRow(ctx, fmt.Sprintf(`EXPLAIN(ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) EXECUTE metadata_balance_lock('%s'::uuid)`, contract)).Scan(&encoded))
				var plans []map[string]any
				server.Raise(json.Unmarshal(encoded, &plans))
				points, locks := 0, 0
				var visit func(map[string]any)
				visit = func(node map[string]any) {
					if node["Node Type"] == "LockRows" {
						locks++
					}
					if relation, ok := node["Relation Name"].(string); ok {
						condition, _ := node["Index Cond"].(string)
						switch relation {
						case "transfer_escrow":
							contractIndexed := strings.Contains(condition, "contract_id =")
							if node["Node Type"] == "Bitmap Heap Scan" {
								children, _ := node["Plans"].([]any)
								if len(children) == 1 {
									index := children[0].(map[string]any)
									condition, _ := index["Index Cond"].(string)
									// The fixture updates one reservation before adding two
									// others. Its old index entry can survive until vacuum;
									// the exact range still returns only three live rows.
									indexRows, _ := index["Actual Rows"].(float64)
									contractIndexed = index["Node Type"] == "Bitmap Index Scan" && strings.Contains(condition, "contract_id =") && indexRows >= 3 && indexRows <= 4 && index["Actual Loops"] == float64(1)
								}
							}
							if !contractIndexed || node["Actual Rows"] != float64(3) || node["Actual Loops"] != float64(1) {
								t.Fatalf("%s metadata contract scope node=%v index=%v rows=%v loops=%v contract_index_condition=%t", mode, node["Node Type"], node["Index Name"], node["Actual Rows"], node["Actual Loops"], contractIndexed)
							}
						case "transfer_balance":
							if !strings.Contains(condition, "balance_id =") || node["Actual Rows"] != float64(1) || node["Actual Loops"] != float64(3) {
								t.Fatal(mode, "metadata escaped balance primary-key probes")
							}
						default:
							t.Fatal(mode, "unexpected metadata locking relation")
						}
						points++
					}
					if children, ok := node["Plans"].([]any); ok {
						for _, child := range children {
							visit(child.(map[string]any))
						}
					}
				}
				visit(plans[0]["Plan"].(map[string]any))
				if points != 2 || locks != 1 {
					t.Fatalf("%s metadata points=%d lock_nodes=%d", mode, points, locks)
				}
				got := []server.Id{}
				rows, err := conn.Query(ctx, settlementMetadataBalanceLocksSQL, contract)
				server.WithPgResult(rows, err, func() {
					for rows.Next() {
						var id server.Id
						server.Raise(rows.Scan(&id))
						got = append(got, id)
					}
				})
				slices.SortFunc(ids, server.Id.Cmp)
				if !slices.Equal(got, ids) {
					t.Fatal(mode, "metadata did not visit balances in the settlement order")
				}
				t.Logf("%s exact_contract_rows=3 primary_key_balance_probes=3 execution_ms=%v", mode, plans[0]["Execution Time"])
			}
		})
	})
}

// A terminal metadata post does not change the amount of open reservations,
// but its settled-flag trigger advances the durable revision. It must not
// invalidate a concurrently maintained snapshot through an unprotected read.
func TestNetEscrowMetadataInterleavedAdmissionKeepsSnapshotCurrent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := seedAdmissionCacheHistory(t, ctx, 10001, 64)
		terminal, err := cacheTestCreate(ctx, f, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		settlementCacheCloseReports(ctx, terminal.ContractId)
		server.Tx(ctx, func(tx server.PgTx) {
			_, closed, err := settleEscrowInTx(ctx, tx, terminal.ContractId, ContractOutcomeSettled)
			server.Raise(err)
			if !closed {
				t.Fatal("normal financial outcome did not commit")
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		if _, err := cacheTestCreate(ctx, f, nil, false); err != nil {
			t.Fatal(err)
		}
		stopHooks := startLegacyStatementHooks(t, ctx)
		defer stopHooks()
		read, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		defer unblock()
		metadataDone := make(chan error, 1)
		go func() {
			metadataDone <- server.HandleError1(func() error {
				server.Tx(ctx, func(tx server.PgTx) {
					hooked, releaseHooks := withLegacyStatementHooks(ctx, tx, metadataInterleaveHook(ctx, read, release))
					defer releaseHooks()
					settleEscrowMetadataInTx(hooked, tx, terminal.ContractId, server.NowUtc(), map[server.Id]sweepPayout{
						f.balanceId: {escrowBalanceByteCount: 1, returnByteCount: 1},
					})
				}, server.TxReadCommitted, server.OptNoRetry())
				return nil
			}, func(err error) error { return err })
		}()
		select {
		case <-read:
		case err := <-metadataDone:
			t.Fatal("metadata did not reach its cached revision read", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		// Since 403b9c55 the metadata owner holds its balance's transaction
		// ownership across the cached read, and admission refuses rather than
		// waits inside its transaction. A concurrent admission on the same grant
		// therefore cannot interleave with the held read: it is refused as busy
		// before it touches a row, and is admitted once the metadata commits.
		if _, err := cacheTestCreate(ctx, f, nil, false); !errors.Is(err, errTransferBalanceOwnershipBusy) {
			t.Fatal("admission interleaved with metadata that owns its grant", err)
		}
		unblock()
		if err := <-metadataDone; err != nil {
			t.Fatal(err)
		}
		if _, err := cacheTestCreate(ctx, f, nil, false); err != nil {
			t.Fatal(err)
		}
		var revision, cachedRevision int64
		var cached ByteCount
		var encoded []byte
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT revision.revision,snapshot.revision,snapshot.reserved_byte_count
				FROM transfer_balance_net_escrow_revision revision
				JOIN transfer_balance_net_escrow_snapshot snapshot USING(balance_id)
				WHERE balance_id=$1`, f.balanceId).Scan(&revision, &cachedRevision, &cached))
			server.Raise(conn.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+netEscrowReservationPageSQL, []server.Id{f.balanceId}).Scan(&encoded))
		})
		var plans []map[string]any
		if err := json.Unmarshal(encoded, &plans); err != nil || len(plans) != 1 {
			t.Fatal("invalid exact census plan", err)
		}
		var escrowRows, contractProbes, buffers float64
		var visit func(map[string]any)
		visit = func(node map[string]any) {
			if name, _ := node["Relation Name"].(string); name == "transfer_escrow" || name == "transfer_contract" {
				rows, _ := node["Actual Rows"].(float64)
				loops, _ := node["Actual Loops"].(float64)
				if name == "transfer_escrow" {
					escrowRows += rows * loops
				} else {
					contractProbes += loops
				}
			}
			children, _ := node["Plans"].([]any)
			for _, child := range children {
				visit(child.(map[string]any))
			}
		}
		plan := plans[0]["Plan"].(map[string]any)
		visit(plan)
		buffers, _ = plan["Shared Hit Blocks"].(float64)
		t.Logf("cache_current=%t exact_escrow_rows=%.0f contract_probes=%.0f buffer_hits=%.0f exact_execution_ms=%v", cachedRevision == revision, escrowRows, contractProbes, buffers, plans[0]["Execution Time"])
		if got := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId})[f.balanceId].reserved; got != 10003 {
			t.Fatal("interleaving changed financial authority", got)
		}
		if cachedRevision != revision || cached != 10003 {
			t.Fatal(fmt.Sprintf("terminal metadata invalidated current cache: revision=%d cached_revision=%d cached_reserved=%d", revision, cachedRevision, cached))
		}
	})
}
