// The device day rollup (audit_device_day.go): the real device sweep, run at
// fixed times across days, keeps each day's count equal to the reference
// replay of the raw events; a day it cannot have seen whole is incomplete; a
// sweep before the migration sweeps as before. The export publishes the
// rollup once it covers the window, otherwise replays the raw events and
// writes the closed days, and once the events are partitioned publishes the
// rollup alone.
package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The sweep's relations and the rollup, as their migrations make them.
func withAuditDeviceDayFixture(t *testing.T, rollup bool, run func(testing.TB, context.Context)) {
	t.Helper()
	(&server.TestEnv{}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				CREATE TABLE network_client (client_id uuid PRIMARY KEY, network_id uuid NOT NULL);
				CREATE TABLE network_client_connection (connection_id uuid PRIMARY KEY, client_id uuid NOT NULL, connected bool NOT NULL);
				CREATE TABLE audit_device_state (device_id uuid PRIMARY KEY, network_id uuid NOT NULL, online bool NOT NULL, event_time timestamp NOT NULL);
				CREATE TABLE audit_device_event (
					event_id uuid NOT NULL, event_time timestamp NOT NULL, network_id uuid NOT NULL, device_id uuid NOT NULL,
					event_type varchar(64) NOT NULL, event_details text,
					PRIMARY KEY (event_time, device_id, event_id))
			`))
			if rollup {
				server.RaisePgResult(tx.Exec(ctx, server.AuditDeviceDaySql))
			}
		})
		run(t, ctx)
	})
}

// Devices connecting and disconnecting, swept at fixed times.
type auditDeviceDayWorld struct {
	t         testing.TB
	ctx       context.Context
	networkId server.Id
	devices   map[string]server.Id
}

func newAuditDeviceDayWorld(t testing.TB, ctx context.Context) *auditDeviceDayWorld {
	return &auditDeviceDayWorld{t: t, ctx: ctx, networkId: server.NewId(), devices: map[string]server.Id{}}
}

func (self *auditDeviceDayWorld) device(name string) server.Id {
	deviceId, ok := self.devices[name]
	if !ok {
		deviceId = server.NewId()
		self.devices[name] = deviceId
		server.Tx(self.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(self.ctx, `INSERT INTO network_client (client_id, network_id) VALUES ($1, $2)`, deviceId, self.networkId))
		})
	}
	return deviceId
}

func (self *auditDeviceDayWorld) connect(names ...string) {
	for _, name := range names {
		deviceId := self.device(name)
		server.Tx(self.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(self.ctx, `INSERT INTO network_client_connection (connection_id, client_id, connected) VALUES ($1, $2, true)`, server.NewId(), deviceId))
		})
	}
}

func (self *auditDeviceDayWorld) disconnect(names ...string) {
	for _, name := range names {
		deviceId := self.device(name)
		server.Tx(self.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(self.ctx, `DELETE FROM network_client_connection WHERE client_id = $1`, deviceId))
		})
	}
}

func (self *auditDeviceDayWorld) sweepAt(at time.Time) {
	sweepDeviceAuditEventsAt(self.ctx, func() time.Time { return at })
}

// A day's row, or exists false.
func (self *auditDeviceDayWorld) day(day time.Time) (count int64, complete bool, exists bool) {
	server.Db(self.ctx, func(conn server.PgConn) {
		result, err := conn.Query(self.ctx, `SELECT device_count, complete FROM audit_device_day WHERE day = $1::date`, day)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count, &complete))
				exists = true
			}
		})
	})
	return
}

func auditDeviceDayTestDay(n int) time.Time {
	return time.Date(2026, 10, n, 0, 0, 0, 0, time.UTC)
}

// The rollup equals the reference replay of the raw events, day by day,
// through a day's reconnects, a device back after a day offline, and a day
// without any sweep, which keeps the online set of the day before it.
func TestAuditDeviceDayMatchesTheReplayAcrossDays(t *testing.T) {
	withAuditDeviceDayFixture(t, true, func(t testing.TB, ctx context.Context) {
		world := newAuditDeviceDayWorld(t, ctx)
		at := func(day int, hour int) time.Time {
			return auditDeviceDayTestDay(day).Add(time.Duration(hour) * time.Hour)
		}
		world.connect("a", "b")
		world.sweepAt(at(1, 10))
		world.disconnect("a")
		world.connect("c")
		world.sweepAt(at(1, 14))
		world.connect("a")
		world.sweepAt(at(1, 18))
		world.disconnect("c")
		world.sweepAt(at(1, 23))
		world.connect("d")
		world.disconnect("b")
		world.sweepAt(at(2, 9))
		world.connect("c")
		world.disconnect("a")
		world.sweepAt(at(2, 20))
		// no sweep on day 3
		world.connect("e")
		world.sweepAt(at(4, 8))
		world.connect("a")
		world.sweepAt(at(4, 12))

		// a sweep run again with nothing changed counts nothing again
		world.sweepAt(at(4, 13))

		var replay map[string]int
		server.Db(ctx, func(conn server.PgConn) {
			replay, _ = referenceComputeStatsDeviceWindow(ctx, conn, "2026-10-01", "2026-10-04", auditDeviceDayTestDay(1))
		})
		for day, expected := range map[int]int64{1: 3, 2: 4, 3: 2, 4: 4} {
			count, complete, exists := world.day(auditDeviceDayTestDay(day))
			if !exists || !complete || count != expected {
				t.Fatalf("day %d: count %d, complete %t, exists %t; want %d", day, count, complete, exists, expected)
			}
			if replayed := replay[auditDeviceDayTestDay(day).Format(time.DateOnly)]; int64(replayed) != count {
				t.Fatalf("day %d: the rollup counts %d, the replay %d", day, count, replayed)
			}
		}
	})
}

// A day whose first sweep finds events already written that day, as from an
// older taskworker or before the deployment, cannot have seen the day whole.
func TestAuditDeviceDayIsIncompleteWhenEventsPrecedeItsFirstSweep(t *testing.T) {
	withAuditDeviceDayFixture(t, true, func(t testing.TB, ctx context.Context) {
		world := newAuditDeviceDayWorld(t, ctx)
		gone := world.device("gone")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_device_event (event_id, event_time, network_id, device_id, event_type, event_details)
				VALUES ($1, $2, $3, $4, $5, 'synthetic older sweep')`, server.NewId(), auditDeviceDayTestDay(1).Add(time.Hour), world.networkId, gone, AuditEventTypeDeviceAdded))
		})
		world.connect("a")
		world.sweepAt(auditDeviceDayTestDay(1).Add(10 * time.Hour))
		if count, complete, exists := world.day(auditDeviceDayTestDay(1)); !exists || complete || count != 1 {
			t.Fatalf("the day after earlier events: count %d, complete %t, exists %t", count, complete, exists)
		}
	})
}

// Days without a sweep are complete only when nothing was written since: an
// older taskworker's sweep after the last row leaves them incomplete. The
// next day's first sweep saw no event that day, and the state the older sweep
// kept says who was online at its start, so that day is complete.
func TestAuditDeviceDayGapAfterOtherWritesIsIncomplete(t *testing.T) {
	withAuditDeviceDayFixture(t, true, func(t testing.TB, ctx context.Context) {
		world := newAuditDeviceDayWorld(t, ctx)
		world.connect("a")
		world.sweepAt(auditDeviceDayTestDay(1).Add(10 * time.Hour))
		// an older taskworker's sweep adds a device on day 2: its event and its
		// state
		other := world.device("other")
		olderAt := auditDeviceDayTestDay(2).Add(12 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_device_event (event_id, event_time, network_id, device_id, event_type, event_details)
				VALUES ($1, $2, $3, $4, $5, 'synthetic older sweep')`, server.NewId(), olderAt, world.networkId, other, AuditEventTypeDeviceAdded))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_device_state (device_id, network_id, online, event_time) VALUES ($1, $2, true, $3)`, other, world.networkId, olderAt))
		})
		world.sweepAt(auditDeviceDayTestDay(4).Add(10 * time.Hour))
		for _, day := range []int{2, 3} {
			if _, complete, exists := world.day(auditDeviceDayTestDay(day)); !exists || complete {
				t.Fatalf("day %d after other writes: complete %t, exists %t", day, complete, exists)
			}
		}
		// online at day 4's start: a, and the device the older sweep added,
		// which this sweep finds gone
		if count, complete, exists := world.day(auditDeviceDayTestDay(4)); !exists || !complete || count != 2 {
			t.Fatalf("day 4: count %d, complete %t, exists %t", count, complete, exists)
		}
		var replay map[string]int
		server.Db(ctx, func(conn server.PgConn) {
			replay, _ = referenceComputeStatsDeviceWindow(ctx, conn, "2026-10-01", "2026-10-04", auditDeviceDayTestDay(1))
		})
		if replay["2026-10-04"] != 2 {
			t.Fatalf("the replay counts %d on day 4", replay["2026-10-04"])
		}
	})
}

// A taskworker deployed before the migration sweeps as before.
func TestAuditDeviceDaySweepWithoutItsTable(t *testing.T) {
	withAuditDeviceDayFixture(t, false, func(t testing.TB, ctx context.Context) {
		world := newAuditDeviceDayWorld(t, ctx)
		world.connect("a", "b")
		world.sweepAt(auditDeviceDayTestDay(1).Add(10 * time.Hour))
		var events int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM audit_device_event`).Scan(&events))
		})
		if events != 2 {
			t.Fatalf("the sweep wrote %d events", events)
		}
	})
}

// Writes a complete row for each day of the export's window, count from n.
func writeAuditDeviceDays(t testing.TB, ctx context.Context, startDay string, endDay string, count func(day string) int64, complete bool) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		for day := startDay; day <= endDay; day = nextDay(day) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_device_day (day, device_count, complete, update_time) VALUES ($1::date, $2, $3, now())
				ON CONFLICT (day) DO UPDATE SET device_count = EXCLUDED.device_count, complete = EXCLUDED.complete`, day, count(day), complete))
		}
	})
}

func readAuditDeviceDayCounts(t testing.TB, ctx context.Context) map[string]auditDeviceDay {
	t.Helper()
	var days map[string]auditDeviceDay
	server.Db(ctx, func(conn server.PgConn) {
		days, _ = readAuditDeviceDaysInConn(ctx, conn, "2000-01-01", "2100-01-01")
	})
	return days
}

// A complete rollup is published as it is: the export reads no raw event,
// shown by a raw history that would count otherwise.
func TestComputeStatsDevicePublishesTheCompleteRollup(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		startDay, endDay := dayRange(90)
		server.Tx(ctx, func(tx server.PgTx) {
			insertAuditStatsTestEvents(ctx, tx, auditStatsDailyHistory(server.NowUtc(), 20, -5, 0))
		})
		index := map[string]int64{}
		for day, n := startDay, int64(1); day <= endDay; day, n = nextDay(day), n+1 {
			index[day] = n
		}
		writeAuditDeviceDays(t, ctx, startDay, endDay, func(day string) int64 { return 1000 + index[day] }, true)
		stats := &Stats{Lookback: 90}
		computeStatsDevice(ctx, stats)
		if len(stats.DevicesData) != len(index) {
			t.Fatalf("%d days published, %d in the window", len(stats.DevicesData), len(index))
		}
		for day, n := range index {
			if stats.DevicesData[day] != int(1000+n) {
				t.Fatalf("%s: published %d, the rollup holds %d", day, stats.DevicesData[day], 1000+n)
			}
		}
	})
}

// Without a complete rollup the export replays the raw events as before and
// writes every closed day it lacked, never today; the next export publishes
// those days from the rollup.
func TestComputeStatsDeviceBackfillsTheClosedDaysFromTheRawEvents(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		startDay, endDay := dayRange(90)
		server.Tx(ctx, func(tx server.PgTx) {
			insertAuditStatsTestEvents(ctx, tx, auditStatsDailyHistory(server.NowUtc(), 30, -10, 0))
		})
		reference := &Stats{Lookback: 90}
		computeStatsDeviceFromEvents(ctx, reference)
		stats := &Stats{Lookback: 90}
		computeStatsDevice(ctx, stats)
		for day := startDay; day <= endDay; day = nextDay(day) {
			if stats.DevicesData[day] != reference.DevicesData[day] {
				t.Fatalf("%s: published %d, the replay %d", day, stats.DevicesData[day], reference.DevicesData[day])
			}
		}
		days := readAuditDeviceDayCounts(t, ctx)
		for day := nextDay(startDay); day < endDay; day = nextDay(day) {
			if row, ok := days[day]; !ok || !row.complete || row.deviceCount != int64(reference.DevicesData[day]) {
				t.Fatalf("%s: the backfill wrote %+v, the replay counts %d", day, row, reference.DevicesData[day])
			}
		}
		// the window's first day is not exact in a replay, and today is the
		// sweep's
		if _, ok := days[startDay]; ok {
			t.Fatal("the backfill wrote the window's first day")
		}
		if _, ok := days[endDay]; ok {
			t.Fatal("the backfill wrote today")
		}

		// with the first day an interior day of the day before's window and
		// today's row the sweep's, the export reads the rollup alone: the
		// raw events are gone
		writeAuditDeviceDays(t, ctx, startDay, startDay, func(day string) int64 { return int64(reference.DevicesData[day]) }, true)
		writeAuditDeviceDays(t, ctx, endDay, endDay, func(day string) int64 { return int64(reference.DevicesData[day]) }, true)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM audit_device_event`))
		})
		again := &Stats{Lookback: 90}
		computeStatsDevice(ctx, again)
		for day := startDay; day <= endDay; day = nextDay(day) {
			if again.DevicesData[day] != reference.DevicesData[day] {
				t.Fatalf("%s: published %d from the rollup, the replay counted %d", day, again.DevicesData[day], reference.DevicesData[day])
			}
		}
	})
}

// Before today's first sweep, today counts the devices online now, as that
// sweep will insert it, with no replay of the raw events.
func TestComputeStatsDevicePublishesTodayFromTheOnlineSetBeforeItsFirstSweep(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		startDay, endDay := dayRange(90)
		writeAuditDeviceDays(t, ctx, startDay, endDay, func(string) int64 { return 7 }, true)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM audit_device_day WHERE day = $1::date`, endDay))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_device_state (device_id, network_id, online, event_time)
				SELECT gen_random_uuid(), gen_random_uuid(), n <= 5, now() FROM generate_series(1, 9) n`))
		})
		stats := &Stats{Lookback: 90}
		computeStatsDevice(ctx, stats)
		if stats.DevicesData[endDay] != 5 || stats.DevicesData[startDay] != 7 {
			t.Fatalf("today %d (want 5 online), the first day %d", stats.DevicesData[endDay], stats.DevicesData[startDay])
		}
	})
}

// Once audit_device_event is partitioned the raw events cover only days, so
// the export publishes the rollup alone, a day it lacks at zero, and the row
// delete stops.
func TestComputeStatsDeviceUsesTheRollupAloneOncePartitioned(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		startDay, endDay := dayRange(90)
		server.Testing_PartitionAuditDeviceEvents(ctx, server.PartitionGranularityDaily.AddRanges(server.PartitionGranularityDaily.RangeStart(server.NowUtc()), -10))
		server.Tx(ctx, func(tx server.PgTx) {
			insertAuditStatsTestEvents(ctx, tx, auditStatsDailyHistory(server.NowUtc(), 20, -5, 0))
		})
		writeAuditDeviceDays(t, ctx, startDay, endDay, func(string) int64 { return 11 }, true)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM audit_device_day WHERE day = $1::date`, nextDay(startDay)))
		})
		stats := &Stats{Lookback: 90}
		computeStatsDevice(ctx, stats)
		if stats.DevicesData[nextDay(startDay)] != 0 || stats.DevicesData[startDay] != 11 {
			t.Fatalf("published %d for the lacking day and %d for the first", stats.DevicesData[nextDay(startDay)], stats.DevicesData[startDay])
		}
		if days := readAuditDeviceDayCounts(t, ctx); len(days) != len(stats.DevicesData)-1 {
			t.Fatal("the partitioned export wrote rows", len(days))
		}
		if removed := RemoveOldAuditDeviceEvents(ctx, server.NowUtc().Add(365*24*time.Hour), 1000); removed != 0 {
			t.Fatalf("the row delete removed %d events of a partitioned table", removed)
		}
		// the backfill reaches back past the days the partitions keep
		var backfillErr error
		server.HandleError(func() {
			BackfillDeviceAuditEvents(ctx, server.NowUtc().Add(-10*24*time.Hour), server.NowUtc())
		}, func(err error) { backfillErr = err })
		if backfillErr == nil || !strings.Contains(backfillErr.Error(), "partitioned") {
			t.Fatal("the backfill ran on a partitioned table", backfillErr)
		}
	})
}
