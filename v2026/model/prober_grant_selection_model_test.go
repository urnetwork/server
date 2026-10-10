// The internal prober's candidate window is dynamic, bounded, and only a read
// optimization. Financial writes and ordinary payer ordering stay shared.
package model

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Synthetic persisted identity, never an operational account or configuration.
func setDynamicProberIdentityForTest(t testing.TB, ctx context.Context, clients escrowSelectionTestClients) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO prober_identity (singleton,network_id,client_id)
			VALUES (true,$1,$2) ON CONFLICT (singleton) DO UPDATE
			SET network_id=EXCLUDED.network_id,client_id=EXCLUDED.client_id`, clients.payerNetworkId, clients.payerId))
	})
}

// Each caller supplies explicit timestamps so order never depends on execution speed.
func addDynamicProberGrantForTest(ctx context.Context, networkId server.Id, start, end time.Time, bytes ByteCount) *TransferBalance {
	grant := &TransferBalance{NetworkId: networkId, StartTime: start, EndTime: end,
		StartBalanceByteCount: ProberTransferBalanceTopUp, BalanceByteCount: bytes}
	AddTransferBalance(ctx, grant)
	return grant
}

// The old allocator deterministically reads every grant and consumes the oldest.
// A later top-up must enter the bounded window without any configuration refresh.
func TestDynamicProberGrantBoundsRowsAndDiscoversReplenishment(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		// This test measures a first-candidate success. A random client whose
		// hash starts at the preceding round's now-reserved grant legitimately
		// needs an extra rejected attempt before discovering the new top-up.
		originalClientId := clients.payerId
		for clients.payerId.Hash()%proberGrantFirstCount != 0 {
			clients.payerId = server.NewId()
		}
		if clients.payerId != originalClientId {
			insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{clients.payerId: clients.payerNetworkId})
		}
		setDynamicProberIdentityForTest(t, ctx, clients)
		now := server.NowUtc()
		for index := range 80 {
			addDynamicProberGrantForTest(ctx, clients.payerNetworkId, now.Add(time.Duration(index-100)*time.Minute), now.Add(time.Hour), 256)
		}
		for round := range 2 {
			newest := addDynamicProberGrantForTest(ctx, clients.payerNetworkId, now.Add(time.Duration(round-2)*time.Minute), now.Add(2*time.Hour), 1024)
			var query *escrowGrantQueryTestTx
			var escrow *TransferEscrow
			var posts []func() any
			server.Tx(ctx, func(tx server.PgTx) {
				query = &escrowGrantQueryTestTx{PgTx: tx}
				var err error
				escrow, posts, err = createTransferEscrowInTx(ctx, query, clients.payerNetworkId, clients.payerId,
					clients.providerNetworkId, clients.providerId, clients.payerNetworkId, 1024, nil)
				server.Raise(err)
			})
			server.RunPosts(ctx, posts...)
			if query.grantQueries != 2 || query.grantRows > 32 {
				t.Errorf("round %d: grant queries=%d rows=%d, want candidate and lock reads of at most 16 rows each", round, query.grantQueries, query.grantRows)
			}
			if len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != newest.BalanceId || escrow.Balances[0].BalanceByteCount != 1024 || escrow.Priority != UnpaidPriority {
				t.Fatalf("round %d: newest sufficient internal grant did not fund the unchanged contract", round)
			}
			if Testing_NetEscrowByteCount(ctx, newest.BalanceId) != 1024 {
				t.Fatal("selected reservation mirror lost the allocated bytes")
			}
		}
	})
}

// The second window skips a fully reserved head; exhausting both windows must
// preserve the original paid grant's exact allocation, priority, and mirrors.
func TestDynamicProberGrantReservedWindowsAndExactFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		// Put the sole unreserved extended-window grant first in this payer's
		// bounded preference order; the fallback round has no free candidate.
		for suffix := range 65536 {
			id := server.Id{0: 0xa9, 14: byte(suffix >> 8), 15: byte(suffix)}
			if id.Hash()%proberGrantExtendedCount == 0 {
				clients.payerId = id
				break
			}
		}
		if clients.payerId.Hash()%proberGrantExtendedCount != 0 {
			t.Fatal("fixture did not choose deterministic extended preference")
		}
		insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{clients.payerId: clients.payerNetworkId})
		setDynamicProberIdentityForTest(t, ctx, clients)
		now := server.NowUtc()
		grants := []*TransferBalance{}
		for index := range 65 {
			grants = append(grants, addDynamicProberGrantForTest(ctx, clients.payerNetworkId,
				now.Add(time.Duration(index-100)*time.Minute), now.Add(time.Hour+time.Duration(index)*time.Minute), 4096))
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=1 WHERE balance_id=$1`, grants[0].BalanceId))
		})
		deadline := now.Truncate(time.Second).Add(time.Hour)
		for index, grant := range grants {
			if index != 0 && index != 48 {
				clients.reserve(ctx, grant.BalanceId, 4096)
			}
		}
		server.Redis(ctx, func(r server.RedisClient) {
			for index, grant := range grants {
				if index != 0 && index != 48 {
					server.Raise(r.Set(ctx, netEscrowKey(grant.BalanceId), 4096, 0).Err())
					server.Raise(r.ExpireAt(ctx, netEscrowKey(grant.BalanceId), deadline).Err())
				}
			}
		})
		for round := range 2 {
			var query *escrowGrantQueryTestTx
			var escrow *TransferEscrow
			var posts []func() any
			server.Tx(ctx, func(tx server.PgTx) {
				query = &escrowGrantQueryTestTx{PgTx: tx}
				var err error
				escrow, posts, err = createTransferEscrowInTx(ctx, query, clients.payerNetworkId, clients.payerId,
					clients.providerNetworkId, clients.providerId, clients.payerNetworkId, 1024, nil)
				server.Raise(err)
			})
			server.RunPosts(ctx, posts...)
			wantedIndex, wantedQueries, wantedRows := 48, proberGrantAttemptsPerWindow+3, 65+proberGrantAttemptsPerWindow
			wantedPriority := Priority(UnpaidPriority)
			if round == 1 {
				wantedIndex, wantedQueries, wantedRows = 0, 2*proberGrantAttemptsPerWindow+3, 129+2*proberGrantAttemptsPerWindow
				wantedPriority = PaidPriority
			}
			if query.grantQueries != wantedQueries || query.grantRows != wantedRows {
				t.Fatalf("round %d: queries=%d rows=%d, want %d/%d", round, query.grantQueries, query.grantRows, wantedQueries, wantedRows)
			}
			if len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != grants[wantedIndex].BalanceId || escrow.Balances[0].BalanceByteCount != 1024 || escrow.Priority != wantedPriority {
				t.Fatalf("round %d: changed selected grant, reserved bytes, or priority", round)
			}
			server.Redis(ctx, func(r server.RedisClient) {
				key := netEscrowKey(grants[64].BalanceId)
				value, err := r.Get(ctx, key).Int64()
				server.Raise(err)
				expires, err := r.ExpireTime(ctx, key).Result()
				server.Raise(err)
				if value != 4096 || expires != time.Duration(deadline.Unix())*time.Second {
					t.Fatal("candidate reads changed an unallocated reservation or its deadline")
				}
				server.Raise(r.Set(ctx, netEscrowKey(grants[48].BalanceId), 4096, time.Hour).Err())
			})
			if round == 0 {
				clients.reserve(ctx, grants[48].BalanceId, 3072)
			}
		}
	})
}

// A matching identity is not a spending exemption. All funding predicates,
// current durable bytes, and committed reservations remain authoritative.
func TestDynamicProberGrantEligibilityAndReservationSemantics(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, legacy := range []bool{false, true} {
			for _, name := range []string{"wrong identity", "missing identity", "wrong payer", "inactive", "expired", "future", "paid", "pro", "negative revenue", "subsidized", "small original", "reserved", "over-reserved", "partial", "negative reservation", "exact spendable", "missing reservation"} {
				clients := newEscrowSelectionTestClients(t, ctx)
				setDynamicProberIdentityForTest(t, ctx, clients)
				now := server.NowUtc()
				old := addDynamicProberGrantForTest(ctx, clients.payerNetworkId, now.Add(-2*time.Hour), now.Add(time.Hour), 4096)
				candidate := addDynamicProberGrantForTest(ctx, clients.payerNetworkId, now.Add(-time.Hour), now.Add(2*time.Hour), 4096)
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_balance_byte_count=4096 WHERE balance_id=$1`, old.BalanceId))
					switch name {
					case "wrong identity":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE prober_identity SET network_id=$1 WHERE singleton`, server.NewId()))
					case "missing identity":
						server.RaisePgResult(tx.Exec(ctx, `DELETE FROM prober_identity WHERE singleton`))
					case "wrong payer":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET network_id=$2 WHERE balance_id=$1`, candidate.BalanceId, server.NewId()))
					case "inactive":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET balance_byte_count=0 WHERE balance_id=$1`, candidate.BalanceId))
					case "expired":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET end_time=$2 WHERE balance_id=$1`, candidate.BalanceId, now.Add(-time.Second)))
					case "future":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_time=$2 WHERE balance_id=$1`, candidate.BalanceId, now.Add(time.Hour)))
					case "paid":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=1 WHERE balance_id=$1`, candidate.BalanceId))
					case "pro":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET pro=true WHERE balance_id=$1`, candidate.BalanceId))
					case "negative revenue":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=-1 WHERE balance_id=$1`, candidate.BalanceId))
					case "subsidized":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET subsidy_net_revenue_nano_cents=1 WHERE balance_id=$1`, candidate.BalanceId))
					case "small original":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_balance_byte_count=4096 WHERE balance_id=$1`, candidate.BalanceId))
					}
				})
				reserved := ByteCount(0)
				switch name {
				case "reserved":
					reserved = 4096
				case "over-reserved":
					reserved = 8192
				case "partial":
					reserved = 3584
				case "negative reservation":
					reserved = -1024
				case "exact spendable":
					reserved = 3072
				}
				if reserved != 0 {
					if reserved > 0 {
						clients.reserve(ctx, candidate.BalanceId, reserved)
					}
					server.Redis(ctx, func(r server.RedisClient) {
						server.Raise(r.Set(ctx, netEscrowKey(candidate.BalanceId), reserved, time.Hour).Err())
					})
				}
				var escrow *TransferEscrow
				var err error
				if legacy {
					escrow, err = createTransferEscrow(ctx, clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId, 1024)
				} else {
					escrow, err = clients.create(ctx, 1024, false)
				}
				if err != nil || escrow == nil {
					t.Fatalf("legacy=%t %s: allocation failed: %v", legacy, name, err)
				}
				wanted := old.BalanceId
				if legacy && name == "negative reservation" || name == "exact spendable" || name == "missing reservation" {
					wanted = candidate.BalanceId
				}
				if len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != wanted || escrow.Balances[0].BalanceByteCount != 1024 || escrow.Priority != UnpaidPriority {
					t.Fatalf("legacy=%t %s: changed eligibility, exact allocation, or priority: %+v", legacy, name, escrow)
				}
			}
		}
	})
}

// A real reservation shortfall cannot create new financial rows or mutate
// mirrors, whether the cache is coherent or malformed.
func TestDynamicProberGrantFailureHasNoWrites(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, malformed := range []bool{false, true} {
			clients := newEscrowSelectionTestClients(t, ctx)
			setDynamicProberIdentityForTest(t, ctx, clients)
			now := server.NowUtc()
			grant := addDynamicProberGrantForTest(ctx, clients.payerNetworkId, now.Add(-time.Hour), now.Add(time.Hour), 1024)
			clients.reserve(ctx, grant.BalanceId, 768)
			mirror := "768"
			if malformed {
				mirror = "not-a-number"
			}
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Set(ctx, netEscrowKey(grant.BalanceId), mirror, time.Hour).Err())
			})
			var escrow *TransferEscrow
			var err error
			panicErr := server.HandleError(func() { escrow, err = clients.create(ctx, 512, false) })
			if escrow != nil || err == nil || panicErr != nil {
				t.Fatal("durable shortfall did not fail closed independently of cache contents")
			}
			server.Db(ctx, func(conn server.PgConn) {
				var contracts, escrows int
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE payer_network_id=$1`, clients.payerNetworkId).Scan(&contracts))
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_escrow WHERE balance_id=$1`, grant.BalanceId).Scan(&escrows))
				if contracts != 1 || escrows != 1 {
					t.Fatal("failed allocation committed financial rows")
				}
			})
			server.Redis(ctx, func(r server.RedisClient) {
				value, err := r.Get(ctx, netEscrowKey(grant.BalanceId)).Result()
				server.Raise(err)
				if value != mirror {
					t.Fatal("failed allocation changed the reservation mirror")
				}
			})
		}
	})
}

// Origin and reverse companion use the payer's grant; committed concurrent
// reservations must retain every mirror increment on the unchanged post path.
func TestDynamicProberGrantCompanionAndConcurrentMirrors(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		setDynamicProberIdentityForTest(t, ctx, clients)
		now := server.NowUtc()
		grant := addDynamicProberGrantForTest(ctx, clients.payerNetworkId, now.Add(-time.Hour), now.Add(time.Hour), ProberTransferBalanceTopUp)
		for _, companion := range []bool{false, true} {
			escrow, err := clients.create(ctx, 1024, companion)
			if err != nil || escrow == nil || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != grant.BalanceId || escrow.Balances[0].BalanceByteCount != 1024 {
				t.Fatal("origin or reverse companion changed payer or reserved bytes")
			}
		}
		var group sync.WaitGroup
		failures := make(chan any, 12)
		start := make(chan struct{})
		for range 12 {
			group.Go(func() {
				<-start
				if failure := server.HandleError(func() {
					escrow, err := clients.create(ctx, 1024, false)
					server.Raise(err)
					if escrow == nil || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != grant.BalanceId {
						panic("unexpected allocation")
					}
				}); failure != nil {
					failures <- failure
				}
			})
		}
		close(start)
		group.Wait()
		close(failures)
		if len(failures) != 0 || Testing_NetEscrowByteCount(ctx, grant.BalanceId) != 14*1024 {
			t.Fatal("committed reservations lost or duplicated a mirror increment")
		}
	})
}

// A representative local table proves index-backed bounded row work without
// planner hints. Main buffer cost remains a separately observed rollout gate.
func TestDynamicProberGrantUsesOrderedIndexWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		setDynamicProberIdentityForTest(t, ctx, clients)
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			for _, networkId := range []server.Id{clients.payerNetworkId, server.NewId()} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance
					(balance_id,network_id,start_time,end_time,start_balance_byte_count,balance_byte_count,
						net_revenue_nano_cents,subsidy_net_revenue_nano_cents,pro)
					SELECT md5($1::uuid::text || ':' || i::text)::uuid,$1::uuid,$2::timestamp-i*interval '1 minute',
						$2::timestamp+interval '1 day',$3,$3,0,0,false FROM generate_series(1,4096) AS i`, networkId, now, ProberTransferBalanceTopUp))
			}
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE transfer_balance`))
			var raw []byte
			server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) `+proberGrantSelectionSql,
				clients.payerNetworkId, now, proberGrantFirstCount, ProberTransferBalanceTopUp).Scan(&raw))
			var plans []map[string]any
			server.Raise(json.Unmarshal(raw, &plans))
			indexFound := false
			var inspect func(map[string]any)
			inspect = func(plan map[string]any) {
				if plan["Relation Name"] == "transfer_balance" {
					if plan["Index Name"] != "transfer_balance_active_network_id_start_end_time" || plan["Scan Direction"] != "Backward" || plan["Actual Rows"].(float64) > 16 {
						t.Fatal("candidate query lost its bounded backward index window")
					}
					indexFound = true
				}
				if children, ok := plan["Plans"].([]any); ok {
					for _, child := range children {
						inspect(child.(map[string]any))
					}
				}
			}
			inspect(plans[0]["Plan"].(map[string]any))
			if !indexFound {
				t.Fatal("candidate query has no index-backed grant read")
			}
		})
	})
}
