//go:build sidecar_pending

package controller

// FS-43 (QUOTA-M6-04; DESIGN.md 22's catalog, FI-13(b); T2: a fake Stripe, real rows): a fake Stripe
// accepts an event, then drops the response; the 24 h dedup window expires; summaries lag; retries
// arrive in both orders; a correction follows its original; a period boundary at 00:00 UTC;
// finalization at +72 h; a missed row. After every step fs43Check holds FI-13(b) on what Stripe
// took against the rows, and at each finalization the billed quantity is the rows' newest values.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// One event Stripe took for a day, as the oracle reads it.
type fs43Receipt struct {
	identifier string
	value      int64
}

// FI-13(b) for one (meter, day): what Stripe took, in the order it took it, against the day's rows,
// oldest version first. Every event is a version's, with that version's value; no event of an older
// version follows a newer one's, so the day keeps its newest received version and a retry never
// puts an old value back; a sent row's event was taken, and a sent newest version is the one Stripe
// keeps; a row missed with no attempt was never taken.
func fs43DayError(rows []*model.StripeMeterReport, receipts []fs43Receipt) error {
	versionOf := map[string]int{}
	valueOf := map[string]int64{}
	for _, row := range rows {
		versionOf[row.Identifier] = row.Version
		valueOf[row.Identifier] = row.Value
	}
	taken := map[string]bool{}
	highest := 0
	for _, receipt := range receipts {
		version, ok := versionOf[receipt.identifier]
		if !ok {
			return fmt.Errorf("Stripe took an event no row carries")
		}
		if receipt.value != valueOf[receipt.identifier] {
			return fmt.Errorf("version %d was taken with value %d, not its row's %d", version, receipt.value, valueOf[receipt.identifier])
		}
		if version < highest {
			return fmt.Errorf("version %d was taken after version %d: the day keeps an old value", version, highest)
		}
		highest = version
		taken[receipt.identifier] = true
	}
	for index, row := range rows {
		switch row.State {
		case model.StripeMeterReportStateSent:
			if !taken[row.Identifier] {
				return fmt.Errorf("version %d reads sent and Stripe never took it", row.Version)
			}
			if index == len(rows)-1 && receipts[len(receipts)-1].identifier != row.Identifier {
				return fmt.Errorf("the newest version %d is sent and Stripe keeps another", row.Version)
			}
		case model.StripeMeterReportStateMissed:
			if row.Attempts == 0 && taken[row.Identifier] {
				return fmt.Errorf("version %d reads missed and Stripe took it", row.Version)
			}
		}
	}
	return nil
}

// Holds fs43DayError on every day of the world's Team, and that each event was stamped inside its
// day and after the subscription's start.
func fs43Check(t testing.TB, world *stripeMeterTestWorld, step string) {
	t.Helper()
	type dayKey struct {
		meter string
		day   time.Time
	}
	days := map[dayKey][]*model.StripeMeterReport{}
	order := []dayKey{}
	for _, report := range model.GetStripeMeterReports(world.ctx, world.networkId, world.subscription.StartTime.AddDate(0, 0, -1)) {
		key := dayKey{report.Meter, report.UsageDay}
		if _, ok := days[key]; !ok {
			order = append(order, key)
		}
		days[key] = append(days[key], report)
	}
	for _, key := range order {
		receipts := []fs43Receipt{}
		for _, receipt := range world.stripe.dayReceipts(world.customerId, key.meter, key.day) {
			if !world.subscription.StartTime.Before(receipt.event.Timestamp) {
				t.Fatalf("%s: an event of %s %s is stamped at or before the subscription's start", step, key.meter, key.day.Format(time.DateOnly))
			}
			receipts = append(receipts, fs43Receipt{identifier: receipt.event.Identifier, value: receipt.event.Value})
		}
		if err := fs43DayError(days[key], receipts); err != nil {
			t.Fatalf("%s: FI-13(b) on %s %s: %s", step, key.meter, key.day.Format(time.DateOnly), err)
		}
	}
	// nothing Stripe took lies outside the days the rows name
	for _, receipt := range world.stripe.receipts {
		if receipt.event.CustomerId != world.customerId {
			continue
		}
		day := time.Date(receipt.event.Timestamp.Year(), receipt.event.Timestamp.Month(), receipt.event.Timestamp.Day(), 0, 0, 0, 0, time.UTC)
		if _, ok := days[dayKey{receipt.event.EventName, day}]; !ok {
			t.Fatalf("%s: Stripe took an event stamped %s, a day with no row", step, receipt.event.Timestamp)
		}
	}
}

// The oracle's order rule fails a history in which an original's retry lands after its
// correction, which is the order the sender never produces.
func TestFs43OracleRefusesAnOldVersionAfterANewer(t *testing.T) {
	rows := []*model.StripeMeterReport{
		{Version: 1, Identifier: "a", Value: 4, State: model.StripeMeterReportStateSent},
		{Version: 2, Identifier: "b", Value: 6, State: model.StripeMeterReportStateSent},
	}
	connect.AssertEqual(t, fs43DayError(rows, []fs43Receipt{{"a", 4}, {"b", 6}}), nil)
	connect.AssertEqual(t, fs43DayError(rows, []fs43Receipt{{"a", 4}, {"a", 4}, {"b", 6}}), nil)
	connect.AssertEqual(t, fs43DayError(rows, []fs43Receipt{{"b", 6}, {"a", 4}}) != nil, true)
	connect.AssertEqual(t, fs43DayError(rows, []fs43Receipt{{"a", 4}, {"b", 6}, {"a", 4}}) != nil, true)
	connect.AssertEqual(t, fs43DayError(rows, []fs43Receipt{{"a", 4}}) != nil, true)
	connect.AssertEqual(t, fs43DayError(rows, []fs43Receipt{{"a", 4}, {"b", 7}}) != nil, true)
	connect.AssertEqual(t, fs43DayError(rows, []fs43Receipt{{"a", 4}, {"b", 6}, {"c", 6}}) != nil, true)
	missed := []*model.StripeMeterReport{
		{Version: 1, Identifier: "a", Value: 4, State: model.StripeMeterReportStateMissed},
	}
	connect.AssertEqual(t, fs43DayError(missed, nil), nil)
	connect.AssertEqual(t, fs43DayError(missed, []fs43Receipt{{"a", 4}}) != nil, true)
}

func TestFs43StripeMeters(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		seats := model.TeamMeterSeats
		transfer := model.TeamMeterTransferOverage
		sent := model.StripeMeterReportStateSent
		uncertain := model.StripeMeterReportStateUncertain
		queued := model.StripeMeterReportStateQueued
		missed := model.StripeMeterReportStateMissed
		// checkout on Nov 1 at 15:00, anchored at the next 00:00:00 UTC: the first period is nine
		// hours, the second runs Nov 2 to Dec 2
		start := time.Date(2026, time.November, 1, 15, 0, 0, 0, time.UTC)
		anchor := testDay(2026, time.November, 2)
		secondEnd := testDay(2026, time.December, 2)
		world := newStripeMeterTestWorld(t, ctx, start, anchor)
		restoreMeters := stripeTeamMetersFunc
		stripeTeamMetersFunc = func() map[string]StripeTeamMeter {
			// the fake lists summaries by event name
			return map[string]StripeTeamMeter{
				seats:    {MeterId: seats},
				transfer: {MeterId: transfer},
			}
		}
		defer func() {
			stripeTeamMetersFunc = restoreMeters
		}()
		at := func(month time.Month, day int, hour int, minute int, second int) {
			t.Helper()
			world.at(time.Date(2026, month, day, hour, minute, second, 0, time.UTC))
		}
		states := func(meter string, day time.Time, expected ...model.StripeMeterReportState) {
			t.Helper()
			connect.AssertEqual(t, world.states(meter, day), expected)
		}

		// 1. Stripe accepts an event, then drops the response. The partial first day's event is
		// stamped 1 s after the start, inside the first period.
		nov1 := testDay(2026, time.November, 1)
		at(time.November, 2, 0, 10, 0)
		world.writeDay(nov1, map[string]int64{seats: 3, transfer: 2})
		world.stripe.setPlan(fakeStripeMeterApplyThenDrop)
		result := world.pass()
		connect.AssertEqual(t, result.UncertainCount, 1)
		connect.AssertEqual(t, result.SentCount, 1)
		states(seats, nov1, uncertain)
		states(transfer, nov1, sent)
		connect.AssertEqual(t, world.stripe.sentEvents()[0].Timestamp, start.Add(time.Second))
		fs43Check(t, world, "the response is dropped")
		// inside the backoff the row is not sent
		at(time.November, 2, 0, 10, 30)
		world.pass()
		connect.AssertEqual(t, world.stripe.sendCount(), 2)
		// the retry carries the same identifier and Stripe refuses it as a duplicate: sent, once
		at(time.November, 2, 0, 12, 0)
		connect.AssertEqual(t, world.pass().SentCount, 1)
		states(seats, nov1, sent)
		connect.AssertEqual(t, len(world.stripe.dayReceipts(world.customerId, seats, nov1)), 1)
		fs43Check(t, world, "the retry is a duplicate")

		// 2. The 24 h dedup window expires before the retry gets through: Stripe takes the same
		// identifier again as a new event, which replaces the day and adds nothing.
		nov2 := testDay(2026, time.November, 2)
		at(time.November, 3, 0, 10, 0)
		world.writeDay(nov2, map[string]int64{seats: 3, transfer: 5})
		world.stripe.setPlan(fakeStripeMeterAnswer, fakeStripeMeterApplyThenDrop)
		result = world.pass()
		connect.AssertEqual(t, result.UncertainCount, 1)
		states(transfer, nov2, uncertain)
		fs43Check(t, world, "the second day's response is dropped")
		for hour := 1; hour <= 24; hour += 1 {
			world.at(testDay(2026, time.November, 3).Add(time.Duration(hour) * time.Hour))
			world.stripe.setPlan(fakeStripeMeterTimeout)
			world.pass()
			states(transfer, nov2, uncertain)
		}
		world.stripe.setPlan()
		fs43Check(t, world, "Stripe is unreachable for a day")
		at(time.November, 4, 1, 10, 0)
		connect.AssertEqual(t, world.pass().SentCount, 1)
		states(transfer, nov2, sent)
		connect.AssertEqual(t, len(world.stripe.dayReceipts(world.customerId, transfer, nov2)), 2)
		connect.AssertEqual(t, world.kept(transfer, nov2), int64(5))
		connect.AssertEqual(t, world.stripe.billedSum(world.customerId, transfer, anchor, secondEnd, world.now), int64(5))
		fs43Check(t, world, "the dedup window has passed")

		// 3. A correction follows its original, whose outcome is not known: the correction waits,
		// whichever of the two is due, so the original's retry can never land after it.
		nov3 := testDay(2026, time.November, 3)
		at(time.November, 4, 1, 20, 0)
		world.writeDay(nov3, map[string]int64{transfer: 4})
		world.stripe.setPlan(fakeStripeMeterTimeout)
		connect.AssertEqual(t, world.pass().UncertainCount, 1)
		at(time.November, 4, 1, 20, 30)
		connect.AssertEqual(t, len(world.writeDay(nov3, map[string]int64{transfer: 6})), 1)
		// the correction is due and the original is in its backoff: nothing is sent
		sendCount := world.stripe.sendCount()
		world.pass()
		connect.AssertEqual(t, world.stripe.sendCount(), sendCount)
		states(transfer, nov3, uncertain, queued)
		fs43Check(t, world, "the correction waits for its original")
		// the original's retry is taken and its answer lost: still unknown, the correction waits
		at(time.November, 4, 1, 22, 0)
		world.stripe.setPlan(fakeStripeMeterApplyThenDrop)
		result = world.pass()
		connect.AssertEqual(t, result.UncertainCount, 1)
		connect.AssertEqual(t, result.SentCount, 0)
		states(transfer, nov3, uncertain, queued)
		connect.AssertEqual(t, world.kept(transfer, nov3), int64(4))
		fs43Check(t, world, "the original is taken, unknown")
		// the original's outcome becomes known, and the correction goes in the same pass
		at(time.November, 4, 1, 25, 0)
		connect.AssertEqual(t, world.pass().SentCount, 2)
		states(transfer, nov3, sent, sent)
		connect.AssertEqual(t, world.kept(transfer, nov3), int64(6))
		fs43Check(t, world, "the correction follows its original")

		// 4. Summaries lag: the audit counts the day Stripe's summary lacks, changes no row and
		// sends nothing; once the summaries catch up it finds every day equal.
		at(time.November, 4, 2, 0, 0)
		world.stripe.summaryLag = 3 * time.Hour
		sendCount = world.stripe.sendCount()
		audit, err := auditStripeMeters(ctx, world.stripe, world.now, nil, world.settings)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, audit.DayCount, 5)
		connect.AssertEqual(t, audit.AbsentCount, 1)
		connect.AssertEqual(t, audit.DifferCount, 0)
		connect.AssertEqual(t, world.stripe.sendCount(), sendCount)
		states(transfer, nov3, sent, sent)
		world.pass()
		connect.AssertEqual(t, world.stripe.sendCount(), sendCount)
		world.stripe.summaryLag = 0
		audit, err = auditStripeMeters(ctx, world.stripe, world.now, nil, world.settings)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, audit.DayCount, 5)
		connect.AssertEqual(t, audit.AbsentCount+audit.DifferCount, 0)
		fs43Check(t, world, "the summaries lag")

		// the first period's invoice finalizes 72 h after its end, with the partial day in it
		at(time.November, 5, 0, 0, 0)
		connect.AssertEqual(t, world.stripe.billedSum(world.customerId, transfer, start, anchor, world.now), int64(2))

		// 5. A row Stripe never takes, with a correction behind it, up to finalization. Stripe
		// answers 500 to every event of Nov 30 and takes the rest.
		nov30 := testDay(2026, time.November, 30)
		dec1 := testDay(2026, time.December, 1)
		dec2 := testDay(2026, time.December, 2)
		world.stripe.faultOf = func(event *stripeMeterEvent) (fakeStripeMeterFault, bool) {
			if !event.Timestamp.Before(nov30) && event.Timestamp.Before(dec1) {
				return fakeStripeMeterServerError, true
			}
			return fakeStripeMeterAnswer, false
		}
		at(time.December, 1, 0, 10, 0)
		world.writeDay(nov30, map[string]int64{transfer: 7})
		connect.AssertEqual(t, world.pass().UncertainCount, 1)
		at(time.December, 1, 0, 11, 0)
		world.writeDay(nov30, map[string]int64{transfer: 8})
		finalization := secondEnd.Add(72 * time.Hour)
		nearCount := 0
		for hourly := testDay(2026, time.December, 1).Add(time.Hour); hourly.Before(finalization); hourly = hourly.Add(time.Hour) {
			world.at(hourly)
			switch {
			case hourly.Equal(dec2.Add(time.Hour)):
				// 6. A period boundary at 00:00 UTC: Dec 1 is the period's last day, sent in the
				// grace after the period ended, and it is not held up behind Nov 30's rows
				world.writeDay(dec1, map[string]int64{transfer: 1})
			case hourly.Equal(dec2.AddDate(0, 0, 1).Add(time.Hour)):
				// Dec 2 is the next period's first day
				world.writeDay(dec2, map[string]int64{transfer: 9})
			}
			result = world.pass()
			if hourly.Before(finalization.Add(-6 * time.Hour)) {
				connect.AssertEqual(t, result.NearDeadlineCount, 0)
			} else {
				nearCount += result.NearDeadlineCount
			}
			connect.AssertEqual(t, result.MissedCount+result.ParkedCount, 0)
			states(transfer, nov30, uncertain, queued)
		}
		// a row unsent within 6 h of its finalization is counted for the billing alert
		connect.AssertEqual(t, 0 < nearCount, true)
		states(transfer, dec1, sent)
		states(transfer, dec2, sent)
		connect.AssertEqual(t, model.TeamStripeMeterSendWindow(world.subscription, dec1).PeriodEnd, secondEnd)
		connect.AssertEqual(t, model.TeamStripeMeterSendWindow(world.subscription, dec2).PeriodEnd, testDay(2027, time.January, 2))
		fs43Check(t, world, "Stripe refuses one day up to its finalization")

		// 7. Finalization at +72 h. The correction was never sent: missed. Its original was tried
		// and never answered: it stays uncertain and is put away. Neither is sent again.
		world.at(finalization)
		result = world.pass()
		connect.AssertEqual(t, result.MissedCount, 1)
		connect.AssertEqual(t, result.ParkedCount, 1)
		states(transfer, nov30, uncertain, missed)
		connect.AssertEqual(t, world.kept(transfer, nov30), int64(-1))
		// the period's billed quantity is its rows' newest values, each day once: Nov 2's 5, Nov
		// 3's corrected 6 and Dec 1's 1; Nov 30 is unbilled
		connect.AssertEqual(t, world.stripe.billedSum(world.customerId, transfer, anchor, secondEnd, finalization), int64(12))
		connect.AssertEqual(t, world.stripe.billedSum(world.customerId, transfer, secondEnd, testDay(2027, time.January, 2), finalization), int64(9))
		fs43Check(t, world, "finalization")

		// Stripe recovers: the missed day is never added to a later invoice
		world.stripe.faultOf = nil
		sendCount = world.stripe.sendCount()
		for _, later := range []time.Duration{time.Hour, 24 * time.Hour, 40 * 24 * time.Hour} {
			world.at(finalization.Add(later))
			result = world.pass()
			connect.AssertEqual(t, result.MissedCount+result.ParkedCount, 0)
		}
		connect.AssertEqual(t, world.stripe.sendCount(), sendCount)
		states(transfer, nov30, uncertain, missed)
		connect.AssertEqual(t, world.stripe.billedSum(world.customerId, transfer, anchor, secondEnd, world.now), int64(12))
		fs43Check(t, world, "after finalization")
	})
}
