package model

// The owner's rule for operator block points (2026-11-13): "please make points
// proportional to traffic served and not revenue/paid traffic. After this
// change revenue should not have an affect on any payout." Each test computes
// a completed block's award (ComputeOperatorBlockPoints) from traffic that a
// revenue-weighted rule would rank differently, and fails on the rule before
// it, which weighed the payout points by a computed payout of each sweep's
// revenue share and a subsidy split equally over the paying networks.

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A paying network whose only balance is a free grant: no net revenue and no
// subscription revenue, so its traffic is unpaid (transfer_balance.paid).
func newOperatorBlockPointsFreePayer(t testing.TB, ctx context.Context) *operatorBlockPointsPayer {
	t.Helper()
	payer := &operatorBlockPointsPayer{network: server.NewId(), client: server.NewId()}
	testingCreatePaymentClient(ctx, payer.network, payer.client)
	now := server.NowUtc()
	server.Tx(ctx, func(tx server.PgTx) {
		server.Raise(AddGrantTransferBalanceInTx(tx, ctx, payer.network, GrantKindFree, 1024*1024*1024*1024, now.Add(-30*24*time.Hour), now.Add(30*24*time.Hour)))
	})
	balances := GetActiveTransferBalances(ctx, payer.network)
	if len(balances) != 1 {
		t.Fatal("missing fixture free grant")
	}
	payer.balance = balances[0].BalanceId
	return payer
}

// The balance is paid or not, as transfer_balance derives it from its revenue.
func operatorBlockPointsRequirePaid(t testing.TB, ctx context.Context, balanceId server.Id, paid bool) {
	t.Helper()
	var got bool
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT paid FROM transfer_balance WHERE balance_id = $1`, balanceId).Scan(&got))
	})
	if got != paid {
		t.Fatalf("fixture balance %s paid = %t, want %t", balanceId, got, paid)
	}
}

// Each network's payout points in an award.
func operatorBlockPointsPayoutByNetwork(award *OperatorBlockPoints) map[server.Id]NanoPoints {
	points := map[server.Id]NanoPoints{}
	for _, point := range award.Points {
		if point.Event == string(AccountPointEventPayout) {
			points[point.NetworkId] += point.PointValue
		}
	}
	return points
}

// The payout points are total shared by bytes: they sum to total, no network
// without bytes has any, and each network's share is within one nano point of
// total x its bytes / all the bytes.
func operatorBlockPointsRequireByteShares(t testing.TB, shares map[server.Id]NanoPoints, byteCounts map[server.Id]ByteCount, total NanoPoints) {
	t.Helper()
	totalByteCount := new(big.Int)
	for _, byteCount := range byteCounts {
		totalByteCount.Add(totalByteCount, big.NewInt(int64(byteCount)))
	}
	sum := NanoPoints(0)
	for networkId, share := range shares {
		if _, ok := byteCounts[networkId]; !ok {
			t.Fatalf("network %s served no bytes and took %d payout points", networkId, share)
		}
		sum += share
	}
	for networkId, byteCount := range byteCounts {
		share := shares[networkId]
		// |share x all bytes - total x bytes| < all bytes
		difference := new(big.Int).Sub(
			new(big.Int).Mul(big.NewInt(share), totalByteCount),
			new(big.Int).Mul(big.NewInt(total), big.NewInt(int64(byteCount))),
		)
		if 0 <= difference.CmpAbs(totalByteCount) {
			t.Fatalf("network %s took %d of %d payout points for %d of %s bytes: not its share of the bytes", networkId, share, total, byteCount, totalByteCount)
		}
	}
	if sum != total {
		t.Fatalf("payout points sum to %d, want the block's %d", sum, total)
	}
}

// The block's payout points as configured: account_points_per_payout scaled
// to the window past the boundary, the whole block unless the boundary is
// inside it.
func operatorBlockPointsConfiguredPayout(config *SubsidyConfig, block int64, cutoff time.Time) NanoPoints {
	start, end, _ := OperatorBlockPointsWindow(block, cutoff)
	scale := min(float64(end.Sub(start))/float64(config.Duration()), 1.0)
	return PointsToNanoPoints(float64(config.AccountPointsPerPayout) * scale)
}

// Equal bytes earn equal points, paid or not: A serves a paying network whose
// balance carries revenue, with the sweep's revenue share, and B serves a free
// network, whose free grant carries none. Their payout points differ by at
// most the nano point the exact split leaves over, and they share the block's
// payout points. Before, A's revenue share weighed its payout, and A took
// more.
func TestOperatorBlockPointsPaidAndUnpaidTrafficEarnAlike(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		block, now, cutoff := operatorBlockPointsTestBlock(server.NowUtc())
		useOperatorBlockPointsTransition(t, cutoff)
		config := operatorBlockPointsTestConfig()
		config.ReliabilityPointsPerPayout = 0
		gib := ByteCount(1024 * 1024 * 1024)

		paidPayer := newOperatorBlockPointsPayer(t, ctx)
		freePayer := newOperatorBlockPointsFreePayer(t, ctx)
		operatorBlockPointsRequirePaid(t, ctx, paidPayer.balance, true)
		operatorBlockPointsRequirePaid(t, ctx, freePayer.balance, false)
		providerA, clientA := operatorBlockPointsProvider(ctx)
		providerB, clientB := operatorBlockPointsProvider(ctx)
		inBlock := operatorBlockPointsTestTrafficTime(block, cutoff)
		paidPayer.contract(t, ctx, providerA, clientA, inBlock, inBlock, gib, UsdToNanoCents(2))
		freePayer.contract(t, ctx, providerB, clientB, inBlock.Add(time.Hour), inBlock.Add(time.Hour), gib, 0)

		award, err := ComputeOperatorBlockPoints(ctx, config, block, now)
		if err != nil {
			t.Fatal(err)
		}
		points := operatorBlockPointsPayoutByNetwork(award)
		paid, unpaid := points[providerA], points[providerB]
		if difference := paid - unpaid; difference < -1 || 1 < difference {
			t.Fatalf("for 1 GiB each, paid traffic earned %d payout points and unpaid traffic %d", paid, unpaid)
		}
		payout := operatorBlockPointsConfiguredPayout(config, block, cutoff)
		operatorBlockPointsRequireByteShares(t, points, map[server.Id]ByteCount{providerA: gib, providerB: gib}, payout)
	})
}

// Points scale with the bytes served, whatever the payer: A serves 3 GiB to
// one free network and B 1 GiB to another, and A takes three times B's payout
// points. Before, the subsidy weighed each paying network equally, whatever
// its bytes, so A and B, each its payer's only provider, took the same.
func TestOperatorBlockPointsScaleWithBytesServed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		block, now, cutoff := operatorBlockPointsTestBlock(server.NowUtc())
		useOperatorBlockPointsTransition(t, cutoff)
		config := operatorBlockPointsTestConfig()
		config.ReliabilityPointsPerPayout = 0
		gib := ByteCount(1024 * 1024 * 1024)

		payerU := newOperatorBlockPointsFreePayer(t, ctx)
		payerV := newOperatorBlockPointsFreePayer(t, ctx)
		providerA, clientA := operatorBlockPointsProvider(ctx)
		providerB, clientB := operatorBlockPointsProvider(ctx)
		inBlock := operatorBlockPointsTestTrafficTime(block, cutoff)
		payerU.contract(t, ctx, providerA, clientA, inBlock, inBlock, 2*gib, 0)
		payerU.contract(t, ctx, providerA, clientA, inBlock.Add(time.Hour), inBlock.Add(time.Hour), gib, 0)
		payerV.contract(t, ctx, providerB, clientB, inBlock.Add(2*time.Hour), inBlock.Add(2*time.Hour), gib, 0)

		award, err := ComputeOperatorBlockPoints(ctx, config, block, now)
		if err != nil {
			t.Fatal(err)
		}
		points := operatorBlockPointsPayoutByNetwork(award)
		a, b := points[providerA], points[providerB]
		if difference := a - 3*b; difference < -3 || 3 < difference {
			t.Fatalf("3 GiB earned %d payout points and 1 GiB %d, not three times as many", a, b)
		}
		payout := operatorBlockPointsConfiguredPayout(config, block, cutoff)
		operatorBlockPointsRequireByteShares(t, points, map[server.Id]ByteCount{providerA: 3 * gib, providerB: gib}, payout)
		byteCounts := map[server.Id]ByteCount{}
		for _, payment := range award.Payments {
			byteCounts[payment.NetworkId] = payment.PayoutByteCount
		}
		if len(byteCounts) != 2 || byteCounts[providerA] != 3*gib || byteCounts[providerB] != gib {
			t.Fatalf("the award's payments record %v bytes", byteCounts)
		}
	})
}

// The block's payout points stay account_points_per_payout scaled to the
// window, exactly, however the traffic and its revenue fall: three providers
// of a paid and a free network, with uneven bytes and uneven revenue, share
// exactly that many nano points, each within one nano point of its share of
// the bytes.
func TestOperatorBlockPointsTotalStaysOnTheConfiguredScale(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		block, now, cutoff := operatorBlockPointsTestBlock(server.NowUtc())
		useOperatorBlockPointsTransition(t, cutoff)
		config := operatorBlockPointsTestConfig()
		config.ReliabilityPointsPerPayout = 0
		mib := ByteCount(1024 * 1024)

		paidPayer := newOperatorBlockPointsPayer(t, ctx)
		freePayer := newOperatorBlockPointsFreePayer(t, ctx)
		providerA, clientA := operatorBlockPointsProvider(ctx)
		providerB, clientB := operatorBlockPointsProvider(ctx)
		providerC, clientC := operatorBlockPointsProvider(ctx)
		inBlock := operatorBlockPointsTestTrafficTime(block, cutoff)
		paidPayer.contract(t, ctx, providerA, clientA, inBlock, inBlock, 1000*mib, UsdToNanoCents(3))
		paidPayer.contract(t, ctx, providerB, clientB, inBlock.Add(time.Hour), inBlock.Add(time.Hour), 333*mib, UsdToNanoCents(0.01))
		freePayer.contract(t, ctx, providerC, clientC, inBlock.Add(2*time.Hour), inBlock.Add(2*time.Hour), 7*mib, 0)

		award, err := ComputeOperatorBlockPoints(ctx, config, block, now)
		if err != nil {
			t.Fatal(err)
		}
		payout := operatorBlockPointsConfiguredPayout(config, block, cutoff)
		if award.PayoutNanoPoints != payout {
			t.Fatalf("the block's payout points are %d, want the configured %d", award.PayoutNanoPoints, payout)
		}
		operatorBlockPointsRequireByteShares(t, operatorBlockPointsPayoutByNetwork(award), map[server.Id]ByteCount{providerA: 1000 * mib, providerB: 333 * mib, providerC: 7 * mib}, payout)
	})
}

// Revenue changes nothing: the same traffic's award is the same after its
// revenue changes, to another subscription price on the paid balance, a larger
// revenue share on every sweep, and the free network's balance turned paid.
// Before, each of these moved the points.
func TestOperatorBlockPointsIgnoreRevenue(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		block, now, cutoff := operatorBlockPointsTestBlock(server.NowUtc())
		useOperatorBlockPointsTransition(t, cutoff)
		config := operatorBlockPointsTestConfig()
		config.ReliabilityPointsPerPayout = 0
		gib := ByteCount(1024 * 1024 * 1024)

		paidPayer := newOperatorBlockPointsPayer(t, ctx)
		freePayer := newOperatorBlockPointsFreePayer(t, ctx)
		providerA, clientA := operatorBlockPointsProvider(ctx)
		providerB, clientB := operatorBlockPointsProvider(ctx)
		providerC, clientC := operatorBlockPointsProvider(ctx)
		inBlock := operatorBlockPointsTestTrafficTime(block, cutoff)
		contractIds := []server.Id{
			paidPayer.contract(t, ctx, providerA, clientA, inBlock, inBlock, 2*gib, UsdToNanoCents(1)),
			paidPayer.contract(t, ctx, providerC, clientC, inBlock.Add(time.Hour), inBlock.Add(time.Hour), gib, UsdToNanoCents(0.5)),
			freePayer.contract(t, ctx, providerB, clientB, inBlock.Add(2*time.Hour), inBlock.Add(2*time.Hour), gib, 0),
		}

		before, err := ComputeOperatorBlockPoints(ctx, config, block, now)
		if err != nil {
			t.Fatal(err)
		}

		server.Tx(ctx, func(tx server.PgTx) {
			// another subscription price
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents = $2, subsidy_net_revenue_nano_cents = $3 WHERE balance_id = $1`,
				paidPayer.balance, UsdToNanoCents(40), UsdToNanoCents(40)))
			// another paid share: the free network's balance turned paid
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents = $2, subsidy_net_revenue_nano_cents = $3 WHERE balance_id = $1`,
				freePayer.balance, UsdToNanoCents(5), UsdToNanoCents(5)))
			// and every sweep's revenue share
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow_sweep SET payout_net_revenue_nano_cents = 10 * payout_net_revenue_nano_cents + $2 WHERE contract_id = ANY($1::uuid[])`,
				contractIds, UsdToNanoCents(3)))
		})
		operatorBlockPointsRequirePaid(t, ctx, freePayer.balance, true)

		after, err := ComputeOperatorBlockPoints(ctx, config, block, now)
		if err != nil {
			t.Fatal(err)
		}
		pointsBefore, pointsAfter := operatorBlockPointsPayoutByNetwork(before), operatorBlockPointsPayoutByNetwork(after)
		if len(pointsAfter) != 3 {
			t.Fatalf("payout points %v", pointsAfter)
		}
		for networkId, points := range pointsBefore {
			if pointsAfter[networkId] != points {
				t.Fatalf("revenue moved network %s's payout points from %d to %d", networkId, points, pointsAfter[networkId])
			}
		}
		if before.Digest() != after.Digest() {
			t.Fatalf("revenue changed the award: %+v -> %+v", before, after)
		}
		payout := operatorBlockPointsConfiguredPayout(config, block, cutoff)
		operatorBlockPointsRequireByteShares(t, pointsAfter, map[server.Id]ByteCount{providerA: 2 * gib, providerB: gib, providerC: gib}, payout)
	})
}
