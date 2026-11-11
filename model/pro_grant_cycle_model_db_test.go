package model

// The per-cycle Pro grant against the database (server/PRO-GRANT-CYCLE.md). The
// grant and the sweep take `now` as an argument here, so each test walks a
// network through its cycles at chosen times; the rows' times are absolute.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// One cycle of a network as pro_grant_cycle and its grant hold it.
type proCycleGrantRow struct {
	cycleStartTime time.Time
	cycleEndTime   time.Time
	endedTime      *time.Time
	granted        bool
	startTime      time.Time
	endTime        time.Time
	byteCount      ByteCount
	subsidy        NanoCents
	pro            bool
	grantKind      GrantKind
}

func testingProCycleGrants(t testing.TB, ctx context.Context, networkId server.Id) []proCycleGrantRow {
	rows := []proCycleGrantRow{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					cycle.cycle_start_time,
					cycle.cycle_end_time,
					cycle.ended_time,
					cycle.balance_id IS NOT NULL,
					COALESCE(balance.start_time, cycle.cycle_start_time),
					COALESCE(balance.end_time, cycle.cycle_end_time),
					COALESCE(balance.start_balance_byte_count, 0),
					COALESCE(balance.subsidy_net_revenue_nano_cents, 0),
					COALESCE(balance.pro, false),
					COALESCE(balance.grant_kind, '')
				FROM pro_grant_cycle cycle
				LEFT JOIN transfer_balance balance ON balance.balance_id = cycle.balance_id
				WHERE cycle.network_id = $1
				ORDER BY cycle.cycle_start_time
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := proCycleGrantRow{}
				server.Raise(result.Scan(
					&row.cycleStartTime,
					&row.cycleEndTime,
					&row.endedTime,
					&row.granted,
					&row.startTime,
					&row.endTime,
					&row.byteCount,
					&row.subsidy,
					&row.pro,
					&row.grantKind,
				))
				rows = append(rows, row)
			}
		})
	})
	return rows
}

// The network's grants: cycles that carry a balance.
func testingProCycleGrantedRows(t testing.TB, ctx context.Context, networkId server.Id) []proCycleGrantRow {
	granted := []proCycleGrantRow{}
	for _, row := range testingProCycleGrants(t, ctx, networkId) {
		if row.granted {
			granted = append(granted, row)
		}
	}
	return granted
}

func testingProCycleRenewal(
	t testing.TB,
	ctx context.Context,
	networkId server.Id,
	market SubscriptionMarket,
	startTime time.Time,
	paidEndTime time.Time,
	grace time.Duration,
	netRevenue NanoCents,
) {
	paid := paidEndTime
	if err := AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
		NetworkId:          networkId,
		SubscriptionType:   SubscriptionTypeSupporter,
		StartTime:          startTime,
		EndTime:            paidEndTime.Add(grace),
		NetRevenue:         netRevenue,
		SubscriptionMarket: market,
		TransactionId:      server.NewId().String(),
		PaidEndTime:        &paid,
	}); err != nil {
		t.Fatal(err)
	}
}

// Runs the cycle grant for the network at `now`, as a credit path does, in its
// own ReadCommitted tx.
func testingGrantProCycle(ctx context.Context, networkId server.Id, now time.Time) (granted bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		granted = grantProCycleInTx(tx, ctx, networkId, now)
	}, server.TxReadCommitted)
	return
}

func testingSetProData(t testing.TB, byteCount ByteCount) {
	config := Pro()
	previous := *config
	t.Cleanup(func() { *config = previous })
	config.Pro.Data = byteCount
}

func testingProCycleClock() time.Time {
	// whole seconds, so times read back from the database compare equal
	return server.NowUtc().Truncate(time.Second)
}

func assertProCycleGrant(t testing.TB, name string, row proCycleGrantRow, start time.Time, end time.Time, byteCount ByteCount) {
	t.Helper()
	if !row.granted || !row.pro || row.grantKind != GrantKindPro {
		t.Fatalf("%s: granted %t pro %t kind %q, want a Pro grant", name, row.granted, row.pro, row.grantKind)
	}
	if !row.cycleStartTime.Equal(start) || !row.cycleEndTime.Equal(end) {
		t.Fatalf("%s: cycle [%s, %s), want [%s, %s)", name, row.cycleStartTime, row.cycleEndTime, start, end)
	}
	if !row.startTime.Equal(start) || !row.endTime.Equal(end.Add(ProGrantGrace)) {
		t.Fatalf("%s: grant [%s, %s), want the cycle plus a day [%s, %s)", name, row.startTime, row.endTime, start, end.Add(ProGrantGrace))
	}
	if row.byteCount != byteCount {
		t.Fatalf("%s: grant of %d bytes, want the full %d", name, row.byteCount, byteCount)
	}
}

// A yearly period gets its monthly cycles from the sweep, each once: the second
// run inside a cycle grants nothing, and the next month's run grants the next
// cycle at its own start.
func TestProCycleSweepGrantsTheMonthlyCyclesOfAYearOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingSetProData(t, 10*Tib)
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "pro-cycle-yearly", server.NewId())

		start := testingProCycleClock().Add(-2 * time.Hour)
		yearEnd := start.Add(365 * 24 * time.Hour)
		testingProCycleRenewal(t, ctx, networkId, SubscriptionMarketBitcoin, start, yearEnd, 30*24*time.Hour, UsdToNanoCents(36.5))

		granted, _ := grantDueProCycles(ctx, start.Add(time.Hour))
		if !containsId(granted, networkId) {
			t.Fatal("the sweep did not grant the year's first cycle")
		}
		granted, _ = grantDueProCycles(ctx, start.Add(2*time.Hour))
		if containsId(granted, networkId) {
			t.Fatal("the sweep granted the running cycle again")
		}
		secondStart := addProCycleMonths(start, 1)
		granted, _ = grantDueProCycles(ctx, secondStart.Add(time.Hour))
		if !containsId(granted, networkId) {
			t.Fatal("the sweep did not grant the year's second cycle")
		}
		rows := testingProCycleGrantedRows(t, ctx, networkId)
		if len(rows) != 2 {
			t.Fatalf("%d grants, want 2", len(rows))
		}
		assertProCycleGrant(t, "first month", rows[0], start, secondStart, 10*Tib)
		assertProCycleGrant(t, "second month", rows[1], secondStart, addProCycleMonths(start, 2), 10*Tib)
		// a month's share of the year's revenue
		if rows[0].subsidy <= 0 || UsdToNanoCents(4) < rows[0].subsidy {
			t.Fatalf("the month's grant carries %d of a year's 36.50 USD", rows[0].subsidy)
		}
	})
}

// A renewal recorded by a path that wrote no grant (an old binary in the
// rollout, with its 600 GiB activation balance and no paid end; or a renewal
// recorded while pro.yml was missing) is caught by the next sweep. The grant
// starts at the period's own start.
func TestProCycleSweepGrantsARenewalThatWasNotGranted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingSetProData(t, 10*Tib)
		now := testingProCycleClock()

		legacyNetworkId := server.NewId()
		Testing_CreateNetwork(ctx, legacyNetworkId, "pro-cycle-legacy-row", server.NewId())
		start := now.Add(-3 * 24 * time.Hour)
		periodEnd := addProCycleMonths(start, 1)
		endTime := periodEnd.Add(30 * 24 * time.Hour)
		if err := AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
			NetworkId:          legacyNetworkId,
			SubscriptionType:   SubscriptionTypeSupporter,
			StartTime:          start,
			EndTime:            endTime,
			NetRevenue:         UsdToNanoCents(3.5),
			SubscriptionMarket: SubscriptionMarketStripe,
			TransactionId:      "in_legacy_" + server.NewId().String(),
		}); err != nil {
			t.Fatal(err)
		}
		AddTransferBalance(ctx, &TransferBalance{
			NetworkId:             legacyNetworkId,
			StartTime:             start,
			EndTime:               endTime,
			StartBalanceByteCount: legacySupporterActivationByteCount,
			BalanceByteCount:      legacySupporterActivationByteCount,
			SubsidyNetRevenue:     UsdToNanoCents(3.5),
			Pro:                   true,
		})

		unGrantedNetworkId := server.NewId()
		Testing_CreateNetwork(ctx, unGrantedNetworkId, "pro-cycle-ungranted", server.NewId())
		testingProCycleRenewal(t, ctx, unGrantedNetworkId, SubscriptionMarketApple, start, periodEnd, 24*time.Hour, UsdToNanoCents(3.5))

		granted, _ := grantDueProCycles(ctx, now)
		for _, networkId := range []server.Id{legacyNetworkId, unGrantedNetworkId} {
			if !containsId(granted, networkId) {
				t.Fatalf("the sweep did not grant %s", networkId)
			}
			rows := testingProCycleGrantedRows(t, ctx, networkId)
			if len(rows) != 1 {
				t.Fatalf("%d grants, want 1", len(rows))
			}
			assertProCycleGrant(t, "caught by the sweep", rows[0], start, periodEnd, 10*Tib)
		}
		// the legacy activation balance is kept: nobody loses data
		kept := 0
		for _, balance := range GetActiveTransferBalances(ctx, legacyNetworkId) {
			if balance.StartBalanceByteCount == legacySupporterActivationByteCount {
				kept += 1
			}
		}
		if kept != 1 {
			t.Fatalf("the 600 GiB activation balance is gone: %d kept", kept)
		}
	})
}

// Two markets paying at once, each credit calling the grant, and then the sweep:
// one grant for the cycle.
func TestProCycleOverlappingMarketsGrantOncePerCycle(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingSetProData(t, 10*Tib)
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "pro-cycle-overlap", server.NewId())
		now := testingProCycleClock()

		stripeStart := now.Add(-5 * 24 * time.Hour)
		testingProCycleRenewal(t, ctx, networkId, SubscriptionMarketStripe, stripeStart, addProCycleMonths(stripeStart, 1), 30*24*time.Hour, UsdToNanoCents(3.5))
		if !testingGrantProCycle(ctx, networkId, now) {
			t.Fatal("the first market's credit did not grant")
		}
		appleStart := now.Add(-time.Hour)
		testingProCycleRenewal(t, ctx, networkId, SubscriptionMarketApple, appleStart, addProCycleMonths(appleStart, 1), 24*time.Hour, UsdToNanoCents(3.5))
		if testingGrantProCycle(ctx, networkId, now) {
			t.Fatal("the second market's credit granted inside the running cycle")
		}
		granted, _ := grantDueProCycles(ctx, now.Add(time.Minute))
		if containsId(granted, networkId) {
			t.Fatal("the sweep granted inside the running cycle")
		}
		rows := testingProCycleGrantedRows(t, ctx, networkId)
		if len(rows) != 1 {
			t.Fatalf("%d grants, want 1", len(rows))
		}
		assertProCycleGrant(t, "the first market's cycle", rows[0], stripeStart, addProCycleMonths(stripeStart, 1), 10*Tib)
		// the grant was written at the first market's credit, with its month's
		// revenue; the App Store period credited inside the cycle counts toward
		// the cycles after it
		if rows[0].subsidy != UsdToNanoCents(3.5) {
			t.Fatalf("the grant carries %d, want the first market's month %d", rows[0].subsidy, UsdToNanoCents(3.5))
		}
	})
}

// Cutover: a network holding the 1st-of-month grant gets nothing more inside
// that month; its first cycle grant comes when that month ends and runs to the
// end of its own cycle. The legacy grant is left whole. A network whose own
// cycle ends inside the legacy grant's one-day grace is covered by it: the cycle
// is recorded without a grant, and the next one is granted at its start.
func TestProCycleCutoverFromTheCalendarGrant(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingSetProData(t, 10*Tib)
		now := testingProCycleClock()
		legacyStart, legacyEnd := ProGrantWindow(now)
		legacyPeriodEnd := legacyEnd.Add(-ProGrantGrace)

		addLegacyGrant := func(networkId server.Id) server.Id {
			var balanceId server.Id
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(AddGrantTransferBalanceInTx(tx, ctx, networkId, GrantKindPro, 10*Tib, legacyStart, legacyEnd))
				server.Raise(tx.QueryRow(ctx, `SELECT balance_id FROM transfer_balance WHERE network_id = $1 AND grant_kind = $2`, networkId, GrantKindPro).Scan(&balanceId))
			})
			return balanceId
		}

		// renews on its own day, mid-month: its cycle straddles the legacy month's end
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "pro-cycle-cutover", server.NewId())
		legacyBalanceId := addLegacyGrant(networkId)
		periodStart := legacyStart.Add(14*24*time.Hour + 9*time.Hour)
		periodEnd := addProCycleMonths(periodStart, 1)
		testingProCycleRenewal(t, ctx, networkId, SubscriptionMarketStripe, periodStart, periodEnd, 30*24*time.Hour, UsdToNanoCents(3.5))

		if testingGrantProCycle(ctx, networkId, periodStart.Add(time.Hour)) {
			t.Fatal("a second full grant inside the legacy month")
		}
		granted, _ := grantDueProCycles(ctx, legacyPeriodEnd.Add(-time.Hour))
		if containsId(granted, networkId) {
			t.Fatal("the sweep granted before the legacy month ended")
		}
		granted, _ = grantDueProCycles(ctx, legacyPeriodEnd.Add(30*time.Minute))
		if !containsId(granted, networkId) {
			t.Fatal("no grant when the legacy month ended")
		}
		rows := testingProCycleGrantedRows(t, ctx, networkId)
		if len(rows) != 1 {
			t.Fatalf("%d grants, want 1", len(rows))
		}
		assertProCycleGrant(t, "the bridge to its own cycle", rows[0], legacyPeriodEnd, periodEnd, 10*Tib)

		// the next renewal: a grant at its own start
		testingProCycleRenewal(t, ctx, networkId, SubscriptionMarketStripe, periodEnd, addProCycleMonths(periodStart, 2), 30*24*time.Hour, UsdToNanoCents(3.5))
		if !testingGrantProCycle(ctx, networkId, periodEnd.Add(time.Minute)) {
			t.Fatal("the renewal after the cutover did not grant")
		}
		rows = testingProCycleGrantedRows(t, ctx, networkId)
		assertProCycleGrant(t, "its own renewal", rows[1], periodEnd, addProCycleMonths(periodStart, 2), 10*Tib)

		// the legacy grant is whole: same window, same bytes
		var startTime, endTime time.Time
		var byteCount ByteCount
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx,
				`SELECT start_time, end_time, start_balance_byte_count FROM transfer_balance WHERE balance_id = $1`,
				legacyBalanceId,
			).Scan(&startTime, &endTime, &byteCount))
		})
		if !startTime.Equal(legacyStart) || !endTime.Equal(legacyEnd) || byteCount != 10*Tib {
			t.Fatalf("the legacy grant changed: [%s, %s) %d bytes", startTime, endTime, byteCount)
		}

		// renews a few hours after the legacy month ends: the legacy grant's grace
		// covers that cycle's last hours, so it carries no grant
		earlyNetworkId := server.NewId()
		Testing_CreateNetwork(ctx, earlyNetworkId, "pro-cycle-cutover-early", server.NewId())
		addLegacyGrant(earlyNetworkId)
		earlyStart := legacyStart.Add(6 * time.Hour)
		earlyEnd := legacyPeriodEnd.Add(6 * time.Hour)
		testingProCycleRenewal(t, ctx, earlyNetworkId, SubscriptionMarketApple, earlyStart, earlyEnd, 24*time.Hour, UsdToNanoCents(3.5))
		granted, _ = grantDueProCycles(ctx, legacyPeriodEnd.Add(time.Hour))
		if containsId(granted, earlyNetworkId) {
			t.Fatal("a full grant for the hours the legacy grant's grace covers")
		}
		cycles := testingProCycleGrants(t, ctx, earlyNetworkId)
		if len(cycles) != 1 || cycles[0].granted || !cycles[0].cycleEndTime.Equal(earlyEnd) {
			t.Fatalf("cycles %+v, want one covered cycle to %s", cycles, earlyEnd)
		}
		testingProCycleRenewal(t, ctx, earlyNetworkId, SubscriptionMarketApple, earlyEnd, addProCycleMonths(earlyStart, 2), 24*time.Hour, UsdToNanoCents(3.5))
		if !testingGrantProCycle(ctx, earlyNetworkId, earlyEnd.Add(time.Minute)) {
			t.Fatal("the early network's renewal did not grant")
		}
		rows = testingProCycleGrantedRows(t, ctx, earlyNetworkId)
		assertProCycleGrant(t, "the early network's own cycle", rows[0], earlyEnd, addProCycleMonths(earlyStart, 2), 10*Tib)
	})
}

// A cancelled subscription keeps the cycles it paid for: a yearly plan's monthly
// cycles run to the paid term's end, and nothing is granted after it. A monthly
// plan that does not renew gets no next cycle.
func TestProCycleCancellationStopsFutureGrants(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingSetProData(t, 10*Tib)
		now := testingProCycleClock()

		monthlyNetworkId := server.NewId()
		Testing_CreateNetwork(ctx, monthlyNetworkId, "pro-cycle-cancel-monthly", server.NewId())
		start := now.Add(-time.Hour)
		periodEnd := addProCycleMonths(start, 1)
		testingProCycleRenewal(t, ctx, monthlyNetworkId, SubscriptionMarketStripe, start, periodEnd, 30*24*time.Hour, UsdToNanoCents(3.5))
		if !testingGrantProCycle(ctx, monthlyNetworkId, now) {
			t.Fatal("no grant for the paid month")
		}
		// cancelled: no renewal. In the manual-payment grace the network is still
		// a renewal holder, but no cycle is paid for
		for _, at := range []time.Time{periodEnd.Add(time.Hour), periodEnd.Add(10 * 24 * time.Hour), periodEnd.Add(31 * 24 * time.Hour)} {
			if granted, _ := grantDueProCycles(ctx, at); containsId(granted, monthlyNetworkId) {
				t.Fatalf("granted at %s after the cancelled month", at)
			}
		}
		if rows := testingProCycleGrantedRows(t, ctx, monthlyNetworkId); len(rows) != 1 {
			t.Fatalf("%d grants, want the paid month's only", len(rows))
		}

		yearlyNetworkId := server.NewId()
		Testing_CreateNetwork(ctx, yearlyNetworkId, "pro-cycle-cancel-yearly", server.NewId())
		yearStart := now.Add(-300 * 24 * time.Hour)
		yearEnd := yearStart.Add(365 * 24 * time.Hour)
		testingProCycleRenewal(t, ctx, yearlyNetworkId, SubscriptionMarketApple, yearStart, yearEnd, 24*time.Hour, UsdToNanoCents(28))
		lastStart := addProCycleMonths(yearStart, 11)
		if granted, _ := grantDueProCycles(ctx, lastStart.Add(time.Hour)); !containsId(granted, yearlyNetworkId) {
			t.Fatal("no grant for the paid year's last month")
		}
		if granted, _ := grantDueProCycles(ctx, yearEnd.Add(time.Hour)); containsId(granted, yearlyNetworkId) {
			t.Fatal("granted after the paid year")
		}
		rows := testingProCycleGrantedRows(t, ctx, yearlyNetworkId)
		if len(rows) != 1 {
			t.Fatalf("%d grants, want the last paid month's", len(rows))
		}
		assertProCycleGrant(t, "the year's last month", rows[0], lastStart, yearEnd, 10*Tib)
	})
}

// A billing grace hold (no paid end, a zero-byte Pro marker) keeps Pro but starts
// no cycle; the recovered charge starts the next one.
func TestProCycleGraceHoldStartsNoCycle(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingSetProData(t, 10*Tib)
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "pro-cycle-grace", server.NewId())
		now := testingProCycleClock()
		start := now.Add(-40 * 24 * time.Hour)
		expires := addProCycleMonths(start, 1)
		testingProCycleRenewal(t, ctx, networkId, SubscriptionMarketApple, start, expires, 24*time.Hour, UsdToNanoCents(3.5))
		if !testingGrantProCycle(ctx, networkId, start.Add(time.Minute)) {
			t.Fatal("no grant for the paid month")
		}

		// the charge failed: the App Store holds the customer entitled
		graceExpires := expires.Add(16 * 24 * time.Hour)
		if err := AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
			NetworkId:          networkId,
			SubscriptionType:   SubscriptionTypeSupporter,
			StartTime:          start,
			EndTime:            graceExpires.Add(24 * time.Hour),
			SubscriptionMarket: SubscriptionMarketApple,
			TransactionId:      "grace-hold",
		}); err != nil {
			t.Fatal(err)
		}
		AddTransferBalance(ctx, &TransferBalance{
			NetworkId: networkId,
			StartTime: start,
			EndTime:   graceExpires.Add(24 * time.Hour),
			Pro:       true,
		})
		if granted, _ := grantDueProCycles(ctx, now); containsId(granted, networkId) {
			t.Fatal("the grace hold started a cycle")
		}

		// the charge recovers: the next period starts the next cycle
		testingProCycleRenewal(t, ctx, networkId, SubscriptionMarketApple, now.Add(-time.Hour), addProCycleMonths(now.Add(-time.Hour), 1), 24*time.Hour, UsdToNanoCents(3.5))
		if !testingGrantProCycle(ctx, networkId, now) {
			t.Fatal("the recovered charge did not grant")
		}
		if rows := testingProCycleGrantedRows(t, ctx, networkId); len(rows) != 2 {
			t.Fatalf("%d grants, want the first month's and the recovered one's", len(rows))
		}
	})
}

// A refund ends the paid period early, and the end path ends the running cycle's
// grant with it, at once, as it ended the 600 GiB credit; a later subscription
// starts a new chain with its own grant. A renewal ended any other way is caught
// by the sweep.
func TestProCycleRefundEndsTheRunningGrant(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingSetProData(t, 10*Tib)
		now := testingProCycleClock()

		addPaidMonth := func(networkId server.Id, transactionId string, start time.Time) {
			paidEnd := addProCycleMonths(start, 1)
			if err := AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   SubscriptionTypeSupporter,
				StartTime:          start,
				EndTime:            paidEnd.Add(30 * 24 * time.Hour),
				NetRevenue:         UsdToNanoCents(3.5),
				SubscriptionMarket: SubscriptionMarketStripe,
				TransactionId:      transactionId,
				PaidEndTime:        &paidEnd,
			}); err != nil {
				t.Fatal(err)
			}
			if !testingGrantProCycle(ctx, networkId, now.Add(-time.Hour)) {
				t.Fatal("no grant for the paid month")
			}
		}

		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "pro-cycle-refund", server.NewId())
		invoiceId := "in_refund_" + server.NewId().String()
		addPaidMonth(networkId, invoiceId, now.Add(-2*24*time.Hour))

		// the refund ends the renewal, its balance and the cycle's grant now
		refundedAt := server.NowUtc()
		if ended, err := EndReconciledEntitlementForNetworkTransactions(ctx, networkId, SubscriptionMarketStripe, []string{invoiceId}, refundedAt); err != nil || !ended {
			t.Fatalf("the refund ended nothing: %t %v", ended, err)
		}
		rows := testingProCycleGrantedRows(t, ctx, networkId)
		if len(rows) != 1 || !rows[0].endTime.Equal(refundedAt) || rows[0].endedTime == nil || !rows[0].endedTime.Equal(refundedAt) {
			t.Fatalf("the refunded cycle's grant still runs: %+v", rows)
		}
		if IsProNetworkFresh(ctx, networkId) {
			t.Fatal("still Pro after the refund")
		}
		// nothing left for the sweep: no second clawback, no new grant
		granted, ended := grantDueProCycles(ctx, refundedAt.Add(time.Minute))
		if containsId(ended, networkId) || containsId(granted, networkId) {
			t.Fatalf("the sweep acted on a refunded network: granted %t ended %t", containsId(granted, networkId), containsId(ended, networkId))
		}

		// resubscribed: a new chain, granted at its own start
		resubscribed := refundedAt.Add(time.Hour).Truncate(time.Second)
		testingProCycleRenewal(t, ctx, networkId, SubscriptionMarketStripe, resubscribed, addProCycleMonths(resubscribed, 1), 30*24*time.Hour, UsdToNanoCents(3.5))
		if !testingGrantProCycle(ctx, networkId, resubscribed.Add(time.Minute)) {
			t.Fatal("the resubscription did not grant")
		}
		rows = testingProCycleGrantedRows(t, ctx, networkId)
		assertProCycleGrant(t, "resubscribed", rows[1], resubscribed, addProCycleMonths(resubscribed, 1), 10*Tib)

		// a renewal ended outside the end paths: the sweep ends the grant
		otherNetworkId := server.NewId()
		Testing_CreateNetwork(ctx, otherNetworkId, "pro-cycle-ended-elsewhere", server.NewId())
		otherInvoiceId := "in_ended_" + server.NewId().String()
		addPaidMonth(otherNetworkId, otherInvoiceId, now.Add(-3*24*time.Hour))
		endedAt := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx,
				`UPDATE subscription_renewal SET end_time = $2 WHERE network_id = $1 AND transaction_id = $3`,
				otherNetworkId, endedAt, otherInvoiceId))
		})
		sweepAt := endedAt.Add(time.Minute)
		if _, ended = grantDueProCycles(ctx, sweepAt); !containsId(ended, otherNetworkId) {
			t.Fatal("the sweep left a grant running with no renewal open")
		}
		rows = testingProCycleGrantedRows(t, ctx, otherNetworkId)
		if len(rows) != 1 || !rows[0].endTime.Equal(sweepAt) || rows[0].endedTime == nil {
			t.Fatalf("the sweep's clawback: %+v", rows)
		}
		if _, ended = grantDueProCycles(ctx, sweepAt.Add(time.Minute)); containsId(ended, otherNetworkId) {
			t.Fatal("the clawback ran twice")
		}
	})
}

// The grant reads the chain after taking the network's lock, so it refuses a
// RepeatableRead tx, whose snapshot may predate an earlier holder's commit, and
// leaves the cycle to the sweep. With no pro.yml amount it grants nothing.
func TestProCycleGrantNeedsReadCommittedAndAnAmount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingSetProData(t, 10*Tib)
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "pro-cycle-isolation", server.NewId())
		now := testingProCycleClock()
		testingProCycleRenewal(t, ctx, networkId, SubscriptionMarketStripe, now.Add(-time.Hour), addProCycleMonths(now.Add(-time.Hour), 1), 30*24*time.Hour, UsdToNanoCents(3.5))

		granted := false
		server.Tx(ctx, func(tx server.PgTx) {
			granted = grantProCycleInTx(tx, ctx, networkId, now)
		})
		if granted || 0 < len(testingProCycleGrants(t, ctx, networkId)) {
			t.Fatal("granted in a RepeatableRead tx")
		}

		testingSetProData(t, 0)
		if testingGrantProCycle(ctx, networkId, now) || 0 < len(testingProCycleGrants(t, ctx, networkId)) {
			t.Fatal("granted with no pro.yml amount")
		}

		testingSetProData(t, 10*Tib)
		if !testingGrantProCycle(ctx, networkId, now) {
			t.Fatal("no grant at ReadCommitted")
		}
	})
}

func containsId(ids []server.Id, id server.Id) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}
