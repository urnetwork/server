// The devices series must stay exactly the replay it has always published,
// while no statement streams per-device rows or reads the whole history.
package model

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"maps"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// The replay published before span counts, with its database clock replaced
// by the test's explicit cutoff: one row per device-day and carry-in, replayed
// in Go into an active set. It is the reference the span counts must equal.
const referenceComputeStatsDeviceSql = `
	SELECT
		t.day,
		t.device_id,
		t.last_event[2] AS event_type,
		t.carry_in
	FROM (
		SELECT
			to_char(event_time, 'YYYY-MM-DD') AS day,
			device_id,
			MAX(ARRAY[event_id::varchar, event_type]) AS last_event,
			false AS carry_in
		FROM audit_device_event
		WHERE
			@windowStart <= event_time AND
			event_type IN (@eventTypeDeviceAdded, @eventTypeDeviceRemoved)
		GROUP BY day, device_id

		UNION ALL

		SELECT
			@startDay AS day,
			device_id,
			MAX(ARRAY[event_id::varchar, event_type]) AS last_event,
			true AS carry_in
		FROM audit_device_event
		WHERE
			event_time < @windowStart AND
			event_type IN (@eventTypeDeviceAdded, @eventTypeDeviceRemoved)
		GROUP BY device_id
	) t
	WHERE NOT t.carry_in OR t.last_event[2] = @eventTypeDeviceAdded
	ORDER BY day ASC, carry_in DESC
`

func referenceComputeStatsDeviceWindow(ctx context.Context, conn server.PgCanQuery, startDay string, endDay string, windowStart time.Time) (map[string]int, int) {
	result, err := conn.Query(ctx, referenceComputeStatsDeviceSql, server.PgNamedArgs{
		"startDay":               startDay,
		"windowStart":            windowStart,
		"eventTypeDeviceAdded":   AuditEventTypeDeviceAdded,
		"eventTypeDeviceRemoved": AuditEventTypeDeviceRemoved,
	})
	devicesData := map[string]int{}
	server.WithPgResult(result, err, func() {
		activeDay := startDay
		activeDevices := map[server.Id]bool{}
		touchedDevices := map[server.Id]bool{}
		exportActive := func() {
			count := len(activeDevices)
			for deviceId := range touchedDevices {
				if !activeDevices[deviceId] {
					count += 1
				}
			}
			devicesData[activeDay] = count
		}
		var day string
		var deviceId server.Id
		var eventType string
		var carryIn bool
		for result.Next() {
			server.Raise(result.Scan(&day, &deviceId, &eventType, &carryIn))
			if day != activeDay {
				exportActive()
				touchedDevices = map[server.Id]bool{}
				for packDay := nextDay(activeDay); packDay < day; packDay = nextDay(activeDay) {
					activeDay = packDay
					exportActive()
				}
				activeDay = day
			}
			if !carryIn {
				touchedDevices[deviceId] = true
			}
			switch AuditEventType(eventType) {
			case AuditEventTypeDeviceRemoved:
				delete(activeDevices, deviceId)
			case AuditEventTypeDeviceAdded:
				activeDevices[deviceId] = true
			}
		}
		exportActive()
		touchedDevices = map[server.Id]bool{}
		for packDay := nextDay(activeDay); packDay <= endDay; packDay = nextDay(activeDay) {
			activeDay = packDay
			exportActive()
		}
	})
	return devicesData, summary(devicesData)
}

func auditStatsTestId(n uint64) server.Id {
	var id server.Id
	binary.BigEndian.PutUint64(id[8:], n)
	return id
}

type auditStatsTestEvent struct {
	id     uint64
	device uint64
	at     time.Time
	kind   AuditEventType
}

func insertAuditStatsTestEvents(ctx context.Context, tx server.PgTx, events []auditStatsTestEvent) {
	rows := make([][]any, 0, len(events))
	for _, event := range events {
		rows = append(rows, []any{auditStatsTestId(event.id), event.at,
			auditStatsTestId(1), auditStatsTestId(event.device), event.kind, "synthetic audit history"})
	}
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"audit_device_event"},
		[]string{"event_id", "event_time", "network_id", "device_id", "event_type", "event_details"},
		pgx.CopyFromRows(rows))
	server.Raise(err)
}

// Statements of the device series in order, with the rows each returned.
type auditStatsRecordingConn struct {
	conn       server.PgCanQuery
	statements []*auditStatsStatement
}

type auditStatsStatement struct {
	sql      string
	args     []any
	rowCount int
}

type auditStatsCountingRows struct {
	pgx.Rows
	statement *auditStatsStatement
}

func (self *auditStatsCountingRows) Next() bool {
	next := self.Rows.Next()
	if next {
		self.statement.rowCount += 1
	}
	return next
}

func (self *auditStatsRecordingConn) Query(ctx context.Context, sql string, args ...any) (server.PgResult, error) {
	result, err := self.conn.Query(ctx, sql, args...)
	if err != nil {
		return result, err
	}
	statement := &auditStatsStatement{sql: sql, args: args}
	self.statements = append(self.statements, statement)
	return &auditStatsCountingRows{Rows: result, statement: statement}, nil
}

type auditStatsPlan struct {
	NodeType     string           `json:"Node Type"`
	RelationName string           `json:"Relation Name"`
	ActualRows   float64          `json:"Actual Rows"`
	ActualLoops  float64          `json:"Actual Loops"`
	FilteredRows float64          `json:"Rows Removed by Filter"`
	SharedHits   int64            `json:"Shared Hit Blocks"`
	SharedReads  int64            `json:"Shared Read Blocks"`
	TempReads    int64            `json:"Temp Read Blocks"`
	Plans        []auditStatsPlan `json:"Plans"`
}

func auditStatsExplain(ctx context.Context, conn server.PgCanQuery, sql string, args ...any) auditStatsPlan {
	rows, err := conn.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON, TIMING OFF) "+sql, args...)
	var plan []struct {
		Plan auditStatsPlan `json:"Plan"`
	}
	server.WithPgResult(rows, err, func() {
		if !rows.Next() {
			panic("missing audit stats query plan")
		}
		var raw []byte
		server.Raise(rows.Scan(&raw))
		server.Raise(json.Unmarshal(raw, &plan))
	})
	return plan[0].Plan
}

// audit_device_event rows a plan visited.
func (p auditStatsPlan) auditWork() (scans int, visited float64) {
	if p.RelationName == "audit_device_event" {
		scans++
		visited += (p.ActualRows + p.FilteredRows) * p.ActualLoops
	}
	for _, child := range p.Plans {
		n, v := child.auditWork()
		scans += n
		visited += v
	}
	return
}

// One sweep-like history: each device connects every morning and disconnects
// every evening of the given days, and every seventh device stays connected.
func auditStatsDailyHistory(cutoff time.Time, deviceCount int, firstDay int, endDay int) []auditStatsTestEvent {
	events := make([]auditStatsTestEvent, 0, 2*deviceCount*(endDay-firstDay))
	appendEvent := func(device uint64, at time.Time, kind AuditEventType) {
		events = append(events, auditStatsTestEvent{uint64(len(events) + 1), device, at, kind})
	}
	for day := firstDay; day < endDay; day++ {
		for device := uint64(1); device <= uint64(deviceCount); device++ {
			at := cutoff.AddDate(0, 0, day).Add(-4 * time.Hour)
			appendEvent(device, at, AuditEventTypeDeviceAdded)
			endKind := AuditEventTypeDeviceRemoved
			if device%7 == 0 {
				endKind = AuditEventTypeDeviceAdded
			}
			appendEvent(device, at.Add(10*time.Hour), endKind)
		}
	}
	return events
}

func TestComputeStatsDeviceWindowBoundaries(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cutoff := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
		day := func(n int) string { return cutoff.AddDate(0, 0, n).Format(time.DateOnly) }
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, "SET LOCAL TIME ZONE 'UTC'"))
			stats := &Stats{}
			computeStatsDeviceWindow(ctx, stats, tx, day(0), day(5), cutoff)
			for n := 0; n <= 5; n++ {
				if value, ok := stats.DevicesData[day(n)]; !ok || value != 0 {
					t.Fatalf("empty history day %d = %d, present %v", n, value, ok)
				}
			}
			insertAuditStatsTestEvents(ctx, tx, []auditStatsTestEvent{
				// Carried connected state counts every eventless day.
				{1, 1, cutoff.AddDate(0, 0, -30), AuditEventTypeDeviceAdded},
				// An already disconnected historical device contributes nothing.
				{2, 2, cutoff.AddDate(0, 0, -20), AuditEventTypeDeviceAdded},
				{3, 2, cutoff.AddDate(0, 0, -10), AuditEventTypeDeviceRemoved},
				// The same date has both carry-in and a later in-window removal.
				{4, 3, cutoff.Add(-time.Hour), AuditEventTypeDeviceAdded},
				{5, 3, cutoff.Add(time.Hour), AuditEventTypeDeviceRemoved},
				// A removal exactly on the cutoff touches the first day; one
				// microsecond before it is only an inert disconnected baseline.
				{6, 4, cutoff, AuditEventTypeDeviceRemoved},
				{7, 5, cutoff.Add(-time.Microsecond), AuditEventTypeDeviceRemoved},
				// A connection straddling midnight counts both dates.
				{8, 6, cutoff.Add(59*time.Hour + 50*time.Minute), AuditEventTypeDeviceAdded},
				{9, 6, cutoff.Add(60*time.Hour + 10*time.Minute), AuditEventTypeDeviceRemoved},
				// Preserve event-ID ordering when a later revision backfills an
				// earlier timestamp. MAX(event_time) would give the wrong state.
				{10, 7, cutoff.Add(4*24*time.Hour + 6*time.Hour), AuditEventTypeDeviceRemoved},
				{11, 7, cutoff.Add(4*24*time.Hour - 4*time.Hour), AuditEventTypeDeviceAdded},
				// Other event types do not change this series or win its MAX.
				{12, 7, cutoff.Add(4*24*time.Hour + 7*time.Hour), AuditEventTypeNetworkDeleted},
				{13, 8, cutoff.Add(24 * time.Hour), AuditEventTypeNetworkCreated},
			})
			computeStatsDeviceWindow(ctx, stats, tx, day(0), day(5), cutoff)
			for n, want := range []int{3, 1, 2, 2, 2, 2} {
				if got := stats.DevicesData[day(n)]; got != want {
					t.Fatalf("connected-per-day day %d = %d, want %d", n, got, want)
				}
			}
			if len(stats.DevicesData) != 6 || stats.DevicesSummary != 2 {
				t.Fatalf("series days or last-three-day summary changed: days=%d summary=%d", len(stats.DevicesData), stats.DevicesSummary)
			}
		})
	})
}

// The span counts equal the published replay on histories built to disagree
// with it wherever they could: event IDs ordered against their times across
// and within days, several events per device-day, events exactly on the
// cutoff, carried state ending on the start day, other event types, events
// after the end day and before the retention of any carry-in. Every cutoff
// time of day and chunk size, including chunks of one event, which split
// every device's events across the chunk-end search, must give the same days,
// counts and summary.
func TestComputeStatsDeviceWindowMatchesReplay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		base := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
		random := rand.New(rand.NewPCG(20261012, 1))
		kinds := []AuditEventType{
			AuditEventTypeDeviceAdded, AuditEventTypeDeviceAdded, AuditEventTypeDeviceAdded,
			AuditEventTypeDeviceRemoved, AuditEventTypeDeviceRemoved, AuditEventTypeDeviceRemoved,
			AuditEventTypeNetworkCreated,
		}
		ids := random.Perm(20000)
		events := []auditStatsTestEvent{}
		appendEvent := func(device uint64, at time.Time, kind AuditEventType) {
			events = append(events, auditStatsTestEvent{uint64(ids[len(events)] + 1), device, at, kind})
		}
		cutoffs := []time.Time{base, base.Add(time.Hour), base.Add(13*time.Hour + 17*time.Minute), base.Add(24*time.Hour - time.Microsecond)}
		for device := uint64(1); device <= 240; device++ {
			for range random.IntN(12) {
				at := base.Add(time.Duration(random.Int64N(int64(70 * 24 * time.Hour)))).Add(-20 * 24 * time.Hour).Truncate(time.Second)
				switch random.IntN(8) {
				case 0:
					// exactly on a cutoff
					at = cutoffs[random.IntN(len(cutoffs))]
				case 1:
					// several revisions within one minute of one day
					at = base.Add(time.Duration(random.IntN(6)) * 24 * time.Hour).Add(time.Duration(random.IntN(60)) * time.Second)
				}
				appendEvent(device, at, kinds[random.IntN(len(kinds))])
			}
		}
		// The realistic daily history, on devices of its own.
		for _, event := range auditStatsDailyHistory(base, 60, -12, 40) {
			appendEvent(1000+event.device, event.at, event.kind)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, "SET LOCAL TIME ZONE 'UTC'"))
			insertAuditStatsTestEvents(ctx, tx, events)
			server.RaisePgResult(tx.Exec(ctx, "ANALYZE audit_device_event"))
			for _, cutoff := range cutoffs {
				startDay := cutoff.Format(time.DateOnly)
				for _, endDayOffset := range []int{0, 5, 30} {
					endDay := cutoff.AddDate(0, 0, endDayOffset).Format(time.DateOnly)
					wantData, wantSummary := referenceComputeStatsDeviceWindow(ctx, tx, startDay, endDay, cutoff)
					for _, chunkEventLimit := range []int{1, 2, 7, 64, computeStatsDeviceChunkEventLimit} {
						stats := &Stats{}
						computeStatsDeviceWindowChunked(ctx, stats, tx, startDay, endDay, cutoff, chunkEventLimit)
						if !maps.Equal(stats.DevicesData, wantData) || stats.DevicesSummary != wantSummary {
							t.Fatalf("cutoff %s to %s in chunks of %d: %d days, summary %d; the replay has %d days, summary %d",
								cutoff, endDay, chunkEventLimit, len(stats.DevicesData), stats.DevicesSummary, len(wantData), wantSummary)
						}
					}
				}
			}
			t.Logf("%d synthetic events agree for %d cutoffs, 3 end days and 5 chunk sizes", len(events), len(cutoffs))
		})
	})
}

// Main's export streamed one row per device-day, 37M rows an hour, into a Go
// replay. Only span counts may leave the database now: their number is bounded
// by the window's days, whatever the number of devices.
func TestComputeStatsDeviceWindowReturnsOnlySpanCounts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cutoff := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
		deviceCount, dayCount := 400, 30
		events := auditStatsDailyHistory(cutoff, deviceCount, -5, dayCount)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, "SET LOCAL TIME ZONE 'UTC'"))
			insertAuditStatsTestEvents(ctx, tx, events)
			conn := &auditStatsRecordingConn{conn: tx}
			stats := &Stats{}
			computeStatsDeviceWindowChunked(ctx, stats, conn, cutoff.Format(time.DateOnly),
				cutoff.AddDate(0, 0, dayCount).Format(time.DateOnly), cutoff, computeStatsDeviceChunkEventLimit)
			rowCount := 0
			for _, statement := range conn.statements {
				rowCount += statement.rowCount
			}
			// at most one row per (start day, end day) pair and chunk
			bound := len(conn.statements) * (dayCount + 2) * (dayCount + 2)
			if bound < rowCount || deviceCount*dayCount <= rowCount {
				t.Fatalf("device series returned %d rows in %d statements for %d devices over %d days, bound %d",
					rowCount, len(conn.statements), deviceCount, dayCount, bound)
			}
			t.Logf("%d devices over %d days: %d rows in %d statements", deviceCount, dayCount, rowCount, len(conn.statements))
		})
	})
}

// The single statement on Main read every retained event and held its snapshot
// for minutes. As in the export, each statement now runs on its own, outside a
// transaction, and reads at most one chunk of the device-leading index; only
// the last chunk-end search may read the remaining tail whole, when that is
// cheaper than seeking through it.
func TestComputeStatsDeviceWindowReadsBoundedChunks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cutoff := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
		events := auditStatsDailyHistory(cutoff, 1000, -30, 30)
		chunkEventLimit := 4000
		server.Tx(ctx, func(tx server.PgTx) {
			insertAuditStatsTestEvents(ctx, tx, events)
		})
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `VACUUM (ANALYZE) audit_device_event`))
			recording := &auditStatsRecordingConn{conn: conn}
			stats := &Stats{}
			computeStatsDeviceWindowChunked(ctx, stats, recording, cutoff.Format(time.DateOnly),
				cutoff.AddDate(0, 0, 30).Format(time.DateOnly), cutoff, chunkEventLimit)
			if len(recording.statements) < 2 {
				t.Fatalf("%d events were read by %d statement(s)", len(events), len(recording.statements))
			}
			var maxVisited float64
			for _, statement := range recording.statements {
				_, visited := auditStatsExplain(ctx, conn, statement.sql, statement.args...).auditWork()
				maxVisited = max(maxVisited, visited)
			}
			if float64(2*chunkEventLimit+1) < maxVisited {
				t.Fatalf("a statement read %.0f of %d events, beyond two %d event chunks", maxVisited, len(events), chunkEventLimit)
			}
			t.Logf("%d events in %d statements, at most %.0f events each", len(events), len(recording.statements), maxVisited)
		})
	})
}

// The export walks chains of chunks at once, each statement on its own pooled
// connection, split where the planner's histogram puts equal shares of the
// index. Neither the split nor the order chains finish in may change the
// series.
func TestComputeStatsDeviceWindowParallelChainsMatchReplay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cutoff := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
		events := auditStatsDailyHistory(cutoff, 1000, -30, 30)
		// Every hundredth device also has an earlier removal on one day with a
		// newer ID than that day's other events, so the latest-ID rule, not
		// time, decides its day.
		for device := uint64(100); device <= 1000; device += 100 {
			events = append(events, auditStatsTestEvent{uint64(len(events) + 1000000), device,
				cutoff.AddDate(0, 0, 3).Add(-5 * time.Hour), AuditEventTypeDeviceRemoved})
		}
		server.Tx(ctx, func(tx server.PgTx) {
			insertAuditStatsTestEvents(ctx, tx, events)
		})
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `VACUUM (ANALYZE) audit_device_event`))
		})
		startDay, endDay := cutoff.Format(time.DateOnly), cutoff.AddDate(0, 0, 30).Format(time.DateOnly)
		var wantData map[string]int
		var wantSummary int
		server.Db(ctx, func(conn server.PgConn) {
			wantData, wantSummary = referenceComputeStatsDeviceWindow(ctx, conn, startDay, endDay, cutoff)
		})
		var stateLock sync.Mutex
		chainedCount := 0
		query := func(run func(server.PgCanQuery)) {
			server.Db(ctx, func(conn server.PgConn) {
				recording := &auditStatsRecordingConn{conn: conn}
				run(recording)
				stateLock.Lock()
				defer stateLock.Unlock()
				for _, statement := range recording.statements {
					if strings.Contains(statement.sql, "@chainEnd") {
						chainedCount += 1
					}
				}
			})
		}
		stats := &Stats{}
		computeStatsDeviceWindowParallel(ctx, stats, query, 4, startDay, endDay, cutoff, 3000)
		if chainedCount == 0 {
			t.Fatal("the histogram split no chains; one walk covered every device")
		}
		if !maps.Equal(stats.DevicesData, wantData) || stats.DevicesSummary != wantSummary {
			t.Fatalf("parallel chains: %d days, summary %d; the replay has %d days, summary %d",
				len(stats.DevicesData), stats.DevicesSummary, len(wantData), wantSummary)
		}
	})
}
