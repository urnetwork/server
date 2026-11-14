package model

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// Team's daily rows (QUOTA-M6-03; DESIGN.md 20.5's Seats, 20.9's Meters; R-QUOTA-6, D-QUOTA-8, FI-13,
// E-c; plan-QUOTA-overage-accounting, QD-090; plan-QUOTA-small-readings #11, QD-143). After a UTC day d
// closes, RollupTeamServiceUsage writes, for each Team network whose subscription was live during d
// (a team_subscription row, owner decision 199), these network_service_usage rows for d:
//
//	team_seats              network_member rows active within 30 days of d's close: a member any of whose
//	                        clients (network_client_owner.member_id) signed in (network_client.auth_time) in
//	                        that window, an always-on device or a member's agent keeping the seat billed;
//	                        Auditor and Billing admin never count, and an agent is no member
//	team_credit_window      W(d), hidden: the sum of used = start_balance_byte_count - balance_byte_count over
//	                        S(d), the network's team_credit balances that start before d's close and end
//	                        after it less 7 days
//	team_credit_running     C(d) = C(p) + U(d), hidden, p the newest earlier day with a window row
//	team_transfer_overage   U(d) = the used now of every line in S(p) or S(d), less W(p): the bytes the
//	                        line paid that the pass first sees on d, exact, whichever of the four paths
//	                        debited them (flushTransferDebitBalance, settleEscrowWithOptionsInTx's
//	                        synchronous branch, ReconcileContractAtDeadline's legacy branch,
//	                        FlushServiceDebits), each once, since only balance_byte_count is read
//	team_storage_overage    the day's stored bytes past the network's storage limit, exact; none while
//	                        storage_gib_month is unset
//
// and the day's stripe_meter_report rows (WriteStripeMeterRowsInTx) in whole units: seats as counted;
// transfer as round(C(d) / GiB) - round(C(p) / GiB), so rounding never accumulates; storage as the
// day's overage in whole GiB-days, rounded. A line's set reaches 7 days past its end so the debits of
// its last contracts, settled late, are still seen; a balance only falls (DESIGN-DETAILS 13.2.2), so
// U(d) is never negative but where a legacy settlement returned bytes, and then no meter value is.
//
// A day's rows are written once, in one transaction a network, the team_seats row's insert claiming
// the day: a rerun of a pass, or a pass killed and resumed from its cursor, writes nothing twice, and
// a closed day is never rewritten by a late debit, which counts on the day the pass first sees it.
// Only RecomputeStripeMeterReports (QUOTA-M6-04) writes a closed day's meter rows again, as new
// versions. Nothing here runs unless the binary links MG6.

// The meters plan-QUOTA-overage-accounting keeps for its own sums, which GET /network/usage hides.
const (
	TeamMeterCreditWindow  = "team_credit_window"
	TeamMeterCreditRunning = "team_credit_running"
)

// The roles whose members never take a seat (§20.5's Seats).
var teamSeatExcludedRoles = []NetworkMemberRole{NetworkMemberRoleAuditor, NetworkMemberRoleBillingAdmin}

type TeamUsageSettings struct {
	// How recently a member's client must have signed in for its seat to count: 30 days (R-QUOTA-6,
	// QD-143).
	SeatActiveWindow time.Duration
	// How long past its end a credit line stays in a day's set: 7 days (QD-090).
	CreditWindowMargin time.Duration
	// The unit the transfer and storage meters count in: a GiB.
	MeterUnitByteCount int64
	// How far back the newest earlier day with a window row is looked for: the rows' 13 months.
	PreviousDayHorizon time.Duration
}

func DefaultTeamUsageSettings() *TeamUsageSettings {
	return &TeamUsageSettings{
		SeatActiveWindow:   30 * 24 * time.Hour,
		CreditWindowMargin: 7 * 24 * time.Hour,
		MeterUnitByteCount: int64(Gib),
		PreviousDayHorizon: 400 * 24 * time.Hour,
	}
}

var teamUsageSettings = DefaultTeamUsageSettings()

// Replaces the process's Team usage settings, returning a restore function. For tests.
func Testing_SetTeamUsageSettings(settings *TeamUsageSettings) func() {
	prev := teamUsageSettings
	teamUsageSettings = settings
	return func() {
		teamUsageSettings = prev
	}
}

var ErrTeamUsageUnavailable = errors.New("Team usage needs MG6.")

// The network's seats at at: its members, Auditor and Billing admin aside, any of whose clients signed in
// within SeatActiveWindow before at.
func CountTeamSeatsInTx(ctx context.Context, tx server.PgTx, networkId server.Id, at time.Time) (seatCount int64) {
	if !server.SidecarMg6Linked() {
		return 0
	}
	excludedRoles := make([]string, 0, len(teamSeatExcludedRoles))
	for _, role := range teamSeatExcludedRoles {
		excludedRoles = append(excludedRoles, string(role))
	}
	at = at.UTC()
	server.Raise(tx.QueryRow(
		ctx,
		`
			SELECT COUNT(DISTINCT member.member_id)
			FROM network_client client
			INNER JOIN network_client_owner owner ON owner.client_id = client.client_id
			INNER JOIN network_member member ON member.member_id = owner.member_id
			WHERE
				client.network_id = $1 AND
				$2 < client.auth_time AND
				client.auth_time <= $3 AND
				member.network_id = $1 AND
				NOT (member.role = ANY($4::varchar[]))
		`,
		networkId,
		at.Add(-teamUsageSettings.SeatActiveWindow),
		at,
		excludedRoles,
	).Scan(&seatCount))
	return
}

// The network's seats at at (CountTeamSeatsInTx).
func CountTeamSeats(ctx context.Context, networkId server.Id, at time.Time) (seatCount int64) {
	server.Tx(ctx, func(tx server.PgTx) {
		seatCount = CountTeamSeatsInTx(ctx, tx, networkId, at)
	}, server.TxReadCommitted)
	return
}

// What one day's rollup of one Team computed.
type TeamUsageDay struct {
	NetworkId server.Id
	UsageDay  time.Time
	SeatCount int64
	// U(d), W(d) and C(d), in bytes
	TransferOverageByteCount int64
	CreditWindowByteCount    int64
	CreditRunningByteCount   int64
	// whether a transfer price is set: unset, no transfer row is written (D-QUOTA-2)
	TransferPriced bool
	// the day's stored bytes past the storage limit, and whether storage_gib_month is priced
	StorageOverageByteCount int64
	StoragePriced           bool
	// the day's Stripe values, by meter
	MeterValues map[string]int64
}

// The value of n bytes in whole units of unit, rounded half up, for n at or above zero.
func teamMeterUnits(n int64, unit int64) int64 {
	if n <= 0 {
		return 0
	}
	return (n + unit/2) / unit
}

// The sum of used over the network's team_credit lines that start before startBefore and end after
// endAfter, as balance_byte_count reads now.
func teamCreditUsedInTx(ctx context.Context, tx server.PgTx, networkId server.Id, startBefore time.Time, endAfter time.Time) (used int64) {
	server.Raise(tx.QueryRow(
		ctx,
		`
			SELECT COALESCE(SUM(start_balance_byte_count - balance_byte_count), 0)::bigint
			FROM transfer_balance
			WHERE
				network_id = $1 AND
				grant_kind = $2 AND
				start_time < $3 AND
				$4 < end_time
		`,
		networkId,
		GrantKindTeamCredit,
		startBefore.UTC(),
		endAfter.UTC(),
	).Scan(&used))
	return
}

// The network's row of meter for the newest day before day within the horizon, and that day, or false.
func teamUsagePreviousInTx(ctx context.Context, tx server.PgTx, networkId server.Id, meter string, day time.Time) (previousDay time.Time, quantity int64, ok bool) {
	result, err := tx.Query(
		ctx,
		`
			SELECT usage_day, quantity
			FROM network_service_usage
			WHERE
				network_id = $1 AND
				meter = $2 AND
				$3 <= usage_day AND
				usage_day < $4
			ORDER BY usage_day DESC
			LIMIT 1
		`,
		networkId,
		meter,
		networkUsageDay(day.Add(-teamUsageSettings.PreviousDayHorizon)),
		networkUsageDay(day),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&previousDay, &quantity))
			previousDay = networkUsageDay(previousDay)
			ok = true
		}
	})
	return
}

// The day's stored bytes: its storage row, else the network's count now.
func teamStoredByteCountInTx(ctx context.Context, tx server.PgTx, networkId server.Id, day time.Time) (storedByteCount int64) {
	result, err := tx.Query(
		ctx,
		`
			SELECT quantity
			FROM network_service_usage
			WHERE network_id = $1 AND usage_day = $2 AND meter = $3
		`,
		networkId,
		networkUsageDay(day),
		NetworkUsageStorageMeter,
	)
	found := false
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&storedByteCount))
			found = true
		}
	})
	if found {
		return
	}
	result, err = tx.Query(ctx, `SELECT stored_byte_count FROM network_quota_state WHERE network_id = $1`, networkId)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&storedByteCount))
		}
	})
	return
}

// Computes the network's rows for the closed day day, reading balances as they are now. It writes
// nothing.
func computeTeamUsageDayInTx(ctx context.Context, tx server.PgTx, networkId server.Id, day time.Time) *TeamUsageDay {
	settings := teamUsageSettings
	day = networkUsageDay(day)
	close := day.AddDate(0, 0, 1)
	usage := &TeamUsageDay{
		NetworkId:   networkId,
		UsageDay:    day,
		MeterValues: map[string]int64{},
	}

	usage.SeatCount = CountTeamSeatsInTx(ctx, tx, networkId, close)
	usage.MeterValues[TeamMeterSeats] = usage.SeatCount

	// QD-090's running sum
	usage.CreditWindowByteCount = teamCreditUsedInTx(ctx, tx, networkId, close, close.Add(-settings.CreditWindowMargin))
	previousDay, previousWindow, hasPrevious := teamUsagePreviousInTx(ctx, tx, networkId, TeamMeterCreditWindow, day)
	previousRunning := int64(0)
	if hasPrevious {
		// the lines of S(p) or S(d): those that start before d's close and end after p's close less
		// the margin, since S(p) is a subset of that and so is S(d)
		previousClose := previousDay.AddDate(0, 0, 1)
		union := teamCreditUsedInTx(ctx, tx, networkId, close, previousClose.Add(-settings.CreditWindowMargin))
		usage.TransferOverageByteCount = union - previousWindow
		if runningDay, running, ok := teamUsagePreviousInTx(ctx, tx, networkId, TeamMeterCreditRunning, day); ok && runningDay.Equal(previousDay) {
			previousRunning = running
		}
	} else {
		usage.TransferOverageByteCount = usage.CreditWindowByteCount
	}
	usage.CreditRunningByteCount = previousRunning + usage.TransferOverageByteCount
	_, usage.TransferPriced = Pro().Team.OverageTransferTibUsd()
	if usage.TransferPriced {
		usage.MeterValues[TeamMeterTransferOverage] = max(0,
			teamMeterUnits(usage.CreditRunningByteCount, settings.MeterUnitByteCount)-teamMeterUnits(previousRunning, settings.MeterUnitByteCount),
		)
	}

	_, usage.StoragePriced = Pro().Team.OverageStorageGibMonthUsd()
	if usage.StoragePriced {
		_, limits := networkSidecarLimitsWithTier(ctx, tx, networkId)
		if limit := limits.Limit(quotaStorageLimitKey); limit != proSidecarLimitNone {
			usage.StorageOverageByteCount = max(0, teamStoredByteCountInTx(ctx, tx, networkId, day)-limit)
		}
		usage.MeterValues[TeamMeterStorageOverage] = teamMeterUnits(usage.StorageOverageByteCount, settings.MeterUnitByteCount)
	}
	return usage
}

// Inserts the day's row of meter unless it is there. Returns whether it wrote it.
func insertTeamUsageRowInTx(ctx context.Context, tx server.PgTx, networkId server.Id, day time.Time, meter string, quantity int64) bool {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_service_usage (network_id, usage_day, meter, quantity)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (network_id, usage_day, meter) DO NOTHING
		`,
		networkId,
		networkUsageDay(day),
		meter,
		quantity,
	))
	return tag.RowsAffected() == 1
}

// Writes the network's rows for the closed day day unless a run wrote them, at now. Returns whether
// it wrote them.
func rollupTeamUsageDayInTx(ctx context.Context, tx server.PgTx, networkId server.Id, day time.Time, now time.Time) bool {
	usage := computeTeamUsageDayInTx(ctx, tx, networkId, day)
	// the seats row claims the day: one already there means a run wrote it
	if !insertTeamUsageRowInTx(ctx, tx, networkId, day, TeamMeterSeats, usage.SeatCount) {
		return false
	}
	insertTeamUsageRowInTx(ctx, tx, networkId, day, TeamMeterCreditWindow, usage.CreditWindowByteCount)
	insertTeamUsageRowInTx(ctx, tx, networkId, day, TeamMeterCreditRunning, usage.CreditRunningByteCount)
	if usage.TransferPriced {
		insertTeamUsageRowInTx(ctx, tx, networkId, day, TeamMeterTransferOverage, usage.TransferOverageByteCount)
	}
	if usage.StoragePriced {
		insertTeamUsageRowInTx(ctx, tx, networkId, day, TeamMeterStorageOverage, usage.StorageOverageByteCount)
	}
	_, err := WriteStripeMeterRowsInTx(ctx, tx, networkId, day, usage.MeterValues, now)
	server.Raise(err)
	return true
}

// Seams a test holds the rollup at. Nil in production.
type teamUsageRollupHooks struct {
	// inside a network's transaction, after its rows, before its commit
	beforeCommit func(networkId server.Id) error
}

// One page of RollupTeamServiceUsage.
type RollupTeamServiceUsageResult struct {
	// the Teams the page looked at, and those whose day it wrote (a rerun's are already there)
	LookedCount  int
	WrittenCount int
	// the page's last network: the next page starts after it; nil when the pass is done
	LastNetworkId *server.Id
}

// Writes the closed UTC day of day's Team rows (see the file header) for at most pageCount Teams whose
// subscription was live during it, after after (nil: from the start), in network order, one
// transaction a Team, at now. A Team whose day is written already is passed over.
func RollupTeamServiceUsage(ctx context.Context, day time.Time, after *server.Id, pageCount int, now time.Time) (RollupTeamServiceUsageResult, error) {
	return rollupTeamServiceUsage(ctx, day, after, pageCount, now, nil)
}

func rollupTeamServiceUsage(ctx context.Context, day time.Time, after *server.Id, pageCount int, now time.Time, hooks *teamUsageRollupHooks) (result RollupTeamServiceUsageResult, returnErr error) {
	if !server.SidecarMg6Linked() {
		return result, ErrTeamUsageUnavailable
	}
	if pageCount <= 0 {
		return result, errors.New("a rollup page of no networks")
	}
	day = networkUsageDay(day)
	close := day.AddDate(0, 0, 1)
	networkIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			ctx,
			`
				SELECT DISTINCT network_id
				FROM team_subscription
				WHERE
					start_time < $1 AND
					(end_time IS NULL OR $2 < end_time) AND
					($3::uuid IS NULL OR $3 < network_id)
				ORDER BY network_id
				LIMIT $4
			`,
			close,
			day,
			after,
			pageCount,
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var networkId server.Id
				server.Raise(rows.Scan(&networkId))
				networkIds = append(networkIds, networkId)
			}
		})
	})
	result.LookedCount = len(networkIds)
	for _, networkId := range networkIds {
		written := false
		server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				written = rollupTeamUsageDayInTx(ctx, tx, networkId, day, now)
				if written && hooks != nil && hooks.beforeCommit != nil {
					server.Raise(hooks.beforeCommit(networkId))
				}
			}, server.TxReadCommitted)
		}, func(err error) {
			returnErr = err
		})
		if returnErr != nil {
			// the next run starts at this network again: its transaction wrote nothing
			glog.Errorf("[quota][team]%s day rollup failed: %s\n", networkId, returnErr)
			return
		}
		if written {
			result.WrittenCount += 1
		}
	}
	if len(networkIds) == pageCount {
		last := networkIds[len(networkIds)-1]
		result.LastNetworkId = &last
	}
	return
}
