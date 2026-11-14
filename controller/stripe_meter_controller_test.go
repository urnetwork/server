//go:build sidecar_pending

package controller

// QUOTA-M6-04: the meter sender against the fake Stripe, on Postgres with MG6: each outcome's
// state, one send in flight a (customer, meter), a dead sender's row sent again, a send's window,
// and RecomputeStripeMeterReports after a restore. FS-43's schedule is fs43_stripe_meter_test.go.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// One Team's meters on a test clock: its subscription row, the fake Stripe and the sender.
type stripeMeterTestWorld struct {
	t          testing.TB
	ctx        context.Context
	now        time.Time
	stripe     *fakeStripeMeters
	settings   *StripeMeterSettings
	networkId  server.Id
	customerId string
	// the subscription as its row holds it
	subscription *model.TeamSubscription
}

func newStripeMeterTestWorld(t testing.TB, ctx context.Context, start time.Time, anchor time.Time) *stripeMeterTestWorld {
	t.Helper()
	world := &stripeMeterTestWorld{
		t:        t,
		ctx:      ctx,
		now:      start,
		settings: DefaultStripeMeterSettings(),
	}
	world.stripe = newFakeStripeMeters(world.clock)
	world.addTeam(start, anchor)
	return world
}

func (self *stripeMeterTestWorld) clock() time.Time {
	return self.now
}

// Moves the clock to at, never back.
func (self *stripeMeterTestWorld) at(at time.Time) {
	self.t.Helper()
	if at.Before(self.now) {
		self.t.Fatalf("the clock would run back from %s to %s", self.now, at)
	}
	self.now = at
}

// Makes the world's Team another network with its own live subscription.
func (self *stripeMeterTestWorld) addTeam(start time.Time, anchor time.Time) {
	self.t.Helper()
	self.networkId = server.NewId()
	self.customerId = "cus_synthetic_" + self.networkId.String()
	self.subscription = &model.TeamSubscription{
		NetworkId:      self.networkId,
		SubscriptionId: "sub_synthetic_" + self.networkId.String(),
		CustomerId:     self.customerId,
		AnchorTime:     anchor,
		StartTime:      start,
	}
	server.Tx(self.ctx, func(tx server.PgTx) {
		connect.AssertEqual(self.t, model.AddTeamSubscriptionInTx(self.ctx, tx, self.subscription), true)
	})
}

// Writes the day's values as the rollup does, at the clock.
func (self *stripeMeterTestWorld) writeDay(day time.Time, meterValues map[string]int64) []*model.StripeMeterReport {
	self.t.Helper()
	var written []*model.StripeMeterReport
	server.Tx(self.ctx, func(tx server.PgTx) {
		var err error
		written, err = model.WriteStripeMeterRowsInTx(self.ctx, tx, self.networkId, day, meterValues, self.now)
		connect.AssertEqual(self.t, err, nil)
	})
	return written
}

// One pass of the sender at the clock.
func (self *stripeMeterTestWorld) pass() *ReportStripeMetersResult {
	self.t.Helper()
	result, err := reportStripeMeters(self.ctx, self.stripe, self.clock, self.settings)
	connect.AssertEqual(self.t, err, nil)
	return result
}

// The versions of the day's row of meter, oldest first.
func (self *stripeMeterTestWorld) rows(meter string, day time.Time) (rows []*model.StripeMeterReport) {
	for _, report := range model.GetStripeMeterReports(self.ctx, self.networkId, day) {
		if report.Meter == meter && report.UsageDay.Equal(day) {
			rows = append(rows, report)
		}
	}
	return
}

func (self *stripeMeterTestWorld) states(meter string, day time.Time) (states []model.StripeMeterReportState) {
	for _, report := range self.rows(meter, day) {
		states = append(states, report.State)
	}
	return
}

// The value Stripe keeps for the day now, or -1 when it holds no event of it.
func (self *stripeMeterTestWorld) kept(meter string, day time.Time) int64 {
	receipt := self.stripe.dayEvent(self.customerId, meter, day, self.now)
	if receipt == nil {
		return -1
	}
	return receipt.event.Value
}

func testDay(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// §20.9's outcomes, by what Stripe answered: no answer, 429 and 5xx are uncertain; a 4xx naming a
// duplicate identifier is sent; any other 4xx is failed; an answer the meter API never gives is
// uncertain.
func TestStripeMeterErrorOutcome(t *testing.T) {
	codes := DefaultStripeMeterSettings().DuplicateIdentifierCodes
	identifier := "c0ffee"
	for _, vector := range []struct {
		err     error
		outcome model.StripeMeterOutcome
	}{
		{context.DeadlineExceeded, model.StripeMeterOutcomeUncertain},
		{errors.New("connection reset"), model.StripeMeterOutcomeUncertain},
		{&stripe.Error{Msg: "no status"}, model.StripeMeterOutcomeUncertain},
		{&stripe.Error{HTTPStatusCode: 429}, model.StripeMeterOutcomeUncertain},
		{&stripe.Error{HTTPStatusCode: 500}, model.StripeMeterOutcomeUncertain},
		{&stripe.Error{HTTPStatusCode: 503}, model.StripeMeterOutcomeUncertain},
		{&stripe.Error{HTTPStatusCode: 302}, model.StripeMeterOutcomeUncertain},
		{&stripe.Error{HTTPStatusCode: 400, Code: stripe.ErrorCodeResourceAlreadyExists}, model.StripeMeterOutcomeSent},
		{&stripe.Error{HTTPStatusCode: 400, Param: "identifier", Msg: "An event with identifier c0ffee exists."}, model.StripeMeterOutcomeSent},
		{&stripe.Error{HTTPStatusCode: 400, Param: "identifier", Msg: "The identifier is too long."}, model.StripeMeterOutcomeFailed},
		{&stripe.Error{HTTPStatusCode: 400, Code: stripe.ErrorCodeParameterMissing}, model.StripeMeterOutcomeFailed},
		{&stripe.Error{HTTPStatusCode: 401}, model.StripeMeterOutcomeFailed},
		{&stripe.Error{HTTPStatusCode: 404}, model.StripeMeterOutcomeFailed},
		{fmt.Errorf("wrapped: %w", &stripe.Error{HTTPStatusCode: 404}), model.StripeMeterOutcomeFailed},
	} {
		if outcome := stripeMeterErrorOutcome(vector.err, identifier, codes); outcome != vector.outcome {
			t.Errorf("%v is %s, not %s", vector.err, outcome, vector.outcome)
		}
	}
}

// §20.9's send row: the event is stamped 12:00:00 UTC of its day, or 1 s after the subscription's
// start when that is later; its invoice finalizes 72 h after the period it lies in, a period
// ending at the anchor and then on the anchor's day each month; an ended subscription's last
// period closes when it ends; and no deadline passes Stripe's 35 days.
func TestTeamStripeMeterSendWindow(t *testing.T) {
	start := time.Date(2026, time.November, 1, 15, 0, 0, 0, time.UTC)
	anchor := testDay(2026, time.November, 2)
	subscription := &model.TeamSubscription{AnchorTime: anchor, StartTime: start}

	// the partial first day lies inside the first period
	window := model.TeamStripeMeterSendWindow(subscription, testDay(2026, time.November, 1))
	connect.AssertEqual(t, window.Timestamp, start.Add(time.Second))
	connect.AssertEqual(t, window.PeriodEnd, anchor)
	connect.AssertEqual(t, window.Deadline, anchor.Add(72*time.Hour))

	// the first whole day opens the second period
	window = model.TeamStripeMeterSendWindow(subscription, testDay(2026, time.November, 2))
	connect.AssertEqual(t, window.Timestamp, time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC))
	connect.AssertEqual(t, window.PeriodEnd, testDay(2026, time.December, 2))
	// 35 days from the timestamp come after the period's finalization
	connect.AssertEqual(t, window.Deadline, testDay(2026, time.December, 5))

	// the period's last day and the next period's first, either side of 00:00 UTC
	window = model.TeamStripeMeterSendWindow(subscription, testDay(2026, time.December, 1))
	connect.AssertEqual(t, window.PeriodEnd, testDay(2026, time.December, 2))
	connect.AssertEqual(t, window.Deadline, testDay(2026, time.December, 5))
	window = model.TeamStripeMeterSendWindow(subscription, testDay(2026, time.December, 2))
	connect.AssertEqual(t, window.PeriodEnd, testDay(2027, time.January, 2))
	connect.AssertEqual(t, window.Deadline, testDay(2027, time.January, 5))

	// an anchor on the 31st bills on a short month's last day
	endOfMonth := &model.TeamSubscription{AnchorTime: testDay(2027, time.January, 31), StartTime: testDay(2027, time.January, 30).Add(9 * time.Hour)}
	window = model.TeamStripeMeterSendWindow(endOfMonth, testDay(2027, time.February, 10))
	connect.AssertEqual(t, window.PeriodEnd, testDay(2027, time.February, 28))

	// ended inside its period: the last invoice is cut at the end, with no grace
	end := time.Date(2026, time.November, 20, 8, 0, 0, 0, time.UTC)
	ended := &model.TeamSubscription{AnchorTime: anchor, StartTime: start, EndTime: &end}
	window = model.TeamStripeMeterSendWindow(ended, testDay(2026, time.November, 19))
	connect.AssertEqual(t, window.Deadline, end)
	// a period that closed before the end keeps its grace
	window = model.TeamStripeMeterSendWindow(ended, testDay(2026, time.November, 1))
	connect.AssertEqual(t, window.Deadline, anchor.Add(72*time.Hour))

	// Stripe's timestamp window bounds a deadline that a longer grace would pass
	defer model.Testing_SetStripeMeterSendSettings(func() *model.StripeMeterSendSettings {
		settings := model.DefaultStripeMeterSendSettings()
		settings.InvoiceGrace = 20 * 24 * time.Hour
		return settings
	}())()
	window = model.TeamStripeMeterSendWindow(subscription, testDay(2026, time.November, 2))
	connect.AssertEqual(t, window.Deadline, window.Timestamp.Add(35*24*time.Hour))
}

// Each outcome lands in its state: 200 is sent; a refusal naming a duplicate is sent; another 4xx
// is failed and never sent again; a timeout, 429 and 500 are uncertain and sent again after the
// backoff with the same identifier and value.
func TestReportStripeMetersOutcomeStates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := time.Date(2026, time.November, 1, 15, 0, 0, 0, time.UTC)
		anchor := testDay(2026, time.November, 2)
		day := testDay(2026, time.November, 3)
		world := newStripeMeterTestWorld(t, ctx, start, anchor)
		world.at(day.AddDate(0, 0, 1).Add(10 * time.Minute))

		// the pairs are claimed in meter order: seats, storage, transfer
		world.writeDay(day, map[string]int64{
			model.TeamMeterSeats:           6,
			model.TeamMeterStorageOverage:  2,
			model.TeamMeterTransferOverage: 9,
		})
		world.stripe.setPlan(fakeStripeMeterAnswer, fakeStripeMeterRefused, fakeStripeMeterRateLimited)
		result := world.pass()
		connect.AssertEqual(t, result.SentCount, 1)
		connect.AssertEqual(t, result.FailedCount, 1)
		connect.AssertEqual(t, result.UncertainCount, 1)
		connect.AssertEqual(t, world.states(model.TeamMeterSeats, day), []model.StripeMeterReportState{model.StripeMeterReportStateSent})
		connect.AssertEqual(t, world.states(model.TeamMeterStorageOverage, day), []model.StripeMeterReportState{model.StripeMeterReportStateFailed})
		connect.AssertEqual(t, world.states(model.TeamMeterTransferOverage, day), []model.StripeMeterReportState{model.StripeMeterReportStateUncertain})
		connect.AssertEqual(t, world.kept(model.TeamMeterSeats, day), int64(6))
		connect.AssertEqual(t, world.kept(model.TeamMeterStorageOverage, day), int64(-1))
		connect.AssertEqual(t, world.kept(model.TeamMeterTransferOverage, day), int64(-1))

		// inside the backoff nothing is sent; past it only the uncertain row is, as it was
		world.at(world.now.Add(30 * time.Second))
		connect.AssertEqual(t, world.pass().UncertainCount, 0)
		connect.AssertEqual(t, world.stripe.sendCount(), 3)
		for _, fault := range []fakeStripeMeterFault{fakeStripeMeterServerError, fakeStripeMeterTimeout} {
			world.at(world.now.Add(time.Hour))
			world.stripe.setPlan(fault)
			result = world.pass()
			connect.AssertEqual(t, result.UncertainCount, 1)
			connect.AssertEqual(t, result.SentCount+result.FailedCount, 0)
		}
		world.at(world.now.Add(time.Hour))
		// the answer of the last try is lost, and its retry is refused as a duplicate
		world.stripe.setPlan(fakeStripeMeterApplyThenDrop)
		connect.AssertEqual(t, world.pass().UncertainCount, 1)
		world.at(world.now.Add(time.Hour))
		connect.AssertEqual(t, world.pass().SentCount, 1)
		connect.AssertEqual(t, world.states(model.TeamMeterTransferOverage, day), []model.StripeMeterReportState{model.StripeMeterReportStateSent})
		connect.AssertEqual(t, world.kept(model.TeamMeterTransferOverage, day), int64(9))

		sends := world.stripe.sentEvents()
		connect.AssertEqual(t, len(sends), 7)
		row := world.rows(model.TeamMeterTransferOverage, day)[0]
		connect.AssertEqual(t, row.Attempts, 5)
		for _, send := range sends[2:] {
			connect.AssertEqual(t, send.EventName, model.TeamMeterTransferOverage)
			connect.AssertEqual(t, send.Identifier, row.Identifier)
			connect.AssertEqual(t, send.Value, int64(9))
			connect.AssertEqual(t, send.CustomerId, world.customerId)
			connect.AssertEqual(t, send.Timestamp, day.Add(12*time.Hour))
		}
		// Stripe took the event once
		connect.AssertEqual(t, len(world.stripe.dayReceipts(world.customerId, model.TeamMeterTransferOverage, day)), 1)

		// nothing is left to send, the failed row included
		world.at(world.now.Add(24 * time.Hour))
		world.pass()
		connect.AssertEqual(t, world.stripe.sendCount(), 7)
	})
}

// At most one send is in flight a (customer, meter): while one pass holds a pair's send, another
// sends nothing of the pair, and the pair's next day follows only once the first is recorded. A
// row whose sender died is sent again with the same identifier once its claim ends, and the dead
// sender's late record changes nothing.
func TestReportStripeMetersOneInFlight(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := time.Date(2026, time.November, 1, 15, 0, 0, 0, time.UTC)
		anchor := testDay(2026, time.November, 2)
		day1 := testDay(2026, time.November, 3)
		day2 := testDay(2026, time.November, 4)
		world := newStripeMeterTestWorld(t, ctx, start, anchor)
		world.at(day2.AddDate(0, 0, 1).Add(10 * time.Minute))
		world.writeDay(day1, map[string]int64{model.TeamMeterTransferOverage: 1})
		world.writeDay(day2, map[string]int64{model.TeamMeterTransferOverage: 2})

		held := make(chan struct{})
		release := make(chan struct{})
		first := true
		world.stripe.sendHook = func(event *stripeMeterEvent) {
			if first {
				first = false
				close(held)
				<-release
			}
		}
		done := make(chan *ReportStripeMetersResult, 1)
		go func() {
			result, err := reportStripeMeters(ctx, world.stripe, world.clock, world.settings)
			if err != nil {
				result = nil
			}
			done <- result
		}()
		select {
		case <-held:
		case <-time.After(30 * time.Second):
			t.Fatal("the first send did not start")
		}
		// the pair is in flight: a second pass sends nothing
		second := world.pass()
		connect.AssertEqual(t, second.SentCount+second.UncertainCount+second.FailedCount+second.LostCount, 0)
		connect.AssertEqual(t, world.states(model.TeamMeterTransferOverage, day1), []model.StripeMeterReportState{model.StripeMeterReportStateSending})
		connect.AssertEqual(t, world.states(model.TeamMeterTransferOverage, day2), []model.StripeMeterReportState{model.StripeMeterReportStateQueued})
		close(release)
		var result *ReportStripeMetersResult
		select {
		case result = <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("the first pass did not end")
		}
		connect.AssertEqual(t, result != nil, true)
		connect.AssertEqual(t, result.SentCount, 2)
		connect.AssertEqual(t, world.stripe.maxInFlight, 1)
		connect.AssertEqual(t, world.kept(model.TeamMeterTransferOverage, day1), int64(1))
		connect.AssertEqual(t, world.kept(model.TeamMeterTransferOverage, day2), int64(2))

		// a sender that dies after its claim, its event taken by Stripe
		day3 := testDay(2026, time.November, 5)
		world.at(day3.AddDate(0, 0, 1).Add(10 * time.Minute))
		world.writeDay(day3, map[string]int64{model.TeamMeterTransferOverage: 3})
		claim, err := model.ClaimStripeMeterReports(ctx, world.now, nil, 10)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(claim.Sends), 1)
		dead := claim.Sends[0]
		connect.AssertEqual(t, world.stripe.SendMeterEvent(ctx, &stripeMeterEvent{
			EventName:  model.TeamMeterTransferOverage,
			CustomerId: dead.CustomerId,
			Identifier: dead.Report.Identifier,
			Value:      dead.Report.Value,
			Timestamp:  dead.Timestamp,
		}), model.StripeMeterOutcomeSent)
		sendCount := world.stripe.sendCount()
		// its claim holds for ClaimTimeout
		world.at(world.now.Add(9 * time.Minute))
		world.pass()
		connect.AssertEqual(t, world.stripe.sendCount(), sendCount)
		// then the row is sent again as it was, and Stripe refuses the duplicate
		world.at(world.now.Add(2 * time.Minute))
		connect.AssertEqual(t, world.pass().SentCount, 1)
		resent := world.stripe.sentEvents()[sendCount]
		connect.AssertEqual(t, resent.Identifier, dead.Report.Identifier)
		connect.AssertEqual(t, resent.Value, int64(3))
		connect.AssertEqual(t, len(world.stripe.dayReceipts(world.customerId, model.TeamMeterTransferOverage, day3)), 1)
		// the dead sender's record arrives late and is dropped
		recorded, err := model.RecordStripeMeterOutcome(ctx, dead, model.StripeMeterOutcomeFailed, world.now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, recorded, false)
		connect.AssertEqual(t, world.states(model.TeamMeterTransferOverage, day3), []model.StripeMeterReportState{model.StripeMeterReportStateSent})
	})
}

func testInsertUsageRow(ctx context.Context, networkId server.Id, day time.Time, meter string, quantity int64) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_service_usage (network_id, usage_day, meter, quantity)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (network_id, usage_day, meter) DO UPDATE SET quantity = $4
			`,
			networkId,
			day,
			meter,
			quantity,
		))
	})
}

// FS-41's and FS-42's M6 meter step at model level: after a restore from a dump, rows written and
// sent since the dump are gone while Stripe still holds their events. A day rolled up again under
// version 1 with what the restored balances give meets Stripe's dedup of the lost row's identifier
// and leaves the old value there; RecomputeStripeMeterReports writes the day again under a version
// from the clock, whose event replaces it. A row the dump caught mid-send is sent again as it was.
// No day is billed twice, a rerun writes nothing, and debits lost with the dump go unbilled.
func TestRecomputeStripeMeterReportsResendsWithoutDoubleBilling(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		gib := int64(model.Gib)
		start := time.Date(2026, time.November, 1, 15, 0, 0, 0, time.UTC)
		anchor := testDay(2026, time.November, 2)
		day1 := testDay(2026, time.November, 3)
		day2 := testDay(2026, time.November, 4)
		world := newStripeMeterTestWorld(t, ctx, start, anchor)
		transfer := model.TeamMeterTransferOverage

		// before the dump: day1 is rolled up and sent; day2 likewise, 3 GiB of overage
		world.at(day2.Add(10 * time.Minute))
		world.writeDay(day1, map[string]int64{model.TeamMeterSeats: 4, transfer: 2})
		connect.AssertEqual(t, world.pass().SentCount, 2)
		world.at(day2.AddDate(0, 0, 1).Add(10 * time.Minute))
		world.writeDay(day2, map[string]int64{model.TeamMeterSeats: 4, transfer: 3})
		connect.AssertEqual(t, world.pass().SentCount, 2)
		connect.AssertEqual(t, world.kept(transfer, day2), int64(3))

		// the restore, to a dump taken as day1's transfer row was being sent and before day2 was
		// rolled up: day2's meter rows are gone, day1's transfer row is sending again, and the
		// usage rows are the dump's, where a GiB of day2's debits is lost
		dumpTime := day2.Add(10 * time.Minute)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM stripe_meter_report WHERE network_id = $1 AND usage_day = $2`, world.networkId, day2))
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE stripe_meter_report SET state = $3, next_time = $4 WHERE network_id = $1 AND usage_day = $2 AND meter = $5`,
				world.networkId, day1, string(model.StripeMeterReportStateSending), dumpTime.Add(10*time.Minute), transfer,
			))
		})
		testInsertUsageRow(ctx, world.networkId, day1, model.TeamMeterSeats, 4)
		testInsertUsageRow(ctx, world.networkId, day1, model.TeamMeterCreditRunning, 2*gib)
		testInsertUsageRow(ctx, world.networkId, day1, transfer, 2*gib)
		testInsertUsageRow(ctx, world.networkId, day2, model.TeamMeterSeats, 5)
		testInsertUsageRow(ctx, world.networkId, day2, model.TeamMeterCreditRunning, 4*gib)
		testInsertUsageRow(ctx, world.networkId, day2, transfer, 2*gib)

		// the rollup's own rerun of day2 writes version 1 again: Stripe refuses its identifier as
		// the lost row's duplicate, so the row reads sent while Stripe keeps the old value
		world.at(world.now.Add(time.Hour))
		world.writeDay(day2, map[string]int64{model.TeamMeterSeats: 5, transfer: 2})
		result := world.pass()
		connect.AssertEqual(t, result.SentCount, 3)
		connect.AssertEqual(t, world.states(transfer, day1), []model.StripeMeterReportState{model.StripeMeterReportStateSent})
		connect.AssertEqual(t, world.kept(transfer, day1), int64(2))
		connect.AssertEqual(t, world.kept(transfer, day2), int64(3))
		connect.AssertEqual(t, world.kept(model.TeamMeterSeats, day2), int64(4))

		// the billing step
		recompute, err := model.Testing_RecomputeStripeMeterReports(ctx, dumpTime, world.now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, recompute.DayCount, 1)
		connect.AssertEqual(t, recompute.WrittenCount, 2)
		rows := world.rows(transfer, day2)
		connect.AssertEqual(t, len(rows), 2)
		connect.AssertEqual(t, rows[1].Value, int64(2))
		connect.AssertEqual(t, rows[1].State, model.StripeMeterReportStateQueued)
		connect.AssertEqual(t, 1000 < rows[1].Version, true)
		connect.AssertEqual(t, rows[1].Identifier, model.StripeMeterIdentifier(world.networkId, transfer, day2, rows[1].Version))
		// day1 is before the dump's day and is left as it is
		connect.AssertEqual(t, len(world.rows(transfer, day1)), 1)

		connect.AssertEqual(t, world.pass().SentCount, 2)
		connect.AssertEqual(t, world.kept(transfer, day2), int64(2))
		connect.AssertEqual(t, world.kept(model.TeamMeterSeats, day2), int64(5))
		// the period's quantity is each day's newest value once: 2 + 2, the lost GiB unbilled
		connect.AssertEqual(t, world.stripe.billedSum(world.customerId, transfer, anchor, testDay(2026, time.December, 2), world.now), int64(4))

		// a rerun of the step, and a later pass, change nothing
		world.at(world.now.Add(time.Hour))
		recompute, err = model.Testing_RecomputeStripeMeterReports(ctx, dumpTime, world.now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, recompute.WrittenCount, 0)
		sendCount := world.stripe.sendCount()
		world.pass()
		connect.AssertEqual(t, world.stripe.sendCount(), sendCount)

		// a day with no usage rows left is rolled up by the step itself
		day3 := testDay(2026, time.November, 5)
		world.at(day3.AddDate(0, 0, 1).Add(time.Hour))
		recompute, err = model.Testing_RecomputeStripeMeterReports(ctx, day3.Add(time.Hour), world.now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, recompute.RolledUpCount, 1)
		connect.AssertEqual(t, 0 < len(world.rows(model.TeamMeterSeats, day3)), true)

		// a day whose invoice has finalized is passed over
		world.at(testDay(2026, time.December, 6))
		recompute, err = model.Testing_RecomputeStripeMeterReports(ctx, day1, world.now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, 0 < recompute.ClosedCount, true)
		connect.AssertEqual(t, len(world.rows(transfer, day1)), 1)
	})
}
