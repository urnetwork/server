package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func setPreferredProberGrantsForTest(t testing.TB, networkId server.Id, balanceIds ...server.Id) {
	t.Helper()
	old := proberPreferredGrants
	config := &proberPreferredGrantConfig{NetworkId: networkId, BalanceIds: append([]server.Id(nil), balanceIds...)}
	proberPreferredGrants = func() *proberPreferredGrantConfig { return config }
	t.Cleanup(func() { proberPreferredGrants = old })
}

func setPreferredProberIdentityForTest(t testing.TB, ctx context.Context, clients escrowSelectionTestClients) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO prober_identity (singleton,network_id,client_id)
			VALUES (true,$1,$2) ON CONFLICT (singleton) DO UPDATE
			SET network_id=EXCLUDED.network_id,client_id=EXCLUDED.client_id`, clients.payerNetworkId, clients.payerId))
	})
}

func addPreferredProberTestGrant(ctx context.Context, networkId server.Id, end time.Time, bytes ByteCount) *TransferBalance {
	grant := &TransferBalance{NetworkId: networkId, StartTime: server.NowUtc().Add(-time.Hour), EndTime: end,
		StartBalanceByteCount: ProberTransferBalanceTopUp, BalanceByteCount: bytes}
	AddTransferBalance(ctx, grant)
	return grant
}

// This fails on the old allocator: it returns all current grants and spends
// the earlier grant rather than the explicitly configured internal free one.
func TestPreferredProberGrantSkipsFullRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		setPreferredProberIdentityForTest(t, ctx, clients)
		now := server.NowUtc()
		oldGrant := addPreferredProberTestGrant(ctx, clients.payerNetworkId, now.Add(time.Hour), 4096)
		preferred := addPreferredProberTestGrant(ctx, clients.payerNetworkId, now.Add(2*time.Hour), ProberTransferBalanceTopUp)
		for range 32 {
			addPreferredProberTestGrant(ctx, clients.payerNetworkId, now.Add(3*time.Hour), 4096)
		}
		setPreferredProberGrantsForTest(t, clients.payerNetworkId, preferred.BalanceId)
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
		if query.grantQueries != 1 || query.grantRows != 1 {
			t.Errorf("grant queries=%d rows=%d, want one PK query and one row", query.grantQueries, query.grantRows)
		}
		if len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != preferred.BalanceId || escrow.Balances[0].BalanceByteCount != 1024 || escrow.Priority != UnpaidPriority {
			t.Fatal("preferred internal grant did not fully fund the unchanged contract")
		}
		if Testing_NetEscrowByteCount(ctx, oldGrant.BalanceId) != 0 || Testing_NetEscrowByteCount(ctx, preferred.BalanceId) != 1024 {
			t.Fatal("reservation mirror changed an unallocated grant or lost the selected reservation")
		}
	})
}

func TestPreferredProberGrantConfigRejectsMalformed(t *testing.T) {
	networkId, balanceId := server.NewId(), server.NewId()
	for _, test := range []struct {
		name, yaml       string
		enabled, invalid bool
	}{
		{"disabled", "enabled: false\n", false, false},
		{"valid", fmt.Sprintf("version: 1\nenabled: true\nnetwork_id: %s\nbalance_ids: [%s]\n", networkId, balanceId), true, false},
		{"duplicate", fmt.Sprintf("version: 1\nenabled: true\nnetwork_id: %s\nbalance_ids: [%s, %s]\n", networkId, balanceId, balanceId), false, true},
		{"invalid payer", fmt.Sprintf("version: 1\nenabled: true\nnetwork_id: invalid\nbalance_ids: [%s]\n", balanceId), false, true},
		{"invalid grant", fmt.Sprintf("version: 1\nenabled: true\nnetwork_id: %s\nbalance_ids: [invalid]\n", networkId), false, true},
		{"missing version", fmt.Sprintf("enabled: true\nnetwork_id: %s\nbalance_ids: [%s]\n", networkId, balanceId), false, true},
		{"empty", fmt.Sprintf("version: 1\nenabled: true\nnetwork_id: %s\nbalance_ids: []\n", networkId), false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Cleanup(server.Config.PushSimpleResource(proberPreferredGrantResource, []byte(test.yaml)))
			config, err := loadProberPreferredGrantConfig()
			if (err != nil) != test.invalid || (config != nil) != test.enabled {
				t.Fatalf("enabled=%t invalid=%t, want %t/%t", config != nil, err != nil, test.enabled, test.invalid)
			}
		})
	}
	t.Run("oversized", func(t *testing.T) {
		text := fmt.Sprintf("version: 1\nenabled: true\nnetwork_id: %s\nbalance_ids:\n", networkId)
		for range proberPreferredGrantLimit + 1 {
			text += fmt.Sprintf("  - %s\n", server.NewId())
		}
		t.Cleanup(server.Config.PushSimpleResource(proberPreferredGrantResource, []byte(text)))
		if config, err := loadProberPreferredGrantConfig(); err == nil || config != nil {
			t.Fatal("oversized candidate list was accepted")
		}
	})
}

func requirePreferredProberAllocation(t testing.TB, ctx context.Context, escrow *TransferEscrow, grant *TransferBalance, bytes ByteCount, priority Priority) {
	t.Helper()
	if escrow == nil || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != grant.BalanceId || escrow.Balances[0].BalanceByteCount != bytes || escrow.Priority != priority || escrow.TransferByteCount != bytes {
		t.Fatal("allocation changed grant, exact reserved bytes, or priority")
	}
	server.Db(ctx, func(conn server.PgConn) {
		var storedPayer, storedBalance server.Id
		var storedBytes ByteCount
		var storedPriority Priority
		server.Raise(conn.QueryRow(ctx, `SELECT c.payer_network_id,e.balance_id,e.balance_byte_count,c.priority
			FROM transfer_contract c JOIN transfer_escrow e USING (contract_id)
			WHERE c.contract_id=$1`, escrow.ContractId).Scan(&storedPayer, &storedBalance, &storedBytes, &storedPriority))
		if storedPayer != grant.NetworkId || storedBalance != grant.BalanceId || storedBytes != bytes || storedPriority != priority {
			t.Fatal("durable escrow or payer differs from returned allocation")
		}
	})
}

func TestPreferredProberGrantOriginAndCompanionRotateAndRecheck(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		setPreferredProberIdentityForTest(t, ctx, clients)
		now := server.NowUtc()
		old := addPreferredProberTestGrant(ctx, clients.payerNetworkId, now.Add(time.Hour), 32768)
		grants := []*TransferBalance{
			addPreferredProberTestGrant(ctx, clients.payerNetworkId, now.Add(2*time.Hour), 4096),
			addPreferredProberTestGrant(ctx, clients.payerNetworkId, now.Add(3*time.Hour), 4096),
		}
		setPreferredProberGrantsForTest(t, clients.payerNetworkId, grants[0].BalanceId, grants[1].BalanceId)
		first := int(clients.payerId.Hash() % 2)
		for _, companion := range []bool{false, true} {
			escrow, err := clients.create(ctx, 1024, companion)
			if err != nil {
				t.Fatal(err)
			}
			requirePreferredProberAllocation(t, ctx, escrow, grants[first], 1024, UnpaidPriority)
		}
		// The first grant now has only 2048 spendable bytes. Re-read Redis and
		// rotate; never reuse the earlier 4096-byte spendability observation.
		escrow, err := clients.create(ctx, 3072, false)
		if err != nil {
			t.Fatal(err)
		}
		requirePreferredProberAllocation(t, ctx, escrow, grants[1-first], 3072, UnpaidPriority)
		if Testing_NetEscrowByteCount(ctx, grants[first].BalanceId) != 2048 || Testing_NetEscrowByteCount(ctx, grants[1-first].BalanceId) != 3072 || Testing_NetEscrowByteCount(ctx, old.BalanceId) != 0 {
			t.Fatal("rotation lost or duplicated reservations")
		}
		// Re-read durable activity too. Neither configured candidate can
		// fully fund this request, so general earliest-expiry fallback wins.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET balance_byte_count=0 WHERE balance_id=$1`, grants[first].BalanceId))
		})
		escrow, err = clients.create(ctx, 2048, false)
		if err != nil {
			t.Fatal(err)
		}
		requirePreferredProberAllocation(t, ctx, escrow, old, 2048, UnpaidPriority)
	})
}

func TestPreferredProberGrantEligibilityAndReservationFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, name := range []string{"wrong payer", "wrong identity", "missing identity", "inactive", "expired", "future", "paid", "pro", "negative revenue", "subsidized", "small original", "missing grant", "reserved", "over-reserved", "partial", "negative reservation", "exact spendable", "missing reservation"} {
			clients := newEscrowSelectionTestClients(t, ctx)
			setPreferredProberIdentityForTest(t, ctx, clients)
			now := server.NowUtc()
			old := addPreferredProberTestGrant(ctx, clients.payerNetworkId, now.Add(time.Hour), 4096)
			preferred := addPreferredProberTestGrant(ctx, clients.payerNetworkId, now.Add(2*time.Hour), 4096)
			configuredId := preferred.BalanceId
			server.Tx(ctx, func(tx server.PgTx) {
				switch name {
				case "wrong payer":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET network_id=$2 WHERE balance_id=$1`, preferred.BalanceId, server.NewId()))
				case "wrong identity":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE prober_identity SET network_id=$1 WHERE singleton`, server.NewId()))
				case "missing identity":
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM prober_identity WHERE singleton`))
				case "inactive":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET balance_byte_count=0 WHERE balance_id=$1`, preferred.BalanceId))
				case "expired":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET end_time=$2 WHERE balance_id=$1`, preferred.BalanceId, now.Add(-time.Second)))
				case "future":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_time=$2 WHERE balance_id=$1`, preferred.BalanceId, now.Add(time.Hour)))
				case "paid":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=1 WHERE balance_id=$1`, preferred.BalanceId))
				case "pro":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET pro=true WHERE balance_id=$1`, preferred.BalanceId))
				case "negative revenue":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=-1 WHERE balance_id=$1`, preferred.BalanceId))
				case "subsidized":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET subsidy_net_revenue_nano_cents=1 WHERE balance_id=$1`, preferred.BalanceId))
				case "small original":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_balance_byte_count=$2 WHERE balance_id=$1`, preferred.BalanceId, 32*Gib))
				case "missing grant":
					configuredId = server.NewId()
				}
			})
			reserved := ByteCount(0)
			reservationExists := true
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
			default:
				reservationExists = false
			}
			deadline := now.Truncate(time.Second).Add(time.Hour)
			if reservationExists {
				server.Redis(ctx, func(r server.RedisClient) {
					server.Raise(r.Set(ctx, netEscrowKey(preferred.BalanceId), reserved, 0).Err())
					server.Raise(r.ExpireAt(ctx, netEscrowKey(preferred.BalanceId), deadline).Err())
				})
			}
			setPreferredProberGrantsForTest(t, clients.payerNetworkId, configuredId)
			escrow, err := clients.create(ctx, 1024, false)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			selected := name == "negative reservation" || name == "exact spendable" || name == "missing reservation"
			want := old
			if selected {
				want = preferred
			}
			requirePreferredProberAllocation(t, ctx, escrow, want, 1024, UnpaidPriority)
			if !selected && reservationExists {
				server.Redis(ctx, func(r server.RedisClient) {
					value, err := r.Get(ctx, netEscrowKey(preferred.BalanceId)).Int64()
					server.Raise(err)
					expires, err := r.ExpireTime(ctx, netEscrowKey(preferred.BalanceId)).Result()
					server.Raise(err)
					if value != reserved || expires != time.Duration(deadline.Unix())*time.Second {
						t.Fatalf("%s: unsuccessful preferred read mutated its mirror or TTL", name)
					}
				})
			}
		}
	})
}

func TestPreferredProberGrantOrdinaryAndZeroByteCompatibility(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		prober := newEscrowSelectionTestClients(t, ctx)
		setPreferredProberIdentityForTest(t, ctx, prober)
		for _, zero := range []bool{false, true} {
			clients := prober
			if !zero {
				clients = newEscrowSelectionTestClients(t, ctx)
			}
			now := server.NowUtc()
			old := addPreferredProberTestGrant(ctx, clients.payerNetworkId, now.Add(time.Hour), 2048)
			preferred := addPreferredProberTestGrant(ctx, clients.payerNetworkId, now.Add(2*time.Hour), 4096)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=1 WHERE balance_id=$1`, old.BalanceId))
			})
			setPreferredProberGrantsForTest(t, prober.payerNetworkId, preferred.BalanceId)
			bytes := ByteCount(1024)
			if zero {
				bytes = 0
			}
			server.Tx(ctx, func(tx server.PgTx) {
				query := &escrowGrantQueryTestTx{PgTx: tx}
				escrow, _, err := createTransferEscrowInTx(ctx, query, clients.payerNetworkId, clients.payerId,
					clients.providerNetworkId, clients.providerId, clients.payerNetworkId, bytes, nil)
				server.Raise(err)
				if query.grantQueries != 1 || query.grantRows != 2 || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != old.BalanceId || escrow.Priority != PaidPriority {
					t.Fatal("ordinary or zero-byte allocation changed query count, earliest anchor, or priority")
				}
			})
		}
	})
}

func TestPreferredProberGrantShortfallAndRedisErrorHaveNoWrites(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, malformed := range []bool{false, true} {
			clients := newEscrowSelectionTestClients(t, ctx)
			setPreferredProberIdentityForTest(t, ctx, clients)
			grant := addPreferredProberTestGrant(ctx, clients.payerNetworkId, server.NowUtc().Add(time.Hour), 1024)
			setPreferredProberGrantsForTest(t, clients.payerNetworkId, grant.BalanceId)
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
			if escrow != nil || (malformed && panicErr == nil) || (!malformed && (err == nil || panicErr != nil)) {
				t.Fatal("invalid reservation or shortfall did not fail closed")
			}
			server.Db(ctx, func(conn server.PgConn) {
				var contracts, escrows int
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE payer_network_id=$1`, clients.payerNetworkId).Scan(&contracts))
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_escrow WHERE balance_id=$1`, grant.BalanceId).Scan(&escrows))
				if contracts != 0 || escrows != 0 {
					t.Fatal("failed allocation committed financial rows")
				}
			})
			server.Redis(ctx, func(r server.RedisClient) {
				value, err := r.Get(ctx, netEscrowKey(grant.BalanceId)).Result()
				server.Raise(err)
				if value != mirror {
					t.Fatal("failed allocation changed reservation mirror")
				}
			})
		}
	})
}

func TestPreferredProberGrantPreservesClientLifecycleFence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		setPreferredProberIdentityForTest(t, ctx, clients)
		grant := addPreferredProberTestGrant(ctx, clients.payerNetworkId, server.NowUtc().Add(time.Hour), 4096)
		setPreferredProberGrantsForTest(t, clients.payerNetworkId, grant.BalanceId)
		server.Tx(ctx, func(tx server.PgTx) {
			query := &escrowGrantQueryTestTx{PgTx: tx, beforeGrant: func(_ []any) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, clients.providerId))
			}}
			escrow, posts, err := createTransferEscrowInTx(ctx, query, clients.payerNetworkId, clients.payerId,
				clients.providerNetworkId, clients.providerId, clients.payerNetworkId, 1024, nil)
			if err == nil || escrow != nil || len(posts) != 0 {
				t.Fatal("preferred grant bypassed inactive destination fence")
			}
			var count int
			server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE payer_network_id=$1`, clients.payerNetworkId).Scan(&count))
			if count != 0 {
				t.Fatal("inactive destination received a durable contract")
			}
		})
		if Testing_NetEscrowByteCount(ctx, grant.BalanceId) != 0 {
			t.Fatal("lifecycle rejection wrote a reservation mirror")
		}
	})
}

func TestPreferredProberGrantConcurrentReservations(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		setPreferredProberIdentityForTest(t, ctx, clients)
		grant := addPreferredProberTestGrant(ctx, clients.payerNetworkId, server.NowUtc().Add(time.Hour), ProberTransferBalanceTopUp)
		setPreferredProberGrantsForTest(t, clients.payerNetworkId, grant.BalanceId)
		var group sync.WaitGroup
		failures := make(chan any, 12)
		for range 12 {
			group.Go(func() {
				if failure := server.HandleError(func() {
					escrow, err := clients.create(ctx, 1024, false)
					server.Raise(err)
					if escrow == nil || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != grant.BalanceId {
						panic("unexpected concurrent allocation")
					}
				}); failure != nil {
					failures <- failure
				}
			})
		}
		group.Wait()
		close(failures)
		if len(failures) != 0 {
			t.Fatal("concurrent preferred allocation failed")
		}
		if Testing_NetEscrowByteCount(ctx, grant.BalanceId) != 12*1024 {
			t.Fatal("concurrent committed reservations lost or duplicated mirror increments")
		}
	})
}

func TestPreferredProberGrantUsesPrimaryKeyBoundary(t *testing.T) {
	if !strings.Contains(proberPreferredGrantSQL, "FROM unnest($1::uuid[])") || !strings.Contains(proberPreferredGrantSQL, "WHERE balance_id = requested.balance_id\n\t\tOFFSET 0") {
		t.Fatal("preferred grant query lost its bounded per-primary-key planner boundary")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		setPreferredProberIdentityForTest(t, ctx, clients)
		ids := []server.Id{}
		server.Tx(ctx, func(tx server.PgTx) {
			for index := range 512 {
				grant := &TransferBalance{NetworkId: clients.payerNetworkId, StartTime: server.NowUtc().Add(-time.Hour), EndTime: server.NowUtc().Add(time.Hour), StartBalanceByteCount: ProberTransferBalanceTopUp, BalanceByteCount: ProberTransferBalanceTopUp}
				AddTransferBalanceInTx(ctx, tx, grant)
				if index < proberPreferredGrantLimit {
					ids = append(ids, grant.BalanceId)
				}
			}
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE transfer_balance`))
			var raw []byte
			server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) `+proberPreferredGrantSQL, ids, clients.payerNetworkId, server.NowUtc(), ProberTransferBalanceTopUp).Scan(&raw))
			var plans []map[string]any
			server.Raise(json.Unmarshal(raw, &plans))
			primaryKey := false
			var inspect func(map[string]any)
			inspect = func(plan map[string]any) {
				if plan["Relation Name"] == "transfer_balance" {
					if plan["Index Name"] != "transfer_balance_pkey" {
						t.Fatal("preferred grant read used a non-primary-key access path")
					}
					primaryKey = true
				}
				if children, ok := plan["Plans"].([]any); ok {
					for _, child := range children {
						inspect(child.(map[string]any))
					}
				}
			}
			inspect(plans[0]["Plan"].(map[string]any))
			if !primaryKey {
				t.Fatal("preferred grant plan has no primary-key probe")
			}
		})
	})
}

func TestPreferredProberGrantExactTimeBoundaries(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, boundary := range []string{"inclusive start", "exclusive end", "future start"} {
			clients := newEscrowSelectionTestClients(t, ctx)
			setPreferredProberIdentityForTest(t, ctx, clients)
			now := server.NowUtc()
			old := addPreferredProberTestGrant(ctx, clients.payerNetworkId, now.Add(time.Hour), 4096)
			preferred := addPreferredProberTestGrant(ctx, clients.payerNetworkId, now.Add(2*time.Hour), 4096)
			setPreferredProberGrantsForTest(t, clients.payerNetworkId, preferred.BalanceId)
			server.Tx(ctx, func(tx server.PgTx) {
				var allocationTime time.Time
				query := &escrowGrantQueryTestTx{PgTx: tx, beforeGrant: func(args []any) {
					if len(args) == 2 {
						if !args[1].(time.Time).Equal(allocationTime) {
							t.Fatal("fallback changed the allocation clock")
						}
						return
					}
					if len(args) != 4 {
						t.Fatal("preferred grant query lost explicit clock or eligibility parameters")
					}
					allocationTime = args[2].(time.Time)
					start, end := allocationTime.Add(-time.Hour), allocationTime.Add(2*time.Hour)
					switch boundary {
					case "inclusive start":
						start = allocationTime
					case "exclusive end":
						end = allocationTime
					case "future start":
						start = allocationTime.Add(time.Microsecond)
					}
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_time=$2,end_time=$3 WHERE balance_id=$1`, preferred.BalanceId, start, end))
				}}
				escrow, _, err := createTransferEscrowInTx(ctx, query, clients.payerNetworkId, clients.payerId,
					clients.providerNetworkId, clients.providerId, clients.payerNetworkId, 1024, nil)
				server.Raise(err)
				want := old.BalanceId
				if boundary == "inclusive start" {
					want = preferred.BalanceId
				}
				if len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != want {
					t.Fatalf("%s: changed exact grant-window semantics", boundary)
				}
			})
		}
	})
}
