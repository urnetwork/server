package model

// The per-cycle Pro grant's cycle arithmetic, without a database
// (server/PRO-GRANT-CYCLE.md). Every time is fixed, so each case is
// deterministic.

import (
	"testing"
	"time"
)

func cycleTime(value string) time.Time {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func assertCycle(t *testing.T, name string, cycle ProCycle, ok bool, start string, end string) {
	t.Helper()
	if !ok {
		t.Fatalf("%s: no cycle due, want [%s, %s)", name, start, end)
	}
	if !cycle.StartTime.Equal(cycleTime(start)) || !cycle.EndTime.Equal(cycleTime(end)) {
		t.Fatalf("%s: cycle [%s, %s), want [%s, %s)", name, cycle.StartTime.Format(time.RFC3339), cycle.EndTime.Format(time.RFC3339), start, end)
	}
}

func assertNoCycle(t *testing.T, name string, cycle ProCycle, ok bool) {
	t.Helper()
	if ok {
		t.Fatalf("%s: cycle [%s, %s) due, want none", name, cycle.StartTime.Format(time.RFC3339), cycle.EndTime.Format(time.RFC3339))
	}
}

func paidPeriod(start string, end string, revenueUsd float64) ProPaidPeriod {
	return ProPaidPeriod{StartTime: cycleTime(start), EndTime: cycleTime(end), NetRevenue: UsdToNanoCents(revenueUsd)}
}

// Months keep the anchor's day and time, clamped to a short month's last day,
// the way Stripe bills a monthly anchor.
func TestAddProCycleMonthsClampsToTheMonthsLastDay(t *testing.T) {
	anchor := cycleTime("2026-01-31T10:30:00Z")
	for months, want := range []string{
		"2026-01-31T10:30:00Z",
		"2026-02-28T10:30:00Z",
		"2026-03-31T10:30:00Z",
		"2026-04-30T10:30:00Z",
	} {
		if got := addProCycleMonths(anchor, months); !got.Equal(cycleTime(want)) {
			t.Fatalf("anchor + %d months = %s, want %s", months, got.Format(time.RFC3339), want)
		}
	}
	if got := addProCycleMonths(cycleTime("2027-11-29T00:00:00Z"), 3); !got.Equal(cycleTime("2028-02-29T00:00:00Z")) {
		t.Fatalf("a leap February keeps the 29th: %s", got.Format(time.RFC3339))
	}
	if got := addProCycleMonths(cycleTime("2026-11-15T12:00:00Z"), 14); !got.Equal(cycleTime("2028-01-15T12:00:00Z")) {
		t.Fatalf("months past a year: %s", got.Format(time.RFC3339))
	}
}

// A mid-month subscriber's first cycle starts at its own period start and runs
// its billing period, not the calendar month (the root cause: ProGrantWindow
// gave every network [the 1st, the next 1st + 1 day)).
func TestProCycleOfMidMonthSubscriberIsItsOwnPeriod(t *testing.T) {
	periods := []ProPaidPeriod{paidPeriod("2026-11-14T10:00:00Z", "2026-12-14T10:00:00Z", 3.5)}
	cycle, ok := NextProCycle(periods, time.Time{}, cycleTime("2026-11-14T10:00:05Z"))
	assertCycle(t, "first cycle", cycle, ok, "2026-11-14T10:00:00Z", "2026-12-14T10:00:00Z")
	if calendarStart, _ := ProGrantWindow(cycleTime("2026-11-14T10:00:05Z")); cycle.StartTime.Equal(calendarStart) {
		t.Fatal("the cycle is still the calendar month")
	}

	// the cycle is running: nothing is due again inside it
	cycle, ok = NextProCycle(periods, cycleTime("2026-12-14T10:00:00Z"), cycleTime("2026-12-01T00:00:00Z"))
	assertNoCycle(t, "inside the first cycle, on the 1st", cycle, ok)

	// the renewal's period starts the next cycle at the previous one's end
	periods = append(periods, paidPeriod("2026-12-14T10:00:00Z", "2027-01-14T10:00:00Z", 3.5))
	cycle, ok = NextProCycle(periods, cycleTime("2026-12-14T10:00:00Z"), cycleTime("2026-12-14T10:30:00Z"))
	assertCycle(t, "renewal", cycle, ok, "2026-12-14T10:00:00Z", "2027-01-14T10:00:00Z")

	// no renewal: the chain stops at the paid end
	cycle, ok = NextProCycle(periods[:1], cycleTime("2026-12-14T10:00:00Z"), cycleTime("2026-12-14T10:30:00Z"))
	assertNoCycle(t, "lapsed", cycle, ok)
}

// A yearly period has twelve monthly cycles from its start; the last runs to the
// period's end.
func TestProCyclesOfYearlyPeriodAreMonthlyFromItsStart(t *testing.T) {
	periods := []ProPaidPeriod{paidPeriod("2026-03-15T08:00:00Z", "2027-03-15T08:00:00Z", 28)}
	head := time.Time{}
	starts := []string{}
	now := cycleTime("2026-03-15T08:00:00Z")
	for i := 0; i < 14; i++ {
		cycle, ok := NextProCycle(periods, head, now)
		if !ok {
			break
		}
		starts = append(starts, cycle.StartTime.Format("2006-01-02"))
		head = cycle.EndTime
		now = cycle.EndTime.Add(time.Minute)
	}
	want := []string{
		"2026-03-15", "2026-04-15", "2026-05-15", "2026-06-15", "2026-07-15", "2026-08-15",
		"2026-09-15", "2026-10-15", "2026-11-15", "2026-12-15", "2027-01-15", "2027-02-15",
	}
	if len(starts) != len(want) {
		t.Fatalf("cycles %v, want %v", starts, want)
	}
	for i := range want {
		if starts[i] != want[i] {
			t.Fatalf("cycles %v, want %v", starts, want)
		}
	}
	if !head.Equal(cycleTime("2027-03-15T08:00:00Z")) {
		t.Fatalf("the last cycle ends at %s, want the period's end", head.Format(time.RFC3339))
	}
}

// A store whose period end is a little past the anchor's month (or short of it)
// never gets a sliver of a second cycle: the boundary within a week of the
// period's end is skipped.
func TestProCycleNeverEndsAPeriodInASliver(t *testing.T) {
	long := ProPaidPeriod{StartTime: cycleTime("2026-11-14T10:00:00Z"), EndTime: cycleTime("2026-12-14T13:00:00Z")}
	if end := proCycleEnd(long, long.StartTime); !end.Equal(long.EndTime) {
		t.Fatalf("a month and three hours ends at %s, want the period's end", end.Format(time.RFC3339))
	}
	short := ProPaidPeriod{StartTime: cycleTime("2026-01-31T10:00:00Z"), EndTime: cycleTime("2026-02-27T10:00:00Z")}
	if end := proCycleEnd(short, short.StartTime); !end.Equal(short.EndTime) {
		t.Fatalf("a short month ends at %s, want the period's end", end.Format(time.RFC3339))
	}
}

// The stores' accelerated test renewals (minutes long) get one 7-day cycle, not a
// full grant every few minutes.
func TestProCycleOfTestRenewalsIsAWeek(t *testing.T) {
	start := cycleTime("2026-11-11T09:00:00Z")
	periods := []ProPaidPeriod{}
	for i := 0; i < 12; i++ {
		periods = append(periods, ProPaidPeriod{StartTime: start.Add(time.Duration(i) * 5 * time.Minute), EndTime: start.Add(time.Duration(i+1) * 5 * time.Minute)})
	}
	cycle, ok := NextProCycle(periods, time.Time{}, start.Add(time.Minute))
	assertCycle(t, "first sandbox renewal", cycle, ok, "2026-11-11T09:00:00Z", "2026-11-18T09:00:00Z")
	for i := 1; i < 12; i++ {
		cycle, ok = NextProCycle(periods, cycleTime("2026-11-18T09:00:00Z"), start.Add(time.Duration(i)*5*time.Minute+time.Minute))
		assertNoCycle(t, "a later sandbox renewal", cycle, ok)
	}
}

// Two markets paying at once get one chain: the second market's period never
// starts a cycle of its own while the first market's cycle runs, and the chain
// keeps its own boundaries where both cover a cycle start.
func TestProCyclesOfOverlappingMarketsAreOneChain(t *testing.T) {
	stripe := paidPeriod("2026-11-05T00:00:00Z", "2026-12-05T00:00:00Z", 3.5)
	apple := paidPeriod("2026-11-20T12:00:00Z", "2026-12-20T12:00:00Z", 3.5)
	periods := []ProPaidPeriod{stripe, apple}

	cycle, ok := NextProCycle(periods, time.Time{}, cycleTime("2026-11-21T00:00:00Z"))
	assertCycle(t, "the first market's cycle", cycle, ok, "2026-11-05T00:00:00Z", "2026-12-05T00:00:00Z")
	cycle, ok = NextProCycle(periods, cycleTime("2026-12-05T00:00:00Z"), cycleTime("2026-11-21T00:00:00Z"))
	assertNoCycle(t, "the second market inside the first's cycle", cycle, ok)

	// Stripe renews too: the chain stays on Stripe's boundaries
	periods = append(periods, paidPeriod("2026-12-05T00:00:00Z", "2027-01-05T00:00:00Z", 3.5))
	cycle, ok = NextProCycle(periods, cycleTime("2026-12-05T00:00:00Z"), cycleTime("2026-12-05T01:00:00Z"))
	assertCycle(t, "both renewing", cycle, ok, "2026-12-05T00:00:00Z", "2027-01-05T00:00:00Z")

	// Stripe cancelled: the App Store period carries the chain to its end
	cycle, ok = NextProCycle([]ProPaidPeriod{stripe, apple}, cycleTime("2026-12-05T00:00:00Z"), cycleTime("2026-12-05T01:00:00Z"))
	assertCycle(t, "the second market after the first ends", cycle, ok, "2026-12-05T00:00:00Z", "2026-12-20T12:00:00Z")
}

// After a lapse, a new subscription starts a new chain at its own start.
func TestProCycleAfterALapseStartsANewChain(t *testing.T) {
	periods := []ProPaidPeriod{
		paidPeriod("2026-08-01T00:00:00Z", "2026-09-01T00:00:00Z", 3.5),
		paidPeriod("2026-11-09T16:00:00Z", "2026-12-09T16:00:00Z", 3.5),
	}
	cycle, ok := NextProCycle(periods, cycleTime("2026-09-01T00:00:00Z"), cycleTime("2026-11-09T16:00:01Z"))
	assertCycle(t, "resubscribed", cycle, ok, "2026-11-09T16:00:00Z", "2026-12-09T16:00:00Z")
}

// A sweep that was down for months grants only the cycle running now, never the
// cycles that ended without a grant.
func TestProCycleStepsOverCyclesThatEndedUngranted(t *testing.T) {
	periods := []ProPaidPeriod{paidPeriod("2026-01-10T00:00:00Z", "2027-01-10T00:00:00Z", 28)}
	cycle, ok := NextProCycle(periods, cycleTime("2026-02-10T00:00:00Z"), cycleTime("2026-06-20T00:00:00Z"))
	assertCycle(t, "late sweep", cycle, ok, "2026-06-10T00:00:00Z", "2026-07-10T00:00:00Z")
}

// Cutover: a network granted on the 1st starts its chain where that grant's
// month ends. Its first cycle runs from there to the end of its own current
// cycle, with no gap, and the next cycle starts on its own boundaries.
func TestProCycleCutoverStartsWhereTheLegacyMonthEnds(t *testing.T) {
	// renews on the 15th, monthly
	periods := []ProPaidPeriod{paidPeriod("2026-11-15T09:00:00Z", "2026-12-15T09:00:00Z", 3.5)}
	legacyPeriodEnd := cycleTime("2026-12-01T00:00:00Z")

	cycle, ok := NextProCycle(periods, legacyPeriodEnd, cycleTime("2026-11-20T00:00:00Z"))
	assertNoCycle(t, "inside the legacy month", cycle, ok)
	cycle, ok = NextProCycle(periods, legacyPeriodEnd, cycleTime("2026-12-01T00:30:00Z"))
	assertCycle(t, "the legacy month ended", cycle, ok, "2026-12-01T00:00:00Z", "2026-12-15T09:00:00Z")

	periods = append(periods, paidPeriod("2026-12-15T09:00:00Z", "2027-01-15T09:00:00Z", 3.5))
	cycle, ok = NextProCycle(periods, cycleTime("2026-12-15T09:00:00Z"), cycleTime("2026-12-15T09:05:00Z"))
	assertCycle(t, "its own renewal", cycle, ok, "2026-12-15T09:00:00Z", "2027-01-15T09:00:00Z")

	// a yearly plan stays on its own monthly boundaries after the cutover
	yearly := []ProPaidPeriod{paidPeriod("2026-03-02T00:00:00Z", "2027-03-02T00:00:00Z", 28)}
	cycle, ok = NextProCycle(yearly, legacyPeriodEnd, cycleTime("2026-12-01T00:30:00Z"))
	assertCycle(t, "yearly, a boundary a day after the cutover is skipped", cycle, ok, "2026-12-01T00:00:00Z", "2027-01-02T00:00:00Z")
}

// A cancelled subscription keeps the cycles it paid for and gets none after.
func TestProCyclesStopAtTheEndOfAPaidTerm(t *testing.T) {
	// a yearly plan cancelled at period end: its monthly cycles run to the end
	yearly := []ProPaidPeriod{paidPeriod("2026-01-20T00:00:00Z", "2027-01-20T00:00:00Z", 28)}
	cycle, ok := NextProCycle(yearly, cycleTime("2026-11-20T00:00:00Z"), cycleTime("2026-11-20T00:10:00Z"))
	assertCycle(t, "paid through", cycle, ok, "2026-11-20T00:00:00Z", "2026-12-20T00:00:00Z")
	cycle, ok = NextProCycle(yearly, cycleTime("2027-01-20T00:00:00Z"), cycleTime("2027-01-20T00:10:00Z"))
	assertNoCycle(t, "after the paid term", cycle, ok)
}

// A cycle carries each period's revenue pro-rated to the part of it the cycle
// covers: twelve cycles carry a year's revenue once, and two markets both count.
func TestProCycleSubsidyNetRevenueCountsEachPeriodOnce(t *testing.T) {
	yearly := []ProPaidPeriod{paidPeriod("2026-03-15T08:00:00Z", "2027-03-15T08:00:00Z", 28)}
	total := NanoCents(0)
	head := time.Time{}
	now := cycleTime("2026-03-15T08:00:00Z")
	for {
		cycle, ok := NextProCycle(yearly, head, now)
		if !ok {
			break
		}
		total += proCycleSubsidyNetRevenue(yearly, cycle)
		head = cycle.EndTime
		now = cycle.EndTime.Add(time.Minute)
	}
	if diff := total - UsdToNanoCents(28); diff < -UsdToNanoCents(0.0001) || UsdToNanoCents(0.0001) < diff {
		t.Fatalf("the year's cycles carry %d, want the year's revenue %d", total, UsdToNanoCents(28))
	}

	monthly := paidPeriod("2026-11-05T00:00:00Z", "2026-12-05T00:00:00Z", 3.5)
	cycle := ProCycle{StartTime: monthly.StartTime, EndTime: monthly.EndTime}
	if got := proCycleSubsidyNetRevenue([]ProPaidPeriod{monthly}, cycle); got != UsdToNanoCents(3.5) {
		t.Fatalf("a monthly cycle carries %d, want its period's revenue", got)
	}
	both := []ProPaidPeriod{monthly, paidPeriod("2026-11-05T00:00:00Z", "2027-11-05T00:00:00Z", 36.5)}
	if got := proCycleSubsidyNetRevenue(both, cycle); got <= UsdToNanoCents(3.5) {
		t.Fatalf("two markets carry %d, want both", got)
	}
}

// Renewal rows become paid periods: the paid end without the grace, a hold left
// out, an ended row cut at its end, a legacy row read by its market's grace, a
// Play row's period started at the purchase's previous paid end, and an x402
// month left out.
func TestProPaidPeriodsFromRenewalRows(t *testing.T) {
	paidEnd := cycleTime("2026-12-10T00:00:00Z")
	refundedAt := cycleTime("2026-11-20T00:00:00Z")
	playOriginalStart := cycleTime("2026-03-15T00:00:00Z")
	playPreviousPaidEnd := cycleTime("2026-11-15T00:00:00Z")
	playPaidEnd := cycleTime("2026-12-15T00:00:00Z")
	rows := []proRenewalRow{
		// a Stripe credit: 30 days of manual-payment grace past its paid end
		{startTime: cycleTime("2026-11-10T00:00:00Z"), endTime: paidEnd.Add(30 * 24 * time.Hour), paidEndTime: &paidEnd, market: SubscriptionMarketStripe},
		// an App Store billing grace hold
		{startTime: cycleTime("2026-10-01T00:00:00Z"), endTime: cycleTime("2026-11-17T00:00:00Z"), market: SubscriptionMarketApple},
		// refunded: an end path set end_time to the refund time
		{startTime: cycleTime("2026-11-01T00:00:00Z"), endTime: refundedAt, paidEndTime: &paidEnd, market: SubscriptionMarketApple},
		// a legacy Stripe monthly with the 30-day grace
		{startTime: cycleTime("2026-10-10T00:00:00Z"), endTime: cycleTime("2026-12-10T00:00:00Z"), market: SubscriptionMarketStripe, legacyCredit: true},
		// a legacy Stripe yearly from before the 30-day grace: a day
		{startTime: cycleTime("2026-03-01T00:00:00Z"), endTime: cycleTime("2027-03-02T00:00:00Z"), market: SubscriptionMarketStripe, legacyCredit: true},
		// Play rows start at the subscription's start
		{startTime: playOriginalStart, endTime: playPreviousPaidEnd.Add(24 * time.Hour), market: SubscriptionMarketGoogle, purchaseToken: "token", legacyCredit: true},
		{startTime: playOriginalStart, endTime: playPaidEnd.Add(24 * time.Hour), paidEndTime: &playPaidEnd, market: SubscriptionMarketGoogle, purchaseToken: "token"},
		// an x402 month carries its own data: not a cycle's period
		{startTime: cycleTime("2026-11-05T00:00:00Z"), endTime: cycleTime("2027-01-04T00:00:00Z"), paidEndTime: &paidEnd, market: SubscriptionMarketX402},
	}
	periods := proPaidPeriods(rows)
	want := []ProPaidPeriod{
		{StartTime: cycleTime("2026-11-10T00:00:00Z"), EndTime: paidEnd},
		{StartTime: cycleTime("2026-11-01T00:00:00Z"), EndTime: refundedAt},
		{StartTime: cycleTime("2026-10-10T00:00:00Z"), EndTime: cycleTime("2026-11-10T00:00:00Z")},
		{StartTime: cycleTime("2026-03-01T00:00:00Z"), EndTime: cycleTime("2027-03-01T00:00:00Z")},
		{StartTime: playOriginalStart, EndTime: playPreviousPaidEnd},
		{StartTime: playPreviousPaidEnd, EndTime: playPaidEnd},
	}
	if len(periods) != len(want) {
		t.Fatalf("%d periods, want %d: %v", len(periods), len(want), periods)
	}
	for i := range want {
		if !periods[i].StartTime.Equal(want[i].StartTime) || !periods[i].EndTime.Equal(want[i].EndTime) {
			t.Fatalf("period %d is [%s, %s), want [%s, %s)", i,
				periods[i].StartTime.Format(time.RFC3339), periods[i].EndTime.Format(time.RFC3339),
				want[i].StartTime.Format(time.RFC3339), want[i].EndTime.Format(time.RFC3339))
		}
	}
}
