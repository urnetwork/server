package model

// The daily free grant for Pro networks and networks in a payment grace. The
// owner, 2026-11-11: "3. yes extend daily data in grace" and "5. i think pro
// should get the 1tib per month plus the 30gb per day", the monthly amount
// later confirmed as pro.yml's 10 TiB. The funding order that keeps Pro traffic
// paid, and the helpers here: transfer_balance_funding_order_db_test.go. Needs
// the test database.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A Stripe month that ended about five days ago and did not renew: the network
// is in the 30-day manual-payment grace. Its renewal and the period's marker
// keep it Pro, its cycle grant expired a day after the cycle, and no paid period
// starts a new cycle.
func testingProDailyGrantStripeGrace(t testing.TB, ctx context.Context, networkId server.Id, now time.Time) {
	t.Helper()
	start := now.Add(-35 * 24 * time.Hour)
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
	if !testingGrantProCycle(ctx, networkId, start.Add(time.Minute)) {
		t.Fatal("no cycle grant for the paid month")
	}
}

// An App Store month that ended about ten days ago whose renewal charge failed:
// the App Store holds the customer entitled (a billing-grace hold, no paid end,
// a zero-byte Pro marker with no revenue). It keeps Pro, but its cycle grant
// expired a day after the paid month, and the hold starts no cycle.
func testingProDailyGrantAppleHold(t testing.TB, ctx context.Context, networkId server.Id, now time.Time) {
	t.Helper()
	start := now.Add(-40 * 24 * time.Hour)
	expires := addProCycleMonths(start, 1)
	revenue := UsdToNanoCents(3.5)
	testingProCycleRenewal(t, ctx, networkId, SubscriptionMarketApple, start, expires, 24*time.Hour, revenue)
	AddTransferBalance(ctx, &TransferBalance{
		NetworkId:         networkId,
		StartTime:         start,
		EndTime:           expires.Add(24 * time.Hour),
		SubsidyNetRevenue: revenue,
		Pro:               true,
	})
	if !testingGrantProCycle(ctx, networkId, start.Add(time.Minute)) {
		t.Fatal("no cycle grant for the paid month")
	}
	holdEnd := expires.Add(17 * 24 * time.Hour)
	if err := AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
		NetworkId:          networkId,
		SubscriptionType:   SubscriptionTypeSupporter,
		StartTime:          start,
		EndTime:            holdEnd,
		SubscriptionMarket: SubscriptionMarketApple,
		TransactionId:      "grace-hold-" + networkId.String(),
	}); err != nil {
		t.Fatal(err)
	}
	AddTransferBalance(ctx, &TransferBalance{
		NetworkId: networkId,
		StartTime: start,
		EndTime:   holdEnd,
		Pro:       true,
	})
}

// The daily run grants a Pro network and a network in either payment grace the
// day's free data, beside a free network, which it grants as before. Each grace
// network keeps Pro after its cycle grant has expired, so without the daily
// grant it has no data; with it, its traffic is funded, unpaid. A Pro network's
// traffic still comes from its paid cycle grant.
func TestDailyFreeGrantReachesProAndPaymentGraceNetworks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingSetProData(t, 16*Mib)
		now := testingProCycleClock()

		free := newProDailyGrantTestClients(t, ctx)
		pro := newProDailyGrantTestClients(t, ctx)
		cycleGrantId := testingProDailyGrantPaidCycle(t, ctx, pro.payerNetworkId, now)
		stripeGrace := newProDailyGrantTestClients(t, ctx)
		testingProDailyGrantStripeGrace(t, ctx, stripeGrace.payerNetworkId, now)
		appleHold := newProDailyGrantTestClients(t, ctx)
		testingProDailyGrantAppleHold(t, ctx, appleHold.payerNetworkId, now)

		graceNetworks := []struct {
			name    string
			clients escrowSelectionTestClients
		}{
			{"a network in the Stripe manual-payment grace", stripeGrace},
			{"a network in an App Store billing-grace hold", appleHold},
		}
		for _, grace := range graceNetworks {
			networkId := grace.clients.payerNetworkId
			if active, _ := HasSubscriptionRenewal(ctx, networkId, SubscriptionTypeSupporter); !active {
				t.Fatalf("%s holds no supporter renewal", grace.name)
			}
			if !IsProNetworkFresh(ctx, networkId) {
				t.Fatalf("%s lost Pro", grace.name)
			}
			cycleGrant := testingProDailyGrantBalance(t, ctx, testingProDailyGrantCycleBalance(t, ctx, networkId))
			if cycleGrant.endTime.After(now) {
				t.Fatalf("%s: the cycle grant runs to %s, want it expired", grace.name, cycleGrant.endTime)
			}
			if len(GetActiveTransferBalances(ctx, networkId)) != 0 {
				t.Fatalf("%s holds data before the daily run", grace.name)
			}
		}

		startTime, endTime := FreeGrantWindow(now)
		added := AddFreeTransferBalanceToAllNetworks(ctx, startTime, endTime, 1*Gib)
		for _, granted := range append([]struct {
			name    string
			clients escrowSelectionTestClients
		}{
			{"a free network", free},
			{"a Pro network", pro},
		}, graceNetworks...) {
			networkId := granted.clients.payerNetworkId
			if added[networkId] != 1*Gib {
				t.Errorf("%s: daily free grant %d, want %d", granted.name, added[networkId], 1*Gib)
			}
			grants := 0
			for _, balance := range GetActiveTransferBalances(ctx, networkId) {
				if balance.GrantKind != GrantKindFree {
					continue
				}
				grants += 1
				if balance.Pro || balance.Paid || balance.NetRevenue != 0 || balance.SubsidyNetRevenue != 0 {
					t.Errorf("%s: the daily grant is pro %t paid %t, want unpaid data that never confers Pro", granted.name, balance.Pro, balance.Paid)
				}
				if !balance.StartTime.Equal(startTime) || !balance.EndTime.Equal(endTime) || balance.StartBalanceByteCount != 1*Gib {
					t.Errorf("%s: the daily grant is [%s, %s) %d bytes, want the day's window and amount", granted.name, balance.StartTime, balance.EndTime, balance.StartBalanceByteCount)
				}
			}
			if grants != 1 {
				t.Errorf("%s: %d active daily free grants, want 1", granted.name, grants)
			}
		}

		// a grace network's traffic is funded by the daily grant, unpaid
		for _, grace := range graceNetworks {
			escrow, err := grace.clients.create(ctx, 1*Mib, false)
			if err != nil {
				t.Fatalf("%s: no contract from the daily grant: %v", grace.name, err)
			}
			for balanceId := range proDailyGrantTestAllocations(escrow) {
				if balance := testingProDailyGrantBalance(t, ctx, balanceId); balance.grantKind != GrantKindFree {
					t.Errorf("%s: the contract drew a %q balance, want the daily grant", grace.name, balance.grantKind)
				}
			}
			if escrow.TransferByteCount != 1*Mib || escrow.Priority != UnpaidPriority {
				t.Errorf("%s: contract of %d bytes at priority %d, want 1 MiB at unpaid priority", grace.name, escrow.TransferByteCount, escrow.Priority)
			}
		}

		// a Pro network's traffic still comes from its paid cycle grant
		escrow, err := pro.create(ctx, 1*Mib, false)
		if err != nil {
			t.Fatal(err)
		}
		if allocations := proDailyGrantTestAllocations(escrow); len(allocations) != 1 || allocations[cycleGrantId] != 1*Mib || escrow.Priority != PaidPriority {
			t.Errorf("a Pro network's contract drew %v at priority %d, want 1 MiB of its cycle grant at paid priority", allocations, escrow.Priority)
		}
	})
}
