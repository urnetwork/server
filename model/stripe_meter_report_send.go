package model

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/server"
)

// The send side of stripe_meter_report (QUOTA-M6-04; DESIGN.md 20.9's Meters; D-QUOTA-8, FI-13(b),
// FS-43). controller.ReportStripeMeters claims rows here, sends each to Stripe and records what
// Stripe answered. Nothing here calls Stripe.
//
//	claim       a pass takes, for each (network, meter) with a due row, the pair's oldest due row
//	            (usage_day, version) that may be sent, under a lock on the pair's due rows, and moves
//	            it to sending with attempts + 1 and next_time the claim's end. At most one row a (network, meter) is sending at a time, which is
//	            Stripe's limit of one send in flight per (customer, meter): a Team's customer is its
//	            network's. A row whose claim ended with no outcome recorded (its sender died) is due
//	            again and is sent again with the same identifier and value, as an uncertain row is
//	versions    a row is claimed only once every earlier version of its day is sent, failed or missed,
//	            so the day's newest version is the event Stripe received last, which is the one a
//	            pre-aggregated daily meter keeps. A row held back this way is passed over, and the
//	            pair's later days are sent meanwhile
//	window      the event's timestamp is the later of EventDayOffset (12 h) into the usage day and
//	            EventStartMargin (1 s) after the subscription's start, so a partial first day lies
//	            inside the first period. The day's invoice is its period's: the period ends at the
//	            first billing anchor after the timestamp and the invoice finalizes InvoiceGrace (72 h)
//	            later. A row may be sent until its deadline: that finalization, or the subscription's
//	            end where that comes before the period's, and never past TimestampWindow (35 days)
//	            after the timestamp
//	deadline    a due row at or past its deadline is never sent: one never attempted is missed
//	            (unbilled, counted, never added to a later invoice); one attempted with no known
//	            outcome stays uncertain and is parked, its next_time past the row's retention, so no
//	            pass meets it again. A row with no subscription over its day is failed
//	outcome     sent, failed or uncertain, recorded against the claim's attempt number so a pass that
//	            lost its claim records nothing; uncertain is due again after a backoff that doubles
//	            from RetryBackoffMin to RetryBackoffMax
//	recompute   RecomputeStripeMeterReports, for §19.10's billing step after a restore: every closed
//	            day since the dump whose deadline has not passed gets its values again from
//	            network_service_usage (a day with no rows there is rolled up first) as a new version
//	            numbered from the clock, whose identifier no row lost with the dump can share, so the
//	            re-send replaces the day in Stripe and never meets Stripe's 24 h dedup of a lost
//	            row's identifier. Debits lost with the dump go unbilled
//
// Nothing here runs unless the binary links MG6.

type StripeMeterSendSettings struct {
	// How long a claim holds a row in sending before another pass may send it again: 10 min, above
	// the longest a pass can take over one page.
	ClaimTimeout time.Duration
	// Due rows of one (network, meter) a claim reads to find the one it takes: 64.
	PairScanCount int
	// The backoff of an uncertain row: RetryBackoffMin after its first attempt, doubling to
	// RetryBackoffMax.
	RetryBackoffMin time.Duration
	RetryBackoffMax time.Duration
	// How long after a period's end its invoice finalizes: 72 h, the invoice rule's delay and
	// Stripe's maximum.
	InvoiceGrace time.Duration
	// How far in the past Stripe takes an event's timestamp: 35 days.
	TimestampWindow time.Duration
	// A row unsent this long before its deadline alerts billing: 6 h.
	UnsentAlertLead time.Duration
	// The event's time within its usage day: 12 h.
	EventDayOffset time.Duration
	// How far after the subscription's start an event's timestamp is at the earliest: 1 s.
	EventStartMargin time.Duration
	// How long past its deadline a parked row's next_time is put: the rows' 13 months, so the
	// retention removes it first.
	ParkDuration time.Duration
	// The instant RecomputeStripeMeterReports numbers its versions from, in VersionClockUnit steps.
	VersionClockEpoch time.Time
	VersionClockUnit  time.Duration
}

func DefaultStripeMeterSendSettings() *StripeMeterSendSettings {
	return &StripeMeterSendSettings{
		ClaimTimeout:      10 * time.Minute,
		PairScanCount:     64,
		RetryBackoffMin:   time.Minute,
		RetryBackoffMax:   time.Hour,
		InvoiceGrace:      72 * time.Hour,
		TimestampWindow:   35 * 24 * time.Hour,
		UnsentAlertLead:   6 * time.Hour,
		EventDayOffset:    12 * time.Hour,
		EventStartMargin:  time.Second,
		ParkDuration:      400 * 24 * time.Hour,
		VersionClockEpoch: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
		VersionClockUnit:  time.Minute,
	}
}

var stripeMeterSendSettings = DefaultStripeMeterSendSettings()

// Replaces the process's send settings, returning a restore function. For tests.
func Testing_SetStripeMeterSendSettings(settings *StripeMeterSendSettings) func() {
	prev := stripeMeterSendSettings
	stripeMeterSendSettings = settings
	return func() {
		stripeMeterSendSettings = prev
	}
}

// When a usage day's event is stamped and until when it may be sent.
type StripeMeterSendWindow struct {
	// the event's timestamp
	Timestamp time.Time
	// the end of the period the timestamp lies in
	PeriodEnd time.Time
	// the row is sent before this or never
	Deadline time.Time
}

// The send window of usageDay under the subscription (the file header's window row).
func TeamStripeMeterSendWindow(subscription *TeamSubscription, usageDay time.Time) StripeMeterSendWindow {
	settings := stripeMeterSendSettings
	day := networkUsageDay(usageDay)
	timestamp := day.Add(settings.EventDayOffset)
	if earliest := subscription.StartTime.Add(settings.EventStartMargin); timestamp.Before(earliest) {
		timestamp = earliest
	}
	// the first billing anchor after the timestamp: the anchor itself, then its day each month
	periodEnd := subscription.AnchorTime
	for !timestamp.Before(periodEnd) {
		periodEnd = teamPeriodEnd(subscription.AnchorTime.Day(), periodEnd)
	}
	deadline := periodEnd.Add(settings.InvoiceGrace)
	if subscription.EndTime != nil && subscription.EndTime.Before(periodEnd) {
		// the subscription's last invoice is cut when it ends
		deadline = *subscription.EndTime
	}
	if latest := timestamp.Add(settings.TimestampWindow); latest.Before(deadline) {
		deadline = latest
	}
	return StripeMeterSendWindow{
		Timestamp: timestamp,
		PeriodEnd: periodEnd,
		Deadline:  deadline,
	}
}

// The network's subscription that was live during usageDay, the newest when a day saw two, or nil.
func teamSubscriptionOfDayInTx(ctx context.Context, tx server.PgTx, networkId server.Id, usageDay time.Time) *TeamSubscription {
	day := networkUsageDay(usageDay)
	subscriptions := teamSubscriptionsInConn(
		ctx,
		tx,
		`
			SELECT `+teamSubscriptionColumns+`
			FROM team_subscription
			WHERE
				network_id = $1 AND
				start_time < $2 AND
				(end_time IS NULL OR $3 < end_time)
			ORDER BY start_time DESC, subscription_id DESC
			LIMIT 1
		`,
		networkId,
		day.AddDate(0, 0, 1),
		day,
	)
	if len(subscriptions) == 0 {
		return nil
	}
	return subscriptions[0]
}

// A row a pass claimed, with what its send carries.
type StripeMeterSend struct {
	// the row as claimed: sending, with this claim's attempt number
	Report     *StripeMeterReport
	CustomerId string
	Timestamp  time.Time
	Deadline   time.Time
}

// Where a claim's page starts: after this (network, meter).
type StripeMeterClaimCursor struct {
	NetworkId server.Id
	Meter     string
}

// What one claim did.
type ClaimStripeMeterReportsResult struct {
	Sends []*StripeMeterSend
	// rows moved to missed, by meter
	MissedCounts map[string]int
	// uncertain rows parked past their deadline, by meter
	ParkedCounts map[string]int
	// rows failed for want of a subscription over their day, by meter
	FailedCounts map[string]int
	// claimed rows within UnsentAlertLead of their deadline
	NearDeadlineCount int
	// due rows held back for an earlier version, and pairs passed over for a send in flight
	HeldCount     int
	InFlightCount int
	// the page's last (network, meter) when the page was full: the next claim starts after it
	Next *StripeMeterClaimCursor
}

// Claims at most pageCount rows due at now, one a (network, meter), in (network, meter) order after
// after (nil: from the start), as the file header's claim, versions and deadline rows say.
func ClaimStripeMeterReports(ctx context.Context, now time.Time, after *StripeMeterClaimCursor, pageCount int) (result ClaimStripeMeterReportsResult, returnErr error) {
	if !server.SidecarMg6Linked() {
		return result, ErrStripeMeterReportUnavailable
	}
	if pageCount <= 0 {
		return result, errors.New("a claim page of no rows")
	}
	settings := stripeMeterSendSettings
	now = now.UTC()
	var afterNetworkId *server.Id
	afterMeter := ""
	if after != nil {
		afterNetworkId = &after.NetworkId
		afterMeter = after.Meter
	}
	server.Tx(ctx, func(tx server.PgTx) {
		result = ClaimStripeMeterReportsResult{
			MissedCounts: map[string]int{},
			ParkedCounts: map[string]int{},
			FailedCounts: map[string]int{},
		}
		pairs := []StripeMeterClaimCursor{}
		rows, err := tx.Query(
			ctx,
			`
				SELECT DISTINCT network_id, meter
				FROM stripe_meter_report
				WHERE
					state IN ($1, $2, $3) AND
					next_time <= $4 AND
					($5::uuid IS NULL OR ($5::uuid, $6::varchar) < (network_id, meter))
				ORDER BY network_id, meter
				LIMIT $7
			`,
			string(StripeMeterReportStateQueued),
			string(StripeMeterReportStateSending),
			string(StripeMeterReportStateUncertain),
			now,
			afterNetworkId,
			afterMeter,
			pageCount,
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var pair StripeMeterClaimCursor
				server.Raise(rows.Scan(&pair.NetworkId, &pair.Meter))
				pairs = append(pairs, pair)
			}
		})
		if len(pairs) == pageCount {
			last := pairs[len(pairs)-1]
			result.Next = &last
		}
		for _, pair := range pairs {
			// the pair's due rows, oldest first, locked: the lock orders this claim against any
			// other's of the pair, which reads the rows again once this one commits
			due := stripeMeterReportsIn(
				ctx,
				tx,
				`
					SELECT `+stripeMeterReportColumns+`
					FROM stripe_meter_report
					WHERE
						network_id = $1 AND meter = $2 AND
						state IN ($3, $4, $5) AND
						next_time <= $6
					ORDER BY usage_day, version
					LIMIT $7
					FOR UPDATE
				`,
				pair.NetworkId,
				pair.Meter,
				string(StripeMeterReportStateQueued),
				string(StripeMeterReportStateSending),
				string(StripeMeterReportStateUncertain),
				now,
				settings.PairScanCount,
			)
			var inFlight bool
			server.Raise(tx.QueryRow(
				ctx,
				`
					SELECT EXISTS (
						SELECT 1
						FROM stripe_meter_report
						WHERE
							network_id = $1 AND meter = $2 AND
							state = $3 AND
							$4 < next_time
					)
				`,
				pair.NetworkId,
				pair.Meter,
				string(StripeMeterReportStateSending),
				now,
			).Scan(&inFlight))
			claimed := false
			for _, report := range due {
				subscription := teamSubscriptionOfDayInTx(ctx, tx, report.NetworkId, report.UsageDay)
				if subscription == nil {
					setStripeMeterReportStateInTx(ctx, tx, report, StripeMeterReportStateFailed, report.Attempts, report.NextTime)
					result.FailedCounts[report.Meter] += 1
					continue
				}
				window := TeamStripeMeterSendWindow(subscription, report.UsageDay)
				if !now.Before(window.Deadline) {
					if report.Attempts == 0 {
						setStripeMeterReportStateInTx(ctx, tx, report, StripeMeterReportStateMissed, report.Attempts, report.NextTime)
						result.MissedCounts[report.Meter] += 1
					} else {
						setStripeMeterReportStateInTx(ctx, tx, report, StripeMeterReportStateUncertain, report.Attempts, window.Deadline.Add(settings.ParkDuration))
						result.ParkedCounts[report.Meter] += 1
					}
					continue
				}
				if claimed || inFlight {
					// the pair has its one send; the rows past their deadline above are still put away
					continue
				}
				var earlierOpen bool
				server.Raise(tx.QueryRow(
					ctx,
					`
						SELECT EXISTS (
							SELECT 1
							FROM stripe_meter_report
							WHERE
								network_id = $1 AND meter = $2 AND usage_day = $3 AND version < $4 AND
								state NOT IN ($5, $6, $7)
						)
					`,
					report.NetworkId,
					report.Meter,
					report.UsageDay,
					report.Version,
					string(StripeMeterReportStateSent),
					string(StripeMeterReportStateFailed),
					string(StripeMeterReportStateMissed),
				).Scan(&earlierOpen))
				if earlierOpen {
					// held back, and the pair's later days are not held back with it
					result.HeldCount += 1
					continue
				}
				report.Attempts += 1
				report.State = StripeMeterReportStateSending
				report.NextTime = now.Add(settings.ClaimTimeout)
				setStripeMeterReportStateInTx(ctx, tx, report, report.State, report.Attempts, report.NextTime)
				if window.Deadline.Sub(now) <= settings.UnsentAlertLead {
					result.NearDeadlineCount += 1
				}
				result.Sends = append(result.Sends, &StripeMeterSend{
					Report:     report,
					CustomerId: subscription.CustomerId,
					Timestamp:  window.Timestamp,
					Deadline:   window.Deadline,
				})
				claimed = true
			}
			if inFlight {
				result.InFlightCount += 1
			}
		}
	}, server.TxReadCommitted)
	return
}

func setStripeMeterReportStateInTx(ctx context.Context, tx server.PgTx, report *StripeMeterReport, state StripeMeterReportState, attempts int, nextTime time.Time) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE stripe_meter_report
			SET state = $5, attempts = $6, next_time = $7
			WHERE network_id = $1 AND meter = $2 AND usage_day = $3 AND version = $4
		`,
		report.NetworkId,
		report.Meter,
		report.UsageDay,
		report.Version,
		string(state),
		attempts,
		nextTime.UTC(),
	))
}

// What Stripe's answer to a send means for its row (§20.9's outcomes).
type StripeMeterOutcome string

const (
	// 200, or a refusal naming a duplicate identifier
	StripeMeterOutcomeSent StripeMeterOutcome = "sent"
	// any other 4xx
	StripeMeterOutcomeFailed StripeMeterOutcome = "failed"
	// a timeout, 429 or 5xx
	StripeMeterOutcomeUncertain StripeMeterOutcome = "uncertain"
)

// When an uncertain row whose latest attempt was its attempts-th is due again, from now.
func stripeMeterRetryTime(now time.Time, attempts int) time.Time {
	settings := stripeMeterSendSettings
	backoff := settings.RetryBackoffMin
	for i := 1; i < attempts && backoff < settings.RetryBackoffMax; i += 1 {
		backoff *= 2
	}
	return now.Add(min(backoff, settings.RetryBackoffMax))
}

// Records the outcome of a claimed row's send at now. recorded is false when the claim was lost:
// the row is no longer sending under this claim's attempt number, and nothing is written.
func RecordStripeMeterOutcome(ctx context.Context, send *StripeMeterSend, outcome StripeMeterOutcome, now time.Time) (recorded bool, returnErr error) {
	if !server.SidecarMg6Linked() {
		return false, ErrStripeMeterReportUnavailable
	}
	now = now.UTC()
	var state StripeMeterReportState
	nextTime := now
	switch outcome {
	case StripeMeterOutcomeSent:
		state = StripeMeterReportStateSent
	case StripeMeterOutcomeFailed:
		state = StripeMeterReportStateFailed
	case StripeMeterOutcomeUncertain:
		state = StripeMeterReportStateUncertain
		nextTime = stripeMeterRetryTime(now, send.Report.Attempts)
	default:
		return false, errors.New("an unknown meter outcome")
	}
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE stripe_meter_report
				SET state = $5, next_time = $6
				WHERE
					network_id = $1 AND meter = $2 AND usage_day = $3 AND version = $4 AND
					state = $7 AND
					attempts = $8
			`,
			send.Report.NetworkId,
			send.Report.Meter,
			send.Report.UsageDay,
			send.Report.Version,
			string(state),
			nextTime,
			string(StripeMeterReportStateSending),
			send.Report.Attempts,
		))
		recorded = tag.RowsAffected() == 1
	}, server.TxReadCommitted)
	return
}

// The version a recompute at at numbers its rows from: the clock's steps since VersionClockEpoch,
// above every version a rollup or a correction writes and above an earlier recompute's.
func stripeMeterRecomputeVersion(at time.Time) int {
	settings := stripeMeterSendSettings
	return int(at.UTC().Sub(settings.VersionClockEpoch) / settings.VersionClockUnit)
}

// The meter values the day's network_service_usage rows give, as the rollup wrote them to
// stripe_meter_report. ok is false when the day has no rows.
func teamMeterValuesOfUsageInTx(ctx context.Context, tx server.PgTx, networkId server.Id, day time.Time) (meterValues map[string]int64, ok bool) {
	settings := teamUsageSettings
	day = networkUsageDay(day)
	quantities := map[string]int64{}
	result, err := tx.Query(
		ctx,
		`SELECT meter, quantity FROM network_service_usage WHERE network_id = $1 AND usage_day = $2`,
		networkId,
		day,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var meter string
			var quantity int64
			server.Raise(result.Scan(&meter, &quantity))
			quantities[meter] = quantity
		}
	})
	seatCount, ok := quantities[TeamMeterSeats]
	if !ok {
		return nil, false
	}
	meterValues = map[string]int64{TeamMeterSeats: seatCount}
	if _, priced := quantities[TeamMeterTransferOverage]; priced {
		running := quantities[TeamMeterCreditRunning]
		previousRunning := int64(0)
		if _, previous, found := teamUsagePreviousInTx(ctx, tx, networkId, TeamMeterCreditRunning, day); found {
			previousRunning = previous
		}
		meterValues[TeamMeterTransferOverage] = max(0,
			teamMeterUnits(running, settings.MeterUnitByteCount)-teamMeterUnits(previousRunning, settings.MeterUnitByteCount),
		)
	}
	if overage, priced := quantities[TeamMeterStorageOverage]; priced {
		meterValues[TeamMeterStorageOverage] = teamMeterUnits(overage, settings.MeterUnitByteCount)
	}
	return meterValues, true
}

type RecomputeStripeMeterReportsResult struct {
	// the Team days looked at, those rolled up for want of rows, and the rows written
	DayCount      int
	RolledUpCount int
	WrittenCount  int
	// the days passed over because their deadline has passed
	ClosedCount int
}

// §19.10's billing step (the file header's recompute row): for every Team whose subscription was
// live on a closed UTC day from sinceTime's day on, writes that day's meter values again as a new
// version, queued, unless the day's newest version already holds them under a version a recompute
// since sinceTime wrote, so a rerun writes nothing more. The sender then sends them in day order.
func RecomputeStripeMeterReports(ctx context.Context, sinceTime time.Time) (RecomputeStripeMeterReportsResult, error) {
	return recomputeStripeMeterReports(ctx, sinceTime, server.NowUtc())
}

// RecomputeStripeMeterReports at now. For tests.
func Testing_RecomputeStripeMeterReports(ctx context.Context, sinceTime time.Time, now time.Time) (RecomputeStripeMeterReportsResult, error) {
	return recomputeStripeMeterReports(ctx, sinceTime, now)
}

func recomputeStripeMeterReports(ctx context.Context, sinceTime time.Time, now time.Time) (result RecomputeStripeMeterReportsResult, returnErr error) {
	if !server.SidecarMg6Linked() {
		return result, ErrStripeMeterReportUnavailable
	}
	now = now.UTC()
	sinceDay := networkUsageDay(sinceTime)
	today := networkUsageDay(now)
	if !sinceDay.Before(today) {
		return result, nil
	}
	sinceVersion := stripeMeterRecomputeVersion(sinceTime)
	version := stripeMeterRecomputeVersion(now)
	if version < 2 || version <= sinceVersion {
		return result, errors.New("the recompute's clock gives no new version")
	}

	networkIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			ctx,
			`
				SELECT DISTINCT network_id
				FROM team_subscription
				WHERE start_time < $1 AND (end_time IS NULL OR $2 < end_time)
				ORDER BY network_id
			`,
			today,
			sinceDay,
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var networkId server.Id
				server.Raise(rows.Scan(&networkId))
				networkIds = append(networkIds, networkId)
			}
		})
	})
	for _, networkId := range networkIds {
		// oldest day first: a day's transfer value reads the day before's running sum
		for day := sinceDay; day.Before(today); day = day.AddDate(0, 0, 1) {
			server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					subscription := teamSubscriptionOfDayInTx(ctx, tx, networkId, day)
					if subscription == nil {
						return
					}
					result.DayCount += 1
					if window := TeamStripeMeterSendWindow(subscription, day); !now.Before(window.Deadline) {
						result.ClosedCount += 1
						return
					}
					meterValues, ok := teamMeterValuesOfUsageInTx(ctx, tx, networkId, day)
					if !ok {
						rollupTeamUsageDayInTx(ctx, tx, networkId, day, now)
						result.RolledUpCount += 1
						meterValues, ok = teamMeterValuesOfUsageInTx(ctx, tx, networkId, day)
						if !ok {
							server.Raise(errors.New("a rolled up day has no rows"))
						}
					}
					for _, meter := range TeamStripeMeters {
						value, ok := meterValues[meter]
						if !ok {
							continue
						}
						newest := newestStripeMeterReportInTx(ctx, tx, networkId, meter, day)
						if newest != nil && sinceVersion <= newest.Version && newest.Value == value {
							continue
						}
						rowVersion := version
						if newest != nil && rowVersion <= newest.Version {
							rowVersion = newest.Version + 1
						}
						server.RaisePgResult(tx.Exec(
							ctx,
							`
								INSERT INTO stripe_meter_report (`+stripeMeterReportColumns+`)
								VALUES ($1, $2, $3, $4, $5, $6, $7, 0, $8)
							`,
							networkId,
							meter,
							day,
							rowVersion,
							StripeMeterIdentifier(networkId, meter, day, rowVersion),
							value,
							string(StripeMeterReportStateQueued),
							now,
						))
						result.WrittenCount += 1
					}
				}, server.TxReadCommitted)
			}, func(err error) {
				returnErr = err
			})
			if returnErr != nil {
				return
			}
		}
	}
	return
}

// The newest version of each of the network's days from the day from on whose row is sent, by meter
// and day: what the audit holds Stripe's summaries against. A day whose newest version is not sent
// is left out, since what Stripe holds for it is not settled.
func GetSentStripeMeterValues(ctx context.Context, networkId server.Id, from time.Time) map[string]map[time.Time]int64 {
	values := map[string]map[time.Time]int64{}
	newest := map[string]map[time.Time]*StripeMeterReport{}
	for _, report := range GetStripeMeterReports(ctx, networkId, from) {
		if newest[report.Meter] == nil {
			newest[report.Meter] = map[time.Time]*StripeMeterReport{}
		}
		// by meter, day and version ascending: the last of a day is its newest
		newest[report.Meter][report.UsageDay] = report
	}
	for meter, days := range newest {
		for day, report := range days {
			if report.State != StripeMeterReportStateSent {
				continue
			}
			if values[meter] == nil {
				values[meter] = map[time.Time]int64{}
			}
			values[meter][day] = report.Value
		}
	}
	return values
}
