// The device day rollup (PARTITION-CANDIDATES.md §7.3; owner 2026-11-16):
// audit_device_day holds one row per UTC day with the day's count of devices,
// the connected-per-day series computeStatsDevice publishes, so the hourly
// export reads at most a few hundred small rows instead of every retained
// device event, and the raw events need only days of retention.
//
// A device counts on day D when it was online at D's start or had an event
// during D. The device sweep is the only writer of device events and state,
// and it keeps today's row in its own transaction, before it writes its
// events:
//   - the first sweep of a day inserts the day's row with the count of
//     devices online in audit_device_state, which, with no event yet that
//     day, are exactly the devices online at the day's start;
//   - every sweep then adds the devices it adds that were neither online at
//     the day's start nor touched earlier that day. A device online at the
//     start is counted already; one touched earlier that day has an
//     audit_device_state row whose event time is that day's, which the
//     offline state retention keeps for a day.
//
// So today's row is the count as of the last sweep, and a past day's row is
// final once the next day's first sweep has run.
//
// A row is complete when its count saw the whole day. A day whose first sweep
// found device events already written that day (the first deployment, or
// events written by an older taskworker) is incomplete, and so is a day
// without any sweep while events were written after it. A day without any
// sweep and without any event after its start is complete: the online set
// stayed as it was, and the day counts it.
//
// The export publishes from the rows when every day of its window has a
// complete row (today may have none yet: it then counts the devices online
// now, as today's first sweep will). Otherwise, while audit_device_event is a
// plain table holding the whole window, it computes the series from the raw
// events as before and writes the complete count of every closed day after
// the window's first one that it lacked; the exports of the deployment day and
// the day after therefore fill the window. Once audit_device_event is
// partitioned, the raw events cover only days, and the export publishes from
// the rows alone, logging the days it lacks.
package model

import (
	"context"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

// One day's row.
type auditDeviceDay struct {
	deviceCount int64
	complete    bool
}

// The UTC day holding t, at 00:00.
func auditDeviceDayStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// Keeps today's row of audit_device_day for a sweep at now, in the sweep's
// transaction and before it writes its events. onlineCount is the number of
// devices online in audit_device_state as the sweep read them; firstAddIds are
// the devices this sweep adds that were not online there.
func updateAuditDeviceDayInTx(ctx context.Context, tx server.PgTx, now time.Time, onlineCount int, firstAddIds []server.Id) {
	// a taskworker deployed before the migration sweeps as before
	var migrated bool
	server.Raise(tx.QueryRow(ctx, `SELECT to_regclass('public.audit_device_day') IS NOT NULL`).Scan(&migrated))
	if !migrated {
		return
	}
	day := auditDeviceDayStart(now)
	var exists bool
	server.Raise(tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_device_day WHERE day = $1::date)`, day).Scan(&exists))
	if !exists {
		eventsSince := func(start time.Time) (found bool) {
			server.Raise(tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_device_event WHERE $1 <= event_time)`, start).Scan(&found))
			return
		}
		eventsToday := eventsSince(day)
		var lastDay *time.Time
		server.Raise(tx.QueryRow(ctx, `SELECT max(day)::timestamp FROM audit_device_day WHERE day < $1::date`, day).Scan(&lastDay))
		if lastDay != nil {
			// days without a sweep: the online set held through them only if
			// nothing was written since
			gapStart := lastDay.UTC().AddDate(0, 0, 1)
			if gapStart.Before(day) {
				complete := !eventsSince(gapStart)
				for gapDay := gapStart; gapDay.Before(day); gapDay = gapDay.AddDate(0, 0, 1) {
					server.RaisePgResult(tx.Exec(
						ctx,
						`
						INSERT INTO audit_device_day (day, device_count, complete, update_time)
						VALUES ($1::date, $2, $3, $4)
						ON CONFLICT (day) DO NOTHING
						`,
						gapDay,
						onlineCount,
						complete,
						now,
					))
				}
			}
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO audit_device_day (day, device_count, complete, update_time)
			VALUES ($1::date, $2, $3, $4)
			`,
			day,
			onlineCount,
			!eventsToday,
			now,
		))
	}
	if len(firstAddIds) == 0 {
		return
	}
	// the devices among them touched earlier today, now offline
	var touchedCount int64
	server.Raise(tx.QueryRow(
		ctx,
		`SELECT count(*) FROM audit_device_state WHERE device_id = ANY($1::uuid[]) AND $2 <= event_time`,
		firstAddIds,
		day,
	).Scan(&touchedCount))
	if added := int64(len(firstAddIds)) - touchedCount; 0 < added {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE audit_device_day
			SET device_count = device_count + $2, update_time = $3
			WHERE day = $1::date
			`,
			day,
			added,
			now,
		))
	}
}

// The rows of [startDay, endDay], days as YYYY-MM-DD, and whether the table
// exists yet.
func readAuditDeviceDaysInConn(ctx context.Context, conn server.PgCanQuery, startDay string, endDay string) (days map[string]auditDeviceDay, migrated bool) {
	days = map[string]auditDeviceDay{}
	result, err := conn.Query(ctx, `SELECT to_regclass('public.audit_device_day') IS NOT NULL`)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&migrated))
		}
	})
	if !migrated {
		return
	}
	result, err = conn.Query(
		ctx,
		`
		SELECT to_char(day, 'YYYY-MM-DD'), device_count, complete
		FROM audit_device_day
		WHERE $1::date <= day AND day <= $2::date
		`,
		startDay,
		endDay,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var day string
			var row auditDeviceDay
			server.Raise(result.Scan(&day, &row.deviceCount, &row.complete))
			days[day] = row
		}
	})
	return
}

// Whether audit_device_event is a partitioned parent, whose raw events cover
// only its retention.
func auditDeviceEventPartitionedInConn(ctx context.Context, conn server.PgCanQuery) (partitioned bool) {
	result, err := conn.Query(
		ctx,
		`SELECT coalesce((SELECT relkind = 'p' FROM pg_class WHERE oid = to_regclass('public.audit_device_event')), false)`,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&partitioned))
		}
	})
	return
}

// The days of [startDay, endDay] without a complete row. Today, endDay, has
// no row only before its first sweep, which is not lacking: its count then is
// the devices online now, as that sweep will insert it.
func auditDeviceDaysLacking(days map[string]auditDeviceDay, startDay string, endDay string) (lacking []string) {
	for day := startDay; day <= endDay; day = nextDay(day) {
		row, ok := days[day]
		if day == endDay && !ok {
			continue
		}
		if !ok || !row.complete {
			lacking = append(lacking, day)
		}
	}
	return
}

// The devices online in audit_device_state: today's count before its first
// sweep.
func auditDeviceOnlineCountInConn(ctx context.Context, conn server.PgCanQuery) (count int64) {
	result, err := conn.Query(ctx, `SELECT count(*) FROM audit_device_state WHERE online`)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&count))
		}
	})
	return
}

// The series from the rows: every day of the window, a day without a row at
// zero.
func setStatsDevicesFromDays(stats *Stats, days map[string]auditDeviceDay, startDay string, endDay string) {
	devicesData := map[string]int{}
	for day := startDay; day <= endDay; day = nextDay(day) {
		devicesData[day] = int(days[day].deviceCount)
	}
	stats.DevicesData = devicesData
	stats.DevicesSummary = summary(devicesData)
}

// Writes the counts of the closed days after startDay and before today from a
// series the raw events gave, as complete rows, over a missing or incomplete
// row only. The replay counts such a day exactly: its carry-in state is the
// latest event before the window's start. The window's first day itself is
// not exact, since its events before the window's start time count as
// carry-in, not as touches; it was an interior day of the day before's window.
func backfillAuditDeviceDays(ctx context.Context, devicesData map[string]int, startDay string, today string, now time.Time) (written int) {
	server.Tx(ctx, func(tx server.PgTx) {
		written = 0
		for day := nextDay(startDay); day < today; day = nextDay(day) {
			count, ok := devicesData[day]
			if !ok {
				continue
			}
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO audit_device_day (day, device_count, complete, update_time)
				VALUES ($1::date, $2, true, $3)
				ON CONFLICT (day) DO UPDATE
				SET device_count = EXCLUDED.device_count, complete = true, update_time = EXCLUDED.update_time
				WHERE NOT audit_device_day.complete
				`,
				day,
				count,
				now,
			))
			written += int(tag.RowsAffected())
		}
	})
	return
}

// The devices series: from the rollup when it covers the window, or once the
// raw events no longer do; otherwise from the raw events, writing the closed
// days the rollup lacked.
func computeStatsDevice(ctx context.Context, stats *Stats) {
	startDay, endDay := dayRange(stats.Lookback)
	var days map[string]auditDeviceDay
	var migrated bool
	var partitioned bool
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		days, migrated = readAuditDeviceDaysInConn(ctx, conn, startDay, endDay)
		partitioned = auditDeviceEventPartitionedInConn(ctx, conn)
	})
	if !migrated {
		computeStatsDeviceFromEvents(ctx, stats)
		return
	}
	lacking := auditDeviceDaysLacking(days, startDay, endDay)
	if len(lacking) == 0 || partitioned {
		if 0 < len(lacking) {
			glog.Errorf("[audit]devices series: the device day rollup lacks %d day(s) of the window (first %s, last %s), published from what it holds\n", len(lacking), lacking[0], lacking[len(lacking)-1])
		}
		if _, ok := days[endDay]; !ok {
			server.ReplicaDb(ctx, func(conn server.PgConn) {
				days[endDay] = auditDeviceDay{deviceCount: auditDeviceOnlineCountInConn(ctx, conn)}
			})
		}
		setStatsDevicesFromDays(stats, days, startDay, endDay)
		return
	}
	computeStatsDeviceFromEvents(ctx, stats)
	if written := backfillAuditDeviceDays(ctx, stats.DevicesData, startDay, endDay, server.NowUtc()); 0 < written {
		glog.Infof("[audit]devices series: wrote %d closed day(s) of the device day rollup from the raw events\n", written)
	}
}
