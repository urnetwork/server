package model

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// midnight UTC, the free grant boundary. 2026-10-01 is also a Pro boundary.
var testGrantBoundary = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func testingGrant(grantKind GrantKind, startTime time.Time, endTime time.Time) *TransferBalance {
	return &TransferBalance{
		BalanceId:             server.NewId(),
		StartTime:             startTime,
		EndTime:               endTime,
		StartBalanceByteCount: 30 * Gib,
		BalanceByteCount:      30 * Gib,
		Pro:                   grantKind == GrantKindPro,
		GrantKind:             grantKind,
	}
}

// testingLegacyGrant is a grant written before grant kinds were recorded
func testingLegacyGrant(pro bool, startTime time.Time, endTime time.Time) *TransferBalance {
	transferBalance := testingGrant(GrantKindNone, startTime, endTime)
	transferBalance.Pro = pro
	if pro {
		// the monthly Pro grant carries subsidy revenue, so it reads as paid
		transferBalance.Paid = true
	}
	return transferBalance
}

func testingFreeGrant(grantKind GrantKind, now time.Time) *TransferBalance {
	startTime, endTime := FreeGrantWindow(now)
	return testingGrant(grantKind, startTime, endTime)
}

func testingProGrant(grantKind GrantKind, now time.Time) *TransferBalance {
	startTime, endTime := ProGrantWindow(now)
	transferBalance := testingGrant(grantKind, startTime, endTime)
	transferBalance.Pro = true
	transferBalance.Paid = true
	return transferBalance
}

func testingReferralGrant(grantKind GrantKind, now time.Time) *TransferBalance {
	startTime, endTime := ReferralGrantWindow(now)
	return testingGrant(grantKind, startTime, endTime)
}

func assertSuperseded(t *testing.T, transferBalances []*TransferBalance, expected ...*TransferBalance) {
	t.Helper()
	expectedIds := map[server.Id]bool{}
	for _, transferBalance := range expected {
		expectedIds[transferBalance.BalanceId] = true
	}
	connect.AssertEqual(t, SupersededGrants(transferBalances), expectedIds)
}

func TestSupersededGrants(t *testing.T) {
	overlap := testGrantBoundary.Add(30 * time.Minute)

	t.Run("overlap hour", func(t *testing.T) {
		yesterday := testingFreeGrant(GrantKindFree, testGrantBoundary.Add(-time.Minute))
		today := testingFreeGrant(GrantKindFree, overlap)
		// yesterday's grant is still active through 01:00
		connect.AssertEqual(t, overlap.Before(yesterday.EndTime), true)
		assertSuperseded(t, []*TransferBalance{yesterday, today}, yesterday)
		assertSuperseded(t, []*TransferBalance{today, yesterday}, yesterday)
	})

	t.Run("outside the hour", func(t *testing.T) {
		now := testGrantBoundary.Add(5 * time.Hour)
		today := testingFreeGrant(GrantKindFree, now)
		referral := testingReferralGrant(GrantKindReferral, now.Add(-3*time.Hour))
		dataCode := testingGrant(GrantKindNone, now.Add(-5*24*time.Hour), now.Add(360*24*time.Hour))
		dataCode.Paid = true
		assertSuperseded(t, []*TransferBalance{today, referral, dataCode})

		// a second grant of a kind that started before the first one's grace (a
		// repeated run) is not the next period's grant: both count
		repeated := testingReferralGrant(GrantKindReferral, now.Add(-3*time.Hour+10*time.Minute))
		assertSuperseded(t, []*TransferBalance{today, referral, repeated})
	})

	t.Run("pro month boundary", func(t *testing.T) {
		lastMonth := testingProGrant(GrantKindPro, testGrantBoundary.Add(-time.Minute))
		thisMonth := testingProGrant(GrantKindPro, testGrantBoundary.Add(12*time.Hour))
		// last month's grant is still active through the 2nd
		connect.AssertEqual(t, testGrantBoundary.Add(12*time.Hour).Before(lastMonth.EndTime), true)
		// a subscription balance carries pro but spans its own billing period
		subscription := testingLegacyGrant(true, time.Date(2026, 9, 14, 8, 21, 7, 0, time.UTC), time.Date(2026, 10, 15, 8, 21, 7, 0, time.UTC))
		referral := testingReferralGrant(GrantKindReferral, testGrantBoundary.Add(3*time.Hour))
		assertSuperseded(t, []*TransferBalance{lastMonth, thisMonth, subscription, referral}, lastMonth)
	})

	t.Run("referral", func(t *testing.T) {
		runTime := time.Date(2026, 9, 30, 7, 13, 22, 123456000, time.UTC)
		previousRun := testingReferralGrant(GrantKindReferral, runTime)
		// the next run lands a little after the period: the referrer and referee
		// bonuses of one run share its window, and both count
		nextRunTime := runTime.Add(Pro().ReferralGrantPeriod()).Add(3 * time.Minute)
		referrerBonus := testingReferralGrant(GrantKindReferral, nextRunTime)
		refereeBonus := testingReferralGrant(GrantKindReferral, nextRunTime)
		connect.AssertEqual(t, nextRunTime.Before(previousRun.EndTime), true)
		assertSuperseded(t, []*TransferBalance{previousRun, referrerBonus, refereeBonus}, previousRun)

		// a free grant does not supersede a referral grant
		today := testingFreeGrant(GrantKindFree, nextRunTime)
		assertSuperseded(t, []*TransferBalance{previousRun, today})
	})

	t.Run("legacy grants", func(t *testing.T) {
		// a recorded grant supersedes the previous period's unrecorded grant
		yesterday := testingLegacyGrant(false, testGrantBoundary.Add(-24*time.Hour), testGrantBoundary.Add(FreeGrantGrace))
		today := testingFreeGrant(GrantKindFree, overlap)
		assertSuperseded(t, []*TransferBalance{yesterday, today}, yesterday)

		lastMonthStart, lastMonthEnd := ProGrantWindow(testGrantBoundary.Add(-time.Minute))
		lastMonth := testingLegacyGrant(true, lastMonthStart, lastMonthEnd)
		thisMonth := testingProGrant(GrantKindPro, overlap)
		assertSuperseded(t, []*TransferBalance{lastMonth, thisMonth}, lastMonth)

		runTime := time.Date(2026, 9, 30, 7, 13, 22, 0, time.UTC)
		previousRunStart, previousRunEnd := ReferralGrantWindow(runTime)
		previousRun := testingLegacyGrant(false, previousRunStart, previousRunEnd)
		nextRun := testingReferralGrant(GrantKindReferral, runTime.Add(Pro().ReferralGrantPeriod()))
		assertSuperseded(t, []*TransferBalance{previousRun, nextRun}, previousRun)

		// two unrecorded grants: only a recorded grant supersedes
		legacyToday := testingLegacyGrant(false, today.StartTime, today.EndTime)
		assertSuperseded(t, []*TransferBalance{yesterday, legacyToday})

		// a newer unrecorded grant (a rollback) does not supersede a recorded one
		recordedYesterday := testingFreeGrant(GrantKindFree, testGrantBoundary.Add(-time.Minute))
		assertSuperseded(t, []*TransferBalance{recordedYesterday, legacyToday})

		// balances that only resemble a grant are never grants
		paidYesterday := testingLegacyGrant(false, yesterday.StartTime, yesterday.EndTime)
		paidYesterday.Paid = true
		proYesterday := testingLegacyGrant(true, yesterday.StartTime, yesterday.EndTime)
		earnedLastMonth := testingLegacyGrant(true, lastMonthStart, lastMonthEnd)
		earnedLastMonth.NetRevenue = UsdToNanoCents(5)
		x402Month := testingLegacyGrant(true, testGrantBoundary.Add(-20*24*time.Hour), testGrantBoundary.Add(40*24*time.Hour))
		proberCredit := testingLegacyGrant(false, testGrantBoundary.Add(-2*time.Hour), testGrantBoundary.Add(time.Hour))
		assertSuperseded(
			t,
			[]*TransferBalance{paidYesterday, proYesterday, earnedLastMonth, x402Month, proberCredit, today, thisMonth, nextRun},
		)
	})

	t.Run("unknown kind", func(t *testing.T) {
		// a kind written by a newer binary is left alone
		yesterday := testingFreeGrant("trial", testGrantBoundary.Add(-time.Minute))
		today := testingFreeGrant("trial", overlap)
		assertSuperseded(t, []*TransferBalance{yesterday, today})
	})
}

func TestSummarizeTransferBalances(t *testing.T) {
	overlap := testGrantBoundary.Add(30 * time.Minute)

	// yesterday's grant: 20 GiB used, 1 GiB reserved by open contracts
	yesterday := testingFreeGrant(GrantKindFree, testGrantBoundary.Add(-time.Minute))
	yesterday.BalanceByteCount = 9 * Gib
	yesterday.reservedByteCount = 1 * Gib
	today := testingFreeGrant(GrantKindFree, overlap)

	summary := SummarizeTransferBalances([]*TransferBalance{yesterday, today}, 1*Gib, overlap)
	// exactly today's grant, nothing used: used = start - available - pending
	connect.AssertEqual(t, summary.StartBalanceByteCount, 30*Gib)
	connect.AssertEqual(t, summary.BalanceByteCount, 30*Gib)
	connect.AssertEqual(t, summary.OpenTransferByteCount, ByteCount(0))
	// both grants stay listed; the old one is still spendable
	connect.AssertEqual(t, summary.ActiveTransferBalances, []*TransferBalance{yesterday, today})

	// other balances still count, with their own reservations
	referral := testingReferralGrant(GrantKindReferral, overlap.Add(-6*time.Hour))
	referral.StartBalanceByteCount = 3 * Gib
	referral.BalanceByteCount = 2 * Gib
	referral.reservedByteCount = Gib / 2
	summary = SummarizeTransferBalances([]*TransferBalance{yesterday, today, referral}, 1*Gib+Gib/2, overlap)
	connect.AssertEqual(t, summary.StartBalanceByteCount, 33*Gib)
	connect.AssertEqual(t, summary.BalanceByteCount, 32*Gib)
	connect.AssertEqual(t, summary.OpenTransferByteCount, Gib/2)

	// outside the hour yesterday's grant has ended
	later := testGrantBoundary.Add(5 * time.Hour)
	summary = SummarizeTransferBalances([]*TransferBalance{yesterday, today}, 1*Gib, later)
	connect.AssertEqual(t, summary.StartBalanceByteCount, 30*Gib)
	connect.AssertEqual(t, summary.BalanceByteCount, 30*Gib)
	connect.AssertEqual(t, summary.OpenTransferByteCount, 1*Gib)

	// the reservation counters can drift past the open total; pending stays >= 0
	summary = SummarizeTransferBalances([]*TransferBalance{yesterday, today}, Gib/2, overlap)
	connect.AssertEqual(t, summary.OpenTransferByteCount, ByteCount(0))
}
