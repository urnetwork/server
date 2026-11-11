package model

// The funding order against the database (transfer_balance_funding_order.go):
// a Pro network spends its paid cycle grant before the daily free grant on
// every admission path, at paid priority, and settlement and the deadline close
// charge its escrows in the same order, while a free network and a network whose
// Pro comes from an old data code keep the earliest-expiry order. Pro networks
// get the daily free grant (owner, 2026-11-11: "pro should get the 1tib per
// month plus the 30gb per day", the monthly amount later confirmed as pro.yml's
// 10 TiB; pro_daily_free_grant_db_test.go). Free daily data carries no revenue,
// and traffic it funds pays providers nothing. Needs the test database.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A payer and a provider, each with a client in a network of its own. The
// payer's network has a network row, so the daily run, which grants every
// network row, reaches it.
func newProDailyGrantTestClients(t testing.TB, ctx context.Context) escrowSelectionTestClients {
	t.Helper()
	clients := escrowSelectionTestClients{
		payerNetworkId: server.NewId(), payerId: server.NewId(),
		providerNetworkId: server.NewId(), providerId: server.NewId(),
	}
	testingCreatePaymentNetworkRow(ctx, clients.payerNetworkId)
	testingCreatePaymentNetworkRow(ctx, clients.providerNetworkId)
	insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{
		clients.payerId: clients.payerNetworkId, clients.providerId: clients.providerNetworkId,
	})
	return clients
}

// The balance of the network's latest granted cycle.
func testingProDailyGrantCycleBalance(t testing.TB, ctx context.Context, networkId server.Id) (balanceId server.Id) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT balance_id FROM pro_grant_cycle
				WHERE network_id = $1 AND balance_id IS NOT NULL
				ORDER BY cycle_start_time DESC
				LIMIT 1
			`,
			networkId,
		).Scan(&balanceId))
	})
	return
}

type proDailyGrantTestBalance struct {
	pro       bool
	paid      bool
	grantKind GrantKind
	startTime time.Time
	endTime   time.Time
}

func testingProDailyGrantBalance(t testing.TB, ctx context.Context, balanceId server.Id) (balance proDailyGrantTestBalance) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT pro, paid, COALESCE(grant_kind, ''), start_time, end_time FROM transfer_balance WHERE balance_id = $1`,
			balanceId,
		).Scan(&balance.pro, &balance.paid, &balance.grantKind, &balance.startTime, &balance.endTime))
	})
	return
}

// Subscribes the network to a paid Stripe month that started an hour ago, as a
// credit path does: the renewal, the period's Pro marker (no bytes, with the
// period's revenue and window), and the cycle's grant (grantProCycleInTx), which
// the period's revenue makes paid. Returns the cycle grant.
func testingProDailyGrantPaidCycle(t testing.TB, ctx context.Context, networkId server.Id, now time.Time) server.Id {
	t.Helper()
	start := now.Add(-time.Hour)
	paidEnd := addProCycleMonths(start, 1)
	grace := 30 * 24 * time.Hour
	revenue := UsdToNanoCents(5)
	testingProCycleRenewal(t, ctx, networkId, SubscriptionMarketStripe, start, paidEnd, grace, revenue)
	AddTransferBalance(ctx, &TransferBalance{
		NetworkId:         networkId,
		StartTime:         start,
		EndTime:           paidEnd.Add(grace),
		SubsidyNetRevenue: revenue,
		Pro:               true,
	})
	if !testingGrantProCycle(ctx, networkId, now) {
		t.Fatal("no cycle grant for the paid month")
	}
	UpdateProNetwork(ctx, networkId)
	cycleGrantId := testingProDailyGrantCycleBalance(t, ctx, networkId)
	if balance := testingProDailyGrantBalance(t, ctx, cycleGrantId); !balance.pro || !balance.paid || balance.grantKind != GrantKindPro {
		t.Fatalf("the cycle grant is pro %t paid %t kind %q, want a paid Pro grant", balance.pro, balance.paid, balance.grantKind)
	}
	return cycleGrantId
}

// The day's free grant, as the daily run writes it for one network: the free
// kind and window, unpaid and not Pro.
func testingProDailyGrantFree(t testing.TB, ctx context.Context, networkId server.Id, now time.Time, byteCount ByteCount) (balanceId server.Id) {
	t.Helper()
	startTime, endTime := FreeGrantWindow(now)
	server.Tx(ctx, func(tx server.PgTx) {
		server.Raise(AddGrantTransferBalanceInTx(tx, ctx, networkId, GrantKindFree, byteCount, startTime, endTime))
	})
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT balance_id FROM transfer_balance WHERE network_id = $1 AND grant_kind = $2 AND start_time = $3`,
			networkId,
			GrantKindFree,
			startTime,
		).Scan(&balanceId))
	})
	return
}

// A data code redeemed while data codes still conferred Pro, as an AppSumo code
// redeemed in early 2026 left it: Pro, with revenue and data, for a century,
// and no grant kind. Such a network already got the daily grant.
func testingProDailyGrantOldProDataCode(ctx context.Context, networkId server.Id, now time.Time, byteCount ByteCount) server.Id {
	balance := &TransferBalance{
		NetworkId:             networkId,
		StartTime:             now.Add(-200 * 24 * time.Hour),
		EndTime:               now.Add(100 * 365 * 24 * time.Hour),
		StartBalanceByteCount: byteCount,
		BalanceByteCount:      byteCount,
		NetRevenue:            UsdToNanoCents(5),
		Pro:                   true,
	}
	AddTransferBalance(ctx, balance)
	return balance.BalanceId
}

// A data pack bought to the network: data only, with its revenue, valid a year.
func testingProDailyGrantDataPack(ctx context.Context, networkId server.Id, now time.Time, byteCount ByteCount) server.Id {
	balance := &TransferBalance{
		NetworkId:             networkId,
		StartTime:             now.Add(-time.Minute),
		EndTime:               now.Add(365 * 24 * time.Hour),
		StartBalanceByteCount: byteCount,
		BalanceByteCount:      byteCount,
		NetRevenue:            UsdToNanoCents(3),
	}
	AddTransferBalance(ctx, balance)
	return balance.BalanceId
}

// Creates the payer's contract on the public Redis admission path or the legacy
// PostgreSQL ledger path, as an origin or as a companion its payer funds.
func proDailyGrantTestCreate(ctx context.Context, clients escrowSelectionTestClients, byteCount ByteCount, legacy bool, companion bool) (*TransferEscrow, error) {
	if !legacy {
		return clients.create(ctx, byteCount, companion)
	}
	if companion {
		return createCompanionTransferEscrow(ctx, clients.providerNetworkId, clients.providerId,
			clients.payerNetworkId, clients.payerId, byteCount, time.Hour)
	}
	return createTransferEscrow(ctx, clients.payerNetworkId, clients.payerId,
		clients.providerNetworkId, clients.providerId, byteCount)
}

func proDailyGrantTestPathName(legacy bool, companion bool) string {
	name := "redis"
	if legacy {
		name = "legacy"
	}
	if companion {
		return name + " companion"
	}
	return name + " origin"
}

func proDailyGrantTestAllocations(escrow *TransferEscrow) map[server.Id]ByteCount {
	allocations := map[server.Id]ByteCount{}
	for _, balance := range escrow.Balances {
		allocations[balance.BalanceId] += balance.BalanceByteCount
	}
	return allocations
}

// The bytes per grant kind among `byteCounts`. A daily run of another test in
// the shared database can add a second daily grant with the same window, which
// ties with the test's own; counting by kind keeps a check exact either way.
func proDailyGrantTestKinds(t testing.TB, ctx context.Context, byteCounts map[server.Id]ByteCount) map[GrantKind]ByteCount {
	t.Helper()
	kinds := map[GrantKind]ByteCount{}
	for balanceId, byteCount := range byteCounts {
		kinds[testingProDailyGrantBalance(t, ctx, balanceId).grantKind] += byteCount
	}
	return kinds
}

// The bytes each balance funded in the contract's settlement sweeps.
func testingProDailyGrantSweptBytes(t testing.TB, ctx context.Context, contractId server.Id) map[server.Id]ByteCount {
	t.Helper()
	swept := map[server.Id]ByteCount{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT balance_id, SUM(payout_byte_count)::bigint
				FROM transfer_escrow_sweep
				WHERE contract_id = $1
				GROUP BY balance_id
			`,
			contractId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var balanceId server.Id
				var byteCount ByteCount
				server.Raise(result.Scan(&balanceId, &byteCount))
				swept[balanceId] = byteCount
			}
		})
	})
	return swept
}

// A Pro network spends its paid cycle grant before the daily free grant on
// every admission path, so its traffic stays paid: the contract is reserved from
// the cycle grant alone, at paid priority. By earliest expiry alone, the daily
// grant, which ends first, funded it unpaid.
func TestProNetworkFundsFromItsPaidCycleGrantBeforeTheDailyGrant(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingSetProData(t, 16*Mib)
		for _, legacy := range []bool{false, true} {
			for _, companion := range []bool{false, true} {
				name := proDailyGrantTestPathName(legacy, companion)
				clients := newProDailyGrantTestClients(t, ctx)
				if companion {
					// the companion's origin, before any grant
					if _, err := clients.create(ctx, 0, false); err != nil {
						t.Fatalf("%s: %v", name, err)
					}
				}
				now := server.NowUtc()
				cycleGrantId := testingProDailyGrantPaidCycle(t, ctx, clients.payerNetworkId, now)
				dailyGrantId := testingProDailyGrantFree(t, ctx, clients.payerNetworkId, now, 8*Mib)
				if daily, cycle := testingProDailyGrantBalance(t, ctx, dailyGrantId), testingProDailyGrantBalance(t, ctx, cycleGrantId); !daily.endTime.Before(cycle.endTime) {
					t.Fatalf("%s: the daily grant ends %s, after the cycle grant's %s", name, daily.endTime, cycle.endTime)
				}

				escrow, err := proDailyGrantTestCreate(ctx, clients, 1*Mib, legacy, companion)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				allocations := proDailyGrantTestAllocations(escrow)
				if len(allocations) != 1 || allocations[cycleGrantId] != 1*Mib || escrow.Priority != PaidPriority {
					t.Errorf("%s: drew cycle %d daily %d at priority %d, want 1 MiB of the paid cycle grant at paid priority",
						name, allocations[cycleGrantId], allocations[dailyGrantId], escrow.Priority)
				}
			}
		}
	})
}

// A Pro network's zero-byte contract anchors on its paid cycle grant, the grant
// that funds first, and so keeps paid priority. The zero escrow fallback's
// priority, read in the same funding order, is paid too.
func TestProNetworkZeroByteContractKeepsPaidPriority(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingSetProData(t, 16*Mib)
		clients := newProDailyGrantTestClients(t, ctx)
		now := server.NowUtc()
		cycleGrantId := testingProDailyGrantPaidCycle(t, ctx, clients.payerNetworkId, now)
		testingProDailyGrantFree(t, ctx, clients.payerNetworkId, now, 8*Mib)

		escrow, err := clients.create(ctx, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != cycleGrantId || escrow.Balances[0].BalanceByteCount != 0 || escrow.Priority != PaidPriority {
			t.Errorf("zero-byte anchor %+v at priority %d, want the paid cycle grant at paid priority", escrow.Balances, escrow.Priority)
		}

		for _, byteCount := range []ByteCount{0, 1 * Mib} {
			var priority Priority
			server.Tx(ctx, func(tx server.PgTx) {
				priority = zeroEscrowContractPriorityInTx(ctx, tx, clients.payerNetworkId, byteCount, server.NowUtc())
			})
			if priority != PaidPriority {
				t.Errorf("zero escrow priority for %d bytes is %d, want paid", byteCount, priority)
			}
		}
	})
}

// A contract larger than what is left of the paid cycle grant takes the rest of
// the cycle grant first and the remainder from the daily grant. When it
// settles, its usage is charged to the cycle grant first, so the bytes used are
// paid while the cycle grant covers them.
func TestProNetworkContractAcrossBothGrantsChargesThePaidGrantFirst(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingSetProData(t, 3*Mib)
		clients := newProDailyGrantTestClients(t, ctx)
		now := server.NowUtc()
		cycleGrantId := testingProDailyGrantPaidCycle(t, ctx, clients.payerNetworkId, now)
		dailyGrantId := testingProDailyGrantFree(t, ctx, clients.payerNetworkId, now, 8*Mib)

		escrow, err := clients.create(ctx, 4*Mib, false)
		if err != nil {
			t.Fatal(err)
		}
		allocations := proDailyGrantTestAllocations(escrow)
		kinds := proDailyGrantTestKinds(t, ctx, allocations)
		if len(allocations) != 2 || allocations[cycleGrantId] != 3*Mib || kinds[GrantKindFree] != 1*Mib || escrow.Priority != (PaidPriority+UnpaidPriority)/2 {
			t.Fatalf("reserved cycle %d daily %d (own daily grant %d) at priority %d, want the cycle grant's 3 MiB, then 1 MiB of the daily grant",
				allocations[cycleGrantId], kinds[GrantKindFree], allocations[dailyGrantId], escrow.Priority)
		}

		if err := CloseContract(ctx, escrow.ContractId, clients.payerId, 2*Mib, false); err != nil {
			t.Fatal(err)
		}
		if err := CloseContract(ctx, escrow.ContractId, clients.providerId, 2*Mib, false); err != nil {
			t.Fatal(err)
		}
		swept := testingProDailyGrantSweptBytes(t, ctx, escrow.ContractId)
		if sweptKinds := proDailyGrantTestKinds(t, ctx, swept); swept[cycleGrantId] != 2*Mib || sweptKinds[GrantKindFree] != 0 {
			t.Errorf("settlement charged cycle %d daily %d, want all 2 MiB used to the paid cycle grant", swept[cycleGrantId], sweptKinds[GrantKindFree])
		}
	})
}

// Settlement and the deadline close charge a contract's escrows in the funding
// order: a contract that a paid Pro grant and the daily grant funded together is
// charged to the paid Pro grant first, though the daily grant ends first. A free
// network's contract over its daily grant and a data pack, and the contract of
// a network whose Pro comes from an old data code, keep the earliest-expiry
// order: the daily grant first.
func TestSettlementChargesEscrowsInFundingOrder(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingSetProData(t, 16*Mib)
		now := server.NowUtc()

		// a contract funded by two grants, 1 MiB each, written as admission
		// leaves its escrow
		contract := func(clients escrowSelectionTestClients, balanceIds ...server.Id) server.Id {
			contractId := server.NewId()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO transfer_contract (contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,payer_network_id)
					VALUES ($1,$2,$3,$4,$5,$6,$2)`, contractId,
					clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId, ByteCount(len(balanceIds))*Mib))
				for _, balanceId := range balanceIds {
					server.RaisePgResult(tx.Exec(ctx, `
						INSERT INTO transfer_escrow (contract_id,balance_id,balance_byte_count)
						VALUES ($1,$2,$3)`, contractId, balanceId, 1*Mib))
				}
			}, server.TxReadCommitted)
			return contractId
		}
		settlementOrder := func(contractId server.Id) (order []server.Id, charged map[server.Id]ByteCount) {
			escrows := []contractSettlementEscrow{}
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx, settlementEscrowReadSql, contractId)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var escrow contractSettlementEscrow
						server.Raise(result.Scan(&escrow.balanceId, &escrow.amount, &escrow.start, &escrow.revenue))
						escrows = append(escrows, escrow)
					}
				})
			})
			payouts, err := contractSettlementSweepPayouts(1*Mib, escrows)
			server.Raise(err)
			charged = map[server.Id]ByteCount{}
			for _, escrow := range escrows {
				order = append(order, escrow.balanceId)
				charged[escrow.balanceId] = payouts[escrow.balanceId].payoutByteCount
			}
			return
		}
		deadlineOrder := func(contractId server.Id, payerNetworkId server.Id) (order []server.Id, charged map[server.Id]ByteCount) {
			escrows := []contractDeadlineEscrow{}
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx, deadlineEscrowRowsSql, contractId)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var escrow contractDeadlineEscrow
						server.Raise(result.Scan(&escrow.balanceId, &escrow.amount, &escrow.start, &escrow.revenue, &escrow.networkId,
							&escrow.available, &escrow.settled, &escrow.priorPayout, &escrow.redisReserved, &escrow.journal))
						escrows = append(escrows, escrow)
					}
				})
			})
			adjustments := []string{}
			payouts := deadlineEscrowPayouts(1*Mib, escrows, payerNetworkId, true, &adjustments)
			charged = map[server.Id]ByteCount{}
			for _, escrow := range escrows {
				order = append(order, escrow.balanceId)
				charged[escrow.balanceId] = payouts[escrow.balanceId].payoutByteCount
			}
			return
		}

		pro := newProDailyGrantTestClients(t, ctx)
		cycleGrantId := testingProDailyGrantPaidCycle(t, ctx, pro.payerNetworkId, now)
		proDailyGrantId := testingProDailyGrantFree(t, ctx, pro.payerNetworkId, now, 8*Mib)
		proContractId := contract(pro, proDailyGrantId, cycleGrantId)

		free := newProDailyGrantTestClients(t, ctx)
		freeDailyGrantId := testingProDailyGrantFree(t, ctx, free.payerNetworkId, now, 8*Mib)
		dataPackId := testingProDailyGrantDataPack(ctx, free.payerNetworkId, now, 16*Mib)
		freeContractId := contract(free, dataPackId, freeDailyGrantId)

		oldCode := newProDailyGrantTestClients(t, ctx)
		oldCodeDailyGrantId := testingProDailyGrantFree(t, ctx, oldCode.payerNetworkId, now, 8*Mib)
		oldCodeId := testingProDailyGrantOldProDataCode(ctx, oldCode.payerNetworkId, now, 16*Mib)
		oldCodeContractId := contract(oldCode, oldCodeId, oldCodeDailyGrantId)

		for _, test := range []struct {
			name       string
			contractId server.Id
			payer      server.Id
			first      server.Id
			second     server.Id
		}{
			{"a Pro network", proContractId, pro.payerNetworkId, cycleGrantId, proDailyGrantId},
			{"a free network", freeContractId, free.payerNetworkId, freeDailyGrantId, dataPackId},
			{"a network whose Pro comes from an old data code", oldCodeContractId, oldCode.payerNetworkId, oldCodeDailyGrantId, oldCodeId},
		} {
			for _, read := range []struct {
				name string
				read func() ([]server.Id, map[server.Id]ByteCount)
			}{
				{"settlement", func() ([]server.Id, map[server.Id]ByteCount) { return settlementOrder(test.contractId) }},
				{"deadline close", func() ([]server.Id, map[server.Id]ByteCount) { return deadlineOrder(test.contractId, test.payer) }},
			} {
				order, charged := read.read()
				if len(order) != 2 || order[0] != test.first || order[1] != test.second {
					t.Errorf("%s, %s: escrow order %v, want %s then %s", test.name, read.name, order, test.first, test.second)
				}
				if charged[test.first] != 1*Mib || charged[test.second] != 0 {
					t.Errorf("%s, %s: charged %v for 1 MiB used, want all of it to %s", test.name, read.name, charged, test.first)
				}
			}
		}
	})
}

// A free network is unchanged: its daily grant funds before a data pack it
// bought, on every admission path, as earliest expiry ordered it, and its
// zero-byte anchor and zero escrow priority stay unpaid.
func TestFreeNetworkKeepsSpendingItsDailyGrantFirst(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, legacy := range []bool{false, true} {
			for _, companion := range []bool{false, true} {
				name := proDailyGrantTestPathName(legacy, companion)
				clients := newProDailyGrantTestClients(t, ctx)
				if companion {
					if _, err := clients.create(ctx, 0, false); err != nil {
						t.Fatalf("%s: %v", name, err)
					}
				}
				now := server.NowUtc()
				dailyGrantId := testingProDailyGrantFree(t, ctx, clients.payerNetworkId, now, 8*Mib)
				dataPackId := testingProDailyGrantDataPack(ctx, clients.payerNetworkId, now, 16*Mib)
				if IsProNetworkFresh(ctx, clients.payerNetworkId) {
					t.Fatalf("%s: a data pack made the network Pro", name)
				}

				escrow, err := proDailyGrantTestCreate(ctx, clients, 1*Mib, legacy, companion)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				allocations := proDailyGrantTestAllocations(escrow)
				kinds := proDailyGrantTestKinds(t, ctx, allocations)
				if len(allocations) != 1 || kinds[GrantKindFree] != 1*Mib || allocations[dataPackId] != 0 || escrow.Priority != UnpaidPriority {
					t.Errorf("%s: drew daily %d (own daily grant %d) data pack %d at priority %d, want 1 MiB of the daily grant, unpaid",
						name, kinds[GrantKindFree], allocations[dailyGrantId], allocations[dataPackId], escrow.Priority)
				}
			}
		}

		clients := newProDailyGrantTestClients(t, ctx)
		now := server.NowUtc()
		testingProDailyGrantFree(t, ctx, clients.payerNetworkId, now, 8*Mib)
		testingProDailyGrantDataPack(ctx, clients.payerNetworkId, now, 16*Mib)
		escrow, err := clients.create(ctx, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(escrow.Balances) != 1 || testingProDailyGrantBalance(t, ctx, escrow.Balances[0].BalanceId).grantKind != GrantKindFree || escrow.Priority != UnpaidPriority {
			t.Errorf("zero-byte anchor %+v at priority %d, want the daily grant, unpaid", escrow.Balances, escrow.Priority)
		}
		var priority Priority
		server.Tx(ctx, func(tx server.PgTx) {
			priority = zeroEscrowContractPriorityInTx(ctx, tx, clients.payerNetworkId, 1*Mib, server.NowUtc())
		})
		if priority != UnpaidPriority {
			t.Errorf("zero escrow priority %d, want unpaid", priority)
		}
	})
}

// A network whose Pro comes from an old data code (Pro, paid, no grant kind:
// an AppSumo code redeemed in early 2026) already got the daily grant, and it
// keeps spending it first on every admission path, as it did: only a recurring
// Pro grant with revenue funds ahead of the daily grant. Its Pro makes the
// Redis census read two phases; the first is empty.
func TestNetworkWithAnOldProDataCodeKeepsSpendingItsDailyGrantFirst(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, legacy := range []bool{false, true} {
			for _, companion := range []bool{false, true} {
				name := proDailyGrantTestPathName(legacy, companion)
				clients := newProDailyGrantTestClients(t, ctx)
				if companion {
					if _, err := clients.create(ctx, 0, false); err != nil {
						t.Fatalf("%s: %v", name, err)
					}
				}
				now := server.NowUtc()
				testingProDailyGrantFree(t, ctx, clients.payerNetworkId, now, 8*Mib)
				oldCodeId := testingProDailyGrantOldProDataCode(ctx, clients.payerNetworkId, now, 16*Mib)
				if !IsProNetwork(ctx, clients.payerNetworkId) {
					t.Fatalf("%s: the old data code did not confer Pro", name)
				}

				escrow, err := proDailyGrantTestCreate(ctx, clients, 1*Mib, legacy, companion)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				allocations := proDailyGrantTestAllocations(escrow)
				kinds := proDailyGrantTestKinds(t, ctx, allocations)
				if len(allocations) != 1 || kinds[GrantKindFree] != 1*Mib || allocations[oldCodeId] != 0 || escrow.Priority != UnpaidPriority {
					t.Errorf("%s: drew daily %d old code %d at priority %d, want 1 MiB of the daily grant, unpaid",
						name, kinds[GrantKindFree], allocations[oldCodeId], escrow.Priority)
				}
			}
		}

		clients := newProDailyGrantTestClients(t, ctx)
		now := server.NowUtc()
		testingProDailyGrantFree(t, ctx, clients.payerNetworkId, now, 8*Mib)
		testingProDailyGrantOldProDataCode(ctx, clients.payerNetworkId, now, 16*Mib)
		escrow, err := clients.create(ctx, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(escrow.Balances) != 1 || testingProDailyGrantBalance(t, ctx, escrow.Balances[0].BalanceId).grantKind != GrantKindFree || escrow.Priority != UnpaidPriority {
			t.Errorf("zero-byte anchor %+v at priority %d, want the daily grant, unpaid", escrow.Balances, escrow.Priority)
		}
	})
}
