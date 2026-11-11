package model

// ----- Pro grants per subscription cycle (server/PRO-GRANT-CYCLE.md) -----
//
// Owner, 2026-11-11: "the subscription start is the first date of the
// network's subscription cycle. Each network has its own subscription cycle."
//
// A network's Pro data comes in cycles of its own. A cycle starts when a paid
// subscription period starts: Stripe's billing period, an App Store or Play
// renewal period, a Gamma or one-time crypto term, a trial. It repeats monthly
// from that start until the period ends. At the start of each cycle the network
// gets the Pro plan's full grant (pro.yml pro.data), valid until the cycle ends
// plus ProGrantGrace. The cycles of a network form one chain, whatever markets
// pay for them. A cycle is keyed by network and cycle start in pro_grant_cycle,
// so the renewal paths and the hourly sweep grant each cycle once.

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// How short a cycle may be. A paid period shorter than this gets one cycle this
// long; these are the stores' accelerated test renewals (minutes in the App
// Store sandbox and for Play license testers). A boundary closer than this to a
// cycle's start, or to its period's end, is skipped, so a period never ends in a
// sliver of a cycle that carries a full grant.
const ProCycleMinDuration = 7 * 24 * time.Hour

// How far back the renewal rows are read past the chain's head: enough for a
// Play row's previous period, a month earlier.
const proCycleRenewalHistory = 400 * 24 * time.Hour

// Bounds the cycles stepped over when the chain lags far behind now.
const proCycleMaxSteps = 1000

// The grace on the end of renewal rows written before paid_end_time was
// recorded. These are copies of controller.SubscriptionGracePeriod and
// controller.manualPaymentGracePeriod, which the model cannot import.
const legacyRenewalStoreGrace = 24 * time.Hour
const legacyRenewalManualPaymentGrace = 30 * 24 * time.Hour

// What every activation path credited before cycles: a Pro balance spanning the
// paid period.
const legacySupporterActivationByteCount = 600 * Gib

// One paid supporter period of a network: what a renewal row's payment or trial
// covers, without the market's grace.
type ProPaidPeriod struct {
	StartTime  time.Time
	EndTime    time.Time
	NetRevenue NanoCents
}

func (self *ProPaidPeriod) covers(t time.Time) bool {
	return !t.Before(self.StartTime) && t.Before(self.EndTime)
}

// Whether t is one of the period's monthly boundaries: its start plus whole
// months.
func (self *ProPaidPeriod) onBoundary(t time.Time) bool {
	for months := 0; months <= proCycleMaxSteps; months++ {
		boundary := addProCycleMonths(self.StartTime, months)
		if !boundary.Before(t) {
			return boundary.Equal(t)
		}
	}
	return false
}

// One cycle of a network's chain.
type ProCycle struct {
	StartTime time.Time
	EndTime   time.Time
}

// The anchor plus whole months, at the anchor's time of day. The day is clamped
// to the target month's last day (Jan 31 -> Feb 28 -> Mar 31), as Stripe bills a
// monthly anchor.
func addProCycleMonths(anchor time.Time, months int) time.Time {
	anchor = anchor.UTC()
	year, month, day := anchor.Date()
	hour, minute, second := anchor.Clock()
	firstOfMonth := time.Date(year, month+time.Month(months), 1, 0, 0, 0, 0, time.UTC)
	lastDay := firstOfMonth.AddDate(0, 1, -1).Day()
	return time.Date(
		firstOfMonth.Year(),
		firstOfMonth.Month(),
		min(day, lastDay),
		hour,
		minute,
		second,
		anchor.Nanosecond(),
		time.UTC,
	)
}

// The end of the cycle that starts at `start` in `period`. That is the first of
// the period's monthly boundaries at least ProCycleMinDuration after `start`.
// When that boundary would leave less than ProCycleMinDuration of the period,
// the cycle runs to the period's end. A period shorter than ProCycleMinDuration
// gets one cycle that long.
func proCycleEnd(period ProPaidPeriod, start time.Time) time.Time {
	if period.EndTime.Sub(period.StartTime) < ProCycleMinDuration {
		return start.Add(ProCycleMinDuration)
	}
	for months := 1; months <= proCycleMaxSteps; months++ {
		boundary := addProCycleMonths(period.StartTime, months)
		if !boundary.Before(period.EndTime) || period.EndTime.Sub(boundary) < ProCycleMinDuration {
			break
		}
		if ProCycleMinDuration <= boundary.Sub(start) {
			return boundary
		}
	}
	return period.EndTime
}

// The period a cycle starting at `start` is cut from. It is a period covering
// `start` whose boundaries include it, so a running chain keeps its own
// boundaries when a second market overlaps it. Otherwise it is the
// earliest-starting period covering `start`. nil when none covers it.
func proCycleAnchorPeriod(periods []ProPaidPeriod, start time.Time) *ProPaidPeriod {
	var earliest *ProPaidPeriod
	for i := range periods {
		period := &periods[i]
		if !period.covers(start) {
			continue
		}
		if period.onBoundary(start) {
			return period
		}
		if earliest == nil || period.StartTime.Before(earliest.StartTime) {
			earliest = period
		}
	}
	return earliest
}

// NextProCycle is the network's cycle that is due at `now`, given its paid
// periods and its chain's head. The head is the end of its latest cycle, or of
// its legacy grant's period; it is zero when there is neither. ok is false when
// no cycle is due: the head is still ahead, or no paid period covers now.
//
// The chain continues at the head when a paid period covers the head. After a
// lapse, or with no chain yet, a new chain starts at the start of the earliest
// paid period covering now. Cycles that ended without a grant (the sweep was
// down) are stepped over and never granted late, so the cycle returned always
// contains now.
func NextProCycle(periods []ProPaidPeriod, head time.Time, now time.Time) (cycle ProCycle, ok bool) {
	if now.Before(head) {
		return ProCycle{}, false
	}
	start := head
	for step := 0; step < proCycleMaxSteps; step++ {
		period := proCycleAnchorPeriod(periods, start)
		if period == nil {
			// no chain yet, or a lapse: a new chain from the earliest period
			// covering now
			for i := range periods {
				if periods[i].covers(now) && (period == nil || periods[i].StartTime.Before(period.StartTime)) {
					period = &periods[i]
				}
			}
			if period == nil {
				return ProCycle{}, false
			}
			start = period.StartTime
		}
		end := proCycleEnd(*period, start)
		if now.Before(end) {
			return ProCycle{StartTime: start, EndTime: end}, true
		}
		start = end
	}
	return ProCycle{}, false
}

// The subsidy revenue of a cycle's grant. Each period's revenue is pro-rated
// by the part of the period the cycle covers, so the cycles of a period carry
// its revenue once between them. A network paying in two markets at once
// carries both.
func proCycleSubsidyNetRevenue(periods []ProPaidPeriod, cycle ProCycle) NanoCents {
	subsidyNetRevenue := 0.0
	for _, period := range periods {
		duration := period.EndTime.Sub(period.StartTime)
		if period.NetRevenue <= 0 || duration <= 0 {
			continue
		}
		overlap := server.MinTime(period.EndTime, cycle.EndTime).Sub(server.MaxTime(period.StartTime, cycle.StartTime))
		if overlap <= 0 {
			continue
		}
		subsidyNetRevenue += float64(period.NetRevenue) * float64(overlap) / float64(duration)
	}
	return NanoCents(subsidyNetRevenue)
}

// A supporter renewal row as the cycles read it.
type proRenewalRow struct {
	startTime     time.Time
	endTime       time.Time
	paidEndTime   *time.Time
	market        SubscriptionMarket
	netRevenue    NanoCents
	purchaseToken string
	// For a row with no paid end, written before the column: whether it
	// credited a period. A market without billing grace holds always did. An
	// App Store or Play row did when its activation balance (positive bytes,
	// the same window) exists; a hold's marker has none.
	legacyCredit bool
}

// The grace a renewal row's end carries past its paid period, for a row
// written before paid_end_time was recorded. The App Store, Play and a manual
// upgrade have a day (controller.SubscriptionGracePeriod). Stripe, the crypto
// rails, x402 and gamma have the 30-day manual-payment grace
// (controller.manualPaymentGracePeriod, since 2026-09-26), or a day on older
// rows. Such a row is told by its length: taking off a manual-payment grace
// leaves a plan's length.
func legacyRenewalGrace(market SubscriptionMarket, startTime time.Time, endTime time.Time) time.Duration {
	switch market {
	case SubscriptionMarketApple, SubscriptionMarketGoogle, SubscriptionMarketManual, "":
		return legacyRenewalStoreGrace
	}
	if legacyPlanDuration(endTime.Sub(startTime) - legacyRenewalManualPaymentGrace) {
		return legacyRenewalManualPaymentGrace
	}
	return legacyRenewalStoreGrace
}

// Whether d is the length of a plan some market sold: a 14-day trial, a month,
// a year, or a year and a trial (the yearly welcome offer).
func legacyPlanDuration(d time.Duration) bool {
	day := 24 * time.Hour
	for _, span := range [][2]time.Duration{
		{12 * day, 16 * day},
		{27 * day, 32 * day},
		{363 * day, 368 * day},
		{377 * day, 382 * day},
	} {
		if span[0] <= d && d <= span[1] {
			return true
		}
	}
	return false
}

// The paid periods of a network's renewal rows. A billing grace hold is left
// out: it keeps Pro, but pays for no period. So is an x402 month, whose own Pro
// balance carries its data (controller.x402GrantProMonth). A Play row starts at
// the subscription's first start, so each Play row spans every period paid so
// far. Its own period starts where the purchase's previous paid period ended.
func proPaidPeriods(rows []proRenewalRow) []ProPaidPeriod {
	periods := []ProPaidPeriod{}
	playPeriodIndexes := map[string][]int{}
	for _, row := range rows {
		if row.market == SubscriptionMarketX402 {
			continue
		}
		var paidEndTime time.Time
		switch {
		case row.paidEndTime != nil:
			// an end path (refund, revoke, reconcile cut) ends the row early
			paidEndTime = server.MinTime(*row.paidEndTime, row.endTime)
		case row.legacyCredit:
			paidEndTime = row.endTime.Add(-legacyRenewalGrace(row.market, row.startTime, row.endTime))
		default:
			continue
		}
		if !row.startTime.Before(paidEndTime) {
			continue
		}
		if row.market == SubscriptionMarketGoogle && row.purchaseToken != "" {
			playPeriodIndexes[row.purchaseToken] = append(playPeriodIndexes[row.purchaseToken], len(periods))
		}
		periods = append(periods, ProPaidPeriod{
			StartTime:  row.startTime,
			EndTime:    paidEndTime,
			NetRevenue: row.netRevenue,
		})
	}
	for _, indexes := range playPeriodIndexes {
		slices.SortFunc(indexes, func(a int, b int) int {
			return periods[a].EndTime.Compare(periods[b].EndTime)
		})
		for i := 1; i < len(indexes); i++ {
			previousEndTime := periods[indexes[i-1]].EndTime
			period := &periods[indexes[i]]
			if period.StartTime.Before(previousEndTime) && previousEndTime.Before(period.EndTime) {
				period.StartTime = previousEndTime
			}
		}
	}
	return periods
}

// The cycle a network is due at `now`, read in the caller's tx. head is where the
// network's chain may continue: the end of its latest cycle, or of the month of
// its newest legacy calendar grant (a Pro balance of kind pro not written for a
// cycle: the 1st-of-month grant, or the old refresh's), whichever is later.
type proCycleState struct {
	periods []ProPaidPeriod
	head    time.Time
}

func readProCycleStateInTx(tx server.PgTx, ctx context.Context, networkId server.Id, now time.Time) *proCycleState {
	state := &proCycleState{}

	// a cycle ended early with its period (ended_time) heads the chain where it
	// ended, so a later subscription starts a new chain
	var latestCycleEndTime *time.Time
	server.Raise(tx.QueryRow(
		ctx,
		`
			SELECT MAX(COALESCE(ended_time, cycle_end_time))
			FROM pro_grant_cycle
			WHERE network_id = $1
		`,
		networkId,
	).Scan(&latestCycleEndTime))
	if latestCycleEndTime != nil {
		state.head = *latestCycleEndTime
	}

	// a calendar grant's month ends a day before the balance (ProGrantGrace)
	var legacyGrantEndTime *time.Time
	server.Raise(tx.QueryRow(
		ctx,
		`
			SELECT MAX(balance.end_time)
			FROM transfer_balance balance
			WHERE
				balance.network_id = $1 AND
				balance.pro AND
				balance.grant_kind = $2 AND
				NOT EXISTS (
					SELECT 1
					FROM pro_grant_cycle cycle
					WHERE
						cycle.network_id = balance.network_id AND
						cycle.balance_id = balance.balance_id
				)
		`,
		networkId,
		GrantKindPro,
	).Scan(&legacyGrantEndTime))
	if legacyGrantEndTime != nil {
		if legacyPeriodEndTime := legacyGrantEndTime.Add(-ProGrantGrace); state.head.Before(legacyPeriodEndTime) {
			state.head = legacyPeriodEndTime
		}
	}

	rows := []proRenewalRow{}
	result, err := tx.Query(
		ctx,
		`
			SELECT
				renewal.start_time,
				renewal.end_time,
				renewal.paid_end_time,
				COALESCE(renewal.market, ''),
				renewal.net_revenue_nano_cents,
				COALESCE(renewal.purchase_token, ''),
				(
					renewal.paid_end_time IS NULL AND (
						COALESCE(renewal.market, '') NOT IN ($3, $4) OR
						EXISTS (
							SELECT 1
							FROM transfer_balance balance
							WHERE
								balance.network_id = renewal.network_id AND
								balance.pro AND
								balance.start_time = renewal.start_time AND
								balance.end_time = renewal.end_time AND
								0 < balance.start_balance_byte_count
						)
					)
				)
			FROM subscription_renewal renewal
			WHERE
				renewal.network_id = $1 AND
				renewal.subscription_type = $2 AND
				$5 < renewal.end_time
		`,
		networkId,
		SubscriptionTypeSupporter,
		SubscriptionMarketApple,
		SubscriptionMarketGoogle,
		server.MinTime(state.head, now).Add(-proCycleRenewalHistory),
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			row := proRenewalRow{}
			server.Raise(result.Scan(
				&row.startTime,
				&row.endTime,
				&row.paidEndTime,
				&row.market,
				&row.netRevenue,
				&row.purchaseToken,
				&row.legacyCredit,
			))
			rows = append(rows, row)
		}
	})
	state.periods = proPaidPeriods(rows)
	return state
}

// The lock that serializes one network's cycle grants: the renewal paths and the
// sweep.
func lockProGrantCycleInTx(tx server.PgTx, ctx context.Context, networkId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		fmt.Sprintf("pro_grant_cycle/%s", networkId),
	))
}

// Whether the tx reads at ReadCommitted, which the cycle grant needs: it reads
// the chain after taking the network's lock, so it must see what an earlier
// holder committed. A RepeatableRead snapshot from before the lock would not.
func isReadCommittedInTx(tx server.PgTx, ctx context.Context) bool {
	var isolation string
	server.Raise(tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&isolation))
	return isolation == "read committed"
}

// GrantProCycleInTx grants the network the Pro plan's full grant for its cycle
// that is due now, if one is due and not yet granted. It does this in the
// caller's ReadCommitted tx, beside the renewal that tx wrote. Every credit
// path calls it after adding its renewal, and the hourly sweep calls it for
// networks whose chain has no running cycle. granted is true when it wrote a
// Pro grant. The caller must then refresh the Pro cache (UpdateProNetwork) once
// the tx commits.
//
// Nothing is granted with no pro.yml (amount zero), or in a tx at another
// isolation. The sweep grants it within the hour.
func GrantProCycleInTx(tx server.PgTx, ctx context.Context, networkId server.Id) (granted bool) {
	return grantProCycleInTx(tx, ctx, networkId, server.NowUtc())
}

// GrantProCycleInTx at a given time, for tests that walk a network through its
// cycles.
func Testing_GrantProCycleInTx(tx server.PgTx, ctx context.Context, networkId server.Id, now time.Time) (granted bool) {
	return grantProCycleInTx(tx, ctx, networkId, now)
}

func grantProCycleInTx(tx server.PgTx, ctx context.Context, networkId server.Id, now time.Time) (granted bool) {
	byteCount := Pro().DataAmount(true)
	if byteCount <= 0 {
		glog.Errorf("[sub]no Pro data amount configured (is pro.yml present?); skipping the cycle grant for %s\n", networkId)
		return false
	}
	if !isReadCommittedInTx(tx, ctx) {
		// the App Store offer-code binding credits at RepeatableRead
		glog.Infof("[sub]the cycle grant for %s needs a ReadCommitted tx; leaving it to the sweep\n", networkId)
		return false
	}

	lockProGrantCycleInTx(tx, ctx, networkId)
	state := readProCycleStateInTx(tx, ctx, networkId, now)
	cycle, ok := NextProCycle(state.periods, state.head, now)
	if !ok {
		return false
	}

	// A cycle that continues the chain and ends inside the one-day grace of the
	// grant before it is covered by that grant: the hours between a legacy
	// grant's month and a cycle boundary just after it, or the sliver of a
	// second market's period that ends just after the first's. It is recorded
	// without a grant, so the next cycle gets one.
	var balanceId *server.Id
	if !cycle.StartTime.Equal(state.head) || state.head.Add(ProGrantGrace).Before(cycle.EndTime) {
		id := server.NewId()
		balanceId = &id
	}

	cycleTag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO pro_grant_cycle (
				network_id,
				cycle_start_time,
				cycle_end_time,
				balance_id
			)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT DO NOTHING
		`,
		networkId,
		cycle.StartTime,
		cycle.EndTime,
		balanceId,
	))
	if cycleTag.RowsAffected() == 0 || balanceId == nil {
		return false
	}

	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO transfer_balance (
				balance_id,
				network_id,
				start_time,
				end_time,
				start_balance_byte_count,
				net_revenue_nano_cents,
				subsidy_net_revenue_nano_cents,
				balance_byte_count,
				pro,
				grant_kind
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $5, true, $8)
		`,
		*balanceId,
		networkId,
		cycle.StartTime,
		cycle.EndTime.Add(ProGrantGrace),
		byteCount,
		NanoCents(0),
		proCycleSubsidyNetRevenue(state.periods, cycle),
		GrantKindPro,
	))
	return true
}

// The end of the period a supporter renewal row keeps open at the time it is
// read, for deciding whether a network still holds any subscription. A row
// with a paid end is open to the earlier of its paid end and its end (an end
// path cuts end_time). A hold, or a row written before paid_end_time, is open
// to its end, grace included, so that no grant is taken from a network that may
// still be paying.
func proRenewalOpenEndSql(alias string) string {
	return fmt.Sprintf(
		`CASE WHEN %[1]s.paid_end_time IS NULL THEN %[1]s.end_time ELSE LEAST(%[1]s.paid_end_time, %[1]s.end_time) END`,
		alias,
	)
}

// The networks the sweep looks at for a grant. Each has a supporter renewal
// window open at `now` (grace included) and no running cycle. It also holds no
// Pro grant whose period runs past now: a cycle's, or a legacy calendar
// grant's. Most networks are mid-cycle and are left out here, at no cost.
func proCycleGrantCandidates(ctx context.Context, now time.Time) (networkIds []server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT DISTINCT renewal.network_id
				FROM subscription_renewal renewal
				WHERE
					renewal.subscription_type = $1 AND
					renewal.start_time <= $2 AND
					$2 < renewal.end_time AND
					NOT EXISTS (
						SELECT 1
						FROM pro_grant_cycle cycle
						WHERE
							cycle.network_id = renewal.network_id AND
							$2 < COALESCE(cycle.ended_time, cycle.cycle_end_time)
					) AND
					NOT EXISTS (
						SELECT 1
						FROM transfer_balance balance
						WHERE
							balance.network_id = renewal.network_id AND
							balance.pro AND
							balance.grant_kind = $3 AND
							balance.start_time <= $2 AND
							-- the grant's period, its end less ProGrantGrace,
							-- runs past now
							$4 < balance.end_time
					)
			`,
			SubscriptionTypeSupporter,
			now,
			GrantKindPro,
			now.Add(ProGrantGrace),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				server.Raise(result.Scan(&networkId))
				networkIds = append(networkIds, networkId)
			}
		})
	})
	return
}

// A running cycle whose grant is still active while the network holds no open
// supporter renewal.
type proCycleClawback struct {
	networkId      server.Id
	cycleStartTime time.Time
	balanceId      server.Id
}

// The running cycles whose network has no supporter renewal open at `now` (see
// proRenewalOpenEndSql). The end paths (EndReconciledEntitlement*) end these with
// the period; this catches a renewal ended any other way. A cycle that runs out
// on its own (the last one of a period) has ended before its grant's one-day
// grace, so it is never here.
func proCycleClawbackCandidates(ctx context.Context, now time.Time) (clawbacks []proCycleClawback) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					cycle.network_id,
					cycle.cycle_start_time,
					cycle.balance_id
				FROM pro_grant_cycle cycle
				INNER JOIN transfer_balance balance ON
					balance.balance_id = cycle.balance_id
				WHERE
					cycle.ended_time IS NULL AND
					$1 < cycle.cycle_end_time AND
					$1 < balance.end_time AND
					NOT EXISTS (
						SELECT 1
						FROM subscription_renewal renewal
						WHERE
							renewal.network_id = cycle.network_id AND
							renewal.subscription_type = $2 AND
							renewal.start_time <= $1 AND
							$1 < `+proRenewalOpenEndSql("renewal")+`
					)
			`,
			now,
			SubscriptionTypeSupporter,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				clawback := proCycleClawback{}
				server.Raise(result.Scan(
					&clawback.networkId,
					&clawback.cycleStartTime,
					&clawback.balanceId,
				))
				clawbacks = append(clawbacks, clawback)
			}
		})
	})
	return
}

// Ends a clawed-back cycle's grant at `now` and marks the cycle ended there, so
// a later subscription starts a new chain. The grant is ended under
// transfer-balance ownership, like the end paths end a period's balance. ended
// is false when the state changed since the scan, or another writer owns the
// balance (a later sweep retries it).
func endProCycleGrant(ctx context.Context, clawback proCycleClawback, now time.Time) (ended bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		ended = false
		lockProGrantCycleInTx(tx, ctx, clawback.networkId)
		var stillDue bool
		server.Raise(tx.QueryRow(
			ctx,
			`
				SELECT EXISTS (
					SELECT 1
					FROM pro_grant_cycle cycle
					INNER JOIN transfer_balance balance ON
						balance.balance_id = cycle.balance_id
					WHERE
						cycle.network_id = $1 AND
						cycle.cycle_start_time = $2 AND
						cycle.balance_id = $3 AND
						cycle.ended_time IS NULL AND
						$4 < cycle.cycle_end_time AND
						$4 < balance.end_time
				) AND NOT EXISTS (
					SELECT 1
					FROM subscription_renewal renewal
					WHERE
						renewal.network_id = $1 AND
						renewal.subscription_type = $5 AND
						renewal.start_time <= $4 AND
						$4 < `+proRenewalOpenEndSql("renewal")+`
				)
			`,
			clawback.networkId,
			clawback.cycleStartTime,
			clawback.balanceId,
			now,
			SubscriptionTypeSupporter,
		).Scan(&stillDue))
		if !stillDue {
			return
		}
		admitted, err := tryTransferBalanceOwnershipInTx(ctx, tx, []server.Id{clawback.balanceId})
		server.Raise(err)
		if !admitted {
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE transfer_balance
				SET end_time = $2
				WHERE balance_id = $1 AND $2 < end_time
			`,
			clawback.balanceId,
			now,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE pro_grant_cycle
				SET ended_time = $3
				WHERE network_id = $1 AND cycle_start_time = $2 AND ended_time IS NULL
			`,
			clawback.networkId,
			clawback.cycleStartTime,
			now,
		))
		ended = true
	}, server.TxReadCommitted, server.OptNoRetry())
	return
}

// GrantDueProCycles is the hourly sweep: it grants every network the cycle that
// is due now and that no renewal path granted. This covers the monthly cycles
// inside a yearly period, a renewal recorded by a path that did not grant (an
// old binary in a rollout, a late webhook caught by a reconciler or poll), and
// a grant skipped for want of pro.yml. Then it claws back the running cycles
// whose period was ended early. It returns the networks granted and those
// clawed back, whose Pro cache it has refreshed.
func GrantDueProCycles(ctx context.Context) (grantedNetworkIds []server.Id, endedNetworkIds []server.Id) {
	return grantDueProCycles(ctx, server.NowUtc())
}

// GrantDueProCycles at a given time, for tests that walk networks through their
// cycles.
func Testing_GrantDueProCycles(ctx context.Context, now time.Time) (grantedNetworkIds []server.Id, endedNetworkIds []server.Id) {
	return grantDueProCycles(ctx, now)
}

func grantDueProCycles(ctx context.Context, now time.Time) (grantedNetworkIds []server.Id, endedNetworkIds []server.Id) {
	grantedNetworkIds = []server.Id{}
	endedNetworkIds = []server.Id{}
	for _, networkId := range proCycleGrantCandidates(ctx, now) {
		granted := false
		server.Tx(ctx, func(tx server.PgTx) {
			granted = false
			if err := LockPaymentNetworkInTx(tx, ctx, networkId); err != nil {
				// deleted since the scan
				return
			}
			granted = grantProCycleInTx(tx, ctx, networkId, now)
		}, server.TxReadCommitted)
		if granted {
			UpdateProNetwork(ctx, networkId)
			grantedNetworkIds = append(grantedNetworkIds, networkId)
		}
	}
	for _, clawback := range proCycleClawbackCandidates(ctx, now) {
		if endProCycleGrant(ctx, clawback, now) {
			UpdateProNetwork(ctx, clawback.networkId)
			endedNetworkIds = append(endedNetworkIds, clawback.networkId)
		}
	}
	return
}
